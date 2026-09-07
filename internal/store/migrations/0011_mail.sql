-- Почта: домены, ящики и алиасы. Панель — источник истины: postfix и dovecot
-- читают файлы, которые она перегенерирует целиком при каждом изменении.
CREATE TABLE mail_domains (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL UNIQUE,
    active        INTEGER NOT NULL DEFAULT 1,
    dkim_selector TEXT NOT NULL DEFAULT '',
    dkim_public   TEXT NOT NULL DEFAULT '',
    dkim_key_enc  TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX mail_domains_user ON mail_domains(user_id);

CREATE TABLE mailboxes (
    id            INTEGER PRIMARY KEY,
    domain_id     INTEGER NOT NULL REFERENCES mail_domains(id) ON DELETE CASCADE,
    local_part    TEXT NOT NULL,
    address       TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    quota_mb      INTEGER NOT NULL DEFAULT 1024,
    active        INTEGER NOT NULL DEFAULT 1,
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE(domain_id, local_part)
);

CREATE TABLE mail_aliases (
    id          INTEGER PRIMARY KEY,
    domain_id   INTEGER NOT NULL REFERENCES mail_domains(id) ON DELETE CASCADE,
    source      TEXT NOT NULL,
    destination TEXT NOT NULL,
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE(domain_id, source)
);
