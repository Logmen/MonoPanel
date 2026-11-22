package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/apitypes"
)

var testSites = []map[string]any{
	{"domain": "example.com", "login": "bob", "php_version": "8.4", "mode": "fpm", "ssl": "auto", "status": "active", "http2": true},
	{"domain": "old.example.net", "login": "carol", "php_version": "8.1", "mode": "fpm", "ssl": "none", "status": "suspended"},
}

var testUsers = []map[string]any{
	{"login": "admin", "role": "admin", "status": "active"},
	{"login": "bob", "role": "user", "status": "active", "unix_uid": 1001, "home": "/var/www/bob"},
}

func apiRoutes() map[string]any {
	return map[string]any{
		"/api/v1/sites":             testSites,
		"/api/v1/sites/example.com": testSites[0],
		"/api/v1/users":             testUsers,
		"/api/v1/users/bob":         testUsers[1],
		"/api/v1/jobs":              []map[string]any{{"id": 7, "type": "site.apply", "status": "done", "progress": 100, "requested_by": "admin"}},
	}
}

// openArea selects an area of the main menu by its title.
func openArea(t *testing.T, m *model, title string) {
	t.Helper()
	for m.top() != m.stack[0] {
		m.pop()
	}
	menu := m.stack[0].(*hubScreen)
	for i, e := range menu.items(m) {
		if e.label == title {
			menu.cursor = i
			press(m, "enter")
			return
		}
	}
	t.Fatalf("no area %q in the menu", title)
}

func viewText(m *model) string { return m.View().Content }

func TestRunFromList(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	fr.out = "job #7\njob #7: done\n"
	openArea(t, m, "Sites")
	if s, ok := m.top().(*listScreen); !ok || len(s.visible()) != 2 {
		t.Fatalf("the sites list: %T", m.top())
	}
	press(m, "p") // site apply on the selected row, a quick action
	if _, ok := m.overlay.(*confirmOverlay); !ok || len(fr.started()) != 0 {
		t.Fatalf("site apply asks first, as the web UI does: %T", m.overlay)
	}
	press(m, "y")
	jobs := fr.started()
	if len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"site", "apply", "example.com"}) {
		t.Fatalf("jobs %+v", jobs)
	}
	env := fr.envs[0]
	if !slices.Contains(env, "MP_SERVER=https://panel:8443") || !slices.Contains(env, "MP_TOKEN=secret-token") || !slices.Contains(env, "MP_LANG=en") {
		t.Errorf("credentials and language go through the environment: %q", env)
	}
	for _, a := range jobs[0].argv {
		if strings.Contains(a, "secret-token") || strings.HasPrefix(a, "--server") || strings.HasPrefix(a, "--token") {
			t.Errorf("a credential on the command line: %q", jobs[0].argv)
		}
	}
	rs, ok := m.top().(*runScreen)
	if !ok || !rs.finished() {
		t.Fatalf("the output view: %T", m.top())
	}
	v := viewText(m)
	for _, want := range []string{"$ mp site apply example.com", "job #7: done", "exit 0"} {
		if !strings.Contains(v, want) {
			t.Errorf("the output view lacks %q:\n%s", want, v)
		}
	}
	press(m, "esc")
	if _, ok := m.top().(*listScreen); !ok {
		t.Errorf("esc goes back to the list: %T", m.top())
	}
}

func TestFormFailureReturnsToForm(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	press(m, "s") // site set: a form with the site's values
	f, ok := m.top().(*form)
	if !ok {
		t.Fatalf("site set opens a form: %T", m.top())
	}
	if fieldByName(f, "php").value() != "8.4" {
		t.Errorf("the form starts from the site: %q", fieldByName(f, "php").value())
	}
	fr.code, fr.out = 2, "error: bad php version\n"
	fieldByName(f, "php").setValue("9.9")
	press(m, "ctrl+s")
	if !slices.Equal(fr.started()[0].argv, []string{"site", "set", "example.com", "--php=9.9"}) {
		t.Fatalf("argv %q", fr.started()[0].argv)
	}
	if v := viewText(m); !strings.Contains(v, "exit status 2") {
		t.Errorf("the exit status:\n%s", v)
	}
	press(m, "esc")
	if m.top() != f {
		t.Fatalf("after a failure esc returns to the form: %T", m.top())
	}
	fr.code = 0
	press(m, "ctrl+s", "esc")
	if _, ok := m.top().(*listScreen); !ok {
		t.Errorf("after a success esc skips the form: %T", m.top())
	}
}

