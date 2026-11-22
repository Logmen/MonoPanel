package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// alex's uid in the fixture is 1500: his Apache listens on 127.100.5.220:81.
const (
	alexApache     = "monopanel-apache@alex.service"
	alexApacheAddr = "127.100.5.220:81"
	alexApacheDir  = "/etc/apache2/monopanel/accounts/alex"
	alexApacheConf = alexApacheDir + "/httpd.conf"
)

var alexApacheCheck = []string{"/usr/sbin/apache2", "-t", "-f", alexApacheConf}

func newApacheFixture(t *testing.T) *siteFixture {
	t.Helper()
	f := newSiteFixture(t)
	// An Apache's check is the file the panel wrote (apacheDemands).
	f.agent.StatWritten = true
	for k, v := range map[string]string{settingApache: "installed", settingApacheInstances: "yes", settingApacheBackends: "yes"} {
		if err := f.db.SetSetting(f.ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// callAt is the position of the first agent call at or after from that
// match takes, or -1.
func (f *siteFixture) callAt(from int, match func(c agenttest.Call) bool) int {
	calls := f.agent.Calls()
	for i := max(from, 0); i < len(calls); i++ {
		if match(calls[i]) {
			return i
		}
	}
	return -1
}

func applyOf(c agenttest.Call) (agent.ApplyConfigSetRequest, bool) {
	var req agent.ApplyConfigSetRequest
	ok := c.Path == "/v1/config/apply" && json.Unmarshal(c.Body, &req) == nil
	return req, ok
}

func removeOf(c agenttest.Call) (agent.RemovePathsRequest, bool) {
	var req agent.RemovePathsRequest
	ok := c.Path == "/v1/paths/remove" && json.Unmarshal(c.Body, &req) == nil
	return req, ok
}

// reloads tells a config/apply or paths/remove that reloads the unit.
func reloads(unit string) func(c agenttest.Call) bool {
	return func(c agenttest.Call) bool {
		if req, ok := applyOf(c); ok {
			return slices.Contains(req.Reload, unit) || slices.Contains(req.Restart, unit)
		}
		if req, ok := removeOf(c); ok {
			return slices.Contains(req.Reload, unit)
		}
		return false
	}
}

// writes tells a config/apply that writes the file.
func writes(file string) func(c agenttest.Call) bool {
	return func(c agenttest.Call) bool {
		req, ok := applyOf(c)
		return ok && slices.ContainsFunc(req.Files, func(f agent.FileSpec) bool { return f.Path == file })
	}
}

// service tells a /v1/service call of the unit with the action.
func service(unit, action string) func(c agenttest.Call) bool {
	return func(c agenttest.Call) bool {
		var req agent.ServiceRequest
		return c.Path == "/v1/service" && json.Unmarshal(c.Body, &req) == nil && req.Unit == unit && req.Action == action
	}
}

func (f *siteFixture) updateSite(domain string, body map[string]any) *store.Site {
	f.t.Helper()
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPatch, "/sites/"+domain, body, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		f.t.Fatalf("%s: %s %s", domain, job.Status, job.Error)
	}
	site, err := f.db.GetSiteByDomain(f.ctx, domain)
	if err != nil {
		f.t.Fatal(err)
	}
	return site
}

func (f *siteFixture) siteAction(domain, action string) {
	f.t.Helper()
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/"+domain+"/"+action, nil, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		f.t.Fatalf("%s %s: %s %s", action, domain, job.Status, job.Error)
	}
}

func (f *siteFixture) deleteSite(domain string) {
	f.t.Helper()
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/sites/"+domain, nil, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		f.t.Fatalf("delete %s: %s %s", domain, job.Status, job.Error)
	}
}

