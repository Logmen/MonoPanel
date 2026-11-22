package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/apitypes"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// upgradeTrace is what the panel had the agent do from call from on, as the
// upgrade job does it: package actions but queries, checks with what they
// reload or restart, service actions but status.
func upgradeTrace(f *siteFixture, from int) []string {
	var out []string
	for _, c := range f.agent.Calls()[from:] {
		switch c.Path {
		case "/v1/pkg":
			var req agent.PkgRequest
			if json.Unmarshal(c.Body, &req) == nil && req.Action != "query" {
				out = append(out, strings.TrimSpace("pkg "+req.Action+" "+strings.Join(req.Packages, " ")))
			}
		case "/v1/config/apply":
			req, _ := applyOf(c)
			line := "check"
			for _, v := range req.Validate {
				line += " " + v[0]
			}
			for _, u := range req.Reload {
				line += " reload " + u
			}
			for _, u := range req.Restart {
				line += " restart " + u
			}
			out = append(out, line)
		case "/v1/service":
			var req agent.ServiceRequest
			if json.Unmarshal(c.Body, &req) == nil && req.Action != "status" && req.Action != "" {
				out = append(out, "service "+req.Unit+" "+req.Action)
			}
		}
	}
	return out
}

// withoutCandidates drops the candidates line, whose package list is long.
func withoutCandidates(trace []string) []string {
	return slices.DeleteFunc(slices.Clone(trace), func(l string) bool { return strings.HasPrefix(l, "pkg candidates ") })
}

func component(t *testing.T, comps []apitypes.StackComponent, name string) apitypes.StackComponent {
	t.Helper()
	for _, c := range comps {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s in the stack: %+v", name, comps)
	return apitypes.StackComponent{}
}

// What the repositories offer shows on the stack list; mp stack upgrade
// installs it — nginx, then the PHP branches —, checks the configuration
// with the new binaries, reloads nginx (nginx.org's package upgraded the
// running binary itself) and restarts php-fpm; the database server is no
// part of it. The record and the PHP branch's version follow.
func TestStackUpgradeUpgradesAndReloads(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.PackageVersions = map[string]string{"nginx": "1.30.5-1~trixie", "php8.4-fpm": "8.4.27-1", "php8.4-common": "8.4.27-1", "percona-server-server": "8.4.6-6-1.trixie"}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{
		"nginx":                 {Version: "1.30.7-1~trixie", Origin: "nginx.org"},
		"php8.4-fpm":            {Version: "8.4.28-1", Origin: "packages.sury.org"},
		"php8.4-common":         {Version: "8.4.28-1", Origin: "packages.sury.org"},
		"percona-server-server": {Version: "8.4.7-7-1.trixie", Origin: "repo.percona.com"},
	})
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "percona", Version: "8.4.6-6", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}

	var comps []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &comps)
	if up := component(t, comps, "nginx").Upgrade; up == nil || up.Component != "nginx" || up.From != "1.30.5-1~trixie" || up.To != "1.30.7-1~trixie" || up.Origin != "nginx.org" {
		t.Fatalf("nginx: %+v", up)
	}
	php := component(t, comps, "php-8.4")
	if up := php.Upgrade; up == nil || up.Component != "php" || up.To != "8.4.28-1" || len(up.Packages) != 2 {
		t.Fatalf("php-8.4: %+v", up)
	}
	if php.Version != "8.4.27-1" {
		t.Errorf("the PHP row shows %q, not the installed package", php.Version)
	}
	if up := component(t, comps, "percona").Upgrade; up == nil || up.Component != "db" {
		t.Fatalf("percona: %+v", up)
	}

	from := len(f.agent.Calls())
	job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{})
	if job.Status != store.JobDone {
		t.Fatalf("upgrade: %s %s", job.Status, job.Error)
	}
	trace := upgradeTrace(f, from)
	if len(trace) < 2 || trace[0] != "pkg update-index" || !strings.HasPrefix(trace[1], "pkg candidates ") {
		t.Fatalf("the index first, then what it offers: %q", trace)
	}
	// Each configuration is checked with the version that runs first: the
	// packages restart nginx and php-fpm themselves on it.
	want := []string{
		"pkg update-index",
		"check /usr/sbin/nginx",
		"pkg upgrade nginx",
		"check /usr/sbin/nginx reload nginx.service",
		"check /usr/sbin/php-fpm8.4",
		"pkg upgrade php8.4-fpm php8.4-common",
		"check /usr/sbin/php-fpm8.4 restart php8.4-fpm.service",
	}
	if got := withoutCandidates(trace); !slices.Equal(got, want) {
		t.Fatalf("upgrade:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v := f.agent.InstalledVersion("percona-server-server"); v != "8.4.6-6-1.trixie" {
		t.Fatalf("the database server was upgraded with all: %s", v)
	}
	full, _ := f.db.GetJob(f.ctx, job.ID)
	for _, line := range []string{"nginx: nginx 1.30.5-1~trixie → 1.30.7-1~trixie from nginx.org", "php-8.4: php8.4-fpm 8.4.27-1 → 8.4.28-1 from packages.sury.org (2 packages)",
		"the configuration is valid with the new nginx; nginx.service reloaded", "php-fpm -t passed with the new PHP 8.4; restarted"} {
		if !strings.Contains(full.Log, line) {
			t.Errorf("the job's log lacks %q:\n%s", line, full.Log)
		}
	}
	last := f.s.lastUpgrade(f.ctx)
	if last == nil || last.JobID != job.ID || last.Error != "" || last.Scheduled || !slices.Contains(last.Upgraded, "nginx 1.30.5-1~trixie → 1.30.7-1~trixie") || len(last.Upgraded) != 3 {
		t.Fatalf("the record: %+v", last)
	}
	if row, _ := f.db.GetPHPVersion(f.ctx, "8.4"); row.PackageVersion != "8.4.28-1" {
		t.Errorf("PHP 8.4 is recorded as %q", row.PackageVersion)
	}
	var after []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &after)
	if component(t, after, "nginx").Upgrade != nil || component(t, after, "php-8.4").Upgrade != nil {
		t.Fatalf("updates still wait after the upgrade: %+v", after)
	}

	// The database server only by name: its packages, its check, its restart
	// (the package did not restart it), the version it reports recorded.
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "mysql" && strings.Contains(req.Stdin, "VERSION()") {
			return &agent.ToolResponse{Output: "8.4.7-7\n"}
		}
		return nil
	}
	from = len(f.agent.Calls())
	if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "db"}); job.Status != store.JobDone {
		t.Fatalf("db: %s %s", job.Status, job.Error)
	}
	want = []string{"pkg update-index", "check /usr/sbin/mysqld", "pkg upgrade percona-server-server", "check /usr/sbin/mysqld restart mysql.service"}
	if got := withoutCandidates(upgradeTrace(f, from)); !slices.Equal(got, want) {
		t.Fatalf("db:\n%s", strings.Join(got, "\n"))
	}
	if inst, _ := f.db.GetDBInstance(f.ctx); inst.Version != "8.4.7-7" || inst.Status != store.DBReady {
		t.Fatalf("the server's record: %+v", inst)
	}
}

