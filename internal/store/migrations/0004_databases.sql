-- Database server and client databases.
CREATE TABLE db_instances (
    id              INTEGER PRIMARY KEY,
    engine          TEXT NOT NULL,
    version         TEXT NOT NULL DEFAULT '',
    socket          TEXT NOT NULL DEFAULT '',
    service         TEXT NOT NULL DEFAULT '',
    native_password INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'installing',
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE databases (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL UNIQUE,
    charset    TEXT NOT NULL DEFAULT 'utf8mb4',
    collation  TEXT NOT NULL DEFAULT 'utf8mb4_0900_ai_ci',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX databases_user ON databases(user_id);

CREATE TABLE db_users (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    database_id INTEGER REFERENCES databases(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    host        TEXT NOT NULL DEFAULT 'localhost',
    auth_plugin TEXT NOT NULL DEFAULT 'caching_sha2_password',
    created_at  TEXT NOT NULL,
    UNIQUE(name, host)
);
