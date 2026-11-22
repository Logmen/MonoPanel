package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/agent/agenttest"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// jailChanges counts the writes of the jails at or after call from that
// restart fail2ban: the agent does so for every one whose content differs
// from the one before (prev, then each write), and for one that forces it.
func (f *siteFixture) jailChanges(from int, prev string) int {
	f.t.Helper()
	n := 0
	for _, c := range f.agent.Calls()[from:] {
		req, ok := applyOf(c)
		if !ok {
			continue
		}
		for _, file := range req.Files {
			if file.Path != fail2banJailPath {
				continue
			}
			if req.Force || file.Content != prev {
				n++
			}
			prev = file.Content
		}
	}
	return n
}

// The jails change once for the whole move of the sites' logs, not once a
// site: the tree logs of a site are listed from when the move made them —
// empty until the site moves — so that the site's own apply leaves the jails
// as they are. Every change restarts fail2ban, and a busy host's moves, one
// site after another, would restart it more often than systemd lets a unit
// start (five times in ten seconds by default), which leaves it stopped; the
// panel restarts it only when the jails change again.
func TestTheLogMoveChangesTheJailsOnce(t *testing.T) {
	f := newSiteFixture(t)
	domains := []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com"}
	for _, d := range domains {
		f.createSite(map[string]any{"domain": d, "user": "alex", "php_version": "8.4", "ssl": "none"})
	}
	f.installFail2ban(t)
	f.settle()
	prev, _ := f.agent.File(fail2banJailPath)
	from := len(f.agent.Calls())
	// The start-up refresh of an upgraded panel: the move, then the jails.
	f.s.moveSiteLogs(f.ctx)
	f.s.refreshFail2ban(f.ctx)
	f.settle()
	if p := f.pendingMoves(); len(p) != 0 {
		t.Fatalf("not moved: %v", p)
	}
	if n := f.jailChanges(from, prev); n > 1 {
		t.Fatalf("the jails changed %d times for the move of %d sites: fail2ban restarts as often", n, len(domains))
	}
	jail, _ := f.agent.File(fail2banJailPath)
	for _, d := range domains {
		if !strings.Contains(jail, "/var/log/monopanel/sites/alex/"+d+".access.log") {
			t.Fatalf("%s is not in the jails:\n%s", d, jail)
		}
	}
}

// listUnmoved puts the site on the moves' list without queuing anything, as
// a panel of 0.8.19 left it at this version's first start.
func (f *siteFixture) listUnmoved(sites ...*store.Site) {
	f.t.Helper()
	if err := f.s.changeSiteMoves(f.ctx, func(m *siteMoves) bool {
		m.Logs = f.s.siteLogsRoot()
		if m.Sites == nil {
			m.Sites = map[int64]*siteMove{}
		}
		for _, site := range sites {
			m.Sites[site.ID] = &siteMove{Domain: site.Domain}
		}
		return true
	}); err != nil {
		f.t.Fatal(err)
	}
}

// heldMove starts the move of the sites to their accounts' Apaches and
// holds it while it applies the site's nginx configuration.
func (f *siteFixture) heldMove(domain string) (release func()) {
	f.t.Helper()
	held, release := heldApply(f, "/etc/nginx/monopanel/sites/"+domain+".conf")
	f.s.refreshApacheInstances(f.ctx)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		f.t.Fatal("the move never applied nginx")
	}
	return release
}

