package cli

import (
	"net/http"
	"strings"
	"testing"
)

// mp stack list shows what the repositories offer and how it gets installed;
// mp stack upgrade sends the component (all by default); mp stack
// auto-upgrade shows the daily upgrade and changes only what a flag sets.
func TestStackUpgradeCommands(t *testing.T) {
	auto := map[string]any{"enabled": true, "hour": 4, "timezone": "Asia/Almaty", "next_run": "2026-11-22T04:00:00+05:00",
		"last": map[string]any{"job_id": 41, "component": "all", "scheduled": true, "at": "2026-11-21T04:00:12Z", "upgraded": []string{"nginx 1.30.5-1~noble → 1.30.7-1~noble"}}}
	f := newFakeAPI(t, map[string]fakeResp{
		"GET /stack": {body: []map[string]any{
			{"name": "nginx", "installed": true, "version": "1.30.5-1~noble", "kind": "web",
				"upgrade": map[string]any{"component": "nginx", "from": "1.30.5-1~noble", "to": "1.30.7-1~noble", "origin": "nginx.org", "packages": []any{}}},
			{"name": "percona", "installed": true, "version": "8.4.6-6", "kind": "db",
				"upgrade": map[string]any{"component": "db", "from": "8.4.6-6-1.noble", "to": "8.4.7-7-1.noble", "packages": []any{}}},
			{"name": "memcached", "installed": true, "version": "1.6.24-1ubuntu0.3", "kind": "tool"},
			{"name": "apache", "installed": true, "version": "2.4.58-1ubuntu8.9", "kind": "web", "instances": map[string]any{"running": 2, "total": 2, "replaced": 1}},
			{"name": "fail2ban", "installed": true, "version": "1.0.2-3ubuntu0.1", "kind": "security", "service": map[string]any{"unit": "fail2ban.service", "active_state": "active", "binary_replaced": true}},
		}},
		"POST /stack/upgrade":     {status: http.StatusAccepted, body: map[string]any{"job_id": 42}},
		"GET /stack/auto-upgrade": {body: auto},
		"PUT /stack/auto-upgrade": {body: auto},
	})
	out, err := mp(t, "", "stack", "list")
	if err != nil {
		t.Fatal(err)
	}
	// An Apache on the binary the distribution's upgrade replaced: the
	// upgrade restarts it, with nothing to install. fail2ban on a Python
	// the distribution upgraded is none of the upgrade's: a restart by hand.
	for _, want := range []string{"UPDATE", "→ 1.30.7-1~noble (nginx.org)", "→ 8.4.7-7-1.noble", "updates wait: mp stack upgrade (nginx, PHP, Apache), or the daily upgrade at 04:00 installs them\n", "mp stack upgrade db (the server restarts)",
		"2 of 2 running · on a replaced binary", "apache runs a binary an upgrade replaced: mp stack upgrade apache restarts it after checking the configuration\n",
		"fail2ban runs a binary an upgrade replaced: the new version takes effect after systemctl restart fail2ban.service\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("stack list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mp stack upgrade fail2ban") {
		t.Errorf("the upgrade restarts fail2ban:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "memcached") && !strings.HasSuffix(strings.TrimSpace(line), " -") {
			t.Errorf("memcached has no update: %q", line)
		}
	}

	// The daily upgrade off, or not to be read: the hint says so, or
	// nothing of it.
	for state, want := range map[string]string{
		"off":        "updates wait: mp stack upgrade (nginx, PHP, Apache); the daily upgrade is off: mp stack auto-upgrade --on\n",
		"unreadable": "updates wait: mp stack upgrade (nginx, PHP, Apache)\n",
	} {
		resp := fakeResp{body: map[string]any{"enabled": false, "hour": 4, "timezone": "Asia/Almaty"}}
		if state == "unreadable" {
			resp = fakeResp{status: http.StatusBadGateway, body: map[string]any{"title": "Bad Gateway", "status": 502}}
		}
		newFakeAPI(t, map[string]fakeResp{"GET /stack": f.routes["GET /stack"], "GET /stack/auto-upgrade": resp})
		out, err := mp(t, "", "stack", "list")
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("daily upgrade %s: %v\n%s", state, err, out)
		}
	}
	f = newFakeAPI(t, f.routes)

	if _, err := mp(t, "", "--no-wait", "stack", "upgrade"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "POST", "/stack/upgrade"), map[string]any{"component": "all"})

	out, err = mp(t, "", "stack", "auto-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"daily upgrade of nginx, PHP and Apache: on, at 04:00 (Asia/Almaty)", "next run: 2026-11-22 04:00", "job #41", "nginx 1.30.5-1~noble → 1.30.7-1~noble"} {
		if !strings.Contains(out, want) {
			t.Errorf("auto-upgrade lacks %q:\n%s", want, out)
		}
	}
	f.none(t, "PUT", "/stack/auto-upgrade")
	if _, err := mp(t, "", "stack", "auto-upgrade", "--off"); err != nil {
		t.Fatal(err)
	}
	wantBody(t, f.call(t, "PUT", "/stack/auto-upgrade"), map[string]any{"enabled": false})
	if _, err := mp(t, "", "stack", "auto-upgrade", "--on", "--off"); err == nil {
		t.Error("--on with --off")
	}
}

