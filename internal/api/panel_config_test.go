package api

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/config"
	"monopanel/internal/store"
)

// newConfigFixture is the fixture with its configuration loaded from a real
// config.yaml in a temporary directory, the way monopanel api starts: the
// file GET /system/config reads and the fake agent edits.
func newConfigFixture(t *testing.T, edit func(*config.Config)) (*siteFixture, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	f := newFixture(t, func(c *config.Config) {
		c.TemplatesDir = t.TempDir()
		if edit != nil {
			edit(c)
		}
		if err := c.Save(path); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		*c = loaded
	})
	f.s.restartDelay = 0
	return f, path
}

// freePort is a TCP port nothing listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitUnitAction(f *siteFixture, unit, want string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.agent.UnitAction(unit) == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// GET /system/config reads the file on every call: what root changed by hand
// shows at once, with restart_pending until the API restarts.
func TestPanelConfigReadsTheFile(t *testing.T) {
	f, path := newConfigFixture(t, func(c *config.Config) {
		c.Web.Hostname = "panel.example.com"
		c.Update.PublicKey = "none"
	})
	var pc apitypes.PanelConfig
	f.call(http.MethodGet, "/system/config", nil, http.StatusOK, &pc)
	if pc.Path != path || pc.Hostname != "panel.example.com" || pc.Listen != ":8443" || pc.LogLevel != "info" || pc.LogFormat != "text" || pc.JobsWorkers != 2 ||
		pc.DataDir != f.s.cfg.DataDir || pc.TemplatesDir != f.s.cfg.TemplatesDir || pc.KeyPinned || pc.RestartPending {
		t.Fatalf("config: %+v", pc)
	}

	raw, _ := os.ReadFile(path)
	edited, _, err := config.EditPanelKeys(raw, map[string]string{config.KeyLogLevel: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o640); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodGet, "/system/config", nil, http.StatusOK, &pc)
	if pc.LogLevel != "debug" || !pc.RestartPending {
		t.Fatalf("after a change by hand: %+v", pc)
	}

	// The built-in release key counts as pinned, as in GET /system/update.
	g, _ := newConfigFixture(t, nil)
	g.call(http.MethodGet, "/system/config", nil, http.StatusOK, &pc)
	var up apitypes.UpdateStatus
	g.call(http.MethodGet, "/system/update", nil, http.StatusOK, &up)
	if pc.KeyPinned != up.KeyPinned || !pc.KeyPinned {
		t.Fatalf("key_pinned %v, update status says %v", pc.KeyPinned, up.KeyPinned)
	}
}

// PUT /system/config hands the agent the keys that change, and only those;
// it answers with the file as it is now and records who changed what.
func TestPanelConfigSetWritesThroughTheAgent(t *testing.T) {
	f, path := newConfigFixture(t, nil)
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	var res apitypes.PanelConfigResult
	f.call(http.MethodPut, "/system/config", map[string]any{"hostname": "Panel.Example.com.", "listen": listen, "log_level": "info", "log_format": "json", "jobs_workers": 4}, http.StatusOK, &res)

	calls := f.agent.PanelConfigs()
	if len(calls) != 1 || calls[0].Path != path {
		t.Fatalf("agent calls: %+v", calls)
	}
	if set := calls[0].Set; len(set) != 4 || set["web.hostname"] != "panel.example.com" || set["web.listen"] != listen || set["log.format"] != "json" || set["jobs.workers"] != "4" {
		t.Fatalf("sent to the agent: %+v", set)
	}
	c := res.Config
	if res.Restarted || !c.RestartPending || c.Hostname != "panel.example.com" || c.Listen != listen || c.LogFormat != "json" || c.JobsWorkers != 4 || c.LogLevel != "info" {
		t.Fatalf("answer: %+v", res)
	}
	back, err := config.Load(path)
	if err != nil || back.Web.Hostname != "panel.example.com" || back.Jobs.Workers != 4 {
		t.Fatalf("file: %+v %v", back.Web, err)
	}
	if a := f.agent.UnitAction("monopanel-api.service"); a != "" {
		t.Fatalf("restarted without being asked: %s", a)
	}
	entries, _ := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "config.set", Limit: 10})
	if len(entries) != 1 || entries[0].Actor != "admin" || entries[0].Target != path {
		t.Fatalf("audit: %+v", entries)
	}
	changes, _ := entries[0].Details["changes"].(map[string]any)
	if len(changes) != 4 || changes["web.listen"] == nil {
		t.Fatalf("audit details: %+v", entries[0].Details)
	}

	// The same form again changes nothing and bothers nobody.
	f.call(http.MethodPut, "/system/config", map[string]any{"hostname": "panel.example.com", "listen": listen, "log_level": "info", "log_format": "json", "jobs_workers": 4}, http.StatusOK, &res)
	f.call(http.MethodPut, "/system/config", map[string]any{}, http.StatusOK, &res)
	if n := len(f.agent.PanelConfigs()); n != 1 {
		t.Fatalf("an unchanged form went to the agent: %d calls", n)
	}
	if entries, _ := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "config.set", Limit: 10}); len(entries) != 1 {
		t.Fatalf("an unchanged form was audited: %+v", entries)
	}
}

