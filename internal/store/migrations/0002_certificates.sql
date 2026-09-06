-- TLS certificates issued via ACME or imported.
CREATE TABLE certificates (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    name          TEXT NOT NULL UNIQUE,
    names         TEXT NOT NULL DEFAULT '[]',
    kind          TEXT NOT NULL DEFAULT 'acme',
    directory_url TEXT NOT NULL DEFAULT '',
    key_type      TEXT NOT NULL DEFAULT 'ec256',
    email         TEXT NOT NULL DEFAULT '',
    issuer        TEXT NOT NULL DEFAULT '',
    serial        TEXT NOT NULL DEFAULT '',
    not_before    TEXT,
    not_after     TEXT,
    auto_renew    INTEGER NOT NULL DEFAULT 1,
    status        TEXT NOT NULL DEFAULT 'pending',
    last_error    TEXT NOT NULL DEFAULT '',
    last_attempt  TEXT,
    cert_path     TEXT NOT NULL DEFAULT '',
    key_path      TEXT NOT NULL DEFAULT '',
    chain_path    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX certificates_renew ON certificates(auto_renew, not_after);
