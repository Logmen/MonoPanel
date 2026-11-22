package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The TUI asks wherever the web UI asks (its Confirm dialogs), with the
// same explanation; these are the actions of web/src/routes/** that ask.

// confirmTree is the test tree with the commands of every confirmed action,
// shaped like the real ones.
func confirmTree() *cobra.Command {
	root := newerTree()
	// ensure finds or makes the groups along a path.
	ensure := func(path string) *cobra.Command {
		c := root
		for _, name := range strings.Fields(path) {
			next := findCmd(c, name)
			if next == nil || next.Parent() != c {
				next = &cobra.Command{Use: name, Short: name + " group"}
				c.AddCommand(next)
			}
			c = next
		}
		return c
	}
	add := func(group, use string, args cobra.PositionalArgs, flags func(fs *pflag.FlagSet)) {
		c := &cobra.Command{Use: use, Short: use, Args: args, RunE: noop}
		if flags != nil {
			flags(c.Flags())
		}
		ensure(group).AddCommand(c)
	}
	add("mail box", "set <user@example.com>", cobra.ExactArgs(1), func(fs *pflag.FlagSet) {
		fs.String("password", "", "new password (with no other flags, one is generated)")
		fs.Bool("password-stdin", false, "read the new password from stdin")
		fs.String("name", "", "name of the mailbox owner")
		fs.Int("quota", 0, "quota in MB")
		fs.String("active", "", "true|false")
	})
	add("mail domain", "set <domain>", cobra.ExactArgs(1), func(fs *pflag.FlagSet) {
		fs.String("lenient", "", "true|false — accept mail without the HELO and sender domain checks")
		fs.String("active", "", "true|false")
		fs.String("send-only", "", "true|false — send only")
	})
	add("mail domain", "dkim <domain>", cobra.ExactArgs(1), nil)
	add("mail", "webmail <domain>", cobra.ExactArgs(1), func(fs *pflag.FlagSet) {
		fs.String("user", "", "site owner")
		fs.String("php", "", "PHP version (defaults to the newest installed)")
		fs.Int("port", 0, "also serve webmail on this port")
	})
	add("stack", "install <component>", cobra.ExactArgs(1), nil)
	add("stack", "remove <component>", cobra.ExactArgs(1), nil)
	add("stack", "upgrade [component]", cobra.MaximumNArgs(1), nil)
	add("php", "remove <version>", cobra.ExactArgs(1), nil)
	add("php ext", "disable <version> <name>", cobra.ExactArgs(2), nil)
	add("db engine", "tune", nil, nil)
	add("user", "totp-reset <login>", cobra.ExactArgs(1), nil)
	add("backup target", "rm <name>", cobra.ExactArgs(1), nil)
	add("dns-provider", "rm <name>", cobra.ExactArgs(1), nil)
	add("ssl panel", "self-signed", nil, nil)
	add("app", "stop <name>", cobra.ExactArgs(1), nil)
	// As in mp: the group's list filter is inherited by mail box set too.
	findCmd(root, "mail box").PersistentFlags().String("domain", "", "show only this domain")
	findCmd(root, "user set").Flags().Bool("sftp-only", false, "SFTP only, chrooted to the home directory")
	findCmd(root, "update settings").Flags().String("repo", "", `repository with the releases (owner/name; "-" turns updates off)`)
	return root
}

