package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

func (f *siteFixture) installValkey(t *testing.T) {
	t.Helper()
	if job := f.stackJob(t, http.MethodPost, "/stack/install", map[string]any{"component": "valkey"}); job.Status != store.JobDone {
		t.Fatalf("install valkey: %s %s", job.Status, job.Error)
	}
}

// installedPackages lists what the panel asked the agent to install.
func (f *siteFixture) installedPackages() []string {
	var out []string
	for _, c := range f.agent.Calls() {
		if c.Path != "/v1/pkg" {
			continue
		}
		var req agent.PkgRequest
		if json.Unmarshal(c.Body, &req) == nil && req.Action == "install" {
			out = append(out, req.Packages...)
		}
	}
	return out
}

func (f *siteFixture) stackEntry(t *testing.T, name string) (installed bool, version string) {
	t.Helper()
	var list []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &list)
	for _, c := range list {
		if c.Name == name {
			return c.Installed, c.Version
		}
	}
	t.Fatalf("%s missing from the stack listing", name)
	return false, ""
}

// The server comes from the distribution — Valkey, or Redis with the same
// protocol where there is no Valkey — and its shared instance, which would
// listen for every account without a password, stays off.
func TestValkeyInstall(t *testing.T) {
	f := newSiteFixture(t)
	f.installValkey(t)
	if pk := f.installedPackages(); !slices.Contains(pk, "valkey-server") {
		t.Fatalf("valkey-server not installed: %v", pk)
	}
	if act := f.agent.UnitAction("valkey-server.service"); act != "disable" {
		t.Fatalf("the packaged instance must be switched off: %q", act)
	}
	if ok, v := f.stackEntry(t, "valkey"); !ok || v != "1.0-test" {
		t.Fatalf("stack listing: installed=%v version=%q", ok, v)
	}

	g := newSiteFixture(t)
	g.agent.MissingPackages = map[string]bool{"valkey-server": true}
	g.installValkey(t)
	if pk := g.installedPackages(); !slices.Contains(pk, "redis-server") || slices.Contains(pk, "valkey-server") {
		t.Fatalf("without valkey in the repositories redis-server goes in: %v", pk)
	}
	if ok, v := g.stackEntry(t, "valkey"); !ok || v != "redis 1.0-test" {
		t.Fatalf("stack listing names the engine: installed=%v version=%q", ok, v)
	}
	var st apitypes.ValkeyStatus
	g.call(http.MethodPut, "/users/alex/valkey/cache", map[string]any{}, http.StatusCreated, &st)
	if unit, _ := g.agent.File("/etc/systemd/system/monopanel-valkey-alex-cache.service"); !strings.Contains(unit, "ExecStart=/usr/bin/redis-server --supervised systemd --daemonize no --port 0") {
		t.Fatalf("the instance runs the installed binary:\n%s", unit)
	}
}