// A site removed while the move of the sites to their accounts' Apaches
// applies it gets no configuration back once the removal is done: the
// removal waits for the site's lock, and an apply after it renders nothing
// for a site being removed. Otherwise the site's nginx server block, its
// pool and its virtual host would stay on the host for a site the panel no
// longer knows.
func TestApacheMoveLeavesADeletedSiteDeleted(t *testing.T) {
	f := newApacheFixture(t)
	a := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.agent.Reset()
	f.sharedLayout(nil)
	f.listUnmoved(a)
	release := f.heldMove("a.example.com")
	var del struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodDelete, "/sites/a.example.com", nil, http.StatusAccepted, &del)
	// The removal runs beside the move (another job's lock), or waits.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := f.db.GetJob(f.ctx, del.JobID); j.Status == store.JobDone || j.Status == store.JobFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	release()
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("delete: %s %s", j.Status, j.Error)
	}
	f.waitJobOfType("apache.migrate")
	f.settle()
	if _, err := f.db.GetSiteByDomain(f.ctx, "a.example.com"); err == nil {
		t.Fatal("the site is still there")
	}
	for _, p := range []string{"/etc/nginx/monopanel/sites/a.example.com.conf", "/etc/php/8.4/fpm/pool.d/a.example.com.conf", alexApacheDir + "/sites/a.example.com.conf"} {
		if _, ok := f.agent.File(p); ok {
			t.Errorf("%s is left behind for a removed site", p)
		}
	}
}

// An administrator who suspends a site while the move applies it keeps the
// suspension: the apply under way saves its status only if nobody changed it
// meanwhile, and the suspension's own apply, which waited for the site's
// lock, renders the stub.
func TestApacheMoveKeepsASuspensionMadeWhileItRan(t *testing.T) {
	f := newApacheFixture(t)
	a := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.agent.Reset()
	f.sharedLayout(nil)
	f.listUnmoved(a)
	release := f.heldMove("a.example.com")
	var out struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/a.example.com/suspend", nil, http.StatusAccepted, &out)
	release()
	if j := f.waitJob(out.JobID); j.Status != store.JobDone {
		t.Fatalf("suspend: %s %s", j.Status, j.Error)
	}
	f.waitJobOfType("apache.migrate")
	f.settle()
	site, _ := f.db.GetSiteByDomain(f.ctx, "a.example.com")
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/a.example.com.conf")
	if site.Status != store.SiteSuspended || !strings.Contains(conf, "site is suspended") {
		t.Fatalf("status %s, nginx:\n%s", site.Status, conf)
	}
}

// The same for a site's own apply: a suspension made while it runs is
// not overwritten with the status the apply read at its start.
func TestSiteApplyKeepsASuspensionMadeWhileItRan(t *testing.T) {
	f := newSiteFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	held, release := heldApply(f, "/etc/nginx/monopanel/sites/a.example.com.conf")
	var apply, suspend struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/sites/a.example.com/apply", nil, http.StatusAccepted, &apply)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the site.apply never applied nginx")
	}
	f.call(http.MethodPost, "/sites/a.example.com/suspend", nil, http.StatusAccepted, &suspend)
	release()
	for _, id := range []int64{apply.JobID, suspend.JobID} {
		if j := f.waitJob(id); j.Status != store.JobDone {
			t.Fatalf("job %d: %s %s", id, j.Status, j.Error)
		}
	}
	site, _ := f.db.GetSiteByDomain(f.ctx, "a.example.com")
	conf, _ := f.agent.File("/etc/nginx/monopanel/sites/a.example.com.conf")
	if site.Status != store.SiteSuspended || !strings.Contains(conf, "site is suspended") {
		t.Fatalf("status %s, nginx:\n%s", site.Status, conf)
	}
}

