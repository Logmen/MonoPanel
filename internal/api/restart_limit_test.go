package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// pkgRuns are the package lists of the /v1/pkg calls of one action, in order.
func (f *siteFixture) pkgRuns(action string) [][]string {
	var out [][]string
	for _, c := range f.agent.Calls() {
		var req agent.PkgRequest
		if c.Path == "/v1/pkg" && json.Unmarshal(c.Body, &req) == nil && req.Action == action {
			out = append(out, req.Packages)
		}
	}
	return out
}

// installPHP installs a branch through the API, its repository taken as
// configured already, and returns the finished job.
func (f *siteFixture) installPHP(version string) *store.Job {
	f.t.Helper()
	if err := f.db.SetSetting(f.ctx, settingPHPRepo+"."+string(f.s.profile.Family()), "ready"); err != nil {
		f.t.Fatal(err)
	}
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/php/versions", map[string]any{"version": version}, http.StatusAccepted, &ref)
	return f.waitJob(ref.JobID)
}

// The optional extensions of a branch install in one run of the package
// manager: every run with packages of a branch restarts its php-fpm, and
// with a run a package Debian 12 restarted php8.4-fpm more often than
// systemd's start limit lets it, which then refused the panel's own reload.
// The repositories are asked once which of them they carry. A name they do
// not carry as a package of its own may be one another package provides —
// Ubuntu's own PHP ships php-imagick as php8.3-imagick, Debian's
// php8.3-xml brings php8.3-xsl —: those go in one more run, one by one only
// when a name nothing provides fails it, and an extension a package of the
// branch has built in already counts as there. What is left is named.
func TestPHPExtrasInstallInOneRun(t *testing.T) {
	const deb12 = "ID=debian\nVERSION_ID=12\n"
	for _, c := range []struct {
		name, release string
		// other are the names the repositories carry no package of, and
		// nothing are those of them nothing provides either.
		other, nothing []string
		iniDir, ini    string
		provided       []string // the job log's "provided by" line
		skipped        string   // its "not available" line
	}{
		{name: "all carried", release: deb12},
		{name: "provided by packages to come", release: "ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=noble\n",
			other: []string{"php8.3-imagick", "php8.3-redis"}, provided: []string{"php8.3-imagick", "php8.3-redis"}},
		{name: "some provided by nothing", release: deb12,
			other: []string{"php8.3-xsl", "php8.3-imap", "php8.3-sodium"}, nothing: []string{"php8.3-imap", "php8.3-sodium"},
			iniDir: "/etc/php/8.3/mods-available", ini: "sodium.ini",
			provided: []string{"php8.3-xsl", "php8.3-sodium"}, skipped: "php8.3-imap"},
		{name: "EL", release: elRelease,
			other: []string{"php83-php-pecl-memcache", "php83-php-sodium"}, nothing: []string{"php83-php-pecl-memcache", "php83-php-sodium"},
			iniDir: "/etc/opt/remi/php83/php.d", ini: "20-sodium.ini",
			provided: []string{"php83-php-sodium"}, skipped: "php83-php-pecl-memcache"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withOSRelease(t, c.release)
			f := newSiteFixture(t)
			f.agent.MissingPackages = map[string]bool{}
			f.agent.InstallFails = map[string]string{}
			for _, p := range c.other {
				f.agent.MissingPackages[p] = true
			}
			for _, p := range c.nothing {
				f.agent.InstallFails[p] = "E: Unable to locate package " + p
			}
			if c.iniDir != "" {
				f.agent.DirEntries[c.iniDir] = []string{c.ini, "README"}
			}
			job := f.installPHP("8.3")
			if job.Status != store.JobDone {
				t.Fatalf("install: %s %s\n%s", job.Status, job.Error, job.Log)
			}
			layout := osprofile.PHP(f.s.profile, "8.3")
			var carried []string
			for _, p := range layout.ExtraPackages {
				if !slices.Contains(c.other, p) {
					carried = append(carried, p)
				}
			}
			want := [][]string{layout.CorePackages, carried}
			if len(c.other) > 0 {
				want = append(want, c.other)
			}
			if len(c.nothing) > 0 {
				for _, p := range c.other {
					want = append(want, []string{p})
				}
			}
			// The branch's own runs: an EL host installs semanage first.
			branch := func(p string) bool {
				return slices.Contains(layout.CorePackages, p) || slices.Contains(layout.ExtraPackages, p)
			}
			var runs [][]string
			for _, run := range f.pkgRuns("install") {
				if slices.ContainsFunc(run, branch) {
					runs = append(runs, run)
				}
			}
			if !slices.EqualFunc(runs, want, slices.Equal[[]string]) {
				t.Fatalf("installs of the branch's packages:\n%q\nwant\n%q", runs, want)
			}
			asked := 0
			for _, run := range f.pkgRuns("available") {
				if slices.Equal(run, layout.ExtraPackages) {
					asked++
				}
			}
			if asked != 1 {
				t.Fatalf("the repositories were asked %d times for the extensions: %q", asked, f.pkgRuns("available"))
			}
			lines := map[string]bool{"optional extensions: " + strings.Join(carried, " ") + "\n": true}
			if len(c.provided) > 0 {
				lines["provided by other packages of the branch: "+strings.Join(c.provided, " ")+"\n"] = true
			}
			if c.skipped != "" {
				lines["not available for this branch, skipped: "+c.skipped+"\n"] = true
			}
			for line := range lines {
				if !strings.Contains(job.Log, line) {
					t.Errorf("the job log lacks %q:\n%s", line, job.Log)
				}
			}
			for _, not := range []string{"one by one", "did not install"} {
				if strings.Contains(job.Log, not) {
					t.Errorf("the job log says %q although every run that had to went through:\n%s", not, job.Log)
				}
			}
			if c.skipped == "" && strings.Contains(job.Log, "not available") {
				t.Errorf("something is called unavailable:\n%s", job.Log)
			}
		})
	}
}

