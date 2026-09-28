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

Status: **0.8.12**, in daily use on a production server hosting several sites and
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
small policy module of its own that a hosting server needs. Ubuntu 26.04 has no `ppa:ondrej/php` builds yet, so PHP comes
from `packages.sury.org` there — the same maintainer's own repository, which carries every branch for 26.04; without
it too, PHP comes from Ubuntu itself.

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
mp                              # terminal menu
```

Remotely: `mp --server https://host:8443 --token <token> status`
(mint one with `mp token create`).

## What it does

Everything below goes through one REST API and is available in the web UI, the CLI
and the terminal menu. Under each heading are that area's `mp` commands.

### Sites

`mp site add|set|apply|fix|suspend|rm|logs`, `mp site nginx|php|tls`, `mp selinux`

- Three modes: `fpm` (nginx → php-fpm), `apache` (nginx → Apache on loopback 8080 via mod_proxy_fcgi) and `proxy` (nginx → your own backend).
- Every site gets its own php-fpm pool, ACLs for the `monopanel-web` group, a placeholder page and an automatic certificate; a suspended site serves a 503 page.
- Access only from listed addresses (`--allow`), HSTS when HTTPS is forced, custom nginx directives in `sites/<domain>.d/*.conf`. `mp site nginx <domain> --set file` writes them with an `nginx -t` check and a rollback; `mp site php <domain>` shows the effective PHP settings and where each one comes from.
- CMS presets `--preset wordpress|joomla|bitrix|opencart` (`mp site presets`): their own nginx locations (pretty URLs, Joomla `/api/`, Bitrix `urlrewrite.php`, OpenCart `_route_`, denied service directories, no PHP execution in uploads) and PHP defaults. Bitrix follows the BitrixVM rules: opcache for 100,000 files, no open_basedir.

### PHP 5.6–8.5

`mp php list|install|remove`, `mp php ext list|enable|disable`, `mp php ini [set|unset]`

- Branches from Sury, the ondrej PPA or Remi, as many side by side as you like; each has its own php-fpm and `99-monopanel.ini`.
- php.ini values stack in layers: panel → global (`mp php ini set`, the PHP page) → preset → site (`mp site set --ini`). Any parameter can be changed for one site or for all of them.
- A branch's extensions can be switched off and on (Debian/Ubuntu — `phpenmod`/`phpdismod`, EL — editing `extension=` in the Remi branch's ini, then a php-fpm restart). It applies to every site on the branch: there is one php-fpm master per version.
- Extensions the repository offers but that are not installed (memcache, redis, imagick …) appear in the same list; enabling one installs the package.

### CMS

`mp cms list|install <domain> <cms>`

- Installs WordPress, Joomla, OpenCart and 1C-Bitrix into an existing site: the distribution comes from the vendor's site, is unpacked into the docroot as the client, gets its own database, and the site gets the matching preset.
- The installer is the CMS's own: wp-cli, the Joomla and OpenCart CLI installers, the Bitrix web wizard, which the panel walks through itself. For Bitrix — the trial Start, Standard, Small business or Business edition, the marketplace's clean install or the demo site.
- The administrator credentials are shown once. In the web UI — the CMS tab on the site page.

### Databases

`mp stack install percona|mysql`, `mp db create|list|passwd|rm`, `mp db config [set|unset]`

