-- DNS providers for ACME DNS-01 and proxy-mode sites.
CREATE TABLE dns_providers (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    type            TEXT NOT NULL,
    credentials_enc TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);
ALTER TABLE sites ADD COLUMN backend TEXT NOT NULL DEFAULT '';
ALTER TABLE certificates ADD COLUMN dns_provider TEXT NOT NULL DEFAULT '';
