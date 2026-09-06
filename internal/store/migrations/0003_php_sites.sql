-- PHP versions installed through the panel and hosted sites.
CREATE TABLE php_versions (
    version         TEXT PRIMARY KEY,
    source          TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'installing',
    fpm_service     TEXT NOT NULL DEFAULT '',
    fpm_binary      TEXT NOT NULL DEFAULT '',
    cli_binary      TEXT NOT NULL DEFAULT '',
    pool_dir        TEXT NOT NULL DEFAULT '',
    package_version TEXT NOT NULL DEFAULT '',
    extensions      TEXT NOT NULL DEFAULT '[]',
    is_default      INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE sites (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    domain           TEXT NOT NULL UNIQUE,
    aliases          TEXT NOT NULL DEFAULT '[]',
    mode             TEXT NOT NULL DEFAULT 'fpm',
    php_version      TEXT NOT NULL,
    docroot          TEXT NOT NULL DEFAULT '',
    ip               TEXT NOT NULL DEFAULT '',
    http2            INTEGER NOT NULL DEFAULT 1,
    http3            INTEGER NOT NULL DEFAULT 0,
    ssl              TEXT NOT NULL DEFAULT 'auto',
    redirect_https   INTEGER NOT NULL DEFAULT 1,
    redirect_www     TEXT NOT NULL DEFAULT 'none',
    static_by_nginx  INTEGER NOT NULL DEFAULT 1,
    fpm_pm           TEXT NOT NULL DEFAULT 'ondemand',
    fpm_max_children INTEGER NOT NULL DEFAULT 8,
    php_ini          TEXT NOT NULL DEFAULT '{}',
    allow_exec       INTEGER NOT NULL DEFAULT 0,
    client_max_body  TEXT NOT NULL DEFAULT '64m',
    certificate_id   INTEGER REFERENCES certificates(id) ON DELETE SET NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    last_error       TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX sites_user ON sites(user_id);
CREATE INDEX sites_php ON sites(php_version);
