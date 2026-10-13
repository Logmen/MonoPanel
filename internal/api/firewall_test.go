package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/store"
)

// The chain is assembled in three tiers: per-source allows (exceptions),
// then every deny, then the always-open ports and port-wide allows. That is
// what makes "deny 8443 + allow 8443 from the VPN" mean "panel only from the
// VPN" instead of "panel closed for everyone".
func TestFirewallPlanTiers(t *testing.T) {
	rules := []*store.FirewallRule{
		{ID: 1, Kind: "allow", Proto: "tcp", Port: "25", Enabled: true},
		{ID: 2, Kind: "deny", Proto: "tcp", Port: "8443", Enabled: true},
		{ID: 3, Kind: "allow", Proto: "tcp", Port: "8443", Source: "31.3.209.5", Enabled: true},
		{ID: 4, Kind: "deny", Proto: "any", Source: "203.0.113.7", Enabled: true},
		{ID: 5, Kind: "allow", Proto: "udp", Port: "5000-5100", Source: "2001:db8::/32", Enabled: false},
	}
	p := planFirewall(rules)
	want := func(name string, got, exp []string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(exp, "|") {
			t.Errorf("%s = %q, want %q", name, got, exp)
		}
	}
	want("except", p.except, []string{"ip saddr 31.3.209.5 tcp dport 8443 accept"})
	want("deny", p.deny, []string{"tcp dport 8443 drop", "ip saddr 203.0.113.7 drop"})
	want("allow", p.allow, []string{"tcp dport 25 accept"})
}

func TestFirewallPortAndSourceMatching(t *testing.T) {
	for _, c := range []struct {
		spec string
		port int
		ok   bool
	}{{"", 22, true}, {"22", 22, true}, {"22", 23, false}, {"8000-9000", 8443, true}, {"8000-9000", 7999, false}, {"x", 22, false}} {
		if got := portCovers(c.spec, c.port); got != c.ok {
			t.Errorf("portCovers(%q, %d) = %v", c.spec, c.port, got)
		}
	}
	for _, c := range []struct {
		src, ip string
		ok      bool
	}{{"10.0.0.0/8", "10.20.30.40", true}, {"10.0.0.0/8", "11.0.0.1", false}, {"203.0.113.7", "203.0.113.7", true}, {"203.0.113.7", "203.0.113.8", false}, {"::1", "local", false}} {
		if got := sourceHas(c.src, c.ip); got != c.ok {
			t.Errorf("sourceHas(%q, %q) = %v", c.src, c.ip, got)
		}
	}
}

func TestFirewallRestrictedPorts(t *testing.T) {
	rules := []*store.FirewallRule{
		{ID: 1, Kind: "deny", Proto: "tcp", Port: "8443", Enabled: true},
		{ID: 2, Kind: "allow", Proto: "tcp", Port: "8443", Source: "31.3.209.5", Enabled: true},
		{ID: 3, Kind: "allow", Proto: "any", Source: "83.97.77.254", Enabled: true}, // every port from there
		{ID: 4, Kind: "deny", Proto: "tcp", Port: "20-30", Enabled: true},
	}
	got := firewallRestricted(rules, []int{22, 80, 443, 8443})
	if len(got) != 2 || got[0].Port != 22 || len(got[0].Sources) != 1 || got[0].Sources[0] != "83.97.77.254" ||
		got[1].Port != 8443 || strings.Join(got[1].Sources, ",") != "31.3.209.5,83.97.77.254" {
		t.Fatalf("restricted = %+v", got)
	}
}

// fwCall is a raw request with the fixture's admin session; the guard's
// refusals come back as problem+json, so the detail is returned for asserts.
func fwCall(f *siteFixture, method, path string, body any) (int, string) {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body) //nolint:errcheck // test fixtures are encodable
	}
	req, _ := http.NewRequest(method, f.ts.URL+"/api/v1"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck // test
	var out struct {
		Detail string `json:"detail"`
	}
	json.NewDecoder(res.Body).Decode(&out) //nolint:errcheck // 204 has no body
	return res.StatusCode, out.Detail
}

