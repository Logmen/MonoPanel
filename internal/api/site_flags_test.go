package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/store"
)

// A site listens on the host's IPv6 by default, can be kept on IPv4 only,
// and its security headers and rate limit reach the nginx config.
func TestSiteIPv6HeadersAndRateLimit(t *testing.T) {
	f := newFixture(t, nil)
	f.s.SetHostIPs(func() []string { return []string{"203.0.113.10"} })
	f.s.SetHostIPv6s(func() []string { return []string{"2001:db8::10"} })
	f.login()

	site := f.createSite(map[string]any{"domain": "six.example.com", "user": "alex", "security_headers": true, "rate_limit": 20})
	if site.IPv6 != "2001:db8::10" || !site.SecurityHeaders || site.RateLimit != 20 {
		t.Fatalf("site: ipv6=%q headers=%v rate=%d", site.IPv6, site.SecurityHeaders, site.RateLimit)
	}
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/six.example.com.conf")
	for _, want := range []string{"listen [2001:db8::10]:80;", "add_header X-Content-Type-Options nosniff always;", "limit_req_zone $mp_six_example_com_key zone=mp_six_example_com:2m rate=20r/s;", "limit_req zone=mp_six_example_com burst=40 nodelay;"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("nginx config lacks %q:\n%s", want, conf)
		}
	}

	four := f.createSite(map[string]any{"domain": "four.example.com", "user": "alex", "ipv6": "none"})
	if four.IPv6 != "" {
		t.Fatalf("ipv6=none kept %q", four.IPv6)
	}
	if c4, _ := f.agent.File("/etc/nginx/monopanel/sites/four.example.com.conf"); !strings.Contains(c4, "listen 203.0.113.10:80;") || strings.Contains(c4, "listen [") {
		t.Fatal("an IPv4-only site must not listen on IPv6")
	}

	// An address the host does not have is refused; switching off works.
	f.call(http.MethodPatch, "/sites/six.example.com", map[string]any{"ipv6": "2001:db8::99"}, http.StatusUnprocessableEntity, nil)
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPatch, "/sites/six.example.com", map[string]any{"ipv6": "none", "rate_limit": 0, "security_headers": false}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("apply after the update: %s %s", job.Status, job.Error)
	}
	conf, _ = f.agent.File("/etc/nginx/monopanel/sites/six.example.com.conf")
	for _, gone := range []string{"listen [", "limit_req", "X-Frame-Options"} {
		if strings.Contains(conf, gone) {
			t.Fatalf("nginx config still has %q after switching off", gone)
		}
	}
}
