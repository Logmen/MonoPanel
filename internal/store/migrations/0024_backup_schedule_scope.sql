-- What a target's schedule backs up, as a run takes it: server (everything),
-- user:<login>, site:<domain> or db:<name>.
ALTER TABLE backup_targets ADD COLUMN schedule_scope TEXT NOT NULL DEFAULT 'server';