func TestConfirmLikeTheWeb(t *testing.T) {
	root := confirmTree()
	type tc struct {
		path  string
		args  []string
		flags map[string]string
		vals  map[string]string
		note  string // a part of the note; "" for no confirmation
	}
	cases := []tc{
		// Sites: the list and the site page.
		{path: "site apply", args: []string{"example.com"}, note: "sites/example.com.d"},
		{path: "site fix", args: []string{"example.com"}, note: "ACL"},
		{path: "site suspend", args: []string{"example.com"}, note: "503"},
		{path: "site unsuspend", args: []string{"example.com"}, note: "placeholder"},
		{path: "site move-ip", args: []string{"203.0.113.7"}, note: "no longer has"},
		{path: "site move-ip", args: []string{"203.0.113.7"}, flags: map[string]string{"from": "198.51.100.1"}, note: "198.51.100.1 moves to 203.0.113.7"},
		{path: "site nginx", args: []string{"example.com"}, flags: map[string]string{"clear": "true"}, note: "nginx reloads"},
		{path: "site nginx", args: []string{"example.com"}},
		// Mail.
		{path: "mail apply", note: "Manual changes"},
		{path: "mail box set", args: []string{"a@example.com"}, note: "generates a new password"},
		// The list filter the group adds leaves the request empty: still a new password.
		{path: "mail box set", args: []string{"a@example.com"}, flags: map[string]string{"domain": "example.com"}, note: "generates a new password"},
		{path: "mail box set", args: []string{"a@example.com"}, flags: map[string]string{"password": "n3w-pass"}},
		{path: "mail box set", args: []string{"a@example.com"}, flags: map[string]string{"name": "Ann", "quota": "500"}},
		{path: "mail box set", args: []string{"a@example.com"}, flags: map[string]string{"active": "false"}, note: "bounce"},
		{path: "mail box set", args: []string{"a@example.com"}, flags: map[string]string{"active": "true"}, note: "accepts mail"},
		{path: "mail domain set", args: []string{"example.com"}, flags: map[string]string{"active": "false"}, note: "no longer signed"},
		{path: "mail domain set", args: []string{"example.com"}, flags: map[string]string{"active": "true"}, note: "works again"},
		{path: "mail domain set", args: []string{"example.com"}, flags: map[string]string{"lenient": "true"}, note: "spam"},
		{path: "mail domain set", args: []string{"example.com"}, flags: map[string]string{"lenient": "false"}, note: "rejected"},
		{path: "mail domain set", args: []string{"example.com"}, flags: map[string]string{"send-only": "true"}, note: "MX"},
		{path: "mail domain set", args: []string{"example.com"}, flags: map[string]string{"send-only": "false"}, note: "regular mail domain"},
		{path: "mail domain dkim", args: []string{"example.com"}, note: "TXT record"},
		{path: "mail webmail", args: []string{"webmail.example.com"}, vals: map[string]string{"webmail": "mail.example.com"}, note: "mail.example.com"},
		{path: "mail webmail", args: []string{"webmail.example.com"}},
		// Firewall.
		{path: "firewall unban", args: []string{"192.0.2.9"}, note: "fail2ban"},
		{path: "firewall disable", note: "nftables"},
		{path: "firewall enable"},
		// Services: stopping anything, switching autostart off, restarting the panel itself.
		{path: "service stop", args: []string{"nginx.service"}, note: "Every site"},
		{path: "service stop", args: []string{"php8.4-fpm.service"}, note: "PHP version"},
		{path: "service stop", args: []string{"mysql.service"}, note: "databases"},
		{path: "service stop", args: []string{"monopanel-api"}, note: "systemctl start monopanel-api"},
		{path: "service stop", args: []string{"cron.service"}, note: "stays stopped"},
		{path: "service disable", args: []string{"cron.service"}, note: "reboot"},
		{path: "service disable", args: []string{"monopanel-api.service"}, note: "panel may not come up"},
		{path: "service restart", args: []string{"monopanel-api.service"}, note: "API restarts"},
		{path: "service reload-or-restart", args: []string{"monopanel-agent.service"}, note: "agent restarts"},
		{path: "service restart", args: []string{"nginx.service"}},
		{path: "service start", args: []string{"nginx.service"}},
		// Extensions and PHP.
		{path: "stack install", args: []string{"memcached"}, note: "packages installed"},
		{path: "stack install", args: []string{"nginx"}, vals: map[string]string{"name": "nginx", "installed": "true"}, note: "runs again"},
		{path: "stack install", args: []string{"percona"}, vals: map[string]string{"name": "percona", "installed": "true"}, note: "MySQL restarts"},
		{path: "stack install", args: []string{"composer"}, note: "updates it"},
		{path: "stack remove", args: []string{"memcached"}, note: "cache"},
		{path: "stack upgrade", note: "The database server is not touched"},
		{path: "stack upgrade", args: []string{"nginx"}, note: "nginx -t checks the configuration"},
		{path: "stack upgrade", args: []string{"php"}, note: "cuts off the requests"},
		{path: "stack upgrade", args: []string{"apache"}, note: "answering 502"},
		{path: "stack upgrade", args: []string{"db"}, note: "the server restarts"},
		{path: "php remove", args: []string{"8.1"}, note: "PHP 8.1"},
		{path: "php ext disable", args: []string{"8.4", "opcache"}, vals: map[string]string{"critical": "true"}, note: "typical site"},
		// Databases, backups, SSL.
		{path: "db config set", args: []string{"max_connections=200"}, note: "MySQL restarts"},
		{path: "db engine tune", note: "MySQL restarts"},
		{path: "backup target rm", args: []string{"offsite"}, note: "password"},
		{path: "dns-provider rm", args: []string{"cf"}, note: "renew"},
		{path: "ssl panel self-signed", note: "browsers"},
		// Settings and accounts.
		{path: "update settings", flags: map[string]string{"repo": "-"}, note: "switched off"},
		{path: "update settings", flags: map[string]string{"clear-token": "true"}, note: "new token"},
		{path: "update settings", flags: map[string]string{"channel": "beta"}},
		{path: "config set", args: []string{"web.listen", ":9443"}, flags: map[string]string{"restart": "true"}, note: "another address"},
		{path: "config set", args: []string{"log.level", "debug"}, flags: map[string]string{"restart": "true"}, note: "monopanel-api restarts"},
		{path: "config set", args: []string{"log.level", "debug"}},
		{path: "user set", args: []string{"bob"}, flags: map[string]string{"sftp-only": "true"}, note: "SSH login is closed"},
		{path: "user set", args: []string{"bob"}, flags: map[string]string{"status": "active"}, note: "works again"},
		{path: "user totp-reset", args: []string{"bob"}, note: "2FA"},
		{path: "app stop", args: []string{"bot"}, note: "proxy mode"},
		{path: "token revoke", args: []string{"7"}, note: "401"},
	}
	for _, c := range cases {
		cmd := findCmd(root, c.path)
		if cmd == nil {
			t.Fatalf("no %s in the test tree", c.path)
		}
		f := newForm(cmd, adminEnv(), &act{path: c.path}, c.vals)
		i := 0
		for _, fd := range f.fields {
			if fd.arg && i < len(c.args) {
				fd.setValue(c.args[i])
				i++
			}
		}
		for k, v := range c.flags {
			fd := fieldByName(f, k)
			if fd == nil {
				t.Fatalf("%s: no --%s", c.path, k)
			}
			fd.setValue(v)
		}
		got := confirmFor(f)
		what := "mp " + c.path + " " + strings.Join(c.args, " ") + " " + f.line()
		switch {
		case c.note == "" && got != nil:
			t.Errorf("%s: asks, but the web UI does not: %q", what, got.note)
		case c.note != "" && got == nil:
			t.Errorf("%s: does not ask, but the web UI does", what)
		case c.note != "" && !strings.Contains(got.note, c.note):
			t.Errorf("%s: the note %q lacks %q", what, got.note, c.note)
		}
	}
}

