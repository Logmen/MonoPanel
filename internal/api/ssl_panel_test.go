package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/config"
	"monopanel/internal/store"
)

// The panel's certificate is the record named after web.hostname: the panel
// endpoint reports it, orders it for that one name, and refuses to drop it
// while a site serves it too.
func TestPanelTLSEndpoints(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Web.Hostname = "panel.example.com"; c.Web.Listen = ":8443" })
	if err := f.s.tls.Reload(); err != nil { // the fixture never serves TLS; load the self-signed pair as Run would
		t.Fatal(err)
	}

	var panel struct {
		Hostname     string
		Source       string
		Record       *struct{ Name, Status string }
		DNSProviders []string `json:"dns_providers"`
	}
	f.call(http.MethodGet, "/ssl/panel", nil, http.StatusOK, &panel)
	if panel.Hostname != "panel.example.com" || panel.Source != "selfsigned" || panel.Record != nil {
		t.Fatalf("fresh panel: %+v", panel)
	}

	// Ordering takes the hostname, nothing else; the record appears at once.
	var issued struct {
		Certificate struct {
			Name   string
			Names  []string
			Status string
		}
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/ssl/panel/issue", map[string]any{"staging": true}, http.StatusAccepted, &issued)
	if issued.Certificate.Name != "panel.example.com" || len(issued.Certificate.Names) != 1 || issued.Certificate.Status != "pending" || issued.JobID == 0 {
		t.Fatalf("panel order: %+v", issued)
	}
	f.call(http.MethodGet, "/ssl/panel", nil, http.StatusOK, &panel)
	if panel.Record == nil || panel.Record.Name != "panel.example.com" {
		t.Fatalf("record missing after the order: %+v", panel)
	}

	// The listing says who uses what; the panel's record cannot be deleted
	// through the generic endpoint.
	var list []struct {
		ID          int64
		Name        string
		UsedByPanel bool     `json:"used_by_panel"`
		UsedBySites []string `json:"used_by_sites"`
	}
	f.call(http.MethodGet, "/certificates", nil, http.StatusOK, &list)
	if len(list) != 1 || !list[0].UsedByPanel || len(list[0].UsedBySites) != 0 {
		t.Fatalf("usage: %+v", list)
	}
	status, raw := f.do(http.MethodDelete, "/certificates/"+itoa(list[0].ID), nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), "self-signed") {
		t.Fatalf("deleting the panel certificate: %d %s", status, raw)
	}

	// A site named like the panel serves the same certificate: then even the
	// panel endpoint keeps it.
	f.createSite(map[string]any{"domain": "panel.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto"})
	f.call(http.MethodGet, "/certificates", nil, http.StatusOK, &list)
	if len(list[0].UsedBySites) != 1 || list[0].UsedBySites[0] != "panel.example.com" {
		t.Fatalf("site usage: %+v", list)
	}
	if status, raw := f.do(http.MethodDelete, "/ssl/panel", nil); status != http.StatusConflict {
		t.Fatalf("panel reset with a site on the name: %d %s", status, raw)
	}
	// Without the site the panel goes back to self-signed.
	f.call(http.MethodDelete, "/sites/panel.example.com", nil, http.StatusAccepted, &issued)
	f.waitJob(issued.JobID)
	f.call(http.MethodDelete, "/ssl/panel", nil, http.StatusNoContent, nil)
	panel.Record = nil // json leaves absent fields alone
	f.call(http.MethodGet, "/ssl/panel", nil, http.StatusOK, &panel)
	if panel.Record != nil || panel.Source != "selfsigned" {
		t.Fatalf("after reset: %+v", panel)
	}
	f.call(http.MethodDelete, "/ssl/panel", nil, http.StatusNotFound, nil)
}

// A panel addressed by IP has nothing to certify.
func TestPanelTLSIssueNeedsHostname(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Web.Hostname = "192.0.2.10" })
	f.call(http.MethodPost, "/ssl/panel/issue", map[string]any{}, http.StatusUnprocessableEntity, nil)
}

// Ordering a certificate for a site takes its domain and aliases, turns SSL
// on so the site is re-applied with HTTPS once the certificate arrives, and
// remembers the DNS provider for DNS-01 renewals.
func TestSiteTLSIssue(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPost, "/dns-providers", map[string]any{"name": "cf", "type": "cloudflare", "credentials": map[string]string{"CLOUDFLARE_DNS_API_TOKEN": "t"}}, http.StatusCreated, nil)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "www": true})

	var issued struct {
		Certificate struct {
			Name        string
			Names       []string
			DNSProvider string `json:"dns_provider"`
			Status      string
		}
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/shop.example.com/tls/issue", map[string]any{"dns": "cf", "staging": true}, http.StatusAccepted, &issued)
	if issued.Certificate.Name != "shop.example.com" || len(issued.Certificate.Names) != 2 || issued.Certificate.DNSProvider != "cf" || issued.Certificate.Status != "pending" {
		t.Fatalf("site order: %+v", issued)
	}
	site, err := f.db.GetSiteByDomain(f.ctx, "shop.example.com")
	if err != nil || site.SSL != "auto" {
		t.Fatalf("site must switch to auto SSL: %v %+v", err, site)
	}
	if status, _ := f.do(http.MethodPost, "/sites/nosuch.example.com/tls/issue", map[string]any{}); status != http.StatusNotFound {
		t.Fatalf("unknown site: %d", status)
	}
	var list []struct {
		Name        string
		UsedBySites []string `json:"used_by_sites"`
	}
	f.call(http.MethodGet, "/certificates", nil, http.StatusOK, &list)
	if len(list) != 1 || len(list[0].UsedBySites) != 1 || list[0].UsedBySites[0] != "shop.example.com" {
		t.Fatalf("usage after the site order: %+v", list)
	}
	_ = store.CertPending
}

// do sends a request and returns the status and the raw body, for the
// assertions call cannot express.
func (f *siteFixture) do(method, path string, body any) (int, []byte) {
	f.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(f.ctx, method, f.ts.URL+"/api/v1"+path, rdr)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}
