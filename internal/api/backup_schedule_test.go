package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// Schedules are written one way; anything else is refused.
func TestBackupScheduleNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "none": "", "manual": "", "daily": "daily 03:00", "daily 3:05": "daily 03:05", "daily 23:59": "daily 23:59", "weekly sun": "weekly sun 03:00", "weekly fri 01:30": "weekly fri 01:30",
	} {
		if got, err := normalizeBackupSchedule(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"daily 24:00", "daily 3:5", "weekly", "weekly sunday 03:00", "hourly", "0 3 * * *", "Daily 03:00"} {
		if _, err := normalizeBackupSchedule(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

// The latest time of a schedule at or before now, and the next after it,
// for a day and for a week.
func TestBackupScheduleTimes(t *testing.T) {
	loc := time.FixedZone("+05", 5*3600)
	at := func(s string) time.Time { v, _ := time.ParseInLocation("2006-01-02 15:04", s, loc); return v }
	for _, c := range []struct{ schedule, now, last, next string }{
		{"daily 03:00", "2026-11-27 02:59", "2026-11-26 03:00", "2026-11-27 03:00"},
		{"daily 03:00", "2026-11-27 03:00", "2026-11-27 03:00", "2026-11-28 03:00"},
		{"daily 03:00", "2026-11-27 20:00", "2026-11-27 03:00", "2026-11-28 03:00"},
		// 2026-11-27 is a Friday.
		{"weekly sun 03:00", "2026-11-27 12:00", "2026-11-22 03:00", "2026-11-29 03:00"},
		{"weekly fri 03:00", "2026-11-27 02:00", "2026-11-20 03:00", "2026-11-27 03:00"},
		{"weekly fri 03:00", "2026-11-27 04:00", "2026-11-27 03:00", "2026-12-04 03:00"},
	} {
		b, ok := parseBackupSchedule(c.schedule)
		if !ok {
			t.Fatalf("%s not parsed", c.schedule)
		}
		now := at(c.now)
		if got := b.last(now); !got.Equal(at(c.last)) {
			t.Errorf("%s at %s: last %s, want %s", c.schedule, c.now, got, c.last)
		}
		if got := b.next(now); !got.Equal(at(c.next)) {
			t.Errorf("%s at %s: next %s, want %s", c.schedule, c.now, got, c.next)
		}
	}
	if _, ok := parseBackupSchedule(""); ok {
		t.Error("by hand parsed as a schedule")
	}
}

// The scheduler queues a target's backup once per time of its schedule, also
// when the API was down then (it makes up for the latest one), never for a
// time that came before the schedule was set, and a backup by hand moves
// nothing. A change of the schedule over PATCH counts from then.
func TestScheduledBackupsByTheClock(t *testing.T) {
	f := newSiteFixture(t)
	queued := func() int {
		jobs, err := f.db.ListJobs(f.ctx, 100, "")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, j := range jobs {
			if j.Type == "backup.run" && j.RequestedBy == "scheduler" {
				n++
			}
		}
		return n
	}
	var res apitypes.BackupTargetResponse
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "local", "type": "local", "repository": "/srv/backup", "schedule": "daily"}, http.StatusCreated, &res)
	if res.Target.Schedule != "daily 03:00" || res.Target.NextRunAt == nil {
		t.Fatalf("target %+v", res.Target)
	}
	// Just set: today's (or yesterday's) 03:00 came before it.
	f.s.scheduledBackups(f.ctx)
	if n := queued(); n != 0 {
		t.Fatalf("%d backups for a time before the schedule was set", n)
	}
	// Set two days ago: the latest 03:00 came after it — once, however often
	// the scheduler looks.
	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := f.db.SQL().ExecContext(f.ctx, `UPDATE backup_targets SET schedule_at=? WHERE id=?`, past, res.Target.ID); err != nil {
		t.Fatal(err)
	}
	f.s.scheduledBackups(f.ctx)
	f.s.scheduledBackups(f.ctx)
	f.s.backupsQueued.due = nil // as after a restart: the job's key holds
	f.s.scheduledBackups(f.ctx)
	if n := queued(); n != 1 {
		t.Fatalf("%d scheduled backups, want 1", n)
	}
	// By hand: nothing of the schedule moves.
	if err := f.db.SetBackupTargetRun(f.ctx, res.Target.ID, "done", ""); err != nil {
		t.Fatal(err)
	}
	f.s.scheduledBackups(f.ctx)
	if n := queued(); n != 1 {
		t.Fatalf("%d after a run by hand", n)
	}

	// PATCH: a new schedule counts from now, retention changes, 0/0/0 refused.
	f.call(http.MethodPatch, "/backups/targets/local", map[string]any{"schedule": "weekly sun 04:30", "keep_daily": 14}, http.StatusOK, &res)
	tg := res.Target
	if tg.Schedule != "weekly sun 04:30" || tg.KeepDaily != 14 || tg.KeepWeekly != 4 || tg.NextRunAt == nil || tg.NextRunAt.Weekday() != time.Sunday || tg.ScheduleAt == nil || time.Since(*tg.ScheduleAt) > time.Minute {
		t.Errorf("after PATCH: %+v", tg)
	}
	f.call(http.MethodPatch, "/backups/targets/local", map[string]any{"keep_daily": 0, "keep_weekly": 0, "keep_monthly": 0}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPatch, "/backups/targets/local", map[string]any{"schedule": "hourly"}, http.StatusUnprocessableEntity, nil)
	var manual apitypes.BackupTargetResponse
	f.call(http.MethodPatch, "/backups/targets/local", map[string]any{"schedule": "none"}, http.StatusOK, &manual)
	if manual.Target.Schedule != "" || manual.Target.NextRunAt != nil {
		t.Errorf("by hand: %+v", manual.Target)
	}
	f.call(http.MethodPatch, "/backups/targets/nope", map[string]any{"schedule": "none"}, http.StatusNotFound, nil)
	var list []*store.BackupTarget
	f.call(http.MethodGet, "/backups/targets", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].Schedule != "" {
		t.Errorf("list %+v", list)
	}
}

// A target's schedule backs up what it was given — the whole server, an
// account, a site or a database — as a run by hand takes it; one that names
// nothing that exists is refused.
func TestScheduledBackupScope(t *testing.T) {
	f := newSiteFixture(t)
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "nope", "type": "local", "repository": "/srv/nope", "schedule": "daily 03:00", "schedule_scope": "user:nobody"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "bad", "type": "local", "repository": "/srv/bad", "schedule": "daily 03:00", "schedule_scope": "home:alex"}, http.StatusUnprocessableEntity, nil)
	var res apitypes.BackupTargetResponse
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "alex", "type": "local", "repository": "/srv/alex", "schedule": "daily 03:00", "schedule_scope": "user:alex"}, http.StatusCreated, &res)
	if res.Target.ScheduleScope != "user:alex" {
		t.Fatalf("scope %q", res.Target.ScheduleScope)
	}
	var plain apitypes.BackupTargetResponse
	f.call(http.MethodPost, "/backups/targets", map[string]any{"name": "all", "type": "local", "repository": "/srv/all"}, http.StatusCreated, &plain)
	if plain.Target.ScheduleScope != "server" {
		t.Errorf("default scope %q", plain.Target.ScheduleScope)
	}
	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := f.db.SQL().ExecContext(f.ctx, `UPDATE backup_targets SET schedule_at=? WHERE id=?`, past, res.Target.ID); err != nil {
		t.Fatal(err)
	}
	f.s.scheduledBackups(f.ctx)
	jobs, err := f.db.ListJobs(f.ctx, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range jobs {
		if j.Type == "backup.run" && j.RequestedBy == "scheduler" {
			full, _ := f.db.GetJob(f.ctx, j.ID)
			found = strings.Contains(string(full.Payload), `"scope":"user:alex"`)
		}
	}
	if !found {
		t.Error("the scheduled backup does not back up user:alex")
	}
	f.call(http.MethodPatch, "/backups/targets/alex", map[string]any{"schedule_scope": "db:nothing"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPatch, "/backups/targets/alex", map[string]any{"schedule_scope": "server"}, http.StatusOK, &res)
	if res.Target.ScheduleScope != "server" {
		t.Errorf("after PATCH: %q", res.Target.ScheduleScope)
	}
}
