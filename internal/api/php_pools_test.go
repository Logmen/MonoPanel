package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

const (
	elRelease     = "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"
	ubuntuRelease = "ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=noble\n"
)

// The pools the distributions ship, as their packages install them.
const (
	debianStockPool = "; Start a new pool named 'www'.\n[www]\nuser = www-data\ngroup = www-data\nlisten = /run/php/php8.4-fpm.sock\nlisten.owner = www-data\nlisten.group = www-data\npm = dynamic\npm.max_children = 5\npm.start_servers = 2\npm.min_spare_servers = 1\npm.max_spare_servers = 3\n"
	remiStockPool   = "; Start a new pool named 'www'.\n[www]\nuser = apache\ngroup = apache\nlisten = /var/opt/remi/php84/run/php-fpm/www.sock\nlisten.acl_users = apache,nginx\nlisten.allowed_clients = 127.0.0.1\npm = dynamic\npm.max_children = 50\nslowlog = /var/opt/remi/php84/log/php-fpm/www-slow.log\nphp_admin_value[error_log] = /var/opt/remi/php84/log/php-fpm/www-error.log\nphp_admin_flag[log_errors] = on\n"
)

// remiStockConf is Remi's php-fpm.conf as php84-php-fpm installs it,
// shortened: the comments mention error_log and syslog too.
const remiStockConf = `;;;;;;;;;;;;;;;;;;;;;
; FPM Configuration ;
;;;;;;;;;;;;;;;;;;;;;

; All relative paths in this configuration file are relative to PHP's install
; prefix.

; Include one or more files. If glob(3) exists, it is used to include a bunch of
; files from a glob(3) pattern. This directive can be used everywhere in the
; file.
include=/etc/opt/remi/php84/php-fpm.d/*.conf

;;;;;;;;;;;;;;;;;;
; Global Options ;
;;;;;;;;;;;;;;;;;;

[global]
; Pid file
; Default Value: none
pid = /var/opt/remi/php84/run/php-fpm/php-fpm.pid

; Error log file
; If it's set to "syslog", log is sent to syslogd instead of being written
; in a local file.
; Default Value: /var/opt/remi/php84/log/php-fpm.log
error_log = /var/opt/remi/php84/log/php-fpm/error.log

; syslog_facility is used to specify what type of program is logging the
; message. This lets syslogd specify that messages from different facilities
; will be handled differently.
; Default Value: daemon
;syslog.facility = daemon

; Log level
; Possible Values: alert, error, warning, notice, debug
; Default Value: notice
;log_level = notice

; Send FPM to background. Set to 'no' to keep FPM in foreground for debugging.
; Default Value: yes
daemonize = yes

;;;;;;;;;;;;;;;;;;;;
; Pool Definitions ;
;;;;;;;;;;;;;;;;;;;;

; See /etc/opt/remi/php84/php-fpm.d/*.conf
`

// configureChecks points the agent's checks at the fixture's run directory
// for one test, so that what the API writes is checked as the agent would.
func configureChecks(t *testing.T, f *siteFixture) {
	t.Helper()
	confcheck.Configure(confcheck.Layout{RunDir: f.s.cfg.RunDir})
	t.Cleanup(func() { confcheck.Configure(confcheck.Layout{}) })
}

// applies are the config/apply requests the fake agent received.
func applies(t *testing.T, f *siteFixture) []agent.ApplyConfigSetRequest {
	t.Helper()
	var out []agent.ApplyConfigSetRequest
	for _, c := range f.agent.Calls() {
		if c.Path != "/v1/config/apply" {
			continue
		}
		var req agent.ApplyConfigSetRequest
		if err := json.Unmarshal(c.Body, &req); err != nil {
			t.Fatal(err)
		}
		out = append(out, req)
	}
	return out
}

func applyWriting(t *testing.T, f *siteFixture, p string) *agent.ApplyConfigSetRequest {
	t.Helper()
	for _, req := range applies(t, f) {
		for _, file := range req.Files {
			if file.Path == p {
				return &req
			}
		}
	}
	return nil
}