// A service still on the binary an upgrade replaced is restarted, not
// reloaded: the reload would keep the old code running. That holds for one
// the distribution's upgrades left there too, with nothing to install.
func TestStackUpgradeRestartsAReplacedBinary(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"nginx": {Version: "1.30.7-1~trixie"}})
	f.agent.SetBinaryReplaced("nginx.service")
	from := len(f.agent.Calls())
	if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "nginx"}); job.Status != store.JobDone {
		t.Fatalf("upgrade: %s %s", job.Status, job.Error)
	}
	if got, want := withoutCandidates(upgradeTrace(f, from)), []string{"pkg update-index", "check /usr/sbin/nginx", "pkg upgrade nginx", "check /usr/sbin/nginx restart nginx.service"}; !slices.Equal(got, want) {
		t.Fatalf("nginx on its old binary:\n%s", strings.Join(got, "\n"))
	}
	if f.agent.RunsReplacedBinary("nginx.service") {
		t.Fatal("nginx still runs the old binary")
	}

	f.agent.SetBinaryReplaced("php8.4-fpm.service")
	from = len(f.agent.Calls())
	if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "php"}); job.Status != store.JobDone {
		t.Fatalf("php: %s %s", job.Status, job.Error)
	}
	if got, want := withoutCandidates(upgradeTrace(f, from)), []string{"pkg update-index", "check /usr/sbin/php-fpm8.4 restart php8.4-fpm.service"}; !slices.Equal(got, want) {
		t.Fatalf("php-fpm on its old binary, nothing to install:\n%s", strings.Join(got, "\n"))
	}
	// And nothing at all when nothing changed.
	from = len(f.agent.Calls())
	f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{})
	if got := withoutCandidates(upgradeTrace(f, from)); !slices.Equal(got, []string{"pkg update-index"}) {
		t.Fatalf("nothing to do:\n%s", strings.Join(got, "\n"))
	}
}

