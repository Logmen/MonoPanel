package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every local address gets a default server on 443 as on 80: a name no site
// has, or a client without SNI, gets a refused handshake rather than the
// address's first site in file order. The QUIC socket with reuseport comes
// once a site serves HTTP/3, and then there is exactly one default_server and
// one reuseport per address and port in the whole configuration.
func TestDefaultServersRefuseUnknownNamesOn443(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetHostIPs(func() []string { return []string{"203.0.113.10"} })
	f.s.SetHostIPv6s(func() []string { return []string{"2001:db8::10"} })
	files, _, err := f.s.nginxGlobalFiles(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, fs := range files {
		got[fs.Path] = fs.Content
	}
	for ip, conf := range map[string]string{"203.0.113.10": got["/etc/nginx/monopanel/http.d/ip-203.0.113.10.conf"], "[2001:db8::10]": got["/etc/nginx/monopanel/http.d/ip-2001:db8::10.conf"]} {
		for _, want := range []string{"listen " + ip + ":80 default_server;", "listen " + ip + ":443 ssl default_server;", "ssl_reject_handshake on;", "include monopanel/snippets/ssl.conf;"} {
			if !strings.Contains(conf, want) {
				t.Errorf("%s: no %q in\n%s", ip, want, conf)
			}
		}
		if strings.Contains(conf, "quic") || strings.Contains(conf, "ssl_certificate") {
			t.Errorf("%s: a QUIC socket or a certificate without a site that needs it:\n%s", ip, conf)
		}
	}

	// A site with HTTP/3 and a certificate: its apply gives the default
	// servers the QUIC socket with reuseport.
	cert, key := testCert(t, "h3.example.com")
	f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": cert, "private_key": key}, http.StatusCreated, nil)
	f.createSite(map[string]any{"domain": "h3.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto", "ip": "203.0.113.10", "ipv6": "2001:db8::10", "http3": true})
	f.createSite(map[string]any{"domain": "plain.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto", "ip": "203.0.113.10", "ipv6": "2001:db8::10"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		conf, _ := f.agent.File("/etc/nginx/monopanel/http.d/ip-203.0.113.10.conf")
		site, _ := f.agent.File("/etc/nginx/monopanel/sites/h3.example.com.conf")
		if strings.Contains(conf, "listen 203.0.113.10:443 quic reuseport;") && strings.Contains(site, "443 quic") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no QUIC socket in the default server after an HTTP/3 site:\n%s\nsite:\n%s", conf, site)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The whole configuration: one default_server and one reuseport per
	// address and port.
	listen := regexp.MustCompile(`listen\s+(\S+)([^;]*);`)
	defaults, reuse := map[string]int{}, map[string]int{}
	for _, p := range f.agent.Files() {
		if !strings.HasPrefix(p, "/etc/nginx/monopanel/") || !strings.HasSuffix(p, ".conf") {
			continue
		}
		conf, _ := f.agent.File(p)
		for _, m := range listen.FindAllStringSubmatch(conf, -1) {
			proto := "tcp"
			if strings.Contains(m[2], "quic") {
				proto = "udp"
			}
			if strings.Contains(m[2], "default_server") {
				defaults[proto+" "+m[1]]++
			}
			if strings.Contains(m[2], "reuseport") {
				reuse[proto+" "+m[1]]++
			}
		}
	}
	for _, k := range []string{"tcp 203.0.113.10:80", "tcp 203.0.113.10:443", "tcp [2001:db8::10]:80", "tcp [2001:db8::10]:443"} {
		if defaults[k] != 1 {
			t.Errorf("%s: %d default_server", k, defaults[k])
		}
	}
	for _, k := range []string{"udp 203.0.113.10:443", "udp [2001:db8::10]:443"} {
		if reuse[k] != 1 {
			t.Errorf("%s: %d reuseport", k, reuse[k])
		}
	}
	if len(defaults) != 4 || len(reuse) != 2 {
		t.Errorf("default_server %v, reuseport %v", defaults, reuse)
	}
}

// quicSites are the HTTP/3 sites of the QUIC tests: a certificate each,
// HTTP/3 on, alex's.
func quicSites(t *testing.T, f *siteFixture, domains ...string) {
	t.Helper()
	f.s.SetHostIPs(func() []string { return []string{"203.0.113.10"} })
	for _, d := range domains {
		cert, key := testCert(t, d)
		f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": cert, "private_key": key}, http.StatusCreated, nil)
		f.createSite(map[string]any{"domain": d, "user": "alex", "php_version": "8.4", "ssl": "auto", "ip": "203.0.113.10", "http3": true})
	}
}

func (f *siteFixture) defaultQuic() bool {
	conf, _ := f.agent.File("/etc/nginx/monopanel/http.d/ip-203.0.113.10.conf")
	return strings.Contains(conf, "quic reuseport")
}

func (f *siteFixture) siteJob(method, path string, body any) {
	f.t.Helper()
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(method, path, body, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != "done" {
		f.t.Fatalf("%s %s: %s %s", method, path, job.Status, job.Error)
	}
}

// The default servers let the QUIC sockets go once the last site stops
// serving HTTP/3 — turned off, or the site removed — not only at the API's
// next start; while another still serves it they stay.
func TestDefaultServersReleaseQUICWithTheLastHTTP3Site(t *testing.T) {
	f := newSiteFixture(t)
	quicSites(t, f, "a.example.com", "b.example.com")
	if !f.defaultQuic() {
		t.Fatal("no QUIC socket with two HTTP/3 sites")
	}
	f.siteJob(http.MethodPatch, "/sites/a.example.com", map[string]any{"http3": false})
	if !f.defaultQuic() {
		t.Error("the QUIC socket went while b still serves HTTP/3")
	}
	f.siteJob(http.MethodPatch, "/sites/b.example.com", map[string]any{"http3": false})
	if f.defaultQuic() {
		t.Error("the QUIC socket stays with no site on HTTP/3")
	}
	// On again, then the site removed.
	f.siteJob(http.MethodPatch, "/sites/b.example.com", map[string]any{"http3": true})
	if !f.defaultQuic() {
		t.Fatal("no QUIC socket after HTTP/3 on again")
	}
	f.siteJob(http.MethodDelete, "/sites/b.example.com", nil)
	if f.defaultQuic() {
		t.Error("the QUIC socket stays after the last HTTP/3 site was removed")
	}
}

// The account's removal takes its HTTP/3 sites' QUIC sockets along.
func TestDefaultServersReleaseQUICWithTheAccount(t *testing.T) {
	f := newSiteFixture(t)
	quicSites(t, f, "a.example.com")
	if !f.defaultQuic() {
		t.Fatal("no QUIC socket")
	}
	f.siteJob(http.MethodDelete, "/users/alex", nil)
	if f.defaultQuic() {
		t.Error("the QUIC socket stays after the account went")
	}
}

// A suspended HTTP/3 site serves no QUIC (its stub listens on TCP), so it
// holds neither the sockets nor the firewall's port; an apply that changes
// nothing writes no default servers, so nginx -t does not run for them.
func TestDefaultServersQUICFollowsWhatNginxServes(t *testing.T) {
	f := newSiteFixture(t)
	quicSites(t, f, "a.example.com")
	sets := func() int {
		n := 0
		for _, s := range f.agent.ConfigSets() {
			if s.Origin == "stack:nginx" {
				n++
			}
		}
		return n
	}
	before := sets()
	f.siteJob(http.MethodPost, "/sites/a.example.com/apply", nil)
	if sets() != before {
		t.Error("an apply with nothing to change wrote the default servers")
	}
	f.siteJob(http.MethodPost, "/sites/a.example.com/suspend", nil)
	if f.defaultQuic() || f.s.anySiteHTTP3(f.ctx) {
		t.Error("a suspended site keeps the QUIC socket")
	}
	f.siteJob(http.MethodPost, "/sites/a.example.com/unsuspend", nil)
	if !f.defaultQuic() {
		t.Error("no QUIC socket after the site came back")
	}
}
