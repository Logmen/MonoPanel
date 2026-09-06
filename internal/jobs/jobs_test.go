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
