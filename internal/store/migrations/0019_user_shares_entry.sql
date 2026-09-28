-- Точка входа: гость после входа по SFTP оказывается сразу в общей папке
-- (internal-sftp -d), а не в корне своего chroot. У гостя она одна.
ALTER TABLE user_shares ADD COLUMN entry INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX user_shares_entry ON user_shares(user_id) WHERE entry = 1;
