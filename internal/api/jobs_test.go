package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// limit counts the jobs the caller may see: an ordinary account gets as many
// of its own jobs as it asked for, however many newer jobs belong to others,
// with the status filter as well.
func TestJobsListLimitCountsOwnJobs(t *testing.T) {
	f := newSiteFixture(t)
	f.alexWithPassword()
	f.s.jobs.Register("test.noop", func(context.Context, *jobs.Context) error { return nil })
	enqueue := func(by string) int64 {
		t.Helper()
		j, err := f.s.jobs.Enqueue(f.ctx, "test.noop", struct{}{}, jobs.WithRequestedBy(by))
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	var mine, theirs []int64
	for range 3 {
		mine = append(mine, enqueue("alex"))
	}
	for range 5 {
		theirs = append(theirs, enqueue("admin"))
	}
	// All of them finish; then a few are marked failed, the administrator's
	// newer than the account's.
	for _, id := range append(append([]int64{}, mine...), theirs...) {
		f.waitJob(id)
	}
	for _, id := range []int64{mine[0], theirs[3], theirs[4]} {
		if err := f.db.FinishJob(f.ctx, id, store.JobFailed, "test"); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(path string) []int64 {
		t.Helper()
		var list []store.Job
		f.call(http.MethodGet, path, nil, http.StatusOK, &list)
		out := []int64{}
		for _, j := range list {
			out = append(out, j.ID)
		}
		return out
	}

	f.loginAs("alex", "alex-password")
	if got := ids("/jobs?limit=2"); !slices.Equal(got, []int64{mine[2], mine[1]}) {
		t.Errorf("limit=2 for the account: %v, want %v", got, []int64{mine[2], mine[1]})
	}
	if got := ids("/jobs?status=failed&limit=1"); !slices.Equal(got, []int64{mine[0]}) {
		t.Errorf("status=failed&limit=1 for the account: %v, want %v", got, []int64{mine[0]})
	}
	if got := ids("/jobs"); !slices.Equal(got, []int64{mine[2], mine[1], mine[0]}) {
		t.Errorf("every job of the account: %v", got)
	}

	f.login()
	if got := ids("/jobs?limit=2"); !slices.Equal(got, []int64{theirs[4], theirs[3]}) {
		t.Errorf("limit=2 for the administrator: %v", got)
	}
	if got := ids("/jobs?status=failed&limit=500"); !slices.Equal(got, []int64{theirs[4], theirs[3], mine[0]}) {
		t.Errorf("status=failed for the administrator: %v", got)
	}
}

// The doctor's jobs line counts a failure until a later run of the same work
// went through: the same type and lock key, and for the stack's jobs, which
// share the lock "stack", the same component. An apache.backend done does
// not fix the apache.migrate that failed before it, nor the nightly upgrade
// a failed mp stack install; mp stack install apache, the repair of the
// move, fixes the move; a failure older than a day is history.
func TestDoctorJobsLineWantsTheSameWork(t *testing.T) {
	now := time.Now()
	job := func(id int64, typ, lock, payload string, age time.Duration) *store.Job {
		return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(payload), CreatedAt: store.Time(now.Add(-age))}
	}
	migrate := job(2, "apache.migrate", "stack", `{}`, time.Hour)
	memcached := job(4, "stack.install", "stack", `{"component":"memcached"}`, time.Hour)
	upgrade := job(5, "stack.upgrade", "stack", `{"component":"all","scheduled":true}`, time.Hour)
	site := job(6, "site.apply", "site:a.example.com", `{"site_id":1}`, time.Hour)
	free := job(7, "backup.run", "", `{}`, time.Hour)
	old := job(1, "site.apply", "site:b.example.com", `{"site_id":2}`, 25*time.Hour)
	failed := []*store.Job{free, site, upgrade, memcached, migrate, old}
	for _, c := range []struct {
		name string
		done []*store.Job
		want int
	}{
		{"nothing done since", nil, 5},
		{"another job of the stack's lock", []*store.Job{job(3, "apache.backend", "stack", `{}`, time.Hour), job(8, "stack.upgrade", "stack", `{"component":"all","scheduled":true}`, 0)}, 4},
		{"the same work again", []*store.Job{job(9, "apache.migrate", "stack", `{}`, 0), job(10, "site.apply", "site:a.example.com", `{"site_id":1}`, 0)}, 3},
		{"another component", []*store.Job{job(11, "stack.install", "stack", `{"component":"git"}`, 0), job(12, "stack.upgrade", "stack", `{"component":"nginx"}`, 0)}, 5},
		{"the same component", []*store.Job{job(13, "stack.install", "stack", `{"component":"memcached"}`, 0), job(14, "stack.upgrade", "stack", `{}`, 0)}, 3},
		{"done before it failed", []*store.Job{job(1, "apache.migrate", "stack", `{}`, 2*time.Hour)}, 5},
		// The repair the doctor and the journal name for a failed move: it
		// moves every site and sets up every backend secret, or fails.
		{"mp stack install apache after the move failed", []*store.Job{job(15, "stack.install", "stack", `{"component":"apache"}`, 0)}, 4},
	} {
		if got := openFailures(failed, c.done, now); got != c.want {
			t.Errorf("%s: %d open, want %d", c.name, got, c.want)
		}
	}
	backend := job(16, "apache.backend", "stack", `{}`, time.Hour)
	install := job(17, "stack.install", "stack", `{"component":"apache"}`, time.Hour)
	if got := openFailures([]*store.Job{migrate, backend}, []*store.Job{job(18, "stack.install", "stack", `{"component":"apache"}`, 0)}, now); got != 0 {
		t.Errorf("the move and the backend secrets after mp stack install apache: %d open", got)
	}
	// Not the other way round: the move alone installs nothing.
	if got := openFailures([]*store.Job{install}, []*store.Job{job(19, "apache.migrate", "stack", `{}`, 0), job(20, "apache.backend", "stack", `{}`, 0)}, now); got != 1 {
		t.Errorf("a failed mp stack install apache after the move: %d open", got)
	}
}

// A failure on a site or an account that is gone is history: a site.delete
// that went through clears every failure of its site's lock key before it
// (an apply, a fix, a CMS install), a user.delete every one of its
// account's (a provision, a migration) — not those of another site or
// account, not one that failed after, and not the other way round: an apply
// that went through fixes no failed delete.
func TestDoctorJobsLineForgetsWhatIsGone(t *testing.T) {
	now := time.Now()
	job := func(id int64, typ, lock string) *store.Job {
		return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(`{}`), CreatedAt: store.Time(now.Add(-time.Hour))}
	}
	failed := []*store.Job{
		job(1, "site.apply", "site:a.example.com"),
		job(2, "site.fix", "site:a.example.com"),
		job(3, "site.cms", "site:a.example.com"),
		job(4, "site.apply", "site:b.example.com"),
		job(5, "user.provision", "user:alex"),
		job(6, "migrate.run", "user:alex"),
		job(7, "user.provision", "user:kate"),
		job(8, "site.delete", "site:c.example.com"),
	}
	for _, c := range []struct {
		name string
		done []*store.Job
		want int
	}{
		{"nothing gone", nil, 8},
		{"the site deleted", []*store.Job{job(10, "site.delete", "site:a.example.com")}, 5},
		{"the account deleted", []*store.Job{job(11, "user.delete", "user:alex")}, 6},
		{"both", []*store.Job{job(10, "site.delete", "site:a.example.com"), job(11, "user.delete", "user:alex")}, 3},
		{"deleted before the failures", []*store.Job{job(0, "site.delete", "site:a.example.com")}, 8},
		{"an apply after a failed delete", []*store.Job{job(12, "site.apply", "site:c.example.com")}, 8},
		{"the delete again", []*store.Job{job(13, "site.delete", "site:c.example.com")}, 7},
	} {
		if got := openFailures(failed, c.done, now); got != c.want {
			t.Errorf("%s: %d open, want %d", c.name, got, c.want)
		}
	}
	// A site made again under the name and failing afterwards is a failure.
	if got := openFailures([]*store.Job{job(20, "site.apply", "site:a.example.com")}, []*store.Job{job(10, "site.delete", "site:a.example.com")}, now); got != 1 {
		t.Errorf("a failure after the delete: %d open", got)
	}
}