// The first site of an account in Apache mode brings up the account's own
// Apache — children as the account, its own address on port 81, checked
// with apache2 -t on its httpd.conf, enabled — before nginx is sent to that
// address; nothing goes to the shared Apache of 0.8.19.
func TestApacheModeSiteRunsOnItsAccountsApache(t *testing.T) {
	f := newApacheFixture(t)
	site := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	if site.Status != store.SiteActive {
		t.Fatalf("site: %s %s", site.Status, site.LastError)
	}
	main, ok := f.agent.File(alexApacheConf)
	if !ok {
		t.Fatalf("no httpd.conf of alex's Apache; files: %v", f.agent.Files())
	}
	for _, want := range []string{"\nUser alex\nGroup alex\n", "\nListen " + alexApacheAddr + "\n", "\nPidFile /run/monopanel-apache/alex/httpd.pid\n",
		"\nMutex file:/run/monopanel-apache/alex default\n", "\nErrorLog /var/log/monopanel/sites/alex/apache.error.log\n",
		"\n<Directory /var/www/alex>\n", "\nIncludeOptional " + alexApacheDir + "/sites/*.conf\n"} {
		if !strings.Contains(main, want) {
			t.Errorf("httpd.conf lacks %q:\n%s", want, main)
		}
	}
	vhost, _ := f.agent.File(alexApacheDir + "/sites/a.example.com.conf")
	if !strings.Contains(vhost, "<VirtualHost "+alexApacheAddr+">") || !strings.Contains(vhost, "IncludeOptional "+alexApacheDir+"/sites/a.example.com.d/*.conf") || strings.Contains(vhost, "Options=") {
		t.Errorf("vhost:\n%s", vhost)
	}
	nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/a.example.com.conf")
	if strings.Count(nginx, "proxy_pass http://"+alexApacheAddr+";") != 2 {
		t.Errorf("nginx does not proxy to alex's Apache:\n%s", nginx)
	}
	if active, enabled := f.agent.InstanceState(alexApache); !active || !enabled {
		t.Fatalf("alex's Apache: active %v, enabled %v", active, enabled)
	}
	// The run directory, the check, the start, then nginx.
	dir := f.callAt(0, func(c agenttest.Call) bool {
		return c.Path == "/v1/dirs/ensure" && strings.Contains(string(c.Body), `"/run/monopanel-apache/alex"`)
	})
	start := f.callAt(0, reloads(alexApache))
	nginxReload := f.callAt(0, func(c agenttest.Call) bool {
		return writes("/etc/nginx/monopanel/sites/a.example.com.conf")(c) && reloads("nginx.service")(c)
	})
	if dir < 0 || start < 0 || nginxReload < 0 || !(dir < start && start < nginxReload) {
		t.Fatalf("run dir at %d, Apache started at %d, nginx at %d", dir, start, nginxReload)
	}
	req, _ := applyOf(f.agent.Calls()[start])
	if !slices.ContainsFunc(req.Validate, func(v []string) bool { return slices.Equal(v, alexApacheCheck) }) || !req.Force {
		t.Fatalf("the start is not checked with apache2 -t: %+v", req)
	}
	if f.callAt(0, service(alexApache, "enable")) < 0 {
		t.Fatal("alex's Apache is not enabled")
	}
	for _, c := range f.agent.Calls() {
		if body := string(c.Body); strings.Contains(body, "127.0.0.1:8080") || strings.Contains(body, "apache2.service") || strings.Contains(body, "apache2ctl") {
			t.Fatalf("%s goes to the shared Apache: %s", c.Path, body)
		}
	}
	// Its own logs are made before it starts.
	if !slices.Contains(f.agent.SiteLogs("alex"), "apache.error.log") {
		t.Fatalf("logs: %v", f.agent.SiteLogs("alex"))
	}
}

// A second site reloads the running Apache, a site leaving Apache mode goes
// from it after nginx moved it, a suspended last site stops and disables it
// (its custom directives stay), and the account's last site removed takes
// the whole directory.
func TestApacheInstanceFollowsTheAccountsSites(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	from := len(f.agent.Calls())
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	if f.callAt(from, reloads(alexApache)) < 0 || f.callAt(from, service(alexApache, "enable")) >= 0 {
		t.Fatal("the second site must reload the running Apache, not enable it again")
	}
	if _, ok := f.agent.File(alexApacheDir + "/sites/b.example.com.conf"); !ok {
		t.Fatal("no vhost of the second site")
	}

	// a leaves Apache mode: nginx first, then the vhost goes, checked, and
	// the Apache reloads.
	from = len(f.agent.Calls())
	f.updateSite("a.example.com", map[string]any{"mode": "fpm"})
	nginxAt := f.callAt(from, writes("/etc/nginx/monopanel/sites/a.example.com.conf"))
	gone := f.callAt(from, func(c agenttest.Call) bool {
		req, ok := removeOf(c)
		return ok && slices.Contains(req.Paths, alexApacheDir+"/sites/a.example.com.conf") && slices.Contains(req.Reload, alexApache) &&
			slices.ContainsFunc(req.Validate, func(v []string) bool { return slices.Equal(v, alexApacheCheck) })
	})
	if nginxAt < 0 || gone < 0 || gone < nginxAt {
		t.Fatalf("nginx at %d, the vhost removed at %d", nginxAt, gone)
	}
	if nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/a.example.com.conf"); strings.Contains(nginx, alexApacheAddr) {
		t.Fatalf("an fpm site still proxies to Apache:\n%s", nginx)
	}
	if active, _ := f.agent.InstanceState(alexApache); !active {
		t.Fatal("b keeps alex's Apache running")
	}

	// b suspended: no site left on it.
	f.siteAction("b.example.com", "suspend")
	if active, enabled := f.agent.InstanceState(alexApache); active || enabled {
		t.Fatalf("with its only site suspended alex's Apache runs (%v) or starts at boot (%v)", active, enabled)
	}
	if _, ok := f.agent.File(alexApacheConf); ok {
		t.Fatal("httpd.conf of a retired Apache stays")
	}
	if _, ok := f.agent.File(alexApacheDir + "/sites/b.example.com.d/README"); !ok {
		t.Fatal("the custom directives of the suspended site went")
	}
	if slices.Contains(f.agent.Removed(), alexApacheDir) {
		t.Fatal("the directory went while a suspended site is in Apache mode")
	}

	// back again, then removed: everything of b goes; a, in fpm mode now,
	// keeps its custom directives for a return to Apache mode.
	f.siteAction("b.example.com", "unsuspend")
	if active, enabled := f.agent.InstanceState(alexApache); !active || !enabled {
		t.Fatalf("unsuspended: active %v, enabled %v", active, enabled)
	}
	f.deleteSite("b.example.com")
	if active, enabled := f.agent.InstanceState(alexApache); active || enabled {
		t.Fatalf("after its last site alex's Apache: active %v, enabled %v", active, enabled)
	}
	for _, p := range f.agent.Files() {
		if strings.HasPrefix(p, alexApacheDir+"/") && !strings.HasPrefix(p, alexApacheDir+"/sites/a.example.com.d/") {
			t.Fatalf("%s stays", p)
		}
	}
	// The account's last site goes: the whole directory with it.
	f.deleteSite("a.example.com")
	if !slices.Contains(f.agent.Removed(), alexApacheDir) {
		t.Fatalf("alex's Apache directory stays: %v", f.agent.Removed())
	}
	for _, p := range f.agent.Files() {
		if strings.HasPrefix(p, alexApacheDir+"/") {
			t.Fatalf("%s stays", p)
		}
	}
}

