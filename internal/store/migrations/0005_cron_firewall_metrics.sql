-- Cron jobs per user, firewall rules and host metrics.
CREATE TABLE cron_jobs (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    schedule    TEXT NOT NULL,
    command     TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    php_version TEXT NOT NULL DEFAULT '',
    comment     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE INDEX cron_user ON cron_jobs(user_id);

CREATE TABLE firewall_rules (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,              -- allow | deny
    proto      TEXT NOT NULL DEFAULT 'tcp', -- tcp | udp | any
    port       TEXT NOT NULL DEFAULT '',    -- 22, 8000-8100, '' = any
    source     TEXT NOT NULL DEFAULT '',    -- ip or cidr, '' = any
    comment    TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL
);

CREATE TABLE metrics (
    ts         INTEGER PRIMARY KEY,        -- unix minute
    cpu        REAL NOT NULL,              -- 0..100
    load1      REAL NOT NULL,
    mem_used   INTEGER NOT NULL,
    mem_total  INTEGER NOT NULL,
    disk_used  INTEGER NOT NULL,
    disk_total INTEGER NOT NULL,
    net_rx     INTEGER NOT NULL,           -- bytes per minute
    net_tx     INTEGER NOT NULL
);
