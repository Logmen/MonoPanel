package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/auth"
)

const appUnitPath = "/etc/systemd/system/monopanel-app-alex-bot.service"

// appFixture signs in as alex, an ordinary account: app services are
// self-service, so whatever an account may write into its unit matters.
func appFixture(t *testing.T) *siteFixture {
	t.Helper()
	f := newSiteFixture(t)
	u, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := auth.HashPassword("alex-password-1")
	if err := f.db.SetUserPassword(f.ctx, u.ID, h); err != nil {
		t.Fatal(err)
	}
	f.agent.Stat["/usr/bin/python3"] = false
	f.agent.Stat["/var/www/alex/data"] = true
	f.agent.Stat["/var/www/alex/data/app\nExecStartPre=+/bin/sh -c id"] = true
	f.agent.Stat["/var/www/alex/data/.env"] = false
	f.loginAs("alex", "alex-password-1")
	return f
}

// Every field of an app lands in the unit file as written, and systemd ends a
// line at \n, \r or NUL: a line break would let an account add a directive of
// its own, and ExecStartPre=+… runs as root.
func TestAppUnitRefusesInjectedLines(t *testing.T) {
	f := appFixture(t)
	inject := "\n[Service]\nExecStartPre=+/bin/sh -c id"
	cases := map[string]map[string]any{
		"description":           {"description": "backend" + inject},
		"description with NUL":  {"description": "backend\x00ExecStartPre=+/bin/sh -c id"},
		"description backslash": {"description": `backend\`},
		"command arguments":     {"command": "/usr/bin/python3 -m http.server\x00ExecStartPre=+/bin/sh -c id"},
		"command backslash":     {"command": `/usr/bin/python3 -m http.server \`},
		"workdir":               {"workdir": "/var/www/alex/data/app\nExecStartPre=+/bin/sh -c id"},
		"env_file":              {"env_file": "/var/www/alex/data/.env\rExecStartPre=+/bin/sh -c id"},
		"env":                   {"env": []string{"A=1\x00ExecStartPre=+/bin/sh -c id"}},
		"description too long":  {"description": strings.Repeat("ж", 121)},
	}
	for name, extra := range cases {
		body := map[string]any{"name": "bot", "command": "/usr/bin/python3 -m http.server"}
		for k, v := range extra {
			body[k] = v
		}
		f.call(http.MethodPost, "/users/alex/apps", body, http.StatusUnprocessableEntity, nil)
		if unit, ok := f.agent.File(appUnitPath); ok {
			t.Fatalf("%s: the unit was written:\n%s", name, unit)
		}
	}

	// The same through an edit of an app that exists.
	f.call(http.MethodPost, "/users/alex/apps", map[string]any{"name": "bot", "command": "/usr/bin/python3 -m http.server"}, http.StatusCreated, nil)
	before, _ := f.agent.File(appUnitPath)
	for _, body := range []map[string]any{
		{"description": "x" + inject},
		{"workdir": "/var/www/alex/data/app\nExecStartPre=+/bin/sh -c id"},
		{"env": []string{"A=1\nExecStartPre=+/bin/sh -c id"}},
		{"description": strings.Repeat("x", 121)},
	} {
		f.call(http.MethodPatch, "/users/alex/apps/bot", body, http.StatusUnprocessableEntity, nil)
	}
	if after, _ := f.agent.File(appUnitPath); after != before || strings.Contains(after, "ExecStartPre") {
		t.Fatalf("a refused edit changed the unit:\n%s", after)
	}
}

// What an account may set reaches the unit: description, environment, the
// environment file, the restart policy and autostart off.
func TestAppUnitCarriesEveryField(t *testing.T) {
	f := appFixture(t)
	off := false
	f.call(http.MethodPost, "/users/alex/apps", map[string]any{
		"name": "bot", "command": "/usr/bin/python3 -m http.server 3000", "description": "app.example.org backend",
		"env": []string{"PORT=3000", "MODE=prod with spaces"}, "env_file": "/var/www/alex/data/.env", "restart": "on-failure", "enabled": off,
	}, http.StatusCreated, nil)
	unit, ok := f.agent.File(appUnitPath)
	if !ok {
		t.Fatalf("no unit written; files: %v", f.agent.Files())
	}
	for _, want := range []string{
		"Description=MonoPanel app alex/bot — app.example.org backend\n",
		// the account's own env file is read as the account, not by
		// systemd as root
		"ExecStart=/usr/bin/monopanel app-exec --env-file /var/www/alex/data/.env -- /usr/bin/python3 -m http.server 3000\n",
		"WorkingDirectory=/var/www/alex/data\n",
		// the panel's root-only file; the account's own, read later by
		// app-exec, wins, as its file always won over Environment= lines
		"EnvironmentFile=/etc/monopanel/apps/monopanel-app-alex-bot.env\n",
		"Restart=on-failure\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit misses %q\n%s", want, unit)
		}
	}
	// The variables are not in the unit, which every local user can read
	// (and systemctl show prints its Environment= to anyone).
	if strings.Contains(unit, "Environment=") || strings.Contains(unit, "PORT") || strings.Contains(unit, "EnvironmentFile=/var/www") {
		t.Errorf("the unit carries the variables:\n%s", unit)
	}
	env, ok := f.agent.File("/etc/monopanel/apps/monopanel-app-alex-bot.env")
	if !ok || !strings.Contains(env, "PORT=\"3000\"\n") || !strings.Contains(env, "MODE=\"prod with spaces\"\n") {
		t.Errorf("environment file: %v\n%s", ok, env)
	}
	if spec, ok := f.agent.FileSpec("/etc/monopanel/apps/monopanel-app-alex-bot.env"); !ok || spec.Mode != 0o600 || spec.Owner != "root" {
		t.Errorf("the environment file must be root's alone: %+v", spec)
	}

	// Without variables the file goes, and the unit names no panel file.
	f.call(http.MethodPatch, "/users/alex/apps/bot", map[string]any{"env": []string{}}, http.StatusOK, nil)
	if _, ok := f.agent.File("/etc/monopanel/apps/monopanel-app-alex-bot.env"); ok {
		t.Error("the environment file of an app without variables stays")
	}
	if unit, _ := f.agent.File(appUnitPath); strings.Contains(unit, "/etc/monopanel/apps/") {
		t.Errorf("the unit still names the panel's file:\n%s", unit)
	}
	if got := f.agent.UnitAction("monopanel-app-alex-bot.service"); got != "stop" {
		t.Errorf("an app created with autostart off must be stopped, last action %q", got)
	}
}

// The owner of an app is the account in the path: the request has no field
// that could name another one (it used to carry an ignored "user").
func TestAppOwnerIsThePathAccount(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.Stat["/usr/bin/python3"] = false
	f.agent.Stat["/var/www/alex/data"] = true
	schema := f.s.API().OpenAPI().Components.Schemas.Map()["AppRequest"]
	if schema == nil || schema.Properties["name"] == nil || schema.Properties["user"] != nil {
		t.Fatalf("AppRequest in the OpenAPI document: %+v", schema)
	}
	if code, detail := fwCall(f, http.MethodPost, "/users/alex/apps", map[string]any{"name": "bot", "user": "admin", "command": "/usr/bin/python3 -m http.server"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("a body naming another owner: %d %s", code, detail)
	}
	var created struct {
		App struct {
			Login string `json:"login"`
		} `json:"app"`
	}
	f.call(http.MethodPost, "/users/alex/apps", map[string]any{"name": "bot", "command": "/usr/bin/python3 -m http.server"}, http.StatusCreated, &created)
	if created.App.Login != "alex" {
		t.Fatalf("owner: %+v", created)
	}
}

// Logins and app names both may hold "-": alex-b's app c and alex's app b-c
// would share monopanel-app-alex-b-c.service, and the second would rewrite
// the first one's unit and read its journal.
func TestAppUnitNamesDoNotCollide(t *testing.T) {
	f := appFixture(t)
	f.userWithPassword("alex-b", "made-up-password", 2002)
	f.agent.Stat["/var/www/alex-b/data"] = true
	f.loginAs("alex-b", "made-up-password")
	f.call(http.MethodPost, "/users/alex-b/apps", map[string]any{"name": "c", "command": "/usr/bin/python3 -m http.server"}, http.StatusCreated, nil)
	f.loginAs("alex", "alex-password-1")
	status, body := f.do(http.MethodPost, "/users/alex/apps", map[string]any{"name": "b-c", "command": "/usr/bin/python3 -m http.server"})
	if status != http.StatusConflict || !strings.Contains(string(body), "belongs to the app alex-b/c") {
		t.Fatalf("a second app on the same unit: %d %s", status, body)
	}
}
