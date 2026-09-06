-- Backup targets (restic repositories) and runs.
CREATE TABLE backup_targets (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    type         TEXT NOT NULL,
    repository   TEXT NOT NULL,
    password_enc TEXT NOT NULL DEFAULT '',
    env_enc      TEXT NOT NULL DEFAULT '',
    keep_daily   INTEGER NOT NULL DEFAULT 7,
    keep_weekly  INTEGER NOT NULL DEFAULT 4,
    keep_monthly INTEGER NOT NULL DEFAULT 3,
    schedule     TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_run_at  TEXT,
    last_status  TEXT NOT NULL DEFAULT '',
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE TABLE backups (
    id          INTEGER PRIMARY KEY,
    target_id   INTEGER NOT NULL REFERENCES backup_targets(id) ON DELETE CASCADE,
    scope       TEXT NOT NULL,
    snapshot_id TEXT NOT NULL DEFAULT '',
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    files       INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'running',
    error       TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL,
    finished_at TEXT
);
CREATE INDEX backups_target ON backups(target_id, id);
