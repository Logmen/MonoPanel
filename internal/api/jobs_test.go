package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

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
