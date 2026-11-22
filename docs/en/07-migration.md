# 07. Migration between panels

> Design document. Direct migration between two MonoPanel servers and the adapters for BitrixVM and FASTPANEL (`mp migrate`, the "Migration" page in the web UI and the "Migration" area in the TUI) are implemented; the bundle file and the other adapters are described here as a design. What works is listed in the [README](../../README.en.md) and marked [x] in [05-roadmap.md](05-roadmap.md).

The task sounds simple: "move the site (account, server) over here" — with the person pressing a single button. Underneath, it is three different tasks, and they must not be mixed up: **a move between two MonoPanel servers**, **taking in a server with a foreign panel** and **taking in a server with no panel at all**. All they share is the format of the parcel; different hands pack it.

## 1. What moves

The unit of migration is the **account**: a user and everything that belongs to them. A site is a subset of an account; a server is a set of accounts plus server-wide settings.

| Scope | What it includes |
|---|---|
| `user:<login>` | the panel account, the unix user, the home directory, sites, databases, cron, app services, Valkey instances (settings, no data), mail domains and mailboxes, certificates |
| `site:<domain>` | one site with its files, its database (if there is only one and it is the site's own), the cron jobs that mention it, its certificate |
| `server` | all accounts + firewall, real-ip, DNS providers, mail settings, PHP branches |

Not moved: the server itself (OS, kernel, network), edits to `/etc` made outside the panel, metrics and the job log, sessions and API tokens (they are issued anew), backup targets with their repository passwords, shared folders between accounts (the files arrive with the site; the guest gets the folder again with `mp user share add`).

## 2. The migration bundle (design)

One self-describing archive. It is also the artefact for "bring up a new server from nothing". Today the data travels directly between panels (see §5); the bundle file is not implemented yet.

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

**Panel → panel, on request** (works). On the source, the administrator allows the move and gets a one-off token with a scope and a lifetime (`mp migrate grant user:alex`, or "Migration" → "Hand an account over" in the web UI). On the target panel — the plan and the run: address, token, what to take. From there **the target pulls the data itself**: it has more resources to spare, it knows its own state and it can resume an interrupted transfer. The source meanwhile only serves the stream and records in the audit log who took what.

**As a file** (planned). `mp migrate export --scope user:alex --out alex.mp` → copy → `mp migrate import alex.mp`. It will always work, including when the source server no longer comes up and only backups are left.

**Through a restic repository** (planned). Export puts `state/` next to a snapshot of an existing backup target, and import reads it from there. Almost free — all the machinery is already there — and it also covers "restore a whole server on new hardware".

## 4. Steps of a move

A move breaks not at the copy but at the DNS switch: while the record points at the old server, the new one can neither be checked in a browser nor get a certificate over HTTP-01. That is why an import is designed not as one action but as four; the first two work today (see §5), the resync and the finish are planned.

1. **Plan** (`mp migrate plan`, "Check" in the web UI). The target answers with a report before it creates anything: what will arrive and how big it is, what conflicts (the login is taken, the domain is already served, a database with that name exists), what is missing (nginx or Apache is not installed, the PHP branch is not installed, there is no MySQL, too little disk space), what does not move at all.
2. **Copy** (`mp migrate run`, "Move the account" in the web UI). A job creates the account, puts the files in place, sets up the databases and applies the state. The site comes up at once, but DNS has not been switched to it yet — the "Migration" page shows a ready-made `curl --resolve` command and a line for `hosts` so you can see it for yourself.
3. **Resync** (planned). `mp migrate resync` goes over only what has changed: files by time and size, fresh dumps. It is run right before the DNS switch and takes minutes instead of hours — the site on the old server keeps working all the while.
4. **Switch-over** (planned). The person changes DNS, then runs `mp migrate finish`: the panel waits until the name resolves to it, orders ACME certificates (until then the certificate that came along is in use, so HTTPS does not drop for a second) and, on an explicit command, suspends the sites on the source. Until this step exists, DNS is switched by hand, and a site without a certificate gets one with `mp ssl issue <domain>`.

Nothing on the source is deleted automatically. The only thing an import does to the other server is read it.

## 5. What already works

Direct migration between two MonoPanel servers: `mp migrate grant` on the source, `mp migrate plan` and `mp migrate run` on the target — or the same on the "Migration" page of the web UI ([09](09-web-ui.md#migration)). On the old panel, the "Hand an account over" tab issues a token for the chosen account and a lifetime from 1 to 168 hours and shows the panel's address, the token (once) and a ready `mp migrate plan …` line; below it are the migration tokens this administrator has issued: until when each is valid, whether the new panel has used it yet, and a button to revoke it. On the new one, the "Move an account here" tab lets you choose the source — MonoPanel, FASTPANEL or BitrixVM; an `mp migrate plan …` line pasted into the address field fills the form by itself, and an ssh key is pasted as text or loaded from a file. "Check" shows the dry run: what stands in the way — with a link to the page where it is fixed — warnings, the source's notes, the fingerprint of its ssh host key and, section by section, everything that will arrive. "Move the account" queues the job; before that the API runs the dry run once more and refuses (409) if something has come in the way meanwhile. A move started earlier is shown again on the page, with the job's log.

The source **only reads**: it serves the state (`/migrate/plan` without secrets, `/migrate/state` with hashes and keys) and two streams — `/migrate/files` (a tar of the home directory or the Maildir) and `/migrate/dump` (mysqldump). Nothing changes even in its own database, apart from audit records of who took what (the token issue, the state, every file stream and every dump each get an entry of their own) and the note of when the token was last used.

The migration token is issued with the scope `migrate:user:<login>` and a lifetime. It is **restricted**: it only lets through GET requests under `/migrate`, and only for its own account; the same token gets a 403 on `/users`. Such a scope opens the account's password hashes, keys, files and databases, so only administrators' tokens carry it: an ordinary account cannot put it into its own token (`POST /tokens` answers 403), an administrator cannot put it into a token issued for an ordinary account, and the `/migrate` handlers require the administrator role whatever scope the token has. Tokens of ordinary accounts with such a scope issued before this rule are revoked by the panel at start-up and recorded in the audit log. The panel still treats all other scopes as labels — tokens issued earlier work as they did.

For `mp migrate plan|run`, the `--token` and `--insecure` flags belong to the source; the command takes its own token and certificate check for `--server` from the environment (`MP_TOKEN`, `MP_INSECURE=1`). The source's token can also be passed on stdin (`--token-stdin`), and a foreign panel's ssh password with `--password-stdin`. In the web UI's console a move from MonoPanel works the same way — a `--token` typed there reaches the command on stdin, not on its command line; for foreign panels the console will not do: it does not read a key from a file (`--key`) and cannot pass a password on stdin — they are entered on the "Migration" page.

The streams run straight through: `tar -c` on the source → HTTPS → `tar -x` here, with nothing stored on disk in between. For this the agent gained two operations (`/v1/stream/out`, `/v1/stream/in`) and a recursive `chown` — unpacking runs as root, while the owner has to be the client.

The target does everything else: it creates the account (always with the `user` role) with the carried-over hashes of the panel password and the unix password, unpacks the home directory, sets up the databases together with their accounts (`SHOW CREATE USER` → `CREATE USER ... AS '<hash>'`), creates the sites, cron, app services (switched off), Valkey instances (empty; a site keeps its PHP sessions in Valkey only if Valkey is installed here and the site's PHP branch has the redis extension enabled, otherwise it switches to files), mail domains with their DKIM keys, mailboxes with their `{BLF-CRYPT}` and aliases, installs the certificates that came along and applies the configuration.

The custom nginx directives of sites from another MonoPanel are not applied: here only an administrator writes them, and they run with the web server's privileges. They go next to the site's `custom.conf` as `sites/<domain>.d/custom.conf.migrated`, a file the server block does not include, and the dry run says so beforehand (in the web UI the "own nginx" button of a site shows them). Once checked, they are applied with `mp site nginx <domain> --set <file>` or pasted on the site's "nginx" tab.

**The bundle is input, not configuration.** The source is another server: FASTPANEL or BitrixVM read over ssh, or a MonoPanel with an administrator of its own. So the target holds every value of the bundle to the rule the API applies to the same value typed by a person in the web UI, `mp` or the TUI. Whatever breaks a rule shows up in the dry run under what stands in the way, and the move does not start. The job checks the bundle once more — the source may have changed since the dry run — leaves such entries out, names each of them in its log and ends failed: nothing is dropped silently.

- *Account.* The login suits a unix account and is neither a system account or group (root, www-data, mysql, vmail, postfix and the other services the panel installs), nor `scheduler` or `system`, nor a unix user or group the server already has. A MonoPanel that answers with another account than the one asked for is refused. The panel password hash is argon2id, the unix one a crypt hash from shadow (yescrypt, SHA-512, SHA-256, bcrypt), both at an ordinary cost: a hash asking for gigabytes of memory would bring sign-in down at every attempt. Any other hash does not move, and the password is set anew (`mp user set <login> --generate`). An account with SSH access arrives with it, and the dry run warns about that.
- *Sites* go through the site-creation rules. A name of the panel itself or of its mail server, and a domain already served here, are refused; the old server's addresses are replaced with this one's.
- *Databases.* The name is made of `a-z`, digits and `_`, is not a MySQL system schema, is not taken here and does not start with another account's `<login>_` prefix. Only local MySQL accounts move (localhost, 127.0.0.1, ::1); the others are skipped with a warning. The source's `CREATE USER` is never run as it is: the plugin (`mysql_native_password` or `caching_sha2_password`) and the hash are taken out of it, and the statement is built anew. The dump is loaded by the `mysql` client with `--binary-mode` and `--local-infile=0` as the database's own account, which has no rights outside it; `DEFINER` in the dump becomes `CURRENT_USER`.
- *Cron* follows the rules of `mp cron add`: one line without control characters and a schedule cron understands. Paths of the old home directory in the commands are replaced with the new one.
- *App services* follow the API's rules: a command with an absolute path, the working directory and the `EnvironmentFile` inside the home directory, values that add no lines to the unit.
- *Mail.* The domain follows the DNS name rules and is neither a name of the panel itself nor one that is, or lies under, a site or a mail domain of another account; the DKIM selector is one label, and the DKIM private key matches the public one; a mailbox's address is built from its local part and the domain, the password hash can only be `{BLF-CRYPT}`, and an alias cannot take a mailbox's address. Writing the maps runs the same checks as for entries made in the panel ([06](06-mail.md#11-limitations)).
- *Certificates* are installed only for the sites that arrive: the key matches, the certificate has not expired, and the names are read from the certificate itself rather than taken from the source's word. A certificate that carries a name of another account's site or of the panel itself is not installed; one not from Let's Encrypt goes in as an imported certificate, without auto-renewal.
- *Files.* The tar stream is rewritten on its way to `tar -x`: only regular files, directories, and symbolic and hard links inside the tree remain, without setuid or setgid, device files or FIFOs; in the mail store a symlink does not lead out of its own directory. The holes of sparse files expand to at most 4 GB of zeros per archive.
- The source's notes and names lose their control characters before they reach the screen or the job log; a MonoPanel source's answer is at most 64 MB.

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

Foreign panels cannot hand over an account through an API, so the adapters read the old server **over ssh as root** — with a password or a private key (`--key`, by default the first of `~/.ssh/id_ed25519`, `id_rsa` and `id_ecdsa` found for whoever runs `mp`; in the web UI the key is pasted on the "Migration" page). All that is sent to the source is `cat`, `ls`, `test`, `getent`, `mysql -e SELECT`, `tar -c`, `mysqldump`: nothing on it changes. There is nothing to check the source's host key against, so its fingerprint is printed in the dry run — compare it with what the old server shows. The ssh password is kept in the job encrypted with the panel's key and is erased along with the job. Each command is a separate ssh session, and if DNS is broken on the old server (a dead nameserver in `resolv.conf`), sshd spends 5 seconds on reverse lookups for each of them: the dry run then takes minutes rather than seconds — fix `resolv.conf` on the source.

```
mp migrate plan --from bitrixvm  --source root@old.example.com --domain shop.example.com
mp migrate run  --from bitrixvm  --source root@old.example.com --domain shop.example.com [--as shop]
mp migrate plan --from fastpanel --source root@old.example.com                 # lists the accounts
mp migrate run  --from fastpanel --source root@old.example.com --scope user:shop [--password-stdin]
```

**BitrixVM.** There is one unix user, `bitrix`, so the scope is always `user:bitrix` (`--as` gives it a different login here). The main site lives in `/home/bitrix/www` under `server_name _` and gets its name from `--domain`; the additional ones live in `/home/bitrix/ext_www/<domain>` and have names of their own. Sites of the *link* type share the main site's core through absolute symlinks — `tar` on the source rewrites their targets for the new home directory (`--transform` for symlink targets only), and after unpacking the same paths (`/home/bitrix/…` → `/var/www/<login>/…`) are fixed in `dbconn.php`, `.settings*.php` and cron commands: that way bitrix-env's `BX_TEMPORARY_FILES_DIRECTORY` keeps working. The sites' own files with `/home/bitrix/` hard-coded in them (export scripts, `bitrix/php_interface`, `local/`; the `bitrix/` core and `upload/` are not searched) are found by the dry run with `grep` on the source and listed, and after the move the paths in them are rewritten the same way — no more than 200 files per site. The `root` value in the nginx config is written by bitrix-env in quotes — they are stripped; a server block with `root` outside `/home/bitrix` is not moved, and the dry run says so. Database credentials come from `.settings.php` — the password is known in plain text, so the MySQL account is created here anew with `IDENTIFIED WITH … BY` and an explicitly named plugin: `mysql_native_password` if any of the sites runs PHP below 7.4 (the dry run warns about it), otherwise `caching_sha2_password`. Cron: besides the `bitrix` user's crontab, root's crontab is read — on BitrixVM everything is done as root, and jobs with `/home/bitrix/` paths (export, exchange) move into the account's crontab marked "from root's crontab"; root's other jobs are not moved, and the dry run lists them. `/usr/bin/php` in commands becomes `php` — that is `~/data/bin/php`, the sites' PHP branch, not the distribution's default PHP. A job that runs something from `/tmp`, `/var/tmp` or `/dev/shm`, or downloads a script straight into a shell, arrives disabled and flagged — that is what backdoors left after a break-in look like. `mp migrate plan` prints every job that will move. If the cache set in `.settings.php` or `dbconn.php` is not file-based (memcache, the clustered `CPHPCacheMemcacheCluster`), the dry run says so: a cache that does not work makes the templates recompute everything on every hit and ties up all the php-fpm processes. The cache (`bitrix/cache`, `managed_cache`, `stack_cache`) is not moved. The environment's push server, memcached and msmtp do not move — the dry run has notes about them. The account is given no web-panel password (`mp user set bitrix --generate`); the unix password travels as a hash.

**FASTPANEL.** The scope is `user:<panel login>`; without `--scope` the dry run lists who is there. The layout matches ours (`/var/www/<login>/data/www/<domain>`), and `data/www` travels whole; FASTPANEL's `index_dir` (an absolute docroot) becomes a relative `docroot`. The `php_fpm` backend is our `fpm`, `fcgi`/`mod_php` is `apache`, and the PHP version comes from `handler_version`. The nginx config generated by the panel is not moved (it is all about the old server), but its allow-list with `deny all` becomes `allow_from`; the dry run reminds you about sites with `manual_changes`. FASTPANEL stores database passwords encrypted, so the hashes from `SHOW CREATE USER` travel; the old `mysql_native_password` is enabled here if needed. A `caching_sha2_password` hash cannot be turned into `mysql_native_password`: if such an account arrives for a site on PHP below 7.4, the migration job says which `ALTER USER` to run. Certificates: uploaded ones are stored in its database as text, Let's Encrypt ones as files in `/var/www/httpd-cert`; self-signed and expired ones stay behind. FASTPANEL mail is not moved (the dry run says how many mailboxes there are). The web-panel password is the unix password (PAM); it arrives as a hash and works for SFTP; in MonoPanel it is set anew.

Old Bitrix servers most often run on bitrix-env 7 and CentOS 7 (both branches long past EOL): the adapter reads them too — the layout is the same, and on the way the MySQL 5.7 dump is cleaned of `NO_AUTO_CREATE_USER` in `sql_mode`, which MySQL 8 does not accept.

Tested on the testbed on 2026-09-12 (`scripts/testbed/sources.sh`): BitrixVM 9.0 on AlmaLinux 9 → Debian 13 (a site with a 1 GB core, the `sitemanager` database, paths rewritten, the database login and the unix hash unchanged), bitrix-env 7 on CentOS 7 with MySQL 5.7 → Ubuntu 24.04, and FASTPANEL 1.11 on Debian 12 → Rocky Linux 9 (WordPress with a real Let's Encrypt certificate — on HTTPS straight away, a site with docroot `public/`, an alias and an allow-list, a database with its old hash, two cron jobs with `data/bin/php`).

## 7. Limits

- Streaming, not "build it in memory": home directories can run to tens of gigabytes, so resuming interrupted transfers is a must.
- Different PHP and MySQL versions on the source and the target are normal; the dry run catches them, and the panel offers to install the PHP branch itself.
- Maildirs are moved as files; live mail sync (`doveadm sync`, imapsync) is a separate step, not in the first version.
- The migration token is a scope of its own: read-only, export-only, with a TTL, visible in the audit log of both panels; only an administrator issues and uses it.
- Moving between different OS families (Debian → EL) is allowed: PHP paths and service names differ, so the target renders the configuration anew, and the dry run warns that your own nginx directives and cron commands may point at paths of the old OS.
