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
	// upgrade restarts it, with nothing to install.
	for _, want := range []string{"UPDATE", "→ 1.30.7-1~noble (nginx.org)", "→ 8.4.7-7-1.noble", "updates wait: mp stack upgrade (nginx, PHP, Apache)", "mp stack upgrade db (the server restarts)",
		"2 of 2 running · on a replaced binary", "a service runs a binary an upgrade replaced"} {
		if !strings.Contains(out, want) {
			t.Errorf("stack list lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "memcached") && !strings.HasSuffix(strings.TrimSpace(line), " -") {
			t.Errorf("memcached has no update: %q", line)
		}
	}

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
