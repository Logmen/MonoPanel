-- CMS presets: nginx locations and PHP defaults tuned for WordPress, Joomla, Bitrix, OpenCart.
ALTER TABLE sites ADD COLUMN preset TEXT NOT NULL DEFAULT '';