func installPHP(t *testing.T, f *siteFixture, version string) {
	t.Helper()
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": version}, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("php %s install: %s %s", version, job.Status, job.Error)
	}
}

// A branch the panel installs keeps the distribution's pool idle from the
// start, written with the rest of its configuration, checked by php-fpm -t
// and taken up by the reload. On Debian the master's log stays where it is.
func TestPHPInstallMakesTheDistributionPoolIdle(t *testing.T) {
	stubSury(t, true)
	f := newSiteFixture(t)
	configureChecks(t, f)
	installPHP(t, f, "8.3")

	const p = "/etc/php/8.3/fpm/pool.d/www.conf"
	want, _ := confcheck.IdlePool(f.s.cfg.RunDir, p)
	if got, ok := f.agent.File(p); !ok || got != want {
		t.Fatalf("%s:\n%s\nwant:\n%s", p, got, want)
	}
	if err := confcheck.Check(p, []byte(want)); err != nil {
		t.Fatalf("the agent refuses the idle pool: %v", err)
	}
	req := applyWriting(t, f, p)
	if req == nil || !slices.ContainsFunc(req.Validate, func(v []string) bool { return v[0] == "/usr/sbin/php-fpm8.3" }) || !slices.Contains(req.Reload, "php8.3-fpm.service") {
		t.Fatalf("the idle pool went in without php-fpm -t and a reload: %+v", req)
	}
	for _, file := range f.agent.Files() {
		if strings.HasSuffix(file, "php-fpm.conf") {
			t.Errorf("Debian's php-fpm.conf was written: %s", file)
		}
	}
}

// On EL the master's log goes to syslog as well, out of the directory
// apache owns: one line of Remi's php-fpm.conf changes, the rest stays byte
// for byte, and the agent takes it.
func TestPHPInstallOnELSendsTheMasterLogToSyslog(t *testing.T) {
	withOSRelease(t, elRelease)
	f := newSiteFixture(t)
	configureChecks(t, f)
	const conf = "/etc/opt/remi/php83/php-fpm.conf"
	stock := strings.ReplaceAll(remiStockConf, "php84", "php83")
	f.agent.ReadFile[conf] = stock
	installPHP(t, f, "8.3")

	const pool = "/etc/opt/remi/php83/php-fpm.d/www.conf"
	want, _ := confcheck.IdlePool(f.s.cfg.RunDir, pool)
	if got, _ := f.agent.File(pool); got != want || !strings.Contains(got, "\ngroup = nobody\n") {
		t.Fatalf("%s:\n%s", pool, got)
	}
	got, ok := f.agent.File(conf)
	if !ok {
		t.Fatal("php-fpm.conf was not written")
	}
	assertOneLineChanged(t, stock, got, "error_log = /var/opt/remi/php83/log/php-fpm/error.log", "error_log = syslog")
	for _, p := range []string{pool, conf} {
		c, _ := f.agent.File(p)
		if err := confcheck.Check(p, []byte(c)); err != nil {
			t.Errorf("the agent refuses %s: %v", p, err)
		}
	}
	if req := applyWriting(t, f, conf); req == nil || !slices.Contains(req.Reload, "php83-php-fpm.service") || len(req.Validate) == 0 {
		t.Fatalf("php-fpm.conf went in without php-fpm -t and a reload: %+v", req)
	}
}

// assertOneLineChanged wants got to be old with the line from replaced by
// to and nothing else changed.
func assertOneLineChanged(t *testing.T, old, got, from, to string) {
	t.Helper()
	ol, gl := strings.Split(old, "\n"), strings.Split(got, "\n")
	if len(ol) != len(gl) {
		t.Fatalf("%d lines became %d:\n%s", len(ol), len(gl), got)
	}
	changed := 0
	for i := range ol {
		if ol[i] == gl[i] {
			continue
		}
		changed++
		if ol[i] != from || gl[i] != to {
			t.Errorf("line %d: %q became %q", i+1, ol[i], gl[i])
		}
	}
	if changed != 1 {
		t.Errorf("%d lines changed, want 1:\n%s", changed, got)
	}
}