// An account goes only after its Apache, whose children run as it, stopped.
func TestAccountRemovalStopsItsApacheFirst(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetUserStatus(f.ctx, alex.ID, store.UserDeleting); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.jobs.Enqueue(f.ctx, "user.delete", userDeletePayload{UserID: alex.ID, Login: "alex"})
	if err != nil {
		t.Fatal(err)
	}
	if j := f.waitJob(job.ID); j.Status != store.JobDone {
		t.Fatalf("delete: %s %s", j.Status, j.Error)
	}
	stop := f.callAt(0, service(alexApache, "stop"))
	removed := f.callAt(0, func(c agenttest.Call) bool { return c.Path == "/v1/user/remove" })
	if stop < 0 || removed < 0 || stop > removed {
		t.Fatalf("Apache stopped at %d, the account removed at %d", stop, removed)
	}
	if active, enabled := f.agent.InstanceState(alexApache); active || enabled {
		t.Fatalf("alex's Apache: active %v, enabled %v", active, enabled)
	}
}

// installApache installs the package and switches its shared server off:
// apache2.service stopped and disabled, www-data out of the web group, no
// module, configuration or site of it enabled and no shared configuration
// written; a host where accounts may bind port 81 is refused.
func TestInstallApacheLeavesTheSharedServerOff(t *testing.T) {
	f := newSiteFixture(t)
	var ref apitypes.JobRef
	f.call(http.MethodPost, "/stack/install", map[string]any{"component": "apache"}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	acts := f.agent.ServiceActions()
	if !slices.Contains(acts, "apache2.service stop") || !slices.Contains(acts, "apache2.service disable") || slices.ContainsFunc(acts, func(a string) bool {
		return strings.HasPrefix(a, "apache2.service ") && a != "apache2.service stop" && a != "apache2.service disable"
	}) {
		t.Fatalf("service actions: %v", acts)
	}
	if ctl := f.agent.ApacheCtls(); len(ctl) != 0 {
		t.Fatalf("a2* run on a fresh install: %+v", ctl)
	}
	for _, c := range f.agent.Calls() {
		if req, ok := applyOf(c); ok {
			for _, file := range req.Files {
				if strings.HasPrefix(file.Path, "/etc/apache2/") {
					t.Fatalf("a shared configuration written: %s", file.Path)
				}
			}
		}
	}
	leaves := f.callAt(0, func(c agenttest.Call) bool {
		var req agent.EnsureUnixUserRequest
		return c.Path == "/v1/user/ensure" && json.Unmarshal(c.Body, &req) == nil && req.Login == "www-data" && slices.Equal(req.RemoveGroups, []string{f.s.cfg.WebGroup}) && len(req.Groups) == 0
	})
	joins := f.callAt(0, func(c agenttest.Call) bool {
		var req agent.EnsureUnixUserRequest
		return c.Path == "/v1/user/ensure" && json.Unmarshal(c.Body, &req) == nil && req.Login == "www-data" && len(req.Groups) > 0
	})
	if leaves < 0 || joins >= 0 {
		t.Fatalf("www-data leaves the web group at %d, joins a group at %d", leaves, joins)
	}
	for k, want := range map[string]string{settingApache: "installed", settingApacheInstances: "yes"} {
		if v, _ := f.db.GetSetting(f.ctx, k); v != want {
			t.Errorf("%s = %q", k, v)
		}
	}
	// The stack shows how many accounts' Apaches run.
	var comps []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &comps)
	for _, c := range comps {
		if c.Name == "apache" && (!c.Installed || c.Service != nil || c.Instances == nil || c.Instances.Total != 0) {
			t.Fatalf("apache: %+v", c)
		}
	}

	g := newSiteFixture(t)
	g.s.SetUnprivilegedPortStart(func() int { return 80 })
	g.call(http.MethodPost, "/stack/install", map[string]any{"component": "apache"}, http.StatusAccepted, &ref)
	if job := g.waitJob(ref.JobID); job.Status != store.JobFailed || !strings.Contains(job.Error, "ip_unprivileged_port_start is 80") {
		t.Fatalf("install with port 81 open to everybody: %s %s", job.Status, job.Error)
	}
	if g.agent.Called("/v1/pkg") {
		t.Fatal("the package was installed anyway")
	}
}

