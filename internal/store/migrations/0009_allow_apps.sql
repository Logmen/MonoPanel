-- Per-site IP allow-lists and user app services (systemd units running as the user).
ALTER TABLE sites ADD COLUMN allow_from TEXT NOT NULL DEFAULT '[]';

CREATE TABLE apps (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    command     TEXT NOT NULL,
    workdir     TEXT NOT NULL DEFAULT '',
    env_file    TEXT NOT NULL DEFAULT '',
    env         TEXT NOT NULL DEFAULT '[]',
    restart     TEXT NOT NULL DEFAULT 'always',
    enabled     INTEGER NOT NULL DEFAULT 1,
    status      TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE(user_id, name)
);