// A user.delete removes the account's sites inline, without a site.delete
// of their own. Once it went through, what failed on the sites it names —
// the account's sites as they were when it was queued — is history as the
// account's own failures are: not what failed on another account's site,
// not what failed after the delete, and not with the payload of an older
// panel, which names no sites.
func TestDoctorJobsLineForgetsTheSitesOfADeletedAccount(t *testing.T) {
	now := time.Now()
	job := func(id int64, typ, lock, payload string) *store.Job {
		return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(payload), CreatedAt: store.Time(now.Add(-time.Hour))}
	}
	failed := []*store.Job{
		job(1, "site.apply", "site:a.example.com", `{"site_id":1}`),
		job(2, "site.fix", "site:a.example.com", `{"site_id":1}`),
		job(3, "site.delete", "site:b.example.com", `{"site_id":2,"purge":true}`),
		job(4, "site.apply", "site:c.example.com", `{"site_id":3}`),
		job(5, "user.provision", "user:alex", `{}`),
	}
	const withSites = `{"user_id":2,"login":"alex","purge":true,"sites":["a.example.com","b.example.com"]}`
	for _, c := range []struct {
		name string
		done []*store.Job
		want int
	}{
		{"nothing gone", nil, 5},
		{"the account and its sites", []*store.Job{job(10, "user.delete", "user:alex", withSites)}, 1},
		{"a delete queued by an older panel", []*store.Job{job(10, "user.delete", "user:alex", `{"user_id":2,"login":"alex","purge":true}`)}, 4},
		{"deleted before the failures", []*store.Job{job(0, "user.delete", "user:alex", withSites)}, 5},
		// Only a user.delete speaks for the sites it names.
		{"another job with the same payload", []*store.Job{job(10, "user.provision", "user:alex", withSites)}, 4},
	} {
		if got := openFailures(failed, c.done, now); got != c.want {
			t.Errorf("%s: %d open, want %d", c.name, got, c.want)
		}
	}
	// A site made again under the name and failing afterwards is a failure.
	if got := openFailures([]*store.Job{job(20, "site.apply", "site:a.example.com", `{"site_id":7}`)}, []*store.Job{job(10, "user.delete", "user:alex", withSites)}, now); got != 1 {
		t.Errorf("a failure after the delete: %d open", got)
	}
}