// TestMailboxEditWithoutAChange: the mailbox's edit form starts from its
// state; --active switched to "" sends no change at all, so the panel
// generates a password — which the confirmation says.
func TestMailboxEditWithoutAChange(t *testing.T) {
	root := confirmTree()
	a := &act{path: "mail box set", args: []string{"address"}, prefill: same("name", "quota", "active"), change: true}
	f := newForm(findCmd(root, "mail box set"), adminEnv(), a, map[string]string{"address": "a@example.com", "name": "Ann", "quota": "500", "active": "true"})
	if c := confirmFor(f); c == nil || !strings.Contains(c.note, "generates a new password") {
		t.Errorf("nothing changed is a new password: %+v", c)
	}
	fieldByName(f, "active").setValue("")
	if !strings.Contains(f.line(), "--active=''") {
		t.Fatalf("line %q", f.line())
	}
	if c := confirmFor(f); c == nil || !strings.Contains(c.note, "generates a new password") {
		t.Errorf("--active='' changes nothing either: %+v", c)
	}
	fieldByName(f, "active").setValue("false")
	if c := confirmFor(f); c == nil || !strings.Contains(c.note, "bounce") {
		t.Errorf("switching it off: %+v", c)
	}
	fieldByName(f, "active").setValue("true")
	fieldByName(f, "name").setValue("Anna")
	if c := confirmFor(f); c != nil {
		t.Errorf("a new name asks nothing: %+v", c)
	}
}

