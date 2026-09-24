-- Valkey (or Redis) instances of an account: one for the cache and one for
-- PHP sessions, each a systemd unit running as the user.
CREATE TABLE valkey_instances (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose     TEXT NOT NULL,
    memory_mb   INTEGER NOT NULL,
    status      TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE(user_id, purpose)
);

-- Where PHP keeps the sessions of a site: '' — files in the account's tmp,
-- 'valkey' — the account's sessions instance.
ALTER TABLE sites ADD COLUMN session_store TEXT NOT NULL DEFAULT '';
