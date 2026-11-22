package jobs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"monopanel/internal/store"
)

// spareRunner is a runner of n workers whose default limit, shortened, stands
// for the hour; migrate.run, a long type, and site.apply run until released
// (site.apply at most for the default), cert.issue goes through at once.
func spareRunner(t *testing.T, n int) (*Runner, *store.DB, context.Context, func(), chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := NewRunner(db, n, nil)
	r.timeout = 5 * time.Second
	release := make(chan struct{})
	hold := func(ctx context.Context, _ *Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.Register("migrate.run", hold)
	r.Register("site.apply", hold)
	r.Register("cert.issue", func(context.Context, *Context) error { return nil })
	r.SetLimit("migrate.run", time.Minute) // a day: 4 × the agent's 6 h stream limit
	stop := func() {
		cancel()
		r.Wait()
	}
	return r, db, ctx, stop, release
}

// started waits for a job to start and says when it did and on which worker.
func started(t *testing.T, db *store.DB, ctx context.Context, id int64) *store.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := db.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.StartedAt != nil {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d (%s) never started: %s", id, got.Type, got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two migrations hold both workers for as long as they run, hours. A job of
// another type queued meanwhile — a site's apply, a certificate — does not
// wait for them: the spare worker takes it at once, though a migration
// queued before it waits for a worker of the two. When a migration is over
// the next one takes its worker, and the spare goes on with the others.
func TestAShortJobDoesNotWaitBehindLongOnes(t *testing.T) {
	r, db, ctx, stop, release := spareRunner(t, 2)
	defer stop()
	r.Start(ctx)
	enqueue := func(typ, lock string) *store.Job {
		t.Helper()
		j, err := r.Enqueue(ctx, typ, nil, WithLockKey(lock))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	m1, m2 := enqueue("migrate.run", "user:alex"), enqueue("migrate.run", "user:bob")
	started(t, db, ctx, m1.ID)
	started(t, db, ctx, m2.ID)
	m3 := enqueue("migrate.run", "user:kate")
	cert := enqueue("cert.issue", "cert:b.example.com")
	apply := enqueue("site.apply", "site:a.example.com")
	for _, j := range []*store.Job{cert, apply} {
		got := started(t, db, ctx, j.ID)
		if waited := got.StartedAt.Sub(got.CreatedAt.Std()); waited > 2*time.Second || got.Worker != "w3" {
			t.Fatalf("%s behind two migrations: started on %s after %s (the default limit %s, theirs %s)", got.Type, got.Worker, waited.Round(time.Millisecond), r.timeout, r.Limit("migrate.run"))
		}
	}
	if got, _ := db.GetJob(ctx, m3.ID); got.Status != store.JobQueued {
		t.Fatalf("a third migration beside two: %s on %s", got.Status, got.Worker)
	}
	// One job at a time on the spare, as on any worker.
	second := enqueue("site.apply", "site:c.example.com")
	time.Sleep(500 * time.Millisecond)
	if got, _ := db.GetJob(ctx, second.ID); got.Status != store.JobQueued {
		t.Fatalf("a second apply while the spare runs one: %s on %s", got.Status, got.Worker)
	}
	close(release)
	got := started(t, db, ctx, m3.ID)
	if got.Worker == "w3" {
		t.Fatalf("the third migration ran on the spare")
	}
	started(t, db, ctx, second.ID)
}

// While the long jobs leave a worker of the n, the spare takes nothing: n
// jobs run at a time, as jobs.workers says. With no type of a long limit
// there is no spare at all.
func TestTheSpareWaitsForEveryWorkerToHoldALongJob(t *testing.T) {
	r, db, ctx, stop, release := spareRunner(t, 2)
	defer stop()
	r.Start(ctx)
	m, err := r.Enqueue(ctx, "migrate.run", nil, WithLockKey("user:alex"))
	if err != nil {
		t.Fatal(err)
	}
	started(t, db, ctx, m.ID)
	a, _ := r.Enqueue(ctx, "site.apply", nil, WithLockKey("site:a.example.com"))
	b, _ := r.Enqueue(ctx, "site.apply", nil, WithLockKey("site:b.example.com"))
	if got := started(t, db, ctx, a.ID); got.Worker == "w3" {
		t.Fatalf("an apply beside one migration went to the spare")
	}
	time.Sleep(500 * time.Millisecond)
	if got, _ := db.GetJob(ctx, b.ID); got.Status != store.JobQueued {
		t.Fatalf("a third job beside a migration and an apply, two workers: %s on %s", got.Status, got.Worker)
	}
	close(release)
	started(t, db, ctx, b.ID)

	plain, db2, ctx2, stop2, release2 := spareRunner(t, 1)
	defer stop2()
	plain.SetLimit("migrate.run", 0)
	plain.Start(ctx2)
	x, _ := plain.Enqueue(ctx2, "site.apply", nil, WithLockKey("site:x.example.com"))
	y, _ := plain.Enqueue(ctx2, "site.apply", nil, WithLockKey("site:y.example.com"))
	started(t, db2, ctx2, x.ID)
	time.Sleep(500 * time.Millisecond)
	if got, _ := db2.GetJob(ctx2, y.ID); got.Status != store.JobQueued {
		t.Fatalf("one worker and no long type: a second job %s on %s", got.Status, got.Worker)
	}
	close(release2)
	started(t, db2, ctx2, y.ID)
}