// While a move waits for the restart, GET /system/config answers with both
// addresses: the file's, where the panel comes back, and the running one,
// where every client still reaches it — not only the session that saved it.
func TestPanelConfigShowsTheRunningAddressWhileAMoveIsPending(t *testing.T) {
	f, path := newConfigFixture(t, func(c *config.Config) { c.Web.Hostname = "old.example.com" })
	var pc apitypes.PanelConfig
	f.call(http.MethodGet, "/system/config", nil, http.StatusOK, &pc)
	if pc.RestartPending || pc.Hostname != "old.example.com" || pc.RunningHostname != "old.example.com" || pc.Listen != ":8443" || pc.RunningListen != ":8443" {
		t.Fatalf("before the move: %+v", pc)
	}

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	var res apitypes.PanelConfigResult
	f.call(http.MethodPut, "/system/config", map[string]any{"hostname": "new.example.com", "listen": listen}, http.StatusOK, &res)
	pending := func(where string, c apitypes.PanelConfig) {
		t.Helper()
		if !c.RestartPending || c.Hostname != "new.example.com" || c.Listen != listen || c.RunningHostname != "old.example.com" || c.RunningListen != ":8443" {
			t.Fatalf("%s: %+v", where, c)
		}
	}
	pending("the answer to the change", res.Config)
	f.call(http.MethodGet, "/system/config", nil, http.StatusOK, &pc)
	pending("a later read", pc)

	// Root's hand edit of the file waits for the restart the same way.
	raw, _ := os.ReadFile(path)
	edited, _, err := config.EditPanelKeys(raw, map[string]string{config.KeyHostname: ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o640); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodGet, "/system/config", nil, http.StatusOK, &pc)
	if !pc.RestartPending || pc.Hostname != "" || pc.RunningHostname != "old.example.com" || pc.RunningListen != ":8443" {
		t.Fatalf("after a hand edit: %+v", pc)
	}
}

// A value that cannot work is refused before anything is written: the panel
// would not come back after the restart, and nobody could reach it to undo
// the change.
func TestPanelConfigRefusesWhatCannotWork(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	prevStart, prevCap := lowPortStart, canBindLowPorts
	lowPortStart, canBindLowPorts = func() int { return 1024 }, func() bool { return false }
	t.Cleanup(func() { lowPortStart, canBindLowPorts = prevStart, prevCap })

	for name, body := range map[string]map[string]any{
		"an IP as the host name":  {"hostname": "203.0.113.5"},
		"a broken host name":      {"hostname": "panel_1.example.com"},
		"no port":                 {"listen": "8443"},
		"port zero":               {"listen": ":0"},
		"port out of range":       {"listen": ":70000"},
		"a privileged port":       {"listen": ":443"},
		"an address of elsewhere": {"listen": "203.0.113.77:9443"},
		"a busy port":             {"listen": busy.Addr().String()},
		"unknown log level":       {"log_level": "verbose"},
		"unknown log format":      {"log_format": "xml"},
		"no workers":              {"jobs_workers": 0},
		"too many workers":        {"jobs_workers": 100},
	} {
		code, detail := fwCall(f, http.MethodPut, "/system/config", body)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, code, detail)
		}
	}
	if n := len(f.agent.PanelConfigs()); n != 0 {
		t.Fatalf("a refused change reached the agent: %d calls", n)
	}
	// With the capability (or a lowered ip_unprivileged_port_start) a low
	// port is fine; a name of this host is too.
	canBindLowPorts = func() bool { return true }
	f.call(http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf("localhost:%d", freePort(t))}, http.StatusOK, nil)
	if n := len(f.agent.PanelConfigs()); n != 1 {
		t.Fatalf("a good address did not reach the agent: %d calls", n)
	}
}

