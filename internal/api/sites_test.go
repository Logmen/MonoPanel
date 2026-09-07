package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monopanel/internal/agent/agenttest"
	"monopanel/internal/auth"
	"monopanel/internal/config"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// siteFixture is a running panel wired to a fake agent, with an admin, an
// active site owner and an installed PHP branch: enough to exercise the whole
// site pipeline (validation, job, rendered configuration) without root.
type siteFixture struct {
	t      *testing.T
	s      *Server
	db     *store.DB
	agent  *agenttest.Agent
	ts     *httptest.Server
	cookie *http.Cookie
	ctx    context.Context
}

func newSiteFixture(t *testing.T) *siteFixture { return newFixture(t, nil) }

// newFixture is newSiteFixture with a say in the configuration, for tests that
// need a pinned release key or another data directory.
func newFixture(t *testing.T, tweak func(*config.Config)) *siteFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	fake := agenttest.Start(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.RunDir = t.TempDir()
	// A real key file makes the encrypted settings (tokens) work in tests.
	cfg.SecretKeyFile = filepath.Join(cfg.DataDir, "secret.key")
	if err := os.WriteFile(cfg.SecretKeyFile, []byte(strings.Repeat("k", 48)), 0o600); err != nil {
		t.Fatal(err)
	}
	if tweak != nil {
		tweak(&cfg)
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=debian\nVERSION_ID=13\n"))
	profile, _ := osprofile.FromRelease(rel)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := jobs.NewRunner(db, 2, quiet)
	s := New(cfg, db, fake.Client(), runner, profile, quiet)
	s.SetReadinessWaits(0, 0) // no php-fpm socket and no nginx behind the fake agent
	runner.Start(ctx)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	h, _ := auth.HashPassword("secret-password")
	if err := db.CreateUser(ctx, &store.User{Login: "admin", Role: store.RoleAdmin, PasswordHash: h}); err != nil {
		t.Fatal(err)
	}
	uid, gid := 1500, 1500
	owner := &store.User{Login: "alex", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &gid, Home: "/var/www/alex", Shell: true}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPHPVersion(ctx, &store.PHPVersion{Version: "8.4", Source: "sury", Status: store.PHPInstalled}); err != nil {
		t.Fatal(err)
	}

	f := &siteFixture{t: t, s: s, db: db, agent: fake, ts: ts, ctx: ctx}
	f.login()
	return f
}

// login authenticates as the administrator and keeps the session cookie, so
// requests travel through the real authentication and CSRF middleware.
func (f *siteFixture) login() { f.loginAs("admin", "secret-password") }

// loginAs switches the fixture to another account.
func (f *siteFixture) loginAs(login, password string) {
	f.t.Helper()
	body := `{"login":"` + login + `","password":"` + password + `"}`
	res, err := http.Post(f.ts.URL+"/api/v1/auth/login", "application/json", strings.NewReader(body))
	if err != nil || res.StatusCode != http.StatusOK {
		f.t.Fatalf("login as %s: %v %v", login, res, err)
	}
	defer res.Body.Close()
	f.cookie = nil
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName {
			f.cookie = c
		}
	}
	if f.cookie == nil {
		f.t.Fatal("no session cookie")
	}
}

