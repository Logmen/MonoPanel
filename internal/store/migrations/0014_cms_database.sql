-- The database the panel made for the site's CMS: a forced reinstall empties
-- only this one, never a database that merely carries the same name.
ALTER TABLE sites ADD COLUMN cms_database TEXT NOT NULL DEFAULT '';