// At its start the API makes the distribution's pool of every installed
// branch idle — the panel up to 0.8.19 left it as shipped — on EL with the
// master's log in syslog. It writes those two files and nothing else, checks
// and reloads that branch's php-fpm, and does so once: an administrator who
// brings the stock pool back afterwards keeps it, and the doctor says so,
// until the file is removed — then the idle pool comes back, and
// php-fpm.conf stays as it is. A branch whose packages are gone, or one that
// is not installed, is left alone.
func TestIdlePHPPoolsAtStart(t *testing.T) {
	for _, c := range []struct {
		name, release, pool, stock, conf, fpmBinary, unit string
	}{
		{"debian", "ID=debian\nVERSION_ID=13\n", "/etc/php/8.4/fpm/pool.d/www.conf", debianStockPool, "", "/usr/sbin/php-fpm8.4", "php8.4-fpm.service"},
		{"ubuntu", ubuntuRelease, "/etc/php/8.4/fpm/pool.d/www.conf", debianStockPool, "", "/usr/sbin/php-fpm8.4", "php8.4-fpm.service"},
		{"el remi", elRelease, "/etc/opt/remi/php84/php-fpm.d/www.conf", remiStockPool, "/etc/opt/remi/php84/php-fpm.conf", "/opt/remi/php84/root/usr/sbin/php-fpm", "php84-php-fpm.service"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withOSRelease(t, c.release)
			f := newSiteFixture(t)
			configureChecks(t, f)
			l := osprofile.PHP(f.s.profile, "8.4")
			if l == nil || phpDistroPool(l) != c.pool {
				t.Fatalf("layout of 8.4: %+v", l)
			}
			// A site's pool in the same directory, and a branch in error
			// whose files are there as well.
			sitePool := l.PoolDir + "/example.com.conf"
			f.agent.ReadFile[sitePool] = "[example.com]\nuser = alex\n"
			if err := f.db.UpsertPHPVersion(f.ctx, &store.PHPVersion{Version: "8.2", Source: l.Source, Status: store.PHPError}); err != nil {
				t.Fatal(err)
			}
			broken := osprofile.PHP(f.s.profile, "8.2")
			f.agent.Stat[broken.PoolDir+"/"] = true

			// The packages are gone: no pool directory, nothing written.
			f.s.refreshIdlePHPPools(f.ctx)
			if got := applies(t, f); len(got) != 0 {
				t.Fatalf("written without a pool directory: %+v", got)
			}

			f.agent.Stat[l.PoolDir+"/"] = true
			f.agent.ReadFile[c.pool] = c.stock
			if c.conf != "" {
				f.agent.ReadFile[c.conf] = remiStockConf
			}
			f.s.refreshIdlePHPPools(f.ctx)
			got := applies(t, f)
			if len(got) != 1 {
				t.Fatalf("%d config/apply calls, want 1: %+v", len(got), got)
			}
			var paths []string
			for _, file := range got[0].Files {
				paths = append(paths, file.Path)
				if err := confcheck.Check(file.Path, []byte(file.Content)); err != nil {
					t.Errorf("the agent refuses %s: %v", file.Path, err)
				}
			}
			want := []string{c.pool}
			if c.conf != "" {
				want = append(want, c.conf)
			}
			if !slices.Equal(paths, want) {
				t.Fatalf("written %v, want %v", paths, want)
			}
			if len(got[0].Validate) != 1 || got[0].Validate[0][0] != c.fpmBinary || !slices.Equal(got[0].Reload, []string{c.unit}) {
				t.Fatalf("validate %v, reload %v", got[0].Validate, got[0].Reload)
			}
			idle, _ := confcheck.IdlePool(f.s.cfg.RunDir, c.pool)
			if content, _ := f.agent.File(c.pool); content != idle {
				t.Fatalf("%s:\n%s", c.pool, content)
			}
			if c.conf != "" {
				content, _ := f.agent.File(c.conf)
				assertOneLineChanged(t, remiStockConf, content, "error_log = /var/opt/remi/php84/log/php-fpm/error.log", "error_log = syslog")
			}
			if _, ok := f.agent.File(sitePool); ok || len(f.agent.Removed()) != 0 {
				t.Fatalf("other files of the pool directory touched: removed %v", f.agent.Removed())
			}

			// Once idle, the next start leaves the branch alone: no php-fpm -t,
			// no reload.
			f.s.refreshIdlePHPPools(f.ctx)
			if n := len(applies(t, f)); n != 1 {
				t.Fatalf("an idle branch was applied again: %d calls", n)
			}

			if v, _ := f.db.GetSetting(f.ctx, settingPHPDistroIdle("8.4")); v != "yes" {
				t.Fatalf("the idle pool is not remembered: %q", v)
			}
			if checks := f.s.phpDistroChecks(f.ctx); len(checks) != 0 {
				t.Fatalf("doctor on an idle branch: %+v", checks)
			}

			// The administrator brings the stock pool back — the maintainer's
			// version at dpkg's prompt, an apt-installed phpMyAdmin that needs
			// it — and on EL the master's log file: the next start leaves both
			// so, the doctor names them.
			f.agent.Reset()
			f.agent.ReadFile[c.pool] = c.stock
			if c.conf != "" {
				f.agent.ReadFile[c.conf] = remiStockConf
			}
			f.s.refreshIdlePHPPools(f.ctx)
			if got := applies(t, f); len(got) != 0 {
				t.Fatalf("the administrator's files were reverted: %+v", got)
			}
			checks := f.s.phpDistroChecks(f.ctx)
			if len(checks) != 1 || checks[0].Status != "warn" || !strings.Contains(checks[0].Detail, c.pool+" is not the panel's idle pool") || !strings.Contains(checks[0].Detail, "remove the file and restart monopanel-api") {
				t.Fatalf("doctor: %+v", checks)
			}
			if c.conf != "" && !strings.Contains(checks[0].Detail, c.conf+" has the php-fpm master log into a file") {
				t.Fatalf("doctor on EL: %+v", checks)
			}

			// Removed, the pool is the panel's again: written at the next
			// start, the pool alone.
			delete(f.agent.ReadFile, c.pool)
			f.s.refreshIdlePHPPools(f.ctx)
			got = applies(t, f)
			if len(got) != 1 || len(got[0].Files) != 1 || got[0].Files[0].Path != c.pool {
				t.Fatalf("after the removal: %+v", got)
			}
			if content, _ := f.agent.File(c.pool); content != idle {
				t.Fatalf("%s after the removal:\n%s", c.pool, content)
			}
		})
	}
}

