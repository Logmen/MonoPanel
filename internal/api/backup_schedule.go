package api

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// A backup target's schedule is a time of the day, every day or on one day of
// the week, in the server's time: "daily 03:00", "weekly sun 03:00"; "" is by
// hand only. Up to 0.9.9 "daily" meant 23 hours after the last run, whenever
// it had been — the hour drifted with each start of the API, and a backup run
// by hand moved the next one by a day.

// backupDefaultTime is the time "daily" alone stands for.
const backupDefaultTime = "03:00"

var (
	backupScheduleRe = regexp.MustCompile(`^(daily|weekly (mon|tue|wed|thu|fri|sat|sun))(?: ([01]?[0-9]|2[0-3]):([0-5][0-9]))?$`)
	backupWeekdays   = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}
)

// backupSchedule is a parsed schedule.
type backupSchedule struct {
	weekly       bool
	day          time.Weekday
	hour, minute int
}

// normalizeBackupSchedule checks a schedule as given and writes it one way:
// "" and "manual"/"none" are by hand, "daily" alone is "daily 03:00", hours
// get two digits.
func normalizeBackupSchedule(in string) (string, error) {
	switch in {
	case "", "manual", "none":
		return "", nil
	}
	m := backupScheduleRe.FindStringSubmatch(in)
	if m == nil {
		return "", fmt.Errorf("schedule %q: daily HH:MM, weekly <mon…sun> HH:MM or none", in)
	}
	at := backupDefaultTime
	if m[3] != "" {
		h, _ := strconv.Atoi(m[3])
		at = fmt.Sprintf("%02d:%s", h, m[4])
	}
	return m[1] + " " + at, nil
}

// parseBackupSchedule reads a normalized schedule; false for "" (by hand) or
// one it does not know.
func parseBackupSchedule(s string) (backupSchedule, bool) {
	m := backupScheduleRe.FindStringSubmatch(s)
	if m == nil || m[3] == "" {
		return backupSchedule{}, false
	}
	b := backupSchedule{weekly: m[2] != ""}
	b.day = backupWeekdays[m[2]]
	b.hour, _ = strconv.Atoi(m[3])
	b.minute, _ = strconv.Atoi(m[4])
	return b, true
}

// last is the latest time the schedule came at or before now, in now's zone.
func (b backupSchedule) last(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), b.hour, b.minute, 0, 0, now.Location())
	if b.weekly {
		t = t.AddDate(0, 0, -((int(now.Weekday()) - int(b.day) + 7) % 7))
		if t.After(now) {
			t = t.AddDate(0, 0, -7)
		}
		return t
	}
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// next is the first time the schedule comes after now.
func (b backupSchedule) next(now time.Time) time.Time {
	if b.weekly {
		return b.last(now).AddDate(0, 0, 7)
	}
	return b.last(now).AddDate(0, 0, 1)
}

// withNextRun fills in when each target's schedule comes next.
func withNextRun(targets ...*store.BackupTarget) {
	now := time.Now()
	for _, t := range targets {
		if b, ok := parseBackupSchedule(t.Schedule); ok && t.Enabled {
			n := b.next(now)
			t.NextRunAt = &n
		}
	}
}

// backupsQueued remembers, by target, the time of the schedule the last
// backup was queued for: a check a minute asks the database only when one
// came. The job's idempotency key, made of the same time, holds across a
// restart.
type backupsQueued struct {
	mu  sync.Mutex
	due map[int64]time.Time
}

// scheduledBackups queues the backup of each target whose time came: the
// latest time of its schedule, once, whenever the API runs then — a server
// that was down at 03:00 backs up when it is up again. A time that came
// before the schedule was set does not count: a target added, or moved to
// 03:00, at noon waits for the next night. A backup run by hand moves
// nothing.
func (s *Server) scheduledBackups(ctx context.Context) {
	targets, err := s.db.ListBackupTargets(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	s.backupsQueued.mu.Lock()
	defer s.backupsQueued.mu.Unlock()
	if s.backupsQueued.due == nil {
		s.backupsQueued.due = map[int64]time.Time{}
	}
	for _, t := range targets {
		b, ok := parseBackupSchedule(t.Schedule)
		if !t.Enabled || !ok {
			continue
		}
		due := b.last(now)
		if t.ScheduleAt != nil && !due.After(*t.ScheduleAt) {
			continue
		}
		if s.backupsQueued.due[t.ID].Equal(due) {
			continue
		}
		key := fmt.Sprintf("backup:%d:%s", t.ID, due.UTC().Format(time.RFC3339))
		scope := t.ScheduleScope
		if scope == "" {
			scope = "server"
		}
		job, err := s.jobs.Enqueue(ctx, "backup.run", backupPayload{TargetID: t.ID, Scope: scope}, jobs.WithLockKey("backup:"+t.Name), jobs.WithRequestedBy("scheduler"), jobs.WithIdempotencyKey(key))
		if err != nil {
			continue
		}
		s.backupsQueued.due[t.ID] = due
		if job.CreatedAt.Std().After(now.Add(-time.Minute)) {
			s.log.Info("scheduled backup", "target", t.Name, "schedule", t.Schedule, "scope", scope, "job", job.ID)
		}
	}
}
