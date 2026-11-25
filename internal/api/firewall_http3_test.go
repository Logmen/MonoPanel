package api

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeFirewalld answers firewall-cmd as a running firewalld with ports open:
// --query-port, --add-port and --remove-port act on them.
type fakeFirewalld struct {
	mu      sync.Mutex
	running bool
	open    map[string]bool
}

func (fw *fakeFirewalld) hook(req agentReq) *agentRes {
	if req.Name != "firewall-cmd" {
		return nil
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	a := req.Args
	switch {
	case len(a) == 1 && a[0] == "--state":
		if fw.running {
			return &agentRes{Output: "running\n"}
		}
		return &agentRes{ExitCode: 252, Output: "not running\n"}
	case len(a) == 2 && strings.HasPrefix(a[1], "--query-port="):
		if fw.open[strings.TrimPrefix(a[1], "--query-port=")] {
			return &agentRes{}
		}
		return &agentRes{ExitCode: 1}
	}
	for _, x := range a {
		if p, ok := strings.CutPrefix(x, "--add-port="); ok {
			fw.open[p] = true
		}
		if p, ok := strings.CutPrefix(x, "--remove-port="); ok {
			delete(fw.open, p)
		}
	}
	return &agentRes{}
}

// firewallCalls are the firewall-cmd and nft calls the fake agent saw from
// the n-th tool call on, joined.
func firewallCalls(f *siteFixture, n int) []string {
	var out []string
	for _, t := range f.agent.Tools()[n:] {
		if t.Name == "firewall-cmd" || t.Name == "nft" {
			out = append(out, t.Name+" "+strings.Join(t.Args, " "))
		}
	}
	return out
}

// http3Site makes h3.example.com with a certificate, HTTP/3 as asked.
func http3Site(t *testing.T, f *siteFixture, http3 bool) {
	t.Helper()
	cert, key := testCert(t, "h3.example.com")
	f.call(http.MethodPost, "/certificates/import", map[string]any{"certificate": cert, "private_key": key}, http.StatusCreated, nil)
	f.createSite(map[string]any{"domain": "h3.example.com", "user": "alex", "php_version": "8.4", "ssl": "auto", "http3": http3})
}

func setHTTP3(t *testing.T, f *siteFixture, on bool) {
	t.Helper()
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPatch, "/sites/h3.example.com", map[string]any{"http3": on}, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
}

// A running firewalld gets udp/443 once a site serves HTTP/3, nothing more
// while it is open, and loses it — the panel opened it — once no site does.
func TestHTTP3OpensAndClosesUDP443InFirewalld(t *testing.T) {
	f := newSiteFixture(t)
	fw := &fakeFirewalld{running: true, open: map[string]bool{"80/tcp": true, "443/tcp": true}}
	f.agent.ToolHook = fw.hook
	n := len(f.agent.Tools())
	http3Site(t, f, true)
	if !fw.open["443/udp"] {
		t.Fatalf("udp/443 not opened: %v", firewallCalls(f, n))
	}
	if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "firewalld" {
		t.Errorf("marker %q", v)
	}
	// Open already: an apply only asks.
	n = len(f.agent.Tools())
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/h3.example.com/apply", nil, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	for _, c := range firewallCalls(f, n) {
		if strings.Contains(c, "--add-port") || strings.Contains(c, "--reload") {
			t.Errorf("changed while open: %s", c)
		}
	}
	setHTTP3(t, f, false)
	if fw.open["443/udp"] {
		t.Error("udp/443 stays open with no site on HTTP/3")
	}
	if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "" {
		t.Errorf("marker %q after closing", v)
	}
}

