package api

import (
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/store"
)

const cliLink = "/var/www/alex/data/bin/php"

func (f *siteFixture) installedPHP(versions ...string) {
	f.t.Helper()
	for _, v := range versions {
		if err := f.db.UpsertPHPVersion(f.ctx, &store.PHPVersion{Version: v, Source: "sury", Status: store.PHPInstalled}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *siteFixture) cliTarget() string {
	t, _ := f.agent.Symlink(cliLink)
	return t
}

// The last job of a type, with its log.
func (f *siteFixture) lastJobLog(typ string) string {
	f.t.Helper()
	jobs, err := f.db.ListJobs(f.ctx, 50, "")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Type == typ {
			full, err := f.db.GetJob(f.ctx, j.ID)
			if err != nil {
				f.t.Fatal(err)
			}
			return full.Log
		}
	}
	return ""
}

// data/bin/php follows the PHP of the account's sites: it used to be made once
// and never moved, so the sites went to 8.2 while cron stayed on 7.4. A move
// is in the job's log; an apply that changes nothing writes nothing.
func TestCLIPHPFollowsTheSites(t *testing.T) {
	f := newSiteFixture(t)
	f.installedPHP("7.4", "8.2")
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "7.4", "ssl": "none"})
	if got := f.cliTarget(); got != "/usr/bin/php7.4" {
		t.Fatalf("first apply: %q", got)
	}
	f.updateSite("a.example.com", map[string]any{"php_version": "8.2"})
	if got := f.cliTarget(); got != "/usr/bin/php8.2" {
		t.Fatalf("after --php 8.2: %q", got)
	}
	if log := f.lastJobLog("site.apply"); !strings.Contains(log, "data/bin/php is PHP 8.2 now (was PHP 7.4)") {
		t.Errorf("the move is not in the log:\n%s", log)
	}
	if ini, _ := f.agent.File("/etc/php/8.2/cli/conf.d/99-monopanel.ini"); ini == "" {
		t.Error("CLI settings of 8.2 not written")
	}
	f.siteAction("a.example.com", "apply")
	if log := f.lastJobLog("site.apply"); strings.Contains(log, "data/bin/php") {
		t.Errorf("an apply that changes nothing logged a move:\n%s", log)
	}
	if got := f.doctorCheck("cli php"); !strings.HasPrefix(got, "ok: ") {
		t.Errorf("doctor: %q", got)
	}
}

// Sites on two branches: the newest. The only site on it gone: back to the
// other. A proxy site has no say.
func TestCLIPHPTakesTheNewestBranchOfTheSites(t *testing.T) {
	f := newSiteFixture(t)
	f.installedPHP("7.4", "8.2")
	f.createSite(map[string]any{"domain": "old.example.com", "user": "alex", "php_version": "7.4", "ssl": "none"})
	f.createSite(map[string]any{"domain": "new.example.com", "user": "alex", "php_version": "8.2", "ssl": "none"})
	f.createSite(map[string]any{"domain": "app.example.com", "user": "alex", "mode": "proxy", "backend": "http://127.0.0.1:3000", "ssl": "none"})
	f.siteAction("old.example.com", "apply")
	if got := f.cliTarget(); got != "/usr/bin/php8.2" {
		t.Fatalf("7.4 and 8.2: %q", got)
	}
	f.deleteSite("new.example.com")
	if got := f.cliTarget(); got != "/usr/bin/php7.4" {
		t.Fatalf("after the 8.2 site went: %q", got)
	}
	if log := f.lastJobLog("site.delete"); !strings.Contains(log, "data/bin/php is PHP 7.4 now (was PHP 8.2)") {
		t.Errorf("the move is not in the log:\n%s", log)
	}
}

// Removing a branch no site runs any more moves the links that point at it:
// they would point at nothing, and every cron job with php fail quietly.
func TestCLIPHPMovesOffARemovedBranch(t *testing.T) {
	f := newSiteFixture(t)
	f.installedPHP("7.4", "8.2")
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "7.4", "ssl": "none"})
	// What 0.9.6 left: the sites moved to 8.2, the link stayed on 7.4.
	f.updateSite("a.example.com", map[string]any{"php_version": "8.2"})
	f.agent.SetSymlink(cliLink, "/usr/bin/php7.4")
	f.agent.Stat["/usr/bin/php7.4"] = false
	if got := f.doctorCheck("cli php"); !strings.HasPrefix(got, "warn: ") || !strings.Contains(got, "alex: data/bin/php is PHP 7.4, its sites run PHP 8.2") {
		t.Errorf("doctor on a stale link: %q", got)
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/php/versions/7.4", nil, http.StatusAccepted, &out)
	if job := f.waitJob(out.JobID); job.Status != store.JobDone {
		t.Fatalf("php remove: %s %s", job.Status, job.Error)
	}
	if got := f.cliTarget(); got != "/usr/bin/php8.2" {
		t.Errorf("after removing 7.4: %q", got)
	}
	if log := f.lastJobLog("php.remove"); !strings.Contains(log, "data/bin/php is PHP 8.2 now (was PHP 7.4)") {
		t.Errorf("the move is not in the log:\n%s", log)
	}
	// A link left pointing at nothing fails the doctor.
	f.agent.SetSymlink(cliLink, "/usr/bin/php7.4")
	delete(f.agent.Stat, "/usr/bin/php7.4")
	if got := f.doctorCheck("cli php"); !strings.HasPrefix(got, "fail: ") || !strings.Contains(got, "which is gone") {
		t.Errorf("doctor on a broken link: %q", got)
	}
	f.siteAction("a.example.com", "apply")
	if got := f.cliTarget(); got != "/usr/bin/php8.2" {
		t.Errorf("apply did not relink: %q", got)
	}
}