// restart: true restarts monopanel-api once the answer has gone out — the
// restart stops the very process that answers.
func TestPanelConfigRestartsAfterTheAnswer(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	var res apitypes.PanelConfigResult
	f.call(http.MethodPut, "/system/config", map[string]any{"log_level": "debug", "restart": true}, http.StatusOK, &res)
	if !res.Restarted || res.Config.LogLevel != "debug" {
		t.Fatalf("answer: %+v", res)
	}
	if !waitUnitAction(f, "monopanel-api.service", "restart") {
		t.Fatal("monopanel-api was not restarted")
	}
	entries, _ := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "config.set", Limit: 10})
	if len(entries) != 1 || entries[0].Details["restart"] != true {
		t.Fatalf("audit: %+v", entries)
	}

	// A restart alone applies what is pending.
	g, _ := newConfigFixture(t, nil)
	g.call(http.MethodPut, "/system/config", map[string]any{"restart": true}, http.StatusOK, &res)
	if !res.Restarted || len(g.agent.PanelConfigs()) != 0 || !waitUnitAction(g, "monopanel-api.service", "restart") {
		t.Fatalf("restart alone: %+v", res)
	}
}

// The handler's answer is written before anything scheduled after it runs.
func TestAfterResponseRunsOnceTheAnswerIsWritten(t *testing.T) {
	rec := httptest.NewRecorder()
	ran := make(chan string, 1)
	h := requestInfoMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		afterResponse(r.Context(), func() { ran <- rec.Body.String() })
		select {
		case <-ran:
			t.Error("ran before the handler returned")
		case <-time.After(20 * time.Millisecond):
		}
		_, _ = w.Write([]byte("the answer"))
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/system/config", nil))
	select {
	case body := <-ran:
		if body != "the answer" {
			t.Fatalf("ran with %q written", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not run")
	}
}

// Moving the panel to another port opens that port first: in the panel's own
// firewall when it is on, in firewalld when that runs. When the firewall
// cannot do it, nothing is written.
func TestPanelConfigOpensTheNewPort(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	if err := f.db.SetSetting(f.ctx, settingFirewall, "yes"); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	f.call(http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", port)}, http.StatusOK, nil)
	rules, _ := f.agent.File(nftRulesPath)
	if !strings.Contains(rules, strconv.Itoa(port)) || !strings.Contains(rules, "8443") {
		t.Fatalf("the firewall does not let both panel ports through:\n%s", rules)
	}
	if len(f.agent.PanelConfigs()) != 1 {
		t.Fatal("the change was not written")
	}

	g, _ := newConfigFixture(t, nil)
	g.agent.ToolHook = func(req agentReq) *agentRes {
		if req.Name == "firewall-cmd" && len(req.Args) == 1 && req.Args[0] == "--state" {
			return &agentRes{Output: "running\n"}
		}
		return nil
	}
	port = freePort(t)
	g.call(http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", port)}, http.StatusOK, nil)
	opened := false
	for _, tool := range g.agent.Tools() {
		if tool.Name == "firewall-cmd" && strings.Contains(strings.Join(tool.Args, " "), fmt.Sprintf("--add-port=%d/tcp", port)) {
			opened = true
		}
	}
	if !opened {
		t.Fatalf("firewalld was not told: %+v", g.agent.Tools())
	}

	h, _ := newConfigFixture(t, nil)
	h.db.SetSetting(h.ctx, settingFirewall, "yes") //nolint:errcheck // test setup
	h.agent.Fail["/v1/config/apply"] = "disk full"
	if code, detail := fwCall(h, http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", freePort(t)), "restart": true}); code != http.StatusBadGateway || !strings.Contains(detail, "nothing was changed") {
		t.Fatalf("firewall failure: %d %s", code, detail)
	}
	if len(h.agent.PanelConfigs()) != 0 || h.agent.UnitAction("monopanel-api.service") != "" {
		t.Fatal("a change the firewall could not follow went ahead")
	}
}

// "allow 8443 from the VPN, deny 8443" keeps the panel for the VPN; a move to
// a port without such rules would open it to everyone, and a move to a port a
// deny closes would lock the administrator out. Both wait until the rules of
// the new port say the same.
func TestPanelConfigMoveKeepsTheFirewallRules(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	rule := func(kind, port, source string) {
		t.Helper()
		if err := f.db.CreateFirewallRule(f.ctx, &store.FirewallRule{Kind: kind, Proto: "tcp", Port: port, Source: source, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	rule("allow", "8443", "203.0.113.5")
	rule("deny", "8443", "")
	to := freePort(t)
	code, detail := fwCall(f, http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", to)})
	if code != http.StatusConflict || !strings.Contains(detail, "keep port 8443 for 203.0.113.5 only") || !strings.Contains(detail, fmt.Sprintf("mp firewall allow --port %d --source 203.0.113.5", to)) {
		t.Fatalf("an open port for a restricted panel: %d %s", code, detail)
	}
	rule("allow", strconv.Itoa(to), "203.0.113.5")
	rule("deny", strconv.Itoa(to), "")
	f.call(http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", to)}, http.StatusOK, nil)

	g, _ := newConfigFixture(t, nil)
	closed := freePort(t)
	if err := g.db.CreateFirewallRule(g.ctx, &store.FirewallRule{Kind: "deny", Proto: "tcp", Port: strconv.Itoa(closed), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if code, detail := fwCall(g, http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", closed)}); code != http.StatusConflict || !strings.Contains(detail, fmt.Sprintf("the panel's new port %d to everyone", closed)) {
		t.Fatalf("a closed port: %d %s", code, detail)
	}
	if len(g.agent.PanelConfigs()) != 0 {
		t.Fatal("a refused move was written")
	}
}

// What the agent refuses comes back as the request's fault with the agent's
// reason; when the agent fails, nothing restarts.
func TestPanelConfigAgentRefusal(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	f.agent.Fail["/v1/panel/config"] = "config.yaml: the edit would change more than the requested keys"
	f.agent.FailStatus = map[string]int{"/v1/panel/config": http.StatusUnprocessableEntity}
	if code, detail := fwCall(f, http.MethodPut, "/system/config", map[string]any{"log_level": "debug", "restart": true}); code != http.StatusUnprocessableEntity || !strings.Contains(detail, "more than the requested keys") {
		t.Fatalf("refusal: %d %s", code, detail)
	}
	f.agent.FailStatus = nil
	if code, _ := fwCall(f, http.MethodPut, "/system/config", map[string]any{"log_level": "debug", "restart": true}); code != http.StatusBadGateway {
		t.Fatalf("agent failure: %d", code)
	}
	time.Sleep(50 * time.Millisecond)
	if a := f.agent.UnitAction("monopanel-api.service"); a != "" {
		t.Fatalf("restarted after a failed change: %s", a)
	}
	entries, _ := f.db.ListAudit(f.ctx, store.AuditFilter{Action: "config.set", Limit: 10})
	if len(entries) != 2 || entries[0].Result != "error" {
		t.Fatalf("failed changes are audited: %+v", entries)
	}
}

func TestPanelConfigIsForAdministrators(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	f.alexWithPassword()
	f.loginAs("alex", "alex-password")
	f.call(http.MethodGet, "/system/config", nil, http.StatusForbidden, nil)
	f.call(http.MethodPut, "/system/config", map[string]any{"log_level": "debug"}, http.StatusForbidden, nil)
	f.call(http.MethodGet, "/system/templates", nil, http.StatusForbidden, nil)
	if len(f.agent.PanelConfigs()) != 0 {
		t.Fatal("an ordinary account reached the agent")
	}
}

// The template list is the renderer's: every built-in template, and for the
// ones a file in templates_dir replaces, that file.
func TestSystemTemplatesShowOverrides(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	dir := f.s.cfg.TemplatesDir
	over := filepath.Join(dir, "nginx", "site.conf.tmpl")
	if err := os.MkdirAll(filepath.Dir(over), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte("# mine\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var list apitypes.TemplateList
	f.call(http.MethodGet, "/system/templates", nil, http.StatusOK, &list)
	if list.Dir != dir || len(list.Templates) < 10 {
		t.Fatalf("list: %s, %d templates", list.Dir, len(list.Templates))
	}
	seen := map[string]apitypes.TemplateInfo{}
	for _, tpl := range list.Templates {
		seen[tpl.Name] = tpl
	}
	if s := seen["nginx/site.conf.tmpl"]; !s.Override || s.Path != over {
		t.Fatalf("override: %+v", s)
	}
	if s, ok := seen["nftables/monopanel.nft.tmpl"]; !ok || s.Override || s.Path != "" {
		t.Fatalf("built-in: %+v", s)
	}
}

// mp firewall trust must be readable back also while fail2ban is not
// installed: the list is kept for it and shown at the top level.
func TestFirewallShowsTrustedWithoutFail2ban(t *testing.T) {
	f := newSiteFixture(t)
	var st apitypes.FirewallStatus
	f.call(http.MethodPost, "/firewall/trust", map[string]any{"ip": "198.51.100.7"}, http.StatusOK, &st)
	if st.Fail2ban != nil || strings.Join(st.Trusted, " ") != "198.51.100.7" {
		t.Fatalf("after trust: %+v %+v", st.Trusted, st.Fail2ban)
	}
	f.call(http.MethodGet, "/firewall", nil, http.StatusOK, &st)
	if strings.Join(st.Trusted, " ") != "198.51.100.7" {
		t.Fatalf("GET /firewall: %+v", st.Trusted)
	}
	f.call(http.MethodPost, "/firewall/untrust", map[string]any{"ip": "198.51.100.7"}, http.StatusOK, &st)
	if st.Trusted == nil || len(st.Trusted) != 0 {
		t.Fatalf("after untrust: %#v", st.Trusted)
	}
}

// The command mp migrate grant prints is for another machine: the address in
// it must be one that machine can reach, also when the grant was asked for
// over the local socket or from the console.
func TestMigrateGrantNamesAReachableAddress(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetHostIPs(func() []string { return []string{"10.0.0.5", "203.0.113.10"} })
	for host, want := range map[string]string{
		"monopanel":            "203.0.113.10:8443", // the local socket
		"127.0.0.1:8443":       "203.0.113.10:8443", // the console
		"panel.example.com":    "panel.example.com:8443",
		"198.51.100.4:9443":    "198.51.100.4:9443",
		"[2001:db8::7]:8443":   "[2001:db8::7]:8443",
		"panel.example.com:80": "panel.example.com:80",
	} {
		ctx := t.Context()
		r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
		r.Host = host
		var got string
		requestInfoMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = f.s.panelAddress(r.Context()) })).ServeHTTP(httptest.NewRecorder(), r)
		if got != want {
			t.Errorf("Host %s: %s, want %s", host, got, want)
		}
	}
	// A panel that listens on one address is reached there.
	f.s.cfg.Web.Listen = "198.51.100.9:9443"
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Host = "monopanel"
	var got string
	requestInfoMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = f.s.panelAddress(r.Context()) })).ServeHTTP(httptest.NewRecorder(), r)
	if got != "198.51.100.9:9443" {
		t.Errorf("listen on one address: %s", got)
	}
	f.s.cfg.Web.Listen = ":8443"

	f.s.cfg.Web.Hostname = "panel.example.org"
	var grant apitypes.MigrationGrantResponse
	f.call(http.MethodPost, "/migrate/grant", map[string]any{"scope": "user:alex"}, http.StatusCreated, &grant)
	if !strings.Contains(grant.Command, "--source https://panel.example.org:8443 ") {
		t.Fatalf("hint: %s", grant.Command)
	}
}

// A move saved without a restart keeps its port open through later firewall
// changes, and the lockout guard counts that port as the panel's: whenever
// monopanel-api restarts — from the panel, after an update, at the next boot
// — the panel must be reachable where it comes back.
func TestPanelConfigPendingPortStaysOpen(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	if err := f.db.SetSetting(f.ctx, settingFirewall, "yes"); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	f.call(http.MethodPut, "/system/config", map[string]any{"listen": fmt.Sprintf(":%d", port)}, http.StatusOK, nil)
	if code, detail := fwCall(f, http.MethodPost, "/firewall/rules", map[string]any{"kind": "allow", "proto": "tcp", "port": "2222"}); code != http.StatusCreated {
		t.Fatalf("another rule: %d %s", code, detail)
	}
	rules, _ := f.agent.File(nftRulesPath)
	if !strings.Contains(rules, strconv.Itoa(port)) || !strings.Contains(rules, "8443") || !strings.Contains(rules, "2222") {
		t.Fatalf("the pending panel port was closed again:\n%s", rules)
	}
	if code, detail := fwCall(f, http.MethodPost, "/firewall/rules", map[string]any{"kind": "deny", "proto": "tcp", "port": strconv.Itoa(port)}); code != http.StatusUnprocessableEntity || !strings.Contains(detail, fmt.Sprintf("the panel (port %d)", port)) {
		t.Fatalf("a deny on the pending panel port: %d %s", code, detail)
	}
}

// A restart applies what config.yaml says, also what root wrote there by
// hand: a web.listen the panel could not come back on is refused before
// anything happens, and so is a restart the agent cannot do.
func TestPanelConfigRestartChecksWhatItApplies(t *testing.T) {
	f, path := newConfigFixture(t, nil)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	raw, _ := os.ReadFile(path)
	edited, _, err := config.EditPanelKeys(raw, map[string]string{config.KeyListen: busy.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o640); err != nil {
		t.Fatal(err)
	}
	if code, detail := fwCall(f, http.MethodPut, "/system/config", map[string]any{"restart": true}); code != http.StatusUnprocessableEntity || !strings.Contains(detail, "which the restart applies") || !strings.Contains(detail, "already in use") {
		t.Fatalf("restart onto a busy port: %d %s", code, detail)
	}
	// Putting a good address in its place is the way out.
	f.call(http.MethodPut, "/system/config", map[string]any{"listen": ":8443", "restart": true}, http.StatusOK, nil)
	if !waitUnitAction(f, "monopanel-api.service", "restart") {
		t.Fatal("monopanel-api was not restarted")
	}

	g, _ := newConfigFixture(t, nil)
	g.agent.Fail["/v1/service"] = "systemd unreachable"
	if code, detail := fwCall(g, http.MethodPut, "/system/config", map[string]any{"log_level": "debug", "restart": true}); code != http.StatusBadGateway || !strings.Contains(detail, "nothing was changed") {
		t.Fatalf("a restart the agent cannot do: %d %s", code, detail)
	}
	if len(g.agent.PanelConfigs()) != 0 {
		t.Fatal("the change was written although the restart could not happen")
	}
}

// A host name saved without a restart is not the panel's yet: ordering its
// certificate says so instead of asking for a name.
func TestPanelCertificateWaitsForTheRestart(t *testing.T) {
	f, _ := newConfigFixture(t, nil)
	f.call(http.MethodPut, "/system/config", map[string]any{"hostname": "panel.example.com"}, http.StatusOK, nil)
	if code, detail := fwCall(f, http.MethodPost, "/ssl/panel/issue", map[string]any{}); code != http.StatusUnprocessableEntity || !strings.Contains(detail, "panel.example.com is in config.yaml but takes effect when monopanel-api restarts") {
		t.Fatalf("issue before the restart: %d %s", code, detail)
	}
	g, _ := newConfigFixture(t, nil)
	if code, detail := fwCall(g, http.MethodPost, "/ssl/panel/issue", map[string]any{}); code != http.StatusUnprocessableEntity || !strings.Contains(detail, "no hostname to certify") {
		t.Fatalf("issue without a name: %d %s", code, detail)
	}
}
