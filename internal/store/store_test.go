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

// An account being removed keeps its mark: the end of a provision, a move
// or a change of the profile that read the account before the mark leaves
// it — and says so — while the removal's own writes (its mark, the way back
// from its failure) go in.
func TestUserStatusOfAnAccountBeingRemoved(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex", Status: UserPending}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if status, err := db.ActivateUser(ctx, u.ID); err != nil || status != UserActive {
		t.Fatalf("activated: %s %v", status, err)
	}
	read, _ := db.GetUserByID(ctx, u.ID)
	if read.Status != UserActive {
		t.Fatalf("activated: %s", read.Status)
	}
	email, suspended := "alex@example.com", UserSuspended
	if err := db.UpdateUserChanges(ctx, u.ID, UserChanges{Email: &email, Status: &suspended}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetUserByID(ctx, u.ID); got.Email != "alex@example.com" || got.Status != UserSuspended {
		t.Fatalf("the profile: %q %s", got.Email, got.Status)
	}
	if err := db.SetUserStatus(ctx, u.ID, UserDeleting); err != nil {
		t.Fatal(err)
	}
	other, active, hash := "other@example.com", UserActive, "new-hash"
	if err := db.UpdateUserChanges(ctx, u.ID, UserChanges{Email: &other, Status: &active, PasswordHash: &hash}); !errors.Is(err, ErrBeingRemoved) {
		t.Fatalf("a profile written over the mark: %v", err)
	}
	if err := db.UpdateUserChanges(ctx, u.ID, UserChanges{}); !errors.Is(err, ErrBeingRemoved) {
		t.Fatalf("an empty change over the mark: %v", err)
	}
	if _, err := db.ActivateUser(ctx, u.ID); !errors.Is(err, ErrBeingRemoved) {
		t.Fatalf("activated over the mark: %v", err)
	}
	if got, _ := db.GetUserByID(ctx, u.ID); got.Status != UserDeleting || got.Email != "alex@example.com" || got.PasswordHash == hash {
		t.Fatalf("over the mark: %s %q, the password hash written %v", got.Status, got.Email, got.PasswordHash == hash)
	}
	if err := db.SetUserStatus(ctx, u.ID, UserSuspended); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetUserByID(ctx, u.ID); got.Status != UserSuspended {
		t.Fatalf("after the removal failed: %s", got.Status)
	}
	if err := db.UpdateUserChanges(ctx, 999, UserChanges{Email: &other}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the profile of a gone account: %v", err)
	}
	if _, err := db.ActivateUser(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a gone account activated: %v", err)
	}
}

// The end of a provision activates an account still pending and no other:
// a suspension — asked for before the provision ran, or together with the
// change of the shell it carries out — stays, and so does an active
// account; the status afterwards is said.
func TestActivateUserOnlyFromPending(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	for _, c := range []struct{ from, want string }{
		{UserPending, UserActive},
		{UserSuspended, UserSuspended},
		{UserActive, UserActive},
	} {
		u := &User{Login: "u" + c.from, Status: c.from}
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		status, err := db.ActivateUser(ctx, u.ID)
		got, _ := db.GetUserByID(ctx, u.ID)
		if err != nil || status != c.want || got.Status != c.want {
			t.Errorf("activate from %s: said %q %v, stored %s (want %s)", c.from, status, err, got.Status, c.want)
		}
	}
}