// A branch the panel installs is idle from then on, remembered as such; on
// EL without the syslog line (php-fpm.conf unreadable) it is not, and the
// next start tries again.
func TestPHPInstallRemembersTheIdlePool(t *testing.T) {
	stubSury(t, true)
	f := newSiteFixture(t)
	installPHP(t, f, "8.3")
	if v, _ := f.db.GetSetting(f.ctx, settingPHPDistroIdle("8.3")); v != "yes" {
		t.Fatalf("Debian: %q", v)
	}

	withOSRelease(t, elRelease)
	g := newSiteFixture(t)
	// php-fpm.conf missing: the master keeps its log file for now.
	installPHP(t, g, "8.3")
	if v, _ := g.db.GetSetting(g.ctx, settingPHPDistroIdle("8.3")); v == "yes" {
		t.Fatal("remembered as idle without the master's log in syslog")
	}
	l := osprofile.PHP(g.s.profile, "8.3")
	g.agent.Stat[l.PoolDir+"/"] = true
	g.agent.ReadFile[l.FPMConf] = strings.ReplaceAll(remiStockConf, "php84", "php83")
	g.s.refreshIdlePHPPools(g.ctx)
	if v, _ := g.db.GetSetting(g.ctx, settingPHPDistroIdle("8.3")); v != "yes" {
		t.Fatalf("EL after the next start: %q", v)
	}
	if conf, _ := g.agent.File(l.FPMConf); !strings.Contains(conf, "\nerror_log = syslog\n") {
		t.Fatalf("php-fpm.conf:\n%s", conf)
	}
}

