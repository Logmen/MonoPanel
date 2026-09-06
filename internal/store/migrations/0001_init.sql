-- Panel state: users, sessions, tokens, settings, jobs, audit.
CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    login         TEXT NOT NULL UNIQUE,
    role          TEXT NOT NULL DEFAULT 'user',
    password_hash TEXT,
    email         TEXT,
    unix_uid      INTEGER,
    unix_gid      INTEGER,
    home          TEXT,
    shell         INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'active',
    quota_mb      INTEGER,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    ip         TEXT NOT NULL DEFAULT '',
    ua         TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    hash         TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL DEFAULT '[]',
    last_used_at TEXT,
    expires_at   TEXT,
    created_at   TEXT NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE jobs (
    id              INTEGER PRIMARY KEY,
    type            TEXT NOT NULL,
    payload         TEXT NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'queued',
    progress        INTEGER NOT NULL DEFAULT 0,
    message         TEXT NOT NULL DEFAULT '',
    log             TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    requested_by    TEXT NOT NULL DEFAULT '',
    lock_key        TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT UNIQUE,
    worker          TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    started_at      TEXT,
    finished_at     TEXT
);
CREATE INDEX jobs_status ON jobs(status, id);
CREATE INDEX jobs_lock ON jobs(lock_key, status);

CREATE TABLE audit_log (
    id      INTEGER PRIMARY KEY,
    ts      TEXT NOT NULL,
    actor   TEXT NOT NULL,
    action  TEXT NOT NULL,
    target  TEXT NOT NULL DEFAULT '',
    ip      TEXT NOT NULL DEFAULT '',
    result  TEXT NOT NULL DEFAULT 'ok',
    details TEXT NOT NULL DEFAULT '{}'
);