// A udp/443 the panel did not open stays when no site serves HTTP/3; with no
// site on HTTP/3 and nothing opened a site apply asks the firewall nothing.
func TestHTTP3LeavesAForeignUDP443AndIdleHostsAlone(t *testing.T) {
	f := newSiteFixture(t)
	fw := &fakeFirewalld{running: true, open: map[string]bool{"443/udp": true}}
	f.agent.ToolHook = fw.hook
	n := len(f.agent.Tools())
	http3Site(t, f, false)
	if calls := firewallCalls(f, n); len(calls) > 0 {
		t.Errorf("a site without HTTP/3 talked to the firewall: %v", calls)
	}
	setHTTP3(t, f, true)
	if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "" {
		t.Errorf("marked as opened by the panel: %q", v)
	}
	setHTTP3(t, f, false)
	if !fw.open["443/udp"] {
		t.Error("closed a udp/443 the panel did not open")
	}
}

// No firewalld and the panel's firewall off: nothing to open, nothing tried.
func TestHTTP3WithoutAHostFirewall(t *testing.T) {
	f := newSiteFixture(t)
	fw := &fakeFirewalld{open: map[string]bool{}}
	f.agent.ToolHook = fw.hook
	n := len(f.agent.Tools())
	http3Site(t, f, true)
	for _, c := range firewallCalls(f, n) {
		if c != "firewall-cmd --state" {
			t.Errorf("unexpected: %s", c)
		}
	}
	if c := f.s.http3Check(f.ctx); c == nil || c.Status != "ok" {
		t.Errorf("doctor: %+v", c)
	}
}

// The panel's nftables table: re-applied once a site serves HTTP/3, not again
// while it lets udp/443 in, and once more when no site does.
func TestHTTP3ReappliesThePanelFirewall(t *testing.T) {
	f := newSiteFixture(t)
	if err := f.db.SetSetting(f.ctx, settingFirewall, "yes"); err != nil {
		t.Fatal(err)
	}
	// The hook runs under the fake agent's lock: what a load puts in the
	// kernel is read from the store (what applyFirewall renders from), the
	// file itself after the job.
	var mu sync.Mutex
	udp := false
	f.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name != "nft" {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		switch strings.Join(req.Args, " ") {
		case "list table inet monopanel":
			if udp {
				return &agentRes{Output: "table inet monopanel {\n\tudp dport 443 accept\n}\n"}
			}
			return &agentRes{Output: "table inet monopanel {\n}\n"}
		case "-f " + nftRulesPath:
			udp = f.s.anySiteHTTP3(f.ctx)
		}
		return &agentRes{}
	}
	table := func() string { s, _ := f.agent.File(nftRulesPath); return s }
	loads := func(n int) int {
		c := 0
		for _, x := range firewallCalls(f, n) {
			if x == "nft -f "+nftRulesPath {
				c++
			}
		}
		return c
	}
	n := len(f.agent.Tools())
	http3Site(t, f, true)
	if loads(n) != 1 || !strings.Contains(table(), "udp dport 443 accept") {
		t.Fatalf("loads %d, table:\n%s", loads(n), table())
	}
	if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "nft" {
		t.Errorf("marker %q", v)
	}
	n = len(f.agent.Tools())
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/h3.example.com/apply", nil, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	if loads(n) != 0 {
		t.Error("re-applied with udp/443 open already")
	}
	if c := f.s.http3Check(f.ctx); c == nil || c.Status != "ok" {
		t.Errorf("doctor: %+v", c)
	}
	n = len(f.agent.Tools())
	setHTTP3(t, f, false)
	if loads(n) != 1 || strings.Contains(table(), "udp dport 443") {
		t.Errorf("after HTTP/3 off: loads %d, table:\n%s", loads(n), table())
	}
}

// The doctor warns while a site serves HTTP/3 and firewalld keeps udp/443
// closed (the panel could not open it).
func TestHTTP3DoctorWarnsWhenFirewalldKeepsItClosed(t *testing.T) {
	f := newSiteFixture(t)
	fw := &fakeFirewalld{running: true, open: map[string]bool{}}
	f.agent.ToolHook = fw.hook
	http3Site(t, f, true)
	fw.mu.Lock()
	delete(fw.open, "443/udp")
	fw.mu.Unlock()
	c := f.s.http3Check(f.ctx)
	if c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "udp/443 is closed in firewalld") {
		t.Errorf("doctor: %+v", c)
	}
}