// waitJobOfType waits for the last job of a type.
func (f *siteFixture) waitJobOfType(typ string) *store.Job {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := f.db.ListJobs(f.ctx, 50, "")
		if err != nil {
			f.t.Fatal(err)
		}
		for _, j := range jobs {
			if j.Type == typ {
				return f.waitJob(j.ID)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("no %s job", typ)
	return nil
}

// sharedLayout makes the fixture a panel of 0.8.19 with the shared Apache:
// the marker unset, the include enabled, an administrator's custom
// directives next to a site's shared virtual host. The files lie in a
// directory of the test that only the fake agent reads, with the real
// agent's code (it is /etc/apache2/monopanel/sites, root's, on a host): the
// API reads none of them itself. It returns that directory.
func (f *siteFixture) sharedLayout(custom map[string]string) string {
	f.t.Helper()
	if err := f.db.SetSetting(f.ctx, settingApacheInstances, ""); err != nil {
		f.t.Fatal(err)
	}
	disk := filepath.Join(f.t.TempDir(), "sites")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.agent.ApacheLegacySites = disk
	for domain, content := range custom {
		d := filepath.Join(disk, domain+".d")
		if err := os.MkdirAll(d, 0o755); err != nil {
			f.t.Fatal(err)
		}
		for name, c := range map[string]string{"custom.conf": content, "README": "Custom Apache directives for " + domain + " (inside <VirtualHost>).\n"} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(c), 0o644); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	f.agent.Stat["/etc/apache2/conf-enabled/monopanel.conf"] = false
	return disk
}

// An upgraded panel moves its sites in Apache mode off the shared Apache
// once: site by site, each account's Apache gets the site and answers before
// nginx is sent there, the custom directives come along, and only then does
// apache2.service stop, the shared configuration go and www-data leave the
// web group.
func TestUpgradeMovesSitesOffTheSharedApache(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.createSite(map[string]any{"domain": "c.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "s.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.siteAction("s.example.com", "suspend")
	f.agent.Reset()
	f.sharedLayout(map[string]string{"a.example.com": "Header set X-Moved yes\n", "s.example.com": "Header set X-Suspended yes\n"})

	f.s.refreshApacheInstances(f.ctx)
	if job := f.waitJobOfType("apache.migrate"); job.Status != store.JobDone {
		t.Fatalf("migrate: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	custom, ok := f.agent.File(alexApacheDir + "/sites/a.example.com.d/custom.conf")
	if !ok || custom != "Header set X-Moved yes\n" {
		t.Fatalf("custom directives: %q %v", custom, ok)
	}
	if _, ok := f.agent.File(alexApacheDir + "/sites/a.example.com.d/README"); !ok {
		t.Fatal("no README next to the custom directives")
	}
	// They come in the very set that gives the site to the Apache, checked
	// with it: the site never runs there without them.
	if at := f.callAt(0, writes(alexApacheDir+"/sites/a.example.com.conf")); at < 0 || !writes(alexApacheDir+"/sites/a.example.com.d/custom.conf")(f.agent.Calls()[at]) {
		t.Fatal("the custom directives of a are not in the set of its virtual host")
	}
	// The suspended site's wait in its directory; nothing serves it.
	if c, ok := f.agent.File(alexApacheDir + "/sites/s.example.com.d/custom.conf"); !ok || c != "Header set X-Suspended yes\n" {
		t.Fatalf("custom directives of the suspended site: %q %v", c, ok)
	}
	if _, ok := f.agent.File(alexApacheDir + "/sites/s.example.com.conf"); ok {
		t.Fatal("the suspended site got a virtual host")
	}
	lastNginx := -1
	for _, d := range []string{"a.example.com", "b.example.com"} {
		nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/" + d + ".conf")
		if !strings.Contains(nginx, "proxy_pass http://"+alexApacheAddr+";") {
			t.Fatalf("%s not on alex's Apache:\n%s", d, nginx)
		}
		at := f.callAt(0, func(c agenttest.Call) bool {
			return writes("/etc/nginx/monopanel/sites/"+d+".conf")(c) && reloads("nginx.service")(c)
		})
		vhost := f.callAt(0, writes(alexApacheDir+"/sites/"+d+".conf"))
		if at < 0 || vhost < 0 || vhost > at {
			t.Fatalf("%s: Apache's vhost at %d, nginx at %d", d, vhost, at)
		}
		lastNginx = max(lastNginx, at)
	}
	stop := f.callAt(0, service("apache2.service", "stop"))
	disable := f.callAt(0, service("apache2.service", "disable"))
	if stop < 0 || disable < 0 || stop < lastNginx {
		t.Fatalf("the shared Apache stops at %d (disabled at %d) — before nginx moved the last site (%d)?", stop, disable, lastNginx)
	}
	if ctl := f.agent.ApacheCtls(); len(ctl) != 1 || ctl[0].Action != "disconf" || ctl[0].Name != "monopanel" {
		t.Fatalf("a2* calls: %+v", ctl)
	}
	for _, p := range []string{filepath.Join(f.s.apacheLegacy, "httpd.conf"), filepath.Join(f.s.apacheLegacy, "sites"), "/etc/apache2/conf-available/monopanel.conf"} {
		if !slices.Contains(f.agent.Removed(), p) {
			t.Errorf("%s stays: %v", p, f.agent.Removed())
		}
	}
	leaves := f.callAt(0, func(c agenttest.Call) bool {
		var req agent.EnsureUnixUserRequest
		return c.Path == "/v1/user/ensure" && json.Unmarshal(c.Body, &req) == nil && req.Login == "www-data" && slices.Equal(req.RemoveGroups, []string{f.s.cfg.WebGroup})
	})
	if leaves < stop {
		t.Fatalf("www-data leaves the web group at %d, the shared Apache stops at %d", leaves, stop)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingApacheInstances); v != "yes" {
		t.Fatalf("marker %q", v)
	}
	rot, _ := f.agent.File(siteLogrotatePath)
	if strings.Contains(rot, "/run/apache2/apache2.pid") || !strings.Contains(rot, `kill -USR1 "$(cat /run/monopanel-apache/alex/httpd.pid)"`) {
		t.Fatalf("rotation after the move:\n%s", rot)
	}
	// Once only.
	f.agent.Reset()
	f.s.refreshApacheInstances(f.ctx)
	if f.agent.Called("/v1/service") || f.agent.Called("/v1/config/apply") {
		t.Fatal("the move ran again")
	}
}

// A site that cannot move keeps the shared Apache running for it: nothing
// of the shared layout is touched, the job names the site, and the next
// start tries again.
func TestUpgradeKeepsTheSharedApacheForASiteThatCannotMove(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	// b's PHP branch went away since: it has nothing to run on.
	b.PHPVersion = "7.4"
	if err := f.db.UpdateSite(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	f.sharedLayout(nil)

	f.s.refreshApacheInstances(f.ctx)
	job := f.waitJobOfType("apache.migrate")
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "b.example.com") || !strings.Contains(job.Error, "keeps serving") {
		t.Fatalf("migrate: %s %s", job.Status, job.Error)
	}
	for _, a := range f.agent.ServiceActions() {
		if strings.HasPrefix(a, "apache2.service ") {
			t.Fatalf("the shared Apache was touched: %v", f.agent.ServiceActions())
		}
	}
	if len(f.agent.ApacheCtls()) != 0 || slices.Contains(f.agent.Removed(), filepath.Join(f.s.apacheLegacy, "sites")) {
		t.Fatal("the shared layout was taken apart")
	}
	if v, _ := f.db.GetSetting(f.ctx, settingApacheInstances); v == "yes" {
		t.Fatal("the move counts as done")
	}
	// a moved all the same; nginx still sends b where it did.
	if nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/a.example.com.conf"); !strings.Contains(nginx, alexApacheAddr) {
		t.Fatalf("a did not move:\n%s", nginx)
	}
	if _, ok := f.agent.File("/etc/nginx/monopanel/sites/b.example.com.conf"); ok {
		t.Fatal("b's nginx changed")
	}
}

// The services and the doctor know the accounts' Apaches, not the shared one.
func TestAccountsApachesAreTheServices(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	if units := f.s.knownUnits(); !slices.Contains(units, alexApache) || slices.Contains(units, "apache2.service") {
		t.Fatalf("units: %v", units)
	}
	var doc apitypes.Doctor
	f.call(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doc)
	names := map[string]string{}
	for _, c := range doc.Checks {
		names[c.Name] = c.Status
	}
	if names["service "+alexApache] != "ok" || names["config "+alexApache] != "ok" {
		t.Fatalf("doctor: %+v", doc.Checks)
	}
	// The fake says apache2.service runs: the doctor warns of it.
	if names["service apache2.service"] != "warn" {
		t.Fatalf("doctor on the shared Apache: %+v", doc.Checks)
	}
	var comps []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &comps)
	for _, c := range comps {
		if c.Name == "apache" && (c.Instances == nil || c.Instances.Total != 1 || c.Instances.Running != 1) {
			t.Fatalf("apache: %+v", c)
		}
	}
}

// Whatever job gives a site to its account's Apache first — a site.apply
// queued by the upgrade's other moves races the move of the sites — it
// brings the site's custom directives of the shared Apache along; one the
// agent refuses keeps the site where it was.
func TestSiteApplyCarriesTheSharedCustomDirectives(t *testing.T) {
	f := newApacheFixture(t)
	f.sharedLayout(map[string]string{"a.example.com": "Redirect 301 /old /new\n"})
	site := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	if site.Status != store.SiteActive {
		t.Fatalf("site: %s %s", site.Status, site.LastError)
	}
	if c, ok := f.agent.File(alexApacheDir + "/sites/a.example.com.d/custom.conf"); !ok || c != "Redirect 301 /old /new\n" {
		t.Fatalf("custom directives: %q %v", c, ok)
	}
	// Carried over once: what is there now stays.
	f.agent.Stat[alexApacheDir+"/sites/a.example.com.d/custom.conf"] = false
	from := len(f.agent.Calls())
	f.siteAction("a.example.com", "apply")
	if at := f.callAt(from, writes(alexApacheDir+"/sites/a.example.com.d/custom.conf")); at >= 0 {
		t.Fatal("the custom directives were carried over again")
	}

	// The agent refuses one: the site stays where it was, nginx untouched.
	g := newApacheFixture(t)
	g.sharedLayout(map[string]string{"b.example.com": "Include /etc/apache2/conf-available/extra.conf\n"})
	g.agent.Fail["/v1/config/apply"] = "Include is not allowed"
	g.agent.FailStatus = map[string]int{"/v1/config/apply": http.StatusForbidden}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	g.call(http.MethodPost, "/sites", map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"}, http.StatusAccepted, &out)
	job := g.waitJob(out.JobID)
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "custom directives of b.example.com") {
		t.Fatalf("site.apply: %s %s", job.Status, job.Error)
	}
	if _, ok := g.agent.File("/etc/nginx/monopanel/sites/b.example.com.conf"); ok {
		t.Fatal("nginx was sent to an Apache without the site's directives")
	}
}

// A shared folder of a site in Apache mode: another account writes there, so
// the site's Apache reads no .htaccess of it and runs no PHP from it. nginx's
// 403 alone leaves both open — a .htaccess in the folder that rewrites x.foo
// to x.php, or a request that goes straight to the account's address.
func TestApacheSharedFolderRunsNoGuestCode(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	var created struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/users", map[string]any{"login": "guest", "password": "guest-password-1", "role": "user"}, http.StatusAccepted, &created)
	f.waitJob(created.JobID)
	var st apitypes.ShareStatus
	f.call(http.MethodPost, "/users/guest/shares", map[string]any{"site": "shop.example.com", "path": "upload/kaspy", "no_php": true}, http.StatusCreated, &st)
	if job := f.waitJob(st.JobID); job.Status != store.JobDone {
		t.Fatalf("site apply: %s %s", job.Status, job.Error)
	}
	vhostPath := alexApacheDir + "/sites/shop.example.com.conf"
	vhost, _ := f.agent.File(vhostPath)
	block := "    <Directory /var/www/alex/data/www/shop.example.com/upload/kaspy>\n        AllowOverride None\n        Options +SymLinksIfOwnerMatch\n" +
		"        <FilesMatch \"\\.(?:php|phtml|phar)$\">\n            Require all denied\n        </FilesMatch>\n    </Directory>\n"
	if !strings.Contains(vhost, block) {
		t.Fatalf("the vhost lets the guest's files run as alex:\n%s", vhost)
	}
	var gone apitypes.ShareStatus
	f.call(http.MethodDelete, "/users/guest/shares/kaspy", nil, http.StatusOK, &gone)
	if job := f.waitJob(gone.JobID); job.Status != store.JobDone {
		t.Fatalf("site apply after removal: %s %s", job.Status, job.Error)
	}
	if vhost, _ := f.agent.File(vhostPath); strings.Contains(vhost, "upload/kaspy") {
		t.Fatalf("the folder is no longer shared, its denial stays:\n%s", vhost)
	}
}

// A site's custom Apache directives are the administrator's and go only with
// the site: a site that leaves Apache mode as its account's last finds them
// again when it comes back (as with the shared Apache of 0.8.19), and a site
// that is removed takes them along, also while a suspended site keeps the
// account's directory.
func TestCustomDirectivesStayWithTheSite(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	custom := func(domain string) string { return alexApacheDir + "/sites/" + domain + ".d/custom.conf" }
	// What an administrator puts there.
	if _, err := f.s.agent.ApplyConfigSet(f.ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: custom("a.example.com"), Content: "Header set X-Custom a\n", Mode: 0o644}}}); err != nil {
		t.Fatal(err)
	}
	f.updateSite("a.example.com", map[string]any{"mode": "fpm"})
	if active, enabled := f.agent.InstanceState(alexApache); active || enabled {
		t.Fatalf("alex's Apache without a site: active %v, enabled %v", active, enabled)
	}
	for _, p := range []string{alexApacheConf, alexApacheDir + "/sites/a.example.com.conf"} {
		if _, ok := f.agent.File(p); ok {
			t.Fatalf("%s stays with no site in Apache mode", p)
		}
	}
	if c, ok := f.agent.File(custom("a.example.com")); !ok || c != "Header set X-Custom a\n" {
		t.Fatalf("the custom directives of a site that left Apache mode went: %q %v", c, ok)
	}
	f.updateSite("a.example.com", map[string]any{"mode": "apache"})
	if _, ok := f.agent.File(custom("a.example.com")); !ok {
		t.Fatal("back in Apache mode without its custom directives")
	}
	if active, _ := f.agent.InstanceState(alexApache); !active {
		t.Fatal("alex's Apache does not run again")
	}

	// b is suspended, a removed: a's directives go, b's stay.
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	if _, err := f.s.agent.ApplyConfigSet(f.ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: custom("b.example.com"), Content: "Header set X-Custom b\n", Mode: 0o644}}}); err != nil {
		t.Fatal(err)
	}
	f.siteAction("b.example.com", "suspend")
	f.deleteSite("a.example.com")
	for _, p := range f.agent.Files() {
		if strings.HasPrefix(p, alexApacheDir+"/sites/a.example.com") {
			t.Fatalf("%s of a removed site stays", p)
		}
	}
	if _, ok := f.agent.File(custom("b.example.com")); !ok {
		t.Fatal("the suspended site's custom directives went")
	}
	if _, ok := f.agent.File(alexApacheConf); ok {
		t.Fatal("httpd.conf stays with no site to serve")
	}
	// The account's last site goes: the whole directory with it.
	f.deleteSite("b.example.com")
	if !slices.Contains(f.agent.Removed(), alexApacheDir) {
		t.Fatalf("alex's Apache directory stays: %v", f.agent.Removed())
	}
}

