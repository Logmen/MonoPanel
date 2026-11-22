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
