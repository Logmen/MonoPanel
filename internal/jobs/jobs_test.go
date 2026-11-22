package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monopanel/internal/store"
)

func waitFor(t *testing.T, ch <-chan Event, typ string) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Type == typ {
				return e
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", typ)
		}
	}
}

// A job cut off by a shutdown is not a failure: it stays running and the
// next start of the panel queues it again.
func TestShutdownRequeuesRunningJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	started := make(chan struct{})
	r := NewRunner(db, 1, nil)
	r.Register("slow", func(ctx context.Context, jc *Context) error {
		close(started)
		<-ctx.Done()
		return errors.New("agent unreachable: " + ctx.Err().Error())
	})
	r.Start(ctx)
	j, err := r.Enqueue(ctx, "slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	r.Wait()
	got, _ := db.GetJob(context.Background(), j.ID)
	if got.Status != store.JobRunning || !strings.Contains(got.Log, "interrupted by shutdown") {
		t.Fatalf("после остановки: %s %q", got.Status, got.Error)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	r2 := NewRunner(db, 1, nil)
	r2.Register("slow", func(context.Context, *Context) error { return nil })
	all, stop := r2.Broker().Subscribe(0, 16)
	defer stop()
	r2.Start(ctx2)
	waitFor(t, all, "done")
	if got, _ := db.GetJob(ctx2, j.ID); got.Status != store.JobDone {
		t.Fatalf("после перезапуска: %s", got.Status)
	}
	cancel2()
	r2.Wait()
}

func TestRunnerLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRunner(db, 2, nil)
	r.Register("ok", func(ctx context.Context, jc *Context) error {
		var p struct{ Name string }
		jc.Unmarshal(&p)
		jc.Progress(50, "half")
		jc.Logf("hello %s", p.Name)
		return nil
	})
	r.Register("boom", func(ctx context.Context, jc *Context) error { return errors.New("kaboom") })
	r.Register("panic", func(ctx context.Context, jc *Context) error { panic("oops") })
	r.Start(ctx)

	all, stop := r.Broker().Subscribe(0, 64)
	defer stop()
	j, err := r.Enqueue(ctx, "ok", map[string]string{"Name": "world"}, WithRequestedBy("alex"))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, all, "done")
	got, _ := db.GetJob(ctx, j.ID)
	if got.Status != store.JobDone || !strings.Contains(got.Log, "hello world") || got.Message != "half" {
		t.Fatalf("job %+v", got)
	}

	jb, _ := r.Enqueue(ctx, "boom", nil)
	e := waitFor(t, all, "failed")
	if e.JobID != jb.ID || e.Error != "kaboom" {
		t.Fatalf("failed event %+v", e)
	}
	jp, _ := r.Enqueue(ctx, "panic", nil)
	waitFor(t, all, "failed")
	got, _ = db.GetJob(ctx, jp.ID)
	if !strings.Contains(got.Error, "panic: oops") {
		t.Fatalf("panic not captured: %+v", got)
	}
	if _, err := r.Enqueue(ctx, "unknown", nil); err == nil {
		t.Fatal("unknown type must be rejected")
	}
	cancel()
	r.Wait()
}

// The daily upgrade and the panel's update never run together, in either
// order, though free workers wait and their lock keys differ: the one that
// comes second stays queued until the first is over and then runs — it does
// not fail. A job of another type runs beside either.
func TestExclusiveJobsWaitForEachOther(t *testing.T) {
	for _, order := range [][2]string{{"stack.upgrade", "panel.update"}, {"panel.update", "stack.upgrade"}} {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel) // a failure ends the handlers it holds
		db, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
		if err != nil {
			t.Fatal(err)
		}
		r := NewRunner(db, 3, nil)
		release := map[string]chan struct{}{}
		started := make(chan string, 8)
		for _, typ := range []string{"stack.upgrade", "panel.update", "site.apply"} {
			release[typ] = make(chan struct{})
			r.Register(typ, func(ctx context.Context, jc *Context) error {
				started <- jc.Type
				select {
				case <-release[jc.Type]:
				case <-ctx.Done():
				}
				return nil
			})
		}
		next := func() string {
			select {
			case typ := <-started:
				return typ
			case <-time.After(5 * time.Second):
				return "nothing"
			}
		}
		r.Exclusive("stack.upgrade", "panel.update")
		all, stop := r.Broker().Subscribe(0, 64)
		r.Start(ctx)
		keys := map[string]string{"stack.upgrade": "stack", "panel.update": "panel:update"}
		first, err := r.Enqueue(ctx, order[0], nil, WithLockKey(keys[order[0]]))
		if err != nil {
			t.Fatal(err)
		}
		if got := next(); got != order[0] {
			t.Fatalf("%v: started %s", order, got)
		}
		second, _ := r.Enqueue(ctx, order[1], nil, WithLockKey(keys[order[1]]))
		r.Enqueue(ctx, "site.apply", nil, WithLockKey("site:1")) //nolint:errcheck // registered above
		if got := next(); got != "site.apply" {
			t.Fatalf("%v: started %s beside %s", order, got, order[0])
		}
		time.Sleep(1500 * time.Millisecond) // the workers' ticks claim what they may
		if got, _ := db.GetJob(ctx, second.ID); got.Status != store.JobQueued {
			t.Fatalf("%v: %s %s while %s runs", order, order[1], got.Status, order[0])
		}
		close(release[order[0]])
		if got := next(); got != order[1] {
			t.Fatalf("%v: started %s", order, got)
		}
		close(release[order[1]])
		close(release["site.apply"])
		for done := 0; done < 3; done++ {
			waitFor(t, all, "done")
		}
		a, _ := db.GetJob(ctx, first.ID)
		b, _ := db.GetJob(ctx, second.ID)
		if a.Status != store.JobDone || b.Status != store.JobDone || b.StartedAt.Before(*a.FinishedAt) {
			t.Fatalf("%v: %s %s→%v, %s %s from %v", order, a.Type, a.Status, a.FinishedAt, b.Type, b.Status, b.StartedAt)
		}
		stop()
		cancel()
		r.Wait()
		db.Close()
	}
}

// Held workers claim nothing until the hold is over — a job queued before
// the API's agent listens is not claimed only to fail on "agent unreachable"
// — and then run what waits; a runner stopped while held simply ends.
func TestHeldWorkersClaimNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	release := make(chan struct{})
	r := NewRunner(db, 2, nil)
	r.Register("ok", func(context.Context, *Context) error { return nil })
	r.Hold(func(ctx context.Context) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	all, stop := r.Broker().Subscribe(0, 16)
	defer stop()
	r.Start(ctx)
	j, err := r.Enqueue(ctx, "ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got, _ := db.GetJob(ctx, j.ID); got.Status != store.JobQueued {
		t.Fatalf("claimed while held: %s", got.Status)
	}
	close(release)
	waitFor(t, all, "done")
	if got, _ := db.GetJob(ctx, j.ID); got.Status != store.JobDone {
		t.Fatalf("after the hold: %s", got.Status)
	}
	cancel()
	r.Wait()

	ctx2, cancel2 := context.WithCancel(context.Background())
	r2 := NewRunner(db, 1, nil)
	r2.Hold(func(ctx context.Context) { <-ctx.Done() })
	r2.Start(ctx2)
	cancel2()
	done := make(chan struct{})
	go func() { r2.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a held runner does not stop")
	}
}