// The custom directives carried over to the account's Apache are no more
// readable than they were: an administrator's file only root read (a root
// umask of 077; it may hold a backend's password) is not made everybody's
// on the host, the accounts' included.
func TestUpgradeCarriesCustomDirectivesNoMoreReadable(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	f.agent.Reset()
	disk := f.sharedLayout(map[string]string{"a.example.com": "RequestHeader set Authorization \"Basic c2VjcmV0\"\n", "b.example.com": "Header set X-B b\n"})
	if err := os.Chmod(filepath.Join(disk, "a.example.com.d", "custom.conf"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.refreshApacheInstances(f.ctx)
	if job := f.waitJobOfType("apache.migrate"); job.Status != store.JobDone {
		t.Fatalf("migrate: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	for domain, want := range map[string]uint32{"a.example.com": 0o600, "b.example.com": 0o644} {
		spec, ok := f.agent.FileSpec(alexApacheDir + "/sites/" + domain + ".d/custom.conf")
		if !ok || spec.Mode != want {
			t.Errorf("%s: carried with mode %o, want %o (%v)", domain, spec.Mode, want, ok)
		}
	}
	if legacyCustomMode(0) != 0o600 || legacyCustomMode(0o444) != 0o644 || legacyCustomMode(0o640) != 0o640 {
		t.Fatal("legacyCustomMode")
	}
}

// The log move's apply of a site that another apply moved while it waited
// in the queue does nothing: on a busy host the move of the sites to their
// accounts' Apaches gets to many of them first, and each second apply would
// reload the site's servers once more for nothing.
func TestTheLogMoveSkipsASiteMovedMeanwhile(t *testing.T) {
	f := newSiteFixture(t)
	a := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.listUnmoved(a, b)
	// a moved by another apply meanwhile; b did not.
	if !f.s.siteMoved(f.ctx, a.ID, f.s.siteLogsRoot()) {
		t.Fatal("a was not listed")
	}
	from := len(f.agent.Calls())
	var ids []int64
	for _, site := range []*store.Site{a, b} {
		j, err := f.s.jobs.Enqueue(f.ctx, "site.apply", sitePayload{SiteID: site.ID, LogsMoved: true}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy("system"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	for _, id := range ids {
		if j := f.waitJob(id); j.Status != store.JobDone {
			t.Fatalf("job %d: %s %s", id, j.Status, j.Error)
		}
	}
	if at := f.callAt(from, writes("/etc/nginx/monopanel/sites/a.example.com.conf")); at >= 0 {
		t.Fatal("a applied again")
	}
	if at := f.callAt(from, writes("/etc/nginx/monopanel/sites/b.example.com.conf")); at < 0 {
		t.Fatal("b not moved")
	}
	if p := f.pendingMoves(); len(p) != 0 {
		t.Fatalf("pending: %v", p)
	}
}

// A site being removed, or one of an account being removed, is not rendered
// again: an apply that comes after its removal took the configuration away
// — the move of the sites to their accounts' Apaches waits for the site's
// lock and then applies — would leave it behind for a site the panel no
// longer knows.
func TestNoApplyForASiteBeingRemoved(t *testing.T) {
	f := newSiteFixture(t)
	a := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	apply := func() *store.Job {
		t.Helper()
		from := len(f.agent.Calls())
		j, err := f.s.jobs.Enqueue(f.ctx, "site.apply", sitePayload{SiteID: a.ID}, jobs.WithLockKey("site:"+a.Domain), jobs.WithRequestedBy("system"))
		if err != nil {
			t.Fatal(err)
		}
		job := f.waitJob(j.ID)
		if at := f.callAt(from, writes("/etc/nginx/monopanel/sites/a.example.com.conf")); at >= 0 {
			t.Fatalf("rendered while being removed:\n%s", job.Log)
		}
		return job
	}
	if err := f.db.SetSiteStatus(f.ctx, a.ID, store.SiteDeleting, ""); err != nil {
		t.Fatal(err)
	}
	if job := apply(); job.Status != store.JobDone || !strings.Contains(job.Log, "being removed") {
		t.Fatalf("site being removed: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if site, _ := f.db.GetSite(f.ctx, a.ID); site.Status != store.SiteDeleting {
		t.Fatalf("status %s", site.Status)
	}
	if err := f.db.SetSiteStatus(f.ctx, a.ID, store.SiteActive, ""); err != nil {
		t.Fatal(err)
	}
	owner, _ := f.db.GetUserByLogin(f.ctx, "alex")
	if err := f.db.SetUserStatus(f.ctx, owner.ID, store.UserDeleting); err != nil {
		t.Fatal(err)
	}
	if job := apply(); job.Status != store.JobDone {
		t.Fatalf("account being removed: %s %s", job.Status, job.Error)
	}
}

// A job that fails marks the site failed only over the status it read: a
// suspension asked for while it ran stays, and the suspension's own apply
// renders the stub.
func TestSiteFailKeepsASuspensionMadeWhileTheJobRan(t *testing.T) {
	f := newSiteFixture(t)
	a := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	read, _ := f.db.GetSite(f.ctx, a.ID)
	if err := f.db.SetSiteStatus(f.ctx, a.ID, store.SiteSuspended, ""); err != nil {
		t.Fatal(err)
	}
	_ = f.s.siteFail(f.ctx, read, errors.New("nginx: [emerg] something else"))
	if site, _ := f.db.GetSite(f.ctx, a.ID); site.Status != store.SiteSuspended || site.LastError != "" {
		t.Fatalf("after the failure: %s %q", site.Status, site.LastError)
	}
	read.Status = store.SiteSuspended
	_ = f.s.siteFail(f.ctx, read, errors.New("nginx: [emerg] something else"))
	if site, _ := f.db.GetSite(f.ctx, a.ID); site.Status != store.SiteError {
		t.Fatalf("a failure over the status the job read: %s", site.Status)
	}
}

// A site in Apache mode that moved to its account's Apache leaves the shared
// Apache of 0.8.19 at once: its virtual host there goes, after nginx sends
// the site elsewhere. The shared Apache keeps running while another site
// cannot move, and the rotation has it reopen its logs every day — as root,
// in the accounts' data/logs, which the move ends — so a site counted as
// moved must not be among them. The site that cannot move keeps its virtual
// host there, and the shared Apache keeps serving it.
func TestAMovedSiteLeavesTheSharedApache(t *testing.T) {
	f := newApacheFixture(t)
	f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "mode": "apache"})
	// b's PHP branch went away since: it cannot move.
	b.PHPVersion = "7.4"
	if err := f.db.UpdateSite(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	f.agent.Reset()
	f.sharedLayout(nil)
	f.s.moveSiteLogs(f.ctx)
	f.settle()
	if p := f.pendingMoves(); len(p) != 1 || p["b.example.com"] == nil {
		t.Fatalf("pending: %v", p)
	}
	shared := "/etc/apache2/monopanel/sites/"
	gone := f.callAt(0, func(c agenttest.Call) bool {
		req, ok := removeOf(c)
		return ok && slices.Contains(req.Paths, shared+"a.example.com.conf")
	})
	nginx := f.callAt(0, func(c agenttest.Call) bool {
		return writes("/etc/nginx/monopanel/sites/a.example.com.conf")(c) && reloads("nginx.service")(c)
	})
	if gone < 0 || nginx < 0 || gone < nginx {
		t.Fatalf("a's shared virtual host removed at %d, nginx switched at %d", gone, nginx)
	}
	if slices.Contains(f.agent.Removed(), shared+"b.example.com.conf") {
		t.Fatal("b left the shared Apache that still serves it")
	}
	// Once the move of the upgrade is done, nothing of the shared Apache is
	// left to look for.
	if err := f.db.SetSetting(f.ctx, settingApacheInstances, "yes"); err != nil {
		t.Fatal(err)
	}
	from := len(f.agent.Calls())
	f.siteAction("a.example.com", "apply")
	if at := f.callAt(from, func(c agenttest.Call) bool {
		req, ok := removeOf(c)
		return ok && slices.Contains(req.Paths, shared+"a.example.com.conf")
	}); at >= 0 {
		t.Fatal("looked for the shared Apache after the move")
	}
}

// A move waits after a failure, at most a day; a next try further away was
// put there by a clock set wrong at the failure (a host that booted years
// ahead, set right later) and is due at once.
func TestARetryWaitsNoLongerThanADay(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		r   retry
		due bool
	}{
		{retry{}, true},
		{retry{Job: 7}, false},
		{retry{Tries: 1, Next: now.Add(9 * time.Minute)}, false},
		{retry{Tries: 5, Next: now.Add(retryMax)}, false},
		{retry{Tries: 1, Next: now.Add(-time.Second)}, true},
		{retry{Tries: 1, Next: now.AddDate(4, 0, 0)}, true},
		{retry{Job: 7, Tries: 1, Next: now.AddDate(4, 0, 0)}, false},
	} {
		if got := c.r.due(now); got != c.due {
			t.Errorf("%+v: due %v, want %v", c.r, got, c.due)
		}
	}
}