// A configuration the new binary refuses keeps the service as it runs: no
// reload, no restart, nothing rolled back — and the job fails loudly: its
// error, the doctor until an upgrade succeeds (and the job webhooks).
func TestStackUpgradeFailedCheckReloadsNothing(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"nginx": {Version: "1.30.7-1~trixie"}, "php8.4-fpm": {Version: "8.4.28-1"}})
	f.agent.NewBinaryRefuses = map[string]agenttest.Refusal{
		"/usr/sbin/nginx":      {Package: "nginx", Output: `nginx: [emerg] unknown directive "http3_hq"`},
		"/usr/sbin/php-fpm8.4": {Package: "php8.4-fpm", Output: "ERROR: [pool shop] unknown entry 'pm.max_spawn_rate'"},
	}
	job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "nginx was not reloaded and runs on as it was") || !strings.Contains(job.Error, "php-fpm was not restarted") {
		t.Fatalf("upgrade with a refused configuration: %s %s", job.Status, job.Error)
	}
	for _, unit := range []string{"nginx.service", "php8.4-fpm.service"} {
		if act := f.agent.UnitAction(unit); act != "" {
			t.Errorf("%s: %s after a failed check", unit, act)
		}
	}
	if v := f.agent.InstalledVersion("nginx"); v != "1.30.7-1~trixie" {
		t.Errorf("the package was rolled back: %s", v)
	}
	if got := f.doctorCheck("stack upgrade"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "nothing was rolled back") || !strings.Contains(got, "unknown directive") {
		t.Fatalf("doctor after a failed upgrade: %q", got)
	}
	f.agent.NewBinaryRefuses = nil
	if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{}); job.Status != store.JobDone {
		t.Fatalf("the next upgrade: %s %s", job.Status, job.Error)
	}
	if got := f.doctorCheck("stack upgrade"); got != "" {
		t.Fatalf("doctor after a good upgrade: %q", got)
	}

	// The package manager failing has the panel touch no service.
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"nginx": {Version: "1.30.8-1~trixie"}})
	f.agent.UpgradeFails = map[string]string{"nginx": "E: Sub-process /usr/bin/dpkg returned an error code (1)"}
	from := len(f.agent.Calls())
	if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "nginx"}); job.Status != store.JobFailed || !strings.Contains(job.Error, "the panel restarted no service") {
		t.Fatalf("a failed apt-get: %s %s", job.Status, job.Error)
	}
	for _, l := range upgradeTrace(f, from) {
		if strings.Contains(l, " reload ") || strings.Contains(l, " restart ") {
			t.Fatalf("a service was touched after the package manager failed: %s", l)
		}
	}
}

// A configuration that fails its check already with the version that runs
// (an edit nobody reloaded, a certificate gone) keeps the upgrade off that
// service: its package restarts it itself — nginx.org's binary upgrade,
// Debian's php-fpm, the database server's — and would take it down on that
// configuration. The rest of the upgrade goes on; the job fails and says so.
func TestStackUpgradeChecksBeforeUpgrading(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.PackageVersions = map[string]string{"nginx": "1.30.5-1~trixie", "php8.4-fpm": "8.4.27-1"}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"nginx": {Version: "1.30.7-1~trixie"}, "php8.4-fpm": {Version: "8.4.28-1"}})
	f.agent.CheckFails = map[string]string{"/usr/sbin/nginx": `nginx: [emerg] cannot load certificate "/etc/ssl/gone.pem"`}
	from := len(f.agent.Calls())
	job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "fails with the version that runs now") || !strings.Contains(job.Error, "cannot load certificate") || !strings.Contains(job.Error, "mp stack upgrade nginx") {
		t.Fatalf("an nginx whose configuration fails now: %s %s", job.Status, job.Error)
	}
	want := []string{"pkg update-index", "check /usr/sbin/nginx", "check /usr/sbin/php-fpm8.4", "pkg upgrade php8.4-fpm", "check /usr/sbin/php-fpm8.4 restart php8.4-fpm.service"}
	if got := withoutCandidates(upgradeTrace(f, from)); !slices.Equal(got, want) {
		t.Fatalf("nginx left out, PHP upgraded:\n%s", strings.Join(got, "\n"))
	}
	if v := f.agent.InstalledVersion("nginx"); v != "1.30.5-1~trixie" {
		t.Fatalf("nginx was upgraded on a configuration that fails: %s", v)
	}
	if got := f.doctorCheck("stack upgrade"); !strings.Contains(got, "fails with the version that runs now") {
		t.Fatalf("doctor: %q", got)
	}

	// A PHP branch and the database server likewise.
	f.agent.CheckFails = map[string]string{"/usr/sbin/php-fpm8.4": "ERROR: Unable to include /etc/php/8.4/fpm/pool.d/gone.conf", "/usr/sbin/mysqld": "[ERROR] unknown variable 'query_cache_size=0'"}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"php8.4-fpm": {Version: "8.4.29-1"}, "percona-server-server": {Version: "8.4.7-7-1.trixie"}})
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "percona", Version: "8.4.6-6", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}
	for _, comp := range []string{"php", "db"} {
		before := len(f.agent.Upgraded())
		if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": comp}); job.Status != store.JobFailed || !strings.Contains(job.Error, "fails with the version that runs now") {
			t.Fatalf("%s: %s %s", comp, job.Status, job.Error)
		}
		if len(f.agent.Upgraded()) != before {
			t.Fatalf("%s was upgraded on a configuration that fails: %v", comp, f.agent.Upgraded())
		}
	}
	if f.agent.UnitAction("mysql.service") != "" || f.agent.UnitAction("php8.4-fpm.service") != "restart" {
		t.Fatalf("services touched: mysql %q, php-fpm %q", f.agent.UnitAction("mysql.service"), f.agent.UnitAction("php8.4-fpm.service"))
	}
}