func TestConfirmTypedName(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Users")
	s := m.top().(*listScreen)
	s.cursor = 1 // bob
	press(m, "d")
	f, ok := m.top().(*form)
	if !ok {
		t.Fatalf("user rm opens a form: %T", m.top())
	}
	if fieldByName(f, "yes") != nil {
		t.Error("--yes is not a field")
	}
	f.focus = 1 // --purge
	press(m, "space", "ctrl+s")
	if _, ok := m.overlay.(*confirmOverlay); !ok {
		t.Fatalf("a confirmation: %T", m.overlay)
	}
	if v := viewText(m); !strings.Contains(v, "Type bob to confirm") {
		t.Errorf("the dialog asks for the login:\n%s", v)
	}
	typeText(m, "bo")
	press(m, "enter")
	if len(fr.started()) != 0 || m.overlay == nil {
		t.Fatal("a wrong name must not run anything")
	}
	typeText(m, "b")
	press(m, "enter")
	jobs := fr.started()
	if len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"user", "rm", "bob", "--purge", "--yes"}) {
		t.Fatalf("argv %+v", jobs)
	}
	if !strings.HasSuffix(jobs[0].shown, "--purge --yes") {
		t.Errorf("shown %q", jobs[0].shown)
	}
}

func TestConfirmCancel(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	press(m, "d", "ctrl+s") // site rm without --purge: y/N
	if _, ok := m.overlay.(*confirmOverlay); !ok {
		t.Fatalf("a confirmation: %T", m.overlay)
	}
	press(m, "n")
	if m.overlay != nil || len(fr.started()) != 0 {
		t.Error("n cancels")
	}
	press(m, "ctrl+s", "y")
	if len(fr.started()) != 1 {
		t.Error("y runs")
	}
}

func TestPalette(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	press(m, ":")
	if _, ok := m.overlay.(*palette); !ok {
		t.Fatalf("the palette: %T", m.overlay)
	}
	typeText(m, "si")
	if p := m.overlay.(*palette); len(p.sugg) == 0 || p.sugg[0].word != "site" {
		t.Errorf("suggestions %+v", p.sugg)
	}
	press(m, "tab")
	typeText(m, "add new.test --php 8.4")
	press(m, "enter")
	jobs := fr.started()
	if len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"site", "add", "new.test", "--php=8.4"}) {
		t.Fatalf("palette run: %+v", jobs)
	}

	press(m, "esc", ":")
	typeText(m, "site add x --token abc")
	press(m, "enter")
	if p, ok := m.overlay.(*palette); !ok || !strings.Contains(p.err, "--token") {
		t.Errorf("a global flag is refused: %+v", m.overlay)
	}
	press(m, "esc", ":")
	typeText(m, "site add")
	press(m, "enter")
	if f, ok := m.top().(*form); !ok || f.path != "site add" {
		t.Errorf("an incomplete line opens the form: %T", m.top())
	}
	press(m, "esc", ":")
	typeText(m, "nosuch")
	press(m, "enter")
	if p, ok := m.overlay.(*palette); !ok || p.err == "" {
		t.Error("an unknown command is reported")
	}
	press(m, "esc", ":")
	typeText(m, "setup")
	press(m, "enter")
	if p, ok := m.overlay.(*palette); !ok || p.err == "" {
		t.Error("setup is not run from the menu")
	}
}

func TestHelpRunsActions(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	press(m, "?")
	h, ok := m.overlay.(*helpOverlay)
	if !ok {
		t.Fatalf("the help: %T", m.overlay)
	}
	v := viewText(m)
	for _, want := range []string{"mp site apply example.com", "All commands of “Sites”", "mp site move-ip <new-address>"} {
		if !strings.Contains(v, want) {
			t.Errorf("the help lacks %q:\n%s", want, v)
		}
	}
	for i, it := range h.items {
		if it.act != nil && it.act.path == "site fix" {
			h.cursor = i
			break
		}
	}
	press(m, "enter", "y") // site fix asks first
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"site", "fix", "example.com"}) {
		t.Errorf("enter in the help runs the action: %+v", jobs)
	}
}