- Percona Server or MySQL 8.4 LTS; root over `auth_socket` (on EL the panel moves it off the package's temporary password itself), settings sized to the available RAM.
- Server settings are changed in the panel: a value takes the panel's place in `zz-monopanel.cnf`, `mysqld --validate-config` runs before the restart, and if the server fails to start the previous settings come back by themselves.
- Databases are named `<login>_<name>`, generated passwords satisfy `validate_password`; `mysql_native_password` is enabled only for PHP < 7.4.

### Valkey per account

`mp stack install valkey`, `mp valkey add|list|restart|rm cache|sessions --user <login> [--memory MB]`, `mp site set <domain> --sessions valkey|files`

- Two separate instances per account: a cache (allkeys-lru, nothing on disk) and PHP sessions (volatile-lru, a snapshot every minute — sessions survive a restart).
- Each is a systemd unit `monopanel-valkey-<login>-<cache|sessions>` running as the account with a memory limit. It listens only on the unix socket `/run/monopanel-valkey/<login>-<cache|sessions>/valkey.sock` with mode 600: neither other accounts nor the web server can connect, and there is no TCP port at all. On EL the instances run in the SELinux domain `redis_t`.
- Valkey comes from the distribution (where it has none — Ubuntu 22.04, Debian 12 without backports — Redis with the same protocol); the package's shared instance is switched off.
- A site moves its sessions to Valkey with one setting (the redis extension of its PHP branch is needed); while a site keeps sessions in an instance, neither the instance nor the extension can be removed. Sessions are locked by phpredis (`redis.session.locking_enabled`, as files are), and the `redis.session.*` keys are tunable among the PHP settings.
- In the web UI — the valkey button on the Users page and the PHP sessions field in the site settings.

### TLS

`mp ssl issue|list|renew|rm`, `mp site tls <domain>`, `mp ssl panel issue|import|self-signed`, `mp dns-provider add`

- lego: HTTP-01 through the nginx webroot, DNS-01 (Cloudflare, Hetzner, DigitalOcean, Gandi, deSEC, Namecheap, RFC2136) for wildcards, renewal 30 days ahead.
- The panel's own certificate and the sites' certificates are kept apart: the panel orders for its hostname only and picks it up live, a site orders for its domain and aliases and switches itself to HTTPS. A certificate in use cannot be deleted.
- Certificates issued elsewhere — `mp ssl import --cert --key`.

### Mail

`mp mail install|status|settings|domain|box|alias|webmail`

- postfix + dovecot + opendkim: domains, mailboxes (passwords and quotas in the panel, Maildir owned by `vmail`), aliases and catch-all, IMAP/POP3/submission on the panel's certificate, DKIM signing, sieve filters.
- Send-only domains: the provider receives the domain's mail, this server signs with DKIM and sends the sites' mail. A lenient domain (`--lenient`) also accepts mail from badly configured senders.
- `mp mail domain dns` prints the required MX, SPF, DKIM, DMARC and PTR records and checks them against public resolvers.
- Roundcube webmail installs as a regular panel site or on a port of the mail host (`--port 2096` — no DNS record and no second certificate).
- More in [docs/en/06-mail.md](docs/en/06-mail.md).

### Backups

`mp backup target add|run|list|snapshots|restore`

- restic in local, SFTP, S3, B2 or REST: files, MySQL dumps and a copy of panel.db.
- Retention by days, weeks and months, a daily schedule; restore into `<data>/restore/<snapshot>` or in place.

### Files

`mp files ls|put|get|mkdir|rm|mv|chmod|extract|size`

- File operations run through `monopanel fsop` behind a helper that drops privileges irreversibly; paths are relative to the account's home.
- The web UI has a file manager with an editor: browsing, drag-and-drop upload, permissions, archive extraction. Files are edited in the VS Code editor (Monaco): highlighting for php, html, css, js, sql, yaml and ini, find and replace, multiple cursors, folding, F1 for the command palette.
- A site's Files tab opens at its docroot.

### Users, SFTP and SSH

`mp user add|set|list|show|totp-reset|rm`, flags `--shell|--sftp-only --password`

- A panel account is a unix user. SFTP-only means a chroot into `/var/www/<login>` via `sshd_config.d/monopanel.conf`; one password for the panel and SFTP; a shell is given by a flag, and with it access to `mp` over the local socket (group `monopanel-cli`); requests to the socket from php-fpm, Apache and nginx processes are refused.
- `mp user rm <login> [--purge]` removes the account together with its sites, databases, cron, app services and certificates.

### Cron

`mp cron add|list|enable|disable|rm`

- The account's crontab is rendered whole from the panel database, with `~/data/bin` on PATH (the php of the right version).

### App services

`mp app add|set|start|stop|restart|logs|rm`

- A systemd unit `monopanel-app-<login>-<name>` running as the account, for gunicorn, node, bots: command, working directory and env-file confined to the home directory, autostart, logs via journalctl.

### Apache and extensions

`mp stack install apache`, `mp stack install|remove memcached|jpegoptim|git|composer|sphinx`, `mp stack memcached --memory-mb --max-conn`

- Apache 2.4 on Debian/Ubuntu: mpm_event + proxy_fcgi, `conf-available/monopanel.conf`.
- The "Extensions" page in the web UI: memcached (127.0.0.1 only, memory and connections adjustable, restarted on change), jpegoptim, git, composer (from getcomposer.org with the published checksum, running on the newest PHP branch the panel installed; installing again updates it).
- Sphinx for 1C-Bitrix: the distribution's Sphinx 2.2 on Debian/Ubuntu, the sphinxsearch.com build of Sphinx 3 on EL; the `bitrix` index as Bitrix's own settings page documents it, a stale index on disk is recreated, SphinxQL on 127.0.0.1:9306.

### Firewall and real IP

`mp firewall enable|allow|deny|ban|unban`, `mp stack install fail2ban`, `mp stack real-ip --cloudflare [--from CIDR]`

- The nftables table `inet monopanel` with a drop policy; SSH, 80, 443 and the panel port are always open; the `monopanel-firewall` unit.
- Allows with a source are checked before denies: `deny --port 8443` + `allow --port 8443 --source <VPN>` limits the panel to the VPN. A deny that would lock you out (SSH or the panel with no per-source allow, or your own current address) is refused.
- fail2ban jails for sshd, nginx and the panel itself.
- Trusted proxies for nginx `real_ip` (Cloudflare ranges built in), so allow-lists and logs see the visitor rather than the proxy.

### Metrics, logs and diagnostics

`mp metrics`, `mp site logs`, `mp logs <unit>`, `mp doctor`

- A sampler every 10 s, stored as one point per minute for 30 days; site and journald log tails through the agent.
- Site logs rotate weekly or at 100 MB under their own account: eight copies, compressed from the second one.
- `mp doctor` checks services, configs, disk, certificates, DNS, jobs and drift of the generated files.
- The audit log — who signed in and from where, who created tokens, who changed what: `mp audit`, the "Audit log" page, `GET /system/audit`; the same feed goes to `/var/log/monopanel/audit.log` as rotated JSON lines, and sign-ins and refusals to journald as well.

### Console in the web UI

`POST /system/console`

- The "Console" page: `mp` commands with the signed-in administrator's rights and streamed output, a one-off token per command.
- The dashboard's doctor findings carry "fix site" and "back to enforcing" buttons.

### Migration between panels

`mp migrate grant|plan|run`

- A whole account moves to another MonoPanel server. The source issues a token scoped to one account and only ever reads; the target reports conflicts first (`plan` changes nothing) and then takes the account.
- Files and dumps stream straight through; panel, SFTP, MySQL and mailbox passwords travel as hashes, so users never notice the move.
- More in [docs/en/07-migration.md](docs/en/07-migration.md).

### Moving in from BitrixVM and FASTPANEL

`mp migrate plan|run --from bitrixvm|fastpanel`

- The same dry run and move, but from a foreign server over ssh (password or key); the source is only read.
- BitrixVM: sites from `/etc/nginx/bx`, database credentials from `.settings.php`; link-site symlinks and the paths in `dbconn.php` and cron are rewritten for the new home.
- FASTPANEL: the account, sites, PHP backends, databases with their password hashes, allow-lists, certificates and cron from its database and files.
- The CMS preset is detected from the site's files.

### Updates

`mp update`, `mp update apply`

- The panel finds a new version in this repository's releases, downloads the package for its OS, verifies the ed25519 signature and installs it from a separate systemd unit. If the new version does not answer, the previous binary comes back.

### Tokens, 2FA and webhooks

`mp token create|list|revoke`, `mp user totp-reset`, `mp webhook add`

- A token belongs to an account; an administrator can mint one for another account (`--user`), and root on the local socket gets one for the single administrator with no flags.
- TOTP 2FA with a QR code in the web UI, Bearer tokens, webhooks signed with HMAC-SHA256 on job events.

### Languages

- The web UI in English and Russian: picked from the browser language (CIS languages → Russian, everything else → English), switchable in Settings and on the sign-in screen, remembered per browser.
- The CLI and the TUI follow the terminal locale by the same rule; `MP_LANG=ru|en` sets it explicitly. API, job and diagnostic messages are in English.
- The placeholder page of a new site and the page of a suspended one follow the visitor's browser language.

### Not there yet

Own PHP builds (Sury/Remi are used instead), tested Apache on EL, phpMyAdmin, disk
quotas, per-site cgroup limits, a DNS server, a WAF, several servers from one panel,
an apt/yum repository (packages ship as releases and the panel installs them itself).
Mail runs on Debian/Ubuntu with dovecot 2.3 and 2.4 (Debian 13, Ubuntu 26.04 get a
configuration in the 2.4 syntax); the configuration for EL is not written yet, and
there is no content filter (rspamd). Accounts move between two MonoPanel servers and in from BitrixVM and
FASTPANEL, without a resync before the DNS switch; other panels and servers without a
panel are planned ([docs/en/07-migration.md](docs/en/07-migration.md)).

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
(`nginx -t`, `apachectl -t`, `php-fpm -t`) → every file written atomically → reload.
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

A version is cut by tagging; CI does the rest. The tag `v0.8.12` builds `.deb` and
`.rpm` for amd64 and arm64, signs the checksum list with an ed25519 key held as a
repository secret and publishes the release; the annotated tag's message becomes the
release notes.

```bash
make keygen                  # once: a signing key pair (the private half becomes the secret)
make release VERSION=0.8.12  # tag and push; CI builds and publishes
make packages VERSION=0.8.12 # the same artefacts locally, without publishing
```

On a server:

```bash
mp update settings --repo owner/name # --token-stdin for a private repository
mp update                            # what is installed and what is available
mp update apply                      # download, verify the signature, install, restart
mp update trust                      # which key the signature is checked against
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
make check            # gofmt + go vet + golangci-lint + tests (about 2 s)
make web              # the web UI into web/build (needs node and pnpm)
make help             # every target
```

| Command | What it does |
| --- | --- |
| `make test` | unit tests; the panel's logic is exercised through a fake agent, without root or systemd |
| `make check` | the above plus `gofmt`, `go vet` and `golangci-lint` — what CI runs |
| `make test-race` | the race detector (about a minute): job queue, SSE broker, certificate cache |
| `make cover` | coverage per package; `make cover-html` opens the report |
| `make web-check` | types and markup of the web UI (`svelte-check`) |
| `scripts/check-templates.sh` | feeds the generated configuration to a real `nginx -t` and `apachectl -t` |
| `make e2e` | a scenario against a live panel: account → site with a preset → database → removal |
| `make testbed-matrix` | the same scenario on the eleven testbed VMs (one per OS of the matrix) in parallel, each rolled back to a clean snapshot first; `make testbed-migrate SRC= DST=` moves a real account between two of them and checks what arrived |

The fake agent (`internal/agent/agenttest`) listens on a unix socket and answers
the privileged operations while recording everything the panel tried to do. A test
can therefore read the generated nginx server block and php-fpm pool and check
behaviour end to end: CMS presets, IP allow-lists, suspending a site, cascading
account removal, update signature verification.

End-to-end runs against a real panel and removes everything it created:

```bash
make e2e HOST=<ssh alias>   # mints a token over ssh and revokes it afterwards
# or explicitly:
MONOPANEL_URL=https://panel:8443 MONOPANEL_TOKEN='…' make e2e
```

CI on every push: tests with the race detector and coverage, the linter, template
validation against real nginx and Apache, the web UI build with type checking, and
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
| [docs/en/04-cli-tui-api.md](docs/en/04-cli-tui-api.md) | CLI commands, TUI screens, the REST API, billing integration (WHMCS) |
| [docs/en/05-roadmap.md](docs/en/05-roadmap.md) | Development stages, the CI matrix, test scenarios, risks |
| [docs/en/06-mail.md](docs/en/06-mail.md) | Mail: postfix + dovecot + opendkim, the path of a message, files and ports, DNS records, webmail, limits |
| [docs/en/07-migration.md](docs/en/07-migration.md) | Moving in: between two MonoPanel servers and from BitrixVM/FASTPANEL over ssh — what moves and how, the order around the DNS switch, the migration bundle and the other adapters (design) |
| [docs/en/08-testbed.md](docs/en/08-testbed.md) | The testbed: a VM on Proxmox for every distribution in the matrix, the e2e and panel-to-panel migration runs, what it found |
| [docs/en/09-web-ui.md](docs/en/09-web-ui.md) | The web UI page by page, with screenshots in the light and dark themes: dashboard, sites, PHP, databases, mail, files, jobs, firewall, backups, settings; how to retake the screenshots |

The same documents in Russian are in [docs/](docs/).
The API reference is served by the panel itself at `/api/v1/docs` (OpenAPI 3.1) once you are signed in or present an API token; the reference and the specification are closed to anonymous visitors, Stoplight Elements is bundled into the binary and the page loads no third-party scripts.

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