// The upgrade keeps the custom Apache directives of sites the shared Apache
// does not serve now — a site that left Apache mode kept them in 0.8.19 —
// in the site's directory of its account's Apache, for a return to Apache
// mode. One that cannot go there holds up nothing: it stays where it was,
// and a return to Apache mode brings it along (or names it).
func TestUpgradeKeepsTheCustomDirectivesOfOtherSites(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.createSite(map[string]any{"domain": "c.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "d.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.agent.Reset()
	disk := f.sharedLayout(map[string]string{"c.example.com": "Header set X-Fpm c\n", "d.example.com": "Header set X-Fpm d\n"})
	unreadable := os.Geteuid() != 0
	if unreadable {
		if err := os.Chmod(filepath.Join(disk, "d.example.com.d", "custom.conf"), 0); err != nil {
			t.Fatal(err)
		}
	}

	f.s.refreshApacheInstances(f.ctx)
	job := f.waitJobOfType("apache.migrate")
	if job.Status != store.JobDone {
		t.Fatalf("migrate: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if c, ok := f.agent.File(alexApacheDir + "/sites/c.example.com.d/custom.conf"); !ok || c != "Header set X-Fpm c\n" {
		t.Fatalf("the custom directives of a site in fpm mode: %q %v", c, ok)
	}
	if _, ok := f.agent.File(alexApacheDir + "/sites/c.example.com.conf"); ok {
		t.Fatal("a site in fpm mode got a virtual host")
	}
	legacy := filepath.Join(f.s.apacheLegacy, "sites")
	if !slices.Contains(f.agent.Removed(), filepath.Join(legacy, "c.example.com.d")) || !slices.Contains(f.agent.Removed(), filepath.Join(f.s.apacheLegacy, "httpd.conf")) {
		t.Fatalf("the shared configuration stays: %v", f.agent.Removed())
	}
	if unreadable {
		if slices.Contains(f.agent.Removed(), legacy) || slices.Contains(f.agent.Removed(), filepath.Join(legacy, "d.example.com.d")) {
			t.Fatalf("the directives that could not move went: %v", f.agent.Removed())
		}
		if !strings.Contains(job.Log, "d.example.com") {
			t.Fatalf("the job does not name the site whose directives stay:\n%s", job.Log)
		}
	}
	if v, _ := f.db.GetSetting(f.ctx, settingApacheInstances); v != "yes" {
		t.Fatalf("marker %q", v)
	}
}

// The jobs of two sites of one account run side by side, and the account's
// Apache takes one of them at a time: the site leaving Apache mode as its
// last one does not stop the Apache and take its httpd.conf away under the
// site coming to Apache mode meanwhile, which nginx then sends there.
func TestApacheOfOneAccountTakesOneSiteAtATime(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.agent.SetBefore(func(path string, body []byte) {
		var req agent.ServiceRequest
		if path == "/v1/service" && json.Unmarshal(body, &req) == nil && req.Unit == alexApache && req.Action == "stop" {
			once.Do(func() { close(held); <-release })
		}
	})
	// Whatever happens, nothing stays held back once the test is over.
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free)
	patch := func(domain string, body map[string]any) int64 {
		var out struct {
			JobID int64 `json:"job_id"`
		}
		f.call(http.MethodPatch, "/sites/"+domain, body, http.StatusAccepted, &out)
		return out.JobID
	}
	// a leaves Apache mode, the account's last site there: its job is held
	// back as it stops alex's Apache.
	jobA := patch("a.example.com", map[string]any{"mode": "fpm"})
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("a's job never stopped alex's Apache")
	}
	// Meanwhile b comes to Apache mode; its job gets as far as it can.
	jobB := patch("b.example.com", map[string]any{"mode": "apache"})
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if j, err := f.db.GetJob(f.ctx, jobB); err == nil && (j.Status == store.JobDone || j.Status == store.JobFailed) {
			break
		}
	}
	free()
	for _, id := range []int64{jobA, jobB} {
		if j := f.waitJob(id); j.Status != store.JobDone {
			t.Fatalf("job %d: %s %s", id, j.Status, j.Error)
		}
	}
	if nginx, _ := f.agent.File("/etc/nginx/monopanel/sites/b.example.com.conf"); !strings.Contains(nginx, "proxy_pass http://"+alexApacheAddr+";") {
		t.Fatalf("b is not on alex's Apache:\n%s", nginx)
	}
	if active, enabled := f.agent.InstanceState(alexApache); !active || !enabled {
		t.Fatalf("nginx sends b to alex's Apache, which is active %v, enabled %v", active, enabled)
	}
	for _, p := range []string{alexApacheConf, alexApacheDir + "/sites/b.example.com.conf"} {
		if _, ok := f.agent.File(p); !ok {
			t.Fatalf("%s is gone under b", p)
		}
	}
}