// The component goes as given; db only by name.
func TestStackUpgradeComponent(t *testing.T) {
	for _, c := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"stack", "upgrade", "db"}, map[string]any{"component": "db"}},
		{[]string{"stack", "upgrade", "php"}, map[string]any{"component": "php"}},
		{[]string{"stack", "auto-upgrade", "--on", "--hour", "3"}, map[string]any{"enabled": true, "hour": float64(3)}},
		{[]string{"stack", "auto-upgrade", "--hour", "23"}, map[string]any{"hour": float64(23)}},
	} {
		f := newFakeAPI(t, map[string]fakeResp{
			"POST /stack/upgrade":     {status: http.StatusAccepted, body: map[string]any{"job_id": 7}},
			"PUT /stack/auto-upgrade": {body: map[string]any{"enabled": true, "hour": 3, "timezone": "UTC"}},
		})
		if _, err := mp(t, "", append([]string{"--no-wait"}, c.args...)...); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		method, path := "POST", "/stack/upgrade"
		if c.args[1] == "auto-upgrade" {
			method, path = "PUT", "/stack/auto-upgrade"
		}
		wantBody(t, f.call(t, method, path), c.want)
	}
}

// While the move of an upgrade onto the accounts' Apaches has not finished,
// mp stack list counts the Apaches that exist, the sites that wait, and the
// shared apache2.service that serves them: on a replaced binary, mp stack
// upgrade apache restarts it; down, the sites that wait answer 502 until it
// runs, as the doctor says.
func TestStackListDuringTheApacheMove(t *testing.T) {
	apache := func(state string, replaced bool) fakeResp {
		return fakeResp{body: []map[string]any{{"name": "apache", "installed": true, "version": "2.4.65-2", "kind": "web",
			"instances": map[string]any{"running": 1, "total": 1, "waiting": 2},
			"service":   map[string]any{"unit": "apache2.service", "active_state": state, "binary_replaced": replaced}}}}
	}
	newFakeAPI(t, map[string]fakeResp{"GET /stack": apache("active", true), "GET /stack/auto-upgrade": {body: map[string]any{"enabled": true, "hour": 4}}})
	out, err := mp(t, "", "stack", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"installed · 1 of 1 running · sites waiting for the move: 2 · apache2.service active · on a replaced binary",
		"apache runs a binary an upgrade replaced: mp stack upgrade apache restarts it after checking the configuration\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("stack list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "answer 502") {
		t.Errorf("the shared Apache runs:\n%s", out)
	}
	newFakeAPI(t, map[string]fakeResp{"GET /stack": apache("failed", false), "GET /stack/auto-upgrade": {body: map[string]any{"enabled": true, "hour": 4}}})
	out, err = mp(t, "", "stack", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sites waiting for the move: 2 · apache2.service failed",
		"apache: apache2.service is failed, and the sites that wait for the move onto their accounts' Apaches (2) answer 502 until it runs: systemctl start apache2.service\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("stack list lacks %q:\n%s", want, out)
		}
	}
}