// A package whose own restart took its service down on a configuration the
// new binary refuses (Debian's php-fpm goes on when that restart fails): the
// job says the service is down, not that it runs on.
func TestStackUpgradeSaysAServiceIsDown(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"php8.4-fpm": {Version: "8.4.28-1"}})
	f.agent.NewBinaryRefuses = map[string]agenttest.Refusal{"/usr/sbin/php-fpm8.4": {Package: "php8.4-fpm", Output: "ERROR: [pool shop] unknown entry"}}
	f.agent.SetBefore(func(p string, body []byte) {
		var req agent.PkgRequest
		if p == "/v1/pkg" && json.Unmarshal(body, &req) == nil && req.Action == "upgrade" {
			f.agent.SetActiveState("php8.4-fpm.service", "failed")
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "php"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "php8.4-fpm.service is failed since its package restarted it") || strings.Contains(job.Error, "runs on as it was") {
		t.Fatalf("php-fpm down after its package's restart: %s %s", job.Status, job.Error)
	}
}

// The accounts' Apaches: restarted when they still run the replaced
// apache2 (a graceful restart keeps the parent, and its binary), a graceful
// restart otherwise — each after apache2 -t on its httpd.conf.
func TestStackUpgradeApacheInstances(t *testing.T) {
	f := newApacheFixture(t)
	if site := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"}); site.Status != store.SiteActive {
		t.Fatalf("site: %s %s", site.Status, site.LastError)
	}
	f.agent.PackageVersions = map[string]string{"apache2": "2.4.65-2", "apache2-bin": "2.4.65-2", "apache2-data": "2.4.65-2"}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"apache2": {Version: "2.4.65-3"}, "apache2-bin": {Version: "2.4.65-3"}})
	f.agent.SetBinaryReplaced(alexApache)
	// The stack list counts the Apaches on the old binary (the clients offer
	// the upgrade for them even with nothing to install).
	var comps []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &comps)
	if in := component(t, comps, "apache").Instances; in == nil || in.Replaced != 1 || in.Running != 1 {
		t.Fatalf("apache instances: %+v", in)
	}
	from := len(f.agent.Calls())
	if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "apache"}); job.Status != store.JobDone {
		t.Fatalf("apache: %s %s", job.Status, job.Error)
	}
	if got, want := withoutCandidates(upgradeTrace(f, from)), []string{"pkg update-index", "pkg upgrade apache2 apache2-bin", "check /usr/sbin/apache2 restart " + alexApache}; !slices.Equal(got, want) {
		t.Fatalf("apache:\n%s", strings.Join(got, "\n"))
	}
	at := f.callAt(from, reloads(alexApache))
	if req, _ := applyOf(f.agent.Calls()[at]); !slices.ContainsFunc(req.Validate, func(v []string) bool { return slices.Equal(v, alexApacheCheck) }) {
		t.Fatalf("not checked with apache2 -t: %+v", req)
	}
	if f.agent.RunsReplacedBinary(alexApache) {
		t.Fatal("alex's Apache still runs the old binary")
	}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"apache2-data": {Version: "2.4.65-3"}})
	from = len(f.agent.Calls())
	f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "apache"})
	if got, want := withoutCandidates(upgradeTrace(f, from)), []string{"pkg update-index", "pkg upgrade apache2-data", "check /usr/sbin/apache2 reload " + alexApache}; !slices.Equal(got, want) {
		t.Fatalf("apache, its binary in place:\n%s", strings.Join(got, "\n"))
	}
}

