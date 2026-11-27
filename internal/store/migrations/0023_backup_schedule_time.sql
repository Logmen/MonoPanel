-- A backup's schedule has a time of day (and a day of the week): "daily
-- HH:MM" or "weekly <day> HH:MM", in the server's time. "daily" used to mean
-- 23 hours after the last run, whenever that was; it becomes 03:00.
-- schedule_at is when the schedule was set: a time of it that came before
-- that does not make up for itself (internal/api/backup_schedule.go).
ALTER TABLE backup_targets ADD COLUMN schedule_at TEXT NOT NULL DEFAULT '';
UPDATE backup_targets SET schedule = 'daily 03:00', schedule_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE schedule = 'daily';
