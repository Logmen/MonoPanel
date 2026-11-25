package api

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeFirewalld answers firewall-cmd as firewalld does, with its two
// configurations apart: runtime (what works now) and permanent (what a
// reload or a reboot brings back). --query-port, --add-port and
// --remove-port act on one of them, --permanent says which; --reload
// replaces runtime with permanent.
type fakeFirewalld struct {
	mu        sync.Mutex
	running   bool
	runtime   map[string]bool
	permanent map[string]bool
}

// newFakeFirewalld is a firewalld with ports open on both sides.
func newFakeFirewalld(running bool, ports ...string) *fakeFirewalld {
	fw := &fakeFirewalld{running: running, runtime: map[string]bool{}, permanent: map[string]bool{}}
	for _, p := range ports {
		fw.runtime[p], fw.permanent[p] = true, true
	}
	return fw
}

func (fw *fakeFirewalld) hook(req agentReq) *agentRes {
	if req.Name != "firewall-cmd" {
		return nil
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	a := req.Args
	if len(a) == 1 && a[0] == "--state" {
		if fw.running {
			return &agentRes{Output: "running\n"}
		}
		return &agentRes{ExitCode: 252, Output: "not running\n"}
	}
	if len(a) > 0 && a[0] == "-q" {
		a = a[1:]
	}
	set := fw.runtime
	if len(a) > 0 && a[0] == "--permanent" {
		set, a = fw.permanent, a[1:]
	}
	if len(a) == 1 && a[0] == "--reload" {
		fw.runtime = map[string]bool{}
		for p := range fw.permanent {
			fw.runtime[p] = true
		}
		return &agentRes{}
	}
	if len(a) == 1 && strings.HasPrefix(a[0], "--query-port=") {
		if set[strings.TrimPrefix(a[0], "--query-port=")] {
			return &agentRes{}
		}
		return &agentRes{ExitCode: 1}
	}
	for _, x := range a {
		if p, ok := strings.CutPrefix(x, "--add-port="); ok {
			set[p] = true
		}
		if p, ok := strings.CutPrefix(x, "--remove-port="); ok {
			delete(set, p)
		}
	}
	return &agentRes{}
}

// state is a port's two sides, under the lock.
func (fw *fakeFirewalld) state(port string) (runtime, permanent bool) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.runtime[port], fw.permanent[port]
}

// set puts a port's two sides as an admin would, under the lock.
func (fw *fakeFirewalld) set(port string, runtime, permanent bool) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.runtime[port], fw.permanent[port] = runtime, permanent
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
	fw := newFakeFirewalld(true, "80/tcp", "443/tcp")
	f.agent.ToolHook = fw.hook
	n := len(f.agent.Tools())
	http3Site(t, f, true)
	if rt, pm := fw.state("443/udp"); !rt || !pm {
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
	if rt, pm := fw.state("443/udp"); rt || pm {
		t.Errorf("udp/443 stays open with no site on HTTP/3: runtime %v, permanent %v", rt, pm)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "" {
		t.Errorf("marker %q after closing", v)
	}
}

// A udp/443 the panel did not open stays when no site serves HTTP/3; with no
// site on HTTP/3 and nothing opened a site apply asks the firewall nothing.
func TestHTTP3LeavesAForeignUDP443AndIdleHostsAlone(t *testing.T) {
	f := newSiteFixture(t)
	fw := newFakeFirewalld(true, "443/udp")
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
	if rt, pm := fw.state("443/udp"); !rt || !pm {
		t.Errorf("closed a udp/443 the panel did not open: runtime %v, permanent %v", rt, pm)
	}
}

// No firewalld and the panel's firewall off: nothing to open, nothing tried.
func TestHTTP3WithoutAHostFirewall(t *testing.T) {
	f := newSiteFixture(t)
	fw := newFakeFirewalld(false)
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
	fw := newFakeFirewalld(true)
	f.agent.ToolHook = fw.hook
	http3Site(t, f, true)
	fw.set("443/udp", false, true)
	c := f.s.http3Check(f.ctx)
	if c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "udp/443 is closed in firewalld") {
		t.Errorf("doctor: %+v", c)
	}
}