// TestConfirmFitsTheScreen: however long the explanation, the dialog keeps
// its frame, the command and the question on screen; the explanation is
// cut short instead.
func TestConfirmFitsTheScreen(t *testing.T) {
	long := strings.Repeat("The panel does a great many things here, each of them worth knowing. ", 40)
	for _, size := range [][2]int{{60, 16}, {80, 24}, {100, 30}} {
		for _, word := range []string{"", "example.com"} {
			m, _ := newTestModel(t, "admin", apiRoutes())
			m.width, m.height = size[0], size[1]
			m.overlay = &confirmOverlay{spec: &confirmSpec{title: "Apply", line: "mp site apply example.com", note: long, word: word}}
			v := viewText(m)
			what := fmt.Sprintf("%dx%d word %q", size[0], size[1], word)
			ask := "[y/N]"
			if word != "" {
				ask = "Type example.com to confirm"
			}
			for _, want := range []string{"mp site apply example.com", ask, "╰", "The panel does"} {
				if !strings.Contains(v, want) {
					t.Errorf("%s: %q is off the screen:\n%s", what, want, v)
				}
			}
			if !strings.Contains(v, "…") {
				t.Errorf("%s: the cut explanation ends with …:\n%s", what, v)
			}
		}
	}
	// A note that fits is shown whole.
	m, _ := newTestModel(t, "admin", apiRoutes())
	m.overlay = &confirmOverlay{spec: &confirmSpec{title: "Apply", line: "mp site apply example.com", note: "Short and complete."}}
	if v := viewText(m); !strings.Contains(v, "Short and complete.") || strings.Contains(v, "…") {
		t.Errorf("a short note:\n%s", v)
	}
}

// TestReinstallFromTheStack: an installed component is installed again from
// its row, as the web UI's "Reinstall", and the question says so.
func TestReinstallFromTheStack(t *testing.T) {
	routes := apiRoutes()
	routes["/api/v1/stack"] = []map[string]any{
		{"name": "nginx", "kind": "web", "installed": true, "version": "1.28"},
		{"name": "memcached", "kind": "tool", "installed": false, "removable": true},
	}
	m, fr := newTestModelOf(t, confirmTree(), "admin", routes)
	openArea(t, m, "Extensions")
	s := m.top().(*listScreen)
	hinted := false
	for _, h := range s.hints(m) {
		hinted = hinted || (h.key == "i" && h.text == "Reinstall")
	}
	if !hinted {
		t.Errorf("an installed component offers i — reinstall: %+v", s.hints(m))
	}
	press(m, "i", "ctrl+s")
	ov, ok := m.overlay.(*confirmOverlay)
	if !ok || !strings.Contains(ov.spec.line, "mp stack install nginx") || !strings.Contains(ov.spec.note, "runs again") {
		t.Fatalf("reinstalling asks, saying it runs again: %T %+v", m.overlay, m.overlay)
	}
	press(m, "y")
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"stack", "install", "nginx"}) {
		t.Errorf("jobs %+v", jobs)
	}
	// One not installed: the first installation, said as such.
	press(m, "esc")
	s = m.top().(*listScreen)
	s.cursor = 1
	press(m, "i", "ctrl+s")
	if ov, ok := m.overlay.(*confirmOverlay); !ok || strings.Contains(ov.spec.note, "runs again") {
		t.Errorf("a first installation: %T %+v", m.overlay, m.overlay)
	}
}

// TestScreenNoteComesFirst: a screen that explains the action itself keeps
// its words; the confirmation adds nothing of its own then.
func TestScreenNoteComesFirst(t *testing.T) {
	root := confirmTree()
	a := &act{path: "firewall disable", note: func(map[string]string) string { return "the screen's own words" }}
	if c := confirmFor(newForm(findCmd(root, "firewall disable"), adminEnv(), a, nil)); c == nil || c.note != "the screen's own words" {
		t.Errorf("%+v", c)
	}
	// config set --restart: the restart is said first, then the screen's warning.
	a = &act{path: "config set", note: func(map[string]string) string { return "may move the panel" }}
	f := newForm(findCmd(root, "config set"), adminEnv(), a, nil)
	f.fields[0].setValue("web.listen")
	f.fields[1].setValue(":9443")
	fieldByName(f, "restart").setValue("true")
	if c := confirmFor(f); c == nil || !strings.HasPrefix(c.note, "monopanel-api restarts") || !strings.HasSuffix(c.note, "may move the panel") {
		t.Errorf("%+v", c)
	}
}

