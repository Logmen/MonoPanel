-- CMS installed by the panel (mp cms install): which one, its version, when.
ALTER TABLE sites ADD COLUMN cms TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN cms_version TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN cms_at TEXT NOT NULL DEFAULT '';
