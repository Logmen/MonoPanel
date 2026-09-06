package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "panel.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := db.SchemaVersion(ctx)
	db.Close()
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	v2, _ := db2.SchemaVersion(ctx)
	if v == 0 || v != v2 {
		t.Fatalf("schema version %d -> %d", v, v2)
	}
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex", Role: RoleAdmin, PasswordHash: "x"}
	if err := db.CreateUser(ctx, u); err != nil || u.ID == 0 {
		t.Fatalf("create: %v id=%d", err, u.ID)
	}
	if err := db.CreateUser(ctx, &User{Login: "alex"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate should be ErrExists, got %v", err)
	}
	got, err := db.GetUserByLogin(ctx, "alex")
	if err != nil || got.Role != RoleAdmin || got.PasswordHash != "x" || got.Status != UserActive {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := db.SetUserUnix(ctx, u.ID, 1001, 1001, "/var/www/alex"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetUserByUnixUID(ctx, 1001)
	if got == nil || got.Home != "/var/www/alex" || *got.UnixGID != 1001 {
		t.Fatalf("unix mapping: %+v", got)
	}
	if _, err := db.GetUserByLogin(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	counts, _ := db.CountUsers(ctx)
	if counts[RoleAdmin] != 1 {
		t.Fatalf("counts %v", counts)
	}
}

func TestJobsClaimAndLock(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	a := &Job{Type: "site.apply", LockKey: "site:1", RequestedBy: "alex"}
	b := &Job{Type: "site.apply", LockKey: "site:1"}
	c := &Job{Type: "user.provision", LockKey: "user:2"}
	for _, j := range []*Job{a, b, c} {
		if err := db.EnqueueJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.ClaimJob(ctx, "w1")
	if err != nil || got == nil || got.ID != a.ID || got.Status != JobRunning {
		t.Fatalf("first claim: %+v %v", got, err)
	}
	got2, _ := db.ClaimJob(ctx, "w2")
	if got2 == nil || got2.ID != c.ID {
		t.Fatalf("second claim must skip locked site:1 and take user job, got %+v", got2)
	}
	if got3, _ := db.ClaimJob(ctx, "w3"); got3 != nil {
		t.Fatalf("third claim should find nothing runnable, got %+v", got3)
	}
	db.AppendJobLog(ctx, a.ID, "step 1\n")
	db.UpdateJobProgress(ctx, a.ID, 50, "half")
	db.FinishJob(ctx, a.ID, JobDone, "")
	fin, _ := db.GetJob(ctx, a.ID)
	if fin.Status != JobDone || fin.Progress != 100 || !strings.Contains(fin.Log, "step 1") || fin.FinishedAt == nil {
		t.Fatalf("finished job: %+v", fin)
	}
	got4, _ := db.ClaimJob(ctx, "w4")
	if got4 == nil || got4.ID != b.ID {
		t.Fatalf("after finishing a, b must be claimable, got %+v", got4)
	}
	db.FinishJob(ctx, b.ID, JobFailed, "boom")
	n, _ := db.RequeueStaleJobs(ctx)
	if n != 1 {
		t.Fatalf("requeue: %d (only c was running)", n)
	}
	counts, _ := db.CountJobs(ctx)
	if counts[JobQueued] != 1 || counts[JobDone] != 1 || counts[JobFailed] != 1 {
		t.Fatalf("counts %v", counts)
	}
	list, _ := db.ListJobs(ctx, 10, "")
	if len(list) != 3 || list[0].ID != c.ID {
		t.Fatalf("list %v", list)
	}
}

func TestJobIdempotency(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	j1 := &Job{Type: "x", IdempotencyKey: "k1"}
	j2 := &Job{Type: "x", IdempotencyKey: "k1"}
	db.EnqueueJob(ctx, j1)
	db.EnqueueJob(ctx, j2)
	if j1.ID != j2.ID {
		t.Fatalf("idempotency key must return the same job: %d %d", j1.ID, j2.ID)
	}
}

func TestSessionsTokensSettings(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	s := &Session{ID: "sess1", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetSession(ctx, "sess1"); err != nil || got.UserID != u.ID {
		t.Fatalf("session: %+v %v", got, err)
	}
	old := &Session{ID: "old", UserID: u.ID, ExpiresAt: time.Now().Add(-time.Hour)}
	db.CreateSession(ctx, old)
	if _, err := db.GetSession(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session returned: %v", err)
	}
	if n, _ := db.DeleteExpiredSessions(ctx); n != 1 {
		t.Fatalf("purged %d", n)
	}
	tok := &APIToken{UserID: u.ID, Name: "whmcs", Hash: "h1", Scopes: []string{"users:rw"}}
	if err := db.CreateAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAPITokenByHash(ctx, "h1")
	if err != nil || got.Scopes[0] != "users:rw" {
		t.Fatalf("token: %+v %v", got, err)
	}
	if err := db.SetSetting(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	db.SetSetting(ctx, "k", "v2")
	if v, _ := db.GetSetting(ctx, "k"); v != "v2" {
		t.Fatalf("setting %q", v)
	}
	db.Audit(ctx, AuditEntry{Actor: "alex", Action: "user.create", Target: "bob", Details: map[string]any{"role": "user"}})
	entries, _ := db.ListAudit(ctx, 10)
	if len(entries) != 1 || entries[0].Details["role"] != "user" {
		t.Fatalf("audit %+v", entries)
	}
	if err := db.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAPITokenByHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("token must cascade on user delete")
	}
}
