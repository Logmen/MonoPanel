-- The site whose certificate a TLS client without SNI gets on the site's
-- address (mp site set --default-tls): a reverse proxy that connects by IP
-- sends no name. One per address; without it the default server refuses
-- the handshake.
ALTER TABLE sites ADD COLUMN default_tls INTEGER NOT NULL DEFAULT 0;