// unmovedApacheSite is a site of 0.8.19 in Apache mode of a new account
// bob that the move has not reached: the shared apache2.service serves it,
// bob has no Apache of his own yet (no httpd.conf), and the move of the
// upgrade has not finished.
func (f *siteFixture) unmovedApacheSite(domain string) *store.Site {
	f.t.Helper()
	uid := 1501
	bob := &store.User{Login: "bob", Role: store.RoleUser, Status: store.UserActive, UnixUID: &uid, UnixGID: &uid, Home: "/var/www/bob"}
	if err := f.db.CreateUser(f.ctx, bob); err != nil {
		f.t.Fatal(err)
	}
	site := &store.Site{UserID: bob.ID, Domain: domain, Mode: store.ModeApache, PHPVersion: "8.4", Docroot: "/var/www/bob/data/www/" + domain, SSL: "none", Status: store.SiteActive}
	if err := f.db.CreateSite(f.ctx, site); err != nil {
		f.t.Fatal(err)
	}
	moves, _ := json.Marshal(siteMoves{Logs: f.s.siteLogsRoot(), Sites: map[int64]*siteMove{site.ID: {Domain: domain}}})
	for k, v := range map[string]string{settingSiteMoves: string(moves), settingApacheInstances: ""} {
		if err := f.db.SetSetting(f.ctx, k, v); err != nil {
			f.t.Fatal(err)
		}
	}
	return site
}