// A change of an account writes the fields it sets and nothing else: a
// password change that read the account before a suspension does not put
// the status back, and one that read it pending does not undo the
// provision's activation; the e-mail, the shell and the quota read with it
// are not written back either.
func TestUpdateUserChangesWritesOnlyWhatItSets(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	quota := 500
	u := &User{Login: "alex", Status: UserActive, Email: "alex@example.com", Shell: true, QuotaMB: &quota}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	// Meanwhile: the account is suspended, its e-mail, shell and quota change.
	if _, err := db.sql.ExecContext(ctx, `UPDATE users SET status=?, email=?, shell=0, quota_mb=700 WHERE id=?`, UserSuspended, "new@example.com", u.ID); err != nil {
		t.Fatal(err)
	}
	hash := "new-hash"
	if err := db.UpdateUserChanges(ctx, u.ID, UserChanges{PasswordHash: &hash}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetUserByID(ctx, u.ID)
	if got.PasswordHash != hash || got.Status != UserSuspended || got.Email != "new@example.com" || got.Shell || got.QuotaMB == nil || *got.QuotaMB != 700 {
		t.Errorf("after a password change: hash %v, status %s, e-mail %q, shell %v, quota %v", got.PasswordHash == hash, got.Status, got.Email, got.Shell, got.QuotaMB)
	}
	// A pending account activated meanwhile stays active.
	carol := &User{Login: "carol", Status: UserPending}
	if err := db.CreateUser(ctx, carol); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ActivateUser(ctx, carol.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUserChanges(ctx, carol.ID, UserChanges{PasswordHash: &hash}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetUserByID(ctx, carol.ID); got.Status != UserActive {
		t.Errorf("the provision's activation undone: %s", got.Status)
	}
	// A status asked for goes in over any but the removal's mark.
	active, shell := UserActive, true
	if err := db.UpdateUserChanges(ctx, u.ID, UserChanges{Status: &active, Shell: &shell}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetUserByID(ctx, u.ID); got.Status != UserActive || !got.Shell || got.Email != "new@example.com" {
		t.Errorf("a status and a shell asked for: %s %v %q", got.Status, got.Shell, got.Email)
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

// Jobs of an exclusive group never run together, whatever their lock keys
// and in either order: one that comes while another runs stays queued, the
// jobs queued after it are claimed past it, and it is claimed once the other
// is over.
func TestJobsClaimExclusive(t *testing.T) {
	ctx := context.Background()
	groups := [][]string{{"stack.upgrade", "panel.update"}}
	for _, order := range [][]string{{"stack.upgrade", "panel.update"}, {"panel.update", "stack.upgrade"}} {
		db := openTest(t)
		keys := map[string]string{"stack.upgrade": "stack", "panel.update": "panel:update"}
		first := &Job{Type: order[0], LockKey: keys[order[0]]}
		second := &Job{Type: order[1], LockKey: keys[order[1]]}
		other := &Job{Type: "site.apply", LockKey: "site:1"}
		stack := &Job{Type: "stack.install", LockKey: "stack"}
		for _, j := range []*Job{first, second, other, stack} {
			if err := db.EnqueueJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := db.ClaimJob(ctx, "w1", groups...); err != nil || got == nil || got.ID != first.ID {
			t.Fatalf("%v: first claim %+v %v", order, got, err)
		}
		// the other one of the group waits; a site's job does not, nor —
		// when the update runs — another stack job
		got, _ := db.ClaimJob(ctx, "w2", groups...)
		if got == nil || got.ID != other.ID {
			t.Fatalf("%v: second claim must pass the held one by, got %+v", order, got)
		}
		got, _ = db.ClaimJob(ctx, "w3", groups...)
		switch order[0] {
		case "stack.upgrade":
			if got != nil {
				t.Fatalf("%v: claimed %+v while the upgrade holds stack and the group", order, got)
			}
		default:
			if got == nil || got.ID != stack.ID {
				t.Fatalf("%v: the update holds no stack key: %+v", order, got)
			}
			db.FinishJob(ctx, stack.ID, JobDone, "")
		}
		db.FinishJob(ctx, first.ID, JobDone, "")
		if got, _ := db.ClaimJob(ctx, "w4", groups...); got == nil || got.ID != second.ID {
			t.Fatalf("%v: after the first, the second: %+v", order, got)
		}
		// without the group the two would have run side by side
		db2 := openTest(t)
		a, b := &Job{Type: order[0], LockKey: keys[order[0]]}, &Job{Type: order[1], LockKey: keys[order[1]]}
		db2.EnqueueJob(ctx, a)
		db2.EnqueueJob(ctx, b)
		db2.ClaimJob(ctx, "w1")
		if got, _ := db2.ClaimJob(ctx, "w2"); got == nil || got.ID != b.ID {
			t.Fatalf("%v: lock keys alone keep them apart: %+v", order, got)
		}
	}
}

// A search by type finds the jobs of that type however many others came
// after them, with the status filter as well.
func TestListJobsByType(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	restore := &Job{Type: "backup.restore", LockKey: "backup:local"}
	if err := db.EnqueueJob(ctx, restore); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := db.EnqueueJob(ctx, &Job{Type: "site.apply", LockKey: "site:1"}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := db.ListJobsFiltered(ctx, JobFilter{Type: "backup.restore", Status: JobQueued, Limit: 2})
	if err != nil || len(list) != 1 || list[0].ID != restore.ID {
		t.Fatalf("queued restores: %+v %v", list, err)
	}
	if list, _ := db.ListJobsFiltered(ctx, JobFilter{Type: "backup.restore", Status: JobRunning}); len(list) != 0 {
		t.Fatalf("running restores: %+v", list)
	}
	if list, _ := db.ListJobsFiltered(ctx, JobFilter{Limit: 2}); len(list) != 2 || list[0].Type != "site.apply" {
		t.Fatalf("without a type: %+v", list)
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
	entries, _ := db.ListAudit(ctx, AuditFilter{Limit: 10})
	if len(entries) != 1 || entries[0].Details["role"] != "user" {
		t.Fatalf("audit %+v", entries)
	}
	if byActor, _ := db.ListAudit(ctx, AuditFilter{Actor: "nobody"}); len(byActor) != 0 {
		t.Fatalf("actor filter: %+v", byActor)
	}
	if byAction, _ := db.ListAudit(ctx, AuditFilter{Action: "user."}); len(byAction) != 1 {
		t.Fatalf("action prefix filter: %+v", byAction)
	}
	if err := db.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAPITokenByHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("token must cascade on user delete")
	}
}