// What the panel did not install is no upgrade, and the request takes the
// components it knows.
func TestStackUpgradeRefusals(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPost, "/stack/upgrade", map[string]any{"component": "apache"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/stack/upgrade", map[string]any{"component": "db"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/stack/upgrade", map[string]any{"component": "postfix"}, http.StatusUnprocessableEntity, nil)
	f.agent.MissingPackages = map[string]bool{"nginx": true}
	f.call(http.MethodPost, "/stack/upgrade", map[string]any{"component": "nginx"}, http.StatusUnprocessableEntity, nil)
	if f.agent.Called("/v1/config/apply") || len(f.agent.Upgraded()) > 0 {
		t.Fatal("a refused upgrade did something")
	}
}

// The daily upgrade is on by default, at 04:00 server time, once a day;
// switched off it queues nothing, moved it runs at the new hour.
func TestAutoUpgradeSchedule(t *testing.T) {
	f := newSiteFixture(t)
	var st apitypes.StackAutoUpgrade
	f.call(http.MethodGet, "/stack/auto-upgrade", nil, http.StatusOK, &st)
	if !st.Enabled || st.Hour != 4 || st.NextRun == nil || st.Timezone == "" || st.Last != nil {
		t.Fatalf("defaults: %+v", st)
	}
	at := func(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, time.Local) }
	if j := f.s.scheduledStackUpgrade(f.ctx, at(22, 3, 55)); j != nil {
		t.Fatalf("queued before its hour: %+v", j)
	}
	j := f.s.scheduledStackUpgrade(f.ctx, at(22, 4, 5))
	if j == nil || j.Type != "stack.upgrade" || j.RequestedBy != "scheduler" || j.LockKey != "stack" {
		t.Fatalf("the daily upgrade: %+v", j)
	}
	var p stackUpgradePayload
	if json.Unmarshal(j.Payload, &p) != nil || p.Component != "all" || !p.Scheduled {
		t.Fatalf("payload %s", j.Payload)
	}
	if again := f.s.scheduledStackUpgrade(f.ctx, at(22, 4, 15)); again == nil || again.ID != j.ID {
		t.Fatalf("queued twice a day: %+v", again)
	}
	if done := f.waitJob(j.ID); done.Status != store.JobDone {
		t.Fatalf("the daily upgrade: %s %s", done.Status, done.Error)
	}
	if last := f.s.lastUpgrade(f.ctx); last == nil || !last.Scheduled || last.JobID != j.ID {
		t.Fatalf("the record of the daily upgrade: %+v", last)
	}
	if next := f.s.scheduledStackUpgrade(f.ctx, at(23, 4, 1)); next == nil || next.ID == j.ID {
		t.Fatalf("the next day: %+v", next)
	}

	st = apitypes.StackAutoUpgrade{}
	f.call(http.MethodPut, "/stack/auto-upgrade", map[string]any{"enabled": false}, http.StatusOK, &st)
	if st.Enabled || st.Hour != 4 || st.NextRun != nil {
		t.Fatalf("off: %+v", st)
	}
	if j := f.s.scheduledStackUpgrade(f.ctx, at(24, 4, 5)); j != nil {
		t.Fatal("queued while off")
	}
	st = apitypes.StackAutoUpgrade{}
	f.call(http.MethodPut, "/stack/auto-upgrade", map[string]any{"enabled": true, "hour": 22}, http.StatusOK, &st)
	if !st.Enabled || st.Hour != 22 {
		t.Fatalf("moved: %+v", st)
	}
	if j := f.s.scheduledStackUpgrade(f.ctx, at(25, 4, 5)); j != nil {
		t.Fatal("queued at the old hour")
	}
	if j := f.s.scheduledStackUpgrade(f.ctx, at(25, 22, 1)); j == nil {
		t.Fatal("not queued at the new hour")
	}
	f.call(http.MethodPut, "/stack/auto-upgrade", map[string]any{"hour": 24}, http.StatusUnprocessableEntity, nil)
	st = apitypes.StackAutoUpgrade{}
	f.call(http.MethodGet, "/stack/auto-upgrade", nil, http.StatusOK, &st)
	if st.Hour != 22 || !st.Enabled {
		t.Fatalf("after a refused change: %+v", st)
	}

	for _, c := range []struct {
		now  time.Time
		last *apitypes.StackUpgradeRun
		want time.Time
	}{
		{at(22, 3, 0), nil, at(22, 4, 0)},
		{at(22, 5, 0), nil, at(23, 4, 0)},
		{at(22, 4, 30), nil, at(22, 4, 0)},
		{at(22, 4, 30), &apitypes.StackUpgradeRun{Scheduled: true, At: at(22, 4, 5).UTC()}, at(23, 4, 0)},
		{at(22, 4, 30), &apitypes.StackUpgradeRun{At: at(22, 4, 5).UTC()}, at(22, 4, 0)},
	} {
		if got := nextUpgradeAt(c.now, 4, c.last); !got.Equal(c.want) {
			t.Errorf("next run at %s (last %+v): %s, want %s", c.now, c.last, got, c.want)
		}
	}
}