func TestFilterForm(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Jobs")
	s := m.top().(*listScreen)
	press(m, "f")
	f, ok := m.top().(*form)
	if !ok || f.path != "job list" {
		t.Fatalf("the filter form: %T", m.top())
	}
	if !strings.Contains(f.line(), "--limit 50") {
		t.Errorf("the filter shows its query: %q", f.line())
	}
	st := fieldByName(f, "status")
	st.setValue("failed")
	press(m, "ctrl+s")
	if m.top() != s || s.query["status"] != "failed" || len(fr.started()) != 0 {
		t.Errorf("the filter applies to the list without running anything: %v %v", s.query, fr.started())
	}
}

func TestRoleMenus(t *testing.T) {
	m, _ := newTestModel(t, "user", apiRoutes())
	var labels []string
	for _, e := range m.stack[0].(*hubScreen).items(m) {
		labels = append(labels, e.label)
	}
	want := []string{"Dashboard", "Sites", "Account", "Databases", "Mail", "Files", "Jobs", "Audit log", "Settings", "Other"}
	if !slices.Equal(labels, want) {
		t.Errorf("an ordinary account's menu %q, want %q", labels, want)
	}
	openArea(t, m, "Account")
	if h, ok := m.top().(*hubScreen); !ok || h.title != "bob" {
		t.Errorf("the account page of the caller: %T", m.top())
	}
	openArea(t, m, "Sites")
	s := m.top().(*listScreen)
	for _, hnt := range s.hints(m) {
		if hnt.key == "m" {
			t.Error("move-ip is an administrator's action")
		}
	}

	m, _ = newTestModel(t, "admin", apiRoutes())
	labels = nil
	for _, e := range m.stack[0].(*hubScreen).items(m) {
		labels = append(labels, e.label)
	}
	for _, want := range []string{"Users", "PHP", "Extensions", "SSL", "Firewall", "Services", "Backups", "Migration", "Other"} {
		if !slices.Contains(labels, want) {
			t.Errorf("the administrator's menu lacks %q: %q", want, labels)
		}
	}
}

func TestRefreshReloads(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	s := m.top().(*listScreen)
	before := s.seq
	press(m, "r")
	if s.seq != before+1 {
		t.Errorf("r reloads the list: seq %d → %d", before, s.seq)
	}
}

func TestHeader(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	if h := m.header(80); !strings.Contains(h, "v9.9.9") || !strings.Contains(h, "h1") {
		t.Errorf("header %q", h)
	}
	// The agent is down: the API sends no host facts then.
	m.status = &apitypes.SystemStatus{Panel: apitypes.PanelStatus{Version: "9.9.9", AgentOK: false, AgentError: "dial unix: no such file"}}
	if h := m.header(80); !strings.Contains(h, "agent unavailable") {
		t.Errorf("the agent warning without host facts: %q", h)
	}
}

func TestViewFits(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {60, 16}} {
		m, _ := newTestModel(t, "admin", apiRoutes())
		m.width, m.height = size[0], size[1]
		check := func(what string) {
			t.Helper()
			lines := strings.Split(viewText(m), "\n")
			if len(lines) != size[1] {
				t.Errorf("%dx%d %s: %d lines", size[0], size[1], what, len(lines))
			}
			for i, l := range lines {
				if w := textWidth(l); w > size[0] {
					t.Errorf("%dx%d %s: line %d is %d cells: %q", size[0], size[1], what, i, w, l)
				}
			}
		}
		check("menu")
		openArea(t, m, "Sites")
		check("list")
		press(m, "s")
		check("form")
		press(m, "esc", "?")
		check("help")
		press(m, "esc", "p")
		check("apply confirm")
		press(m, "y")
		check("output")
		press(m, "esc", "d", "ctrl+s")
		check("confirm")
	}
}

func TestAddNeedsNoRow(t *testing.T) {
	routes := apiRoutes()
	routes["/api/v1/tokens"] = []any{}
	routes["/api/v1/cron"] = []any{}
	m, _ := newTestModel(t, "admin", routes)
	m.push(tokensScreen(m, ""))
	drive(m, m.loadTop())
	press(m, "a")
	f, ok := m.top().(*form)
	if !ok || f.path != "token create" {
		t.Fatalf("add on an empty list of one's own tokens: %T", m.top())
	}
	if fd := fieldByName(f, "user"); fd != nil && fd.passes() {
		t.Errorf("one's own token needs no --user: %q", f.line())
	}
}

