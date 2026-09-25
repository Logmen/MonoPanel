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
		Note: "the server name must point to this address"}
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

	if d.SendOnly {
		add(sendOnlyMX(ctx, d.Name, c.Hostname))
		add(sendOnlySPF(ctx, d.Name, c.Hostname, out.IPv4))
		s.addDKIMAndDMARC(ctx, d, add)
		add(s.mailPTR(ctx, c.Hostname, out.IPv4))
		return out
	}

	mx := apitypes.MailDNSRecord{Name: d.Name, Type: "MX", Value: "10 " + c.Hostname, Status: "unknown", Required: true,
		Note: "where the domain's mail is delivered"}
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
		Note: "who may send mail on behalf of the domain"}
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

	s.addDKIMAndDMARC(ctx, d, add)
	add(s.mailPTR(ctx, c.Hostname, out.IPv4))
	return out
}

// addDKIMAndDMARC adds the signing key and the policy records: the same for
// a domain received here and a send-only one.
func (s *Server) addDKIMAndDMARC(ctx context.Context, d *store.MailDomain, add func(apitypes.MailDNSRecord)) {
	if d.DKIMSelector != "" && d.DKIMPublic != "" {
		name := d.DKIMSelector + "._domainkey." + d.Name
		value := "v=DKIM1; h=sha256; k=rsa; p=" + d.DKIMPublic
		rec := apitypes.MailDNSRecord{Name: name, Type: "TXT", Value: value, Status: "unknown", Required: true,
			Note: "message signing; the value is long, but many DNS panels split it themselves"}
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
		Status: "unknown", Required: true, Note: "what to do with messages that fail the checks"}
	if answers, err := mailLookup(ctx, "_dmarc."+d.Name, dns.TypeTXT); err == nil {
		dmarc.Status = "missing"
		for _, a := range answers {
			if strings.HasPrefix(strings.ToLower(a), "v=dmarc1") {
				dmarc.Found, dmarc.Status = a, "ok"
			}
		}
	}
	add(dmarc)

}

// mailPTR checks the reverse record of the server's first address.
func (s *Server) mailPTR(ctx context.Context, hostname string, ipv4 []string) apitypes.MailDNSRecord {
	ptr := apitypes.MailDNSRecord{Name: strings.Join(ipv4, ", "), Type: "PTR", Value: hostname, Status: "unknown",
		Note: "the reverse zone is set up by the hosting provider; without PTR, mail ends up in spam more often"}
	if len(ipv4) > 0 {
		if rev, err := dns.ReverseAddr(ipv4[0]); err == nil {
			if answers, err := mailLookup(ctx, rev, dns.TypePTR); err == nil {
				ptr.Status = "missing"
				for _, a := range answers {
					ptr.Found = a
					if strings.EqualFold(strings.TrimSuffix(a, "."), hostname) {
						ptr.Status = "ok"
					} else {
						ptr.Status = "mismatch"
					}
				}
			}
		}
	}
	return ptr
}

// sendOnlyMX: another server receives the domain's mail, so the MX must stay
// there. Pointing it here would bounce everything — this server does not
// accept mail for a send-only domain.
func sendOnlyMX(ctx context.Context, domain, hostname string) apitypes.MailDNSRecord {
	rec := apitypes.MailDNSRecord{Name: domain, Type: "MX", Value: "your mail provider's servers, not " + hostname, Status: "unknown",
		Note: "the domain is send-only here: its mail is received by another server"}
	answers, err := mailLookup(ctx, domain, dns.TypeMX)
	if err != nil {
		return rec
	}
	rec.Found = strings.Join(answers, "; ")
	switch {
	case len(answers) == 0:
		rec.Status = "missing"
		rec.Note = "without an MX the domain receives no mail at all; your mail provider gives the value"
	default:
		rec.Status = "ok"
		for _, a := range answers {
			if f := strings.Fields(a); len(f) == 2 && strings.EqualFold(f[1], hostname) {
				rec.Status, rec.Required = "mismatch", true
				rec.Note = "the MX points to this server, which only sends for the domain: incoming mail would be refused"
			}
		}
	}
	return rec
}

// sendOnlySPF: the provider already has an SPF record for the domain; this
// server has to be added to it, not put in its place — "-all" with this
// server alone would fail the provider's own mail.
func sendOnlySPF(ctx context.Context, domain, hostname string, ipv4 []string) apitypes.MailDNSRecord {
	mech := "a:" + hostname
	rec := apitypes.MailDNSRecord{Name: domain, Type: "TXT", Value: "v=spf1 " + mech + " ~all", Status: "unknown", Required: true,
		Note: "add " + mech + " to the SPF record of your mail provider instead of replacing it"}
	answers, err := mailLookup(ctx, domain, dns.TypeTXT)
	if err != nil {
		return rec
	}
	rec.Status = "missing"
	for _, a := range answers {
		if !strings.HasPrefix(strings.ToLower(a), "v=spf1") {
			continue
		}
		rec.Found, rec.Status = a, "mismatch"
		rec.Value = spfWith(a, mech)
		if strings.Contains(a, mech) {
			rec.Status = "ok"
		}
		for _, ip := range ipv4 {
			if strings.Contains(a, "ip4:"+ip) {
				rec.Status = "ok"
			}
		}
	}
	return rec
}

// spfWith inserts a mechanism into an SPF record before its "all" term.
func spfWith(record, mech string) string {
	if strings.Contains(record, mech) {
		return record
	}
	f := strings.Fields(record)
	for i, t := range f {
		if strings.HasSuffix(strings.ToLower(t), "all") && (len(t) == 3 || strings.ContainsAny(t[:1], "+-~?")) {
			return strings.Join(append(append(append([]string{}, f[:i]...), mech), f[i:]...), " ")
		}
	}
	return record + " " + mech
}