// The doctor names each component with an update waiting, how it gets
// installed, and that the database's restarts the server; with nothing
// waiting it says so once.
func TestDoctorUpgradeLines(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.PackageVersions = map[string]string{"nginx": "1.30.5-1~trixie", "php8.4-fpm": "8.4.27-1"}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"nginx": {Version: "1.30.7-1~trixie", Origin: "nginx.org"}, "php8.4-fpm": {Version: "8.4.28-1", Origin: "packages.sury.org"}})
	if got := f.doctorCheck("update nginx"); got != "warn: nginx 1.30.5-1~trixie → 1.30.7-1~trixie from nginx.org: mp stack upgrade nginx, or the daily upgrade at 04:00" {
		t.Errorf("nginx: %q", got)
	}
	if got := f.doctorCheck("update php-8.4"); !strings.HasPrefix(got, "warn: php8.4-fpm 8.4.27-1 → 8.4.28-1 from packages.sury.org: mp stack upgrade php") {
		t.Errorf("php: %q", got)
	}
	for _, c := range f.s.doctor(f.ctx).Checks {
		if c.Name == "update nginx" && (c.Action != "stack.upgrade" || c.Target != "nginx") {
			t.Errorf("the dashboard's button: %+v", c)
		}
	}
	if got := f.doctorCheck("updates"); got != "" {
		t.Errorf("an all-clear next to updates: %q", got)
	}
	f.call(http.MethodPut, "/stack/auto-upgrade", map[string]any{"enabled": false}, http.StatusOK, nil)
	if got := f.doctorCheck("update nginx"); !strings.HasSuffix(got, "mp stack upgrade nginx (the daily upgrade is off: mp stack auto-upgrade --on)") {
		t.Errorf("nginx, daily upgrade off: %q", got)
	}
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "mysql", Version: "8.4.6", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"mysql-community-server": {Version: "8.4.7-1debian13"}, "mysql-community-client": {Version: "8.4.7-1debian13"}})
	if got := f.doctorCheck("update mysql"); !strings.HasSuffix(got, "(2 packages): mp stack upgrade db (the database server restarts)") {
		t.Errorf("mysql: %q", got)
	}
	f.agent.SetUpgrades(nil)
	if got := f.doctorCheck("updates"); !strings.HasPrefix(got, "ok: nginx, php-8.4, mysql: nothing newer") {
		t.Errorf("nothing waiting: %q", got)
	}
	// A service still on a replaced binary is not well: the upgrade
	// restarts nginx after its check (the dashboard leads there), the
	// database server only as db; fail2ban, on a Python the distribution
	// upgraded, is no part of it.
	f.agent.SetBinaryReplaced("nginx.service")
	if got := f.doctorCheck("service nginx.service"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "an upgrade replaced") || !strings.Contains(got, "mp stack upgrade nginx restarts it") {
		t.Errorf("nginx on a replaced binary: %q", got)
	}
	if err := f.db.SetSetting(f.ctx, settingFail2ban, "installed"); err != nil {
		t.Fatal(err)
	}
	f.agent.SetBinaryReplaced("fail2ban.service", "mysql.service")
	if got := f.doctorCheck("service fail2ban.service"); !strings.HasPrefix(got, "warn: ") || !strings.HasSuffix(got, "; systemctl restart fail2ban.service") {
		t.Errorf("fail2ban on a replaced binary: %q", got)
	}
	if got := f.doctorCheck("service mysql.service"); !strings.Contains(got, "mp stack upgrade db restarts it") {
		t.Errorf("the database server on a replaced binary: %q", got)
	}
	for _, c := range f.s.doctor(f.ctx).Checks {
		switch c.Name {
		case "service nginx.service":
			if c.Action != "stack.upgrade" || c.Target != "nginx" {
				t.Errorf("nginx's line leads nowhere: %+v", c)
			}
		case "service fail2ban.service":
			if c.Action != "" {
				t.Errorf("fail2ban's line leads to the upgrade, which does not restart it: %+v", c)
			}
		}
	}
	// The stack list carries it: the row's upgrade is the restart.
	var comps []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &comps)
	if c := component(t, comps, "nginx"); c.Service == nil || !c.Service.BinaryReplaced {
		t.Errorf("the stack list hides nginx's replaced binary: %+v", c.Service)
	}
}