// On EL the instances run confined as redis_t: sockets and snapshots get the
// labels of the packaged server, while the directory above the snapshots keeps
// var_lib_t — systemd may not add a StateDirectory to a redis_var_lib_t one.
func TestValkeyInstallOnELLabelsInstanceDirs(t *testing.T) {
	withOSRelease(t, "ID=almalinux\nVERSION_ID=10.1\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	f.installValkey(t)
	if pk := f.installedPackages(); !slices.Contains(pk, "valkey") || !slices.Contains(pk, "policycoreutils-python-utils") {
		t.Fatalf("packages: %v", pk)
	}
	var rules []string
	for _, tool := range f.agent.Tools() {
		if tool.Name == "semanage" {
			rules = append(rules, strings.Join(tool.Args, " "))
		}
	}
	for _, want := range []string{"-t redis_var_run_t /run/monopanel-valkey(/.*)?", "-t redis_var_lib_t /var/lib/monopanel-valkey/[^/]+(/.*)?"} {
		if !slices.ContainsFunc(rules, func(r string) bool { return strings.HasSuffix(r, want) }) {
			t.Fatalf("no rule %q: %v", want, rules)
		}
	}
	if slices.ContainsFunc(rules, func(r string) bool { return strings.HasSuffix(r, "/var/lib/monopanel-valkey(/.*)?") }) {
		t.Fatalf("the parent of the snapshots must keep var_lib_t: %v", rules)
	}
}

// An account gets a cache and a sessions instance: its own unit running as
// the user, a socket nobody else reaches, a memory limit; removing one takes
// its snapshots along, and the server stays while instances run on it.
func TestValkeyInstances(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPut, "/users/alex/valkey/cache", map[string]any{}, http.StatusUnprocessableEntity, nil)
	f.installValkey(t)

	var st apitypes.ValkeyStatus
	f.call(http.MethodPut, "/users/alex/valkey/cache", map[string]any{}, http.StatusCreated, &st)
	if st.Instance.MemoryMB != 128 || st.Socket != "/run/monopanel-valkey/alex-cache/valkey.sock" || st.Service == nil || st.Service.ActiveState != "active" {
		t.Fatalf("cache: %+v %+v", st.Instance, st.Service)
	}
	unit, _ := f.agent.File("/etc/systemd/system/monopanel-valkey-alex-cache.service")
	for _, want := range []string{"User=alex", "ExecStart=/usr/bin/valkey-server --supervised systemd --daemonize no --port 0 \\", "--unixsocket /run/monopanel-valkey/alex-cache/valkey.sock --unixsocketperm 600", "--maxmemory 128mb", "--maxmemory-policy allkeys-lru --save \"\"", "RuntimeDirectoryMode=0700", "MemoryMax=320M"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("cache unit lacks %q:\n%s", want, unit)
		}
	}
	if act := f.agent.UnitAction("monopanel-valkey-alex-cache.service"); act != "restart" {
		t.Fatalf("cache not started: %q", act)
	}

	f.call(http.MethodPut, "/users/alex/valkey/sessions", map[string]any{"memory_mb": 32}, http.StatusCreated, &st)
	if unit, _ := f.agent.File("/etc/systemd/system/monopanel-valkey-alex-sessions.service"); !strings.Contains(unit, "--maxmemory 32mb") || !strings.Contains(unit, "--maxmemory-policy volatile-lru --save 60 1") {
		t.Fatalf("sessions unit:\n%s", unit)
	}
	f.call(http.MethodPut, "/users/alex/valkey/cache", map[string]any{"memory_mb": 256}, http.StatusOK, &st)
	if unit, _ := f.agent.File("/etc/systemd/system/monopanel-valkey-alex-cache.service"); st.Instance.MemoryMB != 256 || !strings.Contains(unit, "--maxmemory 256mb") {
		t.Fatalf("memory change: %d\n%s", st.Instance.MemoryMB, unit)
	}
	f.call(http.MethodPut, "/users/alex/valkey/queue", map[string]any{}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPut, "/users/alex/valkey/cache", map[string]any{"memory_mb": 1}, http.StatusUnprocessableEntity, nil)

	var list apitypes.ValkeyList
	f.call(http.MethodGet, "/users/alex/valkey", nil, http.StatusOK, &list)
	if list.Engine != "valkey" || len(list.Instances) != 2 {
		t.Fatalf("list: %+v", list)
	}
	f.call(http.MethodPost, "/users/alex/valkey/cache/restart", nil, http.StatusOK, &st)

	if job := f.stackJob(t, http.MethodDelete, "/stack/valkey", nil); job.Status != store.JobFailed || !strings.Contains(job.Error, "instances still run") {
		t.Fatalf("the server must stay while instances run on it: %s %s", job.Status, job.Error)
	}

	f.call(http.MethodDelete, "/users/alex/valkey/cache", nil, http.StatusNoContent, nil)
	removed := f.agent.Removed()
	if !slices.Contains(removed, "/etc/systemd/system/monopanel-valkey-alex-cache.service") || !slices.Contains(removed, "/var/lib/monopanel-valkey/alex-cache") {
		t.Fatalf("the unit and the snapshots must go: %v", removed)
	}
	if act := f.agent.UnitAction("monopanel-valkey-alex-cache.service"); act != "disable" {
		t.Fatalf("cache not stopped: %q", act)
	}
	f.call(http.MethodDelete, "/users/alex/valkey/cache", nil, http.StatusNotFound, nil)
}

