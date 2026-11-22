package store

import (
	"context"
	"testing"
)

// The spare worker's claim takes no job of the long types, and none at all
// until busy of them run; it keeps to the lock keys and the exclusive groups
// as every claim does, and it takes the oldest of what it may.
func TestClaimSpareJob(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	long := []string{"backup.run", "migrate.run"}
	enqueue := func(typ, lock string) *Job {
		t.Helper()
		j := &Job{Type: typ, LockKey: lock}
		if err := db.EnqueueJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	spare := func() *Job {
		t.Helper()
		j, err := db.ClaimSpareJob(ctx, "w3", long, 2, []string{"stack.upgrade", "panel.update"})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	m1, m2 := enqueue("migrate.run", "user:alex"), enqueue("backup.run", "backup:local")
	upgrade := enqueue("stack.upgrade", "stack")
	apply := enqueue("site.apply", "site:a.example.com")
	held := enqueue("site.apply", "site:a.example.com")
	update := enqueue("panel.update", "panel:update")
	if got := spare(); got != nil {
		t.Fatalf("the spare with no long job running: %+v", got)
	}
	if got, _ := db.ClaimJob(ctx, "w1"); got == nil || got.ID != m1.ID {
		t.Fatalf("w1: %+v", got)
	}
	if got := spare(); got != nil {
		t.Fatalf("the spare beside one long job of two workers: %+v", got)
	}
	if got, _ := db.ClaimJob(ctx, "w2"); got == nil || got.ID != m2.ID {
		t.Fatalf("w2: %+v", got)
	}
	for _, want := range []*Job{upgrade, apply} {
		if got := spare(); got == nil || got.ID != want.ID || got.Worker != "w3" || got.Status != JobRunning {
			t.Fatalf("the spare beside two long jobs: %+v, want #%d", got, want.ID)
		}
	}
	// held waits for apply's lock key, update for the upgrade (exclusive).
	if got := spare(); got != nil {
		t.Fatalf("the spare past a held lock key and an exclusive group: %+v", got)
	}
	enqueue("migrate.run", "user:kate")
	if got := spare(); got != nil {
		t.Fatalf("the spare took a long job: %+v", got)
	}
	db.FinishJob(ctx, apply.ID, JobDone, "")
	db.FinishJob(ctx, upgrade.ID, JobDone, "")
	for _, want := range []*Job{held, update} {
		if got := spare(); got == nil || got.ID != want.ID {
			t.Fatalf("the spare after the lock key and the group were let go: %+v, want #%d", got, want.ID)
		}
	}
	if got, err := db.ClaimSpareJob(ctx, "w3", nil, 0); got != nil || err != nil {
		t.Fatalf("a spare with no long types: %+v %v", got, err)
	}
}
