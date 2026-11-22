package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

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