// The stack's Apache row says what the doctor says while the move of an
// upgrade has not finished: an account none of whose sites moved has no
// Apache to count (no "1 of 2 running"), the sites that wait are counted,
// and the row carries the shared apache2.service that serves them — its
// state, down when it is, and a replaced binary, which the row's upgrade
// restarts (mp stack upgrade apache, as the doctor's line says). Once the
// move finished, the shared Apache is no part of the row.
func TestStackRowDuringTheApacheMove(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.unmovedApacheSite("c.example.com")
	row := func() apitypes.StackComponent {
		t.Helper()
		var comps []apitypes.StackComponent
		f.call(http.MethodGet, "/stack", nil, http.StatusOK, &comps)
		return component(t, comps, "apache")
	}
	c := row()
	if in := c.Instances; in == nil || in.Total != 1 || in.Running != 1 || in.Waiting != 1 || in.Replaced != 0 {
		t.Fatalf("the Apaches during the move: %+v", c.Instances)
	}
	if c.Service == nil || c.Service.Unit != "apache2.service" || c.Service.ActiveState != "active" || c.UpgradeComponent() != "apache" {
		t.Fatalf("the shared Apache during the move: %+v", c.Service)
	}
	f.agent.SetBinaryReplaced("apache2.service")
	if c := row(); c.Service == nil || !c.Service.BinaryReplaced {
		t.Fatalf("the shared Apache on a replaced binary: %+v", c.Service)
	}
	f.agent.ActiveState = map[string]string{"apache2.service": "failed"}
	if c := row(); c.Service == nil || c.Service.ActiveState != "failed" || c.Instances.Waiting != 1 {
		t.Fatalf("the shared Apache down while a site waits: %+v %+v", c.Service, c.Instances)
	}
	// The move done: the accounts' Apaches alone.
	if err := f.db.SetSetting(f.ctx, settingApacheInstances, "yes"); err != nil {
		t.Fatal(err)
	}
	f.agent.ActiveState = map[string]string{"apache2.service": "active"}
	if c := row(); c.Service != nil || c.Instances == nil || c.Instances.Waiting != 0 || c.Instances.Total != 2 {
		t.Fatalf("after the move: %+v %+v", c.Service, c.Instances)
	}
}