// firewalld keeps runtime and permanent apart until a reload. The panel looks
// at both: it marks as its own only a permanent rule it added (an admin's
// --permanent not reloaded yet stays theirs), opens the runtime side without
// a reload — that would drop everybody else's runtime rules — and closes
// both sides of its own rule.
func TestHTTP3FirewalldRuntimeAndPermanent(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		runtime, permanent       bool   // udp/443 before HTTP/3
		marker                   string // once a site serves HTTP/3
		offRuntime, offPermanent bool   // once none does
	}{
		{"closed", false, false, "firewalld", false, false},
		{"an admin's permanent rule, not reloaded", false, true, "", true, true},
		{"open until the next reload only", true, false, "firewalld", false, false},
		{"open", true, true, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiteFixture(t)
			fw := newFakeFirewalld(true)
			fw.set("443/udp", tc.runtime, tc.permanent)
			f.agent.ToolHook = fw.hook
			n := len(f.agent.Tools())
			http3Site(t, f, true)
			if rt, pm := fw.state("443/udp"); !rt || !pm {
				t.Errorf("HTTP/3 on: runtime %v, permanent %v; calls %v", rt, pm, firewallCalls(f, n))
			}
			if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != tc.marker {
				t.Errorf("marker %q, want %q", v, tc.marker)
			}
			if c := f.s.http3Check(f.ctx); c == nil || c.Status != "ok" {
				t.Errorf("doctor: %+v", c)
			}
			setHTTP3(t, f, false)
			if rt, pm := fw.state("443/udp"); rt != tc.offRuntime || pm != tc.offPermanent {
				t.Errorf("HTTP/3 off: runtime %v, permanent %v; want %v, %v", rt, pm, tc.offRuntime, tc.offPermanent)
			}
			if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "" {
				t.Errorf("marker %q after HTTP/3 off", v)
			}
			for _, c := range firewallCalls(f, n) {
				if strings.Contains(c, "--reload") {
					t.Errorf("a reload drops other runtime rules: %s", c)
				}
			}
		})
	}
}

// The panel's rule loses its runtime side by hand: closing still removes the
// permanent one, which the next reload or reboot would bring back.
func TestHTTP3ClosesThePermanentRuleWithoutItsRuntimeSide(t *testing.T) {
	f := newSiteFixture(t)
	fw := newFakeFirewalld(true)
	f.agent.ToolHook = fw.hook
	http3Site(t, f, true)
	fw.set("443/udp", false, true)
	setHTTP3(t, f, false)
	if rt, pm := fw.state("443/udp"); rt || pm {
		t.Errorf("runtime %v, permanent %v", rt, pm)
	}
}

// The doctor tells a udp/443 open until the next reload from one open for
// good, and a site apply makes it permanent.
func TestHTTP3DoctorWarnsAboutARuntimeOnlyPort(t *testing.T) {
	f := newSiteFixture(t)
	fw := newFakeFirewalld(true)
	f.agent.ToolHook = fw.hook
	http3Site(t, f, true)
	fw.set("443/udp", true, false)
	c := f.s.http3Check(f.ctx)
	if c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "until the next reload") {
		t.Fatalf("doctor: %+v", c)
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/h3.example.com/apply", nil, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	if _, pm := fw.state("443/udp"); !pm {
		t.Error("the apply left udp/443 open until the next reload only")
	}
	if c := f.s.http3Check(f.ctx); c == nil || c.Status != "ok" {
		t.Errorf("doctor after the apply: %+v", c)
	}
}

// firewalld stopped when the last site drops HTTP/3: the marker waits, and
// the first apply once it runs again closes the panel's rule.
func TestHTTP3ClosesOnceFirewalldRunsAgain(t *testing.T) {
	f := newSiteFixture(t)
	fw := newFakeFirewalld(true)
	f.agent.ToolHook = fw.hook
	http3Site(t, f, true)
	fw.mu.Lock()
	fw.running = false
	fw.mu.Unlock()
	setHTTP3(t, f, false)
	if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "firewalld" {
		t.Fatalf("marker %q while firewalld is stopped", v)
	}
	fw.mu.Lock()
	fw.running = true
	fw.mu.Unlock()
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/h3.example.com/apply", nil, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	if rt, pm := fw.state("443/udp"); rt || pm {
		t.Errorf("runtime %v, permanent %v", rt, pm)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingHTTP3Port); v != "" {
		t.Errorf("marker %q", v)
	}
}
