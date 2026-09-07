package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/miekg/dns"

	"monopanel/internal/acme"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

const (
	settingACMEEmail  = "acme.email"
	renewBefore       = 30 * 24 * time.Hour
	renewRetryBackoff = 12 * time.Hour
)

type certsOutput struct {
	Body []*store.Certificate
}

type certOutput struct {
	Body *store.Certificate
}

type certIDInput struct {
	ID int64 `path:"id" minimum:"1"`
}

type issueCertInput struct {
	Body apitypes.IssueCertificateRequest
}

type certJobOutput struct {
	Status int
	Body   apitypes.CertificateWithJob
}

type tlsInfoOutput struct {
	Body apitypes.TLSInfo
}

type certIssuePayload struct {
	CertID int64 `json:"cert_id"`
}

func (s *Server) registerCerts() {
	huma.Register(s.api, huma.Operation{
		OperationID: "certificates-list", Method: http.MethodGet, Path: "/certificates", Summary: "List certificates", Tags: []string{"ssl"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*certsOutput, error) {
		list, err := s.db.ListCertificates(ctx)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []*store.Certificate{}
		}
		return &certsOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "certificates-issue", Method: http.MethodPost, Path: "/certificates", Summary: "Order an ACME certificate (async, HTTP-01)", Tags: []string{"ssl"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *issueCertInput) (*certJobOutput, error) {
		p := principalFrom(ctx)
		names := make([]string, 0, len(in.Body.Names))
		seen := map[string]bool{}
		if in.Body.DNS != "" {
			if _, err := s.db.GetDNSProvider(ctx, in.Body.DNS); err != nil {
				return nil, huma.Error422UnprocessableEntity("unknown DNS provider " + in.Body.DNS)
			}
		}
		for _, n := range in.Body.Names {
			n = acme.NormalizeName(n)
			var err error
			if in.Body.DNS != "" {
				err = acme.ValidateNameDNS(n)
			} else {
				err = acme.ValidateName(n)
			}
			if err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		directory := acme.LetsEncrypt
		if in.Body.Staging {
			directory = acme.LetsEncryptStaging
		}
		if in.Body.Directory != "" {
			directory = in.Body.Directory
		}
		email := in.Body.Email
		if email != "" {
			s.db.SetSetting(ctx, settingACMEEmail, email)
		} else {
			email, _ = s.db.GetSetting(ctx, settingACMEEmail)
		}
		c, err := s.db.GetCertificateByName(ctx, names[0])
		if errors.Is(err, store.ErrNotFound) {
			c = &store.Certificate{Name: names[0], AutoRenew: true}
		} else if err != nil {
			return nil, err
		}
		c.Names, c.Kind, c.DirectoryURL, c.Email, c.DNSProvider = names, store.CertKindACME, directory, email, in.Body.DNS
		c.KeyType = in.Body.KeyType
		if c.KeyType == "" {
			c.KeyType = "ec256"
		}
		if in.Body.AutoRenew != nil {
			c.AutoRenew = *in.Body.AutoRenew
		}
		c.Status, c.LastError = store.CertPending, ""
		if p.UserID != 0 {
			uid := p.UserID
			c.UserID = &uid
		}
		if err := s.db.UpsertCertificate(ctx, c); err != nil {
			return nil, err
		}
		job, err := s.jobs.Enqueue(ctx, "cert.issue", certIssuePayload{CertID: c.ID}, jobs.WithLockKey("cert:"+c.Name), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cert.issue", Target: c.Name, IP: requestInfo(ctx).IP, Details: map[string]any{"names": names, "directory": directory}})
		return &certJobOutput{Status: http.StatusAccepted, Body: apitypes.CertificateWithJob{Certificate: c, JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "certificates-get", Method: http.MethodGet, Path: "/certificates/{id}", Summary: "Get a certificate", Tags: []string{"ssl"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *certIDInput) (*certOutput, error) {
		c, err := s.db.GetCertificate(ctx, in.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("certificate not found")
		}
		if err != nil {
			return nil, err
		}
		return &certOutput{Body: c}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "certificates-renew", Method: http.MethodPost, Path: "/certificates/{id}/renew", Summary: "Renew (re-issue) a certificate now (async)", Tags: []string{"ssl"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *certIDInput) (*certJobOutput, error) {
		p := principalFrom(ctx)
		c, err := s.db.GetCertificate(ctx, in.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("certificate not found")
		}
		if err != nil {
			return nil, err
		}
		if c.Kind != store.CertKindACME {
			return nil, huma.Error422UnprocessableEntity("only ACME certificates can be renewed")
		}
		job, err := s.jobs.Enqueue(ctx, "cert.issue", certIssuePayload{CertID: c.ID}, jobs.WithLockKey("cert:"+c.Name), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cert.renew", Target: c.Name, IP: requestInfo(ctx).IP})
		return &certJobOutput{Status: http.StatusAccepted, Body: apitypes.CertificateWithJob{Certificate: c, JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "certificates-delete", Method: http.MethodDelete, Path: "/certificates/{id}", Summary: "Delete a certificate and its files", Tags: []string{"ssl"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *certIDInput) (*struct{}, error) {
		p := principalFrom(ctx)
		c, err := s.db.GetCertificate(ctx, in.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("certificate not found")
		}
		if err != nil {
			return nil, err
		}
		if err := s.acme.Remove(c.Name); err != nil {
			return nil, err
		}
		if err := s.db.DeleteCertificate(ctx, c.ID); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cert.delete", Target: c.Name, IP: requestInfo(ctx).IP})
		if containsName(c.Names, s.cfg.Web.Hostname) {
			s.tls.Reload() //nolint:errcheck // the panel keeps serving the old certificate
		}
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "web-tls", Method: http.MethodGet, Path: "/web/tls", Summary: "Certificate the panel serves on its HTTPS port", Tags: []string{"ssl"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*tlsInfoOutput, error) {
		return &tlsInfoOutput{Body: s.tls.Info()}, nil
	})
}

func containsName(names []string, name string) bool {
	name = acme.NormalizeName(name)
	for _, n := range names {
		if acme.NormalizeName(n) == name {
			return true
		}
	}
	return false
}

// certFail records a failed order. A certificate whose files are still valid
// keeps the "valid" status (sites go on serving it); only the error and the
// attempt time are stored, so the renewal loop backs off and retries.
func (s *Server) certFail(ctx context.Context, c *store.Certificate, err error) error {
	status := store.CertError
	if c.CertPath != "" && c.NotAfter != nil && time.Now().Before(*c.NotAfter) {
		status = store.CertValid
	}
	_ = s.db.SetCertificateStatus(context.WithoutCancel(ctx), c.ID, status, err.Error())
	return err
}

// renewalBackoff reports whether the last failed attempt is too recent to retry.
func renewalBackoff(c *store.Certificate) bool {
	return c.LastError != "" && c.LastAttempt != nil && time.Since(*c.LastAttempt) < renewRetryBackoff
}

// behindCloudflare reports whether every address belongs to Cloudflare's edge.
func behindCloudflare(addrs []string) bool {
	if len(addrs) == 0 {
		return false
	}
	var nets []*net.IPNet
	for _, cidr := range render.CloudflareRanges {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, n)
		}
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		hit := false
		for _, n := range nets {
			if ip != nil && n.Contains(ip) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// jobCertIssue orders or renews one certificate.
func (s *Server) jobCertIssue(ctx context.Context, jc *jobs.Context) error {
	var p certIssuePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	c, err := s.db.GetCertificate(ctx, p.CertID)
	if err != nil {
		return err
	}
	var dnsType string
	var dnsCreds map[string]string
	if c.DNSProvider != "" {
		dnsType, dnsCreds, err = s.dnsCredentials(ctx, c.DNSProvider)
		if err != nil {
			return s.certFail(ctx, c, fmt.Errorf("DNS provider %s: %w", c.DNSProvider, err))
		}
	}
	jc.Progress(5, "checking DNS")
	local := localIPv4s()
	localSet := map[string]bool{}
	for _, ip := range local {
		localSet[ip] = true
	}
	for _, n := range c.Names {
		if c.DNSProvider != "" {
			continue // DNS-01: the name need not resolve here (wildcards, other hosts)
		}
		addrs, err := publicLookup(ctx, n)
		if err != nil {
			return s.certFail(ctx, c, fmt.Errorf("DNS: %s does not resolve (%v); create an A record pointing to %s", n, err, strings.Join(local, " / ")))
		}
		jc.Logf("%s → %s", n, strings.Join(addrs, ", "))
		points := false
		for _, a := range addrs {
			if localSet[a] {
				points = true
			}
		}
		if !points && behindCloudflare(addrs) {
			jc.Logf("%s is proxied by Cloudflare: the DNS record's origin must be this host (%s) for HTTP-01 to pass", n, strings.Join(local, " / "))
		} else if !points {
			jc.Logf("warning: %s does not point to this host (%s); validation will fail unless port 80 is forwarded here", n, strings.Join(local, ", "))
		}
	}
	if c.DNSProvider == "" {
		jc.Progress(15, "probing HTTP-01 path")
		probeIP := "127.0.0.1"
		if len(local) > 0 {
			probeIP = local[0]
		}
		if err := s.acme.Probe(ctx, c.Names[0], probeIP); err != nil {
			return s.certFail(ctx, c, err)
		}
		jc.Logf("nginx serves /.well-known/acme-challenge/ for %s", c.Names[0])
	}
	jc.Progress(30, "ordering certificate")
	octx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res, err := s.acme.Issue(octx, acme.IssueRequest{Names: c.Names, Email: c.Email, Directory: c.DirectoryURL, KeyType: c.KeyType, Logf: jc.Logf, DNSProviderType: dnsType, DNSCredentials: dnsCreds})
	if err != nil {
		return s.certFail(ctx, c, err)
	}
	jc.Progress(90, "saving")
	now := time.Now()
	c.CertPath, c.KeyPath, c.ChainPath = res.CertPath, res.KeyPath, res.ChainPath
	c.Issuer, c.Serial = res.CertInfo.Issuer, res.CertInfo.Serial
	nb, na := res.CertInfo.NotBefore, res.CertInfo.NotAfter
	c.NotBefore, c.NotAfter, c.LastAttempt = &nb, &na, &now
	c.Status, c.LastError = store.CertValid, ""
	if err := s.db.UpsertCertificate(context.WithoutCancel(ctx), c); err != nil {
		return err
	}
	jc.Logf("files: %s", res.CertPath)
	s.reapplySitesForCert(context.WithoutCancel(ctx), jc.Logf, c)
	if mc := s.loadMailConfig(ctx); mc.Installed && containsName(c.Names, mc.Hostname) {
		if err := s.applyMailConfig(context.WithoutCancel(ctx), jc.Logf, true); err != nil {
			jc.Logf("предупреждение: почтовые сервисы не подхватили сертификат: %v", err)
		} else {
			jc.Logf("postfix и dovecot переключены на этот сертификат")
		}
	}
	if containsName(c.Names, s.cfg.Web.Hostname) {
		if err := s.tls.Reload(); err != nil {
			jc.Logf("warning: panel certificate reload failed: %v", err)
		} else {
			jc.Logf("panel HTTPS on %s now serves this certificate", s.cfg.Web.Listen)
		}
	}
	jc.Progress(100, "issued")
	return nil
}

// renewLoop re-issues ACME certificates 30 days before expiry and keeps the
// panel's own certificate fresh.
func (s *Server) renewLoop(ctx context.Context) {
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	s.renewDue(ctx)
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.renewDue(ctx)
		}
	}
}

func (s *Server) renewDue(ctx context.Context) {
	due, err := s.db.ListCertificatesForRenewal(ctx, time.Now().Add(renewBefore))
	if err != nil {
		s.log.Error("renewal scan", "err", err)
		return
	}
	for _, c := range due {
		if renewalBackoff(c) {
			continue
		}
		key := fmt.Sprintf("renew:%d:%s", c.ID, time.Now().UTC().Format("2006-01-02"))
		if _, err := s.jobs.Enqueue(ctx, "cert.issue", certIssuePayload{CertID: c.ID}, jobs.WithLockKey("cert:"+c.Name), jobs.WithRequestedBy("scheduler"), jobs.WithIdempotencyKey(key)); err != nil {
			s.log.Error("enqueue renewal", "cert", c.Name, "err", err)
			continue
		}
		s.log.Info("renewal scheduled", "cert", c.Name, "expires", c.NotAfter)
	}
	if err := s.tls.Reload(); err != nil {
		s.log.Error("panel certificate reload", "err", err)
	}
}

// publicLookup resolves a name with real DNS queries to public resolvers (what
// the CA sees). The system resolver is only a fallback: /etc/hosts entries such
// as "127.0.1.1 <hostname>" must not mask a missing public record.
func publicLookup(ctx context.Context, name string) ([]string, error) {
	fqdn := dns.Fqdn(name)
	var lastErr error
	for _, server := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		c := &dns.Client{Timeout: 5 * time.Second}
		var addrs []string
		nxdomain := false
		for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
			m := new(dns.Msg)
			m.SetQuestion(fqdn, qtype)
			m.RecursionDesired = true
			r, _, err := c.ExchangeContext(ctx, m, server)
			if err != nil {
				lastErr = err
				break
			}
			if r.Rcode == dns.RcodeNameError {
				nxdomain = true
				break
			}
			for _, rr := range r.Answer {
				switch v := rr.(type) {
				case *dns.A:
					addrs = append(addrs, v.A.String())
				case *dns.AAAA:
					addrs = append(addrs, v.AAAA.String())
				}
			}
		}
		if nxdomain {
			return nil, fmt.Errorf("NXDOMAIN (no such name in public DNS)")
		}
		if len(addrs) > 0 {
			return addrs, nil
		}
		if lastErr == nil {
			return nil, fmt.Errorf("no A/AAAA records in public DNS")
		}
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	if err != nil {
		return nil, lastErr
	}
	return addrs, nil
}