// While the move of an upgrade has not finished, the doctor judges the
// Apaches by where the sites are: an account none of whose sites moved yet
// has no Apache to check — no failed service, no failed apache2 -t —, one
// warning names the sites that wait, and the shared apache2.service that
// still serves them must run: down, it is a failure, whether enabled or
// not, never the advice to disable it nor an all-clear.
func TestDoctorDuringTheApacheMove(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.unmovedApacheSite("c.example.com")
	checks := func() map[string]apitypes.Check {
		t.Helper()
		out := map[string]apitypes.Check{}
		for _, c := range f.s.doctor(f.ctx).Checks {
			out[c.Name] = c
		}
		return out
	}
	const bobApache = "monopanel-apache@bob.service"
	got := checks()
	for _, name := range []string{"service " + bobApache, "config " + bobApache} {
		if c, ok := got[name]; ok {
			t.Errorf("bob's Apache, which no site of his has reached, is judged: %+v", c)
		}
	}
	if got["service "+alexApache].Status != "ok" || got["config "+alexApache].Status != "ok" {
		t.Errorf("alex's Apache, which serves a moved site: %+v %+v", got["service "+alexApache], got["config "+alexApache])
	}
	if c := got["apache move"]; c.Status != "warn" || c.Detail != "waits for the move: the shared apache2.service still serves c.example.com; the Apache of bob starts with the first of its account's sites that moves, and is checked from then on" {
		t.Errorf("the sites that wait: %+v", c)
	}
	if c := got["service apache2.service"]; c.Status != "warn" || !strings.Contains(c.Detail, "the move of the upgrade: not tried yet") {
		t.Errorf("the shared Apache running: %+v", c)
	}
	for _, state := range []string{"enabled", "disabled"} {
		f.agent.ActiveState = map[string]string{"apache2.service": "failed"}
		f.agent.UnitFileState = map[string]string{"apache2.service": state}
		c := checks()["service apache2.service"]
		if c.Status != "fail" || !strings.Contains(c.Detail, "but it still serves c.example.com") || !strings.Contains(c.Detail, "systemctl start apache2.service") || strings.Contains(c.Detail, "disable") {
			t.Errorf("the shared Apache down, %s: %+v", state, c)
		}
	}
	// The move done: off and disabled is how it should be.
	if err := f.db.SetSetting(f.ctx, settingApacheInstances, "yes"); err != nil {
		t.Fatal(err)
	}
	f.agent.ActiveState = map[string]string{"apache2.service": "inactive"}
	got = checks()
	if c := got["service apache2.service"]; c.Status != "ok" {
		t.Errorf("after the move: %+v", c)
	}
	if _, ok := got["apache move"]; ok {
		t.Error("the move is done, yet sites wait for it")
	}
}
