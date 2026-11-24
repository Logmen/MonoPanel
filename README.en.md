# MonoPanel

English · [Русский](README.md)

A web hosting control panel for Debian/Ubuntu and the RHEL family: sites, PHP,
databases, TLS, mail, backups, firewall and moving accounts between servers — from
one static binary, with no runtime to install, no agents in other languages and no
external services.

[![ci](https://github.com/Logmen/MonoPanel/actions/workflows/ci.yml/badge.svg)](https://github.com/Logmen/MonoPanel/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/Logmen/MonoPanel)](https://github.com/Logmen/MonoPanel/releases)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![go](https://img.shields.io/badge/go-1.27-00ADD8)](go.mod)

The panel installs as a single package, serves its own HTTPS port and does not
depend on the system nginx: if a site's configuration breaks, the panel is still
reachable and can fix it. The web UI, the CLI, the SSH menu and any integration
all speak the same REST API — anything you can do with a mouse you can script.

> **Languages.** The web UI speaks English and Russian: it follows the browser
> language and can be switched in Settings. The documentation comes in both: English
> in [docs/en/](docs/en/), Russian in [docs/](docs/). API, job and diagnostic
> messages are in English; the CLI and the SSH menu follow the terminal locale by the
> same rule (`MP_LANG=ru` or `MP_LANG=en` overrides it). The code and the security
> policy are in English.

Status: **0.9.0**, in daily use on a production server hosting several sites and
mail. Development moves quickly and breaking changes are possible before 1.0.

## Install

```bash
curl -fsSL https://monopanel.app/install.sh | sh
mp setup
```

The address leads to [packaging/install.sh](packaging/install.sh) in this repository.
The script detects the OS and architecture, downloads the latest release package,
checks it against the published checksums and installs it. You can also do it by
hand — `.deb` and `.rpm` for amd64 and arm64 are attached to every
[release](https://github.com/Logmen/MonoPanel/releases).

`mp setup` creates the service user, the directories, the database, a self-signed
certificate and the administrator account, then prints the panel's address and the
password. The panel updates from the releases of the repository the package was built
from: `mp update` shows it as built-in, and only a fork or a private repository needs
the setting changed.

The script does not use the GitHub API: the latest tag comes from the `/releases/latest`
redirect and the files from their plain download links, so the anonymous API quota
(60 requests an hour per address) cannot block an install. The panel itself falls back
to the same links when it checks for updates and the quota is exhausted.

Requires root, systemd and one of: Debian 12/13, Ubuntu 22.04/24.04/26.04,
AlmaLinux, Rocky Linux or Oracle Linux 9/10. The whole matrix runs on a
[testbed](docs/en/08-testbed.md): installing the panel, nginx, PHP and Percona and the
e2e scenario pass on all eleven.
On EL, SELinux stays enforcing — the panel sets up the file contexts, booleans and a
small policy module of its own that a hosting server needs. On Debian and Ubuntu PHP
comes from `packages.sury.org`, the repository of the same maintainer who ran
`ppa:ondrej/php` (the PPA is marked deprecated, and the panel no longer uses it); for an
Ubuntu release Sury does not publish yet, from Ubuntu itself.

## Quick start

```bash
mp setup --admin-password '…'   # or omit it: a password is generated and shown
mp status                       # panel, host, services, jobs
mp stack install nginx
mp config set web.hostname panel.example.com --restart
mp ssl panel issue              # Let's Encrypt for the panel's name, served on :8443 at once

mp user add alex --generate --shell
mp php install 8.4              # alongside 7.4, 5.6 …
mp stack install percona        # Percona Server 8.4, root via auth_socket

mp site add example.com --user alex --www --preset wordpress
mp site add old.example.com --user alex --php 7.4 --mode apache
mp site add app.example.com --user alex --mode proxy --backend http://127.0.0.1:3000
mp db create shop --user alex   # a password is generated and shown
mp stack install valkey && mp valkey add cache --user alex   # the account's own Valkey
mp firewall enable && mp stack install fail2ban
mp backup target add local1 --repo /var/backups/monopanel --schedule daily

mp mail install --hostname mail.example.com   # postfix + dovecot + opendkim
mp mail domain add example.com --user alex    # with a DKIM key
mp mail box add ivan@example.com --quota 2048
mp mail domain dns example.com                # what to publish in DNS
mp mail webmail webmail.example.com --user alex --port 2096   # Roundcube

# move an account in from another MonoPanel server
mp migrate grant user:alex                        # on the old server
mp migrate plan --source https://old:8443 --token … --scope user:alex
mp migrate run  --source https://old:8443 --token … --scope user:alex
# … or from BitrixVM and FASTPANEL: over ssh, the source is only read
mp migrate run --from bitrixvm  --source root@old.example.com --domain shop.example.com
mp migrate run --from fastpanel --source root@old.example.com --scope user:shop
mp                              # terminal menu: the web UI's areas, every action a form of an mp command
```

Remotely: `mp --server https://host:8443 --token <token> status`
(mint one with `mp token create`); the terminal menu opens the same way — `mp --server … --token …`.

## What it does

Everything below goes through one REST API and is available in the web UI, the CLI
and the terminal menu: every API operation has an `mp` command and a place in the web
UI, and the terminal menu offers every `mp` command as a form.
`go test ./internal/parity/` checks this in `make test` and CI. The exceptions are deliberate
and few: browser sign-in and the console are web UI only, installation and the binary's
daemon roles are `mp` only, `mp update trust` and `mp config … --local` are for root on
the server only ([the full list](docs/en/04-cli-tui-api.md#1-principle)).
Under each heading are that area's `mp` commands.

### Sites

`mp site add|set|list|show|apply|fix|suspend|unsuspend|rm|logs|presets|move-ip`, `mp site nginx|php|tls`, `mp selinux`

- Three modes: `fpm` (nginx → php-fpm), `apache` (nginx → the account's own Apache → php-fpm via mod_proxy_fcgi: every account with sites in this mode has an Apache of its own, `monopanel-apache@<login>`, on an address of its own, `127.100.x.y:81`, its children running as the account) and `proxy` (nginx → your own backend).
- Every site gets its own php-fpm pool, ACLs for the `monopanel-web` group, a placeholder page and an automatic certificate; a suspended site serves a 503 page.
- A site listens on the server's IPv6 address too (`--ipv6`, the first global one by default; `none` — IPv4 only). Security headers with one flag (`--security-headers`: X-Content-Type-Options, X-Frame-Options, Referrer-Policy), a rate limit per address on dynamic pages (`--rate-limit 10` requests per second; static files do not count, the excess gets 429).
- Access only from listed addresses (`--allow`), HSTS when HTTPS is forced, custom nginx directives in `sites/<domain>.d/*.conf`. `mp site nginx <domain> --set file` writes them with an `nginx -t` check and a rollback, and what the panel does not take (`fastcgi_pass` to anything but the site's pool, `fastcgi_param PHP_VALUE` and the like) it refuses before anything is written, saying so; `mp site php <domain>` shows the effective PHP settings and where each one comes from.
- CMS presets `--preset wordpress|joomla|bitrix|opencart` (`mp site presets`): their own nginx locations (pretty URLs, Joomla `/api/`, Bitrix `urlrewrite.php`, OpenCart `_route_`, denied service directories, no PHP execution in uploads) and PHP defaults. Bitrix follows the BitrixVM rules: opcache for 100,000 files, no open_basedir.

### PHP 5.6–8.5

`mp php list|install|remove`, `mp php ext list|enable|disable`, `mp php ini [set|unset]`

- Branches from Sury or Remi, as many side by side as you like; each has its own php-fpm and `99-monopanel.ini`.
- php.ini values stack in layers: panel → global (`mp php ini set`, the PHP page) → preset → site (`mp site set --ini`). Any parameter can be changed for one site or for all of them. The global layer also goes into the branch's `99-monopanel.ini`, which PHP reads from the command line, so cron jobs get the same values; `mp doctor` shows a mismatch.
- A branch's extensions can be switched off and on (Debian/Ubuntu — `phpenmod`/`phpdismod`, EL — editing `extension=` in the Remi branch's ini, then a php-fpm restart). It applies to every site on the branch: there is one php-fpm master per version.
- Extensions the repository offers but that are not installed (memcache, redis, imagick …) appear in the same list; enabling one installs the package.

### CMS

`mp cms list|install <domain> <cms>`

- Installs WordPress, Joomla, OpenCart and 1C-Bitrix into an existing site: the distribution comes from the vendor's site, is unpacked into the docroot and handed to the client, gets its own database, and the site gets the matching preset.
- The installer is the CMS's own: wp-cli, the Joomla and OpenCart CLI installers, the Bitrix web wizard, which the panel walks through itself. For Bitrix — the trial Start, Standard, Small business or Business edition, the marketplace's clean install or the demo site.
- A download of the distribution that breaks off is picked up where it stopped when the server serves byte ranges (HTTP Range); what was unpacked before a failure is handed to the client.
- The administrator credentials are shown once. In the web UI — the CMS tab on the site page.

### Databases

`mp stack install percona|mysql`, `mp db create|list|passwd|rm`, `mp db engine [tune]`, `mp db config [set|unset]`, `mp db phpmyadmin install [--port 2083] [--php 8.4]|remove|status`, `mp db phpmyadmin open <database>`

- Percona Server or MySQL 8.4 LTS; root over `auth_socket` (on EL the panel moves it off the package's temporary password itself), settings sized to the available RAM.
- Server settings are changed in the panel: a value takes the panel's place in `zz-monopanel.cnf`, `mysqld --validate-config` runs before the restart, and if the server fails to start the previous settings come back by themselves.
- Databases are named `<login>_<name>`, generated passwords satisfy `validate_password`; `mysql_native_password` is enabled only for PHP < 7.4.
- A database user is granted its database by the literal name (`_` and `%` escaped): before 0.9.1 the name served as a pattern, and a database like `my_ql` opened the system `mysql` too. 0.9.1 rewrites the grants of older databases once, at its first start.
- phpMyAdmin 5.2.3 on a port of its own (2083 by default) with the panel's name and certificate, in a PHP pool of its own as the system user `monopanel-pma`. Sign in with a database user's name and password; the "phpMyAdmin" button of a database (or `mp db phpmyadmin open`) signs in at once — as a temporary MySQL account on that database only, for 8 hours or until you sign out (at most three such sessions per database and user; deleting the database or the account ends them). MySQL's root never signs in there (`auth_socket`), and the panel's session cookie never reaches phpMyAdmin's PHP.

### Valkey per account

`mp stack install valkey`, `mp valkey add|restart|rm cache|sessions [--user <login>] [--memory MB]`, `mp valkey list`, `mp site set <domain> --sessions valkey|files`

- Two separate instances per account: a cache (allkeys-lru, nothing on disk) and PHP sessions (volatile-lru, a snapshot every minute — sessions survive a restart).
- Each is a systemd unit `monopanel-valkey-<login>-<cache|sessions>` running as the account with a memory limit. It listens only on the unix socket `/run/monopanel-valkey/<login>-<cache|sessions>/valkey.sock` with mode 600: neither other accounts nor the web server can connect, and there is no TCP port at all. On EL the instances run in the SELinux domain `redis_t`.
- Valkey comes from the distribution (where it has none — Ubuntu 22.04, Debian 12 without backports — Redis with the same protocol); the package's shared instance is switched off.
- A site moves its sessions to Valkey with one setting (the redis extension of its PHP branch is needed); while a site keeps sessions in an instance, neither the instance nor the extension can be removed. Sessions are locked by phpredis (`redis.session.locking_enabled`, as files are), and the `redis.session.*` keys are tunable among the PHP settings.
- In the web UI — the valkey button on the Users page and the PHP sessions field in the site settings.

### TLS

`mp ssl issue|import|list|show|renew|rm`, `mp site tls <domain>`, `mp ssl panel issue|import|self-signed`, `mp web tls`, `mp dns-provider add|list|rm|types`

- lego: HTTP-01 through the nginx webroot, DNS-01 (Cloudflare, Hetzner, DigitalOcean, Gandi, deSEC, Namecheap, RFC2136) for wildcards, renewal 30 days ahead.
- The panel's own certificate and the sites' certificates are kept apart: the panel orders for its hostname only and picks it up live, a site orders for its domain and aliases and switches itself to HTTPS. A certificate in use cannot be deleted.
- Certificates issued elsewhere (bought, corporate) — `mp ssl import --cert --key` or the "Import" button on the site's row of the SSL page. The sites the certificate covers switch to it; a Let's Encrypt certificate is then renewed by the panel, any other one is not.

### Mail

`mp mail install|status|settings|apply|domain|box|alias|webmail`

- postfix + dovecot + opendkim: domains, mailboxes (passwords and quotas in the panel, Maildir owned by `vmail`), aliases and catch-all, IMAP/POP3/submission on the panel's certificate, DKIM signing, sieve filters.
- Send-only domains: the provider receives the domain's mail, this server signs with DKIM and sends the sites' mail. A lenient domain (`--lenient`) also accepts mail from badly configured senders.
- `mp mail domain dns` prints the required MX, SPF, DKIM, DMARC and PTR records and checks them against public resolvers.
- Roundcube webmail installs as a regular panel site or on a port of the mail host (`--port 2096` — no DNS record and no second certificate). The port opens in the panel's firewall by itself, and a new name or certificate of the mail server reconfigures the webmail by itself; `mp doctor` checks it in its `webmail` line.
- Mail policy: an account does not send mail as another account's domain. Postfix asks the panel's milter (`monopanel-mailpolicy`) about every message before opendkim signs it: a message whose envelope sender or header From is in a domain of another account (its mail domain, site or site alias, subdomains included) is refused with "550 5.7.1 <domain>: this server does not send mail as a domain of another account" — whether a mailbox sends it over 587/465, a program through sendmail, or over SMTP to this server. Domains the panel does not have are free. While the policy is down, mail waits rather than leaving unchecked; `mp doctor` has a `mail policy` line. Limit: inbound mail that an alias or a `~/.forward` sends on to an outside address leaves with its original envelope (no SRS yet). More in [docs/en/06 §11](docs/en/06-mail.md#11-mail-policy).
- DKIM switched on after the mail server was installed without it takes a step by hand — [docs/en/06 §12](docs/en/06-mail.md#12-limitations).
- More in [docs/en/06-mail.md](docs/en/06-mail.md).

### Backups

`mp backup target add|list|rm`, `mp backup run|list|snapshots|restore`

- restic in local, SFTP, S3, B2 or REST: files, MySQL dumps and a copy of panel.db.
- Retention by days, weeks and months, a daily schedule; restore into `/var/lib/monopanel-agent/restore/<snapshot>` (root's alone) or in place.
- restic runs in a Landlock sandbox: a backup reads only what it backs up and follows no symlink in the trees it saves (the web root, the panel's configuration, the data directory with certificates and dumps); the latter also holds on a kernel without Landlock; restic reads the system's files of `/etc` in a private `/etc`, so a host that replaces `resolv.conf` or `/etc/passwd` during a backup does not take DNS or owner names away from it; a restore writes only into its target and brings back no device file, FIFO, socket or set-user-ID / set-group-ID file (a snapshot with one in the paths chosen is refused); a run that could not read some files is marked `partial`. A local repository is a directory of restic's own.
- An in-place restore stages the snapshot on the file system it goes back to (for the web root `<www_root>/.restore`, root's alone), a restore aside lands in `/var/lib/monopanel-agent/restore`; before restic starts the agent compares what the snapshot's listing selects with the free space and inodes there (the parts of one restore on one file system together) and refuses at once when they are short; it removes what an interrupted in-place restore left when it starts.
- An in-place restore does not write into the home of an account being deleted or the tree of a site being deleted (refused when asked for and again when the job starts), and the deletion of an account or a site does not start while an in-place restore into its home or tree is queued or running (409 naming the job) — in whichever order they are asked for, even at the same moment.

### Files

`mp files ls|put|get|mkdir|touch|rm|mv|chmod|extract|size`

- File operations run through `monopanel fsop` behind a helper that drops privileges irreversibly; paths are relative to the account's home.
- The web UI has a file manager with an editor: browsing, drag-and-drop upload, permissions, archive extraction. Files are edited in the VS Code editor (Monaco): highlighting for php, html, css, js, sql, yaml and ini, find and replace, multiple cursors, folding, F1 for the command palette.
- A site's Files tab opens at its docroot.

### Users, SFTP and SSH

`mp user add|set|list|show|totp-reset|rm`, `mp user share list|add|entry|rm`, flags `--shell|--sftp-only`, `--password|--password-stdin|--generate`

- A panel account is a unix user. SFTP-only means a chroot into `/var/www/<login>` via `sshd_config.d/monopanel.conf`; one password for the panel and SFTP; a shell is given by a flag, and with it access to `mp` over the local socket (group `monopanel-cli`); requests to the socket from php-fpm, Apache and nginx processes are refused.
- `mp user rm <login> [--purge]` removes the account together with its sites, databases, cron, app services, Valkey instances and certificates.
- An ordinary account sees what concerns it in the web UI and the terminal menu: its own sites, databases, mail, files, jobs and the Account page — cron, app services, Valkey, shared folders, API tokens, password and 2FA.
- A shared folder (`mp user share add <guest> --site <domain> --path <folder> [--no-php]`): a folder of one account's site is bind-mounted by the agent (from the directory it opened without following a symlink, no systemd unit) into another account's home (for a guest with a shell into `/var/www/.shares/<guest>`, with a symlink in its home) — the guest sees it after signing in over SFTP and nothing else, both accounts and the web server read and write the files (ACLs), `--no-php` stops the site from executing PHP from that folder, and `--entry` (or `mp user share entry`) makes the folder the entry point — the SFTP session starts right inside it. The guest's home does not change: the chroot stays root-owned and account isolation stays as it is. A file uploaded over SFTP with mode 0644 can be read and deleted by the other side but not rewritten in place — that is how the ACL mask works; `mp site fix` lifts the limit from the files already there.

### Cron

`mp cron add|list|set|enable|disable|rm`

- The account's crontab is rendered whole from the panel database, with `~/data/bin` on PATH (the php of the right version).

### App services

`mp app add|set|list|show|start|stop|restart|logs|rm`

- A systemd unit `monopanel-app-<login>-<name>` running as the account, for gunicorn, node, bots: command, working directory and env-file confined to the home directory, autostart, logs via journalctl.

### Apache and extensions

`mp stack list`, `mp stack install apache`, `mp stack install|remove memcached|jpegoptim|git|composer|sphinx`, `mp stack memcached --memory-mb --max-conn`, `mp stack upgrade [nginx|php|apache|db]`, `mp stack auto-upgrade [--on|--off] [--hour 4]`

- Apache 2.4 on Debian/Ubuntu: every account with sites in apache mode gets an instance of its own, `monopanel-apache@<login>.service`; the root parent listens on `127.<100+uid>>16>.<(uid>>8)&255>.<uid&255>:81` (a privileged port on an address of that account alone), and the children run as the account and read only what it reads. The configuration is `/etc/apache2/monopanel/accounts/<login>/` (`httpd.conf`, `backend.conf`, `sites/<domain>.conf`, `sites/<domain>.d/*.conf`); mpm_event and a short module list without cgi, include, lua, ext_filter, status and info; `.htaccess` may do anything (`AllowOverride All`) except in shared folders with `--no-php`, where no `.htaccess` is read and no PHP runs. Only nginx talks to an account's Apache: every request nginx sends carries the account's secret (the `X-MonoPanel-Backend` header), and a local process connecting to the Apache's address directly gets 403, so a site's `--allow` list, rate limit and client address cannot be bypassed past nginx; PHP never sees the secret. What nginx answers 404 for (dotfiles such as `.env` and `.git/`, `composer.json`, `.user.ini`) the account's Apache refuses even with the secret. The panel stops and disables the distribution's `apache2.service` and takes `www-data` out of `monopanel-web`. A site's custom directives (`sites/<domain>.d/`) live as long as the site does: a mode change or a suspension leaves them alone.
- The stack packages the panel installed — nginx from nginx.org, the PHP branches from Sury or Remi, Apache from the distribution, Percona or MySQL — are upgraded from their repositories by `mp stack upgrade` or the Upgrade button on the Extensions page, and only the installed packages of the panel's set. The running version checks the configuration of nginx, PHP and the database server before the upgrade (`nginx -t`, `php-fpm -t`, `mysqld --validate-config`): their packages restart those services themselves, and a component that fails is not upgraded. After the upgrade the new version checks the configuration, each account's Apache included (`apache2 -t`), and then the services reload or restart. While the move of the sites onto their accounts' Apaches after an upgrade from 0.8.19 has not finished, the shared `apache2.service` that still serves the sites not moved is checked with `apache2ctl -t` before the upgrade — Apache is not upgraded when that fails — and restarted after it (gracefully when its binary is unchanged). A service that fails the check after the upgrade is not reloaded and runs on as it was — unless its own package's restart during the upgrade has already taken it down: then it stays down, and the job says so. Nothing is rolled back. The panel upgrades nginx, PHP and Apache by itself as well, every day at 04:00 server time (`mp stack auto-upgrade`, Settings); the database server only by name (`mp stack upgrade db`), as its restart is downtime for the sites. `mp doctor` names the components with an update waiting and the services that still run a binary an upgrade replaced. The stack upgrade and the panel's self-update never run at the same time; the panel runs the installs and upgrades of packages (apt, dnf) as a systemd unit of their own (`monopanel-pkg-<run>.service`), so an agent restart does not break them off in the middle of a transaction.
- The "Extensions" page in the web UI: memcached (127.0.0.1 only, memory and connections adjustable, restarted on change), jpegoptim, git, composer (from getcomposer.org with the published checksum, running on the newest PHP branch the panel installed, for the accounts and not for root; installing again updates it).
- Sphinx for 1C-Bitrix: the distribution's Sphinx 2.2 on Debian/Ubuntu, the sphinxsearch.com build of Sphinx 3 on EL; the `bitrix` index as Bitrix's own settings page documents it, a stale index on disk is recreated, SphinxQL on 127.0.0.1:9306.

### Firewall and real IP

`mp firewall status|enable|disable|apply|allow|deny|rm|ban|unban|trust|untrust`, `mp stack install fail2ban`, `mp stack real-ip --cloudflare [--from CIDR]`

- The nftables table `inet monopanel` with a drop policy; SSH, 80, 443 and the panel port are always open; the `monopanel-firewall` unit.
- Allows with a source are checked before denies: `deny --port 8443` + `allow --port 8443 --source <VPN>` limits the panel to the VPN. A deny that would lock you out (SSH or the panel with no per-source allow, or your own current address) is refused.
- fail2ban jails for sshd, nginx and the panel itself. The nginx jails read every site's own logs (the list follows the sites), not only the shared log. Trusted addresses (`mp firewall trust <ip|network>`) go to `ignoreip`. Your own settings live in `/etc/fail2ban/jail.d/zz-local.local`, which the panel never touches.
- Trusted proxies for nginx `real_ip` (Cloudflare ranges built in), so allow-lists and logs see the visitor rather than the proxy.
- The kernel modules sctp, dccp, rds and tipc never load: hosting needs none of these protocols, and any local account has the kernel load a protocol's module by opening a socket of it. The rule is the package's `/usr/lib/modprobe.d/monopanel.conf` (without the package, `mp setup` writes `/etc/modprobe.d/monopanel.conf`); a file of that name in `/etc/modprobe.d` replaces it, which is how a module is allowed to load again. `mp doctor` names a module that is loaded already.

### Metrics, logs and diagnostics

`mp status`, `mp metrics`, `mp site logs`, `mp logs <unit>`, `mp service`, `mp job list|show|wait`, `mp doctor`, `mp audit`

- A sampler every 10 s, stored as one point per minute for 30 days; site and journald log tails through the agent.
- Services — the units of the panel and the stack (the agent, the web servers, the database server, every installed PHP branch) and any unit by name: state, start, stop, restart, `reload-or-restart`, autostart and the journal — `mp service`, `mp logs`, the Services page. Template instances (but the accounts' Apaches), targets, shells, the power units, runs of systemd-tmpfiles over every file and the panel's own runs (its self-update `monopanel-update`, the package manager `monopanel-pkg-*`) only report their state; a mount unit below the web root the panel never starts or enables. While the site log tree `/var/log/monopanel/sites` is not root's alone or not cleared, php-fpm, nginx and Apache are not started, restarted or reloaded, and no package is installed, upgraded or removed; while the socket directory `/run/monopanel/php` is not root's alone, the same holds for php-fpm and the packages. The agent refuses, saying why, and takes both trees over at its own start. The API stops or restarts the panel itself only after it has answered. Stopping `monopanel-api` leaves the port to its socket, and the next connection starts the API again (and the agent, should it be stopped too); to stop the panel completely: `systemctl stop monopanel-api.socket monopanel-api monopanel-agent`. A package upgrade does not start a panel stopped so, nor enable units that are disabled (but an install after the package's removal enables them, as a fresh one does); to keep the panel down across reboots: `systemctl disable --now monopanel-api.socket monopanel-api.service monopanel-agent.service`.
- Long actions are jobs with a log: `mp job list|show|wait`, the Jobs page. An administrator sees every job, other accounts their own; passwords and tokens in a job's parameters are hidden in the answers.
- Site logs live in `/var/log/monopanel/sites/<login>/`, outside the home (nginx, Apache and php-fpm open them as root), and the account reads them through its group; they rotate weekly or at 100 MB: eight copies, compressed from the second one.
- `mp doctor` checks services, configs, disk, certificates, DNS, jobs and drift of the generated files, and also whether the shared folders are mounted, whether restic starts in its sandbox, the vendor package updates that wait, loaded kernel modules and what the upgrade to 0.9.0 has not finished moving.
- The audit log — who signed in and from where, who created tokens, who changed what: `mp audit`, the "Audit log" page, `GET /system/audit`; the same feed goes to `/var/log/monopanel/audit.log` as rotated JSON lines, and sign-ins and refusals to journald as well.

### Console in the web UI

`POST /system/console`

- The "Console" page: `mp` commands with the signed-in administrator's rights and streamed output, a one-off token per command.
- The command gets the panel's address and the token through its environment, and the console passes the secrets typed in flags (`--password …`) to its stdin: the process list does not show them, and the audit log records the line without them. Commands that read or write files on the server (`files get|put`, `ssl import`, `--cert`/`--key`, `site nginx --set`, `migrate --key`) and `mp update trust` do not run in the console: the web UI takes the same content through its own forms.
- The dashboard's doctor findings carry "fix site" and "back to enforcing" buttons.

### TUI menu

`mp` without arguments in a terminal — on the server or remotely (`mp --server … --token …`)

- The areas are those of the web UI navigation; each has its screens and every command of its `mp` groups. An ordinary account gets the same menus the web UI shows it.
- Every action is an `mp` command: its form is built from the command's arguments and flags, the `mp …` line is shown while the form is filled in, and that very line runs — as a child process, with its output streamed. Secrets are masked in the line and go to the command through stdin.
- Confirmations as in the web UI; deleting an account, or a site with its files, needs the name typed. Files and a site's nginx directives are edited in `$EDITOR`.
- `?` — the screen's actions and every command of the area, `:` — any `mp` command with Tab completion. The areas and the keys are in [docs/en/04-cli-tui-api.md](docs/en/04-cli-tui-api.md#3-tui-menu).

### Migration between panels

`mp migrate grant|plan|run`

- A whole account moves to another MonoPanel server. The source issues a token scoped to one account and only ever reads; the target reports conflicts first (`plan` changes nothing) and then takes the account.
- Files and dumps stream straight through; panel, SFTP, MySQL and mailbox passwords travel as hashes, so users never notice the move.
- Only an administrator issues a migration token. In the web UI — the Migration page: a token for another panel, the dry run and the move from MonoPanel, FASTPANEL and BitrixVM.
- More in [docs/en/07-migration.md](docs/en/07-migration.md).

### Moving in from BitrixVM and FASTPANEL

`mp migrate plan|run --from bitrixvm|fastpanel`

- The same dry run and move, but from a foreign server over ssh (password or key); the source is only read.
- BitrixVM: sites from `/etc/nginx/bx`, database credentials from `.settings.php`; link-site symlinks and the paths in `dbconn.php` and cron are rewritten for the new home.
- FASTPANEL: the account, sites, PHP backends, databases with their password hashes, allow-lists, certificates and cron from its database and files.
- The CMS preset is detected from the site's files.

### Updates

`mp update`, `mp update check|apply|settings`, `mp update trust`

- The panel finds a new version in this repository's releases, downloads the package for its OS, verifies the ed25519 signature and installs it from a separate systemd unit. If the new version does not answer, the previous binary comes back; `mp update apply` installs that version once more, the automatic update does not retry it.
- Only root on the server changes the key the signature is checked against (`mp update trust`): it never passes through the panel process — neither the API nor the console; the web UI shows it read-only.
- The panel never installs a version older than the installed one; should going back be unavoidable, root does it by hand — see [Going back to an older version](#going-back-to-an-older-version): from 0.9.0 to an older version it takes steps afterwards.

### Panel settings

`mp config show|set|templates`

- The panel's name and address, the log level and format and the number of jobs that run at once (`web.hostname`, `web.listen`, `log.level`, `log.format`, `jobs.workers`) are changed through the API — `mp config set` or Settings → Panel in the web UI. A change takes effect once monopanel-api restarts, and `--restart` restarts it right away; the panel opens the new address's port in the firewall before anything is written. systemd holds the panel's port (`monopanel-api.socket`), so no other local process can take it while the API restarts; when `web.listen` changes, the socket moves with the restart.
- If the API does not come up (after a bad `web.listen`, say), root on the server edits the file directly: `mp config set --local`. With `web.listen` it moves the port's socket too, and `--restart` restarts the API with it. Editing `web.listen` in config.yaml by hand does not move the socket: run `mp setup` or `mp config set --local web.listen <address> --restart` afterwards.
- `mp config templates` and the same page list the built-in configuration templates and which of them files in `templates_dir` override.

### Tokens, 2FA and webhooks

`mp token create|list|revoke`, `mp totp status|setup|enable|disable`, `mp user totp-reset`, `mp webhook add|list|test|rm`, `mp whoami`

- A token belongs to an account; an administrator can mint one for another account (`--user`), and root on the local socket gets one for the single administrator with no flags.
- TOTP 2FA with a QR code — in the web UI, the terminal menu and the terminal itself (`mp totp setup` draws the QR code); 2FA can be set up anew only after turning it off with the password. Bearer tokens, webhooks signed with HMAC-SHA256 on job events.
- A new password and a suspension end the account's browser sessions (except the one it changed its own password from); after a password change the API tokens keep working and are revoked separately. A suspension closes the panel only — signing in, the sessions, the API tokens and `mp` over the local socket —, while SFTP/SSH, sites, databases and cron keep working. `mp whoami` shows who you are to the panel: account, role, how you are authenticated, token and 2FA.

### Languages

- The web UI in English and Russian: picked from the browser language (CIS languages → Russian, everything else → English), switchable in Settings and on the sign-in screen, remembered per browser.
- The CLI and the TUI follow the terminal locale by the same rule; `MP_LANG=ru|en` sets it explicitly. API, job and diagnostic messages are in English.
- The placeholder page of a new site and the page of a suspended one follow the visitor's browser language.

### Not there yet

Own PHP builds (Sury/Remi are used instead), apache mode on EL, disk
quotas, per-site cgroup limits, a DNS server, a WAF, several servers from one panel,
an apt/yum repository (packages ship as releases and the panel installs them itself).
Mail runs on all eleven OSes with dovecot 2.3 and 2.4 (Debian 13, Ubuntu 26.04 get a
configuration in the 2.4 syntax; EL keeps SELinux enforcing);
there is no content filter (rspamd) and no SRS for mail forwarded outward. Accounts move between two MonoPanel servers and in from BitrixVM and
FASTPANEL, without a resync before the DNS switch; other panels and servers without a
panel are planned ([docs/en/07-migration.md](docs/en/07-migration.md)).
A suspension of an account closes the panel only, not SFTP/SSH yet; the limitations of
0.9.0 and the ways around them are in [Upgrading to 0.9.0](#upgrading-to-090).

## How it works

One binary, `monopanel` (also `mp`), runs in several roles:

| Role | Privileges | Purpose |
|---|---|---|
| `api` | unprivileged `monopanel` user | HTTPS :8443, a unix socket for the CLI, the job queue, schedulers |
| `agent` | root | a closed set of typed operations over a unix socket — never a command string |
| `helper` | setuid, irreversible drop | file operations on behalf of an account |
| `fsop` | the account's own privileges | the file operation itself |

State lives in SQLite. Changing a site is not an edit to a file on disk but a row
in the database, from which configuration is rendered: template → validation
(`nginx -t`, `apache2 -t -f <the account's httpd.conf>`, `php-fpm -t`) → every file
written atomically → reload.
A failure anywhere on that path rolls the whole set back and returns the error text.

The principles, briefly:

1. One Go binary, no runtime on the host.
2. Split privileges: an unprivileged API process talks to a root agent with an
   allow-list of operations and paths.
3. One API behind the web UI, the CLI, the menu and any integration.
4. Declared state → render → validate → apply atomically → reload, with rollback.
5. The panel serves its own port and does not depend on the system nginx.
6. One php-fpm master per version, one pool per site; Apache only through
   `mod_proxy_fcgi`, never `mod_php`.

## Releases and updates

A version is cut by tagging; CI does the rest. The tag `v0.9.0` builds `.deb` and
`.rpm` for amd64 and arm64, signs the checksum list with an ed25519 key held as a
repository secret and publishes the release; the annotated tag's message becomes the
release notes.

```bash
make keygen                  # once: a signing key pair (the private half becomes the secret)
make release VERSION=0.9.0   # tag and push; CI builds and publishes
make packages VERSION=0.9.0  # the same artefacts locally, without publishing
```

On a server:

```bash
mp update settings --repo owner/name # --token-stdin for a private repository
mp update                            # what is installed and what is available
mp update apply                      # download, verify the signature, install, restart
mp update trust                      # which key the signature is checked against (root on the server only)
mp update trust --key <fork key>     # your own key instead of the built-in one; --off disables the check
```

The public half of the release signing key is built into the panel and into
`install.sh`, so the signature is checked by default — on the first install (when
openssl 1.1.1+ is present) and on every update. The key can be compared with what the
`release-key` workflow prints:

```
eoGfJciXIG9upyFNJQR7rIsXtSs506DkiXT5kiBUyhg=
```

Update checks run on a schedule (daily by default) and `--auto-apply` installs what they
find. The panel does not install the package itself: the agent starts a transient
`monopanel-update.service`, which survives the restart of both daemons and restores
the previous binary if the new version fails to answer. An unsigned release will not
install unless the check is switched off explicitly. A slow link is fine: the package
download has no overall deadline, only a stalled stream is given up.

The package manager runs with the panel's environment: on Ubuntu `NEEDRESTART_MODE=l`, so
needrestart lists the services left on old libraries instead of restarting them all, the
database server among them; `install.sh` does the same. The update's install never runs
at the same time as the stack upgrade or the panel's other apt and dnf runs: they wait
for each other. An update that rolled back leaves the package database with the new
version while the previous binary runs: `mp update apply` installs it once more
(`apt-get --reinstall`, `dnf reinstall`); `mp update`, Settings, the TUI and `mp doctor`
say so; and the automatic update does not retry that version (it installs the next
release as usual).

An update from 0.8.19 to 0.9.0 that rolled back is an exception: the retry runs 0.8.19's
updater, whose plain `apt-get install` / `dnf install` changes nothing for a version the
package database lists already, so it rolls back again (and 0.8.19 with automatic
updates on retries at every check). Install the package by hand:
`apt install --reinstall ./monopanel_0.9.0_<arch>.deb` or
`dnf reinstall ./monopanel-0.9.0.<arch>.rpm`. Take the file from the 0.9.0 release; the
one 0.8.19 downloaded is in `/var/lib/monopanel/downloads/`, but that directory is the API
process's — check its sum against the release's `SHA256SUMS`. From 0.9.0 on,
`mp update apply` reinstalls such a version itself.

### Upgrading to 0.9.0

What an administrator notices after the upgrade from 0.8.19:

- `monopanel-api.socket` holds the panel's port. A stopped `monopanel-api` is started
  again by the first connection; to stop the panel completely:
  `systemctl stop monopanel-api.socket monopanel-api monopanel-agent`. Editing
  `web.listen` in `config.yaml` by hand does not move the socket: run `mp setup` or
  `mp config set --local web.listen <address> --restart` afterwards. Should the
  self-update roll back, the port needs nothing: before the old version starts, a drop-in
  of the API's unit stops the socket, and the old API listens on the port itself. A
  package downgrade to 0.8.19 takes steps — see
  [Going back to an older version](#going-back-to-an-older-version).
- A package upgrade leaves enabled and disabled units as they are (up to 0.8.19 it
  enabled the agent and the API every time) and does not start a stopped panel: when the
  API was not running, its socket is not started either. Upgrading from 0.8.19, the
  socket of the panel's port is enabled only if the API is. Installing the package again
  after its removal (`apt remove`, `dnf remove`) enables the panel as a fresh install
  does: the removal turned it off, not the administrator; `mp setup` starts it.
- Units in `/etc/systemd/system`: up to 0.8.19 `mp setup` copied
  `monopanel-agent.service` and `monopanel-api.service` there on every server, and the
  copies overrode the package's units for good. The upgrade removes the ones that are
  provably the panel's own and points the symlinks that enable them at the package's
  units (`/usr/lib/systemd/system`); `mp setup` on a server with the package writes no
  units into `/etc` any more. On servers set up with 0.8.12 or earlier the agent thereby
  gets its unit's fences for the first time (`ProtectHome=read-only`, `PrivateTmp=yes`
  and the rest), which the others have had since 0.8.13. The administrator's own unit,
  or a copy for another binary, the upgrade leaves alone, and `mp setup` warns that it
  overrides the package's unit.
- After a start the panel waits for its agent however long the agent takes (it may be
  taking over the previous version's mount units of shared folders and checking what they
  mounted): the API's journal says `start-up refresh: waiting for the agent` and
  `the agent answers`, the start-up work (the moves, the checks) begins once it answers,
  and queued jobs wait for the agent up to six minutes instead of failing on "agent
  unreachable". Should the API stop first, the start-up work runs at the next start.
- An API that cannot take what it serves on — the port of `web.listen` (held by another
  process), its TLS key, the socket `api.sock` — no longer runs on without its port: it
  changes nothing on the host, logs `monopanel api cannot start` with the reason and
  exits, and systemd starts it again. The jobs, the start-up refresh and the loops start
  only once the API has taken everything it serves on.
- The panel never installs a version older than the installed one (nor a release over a
  later build such as `0.8.19-5-gabc1234`); the refusal says what going back would break
  and points to [Going back to an older version](#going-back-to-an-older-version).
- Apache mode: every account has an Apache of its own (`monopanel-apache@<login>`). The
  sites move by themselves, one at a time and without downtime, and then the
  distribution's `apache2.service` is stopped and disabled — an administrator's own
  sites in it, if any, stop working — and `www-data` leaves `monopanel-web`. Only nginx
  talks to an account's Apache: every request nginx sends carries the account's secret
  (the `X-MonoPanel-Backend` header), and any local process connecting to
  `127.100.x.y:81` directly gets 403. An account's Apache stops gracefully: the requests
  in flight get up to a minute to finish. Where `net.ipv4.ip_unprivileged_port_start` is
  81 or lower, `mp stack install apache` refuses and `mp doctor` warns: any account could
  take the Apaches' port there.
- A site in Apache mode stays on the shared Apache until the agent takes its custom
  directives for the account's Apache: not with an `Include`, a log in another
  directory, `APACHE_*` variables, nor, at the virtual host's level, `RewriteEngine Off`
  or `RewriteOptions Inherit`/`IgnoreInherit` (they would remove the secret's check).
  Fix them (move `RewriteEngine Off` into a `<Directory>`, use `InheritBefore` instead of
  `Inherit`) and run `mp stack install apache` again. The agent reads the directive files
  (root-only ones too, umask 077) and carries them over no more open than they were: one
  only root could read stays so with the account's Apache. They must be plain files in
  `/etc/apache2/monopanel/sites/<domain>.d/` — a symlink (for a file, or for the `sites`
  or `<domain>.d` directory itself), a hard link or a file over 1 MiB keeps the site
  where it is until the file itself is put there; a symlink of root's further up, such as
  an `/etc/apache2` kept elsewhere, is fine. Template overrides from 0.8.19 keep a site
  there the same way: `nginx/site.conf.tmpl` without `include {{ .ApacheBackendConf }};`
  in both Apache-mode locations, and `apache/site.conf.tmpl` without `RewriteEngine On`
  right inside the `<VirtualHost>`. The move does not repeat at every start: after a
  failure the next try waits as for the logs (below), `mp doctor` shows the state in its
  `service apache2.service` line, and `mp stack install apache` tries at once. While the
  move has not finished, an `apache move` line names the sites on the shared Apache, the
  Apaches of accounts none of whose sites has moved yet are not checked, and the shared
  Apache stopped meanwhile is a failure in `mp doctor`: those sites answer 502
  (`systemctl start apache2.service`). `mp stack upgrade apache` and the daily upgrade
  check its configuration first (`apache2ctl -t`), leave Apache alone when that fails, and
  restart it after the upgrade. Meanwhile the Apache row of the Extensions page,
  `mp stack list` and the TUI gives the number of sites waiting for the move and the state
  of the shared `apache2.service`; stopped, it is a red tag, and `mp stack list` hints
  `systemctl start apache2.service`.
- The panel makes every php-fpm branch's stock `www` pool idle once — when it installs the
  branch, or at the first start of 0.9.0: a virtual host of your own that used its socket
  (`/run/php/php<version>-fpm.sock`, on EL `/var/opt/remi/phpXY/run/php-fpm/www.sock`;
  phpMyAdmin from apt, for example) stops working. Put your own `www.conf` back and the
  panel leaves it alone, and `mp doctor` warns in its `php X.Y pool www` line; to hand the
  file back to the panel, remove it and restart `monopanel-api`. The same happens if you
  take the maintainer's version when dpkg asks about `www.conf` during a manual upgrade of
  php-fpm; the default answer keeps the panel's pool, and the apt runs of the panel itself
  ask no such question. Comments do not count: a pool that differs from the panel's idle
  pool in them alone stays the panel's, and `mp doctor` does not warn about it. On EL the
  panel likewise sets `error_log = syslog` in `php-fpm.conf` once: should the line be
  removed or changed later, the same doctor line warns, and it is put back by hand — the
  panel never writes `php-fpm.conf` anew. The
  output of php-fpm's workers no longer goes into the master's log
  (`catch_workers_output = no`); PHP's errors still go to `<domain>.php.error.log`. A pool
  that still has it on is named in the doctor's `php-fpm pools` line: the sites' pools
  lose it as the sites move (below); take the line out of a pool of your own.
- The panel no longer writes custom nginx directives with a `fastcgi_pass` to anything
  but a site's pool, `fastcgi_param PHP_VALUE` or `PHP_ADMIN_VALUE`, a `uwsgi_modifier1`
  other than 0, a `proxy_method` that is not a word, an `include` of
  `/etc/nginx/default.d` or with a wildcard in a directory name, a log deeper than
  `/var/log/monopanel/sites/<login>/`, or a `proxy_pass` to an account's Apache without
  `include monopanel/apache/<login>.conf;`. A `custom.conf` written earlier keeps
  working, but can only be written again without them.
- Units: `mp service` and the Services page only show the state of template instances
  (but the accounts' Apaches), targets, shells such as `debug-shell`, the power units and
  runs of systemd-tmpfiles over every file (`systemd-tmpfiles-setup*`,
  `systemd-tmpfiles-clean`). The same holds for the panel's own runs,
  `monopanel-update.service` and `monopanel-pkg-<run>.service`, and a mount unit of yours
  below the web root the panel never starts, reloads or enables: use `systemctl` for
  those.
- The panel runs the installs, upgrades and removals of packages (apt, dnf) not as the
  agent's child processes but as a systemd unit of their own,
  `monopanel-pkg-<run>.service` (`systemctl list-units 'monopanel-pkg-*'` shows a run
  while it goes): an agent restart — a self-update, `systemctl restart` — no longer
  cuts a dpkg or rpm transaction off, and the next run waits until it is over. A job
  whose call died with the agent names the unit; its result is in
  `journalctl -u monopanel-pkg-<run>.service`. The stack upgrade and the panel's
  self-update never run at the same time: the one that comes second waits in the queue.
- EL: installs and upgrades of Percona no longer leave an SELinux denial (AVC) and an
  `rpm_script_tmp_t` directory in `/tmp` until a reboot, and the agent's restart in the
  upgrade from 0.8.19 itself passes without one too: the package manager's runs and the
  agent empty their own `/tmp` and `/var/tmp`, and the agent's unit has an `ExecStopPost`
  for it (`monopanel clean-private-tmp`, [docs/en/02 §6](docs/en/02-platform-matrix.md#6-selinux-el9--el10)).
  An edited copy of the agent's unit in `/etc/systemd/system`, which the upgrade keeps,
  and the `/etc` unit of an install without the package (until
  `mp setup --install-units` writes it anew) lack that line: on such a server the agent's
  restart in the upgrade gives one AVC. Directories left by earlier runs stay until a
  reboot but give no new denial. With a drop-in that sets `PrivateTmp=no` for
  `monopanel-*.service`, the log of a job with packages ends with a "… left as they are"
  line: the cleanup leaves a `/tmp` that is not the unit's own alone.
- The pools' socket directory `/run/monopanel/php` and the site log tree
  `/var/log/monopanel/sites` are root's alone: systemd-tmpfiles makes `php/` at boot, and
  the agent prepares both at its start, before the API. A directory someone else made (up
  to 0.8.19 `/var/log/monopanel` belonged to the user `monopanel`) the agent takes over,
  and it clears the tree of what root does not put there: directories go to an
  `untrusted-<time>-<random>` next to it. While the site log tree is not root's alone, or
  not cleared completely, the agent does not start, restart or reload php-fpm, nginx or
  Apache, run their configuration checks, or install, upgrade or remove packages; while
  `php/` is not root's alone, the same holds for php-fpm and the packages. It answers 403
  with the reason: a directory of someone else's it takes over when restarted, and the
  site log tree it tries to clear again before each such action. The upgrade adds the line
  for `php/` to the copy of `/etc/tmpfiles.d/monopanel.conf` that `mp setup` left as well,
  and keeps `/etc/tmpfiles.d/monopanel-php.conf`, which the API wrote: a version older
  than 0.9.0 needs it after its own `mp setup`, and the same line in two files is no
  warning. Should that file hold anything else, the package warns: check what it makes as
  root at boot, and leave the panel's line in it.
- composer does not run as root: `su -s /bin/sh -c 'composer …' <login>`. The package
  sources the panel wrote over http (nginx.org, repo.mysql.com) it moves to https at its
  start, and it writes no others.
- PHP on Ubuntu now comes from `packages.sury.org`, as on Debian: its maintainer marked
  `ppa:ondrej/php` deprecated, and it is empty for Ubuntu 26.04. A server where a panel up
  to 0.8.19 added the PPA (`/etc/apt/sources.list.d/ondrej-php.list`, the key
  `/etc/apt/keyrings/ondrej-php.asc`) moves to Sury at the start of the API, as the last
  step of the start-up work: Sury's key and source go in beside the PPA's, the index is
  updated, and the PPA's files go only if apt read Sury in that update and said nothing
  against it — however `apt-get update` itself ended (it fails for any failing source,
  the PPA itself included); an index of Sury left from before does not count. Otherwise
  Sury's files go again, the PPA stays, the `php repository` line of `mp doctor` says why
  (in detail: `journalctl -u monopanel-api`), and every start of the API tries again:
  once the cause is fixed, run `systemctl restart monopanel-api`. Nothing is reinstalled
  and php-fpm does not restart for the move: Sury's builds are newer than the PPA's, and
  the next stack upgrade installs them — the daily one at 04:00 or
  `mp stack upgrade php`; until then `mp doctor` lists them as an update waiting
  (`update php-X.Y … from packages.sury.org`). A source of the PPA the administrator
  added (`add-apt-repository`: `ondrej-ubuntu-php-<release>.sources` or `.list`) is left
  alone; should apt read no source at all with it beside the panel's, `mp doctor` says
  which of the two to remove ([02 §2](docs/en/02-platform-matrix.md#2-package-sources)).
  An `ondrej-php.asc` left without its source (as 0.8.19 left Ubuntu 26.04, where apt
  refused the PPA's key) is removed. The agent no longer lets the PPA's source or key be
  written; removing them it allows.
- A source of Sury the server has already — one the administrator added
  (`/etc/apt/sources.list.d/php.list` per Sury's README) or a mirror of Sury (Sury's
  `+debsuryorg` builds from another address) — is kept once apt has read it (any
  `apt-get update`): the panel writes no source of its own beside it, and the move
  removes only the PPA, after apt has read that source anew with the PPA still there.
  Before apt has read it, apt refuses the panel's source beside it ("Conflicting values
  set for option Signed-By"): the move keeps the PPA, a PHP install fails, and
  `mp doctor` and the install's error advise `apt-get update` (after which the
  administrator's source is kept) or removing that source. The panel's own
  `sury-php.list` left from before is not taken for anybody else's: it is written again
  with its key. A PHP install on Debian and Ubuntu now needs apt to have actually read
  Sury's index: when `apt-get update` left it out for a passing network failure, the
  install fails with apt's warning and tries again the next time.
- The agent runs an index update again for up to ten minutes while another
  `apt-get update` holds the lists' lock (`/var/lib/apt/lists/lock`) — apt-daily right
  after boot does: `DPkg::Lock::Timeout` covers dpkg's locks only, and such an update used
  to fail at once (a PHP install, a stack upgrade; the move to Sury waited for the next
  start of the API). Whether the lock is free the agent asks the lock itself (fcntl), not
  a new run of apt, and runs the update again once it is: systemd's journal keeps one
  failed start of the unit `monopanel-pkg-<run>.service` ("Failed to start MonoPanel
  package manager…", priority err), not one every five seconds. The agent runs apt and
  dnf with an empty `LANGUAGE`: on a server that sets `LANGUAGE` (say
  `LANGUAGE=ru_RU:ru` in `/etc/default/locale`) apt answered in Russian even under
  `LC_ALL=C.UTF-8` (Debian 12, Ubuntu 22.04; Ubuntu 26.04's glibc ignores LANGUAGE under
  C.UTF-8 itself), the panel did not find the words it reads in its output, and a PHP
  install called the branch unavailable.
- One's own password changes in the web UI and with an API token only with the current
  one (`mp user set` — `--current-password`); over the local socket the current one is
  not needed. A password change ends the account's other browser sessions, and 2FA is set
  up anew only after turning it off with the password. Migration tokens found with
  ordinary accounts are revoked at the panel's start.
- The site logs move from `data/logs` to `/var/log/monopanel/sites/<login>/`; the old
  files stay with the account. At its first start the panel queues `site.apply` for every
  site an older panel set up with its logs in `data/logs` (active, in error or pending;
  not a suspended one, which has no logs there) and `mail.apply` for a webmail on its own
  port; it remembers what finished, not what it queued: a site counts as moved only once
  it has been applied. A site that cannot be applied (its PHP branch is gone, say) stays
  on the old paths — `mp doctor` names it and the reason in its `site logs` line — and the
  panel tries again by itself: at every start and once an hour it queues the site again
  once its next try is due — 10 minutes after the first failure, then four times as long
  after each further one, at least once a day. Once the cause is fixed,
  `mp site apply <domain>` moves the site at once. Until a site has moved, nothing rotates
  its logs in `data/logs` and fail2ban does not read them: root does not work on files in
  a client's directory; its log tail (the "Logs" tab, `mp site logs`) shows the one of its
  two files written last: the one in `~/data/logs` (the account reads it) or the log
  tree's. On EL with SELinux the sites move only once the label rule of the log
  tree is in place. If `semanage` failed, the panel tries the rule again every hour and
  before it applies a site; while the rule is missing, a site on an enforcing host is not
  applied — the job gives `semanage`'s reason, and the site runs on as it was — and the
  webmail on its port stays as it is. The config history is in
  `/var/lib/monopanel-agent/confhistory/`.
- Backups: restic runs in a sandbox. A local repository must be a directory of restic's
  own — a target whose repository lies right in a directory with other files (such as
  `/var/backups`) stops working: add a new one, `/var/backups/monopanel` for example.
  S3, B2 and REST credentials go in the target's variables only; root's key and
  `known_hosts` for SFTP are set up beforehand. A run in which restic could not read some
  files is marked `partial`. An in-place restore stages the snapshot on the file system it
  goes back to (for the web root in `/var/www/.restore`) and checks the free space and
  inodes there before restic starts; a restore aside is checked the same way. When there
  is not enough, the job fails at once, saying how much it takes and how much is free. A
  restore refuses a snapshot with device files, FIFOs, sockets or set-user-ID /
  set-group-ID files in the paths chosen — choose the paths without them; while an
  in-place restore runs, no shared folder is bound or unbound (a change of a folder waits
  up to 30 s, then is refused: try again once the restore is done). The `restic sandbox`
  line of `mp doctor` names the restic version that started in a backup's sandbox.
  "restic does not start in its sandbox" is a failure (`mp doctor` exits 3, the dashboard
  counts it as failed): restic does not start there, and backups fail with that same
  error (EPEL's restic on AlmaLinux, Rocky and Oracle Linux reads
  `/proc/sys/crypto/fips_enabled` as it starts — the sandbox grants it that file).
  "restic took too long to start in its sandbox" is a warning: `restic version` did not
  finish within 30 seconds, as on a host too busy at that moment; the agent tests it
  again on the first status request 10 minutes later, and if it keeps timing out,
  backups hang the same way — look at the host's load, memory and disk, and at how long
  `restic version` takes outside the panel.
- On Oracle Linux with the UEK kernel Landlock is built in but not enabled: restic runs
  without the sandbox, while a backup still reads the trees it saves without symlinks.
  The `restic sandbox` line of `mp doctor` says how to turn Landlock on:
  `grubby --update-kernel=ALL --args=lsm=$(cat /sys/kernel/security/lsm),landlock` and a
  reboot, or the RHCK kernel (`dnf install kernel`, then `grubby --set-default`).
- The panel now upgrades nginx, PHP and Apache by itself: every day at 04:00 server time
  it installs what their repositories (nginx.org, Sury, Remi, the distribution for
  Apache) offer for its installed packages, checks the configuration and
  reloads or restarts the services. To turn it off: `mp stack auto-upgrade --off`; to move
  it: `mp stack auto-upgrade --hour <hour>`, or the "Daily upgrade of nginx, PHP and
  Apache" card in Settings. At once: `mp stack upgrade [nginx|php|apache|db]` or the
  buttons on the Extensions page; the database server is upgraded only that way, by name.
  The panel leaves packages held with `apt-mark hold` alone, and on EL those locked with
  `dnf versionlock`. `mp doctor` names the components with an update waiting, the held
  packages, and the stack's services that still run a binary an upgrade replaced (the
  distribution's upgrades included); the line says how to restart such a service — for
  nginx, PHP, Apache and the database server `mp stack upgrade <component>`, after the
  configuration check. Ubuntu's needrestart, after the apt runs with which the panel
  installs and upgrades the stack, now only lists the services on old libraries and
  restarts nothing itself, the database server included. If the agent restarts in the
  middle of an upgrade, the job runs again after the panel restarts and waits for the
  apt/dnf run to end; its result is in `journalctl -u monopanel-pkg-<run>.service`, its
  output in `/var/log/apt/term.log` or `/var/log/dnf.log`. The record of the last upgrade
  (`mp stack auto-upgrade`, Settings) keeps one line of the error: its start and its end,
  where apt-get or dnf give the reason; all of it is in `mp job show <id>`. Below its
  table `mp stack list` says how the waiting updates get installed (and whether the daily
  upgrade is off), and for each service on a replaced binary what restarts it.
- The kernel modules sctp, dccp, rds and tipc no longer load
  (`/usr/lib/modprobe.d/monopanel.conf`). A module loaded already stays until a reboot or
  `rmmod`, and `mp doctor` warns about it. To let a module load, put a copy of the file
  without its line into `/etc/modprobe.d/monopanel.conf` (an empty file lets all four
  load). A copy of the rule `mp setup` wrote on an install from the binary alone goes once
  the package is installed; a file you changed stays, and the install warns. In a
  container the host's kernel loads the modules: the `kernel modules` line warns that the
  rule belongs in the host's `/etc/modprobe.d`.
- The agent binds the shared folders, not systemd: it takes the previous version's mount
  units over at its first start — those whose guest or owner no longer passes the account
  check too — and takes a mount that does not show the folder (systemd mounted it by path,
  through the owner's symlink), or shows a folder the agent would not open now (its owner
  no longer passes the account check, say), off at once, in use or not; a folder of a
  guest that fails the check stays mounted until a reboot. There are no units any more:
  nothing mounts the folders at boot, the panel binds them at its start, and until the
  API starts no shared folder is mounted. A folder its owner swapped for a symlink, a
  folder of an account in a system or privileged group (`users`, `sudo`, `docker` … — the
  rules of an account's own units) the agent does not bind (a volume of your own at the
  account's `data` or deeper down, for a site's files, is no hindrance): its status is `failed` with why,
  and the `shared folders` line of `mp doctor` names it; once the cause is fixed, run
  `mp site fix <domain>`. The folders of a guest in a system group can be taken away all
  the same: `mp user share rm` and `mp user rm` unbind them by the agent's record, without
  the account check. Folders the panel's database does not have (after a restore of it
  from a copy, say) are unbound by the panel at its start, and where the agent keeps
  another directory than the database names, the panel binds the database's — so after a
  site's docroot changes, its shared folders move to the new docroot at the panel's next
  start (or with `mp site fix`) as well. The API passes the agent's refusal to give or
  take away a folder on with its status — 403, 409 (in use), 503 (an extraction, a
  restore or another long walk of a tree below the web root is under way, try again
  later) — not as a 502. Mount units of your own below the web root the panel never
  starts or enables — they are for `systemctl`. The
  shared folders of guests with a shell move to `/var/www/.shares/<guest>/<name>`, with a
  symlink left in the home; a folder in use moves at the next start or after
  `mp site fix <domain>`. In detail:
  [docs/en/10-shared-folders.md](docs/en/10-shared-folders.md); what becomes of the folders
  after going back: [Going back to an older version](#going-back-to-an-older-version).
- When systemd refuses the panel a start or restart of a service for its start limit (it
  was restarted too often, not crashing), the agent resets it and runs the start once
  more; a crashing service it does not bring up this way. A PHP branch's optional
  extensions install in one run of apt or dnf: with one package per run the branch's
  php-fpm restarted more often than systemd allows, and `mp php install` failed. A
  fail2ban that is enabled but not running the panel restarts at the next refresh of the
  jails (a site created or deleted, a start of the panel), and a running one once a site
  log on its list appears (a site suspended under 0.8.19 and unsuspended, say).
- New and changed lines of `mp doctor`: `shared folders` (the shared folders that are not
  mounted, and why, the folders where another directory is mounted, and the folders the
  agent keeps that the panel's database does not name), `apache move` (the sites still on
  the shared Apache), `restic sandbox` (the version of the restic that started in the
  sandbox; its error is a failure, a self-test that did not finish within 30 s a
  warning), `kernel modules` in a container (the host's kernel loads the
  modules), `update attempt` (`mp update apply` installs a version that rolled back). The
  API judges the `api socket` line itself: a warning when it listens on the port itself
  although the socket could hold it (enabled or not), and when the socket that passed it
  the port has been stopped since (Stop on the Services page, or
  `systemctl restart monopanel-api.socket` while the API runs): once the API stops or
  crashes, the port is free. `mp setup` brings the socket back (a stopped one,
  `systemctl restart monopanel-api` too). To have the API listen on the port itself on
  purpose, mask the socket: `systemctl mask monopanel-api.socket` (on an install without
  the package remove `/etc/systemd/system/monopanel-api.socket` first), and `mp setup`
  leaves the mask alone. `jobs` does not count a failed job that the same work fixed
  later, nor a job of a site or an account deleted after it (`mp site rm`, `mp user rm` —
  and the jobs of the sites deleted along with an account), and a `site.apply` of a site
  deleted while the job waited in the queue ends done; the line counts the day from a
  job's failure, not from its queueing
  ([docs/en/04](docs/en/04-cli-tui-api.md#2-cli--monopanel-alias-mp)).
- A job still runs for an hour at most, but a backup, a restore and a migration are no
  longer cut off after an hour: a backup (`backup.run`) gets 8 hours, a restore
  (`backup.restore`) 7, a migration (`migrate.run`) a day ([docs/en/01 §3](docs/en/01-architecture.md#3-components)).
  While such jobs hold every worker (`jobs.workers`), one more worker takes the other
  jobs one at a time, so that a site's apply or a certificate's renewal does not wait for
  hours. The automatic update of the panel (it restarts the API) waits while a backup, a
  restore or a migration is queued or running; an update an administrator starts does
  not wait and interrupts them: a job starts over after the restart, while a migration
  stops on the account it made — remove it (`mp user rm <login> --purge`) and run the
  migration again.
- A site or an account whose removal was asked for takes no more changes: the changes of
  a site (`mp site set`, `apply`, `suspend`, `unsuspend`, `fix`, `mp site nginx`, ordering
  its certificate, a CMS install) and of an account (`mp user set`) are refused with a 409
  instead of being answered as accepted; a refusal means nothing changed, the password
  included. The old way out of a stuck removal mark (unsuspending, applying the site,
  changing the account's status) is gone: asking for the removal again marks the row anew
  and queues the job, and a removal of an account asked for again while the first one
  still waits ends without an error. A removal broken off by a restart of the panel goes
  on after the start with the same mark; a removal that fails lifts it: a site goes to
  `error`, an account to `suspended`. The 202 answers to a site's changes
  (`PATCH /sites/{domain}` and the site actions) and the answer to `PATCH /users/{login}`
  carry the row as stored; if the SFTP/SSH password cannot be set, the 502 says that the
  rest of the change and the panel password are saved.
- On Ubuntu the update to 0.9.0 itself runs 0.8.19's updater, whose apt run may let
  needrestart restart the services left on old libraries, the database server among them.
  To avoid that, update by hand:
  `NEEDRESTART_MODE=l apt install ./monopanel_0.9.0_<arch>.deb`. A self-update from
  0.8.19 that rolled back is not mended by a retry — see
  [Releases and updates](#releases-and-updates).
- The web UI's console runs no command that reads or writes files on the server
  (`files get|put`, `ssl import`, `site nginx --set`, `migrate --key`), nor
  `mp update trust`: the panel's pages or `mp` over SSH do that.
- Mail policy. Before 0.9.0 a site or a mailbox of one account could send mail as a
  domain of another account on the same server, and opendkim signed it with that
  domain's key. Now postfix asks the service `monopanel-mailpolicy` about every message
  before opendkim and refuses such a message: "550 5.7.1 <domain>: this server does not
  send mail as a domain of another account" ([docs/en/06 §11](docs/en/06-mail.md#11-mail-policy)).
  On a server with mail, the API's first start after the upgrade queues a `mail.apply`
  (by `system`): the policy's socket and `monopanel-mailpolicy` are enabled (on EL also
  the nftables package and `monopanel-mailguard`), the policy file is written, `main.cf`
  puts it first (`default_action=tempfail`) before opendkim, `master.cf` gets
  `pickup-cleanup`, opendkim's `TrustedHosts` become the server's addresses only, and
  dovecot gets `sieve_vacation_send_from_recipient` and
  `sieve_redirect_envelope_from = recipient`. Check: `mp job list` (`mail.apply` done)
  and the `mail policy` line of `mp doctor` (ok). If the service cannot run, the job
  fails and `main.cf` stays as it was (retried hourly). The panel watches `main.cf` and
  puts the policy back where it is missing (the package removed and installed again, a
  downgrade and an upgrade again). What else changes: sieve redirects leave with the
  forwarding mailbox as envelope sender, and redirects of mail from another account's
  domain are refused (the refusal goes to the forwarder); vacation replies leave with
  the mailbox as envelope sender; an account cannot take another account's mail domain
  as a site or an alias. Going back below 0.9.0: the package's script takes the policy
  out of `main.cf` and reloads postfix itself (mail flows without the policy); upgrading
  again brings the policy back by itself.

Known limitations of 0.9.0, which the next release lifts
([docs/en/05](docs/en/05-roadmap.md#what-090-leaves-for-the-next-release)):

- DKIM switched on after the mail server was installed without it
  (`mp mail settings --dkim true`, the web UI, the TUI) only renders the configuration:
  the install's DKIM steps — the keys directory `root:opendkim 0750`, postfix in the
  `opendkim` group, opendkim enabled and started — do not run, so on EL 10 opendkim
  defers signed mail (451), and on Debian and Ubuntu mail goes out unsigned. Once DKIM is
  on, run `mp mail install` again.
- EL: while the mail server's host name has no valid certificate, postfix cannot read
  the panel's self-signed stand-in (`/var/lib/monopanel/mail/tls.crt`) under SELinux,
  and STARTTLS on 587 answers "TLS not available". Lifted in 0.9.1: the files get the
  `cert_t` label at its first start.
- A sieve redirect (a Roundcube filter) of a message whose sender is a domain of another
  account on this server is refused by the mail policy: the refusal, with the message in
  it, returns to the forwarding mailbox. Forwarding by the panel's aliases is not
  affected — forward such mail by an alias.
- A suspension closes the panel only — signing in, the sessions, the API tokens and `mp`
  over the local socket; SFTP/SSH, sites, databases and cron keep working. To close
  SFTP/SSH too, give the account a new password (`mp user set <login> --generate`) and
  take its keys out of `~/.ssh/authorized_keys`.

More in [docs/en/05-roadmap.md](docs/en/05-roadmap.md) and the documents it links to.

### Going back to an older version

The panel never installs a version older than the installed one: an older release would
bring back what later ones fixed, the agent's own checks included — and the refusal of
`mp update apply` points here. Should going back be unavoidable, root installs the older
package by hand, over the newer one:
`apt install --allow-downgrades ./monopanel_<version>_<arch>.deb` or
`dnf downgrade -y ./monopanel-<version>.<arch>.rpm` (without `-q`: with it dnf does not
show the script's notice, which then goes only to `/var/log/dnf.rpm.log`). Install it over
the newer one: then the package's script says what breaks and what to do. After
`apt remove` or `dnf remove` the script cannot know that an older version follows: it
keeps the accounts' Apache template and the php-fpm socket directory's line all the same,
but names only what stops — the steps below are needed then too.

Going back from 0.9.0 or later to a version older than 0.9.0 takes steps: the older
version does not know what 0.9.0 changed on the server. The package script of 0.9.0 and
later does not stop the installation (a failing prerm would leave dpkg with a
half-installed package and rpm with both versions at once); it keeps the accounts' Apache
template and the php-fpm socket directory's line, and prints what breaks and what to do.
In detail:

- the site logs are in `<log_dir>/sites` (`/var/log/monopanel/sites`), while the older
  version's logrotate and fail2ban look at the accounts' `data/logs`: the logs are not
  rotated and attacks on the sites go unseen. The older package also hands
  `/var/log/monopanel` back to the user `monopanel`, so the logs root opens there sit
  below a directory of the service user — apply the sites right away;
- the php-fpm socket directory `<run_dir>/php` (`/run/monopanel/php`): the older
  version's `mp setup` writes `/etc/tmpfiles.d/monopanel.conf` without its line, and after
  a reboot no php-fpm would start (502 on every PHP site). The script puts
  `d /run/monopanel/php 0755 root root -` into `/etc/tmpfiles.d/monopanel-php.conf` when
  that file is missing; keep it while a version older than 0.9.0 runs. On an install from
  the binary alone, `mp setup` of 0.9.0 and later writes that file;
- sites in Apache mode run on their accounts' Apaches (`monopanel-apache@<login>`), and
  the distribution's `apache2.service` is off. The older package has no
  `monopanel-apache@.service` template: the script copies it to `/etc/systemd/system`,
  so the accounts' Apaches survive a reboot. But a site the older panel applies (an edit,
  a certificate) it sends to the shared Apache, and until `mp stack install apache` that
  site answers 502;
- apps with an env file start through `monopanel app-exec`, which the older version
  lacks, and do not come up at their next start — the script names them;
- the 0.9.0 agent binds the shared folders, with no units: until a reboot they stay
  mounted (the shell guests' ones in `/var/www/.shares`, reachable through the link in
  their homes; the older panel shows them unmounted), and after one none is mounted. The
  folders of SFTP-only guests the older version's `mp site fix <domain>` mounts again,
  writing their units; those of shell guests the older version cannot mount — leave them
  be until the next upgrade (`mp user share rm` under the older version leaves the mount
  behind);
- the panel's port: nothing to do — before the older version starts, the API unit's
  drop-in stops the socket, and it listens on the port itself; the drop-in can go:
  `rm /etc/systemd/system/monopanel-api.service.d/socket.conf && systemctl daemon-reload`;
- the mail policy: the older version does not know it — the package's script takes it
  out of `main.cf` and reloads postfix itself, and mail flows without it, as before
  0.9.0: an account can send as another account's domain again. On EL the nftables
  table of `monopanel-mailguard` stays loaded until a reboot, in nobody's way.

Once the older version runs, as root, in this order:

```sh
mp stack install apache          # sites in Apache mode: the shared Apache again, before the sites
mp site list | awk 'NR > 1 {print $1}' | while read -r d; do mp site apply "$d"; done
mp mail apply                    # if the webmail has a port of its own
mp app set <name> --user <login> # every app with an env file (mp app list)
systemctl disable --now monopanel-apache@<login>   # each account's Apache, once its sites are applied
```

`mp site apply` writes a site's nginx, pool and Apache the way the older version makes
them: the logs go to `data/logs` again, which its logrotate and fail2ban already watch (it
rewrote both at its start); nothing writes the files in `/var/log/monopanel/sites` any
more — keep or remove them. `mp app set` with no change flags (`--user` alone) rewrites
the app's unit and restarts it; the older version does not read the root-only variable
files in `/etc/monopanel/apps/`.

Back to 0.9.0 or later: the package's upgrade removes the copy of the Apache template
from `/etc/systemd/system` itself, and `/etc/tmpfiles.d/monopanel-php.conf` stays: one
line in two tmpfiles files is no warning. The shared folders' units the older version
wrote the agent takes over at its start, and the panel binds the folders. The one-time
moves of 0.9.0 remember they were done and do not repeat by themselves after such an
upgrade: run `mp stack install apache` (if sites run in Apache mode), `mp site apply` for
every site, and `mp mail apply` if the webmail has a port of its own. The panel puts the
mail policy back into `main.cf` by itself.

### Removing the panel

`apt remove monopanel` / `dnf remove monopanel` stops and disables the panel; what it set
up keeps running. Sites in Apache mode keep their accounts' Apaches: the script copies the
`monopanel-apache@.service` template to `/etc/systemd/system`
(`systemctl disable --now monopanel-apache@<login>` stops one for good; the next install
of the package removes the copy). `/etc/tmpfiles.d/monopanel-php.conf` keeps making
`/run/monopanel/php`, where the sites' pools listen, at boot. Apps with an env file of
their own start through `monopanel app-exec`, which goes with the package: they fail at
their next start, and the script names them. Before removing the panel, switch them to
panel variables (`mp app set <name> --user <login> --env-file '' --env KEY=…`) or remove
them; installing the panel again brings them back. Shared folders stay mounted until a
reboot: the agent binds them, with no units, and after one none is mounted.

A removal from a host `mp setup` never set up leaves nothing behind. Where the line for
`/run/monopanel/php` cannot be kept (`monopanel-php.conf` holds something else), the
script says what to put there before an older version is installed. dnf shows the
script's notice only without `-q`. To retire the accounts' Apaches for good:
`systemctl disable --now monopanel-apache@<login>` for each, then
`rm /etc/systemd/system/monopanel-apache@.service && systemctl daemon-reload`
(`apt purge` leaves the copy too).

## Moving in

An account moves as a whole: the unix user with its password, the sites, the
databases with their passwords, cron, certificates — and between two MonoPanel servers
also mail, app services (switched off) and the settings of its Valkey instances. The move runs on the **new**
server and only ever reads the source — not one file changes there, so the old
server stays a fallback until DNS is switched. `plan` creates nothing and says what
would arrive and what stands in the way: a login already taken (`--as <login>`),
a domain or database that already exists here, a missing PHP branch
(`mp php install 8.2`), too little disk.

**From another MonoPanel server.** The old server issues a token — for one
account, read-only, with a lifetime:

```bash
mp migrate grant user:alex                                             # on the old server
mp migrate plan --source https://old:8443 --token … --scope user:alex   # on the new one: dry run
mp migrate run  --source https://old:8443 --token … --scope user:alex   # on the new one: move
```

**From BitrixVM / bitrix-env 7–9.** Needs root ssh to the old server: a key
(`--key`, default `~/.ssh/id_ed25519` of whoever runs `mp`) or a password
(`--password-stdin`). The main BitrixVM site answers to `server_name _`, so
`--domain` names it; the extra sites in `/home/bitrix/ext_www` come under their
own names. There is one account, `bitrix`; take it under another login with `--as`.

```bash
mp migrate plan --from bitrixvm --source root@old.example.com --domain shop.example.com
mp migrate run  --from bitrixvm --source root@old.example.com --domain shop.example.com
```

Database credentials come from `bitrix/.settings.php` (or `dbconn.php`) and the
MySQL account is recreated with the same password; `/home/bitrix` paths are
rewritten in `dbconn.php`, `.settings*.php`, cron commands and the symlinks of link
sites. The Bitrix cache, the push server, memcached and msmtp of the environment do
not travel (`mp stack install memcached` if needed). Works from bitrix-env 7 on
CentOS 7 too: the MySQL 5.7 dump is adjusted for MySQL 8 on the way.

**From FASTPANEL 2.** The same root ssh. Without `--scope` the dry run lists the
panel's accounts:

```bash
mp migrate plan --from fastpanel --source root@old.example.com                 # who is on the source
mp migrate run  --from fastpanel --source root@old.example.com --scope user:shop [--as shop2]
```

Sites arrive with their PHP version and mode (php-fpm or Apache), docroot, aliases
and allow-lists, databases with their password hashes, certificates (uploaded and
Let's Encrypt), cron. FASTPANEL mail does not move — the dry run says how many
mailboxes to recreate.

**After the move** (any source):

1. Check the site on the new server without touching DNS:
   `curl --resolve shop.example.com:80:<new IP> http://shop.example.com/`.
2. Give an account from a foreign panel a web-panel password: `mp user set <login>
   --generate` (the unix password for SFTP is already the old one).
3. Switch DNS. The certificates that came along work at once and the panel renews
   them; a site without one gets `mp ssl issue <domain>`.

If DNS is broken on the old server (a dead nameserver in `resolv.conf`), every ssh
command of the dry run takes seconds — fix `resolv.conf` there. What moves and what
does not, in detail: [docs/en/07-migration.md](docs/en/07-migration.md).

## Stack

| Layer | Choice |
|---|---|
| Core, CLI, menu | Go 1.27, one static binary (`CGO_ENABLED=0`) |
| HTTP / API | `chi` + `huma` v2 (OpenAPI 3.1 from types), SSE for job progress |
| State | SQLite (WAL) via `modernc.org/sqlite`, embedded SQL migrations |
| Web UI | Svelte 5 + SvelteKit 2 (static), TypeScript, Tailwind 4 — built into `web/build` and embedded in the binary; responsive: the menu becomes a drawer on a phone and tables become cards |
| CLI / TUI | `cobra`, Bubble Tea v2 + Lip Gloss v2 |
| ACME | `lego` as a library |
| Mail | postfix + dovecot (IMAP/POP3/LMTP/sieve) + opendkim, Roundcube webmail |
| systemd | D-Bus (`go-systemd`) |
| Packaging | `nfpm` → .deb/.rpm, releases with signed checksums |

## Development

```bash
make build            # dist/monopanel
make check            # gofmt + go vet + golangci-lint + tests + the web UI type check
make web              # the web UI into web/build (needs node and pnpm)
make help             # every target
```

| Command | What it does |
| --- | --- |
| `make test` | unit tests; the panel's logic is exercised through a fake agent, without root or systemd |
| `make check` | the above plus `gofmt`, `go vet`, `golangci-lint` and `svelte-check` — what CI runs |
| `make test-race` | the race detector (about a minute): job queue, SSE broker, certificate cache |
| `make cover` | coverage per package; `make cover-html` opens the report |
| `make web-check` | types and markup of the web UI (`svelte-check`) |
| `scripts/check-templates.sh` | feeds the generated configuration to a real `nginx -t` and `apache2 -t` (the whole configuration of an account's Apache with its virtual host) |
| `make e2e` | a scenario against a live panel: account → site with a preset → database → removal |
| `make testbed-matrix` | the same scenario on the eleven testbed VMs (one per OS of the matrix) in parallel, each rolled back to a clean snapshot first; `make testbed-migrate SRC= DST=` moves a real account between two of them and checks what arrived |

The fake agent (`internal/agent/agenttest`) listens on a unix socket and answers
the privileged operations while recording everything the panel tried to do. A test
can therefore read the generated nginx server block and php-fpm pool and check
behaviour end to end: CMS presets, IP allow-lists, suspending a site, cascading
account removal, update signature verification.

`internal/parity` checks the three clients against the API: every operation needs an
`mp` command and a place in the web UI, every command a place in the TUI menus, every
request field a flag and a field on a page, and no administrator's action may reach an
ordinary account. The test names what is missing where; the deliberate exceptions are
listed in the test itself and in [docs/en/04-cli-tui-api.md](docs/en/04-cli-tui-api.md#1-principle).

End-to-end runs against a real panel and removes everything it created:

```bash
make e2e HOST=<ssh alias>   # mints a token over ssh and revokes it afterwards
# or explicitly:
MONOPANEL_URL=https://panel:8443 MONOPANEL_TOKEN='…' make e2e
```

CI on every push: tests with the race detector and coverage, the linter, template
validation against a real `nginx -t` and `apache2 -t` (the whole configuration of an account's Apache with its virtual host), the web UI build with type checking, and
binaries for amd64 and arm64. E2E runs on demand (`workflow_dispatch`) because it
needs a live host. The OS matrix runs from a workstation against a Proxmox testbed
([docs/en/08-testbed.md](docs/en/08-testbed.md)): the scripts live in `scripts/testbed/`, the
host and network in the git-ignored `.dev/testbed.env`.

### Layout

```
cmd/monopanel/        entry point
internal/api/         HTTP API (huma + chi), SSE, UI, TLS, job handlers
internal/agent/       the privileged agent: ApplyConfigSet, EnsureUnixUser, Service, Pkg
internal/jobs/        job queue, workers, event broker
internal/store/       SQLite, migrations, models
internal/osprofile/   Debian/RHEL differences
internal/render/      template rendering with golden tests
internal/updater/     finding a release, verifying it, installing with rollback
internal/cli/ tui/    the mp commands and the terminal menu
internal/client/      Go client for the API (CLI, menu, setup)
internal/parity/      the check that the web UI, mp and the menu can do the same things
templates/            nginx/, apache/, php-fpm/, systemd/
web/                  SvelteKit application (build/ is embedded in the binary)
web/static/monaco/    the VS Code editor (Monaco), trimmed build — see its README
packaging/            nfpm.yaml, units, sysusers/tmpfiles, install.sh
scripts/release/      key generation and SHA256SUMS signing for a release
scripts/testbed/      the testbed: VMs on Proxmox, panel bootstrap, matrix and migration runs
```

## Documentation

| Document | Contents |
|---|---|
| [docs/en/01-architecture.md](docs/en/01-architecture.md) | Goals, architectural decisions, components, the data model, the pipeline that applies configuration, security, observability, packaging |
| [docs/en/02-platform-matrix.md](docs/en/02-platform-matrix.md) | Supported OSes, package sources, the PHP and extension matrix, MySQL/Percona, the OS profile, SELinux, the firewall |
| [docs/en/03-web-stack.md](docs/en/03-web-stack.md) | The nginx+php-fpm and nginx+Apache modes, the file layout, templates, isolation and limits, TLS/ACME, HTTP/3, logs |
| [docs/en/04-cli-tui-api.md](docs/en/04-cli-tui-api.md) | The three clients and their deliberate differences, CLI commands, TUI areas and keys, the REST API, billing integration (WHMCS) |
| [docs/en/05-roadmap.md](docs/en/05-roadmap.md) | Development stages, the CI matrix, test scenarios, risks |
| [docs/en/06-mail.md](docs/en/06-mail.md) | Mail: postfix + dovecot + opendkim, the path of a message, files and ports, DNS records, webmail, limits |
| [docs/en/07-migration.md](docs/en/07-migration.md) | Moving in: between two MonoPanel servers and from BitrixVM/FASTPANEL over ssh — what moves and how, the order around the DNS switch, the migration bundle and the other adapters (design) |
| [docs/en/08-testbed.md](docs/en/08-testbed.md) | The testbed: a VM on Proxmox for every distribution in the matrix, the e2e and panel-to-panel migration runs, what it found |
| [docs/en/10-shared-folders.md](docs/en/10-shared-folders.md) | Shared folders: a contractor's SFTP access to one folder of a site — step by step, how it works, file permissions |
| [docs/en/09-web-ui.md](docs/en/09-web-ui.md) | The web UI page by page, with screenshots in the light and dark themes: dashboard, sites, PHP, databases, mail, files, jobs, firewall, backups, settings; how to retake the screenshots |

The same documents in Russian are in [docs/](docs/).
The API reference is served by the panel itself at `/api/v1/docs` (OpenAPI 3.1) once you are signed in or present an API token; the reference and the specification are closed to anonymous visitors, Stoplight Elements is bundled into the binary and the page loads no third-party scripts.

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
