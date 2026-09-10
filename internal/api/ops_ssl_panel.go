package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// The panel's certificate is the stored certificate named after web.hostname:
// that is the directory the HTTPS port loads from (see certHolder.Reload).
// A site's certificate is the one it references, or the one named after its
// domain when the reference is not set yet. These endpoints give both their
// own place instead of one list of "names".

type panelTLSOutput struct {
	Body apitypes.PanelTLS
}

type panelTLSIssueInput struct {
	Body apitypes.PanelTLSIssueRequest
}

type panelTLSImportInput struct {
	Body apitypes.PanelTLSImportRequest
}

type siteTLSIssueInput struct {
	Domain string `path:"domain"`
	Body   apitypes.SiteTLSIssueRequest
}

// panelCertName is the name the panel's certificate record carries.
func (s *Server) panelCertName() string {
	return acme.NormalizeName(s.cfg.Web.Hostname)
}

// certUsers reports which sites serve each certificate: by reference, and by
// name for auto-SSL sites that have not been re-applied since their
// certificate arrived.
func (s *Server) certUsers(ctx context.Context) (byID map[int64][]string, byName map[string][]string) {
	byID, byName = map[int64][]string{}, map[string][]string{}
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return byID, byName
	}
	for _, site := range sites {
		if site.Status == store.SiteDeleting {
			continue
		}
		switch {
		case site.CertificateID != nil:
			byID[*site.CertificateID] = append(byID[*site.CertificateID], site.Domain)
		case site.SSL == "auto":
			byName[site.Domain] = append(byName[site.Domain], site.Domain)
		}
	}
	return byID, byName
}

// decorateCertUsage fills UsedByPanel and UsedBySites for a listing.
func (s *Server) decorateCertUsage(ctx context.Context, certs []*store.Certificate) {
	byID, byName := s.certUsers(ctx)
	panel := s.panelCertName()
	for _, c := range certs {
		c.UsedByPanel = panel != "" && c.Name == panel
		c.UsedBySites = append(append([]string{}, byID[c.ID]...), byName[c.Name]...)
		if c.UsedBySites == nil {
			c.UsedBySites = []string{}
		}
	}
}

// certInUse refuses to delete a certificate somebody serves.
func (s *Server) certInUse(ctx context.Context, c *store.Certificate) error {
	s.decorateCertUsage(ctx, []*store.Certificate{c})
	switch {
	case c.UsedByPanel && len(c.UsedBySites) > 0:
		return huma.Error409Conflict(fmt.Sprintf("the panel and the sites %s serve this certificate; switch the panel to self-signed and the sites to another certificate first", strings.Join(c.UsedBySites, ", ")))
	case c.UsedByPanel:
		return huma.Error409Conflict("the panel serves this certificate on its HTTPS port; switch it to self-signed first (mp ssl panel self-signed)")
	case len(c.UsedBySites) > 0:
		return huma.Error409Conflict(fmt.Sprintf("the sites %s serve this certificate; give them another one or turn their SSL off first", strings.Join(c.UsedBySites, ", ")))
	}
	return nil
}

func (s *Server) panelTLS(ctx context.Context) apitypes.PanelTLS {
	out := apitypes.PanelTLS{TLSInfo: s.tls.Info(), DNSProviders: []string{}}
	if name := s.panelCertName(); name != "" {
		if c, err := s.db.GetCertificateByName(ctx, name); err == nil {
			s.decorateCertUsage(ctx, []*store.Certificate{c})
			out.Record = c
		}
	}
	if providers, err := s.db.ListDNSProviders(ctx); err == nil {
		for _, p := range providers {
			out.DNSProviders = append(out.DNSProviders, p.Name)
		}
	}
	return out
}