// slowRunner keeps the command running until it is interrupted.
type slowRunner struct {
	fakeRunner
	cancelled chan struct{}
}

func (s *slowRunner) start(job runJob, env []string) (<-chan runEvent, func(), error) {
	ch := make(chan runEvent, 2)
	ch <- runEvent{out: "working\n"}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			close(s.cancelled)
			ch <- runEvent{done: true, code: 130}
			close(ch)
		})
	}, nil
}

func TestInterruptRunning(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	sr := &slowRunner{cancelled: make(chan struct{})}
	m.runner = sr
	openArea(t, m, "Sites")
	m.Update(key("p"))
	_, cmd := m.Update(key("y"))
	// Deliver the first output only; the command is still running.
	rs := m.top().(*runScreen)
	for _, c := range expand(cmd) {
		if msg, ok := runOnce(c).(runEventMsg); ok {
			m.Update(msg)
		}
	}
	if !rs.running {
		t.Fatal("the command runs")
	}
	press(m, "esc")
	if m.top() != rs || !strings.Contains(m.flashText, "ctrl+c") {
		t.Error("esc does not leave a running command")
	}
	m.Update(key("ctrl+c"))
	select {
	case <-sr.cancelled:
	default:
		t.Fatal("ctrl+c interrupts the child")
	}
	if m.top() != rs {
		t.Fatal("ctrl+c stops the command, not the TUI")
	}
	drive(m, waitRun(rs.id, rs.ch))
	if !rs.finished() || !strings.Contains(viewText(m), "exit status 130") {
		t.Errorf("the interrupted command ends with its status:\n%s", viewText(m))
	}
}

func expand(c tea.Cmd) []tea.Cmd {
	if c == nil {
		return nil
	}
	msg := runOnce(c)
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Cmd
		for _, x := range b {
			out = append(out, x)
		}
		return out
	}
	return []tea.Cmd{func() tea.Msg { return msg }}
}

func runOnce(c tea.Cmd) tea.Msg {
	if c == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- c() }()
	select {
	case msg := <-done:
		return msg
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}

func TestFormCommandHelp(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	press(m, "a")
	m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	if _, ok := m.overlay.(*cmdHelp); !ok {
		t.Fatalf("F1 in a form: %T", m.overlay)
	}
	v := viewText(m)
	for _, want := range []string{"mp site add <domain>", "--mode", "fpm|apache|proxy", "additional names"} {
		if !strings.Contains(v, want) {
			t.Errorf("the command help lacks %q:\n%s", want, v)
		}
	}
	press(m, "esc")
	if m.overlay != nil {
		t.Error("esc closes it")
	}
}

func TestOutputKeepsSpaces(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	fr.out = "DOMAIN       USER\nexample.com  bob\n"
	press(m, ":")
	typeText(m, "site list")
	press(m, "enter")
	if v := viewText(m); !strings.Contains(v, "DOMAIN       USER") || !strings.Contains(v, "example.com  bob") {
		t.Errorf("the columns of mp's output stay aligned:\n%s", v)
	}
}

func TestEditAgainAfterFailedSave(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	dir := t.TempDir()
	local := dir + "/custom.conf"
	st := &editState{kind: "nginx", dir: dir, local: local, remote: "example.com", before: []byte("old\n")}
	if err := os.WriteFile(local, []byte("broken;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fr.code, fr.out = 2, "error: nginx -t failed\n"
	drive(m, m.edited(editedMsg{st: st}))
	rs, ok := m.top().(*runScreen)
	if !ok || !rs.finished() || rs.failed == 0 {
		t.Fatalf("the save ran and failed: %T", m.top())
	}
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"site", "nginx", "example.com", "--set=" + local}) {
		t.Fatalf("jobs %+v", jobs)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("the edited copy is kept after a failed save")
	}
	found := false
	for _, h := range rs.hints(m) {
		found = found || h.key == "e"
	}
	if !found {
		t.Error("e — edit again is offered")
	}
	press(m, "e") // the editor runs in the terminal (skipped here)
	if m.top() == rs {
		t.Fatal("e leaves the failed output for the editor")
	}
	// The editor is done: saved again, and this time it works.
	fr.code, fr.out = 0, "written\n"
	os.WriteFile(local, []byte("fixed;\n"), 0o600) //nolint:errcheck // the test's own file
	drive(m, m.edited(editedMsg{st: st}))
	if rs2, ok := m.top().(*runScreen); !ok || rs2.failed != 0 {
		t.Fatalf("the second save: %T", m.top())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a saved copy is removed")
	}

	// Leaving a failed save drops the copy.
	dir = t.TempDir()
	local = dir + "/x.txt"
	os.WriteFile(local, []byte("new"), 0o600) //nolint:errcheck // the test's own file
	st = &editState{kind: "file", dir: dir, local: local, remote: "/x.txt", user: "bob", before: []byte("old")}
	fr.code = 1
	drive(m, m.edited(editedMsg{st: st}))
	press(m, "esc")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("esc after a failed save removes the copy")
	}
}

