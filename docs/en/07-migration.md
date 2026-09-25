# 07. Migration between panels

> Design document. Direct migration between two MonoPanel servers and the adapters for BitrixVM and FASTPANEL (`mp migrate`) are implemented; the bundle file and the other adapters are described here as a design. What works is listed in the [README](../../README.en.md) and marked [x] in [05-roadmap.md](05-roadmap.md).

The task sounds simple: "move the site (account, server) over here" — with the person pressing a single button. Underneath, it is three different tasks, and they must not be mixed up: **a move between two MonoPanel servers**, **taking in a server with a foreign panel** and **taking in a server with no panel at all**. All they share is the format of the parcel; different hands pack it.

## 1. What moves

The unit of migration is the **account**: a user and everything that belongs to them. A site is a subset of an account; a server is a set of accounts plus server-wide settings.

| Scope | What it includes |
|---|---|
| `user:<login>` | the panel account, the unix user, the home directory, sites, databases, cron, app services, Valkey instances (settings, no data), mail domains and mailboxes, certificates |
| `site:<domain>` | one site with its files, its database (if there is only one and it is the site's own), the cron jobs that mention it, its certificate |
| `server` | all accounts + firewall, real-ip, DNS providers, mail settings, PHP branches |

Not moved: the server itself (OS, kernel, network), edits to `/etc` made outside the panel, metrics and the job log, sessions and API tokens (they are issued anew), backup targets with their repository passwords.

## 2. The migration bundle

One self-describing archive. It is also the artefact for "bring up a new server from nothing".

```
manifest.json          format, source panel version, scope, time, sha256 of every file
state/
  users.json           login, role, argon2id hash of the panel password, uid/gid, shell, quota, shadow hash of the unix password
  sites.json           domain, aliases, mode, PHP, docroot, php_ini, preset, allow_from, where PHP keeps sessions, custom nginx directives
  databases.json       databases and accounts with the authentication string from SHOW CREATE USER
  cron.json            schedules and commands
  apps.json            app services: command, workdir, env
  valkey.json          Valkey instances: purpose (cache, sessions) and memory — no data
  mail.json            domains, mailboxes (bcrypt), aliases, DKIM keys
  certificates.json    names, type, auto-renewal
files/                 home directories
dumps/                 a mysqldump for each database
certs/                 fullchain and key of the certificates in use
```

**Passwords travel as hashes, not as text.** The panel password is the argon2id hash straight from the database row; the unix password is the hash from shadow plus `chpasswd -e` on the other side; for MySQL it is the `IDENTIFIED WITH ... AS '<hash>'` that `SHOW CREATE USER` returns; for a mailbox, the same `{BLF-CRYPT}` that sits in dovecot's passwd file. Nobody re-enters a password, users do not notice the move, and the panel never sees plain text anywhere.

The exception is whatever is encrypted with the source panel's key: DKIM private keys, TOTP secrets, the update repository token. These are re-encrypted when the bundle is built, so **the bundle is always encrypted**: either with a passphrase that the person carries over themselves, or with the target panel's public key (known in a direct migration).

## 3. How the bundle reaches the new server

**As a file.** `mp migrate export --scope user:alex --out alex.mp` → copy → `mp migrate import alex.mp`. It always works, including when the source server no longer comes up and only backups are left.

**Panel → panel, on request.** On the source, the administrator allows the move and gets a one-off token with a scope and a lifetime (`mp migrate grant --scope user:alex --ttl 2h`). On the target panel — "Import from another panel": address, token, what to take. From there **the target pulls the bundle itself**: it has more resources to spare, it knows its own state and it can resume an interrupted transfer. The source meanwhile only serves the stream and records in the audit log who took what.

**Through a restic repository.** Export puts `state/` next to a snapshot of an existing backup target, and import reads it from there. Almost free — all the machinery is already there — and it also covers "restore a whole server on new hardware".

## 4. Steps of a move

A move breaks not at the copy but at the DNS switch: while the record points at the old server, the new one can neither be checked in a browser nor get a certificate over HTTP-01. That is why an import is not one action but four.

1. **Dry run.** The target answers with a report before it creates anything: what will arrive and how big it is, what conflicts (the login is taken, the domain is already served, a database with that name exists), what is missing (the PHP branch is not installed, there is no MySQL, too little disk space), what does not move at all.
2. **Copy.** A job creates the account, puts the files in place, sets up the databases and applies the state. The site comes up at once, but DNS has not been switched to it yet — the panel shows a ready-made `curl --resolve` command and a line for `hosts` so you can see it for yourself.
3. **Resync.** `mp migrate resync` goes over only what has changed: files by time and size, fresh dumps. It is run right before the DNS switch and takes minutes instead of hours — the site on the old server keeps working all the while.
4. **Switch-over.** The person changes DNS, then runs `mp migrate finish`: the panel waits until the name resolves to it, orders ACME certificates (until then the certificate that came along is in use, so HTTPS does not drop for a second) and, on an explicit command, suspends the sites on the source.

Nothing on the source is deleted automatically. The only thing an import does to the other server is read it.

## 5. What already works

Direct migration between two MonoPanel servers: `mp migrate grant` on the source, `mp migrate plan` and `mp migrate run` on the target.

The source **only reads**: it serves the state (`/migrate/plan` without secrets, `/migrate/state` with hashes and keys) and two streams — `/migrate/files` (a tar of the home directory or the Maildir) and `/migrate/dump` (mysqldump). Nothing changes even in its own database, apart from an audit record of who took what.

The migration token is issued with the scope `migrate:user:<login>` and a lifetime. It is **restricted**: it only lets through GET requests under `/migrate`, and only for its own account; the same token gets a 403 on `/users`. The panel still treats all other scopes as labels — tokens issued earlier work as they did.

The streams run straight through: `tar -c` on the source → HTTPS → `tar -x` here, with nothing stored on disk in between. For this the agent gained two operations (`/v1/stream/out`, `/v1/stream/in`) and a recursive `chown` — unpacking runs as root, while the owner has to be the client.

The target does everything else: it creates the account with the carried-over hashes of the panel password and the unix password, unpacks the home directory, sets up the databases together with their accounts (`SHOW CREATE USER` → `CREATE USER ... AS '<hash>'`), creates the sites with their custom nginx directives, cron, app services (switched off), Valkey instances (empty; a site keeps its PHP sessions in Valkey only if Valkey is installed here and the site's PHP branch has the redis extension enabled, otherwise it switches to files), mail domains with their DKIM keys, mailboxes with their `{BLF-CRYPT}` and aliases, installs the certificates that came along and applies the configuration.

Tested on two real servers of the [testbed](08-testbed.md) on 2026-09-09 (Ubuntu 24.04 → Debian 13, `make testbed-migrate`): an account with a site, a PHP file, a database and cron arrived whole — signing in to the panel and SFTP with the old passwords, the site answering with the same file, the rows and the MySQL user's password in place, the job in the crontab. The first live run caught a bug that the two-panel test with a fake agent could not see: a `caching_sha2_password` hash contains a binary salt, and through the client's text output of `SHOW CREATE USER` it arrived corrupted (`ERROR 1827`). Now the source asks the server to print the hash as a hex literal (`print_identified_with_as_hex`), and the target reproduces it without loss. Different OS families do not get in the way of a move: Debian 12 → AlmaLinux 10 passed the same checks — the target renders all the configuration anew for its own paths and service names, and the dry run keeps a warning: custom nginx directives and cron commands may refer to paths of the old OS.

Not there yet: resyncing (`resync`), finishing the move (`finish`), the `site:` and `server:` scopes, and the bundle file.

## 6. Foreign panels and servers without a panel

There is one format — the same `MigrationBundle` that MonoPanel serves — and different adapters assemble it. From there the import is shared: conflict checks, the account, streams, sites, certificates and cron work the same way for any source.

| Adapter | What it reads | Status |
|---|---|---|
| `monopanel` | the source panel's API, with a migration token | works |
| `bitrixvm` | "1C-Bitrix: Web Environment" (bitrix-env 7–9): the nginx configs in `/etc/nginx/bx/site_enabled`, each site's `bitrix/.settings.php` and `dbconn.php`, `/root/.my.cnf` | works |
| `fastpanel` | FASTPANEL 2: its SQLite `fastpanel2.db` (accounts, sites, backends, databases, certificates), the user's crontab, `/var/www/httpd-cert` | works |
| `plain` | scans nginx/apache vhosts, `/var/www` and the list of MySQL databases and proposes a mapping — a server without a panel | planned |
| `ispmanager`, `cpanel` | later; cPanel has its own `cpmove` format, which is easier to accept as it is | planned |

Foreign panels cannot hand over an account through an API, so the adapters read the old server **over ssh as root** — with a password or a private key (`--key`, default `~/.ssh/id_ed25519` of whoever runs `mp`). All that is sent to the source is `cat`, `ls`, `test`, `getent`, `mysql -e SELECT`, `tar -c`, `mysqldump`: nothing on it changes. There is nothing to check the source's host key against, so its fingerprint is printed in the dry run — compare it with what the old server shows. The ssh password is kept in the job encrypted with the panel's key and is erased along with the job. Each command is a separate ssh session, and if DNS is broken on the old server (a dead nameserver in `resolv.conf`), sshd spends 5 seconds on reverse lookups for each of them: the dry run then takes minutes rather than seconds — fix `resolv.conf` on the source.

```
mp migrate plan --from bitrixvm  --source root@old.example.com --domain shop.example.com
mp migrate run  --from bitrixvm  --source root@old.example.com --domain shop.example.com [--as shop]
mp migrate plan --from fastpanel --source root@old.example.com                 # lists the accounts
mp migrate run  --from fastpanel --source root@old.example.com --scope user:shop [--password-stdin]
```

**BitrixVM.** There is one unix user, `bitrix`, so the scope is always `user:bitrix` (`--as` gives it a different login here). The main site lives in `/home/bitrix/www` under `server_name _` and gets its name from `--domain`; the additional ones live in `/home/bitrix/ext_www/<domain>` and have names of their own. Sites of the *link* type share the main site's core through absolute symlinks — `tar` on the source rewrites their targets for the new home directory (`--transform` for symlink targets only), and after unpacking the same paths (`/home/bitrix/…` → `/var/www/<login>/…`) are fixed in `dbconn.php`, `.settings*.php` and cron commands: that way bitrix-env's `BX_TEMPORARY_FILES_DIRECTORY` keeps working. The sites' own files with `/home/bitrix/` hard-coded in them (export scripts, `bitrix/php_interface`, `local/`; the `bitrix/` core and `upload/` are not searched) are found by the dry run with `grep` on the source and listed, and after the move the paths in them are rewritten the same way — no more than 200 files per site. The `root` value in the nginx config is written by bitrix-env in quotes — they are stripped; a server block with `root` outside `/home/bitrix` is not moved, and the dry run says so. Database credentials come from `.settings.php` — the password is known in plain text, so the MySQL account is created here anew with `IDENTIFIED WITH … BY` and an explicitly named plugin: `mysql_native_password` if any of the sites runs PHP below 7.4 (the dry run warns about it), otherwise `caching_sha2_password`. Cron: besides the `bitrix` user's crontab, root's crontab is read — on BitrixVM everything is done as root, and jobs with `/home/bitrix/` paths (export, exchange) move into the account's crontab marked "из crontab root" (from root's crontab); root's other jobs are not moved, and the dry run lists them. `/usr/bin/php` in commands becomes `php` — that is `~/data/bin/php`, the sites' PHP branch, not the distribution's default PHP. A job that runs something from `/tmp`, `/var/tmp` or `/dev/shm`, or downloads a script straight into a shell, arrives disabled and flagged — that is what backdoors left after a break-in look like. `mp migrate plan` prints every job that will move. If the cache set in `.settings.php` or `dbconn.php` is not file-based (memcache, the clustered `CPHPCacheMemcacheCluster`), the dry run says so: a cache that does not work makes the templates recompute everything on every hit and ties up all the php-fpm processes. The cache (`bitrix/cache`, `managed_cache`, `stack_cache`) is not moved. The environment's push server, memcached and msmtp do not move — the dry run has notes about them. The account is given no web-panel password (`mp user set bitrix --generate`); the unix password travels as a hash.

**FASTPANEL.** The scope is `user:<panel login>`; without `--scope` the dry run lists who is there. The layout matches ours (`/var/www/<login>/data/www/<domain>`), and `data/www` travels whole; FASTPANEL's `index_dir` (an absolute docroot) becomes a relative `docroot`. The `php_fpm` backend is our `fpm`, `fcgi`/`mod_php` is `apache`, and the PHP version comes from `handler_version`. The nginx config generated by the panel is not moved (it is all about the old server), but its allow-list with `deny all` becomes `allow_from`; the dry run reminds you about sites with `manual_changes`. FASTPANEL stores database passwords encrypted, so the hashes from `SHOW CREATE USER` travel; the old `mysql_native_password` is enabled here if needed. A `caching_sha2_password` hash cannot be turned into `mysql_native_password`: if such an account arrives for a site on PHP below 7.4, the migration job says which `ALTER USER` to run. Certificates: uploaded ones are stored in its database as text, Let's Encrypt ones as files in `/var/www/httpd-cert`; self-signed and expired ones stay behind. FASTPANEL mail is not moved (the dry run says how many mailboxes there are). The web-panel password is the unix password (PAM); it arrives as a hash and works for SFTP; in MonoPanel it is set anew.

Old Bitrix servers most often run on bitrix-env 7 and CentOS 7 (both branches long past EOL): the adapter reads them too — the layout is the same, and on the way the MySQL 5.7 dump is cleaned of `NO_AUTO_CREATE_USER` in `sql_mode`, which MySQL 8 does not accept.

Tested on the testbed on 2026-09-12 (`scripts/testbed/sources.sh`): BitrixVM 9.0 on AlmaLinux 9 → Debian 13 (a site with a 1 GB core, the `sitemanager` database, paths rewritten, the database login and the unix hash unchanged), bitrix-env 7 on CentOS 7 with MySQL 5.7 → Ubuntu 24.04, and FASTPANEL 1.11 on Debian 12 → Rocky Linux 9 (WordPress with a real Let's Encrypt certificate — on HTTPS straight away, a site with docroot `public/`, an alias and an allow-list, a database with its old hash, two cron jobs with `data/bin/php`).

## 7. Limits

- Streaming, not "build it in memory": home directories can run to tens of gigabytes, so resuming interrupted transfers is a must.
- Different PHP and MySQL versions on the source and the target are normal; the dry run catches them, and the panel offers to install the PHP branch itself.
- Maildirs are moved as files; live mail sync (`doveadm sync`, imapsync) is a separate step, not in the first version.
- The migration token is a scope of its own: read-only, export-only, with a TTL, visible in the audit log of both panels.
- Moving between different OS families (Debian → EL) is allowed: PHP paths and service names differ, so the target renders the configuration anew, and the dry run warns that your own nginx directives and cron commands may point at paths of the old OS.