func (s *Server) registerPanelTLS() {
	huma.Register(s.api, huma.Operation{
		OperationID: "ssl-panel", Method: http.MethodGet, Path: "/ssl/panel", Summary: "The panel's own certificate: what is served and the record behind it", Tags: []string{"ssl"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*panelTLSOutput, error) {
		return &panelTLSOutput{Body: s.panelTLS(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "ssl-panel-issue", Method: http.MethodPost, Path: "/ssl/panel/issue", Summary: "Order a certificate for the panel hostname (async)", Tags: []string{"ssl"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *panelTLSIssueInput) (*certJobOutput, error) {
		host := s.panelCertName()
		if host == "" || net.ParseIP(host) != nil {
			return nil, huma.Error422UnprocessableEntity("the panel has no hostname to certify: set one first (mp config set web.hostname panel.example.com --restart)")
		}
		c, jobID, err := s.issueCertificate(ctx, principalFrom(ctx), apitypes.IssueCertificateRequest{
			Names: []string{host}, Email: in.Body.Email, Staging: in.Body.Staging, DNS: in.Body.DNS, KeyType: in.Body.KeyType,
		})
		if err != nil {
			return nil, err
		}
		return &certJobOutput{Status: http.StatusAccepted, Body: apitypes.CertificateWithJob{Certificate: c, JobID: jobID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "ssl-panel-import", Method: http.MethodPost, Path: "/ssl/panel/import", Summary: "Install an existing certificate for the panel hostname", Tags: []string{"ssl"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *panelTLSImportInput) (*certImportOutput, error) {
		host := s.panelCertName()
		if host == "" || net.ParseIP(host) != nil {
			return nil, huma.Error422UnprocessableEntity("the panel has no hostname to certify: set one first (mp config set web.hostname panel.example.com --restart)")
		}
		c, err := s.importCertificatePEM(ctx, principalFrom(ctx), apitypes.ImportCertificateRequest{Name: host, Certificate: in.Body.Certificate, PrivateKey: in.Body.PrivateKey})
		if err != nil {
			return nil, err
		}
		return &certImportOutput{Status: http.StatusCreated, Body: c}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "ssl-panel-reset", Method: http.MethodDelete, Path: "/ssl/panel", Summary: "Drop the panel's certificate and go back to the self-signed one", Tags: []string{"ssl"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		p := principalFrom(ctx)
		c, err := s.db.GetCertificateByName(ctx, s.panelCertName())
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("the panel serves the self-signed certificate already")
		}
		if err != nil {
			return nil, err
		}
		s.decorateCertUsage(ctx, []*store.Certificate{c})
		if len(c.UsedBySites) > 0 {
			return nil, huma.Error409Conflict(fmt.Sprintf("the sites %s serve this certificate too; it stays until they have another one", strings.Join(c.UsedBySites, ", ")))
		}
		if err := s.acme.Remove(c.Name); err != nil {
			return nil, err
		}
		if err := s.db.DeleteCertificate(ctx, c.ID); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cert.delete", Target: c.Name, IP: requestInfo(ctx).IP, Details: map[string]any{"panel": true}})
		if err := s.tls.Reload(); err != nil {
			s.log.Warn("panel certificate reload", "err", err)
		}
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-tls-issue", Method: http.MethodPost, Path: "/sites/{domain}/tls/issue", Summary: "Order a certificate for a site's names and switch it to HTTPS (async)", Tags: []string{"sites"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *siteTLSIssueInput) (*certJobOutput, error) {
		p := principalFrom(ctx)
		site, err := s.db.GetSiteByDomain(ctx, in.Domain)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("site not found")
		}
		if err != nil {
			return nil, err
		}
		if site.SSL != "auto" {
			// The certificate job re-applies auto-SSL sites it covers; a
			// site with SSL off would stay on HTTP.
			site.SSL = "auto"
			if err := s.db.UpdateSite(ctx, site); err != nil {
				return nil, err
			}
		}
		names := append([]string{site.Domain}, site.Aliases...)
		c, jobID, err := s.issueCertificate(ctx, p, apitypes.IssueCertificateRequest{Names: names, Email: in.Body.Email, Staging: in.Body.Staging, DNS: in.Body.DNS})
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.tls", Target: site.Domain, IP: requestInfo(ctx).IP, Details: map[string]any{"names": names, "dns": in.Body.DNS}})
		return &certJobOutput{Status: http.StatusAccepted, Body: apitypes.CertificateWithJob{Certificate: c, JobID: jobID}}, nil
	})
}