func TestPaletteUnknownSubcommand(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	press(m, ":")
	typeText(m, "site nosuch")
	press(m, "enter")
	if p, ok := m.overlay.(*palette); !ok || !strings.Contains(p.err, "site nosuch") {
		t.Errorf("an unknown command of a group is reported: %+v", m.overlay)
	}
	if len(fr.started()) != 0 {
		t.Error("nothing runs")
	}
}

func TestRootHasNoOwn2FA(t *testing.T) {
	routes := apiRoutes()
	routes["/api/v1/auth/me"] = map[string]any{"login": "root", "role": "admin", "via": "peercred"}
	m, _ := newTestModel(t, "admin", routes)
	openArea(t, m, "Settings")
	h := m.top().(*hubScreen)
	for _, e := range h.items(m) {
		if e.key == "2" {
			t.Error("root over the socket has no account and so no 2FA of its own")
		}
	}
	m, _ = newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Settings")
	found := false
	for _, e := range m.top().(*hubScreen).items(m) {
		found = found || e.key == "2"
	}
	if !found {
		t.Error("an account's own 2FA is in Settings")
	}
}

func TestOwnAccountHasNoDelete(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Users")
	s := m.top().(*listScreen)
	keys := func() []string {
		var out []string
		for _, h := range s.hints(m) {
			out = append(out, h.key)
		}
		return out
	}
	s.cursor = 0 // admin: the caller
	if slices.Contains(keys(), "d") {
		t.Error("one's own account has no delete, as in the web UI")
	}
	s.cursor = 1 // bob
	if !slices.Contains(keys(), "d") {
		t.Error("another account can be deleted")
	}
}

// allScreens builds every screen the menus can open, for checks that walk
// all of them.
func allScreens(m *model) map[string]screen {
	out := map[string]screen{}
	for _, a := range areaDefs(true) {
		out["area "+a.id] = a.open(m, a)
	}
	out["site page"] = sitePage(m, "example.com")
	out["user page"] = userPage(m, "bob")
	out["cron"] = cronScreen(m, "bob")
	out["all cron"] = cronScreen(m, "")
	out["apps"] = appsScreen(m, "bob")
	out["app page"] = appPage(m, map[string]string{"login": "bob", "name": "bot"})
	out["valkey"] = valkeyScreen(m, "bob")
	out["shares"] = sharesScreen(m, "bob")
	out["tokens"] = tokensScreen(m, "bob")
	out["php ext"] = phpExtScreen(m, "8.4")
	out["php ini"] = phpIniScreen(m)
	out["memcached"] = memcachedScreen(m)
	out["real-ip"] = realIPScreen(m)
	out["db engine"] = dbEngineScreen(m)
	out["db config"] = dbConfigScreen(m)
	out["certs"] = certsScreen(m)
	out["panel tls"] = panelTLSScreen(m)
	out["dns providers"] = dnsProvidersScreen(m)
	out["fw rules"] = firewallRulesScreen(m)
	out["bans"] = bansScreen(m)
	out["trusted"] = trustedScreen(m)
	out["targets"] = backupTargetsScreen(m)
	out["snapshots"] = snapshotsScreen(m, "local")
	out["backup runs"] = backupRunsScreen(m, "")
	out["mail domains"] = mailDomainsScreen(m)
	out["mail domain"] = mailDomainPage(m, map[string]string{"name": "example.com"})
	out["mailboxes"] = mailboxesScreen(m, "")
	out["aliases"] = aliasesScreen(m, "")
	out["webhooks"] = webhooksScreen(m)
	out["updates"] = updatesScreen(m)
	out["panel config"] = panelConfigScreen(m)
	out["templates"] = templatesScreen(m)
	out["totp"] = totpScreen(m)
	out["files"] = filesScreen(m, "bob", "/")
	out["doctor"] = openDoctor(m)
	out["metrics"] = openMetrics(m)
	out["selinux"] = openSELinux(m)
	return out
}

