package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// timeSelector names a DKIM key by the day it was made: a rotation gets a new
// selector, so the old signatures keep verifying until the record is replaced.
func timeSelector() string { return time.Now().UTC().Format("20060102") }

// mailLookup asks public resolvers, not the host's: what matters is the record
// the rest of the world sees, and /etc/hosts must not answer for it.
func mailLookup(ctx context.Context, name string, qtype uint16) ([]string, error) {
	fqdn := dns.Fqdn(name)
	var lastErr error
	for _, server := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		c := &dns.Client{Timeout: 5 * time.Second}
		m := new(dns.Msg)
		m.SetQuestion(fqdn, qtype)
		m.RecursionDesired = true
		r, _, err := c.ExchangeContext(ctx, m, server)
		if err != nil {
			lastErr = err
			continue
		}
		out := []string{}
		for _, rr := range r.Answer {
			switch v := rr.(type) {
			case *dns.MX:
				out = append(out, fmt.Sprintf("%d %s", v.Preference, strings.TrimSuffix(v.Mx, ".")))
			case *dns.TXT:
				out = append(out, strings.Join(v.Txt, ""))
			case *dns.A:
				out = append(out, v.A.String())
			case *dns.AAAA:
				out = append(out, v.AAAA.String())
			case *dns.PTR:
				out = append(out, strings.TrimSuffix(v.Ptr, "."))
			case *dns.CNAME:
				out = append(out, strings.TrimSuffix(v.Target, "."))
			}
		}
		return out, nil
	}
	return nil, lastErr
}

// dkimValue extracts p= from a DKIM record.
func dkimValue(txt string) string {
	for _, part := range strings.Split(txt, ";") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, "p="); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// mailDNS builds the records the domain needs and compares them with what is
// published right now.
func (s *Server) mailDNS(ctx context.Context, d *store.MailDomain) apitypes.MailDNS {
	c := s.loadMailConfig(ctx)
	out := apitypes.MailDNS{Domain: d.Name, Hostname: c.Hostname, IPv4: localIPv4s(), OK: true}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	add := func(r apitypes.MailDNSRecord) {
		if r.Required && r.Status != "ok" {
			out.OK = false
		}
		out.Records = append(out.Records, r)
	}

	// A-запись почтового хоста: без неё не выпустить сертификат и не принять почту.
	hostRec := apitypes.MailDNSRecord{Name: c.Hostname, Type: "A", Value: strings.Join(out.IPv4, ", "), Status: "unknown", Required: true,
		Note: "имя сервера должно указывать на этот адрес"}
	if addrs, err := mailLookup(ctx, c.Hostname, dns.TypeA); err == nil {
		hostRec.Found = strings.Join(addrs, ", ")
		hostRec.Status = "missing"
		for _, a := range addrs {
			for _, ip := range out.IPv4 {
				if a == ip {
					hostRec.Status = "ok"
				}
			}
		}
		if hostRec.Status == "missing" && len(addrs) > 0 {
			hostRec.Status = "mismatch"
		}
	}
	add(hostRec)

	mx := apitypes.MailDNSRecord{Name: d.Name, Type: "MX", Value: "10 " + c.Hostname, Status: "unknown", Required: true,
		Note: "куда доставлять почту домена"}
	if answers, err := mailLookup(ctx, d.Name, dns.TypeMX); err == nil {
		mx.Found = strings.Join(answers, "; ")
		mx.Status = "missing"
		for _, a := range answers {
			if f := strings.Fields(a); len(f) == 2 && strings.EqualFold(f[1], c.Hostname) {
				mx.Status = "ok"
			}
		}
		if mx.Status == "missing" && len(answers) > 0 {
			mx.Status = "mismatch"
		}
	}
	add(mx)

	spfValue := fmt.Sprintf("v=spf1 mx a:%s -all", c.Hostname)
	spf := apitypes.MailDNSRecord{Name: d.Name, Type: "TXT", Value: spfValue, Status: "unknown", Required: true,
		Note: "кому разрешено отправлять от имени домена"}
	if answers, err := mailLookup(ctx, d.Name, dns.TypeTXT); err == nil {
		spf.Status = "missing"
		for _, a := range answers {
			if !strings.HasPrefix(strings.ToLower(a), "v=spf1") {
				continue
			}
			spf.Found = a
			spf.Status = "mismatch"
			if strings.Contains(a, " mx") || strings.Contains(a, "a:"+c.Hostname) {
				spf.Status = "ok"
			}
		}
	}
	add(spf)

	if d.DKIMSelector != "" && d.DKIMPublic != "" {
		name := d.DKIMSelector + "._domainkey." + d.Name
		value := "v=DKIM1; h=sha256; k=rsa; p=" + d.DKIMPublic
		rec := apitypes.MailDNSRecord{Name: name, Type: "TXT", Value: value, Status: "unknown", Required: true,
			Note: "подпись писем; значение длинное — многие панели DNS разбивают его сами"}
		if answers, err := mailLookup(ctx, name, dns.TypeTXT); err == nil {
			rec.Status = "missing"
			for _, a := range answers {
				if !strings.Contains(a, "p=") {
					continue
				}
				rec.Found = a
				if dkimValue(a) == d.DKIMPublic {
					rec.Status = "ok"
				} else {
					rec.Status = "mismatch"
				}
			}
		}
		add(rec)
	}

	dmarc := apitypes.MailDNSRecord{Name: "_dmarc." + d.Name, Type: "TXT",
		Value:  fmt.Sprintf("v=DMARC1; p=quarantine; rua=mailto:postmaster@%s; adkim=r; aspf=r", d.Name),
		Status: "unknown", Required: true, Note: "что делать с письмами, не прошедшими проверку"}
	if answers, err := mailLookup(ctx, "_dmarc."+d.Name, dns.TypeTXT); err == nil {
		dmarc.Status = "missing"
		for _, a := range answers {
			if strings.HasPrefix(strings.ToLower(a), "v=dmarc1") {
				dmarc.Found, dmarc.Status = a, "ok"
			}
		}
	}
	add(dmarc)

	ptr := apitypes.MailDNSRecord{Name: strings.Join(out.IPv4, ", "), Type: "PTR", Value: c.Hostname, Status: "unknown",
		Note: "обратную зону настраивает хостер; без PTR письма чаще попадают в спам"}
	if len(out.IPv4) > 0 {
		if rev, err := dns.ReverseAddr(out.IPv4[0]); err == nil {
			if answers, err := mailLookup(ctx, rev, dns.TypePTR); err == nil {
				ptr.Status = "missing"
				for _, a := range answers {
					ptr.Found = a
					if strings.EqualFold(strings.TrimSuffix(a, "."), c.Hostname) {
						ptr.Status = "ok"
					} else {
						ptr.Status = "mismatch"
					}
				}
			}
		}
	}
	add(ptr)
	return out
}
