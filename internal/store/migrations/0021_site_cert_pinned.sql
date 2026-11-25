-- A site an administrator gave a certificate by hand (mp site set --cert,
-- mp ssl import --site) keeps it: another certificate that names or covers
-- the site, issued or renewed later, does not take it over.
ALTER TABLE sites ADD COLUMN cert_pinned INTEGER NOT NULL DEFAULT 0;
