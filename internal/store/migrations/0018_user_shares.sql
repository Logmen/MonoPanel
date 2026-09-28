-- Общая папка: каталог сайта одного аккаунта, смонтированный (bind) в chroot
-- другого аккаунта, с ACL на чтение и запись для гостя. Гость видит папку
-- после входа по SFTP и ничего больше; дом гостя остаётся его собственным.
CREATE TABLE user_shares (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    site_id     INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    name        TEXT NOT NULL,
    no_php      INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE(user_id, name)
);