// catch_workers_output in any pool of an installed branch: the way of
// CVE-2026-129291 from a worker to the root master.
func TestDoctorCatchWorkersOutput(t *testing.T) {
	f := newSiteFixture(t)
	const dir = "/etc/php/8.4/fpm/pool.d"
	if got := f.doctorCheck("php-fpm pools"); got != "ok: no pool has catch_workers_output on" {
		t.Errorf("no pools: %q", got)
	}
	f.agent.DirEntries[dir] = []string{"shop.example.com.conf", "blog.example.com.conf", "www.conf", "notes.txt"}
	f.agent.ReadFile[dir+"/shop.example.com.conf"] = "[shop.example.com]\nuser = alex\ncatch_workers_output = yes\n"
	f.agent.ReadFile[dir+"/blog.example.com.conf"] = "[blog.example.com]\ncatch_workers_output = no\n"
	f.agent.ReadFile[dir+"/www.conf"] = "[www]\n;catch_workers_output = yes\n"
	f.agent.ReadFile[dir+"/notes.txt"] = "catch_workers_output = yes\n"
	got := f.doctorCheck("php-fpm pools")
	for _, want := range []string{"warn: catch_workers_output = yes in 1 pool file(s): " + dir + "/shop.example.com.conf.", "CVE-2026-129291", "8.4.28", "0.8.20", "mp site apply"} {
		if !strings.Contains(got, want) {
			t.Errorf("pools: %q lacks %q", got, want)
		}
	}
	if !f.agent.Called("/v1/dir/list") {
		t.Fatal("the pool directory was not listed")
	}
	for _, c := range []struct {
		conf string
		want []string
	}{
		{"[a]\ncatch_workers_output = yes\n[b]\ncatch_workers_output=On ; for the logs\n[c]\ncatch_workers_output = no\n", []string{"a", "b"}},
		{"[a]\n# catch_workers_output = yes\ncatch_workers_output = 'true'\n", []string{"a"}},
		{"[a]\ncatch_workers_output = off\n", nil},
	} {
		if got := catchingPools(c.conf); !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.conf, got, c.want)
		}
	}

	// A pool directory the agent cannot list is no all-clear: a pool there
	// may have it on.
	delete(f.agent.DirEntries, dir)
	f.agent.Fail["/v1/dir/list"] = "permission denied"
	if got := f.doctorCheck("php-fpm pools"); !strings.HasPrefix(got, "warn: cannot read "+dir) || !strings.Contains(got, "CVE-2026-129291") {
		t.Errorf("an unreadable pool directory: %q", got)
	}
}