// A deny that would lock the administrator out is refused with a hint; the
// same set in the right order is accepted, and the last exception cannot be
// removed while the deny stands.
func TestFirewallGuardRefusesLockout(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	deny := func(port, source string) (int, string) {
		return fwCall(f, http.MethodPost, "/firewall/rules", map[string]any{"kind": "deny", "proto": "tcp", "port": port, "source": source})
	}
	if code, detail := deny("8443", ""); code != 422 || !strings.Contains(detail, "allow") || !strings.Contains(detail, "127.0.0.1") {
		t.Fatalf("panel deny without exceptions: %d %q", code, detail)
	}
	if code, detail := deny("20-30", ""); code != 422 || !strings.Contains(detail, "SSH") {
		t.Fatalf("ssh range deny: %d %q", code, detail)
	}
	if code, detail := deny("", "127.0.0.0/8"); code != 422 || !strings.Contains(detail, "127.0.0.1") {
		t.Fatalf("deny covering the caller: %d %q", code, detail)
	}
	if code, detail := fwCall(f, http.MethodPost, "/firewall/ban", map[string]any{"ip": "127.0.0.1"}); code != 422 || !strings.Contains(detail, "your own address") {
		t.Fatalf("self-ban: %d %q", code, detail)
	}
	// 443 is open by default but not an access port: closing it is allowed.
	if code, detail := deny("443", ""); code != 201 {
		t.Fatalf("deny 443: %d %q", code, detail)
	}
	if code, _ := fwCall(f, http.MethodPost, "/firewall/rules", map[string]any{"kind": "allow", "proto": "tcp", "port": "8443", "source": "198.51.100.4"}); code != 201 {
		t.Fatalf("allow with source: %d", code)
	}
	if code, detail := deny("8443", ""); code != 201 {
		t.Fatalf("deny after the exception: %d %q", code, detail)
	}
	rules, _ := f.db.ListFirewallRules(f.ctx)
	var allowID, denyID int64
	for _, r := range rules {
		if r.Port == "8443" && r.Kind == "allow" {
			allowID = r.ID
		}
		if r.Port == "8443" && r.Kind == "deny" {
			denyID = r.ID
		}
	}
	if code, detail := fwCall(f, http.MethodDelete, "/firewall/rules/"+itoa(allowID), nil); code != 409 || !strings.Contains(detail, "first delete deny #"+itoa(denyID)) {
		t.Fatalf("deleting the last exception: %d %q", code, detail)
	}
	if code, _ := fwCall(f, http.MethodDelete, "/firewall/rules/"+itoa(denyID), nil); code != 204 {
		t.Fatalf("deleting the deny: %d", code)
	}
	if code, _ := fwCall(f, http.MethodDelete, "/firewall/rules/"+itoa(allowID), nil); code != 204 {
		t.Fatalf("deleting the allow afterwards: %d", code)
	}
}

// Applying renders the exception above the deny and reports the restricted
// port with its sources.
func TestFirewallApplyOrdersExceptionsFirst(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	if code, _ := fwCall(f, http.MethodPost, "/firewall/rules", map[string]any{"kind": "allow", "proto": "tcp", "port": "8443", "source": "198.51.100.4", "comment": "VPN"}); code != 201 {
		t.Fatal("allow")
	}
	if code, detail := fwCall(f, http.MethodPost, "/firewall/rules", map[string]any{"kind": "deny", "proto": "tcp", "port": "8443"}); code != 201 {
		t.Fatalf("deny: %d %q", code, detail)
	}
	if code, detail := fwCall(f, http.MethodPost, "/firewall/enable", nil); code != 200 {
		t.Fatalf("enable: %d %q", code, detail)
	}
	text, ok := f.agent.File(nftRulesPath)
	if !ok {
		t.Fatalf("no nft rules written; files: %v", f.agent.Files())
	}
	except := strings.Index(text, "ip saddr 198.51.100.4 tcp dport 8443 accept")
	drop := strings.Index(text, "tcp dport 8443 drop")
	open := strings.Index(text, "8443 } accept")
	if except < 0 || drop < 0 || open < 0 || !(except < drop && drop < open) {
		t.Fatalf("rule order wrong:\n%s", text)
	}
	var st struct {
		Restricted []struct {
			Port    int      `json:"port"`
			Sources []string `json:"sources"`
		} `json:"restricted"`
	}
	f.call(http.MethodGet, "/firewall", nil, http.StatusOK, &st)
	if len(st.Restricted) != 1 || st.Restricted[0].Port != 8443 || strings.Join(st.Restricted[0].Sources, ",") != "198.51.100.4" {
		t.Fatalf("restricted = %+v", st.Restricted)
	}
}