// The delete of an account names its sites in the job it queues; once the
// job went through, the doctor's jobs line no longer counts what failed on
// them.
func TestUserDeleteTakesItsSitesOffTheJobsLine(t *testing.T) {
	f := newSiteFixture(t)
	for _, d := range []string{"a.example.com", "b.example.com"} {
		f.createSite(map[string]any{"domain": d, "user": "alex", "php_version": "8.4", "ssl": "none"})
	}
	f.agent.Fail["/v1/config/apply"] = "nginx: [emerg] unknown directive"
	var ref apitypes.SiteWithJob
	f.call(http.MethodPost, "/sites/a.example.com/apply", nil, http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobFailed {
		t.Fatalf("the apply meant to fail: %s", job.Status)
	}
	delete(f.agent.Fail, "/v1/config/apply")
	if got := f.doctorCheck("jobs"); !strings.HasPrefix(got, "warn: 1 failed") {
		t.Fatalf("jobs line with the failed apply: %q", got)
	}

	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex", nil, http.StatusAccepted, &del)
	job := f.waitJob(del.JobID)
	if job.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", job.Status, job.Error)
	}
	var p userDeletePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || !slices.Equal(p.Sites, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("the delete names the sites %v (%v)", p.Sites, err)
	}
	if got := f.doctorCheck("jobs"); !strings.HasPrefix(got, "ok:") {
		t.Fatalf("jobs line after the account went with its sites: %q", got)
	}
}

// A site.apply that runs once its site is gone — queued by the webmail, the
// shared folders or sites.move-ip, and run after the site's removal — has
// nothing to render and ends done: a failure would count in the jobs line
// for a site that does not exist.
func TestSiteApplyOfAGoneSiteIsDone(t *testing.T) {
	f := newSiteFixture(t)
	site := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.deleteSite("a.example.com")
	id, err := f.s.enqueueSiteApply(f.ctx, site, "admin")
	if err != nil {
		t.Fatal(err)
	}
	job := f.waitJob(id)
	if job.Status != store.JobDone || job.Message != "the site is gone: nothing to apply" {
		t.Fatalf("an apply of a gone site: %s %q %s", job.Status, job.Message, job.Error)
	}
	if conf, ok := f.agent.File("/etc/nginx/monopanel/sites/a.example.com.conf"); ok {
		t.Fatalf("the apply of a gone site wrote its server block:\n%s", conf)
	}
}