// The kernel modules hosting does not need: the rule must be there, and a
// module it covers must not be loaded (the rule does not unload) nor built
// in; one the administrator's /etc file lets load is their choice.
func TestDoctorKernelModules(t *testing.T) {
	f := newSiteFixture(t)
	dir := t.TempDir()
	etc, lib := filepath.Join(dir, "etc.conf"), filepath.Join(dir, "lib.conf")
	mods, builtin := filepath.Join(dir, "modules"), filepath.Join(dir, "modules.builtin")
	f.s.SetKernelSources([]string{etc, lib}, mods, builtin)
	write := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(mods, "ext4 1060864 1 - Live 0x0000000000000000\nnf_tables 376832 1 - Live 0x0000000000000000\n")
	write(builtin, "kernel/fs/ext4/ext4.ko\n")
	check := func() string {
		c := f.s.kernelModulesCheck()
		return c.Status + ": " + c.Detail
	}

	if got := check(); !strings.HasPrefix(got, "warn: neither ") || !strings.Contains(got, "mp setup writes the rule") {
		t.Errorf("no rule: %q", got)
	}
	write(lib, osprofile.ModprobeConf)
	if got := check(); got != "ok: sctp, dccp, rds, tipc cannot load ("+lib+")" {
		t.Errorf("the package's rule: %q", got)
	}
	write(mods, "sctp 487424 4 - Live 0x0000000000000000\next4 1060864 1 - Live 0x0000000000000000\n")
	if got := check(); !strings.HasPrefix(got, "warn: sctp is loaded") || !strings.Contains(got, "rmmod sctp, or reboot") {
		t.Errorf("sctp loaded: %q", got)
	}
	// The administrator lets sctp load: /etc's file replaces the package's.
	write(etc, strings.Replace(osprofile.ModprobeConf, "install sctp /bin/false\n", "", 1))
	if got := check(); got != "ok: dccp, rds, tipc cannot load ("+etc+"); sctp may: "+etc+" allows them" {
		t.Errorf("sctp allowed: %q", got)
	}
	write(builtin, "kernel/net/tipc/tipc.ko\n")
	if got := check(); !strings.HasPrefix(got, "warn: tipc is built into the kernel") {
		t.Errorf("tipc built in: %q", got)
	}
}

// On EL the branches are Remi's: their packages, their php-fpm -t, their
// unit.
func TestStackUpgradeEL(t *testing.T) {
	withOSRelease(t, "ID=almalinux\nVERSION_ID=9.6\nID_LIKE=\"rhel centos fedora\"\n")
	f := newSiteFixture(t)
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"php84-php-fpm": {Version: "8.4.28-1.el9.remi", Origin: "remi-safe"}, "php84-php-common": {Version: "8.4.28-1.el9.remi", Origin: "remi-safe"}})
	from := len(f.agent.Calls())
	if job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{"component": "php"}); job.Status != store.JobDone {
		t.Fatalf("php on EL: %s %s", job.Status, job.Error)
	}
	want := []string{"pkg update-index", "check /opt/remi/php84/root/usr/sbin/php-fpm", "pkg upgrade php84-php-fpm php84-php-common", "check /opt/remi/php84/root/usr/sbin/php-fpm restart php84-php-fpm.service"}
	if got := withoutCandidates(upgradeTrace(f, from)); !slices.Equal(got, want) {
		t.Fatalf("php on EL:\n%s", strings.Join(got, "\n"))
	}
}

// What the administrator holds back (apt-mark hold) the upgrade leaves, and
// the doctor says that its fixes stay open.
func TestStackUpgradeLeavesHeldPackages(t *testing.T) {
	f := newSiteFixture(t)
	f.agent.PackageVersions = map[string]string{"nginx": "1.30.5-1~trixie"}
	f.agent.HeldPackages = map[string]bool{"nginx": true}
	f.agent.SetUpgrades(map[string]osprofile.Candidate{"nginx": {Version: "1.30.7-1~trixie", Origin: "nginx.org"}, "php8.4-fpm": {Version: "8.4.28-1"}})
	var comps []apitypes.StackComponent
	f.call(http.MethodGet, "/stack", nil, http.StatusOK, &comps)
	if up := component(t, comps, "nginx").Upgrade; up != nil {
		t.Fatalf("a held nginx offered for upgrade: %+v", up)
	}
	if got := f.doctorCheck("held nginx"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "nginx 1.30.5-1~trixie") || !strings.Contains(got, "apt-mark unhold nginx, then mp stack upgrade nginx") {
		t.Errorf("doctor on a held nginx: %q", got)
	}
	from := len(f.agent.Calls())
	job := f.stackJob(t, http.MethodPost, "/stack/upgrade", map[string]any{})
	if job.Status != store.JobDone {
		t.Fatalf("upgrade: %s %s", job.Status, job.Error)
	}
	for _, l := range upgradeTrace(f, from) {
		if l == "pkg upgrade nginx" || strings.Contains(l, "nginx.service") {
			t.Fatalf("the held nginx was touched: %s", l)
		}
	}
	if full, _ := f.db.GetJob(f.ctx, job.ID); !strings.Contains(full.Log, "held back by the administrator (apt-mark hold), left as they are: nginx 1.30.5-1~trixie") {
		t.Errorf("the log does not say so:\n%s", full.Log)
	}
	if v := f.agent.InstalledVersion("php8.4-fpm"); v != "8.4.28-1" {
		t.Errorf("php was not upgraded next to the held nginx: %s", v)
	}
}