// Unbanning from the fail2ban list does just that: with the panel's firewall
// off it neither switches the firewall on nor retires firewalld (which on EL
// took fail2ban down with it, ozarign.kz 13.10.2026). A ban is a deny rule and
// needs the firewall, so it is refused while the firewall is off.
func TestFirewallUnbanLeavesFirewallOff(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	f.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name == "firewall-cmd" && len(req.Args) == 1 && req.Args[0] == "--state" {
			return &agentRes{ExitCode: 0, Output: "running\n"}
		}
		return nil
	}
	if code, detail := fwCall(f, http.MethodPost, "/firewall/unban", map[string]any{"ip": "203.0.113.7"}); code != 200 {
		t.Fatalf("unban: %d %q", code, detail)
	}
	unbanned := false
	for _, tool := range f.agent.Tools() {
		args := strings.Join(tool.Args, " ")
		if tool.Name == "fail2ban-client" && args == "unban 203.0.113.7" {
			unbanned = true
		}
		if tool.Name == "nft" && strings.Contains(args, "-f") {
			t.Fatalf("the firewall was applied: nft %s", args)
		}
	}
	if !unbanned {
		t.Fatal("fail2ban was not asked to unban the address")
	}
	if a := f.agent.UnitAction("firewalld.service"); a != "" {
		t.Fatalf("firewalld touched: %s", a)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingFirewall); v == "yes" {
		t.Fatal("unban switched the firewall on")
	}
	if code, detail := fwCall(f, http.MethodPost, "/firewall/ban", map[string]any{"ip": "203.0.113.8"}); code != 409 || !strings.Contains(detail, "firewall is off") {
		t.Fatalf("ban with the firewall off: %d %q", code, detail)
	}
	if rules, _ := f.db.ListFirewallRules(f.ctx); len(rules) != 0 {
		t.Fatalf("a refused ban left a rule: %+v", rules)
	}
}

// Enabling the firewall retires firewalld; the EL fail2ban unit is
// PartOf=firewalld.service and stops with it, so the panel starts it again.
// Without firewalld fail2ban is not touched.
func TestFirewallEnableBringsFail2banBack(t *testing.T) {
	f := newSiteFixture(t)
	f.login()
	f.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name == "firewall-cmd" && len(req.Args) == 1 && req.Args[0] == "--state" {
			return &agentRes{ExitCode: 0, Output: "running\n"}
		}
		return nil
	}
	if code, detail := fwCall(f, http.MethodPost, "/firewall/enable", nil); code != 200 {
		t.Fatalf("enable: %d %q", code, detail)
	}
	if a := f.agent.UnitAction("firewalld.service"); a != "disable" {
		t.Fatalf("firewalld: %q", a)
	}
	if a := f.agent.UnitAction(fail2banService); a != "start" {
		t.Fatalf("fail2ban after firewalld was retired: %q", a)
	}

	g := newSiteFixture(t)
	g.login()
	if code, detail := fwCall(g, http.MethodPost, "/firewall/enable", nil); code != 200 {
		t.Fatalf("enable without firewalld: %d %q", code, detail)
	}
	if a := g.agent.UnitAction(fail2banService); a != "" {
		t.Fatalf("fail2ban touched without firewalld: %q", a)
	}
}