// A site can keep PHP sessions in the account's sessions instance: only with
// that instance and the redis extension of its PHP, and neither can go away
// while the site depends on them.
func TestSiteSessionsInValkey(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	patch := func(where string, status int) {
		t.Helper()
		var res apitypes.SiteWithJob
		f.call(http.MethodPatch, "/sites/example.com", map[string]any{"session_store": where}, status, &res)
		if status == http.StatusAccepted {
			if job := f.waitJob(res.JobID); job.Status != store.JobDone {
				t.Fatalf("apply: %s %s", job.Status, job.Error)
			}
		}
	}
	patch("valkey", http.StatusUnprocessableEntity) // no sessions instance

	f.installValkey(t)
	f.call(http.MethodPut, "/users/alex/valkey/sessions", map[string]any{}, http.StatusCreated, nil)
	f.agent.DirEntries["/etc/php/8.4/mods-available"] = []string{"opcache.ini", "redis.ini"}
	f.agent.DirEntries["/etc/php/8.4/fpm/conf.d"] = []string{"10-opcache.ini"}
	patch("valkey", http.StatusUnprocessableEntity) // redis extension off

	f.agent.DirEntries["/etc/php/8.4/fpm/conf.d"] = []string{"10-opcache.ini", "20-redis.ini"}
	patch("valkey", http.StatusAccepted)
	pool, _ := f.agent.File("/etc/php/8.4/fpm/pool.d/example.com.conf")
	if !strings.Contains(pool, "php_admin_value[session.save_handler] = redis") || !strings.Contains(pool, `php_admin_value[session.save_path] = "unix:///run/monopanel-valkey/alex-sessions/valkey.sock"`) {
		t.Fatalf("pool does not send sessions to valkey:\n%s", pool)
	}
	// Sessions in Valkey are locked like files are: without it parallel AJAX
	// requests of one visitor overwrite each other's changes.
	for _, want := range []string{"php_value[redis.session.locking_enabled] = 1", "php_value[redis.session.lock_retries] = -1", "php_value[redis.session.lock_wait_time] = 20000"} {
		if !strings.Contains(pool, want) {
			t.Fatalf("pool lacks %q:\n%s", want, pool)
		}
	}
	// The keys can be tuned per site like any other php.ini value.
	var upd apitypes.SiteWithJob
	f.call(http.MethodPatch, "/sites/example.com", map[string]any{"php_ini": map[string]string{"redis.session.lock_retries": "3000"}}, http.StatusAccepted, &upd)
	if job := f.waitJob(upd.JobID); job.Status != store.JobDone {
		t.Fatalf("apply: %s %s", job.Status, job.Error)
	}
	if pool, _ := f.agent.File("/etc/php/8.4/fpm/pool.d/example.com.conf"); !strings.Contains(pool, "php_value[redis.session.lock_retries] = 3000") || strings.Contains(pool, "lock_retries] = -1") {
		t.Fatalf("site override of the lock retries:\n%s", pool)
	}
	var php apitypes.SitePHP
	f.call(http.MethodGet, "/sites/example.com/php", nil, http.StatusOK, &php)
	sources := map[string]string{}
	for _, v := range php.Values {
		sources[v.Key] = v.Source
	}
	if sources["redis.session.locking_enabled"] != "default" || sources["redis.session.lock_retries"] != "site" {
		t.Fatalf("sources: %v", sources)
	}

	f.call(http.MethodPost, "/php/versions/8.4/extensions", map[string]any{"name": "redis", "enabled": false}, http.StatusConflict, nil)
	f.call(http.MethodDelete, "/users/alex/valkey/sessions", nil, http.StatusConflict, nil)

	patch("files", http.StatusAccepted)
	if pool, _ := f.agent.File("/etc/php/8.4/fpm/pool.d/example.com.conf"); !strings.Contains(pool, "php_admin_value[session.save_path] = /var/www/alex/data/tmp/sess") || strings.Contains(pool, "save_handler") || strings.Contains(pool, "locking_enabled") {
		t.Fatalf("back to files:\n%s", pool)
	}

	// A site turning into a proxy has no PHP sessions left to keep, and
	// asking a proxy for them is a mistake.
	patch("valkey", http.StatusAccepted)
	var res apitypes.SiteWithJob
	f.call(http.MethodPatch, "/sites/example.com", map[string]any{"mode": "proxy", "backend": "http://127.0.0.1:3000", "session_store": "valkey"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPatch, "/sites/example.com", map[string]any{"mode": "proxy", "backend": "http://127.0.0.1:3000"}, http.StatusAccepted, &res)
	if res.Site.SessionStore != "" {
		t.Fatalf("a proxy keeps no sessions: %q", res.Site.SessionStore)
	}
	f.waitJob(res.JobID)
	f.call(http.MethodDelete, "/users/alex/valkey/sessions", nil, http.StatusNoContent, nil)
}

// Deleting an account removes its instances with it.
func TestValkeyGoesWithTheUser(t *testing.T) {
	f := newSiteFixture(t)
	f.installValkey(t)
	f.call(http.MethodPut, "/users/alex/valkey/cache", map[string]any{}, http.StatusCreated, nil)
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/users/alex", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("delete user: %s %s", job.Status, job.Error)
	}
	if !slices.Contains(f.agent.Removed(), "/etc/systemd/system/monopanel-valkey-alex-cache.service") {
		t.Fatalf("the instance must go with the user: %v", f.agent.Removed())
	}
	if list, _ := f.db.ListValkey(f.ctx, 0); len(list) != 0 {
		t.Fatalf("rows left: %+v", list)
	}
}