// TestKeysNotShadowed: a key shown for an action must reach it — the lists
// and menus take j, k and the rest for themselves first.
func TestKeysNotShadowed(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	global := []string{"?", ":", "r", "q", "esc", "enter"}
	for name, s := range allScreens(m) {
		var keys []string
		reserved := append([]string{}, global...)
		switch x := s.(type) {
		case *listScreen:
			reserved = append(reserved, "up", "k", "down", "j", "/", "home", "end", "pgup", "pgdown")
			if x.filterCmd != "" {
				reserved = append(reserved, "f")
			}
			if x.keyHook != nil {
				reserved = append(reserved, "backspace", "left", "h")
			}
			for _, a := range x.acts {
				keys = append(keys, a.key)
			}
			for _, l := range x.links {
				keys = append(keys, l.key)
			}
		case *hubScreen:
			reserved = append(reserved, "up", "k", "down", "j", "home", "end")
			for _, e := range x.entries {
				keys = append(keys, e.key)
			}
		}
		for _, k := range keys {
			if k != "" && slices.Contains(reserved, k) {
				t.Errorf("%s: the key %q of an action never reaches it", name, k)
			}
		}
	}
}

// TestAreasOfferAllCommands: the first page of every area, for both roles,
// lists the area's commands under *.
func TestAreasOfferAllCommands(t *testing.T) {
	for _, role := range []string{"admin", "user"} {
		m, _ := newTestModel(t, role, apiRoutes())
		for _, e := range m.stack[0].(*hubScreen).items(m) {
			if e.label == "Other" {
				continue // a list of commands itself
			}
			openArea(t, m, e.label)
			found := false
			switch x := m.top().(type) {
			case *listScreen:
				for _, l := range x.links {
					found = found || l.key == "*"
				}
			case *hubScreen:
				for _, it := range x.items(m) {
					found = found || it.key == "*"
				}
			}
			if !found {
				t.Errorf("%s: %s has no * (all commands)", role, e.label)
				continue
			}
			press(m, "*")
			s, ok := m.top().(*listScreen)
			if !ok || s.title != "All commands" || s.area == nil || s.area.title != e.label {
				t.Errorf("%s: * on %s: %T", role, e.label, m.top())
				continue
			}
			// The groups of these areas are in the test tree.
			if (e.label == "Sites" || e.label == "Users" || e.label == "Account" || e.label == "Files") && len(s.res.rows) == 0 {
				t.Errorf("%s: * on %s lists no commands", role, e.label)
			}
		}
	}
}

func TestEditFileFetchesIt(t *testing.T) {
	routes := apiRoutes()
	routes["/api/v1/files"] = map[string]any{"user": "bob", "home": "/var/www/bob", "path": "/", "entries": []map[string]any{
		{"name": "notes.txt", "type": "file", "size": 5, "mode": "-rw-r--r--", "mtime": "2026-11-20T10:00:00Z"}}}
	m, fr := newTestModel(t, "admin", routes)
	m.push(filesScreen(m, "bob", "/"))
	drive(m, m.loadTop())
	s := m.top().(*listScreen)
	s.cursor = 0
	press(m, "e")
	jobs := fr.started()
	if len(jobs) != 1 || len(jobs[0].argv) != 5 || jobs[0].argv[0] != "files" || jobs[0].argv[1] != "get" || jobs[0].argv[2] != "/notes.txt" || jobs[0].argv[4] != "--user=bob" {
		t.Fatalf("the edit starts with mp files get: %+v", jobs)
	}
	local := jobs[0].argv[3]
	if !strings.HasSuffix(local, "/notes.txt") || !strings.Contains(local, "mp-edit-") {
		t.Errorf("the copy goes to a private temporary directory: %q", local)
	}
	if len(m.running) != 0 {
		t.Errorf("the download is no longer tracked once it ended: %d", len(m.running))
	}
	os.RemoveAll(filepath.Dir(local)) //nolint:errcheck // the editor never ran in the test
}
