package api

import (
	"net/http"
	"testing"
	"time"

	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/store"
)

// The install of an update restarts the API, and a backup, a restore or a
// migration it met would run again from its start, a migration not at all.
// The automatic update waits while such a job is queued or running and goes
// in at a later round, once none is; jobs of other types do not hold it.
// Without automatic installs the check goes on as before.
func TestTheAutomaticUpdateWaitsForALongJob(t *testing.T) {
	// Set before the fixture's job runner starts, put back after it stopped.
	running := buildinfo.Version
	buildinfo.Version = "9.9.7"
	t.Cleanup(func() { buildinfo.Version = running })
	rel := newRelease(t, "9.9.9")
	f := newFixture(t, func(c *config.Config) { c.Update.PublicKey = rel.pub })
	f.configureUpdates(rel)
	f.call(http.MethodPut, "/system/update", map[string]any{"auto_apply": true}, http.StatusOK, nil)
	// A job as the runner leaves one, without running anything: the rows
	// hold their lock keys as the runner's own would.
	insert := func(typ, lock, status string) int64 {
		t.Helper()
		at := time.Now().UTC().Format(time.RFC3339Nano)
		var id int64
		if err := f.db.SQL().QueryRowContext(f.ctx, `INSERT INTO jobs(type, status, lock_key, created_at, started_at) VALUES(?,?,?,?,?) RETURNING id`, typ, status, lock, at, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	finish := func(id int64) {
		t.Helper()
		if err := f.db.FinishJob(f.ctx, id, store.JobDone, ""); err != nil {
			t.Fatal(err)
		}
	}
	// The scheduler's update jobs so far; each, until it is over, may write
	// back the settings it read when it started (its own check).
	updates := func() []*store.Job {
		t.Helper()
		list, err := f.db.ListJobsFiltered(f.ctx, store.JobFilter{Type: "panel.update", RequestedBy: "scheduler", Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	settled := func() {
		t.Helper()
		for _, j := range updates() {
			f.waitJob(j.ID)
		}
	}
	// One round of the scheduler, a check due: the update jobs it queued, and
	// whether it checked.
	tick := func() (int, bool) {
		t.Helper()
		settled()
		c := f.s.loadUpdateConfig(f.ctx)
		c.CheckedAt = nil
		if err := f.s.saveUpdateConfig(f.ctx, c); err != nil {
			t.Fatal(err)
		}
		before := len(updates())
		f.s.updateTick(f.ctx)
		queued, checked := len(updates())-before, f.s.loadUpdateConfig(f.ctx).CheckedAt != nil
		return queued, checked
	}

	for _, typ := range []string{"migrate.run", "backup.run", "backup.restore"} {
		long := insert(typ, "user:alex2", store.JobRunning)
		if n, checked := tick(); n != 0 || checked {
			t.Fatalf("the automatic update beside a running %s: %d queued, checked %v", typ, n, checked)
		}
		finish(long)
		if n, _ := tick(); n != 1 {
			t.Fatalf("the automatic update once the %s is over: %d queued, want one", typ, n)
		}
	}
	// One that waits in the queue — behind its account's provision — would
	// start beside the install.
	provision := insert("user.provision", "user:kate", store.JobRunning)
	queued := insert("migrate.run", "user:kate", store.JobQueued)
	if n, checked := tick(); n != 0 || checked {
		t.Fatalf("the automatic update beside a queued migration: %d queued, checked %v", n, checked)
	}
	if _, err := f.db.SQL().ExecContext(f.ctx, `UPDATE jobs SET status=? WHERE id=?`, store.JobCancelled, queued); err != nil {
		t.Fatal(err)
	}
	// A job of another type holds nothing.
	if n, _ := tick(); n != 1 {
		t.Fatalf("the automatic update beside a running provision: %d queued, want one", n)
	}
	finish(provision)

	// Without automatic installs the check goes on, a migration running or not.
	settled()
	f.call(http.MethodPut, "/system/update", map[string]any{"auto_apply": false}, http.StatusOK, nil)
	insert("migrate.run", "user:bob", store.JobRunning)
	if n, checked := tick(); n != 0 || !checked {
		t.Fatalf("no automatic installs, a migration running: %d queued, checked %v", n, checked)
	}
}