// An account moving to another panel brings its instances as settings — the
// data stays behind — and a site keeps its sessions in Valkey only where the
// receiver can serve them.
func TestMigrationBringsValkeyInstances(t *testing.T) {
	source := newSiteFixture(t)
	source.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	source.installValkey(t)
	source.call(http.MethodPut, "/users/alex/valkey/cache", map[string]any{"memory_mb": 256}, http.StatusCreated, nil)
	source.call(http.MethodPut, "/users/alex/valkey/sessions", map[string]any{}, http.StatusCreated, nil)
	redisOn := func(f *siteFixture) {
		f.agent.DirEntries["/etc/php/8.4/mods-available"] = []string{"opcache.ini", "redis.ini"}
		f.agent.DirEntries["/etc/php/8.4/fpm/conf.d"] = []string{"10-opcache.ini", "20-redis.ini"}
	}
	redisOn(source)
	var res apitypes.SiteWithJob
	source.call(http.MethodPatch, "/sites/shop.example.com", map[string]any{"session_store": "valkey"}, http.StatusAccepted, &res)
	source.waitJob(res.JobID)

	migrate := func(target *siteFixture) (*store.Site, []*store.ValkeyInstance) {
		t.Helper()
		var grant struct {
			Token string `json:"token"`
		}
		source.call(http.MethodPost, "/migrate/grant", map[string]any{"scope": "user:alex", "hours": 1}, http.StatusCreated, &grant)
		var ref struct {
			JobID int64 `json:"job_id"`
		}
		target.call(http.MethodPost, "/migrate/run", map[string]any{"source": source.ts.URL, "token": grant.Token, "scope": "user:alex", "as": "alex2", "insecure": true}, http.StatusAccepted, &ref)
		if job := target.waitJob(ref.JobID); job.Status != store.JobDone {
			t.Fatalf("migration: %s %s", job.Status, job.Error)
		}
		u, err := target.db.GetUserByLogin(target.ctx, "alex2")
		if err != nil {
			t.Fatal(err)
		}
		site, err := target.db.GetSiteByDomain(target.ctx, "shop.example.com")
		if err != nil {
			t.Fatal(err)
		}
		list, _ := target.db.ListValkey(target.ctx, u.ID)
		return site, list
	}

	full := newSiteFixture(t)
	full.installValkey(t)
	redisOn(full)
	site, list := migrate(full)
	if site.SessionStore != store.SessionStoreValkey || len(list) != 2 || list[0].Purpose != store.ValkeyCache || list[0].MemoryMB != 256 {
		t.Fatalf("with Valkey and redis: sessions=%q instances=%+v", site.SessionStore, list)
	}
	if unit, _ := full.agent.File("/etc/systemd/system/monopanel-valkey-alex2-sessions.service"); !strings.Contains(unit, "User=alex2") {
		t.Fatalf("the instance must run as the new login:\n%s", unit)
	}

	noRedis := newSiteFixture(t)
	noRedis.installValkey(t)
	if site, list := migrate(noRedis); site.SessionStore != "" || len(list) != 2 {
		t.Fatalf("without the redis extension: sessions=%q instances=%d", site.SessionStore, len(list))
	}

	noValkey := newSiteFixture(t)
	if site, list := migrate(noValkey); site.SessionStore != "" || len(list) != 0 {
		t.Fatalf("without Valkey: sessions=%q instances=%d", site.SessionStore, len(list))
	}
}