// createSite posts a site and waits for its apply job to finish.
func (f *siteFixture) createSite(body map[string]any) *store.Site {
	f.t.Helper()
	var out struct {
		Site  *store.Site `json:"site"`
		JobID int64       `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites", body, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	site, err := f.db.GetSiteByDomain(f.ctx, out.Site.Domain)
	if err != nil {
		f.t.Fatalf("site %s not stored: %v", out.Site.Domain, err)
	}
	return site
}

func (f *siteFixture) waitJob(id int64) *store.Job {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		job, err := f.db.GetJob(f.ctx, id)
		if err != nil {
			f.t.Fatalf("job %d: %v", id, err)
		}
		if job.Status == store.JobDone || job.Status == store.JobFailed {
			return job
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %d stuck in %s", id, job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSiteApplyRendersConfiguration(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "www": true})

	if site.Status != store.SiteActive {
		t.Fatalf("status = %s, last_error = %s", site.Status, site.LastError)
	}
	conf, ok := f.agent.File("/etc/nginx/monopanel/sites/example.com.conf")
	if !ok {
		t.Fatalf("no nginx config written; files: %v", f.agent.Files())
	}
	for _, want := range []string{
		"server_name example.com www.example.com;",
		"root  /var/www/alex/data/www/example.com;",
		"fastcgi_pass unix:" + filepath.Join(f.s.cfg.RunDir, "php", "example.com.sock") + ";",
		"include monopanel/sites/example.com.d/*.conf;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("nginx config missing %q\n%s", want, conf)
		}
	}
	pool, ok := f.agent.File("/etc/php/8.4/fpm/pool.d/example.com.conf")
	if !ok {
		t.Fatal("no php-fpm pool written")
	}
	for _, want := range []string{"[example.com]", "user  = alex", "pm = ondemand"} {
		if !strings.Contains(pool, want) {
			t.Errorf("pool missing %q", want)
		}
	}
	if f.agent.UnitAction("nginx.service") == "" {
		t.Error("nginx was not reloaded")
	}
}

func TestSitePresetsRenderExpectedRules(t *testing.T) {
	cases := []struct {
		preset string
		want   []string
		absent string
	}{
		{"wordpress", []string{"location = /xmlrpc.php  { deny all; return 403; }", "wp-content/(?:uploads|files|cache|upgrade)", "try_files $uri $uri/ /index.php?$args;"}, ""},
		{"joomla", []string{"location /api/ { try_files $uri $uri/ /api/index.php?$args; }", "configuration\\.php"}, ""},
		{"bitrix", []string{"/bitrix/urlrewrite.php$is_args$args", "local_cache"}, "/index.php?$args"},
		{"opencart", []string{"_route_=$1", "location ~* ^/(?:system|storage|vendor)/"}, ""},
	}
	for _, c := range cases {
		t.Run(c.preset, func(t *testing.T) {
			f := newSiteFixture(t)
			site := f.createSite(map[string]any{"domain": c.preset + ".example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "preset": c.preset})
			if site.Preset != c.preset {
				t.Fatalf("preset = %q", site.Preset)
			}
			conf, ok := f.agent.File("/etc/nginx/monopanel/sites/" + c.preset + ".example.com.conf")
			if !ok {
				t.Fatal("no nginx config written")
			}
			for _, want := range c.want {
				if !strings.Contains(conf, want) {
					t.Errorf("%s preset missing %q\n%s", c.preset, want, conf)
				}
			}
			if c.absent != "" && strings.Contains(conf, c.absent) {
				t.Errorf("%s preset should not contain %q", c.preset, c.absent)
			}
			// Preset PHP values must reach the pool but stay overridable.
			pool, _ := f.agent.File("/etc/php/8.4/fpm/pool.d/" + c.preset + ".example.com.conf")
			if c.preset == "bitrix" && !strings.Contains(pool, "php_value[short_open_tag] = On") {
				t.Errorf("bitrix pool missing short_open_tag:\n%s", pool)
			}
		})
	}
}

func TestSitePresetOverriddenByPHPIni(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{
		"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none",
		"preset": "bitrix", "php_ini": map[string]string{"memory_limit": "1024M"},
	})
	pool, _ := f.agent.File("/etc/php/8.4/fpm/pool.d/shop.example.com.conf")
	if !strings.Contains(pool, "php_value[memory_limit] = 1024M") {
		t.Errorf("site php_ini must win over the preset:\n%s", pool)
	}
	if strings.Contains(pool, "php_value[memory_limit] = 512M") {
		t.Error("preset value still present after override")
	}
}

func TestSiteAllowFromAndValidation(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{
		"domain": "closed.example.com", "user": "alex", "php_version": "8.4", "ssl": "none",
		"allow_from": []string{"203.0.113.0/24", "198.51.100.7"},
	})
	if len(site.AllowFrom) != 2 {
		t.Fatalf("allow_from = %v", site.AllowFrom)
	}
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/closed.example.com.conf")
	for _, want := range []string{"allow 203.0.113.0/24;", "allow 198.51.100.7;", "deny  all;"} {
		if !strings.Contains(conf, want) {
			t.Errorf("missing %q in\n%s", want, conf)
		}
	}
	// ACME must stay reachable even for a restricted site.
	if !strings.Contains(conf, "include monopanel/snippets/acme.conf;") {
		t.Error("acme snippet dropped for a restricted site")
	}

	// Garbage is rejected before anything is written.
	f.call(http.MethodPost, "/sites", map[string]any{
		"domain": "bad.example.com", "user": "alex", "php_version": "8.4", "allow_from": []string{"not-an-ip"},
	}, http.StatusUnprocessableEntity, nil)
}

func TestSiteRejectsBadInput(t *testing.T) {
	f := newSiteFixture(t)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown php", map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "7.2"}},
		{"unknown owner", map[string]any{"domain": "b.example.com", "user": "nobody", "php_version": "8.4"}},
		{"proxy without backend", map[string]any{"domain": "c.example.com", "user": "alex", "mode": "proxy"}},
		{"proxy to public ip", map[string]any{"domain": "d.example.com", "user": "alex", "mode": "proxy", "backend": "http://8.8.8.8:80"}},
		{"php_ini not allow-listed", map[string]any{"domain": "e.example.com", "user": "alex", "php_version": "8.4", "php_ini": map[string]string{"disable_functions": ""}}},
		{"docroot escape", map[string]any{"domain": "f.example.com", "user": "alex", "php_version": "8.4", "docroot": "../../etc"}},
		{"unknown preset", map[string]any{"domain": "g.example.com", "user": "alex", "php_version": "8.4", "preset": "drupal"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.call(http.MethodPost, "/sites", c.body, http.StatusUnprocessableEntity, nil)
			if _, err := f.db.GetSiteByDomain(f.ctx, c.body["domain"].(string)); err == nil {
				t.Error("invalid site was stored")
			}
		})
	}
}

func TestSiteSuspendAndDelete(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "temp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})

	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/"+site.Domain+"/suspend", nil, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/temp.example.com.conf")
	if !strings.Contains(conf, "return 503") {
		t.Errorf("suspended site should serve 503:\n%s", conf)
	}
	if s, _ := f.db.GetSiteByDomain(f.ctx, site.Domain); s.Status != store.SiteSuspended {
		t.Errorf("status = %s", s.Status)
	}

	f.call(http.MethodDelete, "/sites/"+site.Domain+"?purge=true", nil, http.StatusAccepted, &out)
	f.waitJob(out.JobID)
	if _, err := f.db.GetSiteByDomain(f.ctx, site.Domain); err == nil {
		t.Error("site row survived delete")
	}
	removed := strings.Join(f.agent.Removed(), " ")
	for _, want := range []string{"/etc/nginx/monopanel/sites/temp.example.com.conf", "/var/www/alex/data/www/temp.example.com"} {
		if !strings.Contains(removed, want) {
			t.Errorf("delete did not remove %s (removed: %s)", want, removed)
		}
	}
}

func TestSiteCustomNginxValidatedAndRolledBack(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "custom.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})

	var got map[string]any
	f.call(http.MethodPut, "/sites/"+site.Domain+"/nginx", map[string]any{"custom": "add_header X-Test 1 always;"}, http.StatusOK, &got)
	custom, ok := f.agent.File("/etc/nginx/monopanel/sites/custom.example.com.d/custom.conf")
	if !ok || !strings.Contains(custom, "X-Test") {
		t.Fatalf("custom directives not written: %q", custom)
	}

	// A validator failure must surface as 422, not a 500.
	f.agent.Fail["/v1/config/apply"] = "nginx: [emerg] unknown directive"
	f.call(http.MethodPut, "/sites/"+site.Domain+"/nginx", map[string]any{"custom": "nonsense;"}, http.StatusUnprocessableEntity, nil)
	delete(f.agent.Fail, "/v1/config/apply")
}

// Смена ветки PHP: пул старой версии удаляется, и её мастер перезапускается
// раньше, чем занимает сокет новый — иначе новый не стартует.
func TestSitePHPSwitchReleasesOldPoolFirst(t *testing.T) {
	f := newSiteFixture(t)
	if err := f.db.UpsertPHPVersion(f.ctx, &store.PHPVersion{Version: "8.3", Source: "sury", Status: store.PHPInstalled}); err != nil {
		t.Fatal(err)
	}
	site := f.createSite(map[string]any{"domain": "switch.example.com", "user": "alex", "php_version": "8.3", "ssl": "none"})
	if site.PHPVersion != "8.3" {
		t.Fatalf("создан на %s", site.PHPVersion)
	}
	// Reset() стёр бы и файлы, которые фейковый агент «записал» при создании,
	// и тогда пул 8.3 не считался бы существующим — смотрим по срезу вызовов.
	base := len(f.agent.Calls())

	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPatch, "/sites/switch.example.com", map[string]any{"php_version": "8.4"}, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("переключение: %s %s", job.Status, job.Error)
	}

	// Порядок: сначала удаление пула 8.3 и перезапуск его мастера, потом
	// запись нового пула и перезапуск 8.4.
	removeAt, oldReloadAt, applyAt := -1, -1, -1
	for i, c := range f.agent.Calls()[base:] {
		body := string(c.Body)
		switch {
		case c.Path == "/v1/paths/remove" && strings.Contains(body, "/etc/php/8.3/fpm/pool.d/switch.example.com.conf"):
			removeAt = i
		case c.Path == "/v1/service" && strings.Contains(body, "php8.3-fpm.service"):
			oldReloadAt = i
		case c.Path == "/v1/config/apply" && strings.Contains(body, "/etc/php/8.4/fpm/pool.d/switch.example.com.conf"):
			applyAt = i
		}
	}
	if removeAt < 0 || oldReloadAt < 0 || applyAt < 0 {
		t.Fatalf("шаги не найдены: remove=%d reload=%d apply=%d", removeAt, oldReloadAt, applyAt)
	}
	if !(removeAt < oldReloadAt && oldReloadAt < applyAt) {
		t.Fatalf("новый пул применён до освобождения сокета: remove=%d reload=%d apply=%d", removeAt, oldReloadAt, applyAt)
	}
}

func TestSitePHPEndpointReportsSources(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{
		"domain": "php.example.com", "user": "alex", "php_version": "8.4", "ssl": "none",
		"preset": "wordpress", "php_ini": map[string]string{"max_execution_time": "45"},
	})
	var out struct {
		Version string `json:"version"`
		Values  []struct {
			Key, Value, Source string
		} `json:"values"`
		Allowed []string `json:"allowed"`
	}
	f.call(http.MethodGet, "/sites/"+site.Domain+"/php", nil, http.StatusOK, &out)
	sources := map[string]struct{ value, source string }{}
	for _, v := range out.Values {
		sources[v.Key] = struct{ value, source string }{v.Value, v.Source}
	}
	if got := sources["max_execution_time"]; got.source != "site" || got.value != "45" {
		t.Errorf("max_execution_time = %+v, want site/45", got)
	}
	if got := sources["upload_max_filesize"]; got.source != "preset" || got.value != "128M" {
		t.Errorf("upload_max_filesize = %+v, want preset/128M", got)
	}
	if got := sources["display_errors"]; got.source != "default" {
		t.Errorf("display_errors source = %s, want default", got.source)
	}
	if len(out.Allowed) == 0 {
		t.Error("allowed keys not reported")
	}
}

func TestUserDeleteRemovesEverythingOwned(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "owned.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobDone {
		t.Fatalf("user.delete failed: %s", job.Error)
	}
	if _, err := f.db.GetUserByLogin(f.ctx, "alex"); err == nil {
		t.Error("user row survived")
	}
	if _, err := f.db.GetSiteByDomain(f.ctx, "owned.example.com"); err == nil {
		t.Error("site of a deleted user survived")
	}
	if !f.agent.Called("/v1/user/remove") {
		t.Error("unix account was not removed")
	}
	if !strings.Contains(strings.Join(f.agent.Removed(), " "), "/etc/nginx/monopanel/sites/owned.example.com.conf") {
		t.Error("site configuration of a deleted user was left behind")
	}
}

func TestUserDeleteRefusesSelfAndLastAdmin(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodDelete, "/users/admin", nil, http.StatusUnprocessableEntity, nil)
	if _, err := f.db.GetUserByLogin(f.ctx, "admin"); err != nil {
		t.Fatal("admin must survive a refused delete")
	}
}

func TestPresetsEndpoint(t *testing.T) {
	f := newSiteFixture(t)
	var out []struct{ ID, Name, Description string }
	f.call(http.MethodGet, "/sites/presets", nil, http.StatusOK, &out)
	ids := map[string]bool{}
	for _, p := range out {
		ids[p.ID] = true
		if p.Name == "" || p.Description == "" {
			t.Errorf("preset %q has no name or description", p.ID)
		}
	}
	for _, want := range []string{"", "wordpress", "joomla", "bitrix", "opencart"} {
		if !ids[want] {
			t.Errorf("preset %q missing", want)
		}
	}
}

// call performs an authenticated request against the API and checks the status.
func (f *siteFixture) call(method, path string, body any, wantStatus int, out any) {
	f.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(f.ctx, method, f.ts.URL+"/api/v1"+path, rdr)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != wantStatus {
		f.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.StatusCode, wantStatus, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("%s %s: decode %v: %s", method, path, err, raw)
		}
	}
}