// A pool file the agent does not find is written, with the panel's socket
// directory made first (php-fpm does not start without it); one it cannot
// read is left alone.
func TestIdlePHPPoolMissingOrUnreadable(t *testing.T) {
	f := newSiteFixture(t)
	l := osprofile.PHP(f.s.profile, "8.4")
	f.agent.Stat[l.PoolDir+"/"] = true
	f.agent.Fail["/v1/file/read"] = "open " + phpDistroPool(l) + ": no such file or directory"
	f.agent.FailStatus = map[string]int{"/v1/file/read": http.StatusNotFound}
	f.s.refreshIdlePHPPools(f.ctx)
	if _, ok := f.agent.File(phpDistroPool(l)); !ok {
		t.Fatal("a missing pool file was not written")
	}
	if _, ok := f.agent.DirSpec(f.s.cfg.RunDir + "/php"); !ok {
		t.Error("the socket directory was not made before the idle pool")
	}

	f.agent.Reset()
	f.agent.FailStatus["/v1/file/read"] = http.StatusForbidden
	f.s.refreshIdlePHPPools(f.ctx)
	if got := applies(t, f); len(got) != 0 {
		t.Fatalf("written although the agent could not read the pool: %+v", got)
	}
}

// fpmSyslog changes the master's error_log lines and nothing else; the
// pools' own settings stay.
func TestFPMSyslog(t *testing.T) {
	for _, c := range []struct {
		name, in, out string
		changed       bool
	}{
		{"in [global]", "[global]\npid = /run/x.pid\nerror_log = /var/log/x.log\n", "[global]\npid = /run/x.pid\nerror_log = syslog\n", true},
		{"syslog already", "[global]\nerror_log = syslog\n", "[global]\nerror_log = syslog\n", false},
		{"syslog quoted", "[global]\nerror_log = \"syslog\" ; ours\n", "[global]\nerror_log = \"syslog\" ; ours\n", false},
		{"before any section", "include=/etc/x/*.conf\nerror_log=/var/log/x.log\n[global]\n", "include=/etc/x/*.conf\nerror_log = syslog\n[global]\n", true},
		{"upper case", "[GLOBAL]\nERROR_LOG = /x\n", "[GLOBAL]\nerror_log = syslog\n", true},
		{"every line of it", "[global]\nerror_log = /a\nerror_log = /b\n", "[global]\nerror_log = syslog\nerror_log = syslog\n", true},
		{"commented out", "[global]\n;error_log = /var/log/x.log\ndaemonize = yes\n", "[global]\nerror_log = syslog\n;error_log = /var/log/x.log\ndaemonize = yes\n", true},
		{"none, no [global]", "; comment\ndaemonize = yes\n", "error_log = syslog\n; comment\ndaemonize = yes\n", true},
		{"a pool's own", "[global]\npid = /run/x.pid\n[www]\nerror_log = /pool.log\n", "[global]\nerror_log = syslog\npid = /run/x.pid\n[www]\nerror_log = /pool.log\n", true},
		{"no trailing line feed", "[global]\nerror_log = /x", "[global]\nerror_log = syslog", true},
	} {
		out, changed := fpmSyslog(c.in)
		if out != c.out || changed != c.changed {
			t.Errorf("%s: %v\n%q\nwant %v\n%q", c.name, changed, out, c.changed, c.out)
		}
	}
	out, _ := fpmSyslog(remiStockConf)
	assertOneLineChanged(t, remiStockConf, out, "error_log = /var/opt/remi/php84/log/php-fpm/error.log", "error_log = syslog")
	if err := confcheck.Check("/etc/opt/remi/php84/php-fpm.conf", []byte(out)); err != nil {
		t.Fatalf("the agent refuses Remi's php-fpm.conf with syslog: %v", err)
	}
	if err := confcheck.Check("/etc/opt/remi/php84/php-fpm.conf", []byte(remiStockConf)); err == nil {
		t.Fatal("the agent takes the master's log in apache's directory")
	}
}