// A removal that finds its site removed already — a site.delete asked for
// twice, or one that a user.delete of the site's account got ahead of — is
// done, not failed; so is a user.delete whose site went by its own
// site.delete while it waited for the site's lock.
func TestRemovalsOfAGoneSiteAreDone(t *testing.T) {
	f := newSiteFixture(t)
	a := f.createSite(map[string]any{"domain": "a.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.deleteSite("a.example.com")
	again, err := f.s.jobs.Enqueue(f.ctx, "site.delete", sitePayload{SiteID: a.ID, Purge: true}, jobs.WithLockKey("site:a.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if job := f.waitJob(again.ID); job.Status != store.JobDone || !strings.HasPrefix(job.Message, "the site is gone already") {
		t.Fatalf("a site.delete of a gone site: %s %q %s", job.Status, job.Message, job.Error)
	}

	b := f.createSite(map[string]any{"domain": "b.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/paths/remove" && strings.Contains(string(body), `"/etc/nginx/monopanel/sites/b.example.com.conf"`) {
			once.Do(func() {
				if err := f.db.DeleteSite(f.ctx, b.ID); err != nil {
					t.Error(err)
				}
			})
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex", nil, http.StatusAccepted, &del)
	if job := f.waitJob(del.JobID); job.Status != store.JobDone {
		t.Fatalf("user.delete whose site went meanwhile: %s %s", job.Status, job.Error)
	}
}

// A mail.webmail runs under the lock key "mail", in a site and for an
// account. Once a removal took either of them away after the job — queued
// after it, or over after it started — its failure is history, whatever it
// failed of: the account's user.delete, the site's site.delete, the
// user.delete of another account whose site it was. Not for the removal of
// another account or site, nor for one queued before the job and over
// before it started (it ran on an account made again under the name
// meanwhile); nor for the removal of the account it named while the site
// is there as another account's, which the account it named was never the
// owner of. A later webmail that went through still fixes it, a webmail
// that went through fixes no failed removal, and a payload without the
// domain and the user counts as before.
func TestDoctorJobsLineForgetsAWebmailWhoseSiteOrAccountWent(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time {
		at := now.Add(-d)
		return &at
	}
	// Queued an hour ago; a failed job ran from 50 to 49 minutes ago, a
	// job that went through ended the given time ago.
	failedJob := func(id int64, typ, lock, payload string) *store.Job {
		return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(payload), Status: store.JobFailed, CreatedAt: store.Time(now.Add(-time.Hour)), StartedAt: ago(50 * time.Minute), FinishedAt: ago(49 * time.Minute)}
	}
	doneJob := func(id int64, typ, lock, payload string, ended time.Duration) *store.Job {
		return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(payload), Status: store.JobDone, CreatedAt: store.Time(now.Add(-time.Hour)), StartedAt: ago(ended + time.Minute), FinishedAt: ago(ended)}
	}
	webmail := failedJob(5, "mail.webmail", "mail", `{"domain":"webmail.example.com","user":"alex"}`)
	for _, c := range []struct {
		name  string
		done  []*store.Job
		sites []string // there now
		want  int
	}{
		{"nothing since", nil, nil, 1},
		{"its account removed", []*store.Job{doneJob(6, "user.delete", "user:alex", `{"user_id":2,"login":"alex","purge":true}`, 10*time.Minute)}, nil, 0},
		{"its site removed", []*store.Job{doneJob(6, "site.delete", "site:webmail.example.com", `{"site_id":3,"purge":true}`, 10*time.Minute)}, nil, 0},
		{"its site removed with another account, the site's owner", []*store.Job{doneJob(6, "user.delete", "user:kate", `{"user_id":3,"login":"kate","sites":["webmail.example.com"]}`, 10*time.Minute)}, nil, 0},
		{"another account removed", []*store.Job{doneJob(6, "user.delete", "user:kate", `{"user_id":3,"login":"kate","sites":["kate.example.com"]}`, 10*time.Minute)}, nil, 1},
		{"another site removed", []*store.Job{doneJob(6, "site.delete", "site:kate.example.com", `{"site_id":4}`, 10*time.Minute)}, nil, 1},
		// The removal was asked for first, the webmail meanwhile: it failed
		// as the account was being removed, under another lock key.
		{"its account's removal queued before it and over after it started", []*store.Job{doneJob(4, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`, 10*time.Minute)}, nil, 0},
		// Queued before the removal, it waited behind another mail job and
		// started once the removal was over: it failed on an account that
		// was gone.
		{"its account removed after it was queued, before it started", []*store.Job{doneJob(6, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`, 55*time.Minute)}, nil, 0},
		// Queued after the removal and started after it: on the account
		// made again under the name.
		{"its account removed before it was queued", []*store.Job{doneJob(4, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`, 55*time.Minute)}, nil, 1},
		// The site is there, another account's: the run went into it, and
		// the account the request named was none of its business.
		{"the account it named removed, its site there", []*store.Job{doneJob(6, "user.delete", "user:alex", `{"user_id":2,"login":"alex","sites":["alex.example.com"]}`, 10*time.Minute)}, []string{"kate.example.com", "webmail.example.com"}, 1},
		{"the account it named removed, other sites there", []*store.Job{doneJob(6, "user.delete", "user:alex", `{"user_id":2,"login":"alex","sites":["alex.example.com"]}`, 10*time.Minute)}, []string{"kate.example.com"}, 0},
		{"its site removed, though a site of the name is there again", []*store.Job{doneJob(6, "site.delete", "site:webmail.example.com", `{"site_id":3}`, 10*time.Minute)}, []string{"webmail.example.com"}, 0},
		{"a later webmail went through", []*store.Job{doneJob(7, "mail.webmail", "mail", `{"domain":"other.example.com","user":"kate"}`, 10*time.Minute)}, nil, 0},
	} {
		if got := openFailures([]*store.Job{webmail}, c.done, now, c.sites...); got != c.want {
			t.Errorf("%s: %d open, want %d", c.name, got, c.want)
		}
	}
	removals := []*store.Job{
		doneJob(6, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`, 10*time.Minute),
		doneJob(7, "site.delete", "site:webmail.example.com", `{"site_id":3}`, 10*time.Minute),
	}
	if got := openFailures([]*store.Job{failedJob(5, "mail.webmail", "mail", `{}`)}, removals, now); got != 1 {
		t.Errorf("a payload without the domain and the user: %d open", got)
	}
	// Each only one way: a webmail that went through removes nothing.
	if got := openFailures([]*store.Job{failedJob(5, "site.delete", "site:webmail.example.com", `{"site_id":3}`)}, []*store.Job{doneJob(6, "mail.webmail", "mail", `{"domain":"webmail.example.com","user":"alex"}`, 10*time.Minute)}, now); got != 1 {
		t.Errorf("a failed removal of the webmail's site after a webmail went through: %d open", got)
	}
	// Jobs without the times go by their ids.
	bare := func(id int64, typ, lock, payload string) *store.Job {
		return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(payload), CreatedAt: store.Time(now.Add(-time.Hour))}
	}
	untimed := bare(5, "mail.webmail", "mail", `{"domain":"webmail.example.com","user":"alex"}`)
	if got := openFailures([]*store.Job{untimed}, []*store.Job{bare(6, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`)}, now); got != 0 {
		t.Errorf("an untimed removal queued after: %d open", got)
	}
	if got := openFailures([]*store.Job{untimed}, []*store.Job{bare(4, "user.delete", "user:alex", `{"user_id":2,"login":"alex"}`)}, now); got != 1 {
		t.Errorf("an untimed removal queued before: %d open", got)
	}
}

// A backup.run that failed failed on its target — the repository did not
// answer, its disk was full, its password was wrong — whatever it was to
// save: the removal of the account or the site it was to save afterwards
// fixes nothing on the target, and the scope's newest data is gone with it.
// The failure counts until a later backup to the target goes through.
func TestDoctorJobsLineKeepsTheBackupOfAGoneScope(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time {
		at := now.Add(-d)
		return &at
	}
	backup := func(id int64, scope string) *store.Job {
		return &store.Job{ID: id, Type: "backup.run", LockKey: "backup:local", Payload: json.RawMessage(`{"target_id":1,"scope":"` + scope + `"}`), Status: store.JobFailed, CreatedAt: store.Time(now.Add(-time.Hour)), StartedAt: ago(50 * time.Minute), FinishedAt: ago(49 * time.Minute)}
	}
	doneJob := func(id int64, typ, lock, payload string, ended time.Duration) *store.Job {
		return &store.Job{ID: id, Type: typ, LockKey: lock, Payload: json.RawMessage(payload), Status: store.JobDone, CreatedAt: store.Time(now.Add(-time.Hour)), StartedAt: ago(ended + time.Minute), FinishedAt: ago(ended)}
	}
	failed := []*store.Job{backup(1, "user:alex"), backup(2, "site:a.example.com"), backup(3, "server"), backup(4, "db:alex_wp")}
	for _, c := range []struct {
		name string
		done []*store.Job
		want int
	}{
		{"nothing gone", nil, 4},
		{"the account removed", []*store.Job{doneJob(10, "user.delete", "user:alex", `{"user_id":2,"login":"alex","purge":true}`, 10*time.Minute)}, 4},
		{"the account removed with the site", []*store.Job{doneJob(10, "user.delete", "user:alex", `{"user_id":2,"login":"alex","purge":true,"sites":["a.example.com"]}`, 10*time.Minute)}, 4},
		{"the site purged", []*store.Job{doneJob(10, "site.delete", "site:a.example.com", `{"site_id":1,"purge":true}`, 10*time.Minute)}, 4},
		{"another account removed", []*store.Job{doneJob(10, "user.delete", "user:kate", `{"user_id":3,"login":"kate","sites":["kate.example.com"]}`, 10*time.Minute)}, 4},
		// What clears it: any backup to the target that went through later.
		{"a later backup went through", []*store.Job{doneJob(11, "backup.run", "backup:local", `{"target_id":1,"scope":"server"}`, 10*time.Minute)}, 0},
		{"an earlier backup went through", []*store.Job{doneJob(0, "backup.run", "backup:local", `{"target_id":1,"scope":"server"}`, 55*time.Minute)}, 4},
		{"a later backup to another target went through", []*store.Job{doneJob(11, "backup.run", "backup:offsite", `{"target_id":2,"scope":"server"}`, 10*time.Minute)}, 4},
	} {
		if got := openFailures(failed, c.done, now); got != c.want {
			t.Errorf("%s: %d open, want %d", c.name, got, c.want)
		}
	}
}

// A webmail asked for once its account's removal was queued fails, the
// account being removed, and counts in the doctor's jobs line until the
// removal went through — then it is history, though the removal was queued
// before it and ran under another lock key.
func TestJobsLineForgetsAWebmailRefusedForAnAccountBeingRemoved(t *testing.T) {
	f, _, _ := newWebmailFixture(t)
	if got := f.doctorCheck("jobs"); !strings.HasPrefix(got, "ok:") {
		t.Fatalf("jobs line before: %q", got)
	}
	atRemove, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	letGo := func() { released.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	f.agent.SetBefore(func(p string, _ []byte) {
		if p == "/v1/user/remove" {
			once.Do(func() {
				close(atRemove)
				<-release
			})
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	var del apitypes.JobRef
	f.call(http.MethodDelete, "/users/alex?purge=true", nil, http.StatusAccepted, &del)
	select {
	case <-atRemove:
	case <-time.After(20 * time.Second):
		t.Fatal("the removal did not get to the unix account")
	}
	job := f.webmailJob(map[string]any{"domain": "webmail.example.com", "user": "alex"})
	if job.Status != store.JobFailed || job.Error != "the account alex is being removed: the webmail is not installed for it" || job.ID < del.JobID {
		t.Fatalf("the webmail for an account being removed: #%d %s %q (the removal #%d)", job.ID, job.Status, job.Error, del.JobID)
	}
	if got := f.doctorCheck("jobs"); !strings.HasPrefix(got, "warn: 1 failed") {
		t.Fatalf("jobs line with the refused webmail: %q", got)
	}
	letGo()
	if j := f.waitJob(del.JobID); j.Status != store.JobDone {
		t.Fatalf("user.delete: %s %s", j.Status, j.Error)
	}
	if got := f.doctorCheck("jobs"); !strings.HasPrefix(got, "ok:") {
		t.Fatalf("jobs line after the removal went through: %q", got)
	}
}