func TestQuickKeysAsk(t *testing.T) {
	routes := apiRoutes()
	routes["/api/v1/mail"] = map[string]any{"installed": true, "hostname": "mx.example.com"}
	routes["/api/v1/firewall"] = map[string]any{"enabled": true, "active": true, "fail2ban": map[string]any{"installed": true, "running": true,
		"jails": []map[string]any{{"name": "sshd", "banned": 1, "total": 3, "ips": []string{"192.0.2.9"}}}}}
	m, fr := newTestModel(t, "admin", routes)

	// The sites list: S suspends an active site and resumes a suspended
	// one — after asking, with nothing to fill in first.
	openArea(t, m, "Sites")
	press(m, "S")
	if ov, ok := m.overlay.(*confirmOverlay); !ok || !strings.Contains(ov.spec.line, "mp site suspend example.com") || !strings.Contains(ov.spec.note, "503") {
		t.Fatalf("site suspend asks at once: %T %T", m.overlay, m.top())
	}
	press(m, "esc")
	s := m.top().(*listScreen)
	s.cursor = 1 // old.example.net, suspended
	press(m, "S")
	ov, ok := m.overlay.(*confirmOverlay)
	if !ok || !strings.Contains(ov.spec.line, "mp site unsuspend old.example.net") || len(fr.started()) != 0 {
		t.Fatalf("site unsuspend asks first: %T", m.overlay)
	}
	press(m, "n")
	if m.overlay != nil || len(fr.started()) != 0 {
		t.Fatal("n cancels")
	}
	press(m, "S", "y")
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"site", "unsuspend", "old.example.net"}) {
		t.Fatalf("y runs it: %+v", jobs)
	}

	// Mail: p regenerates the configuration — after asking.
	openArea(t, m, "Mail")
	press(m, "p")
	if ov, ok := m.overlay.(*confirmOverlay); !ok || !strings.Contains(ov.spec.note, "postfix") {
		t.Fatalf("mail apply asks, saying what it overwrites: %T", m.overlay)
	}
	press(m, "y")
	if jobs := fr.started(); len(jobs) != 2 || !slices.Equal(jobs[1].argv, []string{"mail", "apply"}) {
		t.Fatalf("mail apply runs: %+v", jobs)
	}

	// Firewall: u on a banned address unbans it — after asking.
	openArea(t, m, "Firewall")
	press(m, "b")
	if _, ok := m.top().(*listScreen); !ok {
		t.Fatalf("the bans: %T", m.top())
	}
	press(m, "u")
	if ov, ok := m.overlay.(*confirmOverlay); !ok || !strings.Contains(ov.spec.line, "mp firewall unban 192.0.2.9") {
		t.Fatalf("firewall unban asks: %T", m.overlay)
	}
	press(m, "y")
	if jobs := fr.started(); len(jobs) != 3 || !slices.Equal(jobs[2].argv, []string{"firewall", "unban", "192.0.2.9"}) {
		t.Fatalf("firewall unban runs: %+v", jobs)
	}
}

// TestFixEverySiteAsks: the doctor's "fix every site" runs mp site fix for
// each site after one question.
func TestFixEverySiteAsks(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	drive(m, fixAllSites(m, nil))
	ov, ok := m.overlay.(*confirmOverlay)
	if !ok || len(fr.started()) != 0 {
		t.Fatalf("fixing every site asks first: %T", m.overlay)
	}
	if !strings.Contains(ov.spec.line, "mp site fix example.com") || !strings.Contains(ov.spec.note, "ACL") {
		t.Errorf("the question: %+v", ov.spec)
	}
	press(m, "y")
	var got []string
	for _, j := range fr.started() {
		got = append(got, strings.Join(j.argv, " "))
	}
	if !slices.Equal(got, []string{"site fix example.com", "site fix old.example.net"}) {
		t.Errorf("jobs %q", got)
	}
}

// TestMissingCommandKeysAreQuiet: a key bound to a command this mp does not
// have (site tls is not in the test tree) is neither shown nor answered.
func TestMissingCommandKeysAreQuiet(t *testing.T) {
	m, fr := newTestModel(t, "admin", apiRoutes())
	openArea(t, m, "Sites")
	s := m.top().(*listScreen)
	for _, h := range s.hints(m) {
		if h.key == "t" {
			t.Error("the key of a missing command is shown")
		}
	}
	press(m, "t")
	if m.top() != s || m.overlay != nil || m.flashText != "" || len(fr.started()) != 0 {
		t.Errorf("the key of a missing command did something: %T %T %q", m.top(), m.overlay, m.flashText)
	}
}