// When the one run fails — a package whose dependency no repository carries
// fails apt's whole transaction — the extensions install one by one, the
// job says so, and the broken one keeps none of the others out.
func TestPHPExtrasOneByOneWhenTheRunFails(t *testing.T) {
	f := newSiteFixture(t)
	const broken = "php8.3-imagick"
	f.agent.InstallFails = map[string]string{broken: "Reading package lists...\nThe following packages have unmet dependencies:\n php8.3-imagick : Depends: libmagickwand-6.q16-7 but it is not installable\nE: Unable to correct problems, you have held broken packages."}
	job := f.installPHP("8.3")
	if job.Status != store.JobDone {
		t.Fatalf("install: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	layout := osprofile.PHP(f.s.profile, "8.3")
	runs := f.pkgRuns("install")
	if len(runs) != 2+len(layout.ExtraPackages) || !slices.Equal(runs[1], layout.ExtraPackages) {
		t.Fatalf("installs: %q", runs)
	}
	var ok []string
	for i, p := range layout.ExtraPackages {
		if !slices.Equal(runs[2+i], []string{p}) {
			t.Fatalf("install %d: %q, want %s alone", 2+i, runs[2+i], p)
		}
		if p != broken {
			ok = append(ok, p)
		}
	}
	for _, line := range []string{
		"the optional extensions did not install in one run (",
		"libmagickwand-6.q16-7 but it is not installable",
		"): installing them one by one\n",
		"optional extensions: " + strings.Join(ok, " ") + "\n",
		"did not install, skipped: " + broken + "\n",
	} {
		if !strings.Contains(job.Log, line) {
			t.Errorf("the job log lacks %q:\n%s", line, job.Log)
		}
	}
	if strings.Contains(job.Log, "not available for this branch") {
		t.Errorf("a package the repositories carry is called unavailable:\n%s", job.Log)
	}
}

// A fail2ban that should run and does not — systemd refused a restart for
// the start limit after many new sites, or it crashed — is restarted by the
// next refresh of the jails even when they stay as they are: once, and said
// so. One that runs is not restarted for jails that did not change, one an
// administrator disabled stays off, one on its way up or down is left to it.
func TestFail2banRestartedWhenDown(t *testing.T) {
	f := newSiteFixture(t)
	f.installFail2ban(t)
	logs := &logBuffer{}
	f.s.log = slog.New(slog.NewTextHandler(logs, nil))
	for _, c := range []struct {
		name, active, file string
		restart            bool
	}{
		{"running", "active", "enabled", false},
		{"refused for the start limit", "failed", "enabled", true},
		{"stopped", "inactive", "enabled", true},
		{"disabled by the administrator", "inactive", "disabled", false},
		{"starting", "activating", "enabled", false},
	} {
		f.agent.Reset()
		f.agent.ActiveState = map[string]string{fail2banService: c.active}
		f.agent.UnitFileState = map[string]string{fail2banService: c.file}
		before := strings.Count(logs.String(), "fail2ban is enabled but not running")
		f.s.refreshFail2ban(f.ctx)
		var forced []bool
		for _, call := range f.agent.Calls() {
			if req, ok := applyOf(call); ok && slices.ContainsFunc(req.Files, func(fs agent.FileSpec) bool { return fs.Path == fail2banJailPath }) {
				forced = append(forced, req.Force)
				if !slices.Equal(req.Restart, []string{fail2banService}) {
					t.Errorf("%s: restarts %v", c.name, req.Restart)
				}
			}
		}
		if len(forced) != 1 || forced[0] != c.restart {
			t.Errorf("%s: the jails applied %d time(s), forced %v; want once, forced %v", c.name, len(forced), forced, c.restart)
		}
		if logged := strings.Count(logs.String(), "fail2ban is enabled but not running") - before; logged != map[bool]int{true: 1}[c.restart] {
			t.Errorf("%s: logged the restart %d time(s)", c.name, logged)
		}
	}
}

// enforcingEL is an EL host with SELinux enforcing (or permissive, when the
// test writes "0" to the returned file).
func enforcingEL(t *testing.T) string {
	t.Helper()
	withOSRelease(t, elRelease)
	enforce := filepath.Join(t.TempDir(), "enforce")
	if err := os.WriteFile(enforce, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := selinuxEnforcePath
	selinuxEnforcePath = enforce
	t.Cleanup(func() { selinuxEnforcePath = prev })
	return enforce
}

// failSemanage has every semanage fail, as a broken policy store does.
func failSemanage(f *siteFixture) {
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "semanage" {
			return &agent.ToolResponse{ExitCode: 1, Output: "libsemanage.semanage_commit_sandbox: Error while renaming /var/lib/selinux/targeted/active"}
		}
		return nil
	}
}

// semanageCall tells a call that runs semanage.
func semanageCall(c agenttest.Call) bool {
	var req agent.ToolRequest
	return c.Path == "/v1/tool" && json.Unmarshal(c.Body, &req) == nil && req.Name == "semanage"
}

func siteLogsCall(c agenttest.Call) bool { return c.Path == "/v1/sitelogs/ensure" }

// The hourly retry gives the host the SELinux label rule of the log tree
// before it moves the sites' logs: a semanage that failed at the start and
// works again lets them move within the hour, not at the next start of the
// panel. While it fails, nothing moves.
func TestHourlyRetryAddsTheSELinuxRuleFirst(t *testing.T) {
	enforcingEL(t)
	f := newSiteFixture(t)
	logs := &logBuffer{}
	f.s.log = slog.New(slog.NewTextHandler(logs, nil))
	site := f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	// Upgraded from 0.8.19 with the policy in place, the rule of the tree
	// not added at this start: semanage failed.
	if err := f.db.SetSetting(f.ctx, settingSELinux, selinuxPolicyVersion); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetSetting(f.ctx, settingSELinuxSiteLogs, ""); err != nil {
		t.Fatal(err)
	}
	f.listUnmoved(site)
	f.s.refreshed.Store(true)
	failSemanage(f)
	f.agent.Reset()
	f.s.retryMoves(f.ctx)
	if f.callAt(0, semanageCall) < 0 {
		t.Fatal("the hour did not try the rule")
	}
	if sites, _ := f.logMoveJobs(); len(sites) != 0 || f.callAt(0, siteLogsCall) >= 0 {
		t.Fatalf("moved without the rule: %v", sites)
	}
	if !strings.Contains(logs.String(), "the panel tries the rule again every hour") {
		t.Errorf("log:\n%s", logs.String())
	}

	f.agent.ToolHook = nil
	f.agent.Reset()
	f.s.retryMoves(f.ctx)
	if rule, made := f.callAt(0, semanageCall), f.callAt(0, siteLogsCall); rule < 0 || made < 0 || rule > made {
		t.Fatalf("the rule at call %d, the site's logs at call %d: the rule comes first", rule, made)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingSELinuxSiteLogs); v != f.s.selinuxSiteLogsContext()[0] {
		t.Fatalf("the rule is not remembered: %q", v)
	}
	f.settle()
	if sites, _ := f.logMoveJobs(); len(sites[site.ID]) != 1 || len(f.pendingMoves()) != 0 {
		t.Fatalf("moved: %v, pending %v", sites, f.pendingMoves())
	}
	// In place now: the next hour asks semanage nothing.
	f.agent.Reset()
	f.s.retryMoves(f.ctx)
	if f.callAt(0, semanageCall) >= 0 {
		t.Fatal("the rule was added again")
	}
}

// A site applied on an enforcing host whose label rule of the log tree is
// not in place gets the rule first. Where semanage fails, the site is not
// applied — its servers could not open its logs in the tree, and php-fpm
// would stop every site of the branch — and keeps what it runs on, nothing
// of it written; the job says why. A permissive host denies nothing: the
// site is applied, with a warning. A suspended site's stub logs nothing.
func TestSiteApplyAddsTheSELinuxRuleFirst(t *testing.T) {
	enforce := enforcingEL(t)
	f := newSiteFixture(t)
	if err := f.db.SetSetting(f.ctx, settingSELinux, selinuxPolicyVersion); err != nil {
		t.Fatal(err)
	}
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	rule, conf := f.callAt(0, semanageCall), f.callAt(0, writes("/etc/nginx/monopanel/sites/example.com.conf"))
	if rule < 0 || conf < 0 || rule > conf {
		t.Fatalf("the rule at call %d, the site's configuration at call %d", rule, conf)
	}
	if v, _ := f.db.GetSetting(f.ctx, settingSELinuxSiteLogs); v != f.s.selinuxSiteLogsContext()[0] {
		t.Fatalf("the rule is not remembered: %q", v)
	}

	apply := func() *store.Job {
		t.Helper()
		var out struct {
			JobID int64 `json:"job_id"`
		}
		f.call(http.MethodPost, "/sites/example.com/apply", nil, http.StatusAccepted, &out)
		return f.waitJob(out.JobID)
	}
	if err := f.db.SetSetting(f.ctx, settingSELinuxSiteLogs, ""); err != nil {
		t.Fatal(err)
	}
	failSemanage(f)
	f.agent.Reset()
	job := apply()
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "the SELinux label rule of /var/log/monopanel/sites is not in place") || !strings.Contains(job.Error, "semanage") {
		t.Fatalf("apply without the rule: %s %q", job.Status, job.Error)
	}
	for _, c := range f.agent.Calls() {
		if c.Path == "/v1/config/apply" || c.Path == "/v1/sitelogs/ensure" || c.Path == "/v1/paths/remove" || c.Path == "/v1/service" {
			t.Fatalf("applied something without the rule: %s %s", c.Path, c.Body)
		}
	}

	// Permissive: applied, the rule tried and the warning in the job's log.
	if err := os.WriteFile(enforce, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	if job := apply(); job.Status != store.JobDone || !strings.Contains(job.Log, "warning: the SELinux label rule of /var/log/monopanel/sites is not in place") {
		t.Fatalf("permissive: %s %s\n%s", job.Status, job.Error, job.Log)
	}

	// The suspended stub: no rule needed.
	if err := os.WriteFile(enforce, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	f.siteAction("example.com", "suspend")
	if f.callAt(0, semanageCall) >= 0 {
		t.Fatal("the suspension asked for the rule")
	}
}

// The webmail on its own port logs into the site log tree as well. On an
// enforcing host whose label rule of the tree is not in place, nginx could
// not open those logs: a mail change goes through, and the webmail's server
// block stays as it is — an older panel's, logging into data/logs, which
// the move of the logs takes over once the rule is there — instead of one
// nginx cannot load. The rule is tried first, as for a site; once semanage
// works again, the next mail change adds it and moves the webmail.
func TestWebmailPortWaitsForTheSELinuxRule(t *testing.T) {
	enforcingEL(t)
	f := newMailFixture(t)
	f.createSite(map[string]any{"domain": "example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	c := f.s.loadMailConfig(f.ctx)
	c.Webmail, c.WebmailPort = "example.com", 2096
	if err := f.s.saveMailConfig(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	// Upgraded from 0.8.19: the port prepared long ago, the webmail not
	// moved yet, the rule of the tree not added at this start.
	if err := f.db.SetSetting(f.ctx, settingWebmailPortReady, "2096"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.changeSiteMoves(f.ctx, func(m *siteMoves) bool {
		m.Logs, m.Webmail = f.s.siteLogsRoot(), &retry{}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetSetting(f.ctx, settingSELinuxSiteLogs, ""); err != nil {
		t.Fatal(err)
	}
	failSemanage(f)
	logs := &logBuffer{}
	f.s.log = slog.New(slog.NewTextHandler(logs, nil))
	var jobLog []string
	logf := func(format string, args ...any) { jobLog = append(jobLog, fmt.Sprintf(format, args...)) }
	conf := f.s.webmailConfPath()

	f.agent.Reset()
	if err := f.s.applyMail(f.ctx, logf); err != nil {
		t.Fatalf("a mail change without the rule: %v", err)
	}
	if f.callAt(0, semanageCall) < 0 {
		t.Fatal("the rule was not tried")
	}
	if f.callAt(0, writes(conf)) >= 0 || f.callAt(0, siteLogsCall) >= 0 {
		t.Fatal("the webmail's server block was rendered into the tree without the rule")
	}
	if f.callAt(0, writes("/etc/postfix/main.cf")) < 0 {
		t.Fatal("the mail settings were not applied")
	}
	if f.s.siteMovesState(f.ctx).Webmail == nil {
		t.Fatal("the webmail left the moves without moving")
	}
	const stays = "the webmail on port 2096 stays as it is"
	if !strings.Contains(strings.Join(jobLog, "\n"), stays) || !strings.Contains(jobLog[len(jobLog)-1], "semanage") {
		t.Errorf("the job's log:\n%s", strings.Join(jobLog, "\n"))
	}
	if journal := logs.String(); !strings.Contains(journal, "the webmail on its port stays as it is") || !strings.Contains(journal, "port=2096") || !strings.Contains(journal, "semanage") {
		t.Errorf("the panel's log (a mail change of the API has no job log):\n%s", journal)
	}

	// semanage works again: the rule first, then the block, and the move
	// is done.
	f.agent.ToolHook = nil
	f.agent.Reset()
	if err := f.s.applyMail(f.ctx, logf); err != nil {
		t.Fatal(err)
	}
	if rule, block := f.callAt(0, semanageCall), f.callAt(0, writes(conf)); rule < 0 || block < 0 || rule > block {
		t.Fatalf("the rule at call %d, the webmail's block at call %d: the rule comes first", rule, block)
	}
	if got, _ := f.agent.File(conf); !strings.Contains(got, "/var/log/monopanel/sites/alex/") {
		t.Fatalf("the webmail's block does not log into the tree:\n%s", got)
	}
	if f.s.siteMovesState(f.ctx).Webmail != nil {
		t.Fatal("the webmail is still listed as not moved")
	}
}
