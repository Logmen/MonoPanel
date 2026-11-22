# 01. MonoPanel architecture

As of October 2026 (MonoPanel 0.8.19). Component versions are given as of that date; before a release, a run of the OS matrix on the [testbed](08-testbed.md) checks that they are still current.

> This is a design document: it describes decisions and the reasoning behind them, including the parts the implementation has not reached yet. What actually works is in the [README](../../README.en.md) and in the [x] marks in [05-roadmap.md](05-roadmap.md). Below, such parts are marked "planned".

## 1. Goals and scope

**In scope**

- A web hosting control panel for a single server (in the class of FASTPANEL / ISPmanager Lite).
- OS: Debian 12/13, Ubuntu 22.04/24.04/26.04 LTS; RHEL 9/10 and its clones (AlmaLinux, Rocky Linux, Oracle Linux).
- Site modes: `nginx → php-fpm` and `nginx → Apache → php-fpm`.
- PHP 5.6 … 8.5 side by side, with the version chosen per site; extensions per version.
- Database server: MySQL 8.4 LTS **or** Percona Server for MySQL 8.4 LTS (chosen at installation, one provider in the code).
- Interfaces: web UI, CLI (scriptable, `--json`), TUI menu (interactive, over SSH), REST API (OpenAPI 3.1).
- TLS (ACME), backups, cron, firewall, brute-force protection, metrics, logs, file manager.

**Out of scope for 1.0** (see the [roadmap](05-roadmap.md)): a DNS server, managing several servers, containerised applications, a WAF, a reseller role. Mail (postfix + dovecot + opendkim + Roundcube) was not in the original plan; it came later and is described separately — [06-mail.md](06-mail.md).

## 2. Architecture decisions (ADR, in brief)

| # | Decision | Why |
|---|---|---|
| A1 | The panel core is one static binary written in **Go** | No runtime dependencies on the host (two OS families, eleven releases), one artefact for daemon + CLI + TUI, simple packaging into .deb/.rpm, fast CLI start-up, built-in HTTP server and TLS |
| A2 | **Privilege separation**: `monopaneld api` (user `monopanel`) ↔ `monopaneld agent` (root) over a unix socket | The code that parses untrusted input and serves the web UI does not run as root. The agent accepts only typed commands, with an allow-list of paths and operations |
| A3 | **One API for all clients**: the web UI, CLI, TUI and external integrations call the same REST API | No divergence in logic; anything you can do in the web UI you can do in the CLI and the TUI, and vice versa — the `internal/parity` test checks this (§11) |
| A4 | **Declared state → render → validate → apply atomically → reload, with rollback** | The panel is the source of truth; manual edits live in include directories; a broken config never reaches a reload |
| A5 | The panel serves its **own** HTTPS port (8443) with Go's `net/http`; systemd holds the port itself (`monopanel-api.socket`) and hands it to the process | The panel stays reachable even if the system nginx is broken or not installed yet; while the API restarts or is stopped the port stays bound, so no local user can put a fake panel on it |
| A6 | Panel state is kept in **SQLite** (WAL) | The panel must work when MySQL is not installed or has gone down; a backup of the state is a single file |
| A7 | PHP: **one php-fpm master per version, one pool per site**; Apache only through `mod_proxy_fcgi` | One FPM infrastructure for both modes; switching the mode = switching the nginx/Apache template. `mod_php` is not supported on principle (one version per Apache process, runs as `www-data`) |
| A8 | PHP builds: stage 1 — the Sury (deb) / Remi (rpm) repositories; stage 2 — **our own builds** with a single layout `/opt/monopanel/php/<X.Y>` | A quick start without a build farm; later, independence from third-party repositories (each is maintained by one person) and the same paths on every OS |
| A9 | nginx from **nginx.org** (stable) on every OS; Apache from the distribution | One nginx layout (`conf.d`, user `nginx`) and current versions with HTTP/3 on every OS; the distributions' Apache is recent enough and well integrated with MAC |
| A10 | OS abstraction — the **OS Profile** (`debian`, `rhel`): package manager, paths, service names, MAC (SELinux/AppArmor), firewall | All the differences between the families are confined to one code package |
| A11 | The ACME client is built in (the **lego** library) | No certbot/python on the host; HTTP-01 and DNS-01 (wildcard) from one place, one renewal logic |
| A12 | File operations on behalf of a client go through a **privilege-dropping helper** | Protection against symlink attacks and path substitution: the agent never touches a client's files as root |
| A13 | The internal api↔agent RPC is HTTP/JSON over a unix socket with `SO_PEERCRED` | Reuses the same types and tools as the external API; moving to multi-server swaps the transport for mTLS without rewriting the operations |

## 3. Components

```
                       ┌──────────────────────────────────────────────────────────────┐
 browser ──HTTPS:8443──▶  monopaneld api      (User=monopanel, unprivileged)            │
 CLI/TUI ──unix sock───▶  ┌──────────────┐ ┌─────────────┐ ┌───────────┐ ┌──────────┐ │
 WHMCS/scripts ─Bearer─▶  │ HTTP/API     │ │ Auth / RBAC │ │ Jobs      │ │ Render   │ │
                          │ chi + huma   │ │ argon2id    │ │ queue +   │ │ text/    │ │
                          │ SSE, WS      │ │ TOTP/WebAuthn│ │ workers   │ │ template │ │
                          └──────────────┘ └─────────────┘ └───────────┘ └──────────┘ │
                          ┌──────────────┐ ┌─────────────┐ ┌───────────┐              │
                          │ Scheduler    │ │ ACME (lego) │ │ Metrics   │  Web UI      │
                          │ renew/backup │ │             │ │ rollups   │  (embed.FS)  │
                          └──────────────┘ └─────────────┘ └───────────┘              │
                          SQLite WAL  /var/lib/monopanel/panel.db                     │
                       └───────────────────────────┬──────────────────────────────────┘
                                                   │ /run/monopanel/agent.sock
                                                   │ HTTP/JSON, SO_PEERCRED, allow-list of operations
                       ┌───────────────────────────▼──────────────────────────────────┐
                       │  monopaneld agent   (root)                                     │
                       │  ApplyConfigSet · EnsureUnixUser · SetACL · SetQuota           │
                       │  Service{reload,restart} (systemd D-Bus) · Pkg{apt,dnf}        │
                       │  MySQL{root via auth_socket} · Nft{apply} · SELinux/AppArmor   │
                       │  RunAsUser → helper (setuid) for client files                  │
                       └───┬──────────┬───────────┬──────────┬──────────┬──────────────┘
                           ▼          ▼           ▼          ▼          ▼
                        nginx      apache×N   php-fpm×N    mysqld    systemd / apt / dnf / nft
```

### 3.1 `monopaneld api` — the unprivileged process

- The panel's HTTPS server (port 8443, optionally bound to an IP of your choice; certificate: self-signed at installation → ACME for the panel's hostname). PID 1 binds the port in `monopanel-api.socket` (`BindIPv6Only=both`, `FreeBind=yes`) and hands it to the API (socket activation): while the API restarts, crashes, is stopped or has not started yet at boot the port stays bound, and no local account (a client with SSH, a site's PHP) can take it to collect administrators' passwords and TOTP codes. The address follows `web.listen` through the drop-in `/etc/systemd/system/monopanel-api.socket.d/listen.conf`, which only root writes: the agent when `web.listen` changes through the API, `mp setup`, `mp config set --local` and the package script (`monopanel api-socket-sync`). The API neither writes it nor chooses its content. A port below `net.ipv4.ip_unprivileged_port_start` keeps the socket off: no unprivileged process can take it anyway, and PID 1 would hand the API a port it cannot bind itself. Without a socket the API listens on `web.listen` itself, as before. A socket on another address the API closes on its side, with a warning in the log, and listens on `web.listen` itself too; should the socket hold the same port on another address (web.listen edited by hand, or a drop-in that could not be written), the API also stops it through the agent. The socket moves to `web.listen` only with `mp setup` or `mp config set --local web.listen …`; a reboot does not fix it. A failed start of the API that the socket made waits 2 s before it exits: otherwise a waiting connection would start it again at once, and after 20 starts in 2 s systemd would close the socket (trigger-limit-hit) and free the port. Where a running API has its port from, `GET /system/status` says (`panel.listen_via`: `socket` or `itself`).
- Serves the web UI (a static SvelteKit build embedded via `embed.FS`).
- REST API `/api/v1`: OpenAPI 3.1 is generated from Go types (`huma`), SSE for job progress. A WebSocket web terminal is planned; for now the web UI has the "Console" page: `mp` commands with output streamed over HTTP.
- Authentication: sessions (cookie `Secure; HttpOnly; SameSite=Strict`), Bearer tokens, TOTP; WebAuthn (passkeys) is planned. RBAC: `admin`, `user`. A token's scope restricts only migration tokens (`migrate:user:<login>`, which only administrators' tokens carry — §7); any other scope is just a label.
- Unix socket `/run/monopanel/api.sock` for the CLI (0660, group `monopanel-cli`): uid 0 → admin without a password; the uid of an account with shell access → that account's rights (a client on SSH manages their own sites with `mp`). Accounts without a shell are not in the group, and a request coming out of a php-fpm, Apache or nginx worker — through `exec()` too — is refused by walking the ancestry in `/proc`: a hacked site must not drive its owner's account.
- Job runner: a job queue in SQLite, N workers; each job is an idempotent sequence of steps with a log and progress; one job per entity at a time (a lock on `site_id`/`user_id`).
- Configuration rendering (`text/template`) from built-in templates, which can be overridden in `/etc/monopanel/templates/`.
- A scheduler for internal tasks: certificate renewal, scheduled backups, metrics collection, update checks.
- Stores ACME keys and client secrets encrypted (see 3.6).

### 3.2 `monopaneld agent` — the privileged process

- The same binary in `agent` mode, unit `monopanel-agent.service`, root.
- Socket `/run/monopanel/agent.sock` (0660 root:monopanel). A `SO_PEERCRED` check: only the `monopanel` uid and root are accepted.
- Operations are a closed, typed set (there is no "run this string in a shell"):
  - `config/apply` — a transactional write of a set of files: the previous versions go to `confhistory` → write to a temporary file + `rename` → validators (`nginx -t`, `apache2 -t -f /etc/apache2/monopanel/accounts/<login>/httpd.conf`, `php-fpm -t`) → on error, restore and return stderr; on success, reload the services.
  - `user/ensure|remove|password|shadow`, `group/ensure`, `dirs/ensure`, `file/ensure|read`, `sitelogs/ensure|remove`, `symlink/ensure`, `acl/set`, `chown`, `paths/remove`, `dir/list`, `stat`.
  - `service` — start/stop/reload/restart/enable/disable/status and daemon-reload through systemd D-Bus (`go-systemd`) rather than `systemctl`.
  - `pkg` — install/remove/query/available through apt/dnf according to the OS Profile, with a global lock and output logged to the job.
  - `tool` — a closed list of programs with argument checks: `mysql`/`mysqldump` (root via `auth_socket`, the root password is not stored), `restic`, `nft`, `crontab`, `semanage`/`restorecon`/`setsebool`, `firewall-cmd`, `postfix`/`doveadm` and others.
  - `runas` — the privilege-dropping helper for a client's file operations; `stream/in|out` — tar streams for migration; `panel/install` — installing the panel package during an update; `panel/config` — the panel's own settings in `config.yaml`, exactly five keys (`web.hostname`, `web.listen`, `log.level`, `log.format`, `jobs.workers`): the rest of the file stays byte for byte, its mode and owner stay as they were, and the previous version goes to `confhistory`.
- Allow-list of write paths (`DefaultAllowedWritePrefixes` in `internal/config`): `/etc/monopanel/`, the panel's directories in the nginx and Apache configuration, the configs of PHP (`/etc/php/`, `/etc/opt/remi/`), MySQL, mail, memcached and Sphinx, package sources, `/etc/nftables.d/`, `/etc/logrotate.d/`, `/etc/fail2ban/jail.d/`, Valkey snapshots `/var/lib/monopanel-valkey/`, `/var/lib/monopanel/`; crontab goes through `crontab -u`; client files `/var/www/<user>/...` through `runas`. The exceptions to the allow-list are `config.yaml` and `secret.key`: the generic operations do not write, remove, re-mode or re-own them or the directories above them (§7).

### 3.3 Helper (drop-privileges)

`monopaneld helper --uid N --gid N --groups ... -- <op> <args>`: the agent forks a process that irreversibly drops its privileges (`setgroups` → `setgid` → `setuid`) before running the operation and works with the files as the client. The file manager, uploads and archive extraction, and the CMS installers (`wp-cli` and others) go through it; git deploy and the terminal are planned. The helper itself has no privileged code paths.

### 3.4 Web UI

- SvelteKit 2 (Svelte 5, runes), `adapter-static`, TypeScript strict, Tailwind CSS 4. Built on this set without third-party UI libraries: the components, charts and tables are our own, and the only external runtime dependency is `qrcode` (the QR code for TOTP).
- Two languages without a library: the dictionary in `web/src/lib/i18n` (one fragment per page; `frag({ en, ru })` — the type requires the same set of keys in both, and `t()`/`tn()` read the current language from rune state, so switching redraws everything at once). The default language comes from `navigator.languages`: CIS languages → Russian, everything else → English; a choice made in Settings is stored in `localStorage.lang`, and `<html lang>` is set before the app loads. API, job and diagnostic messages are in English; the CLI and the TUI pick the language from the terminal locale by the same rule (`internal/i18n`; `MP_LANG=ru|en` sets it explicitly).
- Planned: a component library (shadcn-svelte / Bits UI), TanStack Query + Table, CodeMirror 6 for the config editor, xterm.js for the terminal, client generation from OpenAPI (`@hey-api/openapi-ts`).
- Built into `web/build` and embedded in the binary; an SPA, one HTML file, target JS size ≈ 200–300 KB gzipped.
- Two interface modes: an administrator sees the whole server and every page; an ordinary account sees the dashboard (the server's load and services, without the diagnostics) and, on the pages of sites, "Account" (its own cron, app services, Valkey, shared folders, API tokens and password), databases, mail, files, jobs, the audit log and settings, only its own things. The pages one by one are in [09-web-ui.md](09-web-ui.md).

### 3.5 CLI and TUI

- `monopanel` (alias `mp`) — Cobra commands, output as a table or `--json`. Exit codes: 0 — success, 1 — error, 2 — wrong arguments or missing input, 3 — the job failed, 4 — no permission. Every secret — passwords, tokens, the webhook secret, DNS provider credentials, backup and app service variables — has a `--…-stdin` twin: the value stays out of the shell history and out of `ps`.
- Without arguments in an interactive terminal it opens the TUI menu (Bubble Tea v2 + Lip Gloss). Its areas follow the web UI's navigation, and an ordinary account sees the same areas and commands as in the web UI. Every action is an `mp` command: its form is generated from the command's flags, the equivalent `mp` line is shown next to it (secrets masked), and the command runs as a child process of the same binary with its output streamed to the screen. Before anything that deletes or interrupts, the TUI asks the way the web UI does, and deleting an account, deleting a site with its files and restoring a snapshot over the files ask you to type the account, the domain or the snapshot. `?` or F1 — the screen's actions, the area's commands and the keys, `:` — any `mp` command, `*` — all commands of the area. Works over SSH: locally through the socket, remotely with `--server` and `--token`; the child commands get the address and the token through the environment, not on their command line.
- The CLI/TUI are thin API clients (locally `/run/monopanel/api.sock`, remotely `https://host:8443` with a token; the environment variables `MP_SERVER`, `MP_TOKEN`, `MP_INSECURE=1`). No operation is implemented "in the CLI only": without the API only `help`, `completion` and `version` work, and, as root on the server, `setup`, `config show|set --local` (recovery when the API does not start) and `update trust`.
- That every API operation and request field is reachable from the web UI, `mp` and the TUI, apart from deliberate exceptions, is checked by the `internal/parity` test (§11). Details and the list of exceptions are in [04-cli-tui-api.md](04-cli-tui-api.md).

### 3.6 State storage

- `/var/lib/monopanel/panel.db` — SQLite, WAL, `foreign_keys=ON`, `busy_timeout`. The `modernc.org/sqlite` driver (pure Go → a CGO-free static binary). Queries are hand-written in `internal/store`; migrations are embedded SQL files applied at start-up.
- Secrets (clients' database passwords, DNS provider tokens, ACME keys, SFTP/S3 backup passwords) are encrypted with AES-256-GCM using the key from `/etc/monopanel/secret.key` (0640 root:monopanel). A backup of the SQLite database is useless to an attacker without the key.
- Config history: `/var/lib/monopanel-agent/confhistory/` (the agent's own directory, root's alone; up to 0.8.19 `/var/lib/monopanel/confhistory/`) — previous versions of the generated files, saved by the agent when it writes; a diff in the UI and manual rollback are planned.
- A snapshot of the state is part of the server backup (`mp backup run`, scope `server`): a copy of `panel.db` (`VACUUM INTO`), `/etc/monopanel` together with the secrets key, `/var/lib/monopanel/{certs,acme}`, `/var/www` and dumps of all databases. Restoring the panel from it on a new server is done by hand; to move accounts between live servers, `mp migrate` is more convenient ([07](07-migration.md)).

## 4. Panel stack and rationale

### 4.1 Backend / core

| Candidate | Pros | Cons | Verdict |
|---|---|---|---|
| **Go 1.26+** | Static binary, fast start-up, goroutines for jobs, stdlib HTTP/TLS, mature libraries for systemd/ACME/nftables/TUI, cross-compilation for amd64/arm64, nfpm | GC pauses do not matter for a panel | **chosen** |
| Rust | Maximum performance and memory safety | Development takes 2–3 times longer; the panel's bottleneck is nginx/PHP, not the core | no |
| Python (FastAPI) | Development speed | An interpreter on the host: version conflicts across 9 OS releases, venv, slow CLI start-up | no |
| PHP / Node.js | Familiar to web developers | A runtime on the host, a poor fit for a system daemon with root operations | no |

Libraries: `go-chi/chi` (router), `danielgtaylor/huma/v2` (OpenAPI 3.1 from types, validation), `modernc.org/sqlite`, `coreos/go-systemd/v22` (D-Bus), `go-acme/lego/v4` (ACME), `x/crypto/argon2`, `pquerna/otp` (TOTP), `boombuler/barcode` (the QR code in the terminal for `mp totp setup`), `spf13/cobra`, `charm.land/bubbletea/v2` + `lipgloss/v2`, `miekg/dns`, `log/slog`, `goccy/go-yaml`, `goreleaser/nfpm` (deb/rpm, a separate tool). Planned: `go-webauthn/webauthn`, `google/nftables` (nftables is currently managed through a rules file and `nft`), `yookoala/gofast` (a FastCGI client for phpMyAdmin through the panel).

### 4.2 Frontend

| Candidate | Verdict |
|---|---|
| **Svelte 5 + SvelteKit 2** | **chosen**: the smallest bundle and the best runtime speed among the mainstream options, compiled reactivity (runes), a static adapter, a mature ecosystem of admin components (shadcn-svelte, Bits UI, TanStack) |
| React 19 + Vite | A bigger ecosystem, but a heavier bundle and runtime; an acceptable alternative if the team works in React |
| Vue 3 / Nuxt | Good, but with no advantage over Svelte for this task |

Tools: Node 24 LTS, pnpm, Vite (latest stable), TypeScript strict, ESLint + Prettier, Vitest (unit), Playwright (e2e).

### 4.3 Transport and protocols

- REST + JSON, OpenAPI 3.1, errors in RFC 9457 format (`application/problem+json`).
- SSE for streaming (job events, logs, metrics): passes through any proxy, with none of the WebSocket timeout problems.
- WebSocket only for the interactive terminal (planned).
- The internal api↔agent RPC is the same HTTP/JSON over a unix socket (reusing the huma types), with a mandatory peer-cred check.

## 5. Data model (SQLite)

The main entities (simplified):

```
users            id, login, role(admin|user), unix_uid, unix_gid, home, shell(bool), status,
                 quota_mb, email, created_at
sites            id, user_id, domain, aliases(json), mode(fpm|apache), php_version, docroot, ip_id,
                 http2, http3, redirect_https, redirect_www(none|to_www|to_root),
                 fpm_pm(ondemand|dynamic|static), fpm_max_children, php_ini(json), disable_functions(json),
                 allow_exec(bool), static_by_nginx(bool), cert_id, status(active|suspended|disabled)
certificates     id, user_id, kind(acme|custom|selfsigned), names(json), acme_account_id, dns_provider_id,
                 not_before, not_after, key_type(ec256|rsa2048), auto_renew, last_error
acme_accounts    id, directory_url, email, key(enc), registered_at
dns_providers    id, user_id, type(cloudflare|route53|hetzner|rfc2136|...), credentials(enc)
ip_addresses     id, ip, iface, is_default, note
php_versions     version, source(sury|remi|monopanel), fpm_service, bin_path, fpm_conf_dir,
                 extensions(json), eol_status, status
db_instances     id, engine(mysql|percona), version, socket, native_password(bool), status
databases        id, user_id, name, charset, collation, size_bytes
db_users         id, user_id, name, host, password(enc), auth_plugin, grants(json)
cron_jobs        id, user_id, schedule, command, enabled, php_version, mail_to
backup_targets   id, type(local|sftp|s3), config(enc), retention(json), schedule
backups          id, target_id, scope(server|panel|user|site|db), snapshot_id, size, status, started_at
jobs             id, type, payload(json), status(queued|running|done|failed|cancelled), progress,
                 log_path, requested_by, lock_key, idempotency_key, created_at, finished_at
audit_log        id, ts, actor, action, target, ip, result, details(json)
sessions         id, user_id, expires_at, ip, ua
api_tokens       id, user_id, name, hash, scopes(json), last_used, expires_at
settings         key, value(json)
metrics_*        rollups 10s/1m/1h: cpu, mem, disk, net, load; per-site requests/traffic
```

Invariants:
- `domain` is unique (punycode, lower case, no trailing dot); an alias cannot match another site's domain.
- One site = one FPM pool = one unix socket `/run/monopanel/php/<domain>.sock`.
- `php_version` refers to an installed version; removing a version is blocked while there are sites on it.
- Deleting a user cascades to its sites/databases/cron/certificates only with an explicit `purge` (otherwise it is a `suspend`).
- Jobs with the same `lock_key` run strictly one after another.

## 6. Configuration apply pipeline

```
API: PATCH /sites/{domain} ─▶ validation ─▶ desired state in SQLite ─▶ job "site.apply" (lock_key=site:<id>)
                                                                        │
worker: 1) build the site model from the database (site + user + php_version + cert + ip + settings)
        2) render: nginx sites/<domain>.conf, the FPM pool, (default-server IP);
           in apache mode also httpd.conf and sites/<domain>.conf of the account's Apache
        3) apache mode, before nginx: agent.ApplyConfigSet(httpd.conf + the virtual host,
              validate = ["apache2 -t -f <the account's httpd.conf>"], reload = [monopanel-apache@<login>])
           — or a start and an enable when it was not running, and a wait until it answers on its address
        4) agent.ApplyConfigSet(
              files    = [...],
              validate = ["nginx -t", "php-fpm -t -y <fpm.conf>"],
              reload   = [nginx, php-fpm@X.Y (or the old and the new master when the version changes)])
           agent: back up the old files → write temporary files → rename → validators;
                  error  → restore the backup → return the validator's stderr to the job log;
                  ok     → reload via D-Bus → confhistory
        5) a site that left apache mode or is suspended leaves the account's Apache now, after nginx
        6) post-steps: restorecon (SELinux), ACL, the data/bin/php symlink when the version changes
        7) job=done, SSE event ⇒ the UI refreshes the site page
```

In apache mode a site comes to its account's Apache first: its `httpd.conf` and `sites/<domain>.conf`, `apache2 -t -f`, a reload (graceful) or a start and `enable`, waiting until it answers on its address; then nginx and php-fpm — until nginx reloads, the site is served where it was. A site leaving Apache (a mode change, suspension, removal) leaves nginx first, then the Apache. An Apache left without sites is stopped and disabled, and its `httpd.conf` and virtual hosts are removed; a site's custom directives (`sites/<domain>.d/`) stay as long as the site exists, and the account's directory goes with its last site. An account's Apache takes one site's change at a time: two of its sites' jobs running side by side do not get in each other's way.

Principles:
- The panel owns only the files in its own directories and the main `nginx.conf` (from a template, with `include conf.d/*.conf` for manual edits). Configs it does not own are never edited line by line.
- User edits go only into the include directories `<domain>.d/*.conf`; the panel does not overwrite them but validates them together with the whole set.
- A template is overridden by a file with the same path under `/etc/monopanel/templates/` (for example `nginx/site.conf.tmpl`); `mp config templates` (and the "Configuration templates" card in the web UI's Settings) lists the built-in ones and shows which of them are overridden. Commands that copy a built-in template and show how it diverges from a new panel version are planned.
- A site is regenerated from the database as a whole with `mp site apply <domain>` (`mp site fix` also fixes the owner, permissions and labels of its files). A full reconcile of the whole server with one command is planned.
- Changing the PHP version: the new pool is created before the old one is removed, both masters are reloaded, then nginx/Apache is switched over — no downtime.

## 7. Security

- Panel: TLS 1.2/1.3, HSTS, CSP without inline scripts, an Origin check on session mutations (the cookie is `SameSite=Strict` anyway), sign-in rate limiting + a fail2ban jail on the panel log, argon2id, TOTP (WebAuthn is planned), an audit of all mutations, API tokens with a scope and an expiry, optional binding of the panel to a separate IP/VPN.
- Sessions and tokens: changing the password ends the account's other browser sessions (the one it was changed from stays), while a reset by an administrator and a suspension of the account end all of them; API tokens are not revoked by this — they are revoked separately — but those of a suspended account do not work until it is unsuspended. Two-factor authentication cannot be set up anew over an enabled one: it is switched off first, which takes the password. A migration scope (`migrate:user:<login>`) opens an account's password hashes, keys, files and databases, so only administrators' tokens carry one: an ordinary account cannot put it into its own token, an administrator cannot put it into a token issued for an ordinary account, and the `/migrate` handlers require the administrator role whatever the scope. Such tokens of ordinary accounts left over from earlier versions are revoked by the panel at start-up and recorded in the audit log. Logins of the server's system accounts (uid below `UID_MIN`) and the names the panel signs its own jobs and audit entries with (`scheduler`, `system`) are not given to accounts.
- Processes: api — `User=monopanel`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, `CapabilityBoundingSet=` (empty), `ReadWritePaths=/var/lib/monopanel /run/monopanel /var/log/monopanel`; agent — root (it installs packages, makes users, writes `/etc`), so `ProtectSystem=` and a reduced capability set are impossible for it, but the rest is fenced off: `ProtectHome=read-only` (clients live in `/var/www`), `ProtectKernelModules`, `ProtectKernelTunables`, `ProtectKernelLogs`, `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`, `PrivateTmp`, `RestrictRealtime`, `LockPersonality`, `SystemCallArchitectures=native`; plus an allow-list of operations and paths, every exec is an `argv` array without a shell.
- Agent: no generic operation writes, removes, re-modes or re-owns `config.yaml` or `secret.key`, although `/etc/monopanel/` is on the allow-list (templates and the SELinux module live there): `config.yaml` decides which key vouches for releases installed as root (`update.public_key` replaces the built-in key or switches the check off), and `secret.key` decrypts every secret. The panel changes its five own settings in `config.yaml` through a separate `panel/config` operation; the rest of the file is root's alone (`mp config set --local`, `mp update trust`). Directories and files under `/var/www` that the agent makes or reads as root are reached one directory at a time without following a symlink: a client who swaps a directory for a symlink cannot turn such a write into one in `/etc` or `/root`, nor read `/etc/shadow` through it. Site logs, which nginx, Apache and php-fpm open as root, live outside the homes, in `/var/log/monopanel/sites/<login>/`, where only root creates and changes entries; the agent walks that tree the same way, refuses a directory anyone else may write to, and reads logs for the API only through directories without symlinks and files with a single name. The output file of the `tool` operation must be a plain file with a single name, and the agent never turns a system account (uid below `UID_MIN`) into a panel account. A shared folder's unit ([10](10-shared-folders.md)) is written and started (start, restart, reload, enable) only when every directory from the web root down to the mount point is root's, not writable by others (the ACL mask and the entries it holds back now counted) and no symlink: systemd checks `Where=`, and mount(8) then resolves the path once more, following symlinks — whoever may rename an entry on that way could have the folder bound over `/etc/systemd/system`. While the unit exists, no agent operation (`dirs/ensure`, `chown`, `user/ensure`, `user/remove`, `acl/set`, `acl/site`, `paths/remove`, `symlink/ensure`, `file/ensure`, tar extraction, in-place restore) hands a directory on the way to the mount point to an account, makes it writable, removes it or replaces it — 409. `/v1/units/remove` of a shared folder's unit holds the shared folders' lock, deletes the file and has systemd reload before it answers (a failed reload puts the file back): systemd would still mount a unit it has loaded. At its start the agent removes the units in the panel's form whose mount point lies below a directory someone else owns, may write to, or reaches through a symlink.
- The panel's port: `monopanel-api.socket` (PID 1) holds the HTTPS port, not the API process, so the port is never free while the API restarts (after a settings change, an update, a crash), is stopped or has not started yet at boot. Only root writes the address drop-in, from `web.listen` in `config.yaml`; the typed `/v1/units/*` does not know it. A restart that keeps the address restarts `monopanel-api.service` alone. One that moves it stops the socket first, and the API's restart brings it back on the new address: systemd refuses to start a socket while its service runs. For the same reason a restart of `monopanel-api.socket` itself (the Services page, `mp service restart`) is carried out as a stop of the socket and a restart of `monopanel-api`.
- The agent does not trust the API either: an API process taken over through a vulnerability must not get root. The `tool`, `stream` and `pkg` operations, the configuration checks and the account operations accept only the argument shapes the panel builds — an extra option, a foreign path or an account that is not the panel's (uid below `UID_MIN`, a home outside the web root, a privileged group) is refused with a 403 that says why. Archives are unpacked by the agent itself, not by `tar` running as root; restic snapshots are restored alongside into `/var/lib/monopanel-agent/restore/`, or in place only into the web root, the panel's configuration, certificates, the ACME account and dumps. The content of the configuration of daemons that run as root (nginx — `fastcgi_pass` only to the panel's site pools, no `PHP_VALUE`/`PHP_ADMIN_VALUE`, and no client-chosen first bytes towards a socket; the accounts' Apaches; php-fpm and `php.ini` — every branch's stock `www` pool only as the panel's idle pool, and `[www]` nowhere else; MySQL, Postfix, Dovecot, OpenDKIM, logrotate, fail2ban, sshd; apt and dnf sources — https only; the composer wrapper never runs as root, also when `/usr/bin/id` gives no answer) is checked before it is written: directives that load code, run programs or write files as root are refused. An `include` of nginx, php-fpm or dovecot is checked as glob(3) expands it: a wildcard in the file name only, no backslash or braces. The files of an account's Apache are written only for a hosting account and only in a shape where the parent listens on that account's address and the children run as the account: `User`, `Group`, `Listen`, the pid file, the locks and the modules (from a list, without CGI, SSI, Lua, status and info) are set by the account's `httpd.conf` alone, which includes only its own virtual hosts, they only their own directives, and those nothing; logs go to the account's log directory only. No generic write touches `/etc/systemd`: the panel's units are written by the typed `units/apply|remove` operations, which accept only its own units and keys. The agent starts, stops and enables any systemd unit by name except instances of template units (`modprobe@…`, `getty@…`), targets, slices, devices and scopes, shells (`debug-shell`, `emergency`, `rescue`, `console-getty`), the power units (`systemd-halt`, `-poweroff`, `-reboot`, `-kexec`, `-soft-reboot`, `-exit`, `-suspend`, `-hibernate`, `-hybrid-sleep`, `-suspend-then-hibernate`) and `systemd-sysext`/`systemd-confext`, which only report their state. Instances of the panel's own template `monopanel-apache@<login>` the agent does start and stop: the name picks nothing but `/etc/apache2/monopanel/accounts/<login>/httpd.conf`, without which the unit does not start, and that file the agent writes only in the shape described above. The agent runs no `a2enmod`, `a2enconf` or `a2ensite` (only `a2dis*`, to switch off what an older version enabled in the distribution's Apache), and may only take the distribution's Apache account (`www-data`) out of `monopanel-web`. The agent keeps its own things (the config history, restored snapshots, the update package and the previous binary) in `/var/lib/monopanel-agent` (root, 0700), and restic's cache and temporary directory in `/var/cache/monopanel-agent`, not in the API's data directory, where the API could leave a symlink.
- restic runs in a Landlock sandbox (`monopanel sandbox-exec`): a backup reads only the trees it backs up — the agent opened them without following a symlink and hands them over as descriptors —, a restore writes only into its target in `/var/lib/monopanel-agent/restore/`, and besides that restic reaches only its cache and temporary directory (`/var/cache/monopanel-agent/`), a local repository and the world-readable system files (the account databases, DNS, the time zone, CA certificates, libraries). A client who swaps a directory of its own for a symlink to `/` during a backup cannot make root store `/etc/shadow`, root's keys or the agent's state and, in a server backup, the panel's configuration, certificates, dumps or another account's files as its own: restic cannot read them, the snapshot is made and the run is marked `partial` with what it lacks. A backup also reads the web root in restic's own mount namespace, where the web root is remounted `nosymfollow`: no symlink below it is followed (Landlock grants a tree however it is reached, and a server backup also reads the panel's configuration, certificates, dumps and the other accounts' homes). The agent checks at start that it can do this; where mounting is refused (a container's profile, say), a backup runs under Landlock alone and the agent and `mp doctor` warn. A local repository must be a directory of restic's own: the sandbox lets restic read all of it, so a directory holding other files (such as `/var/backups` with its copies of `/etc/shadow`) is refused. An SFTP repository is reached through an ssh master connection the agent opens outside the sandbox with root's keys (ControlMaster); the ssh inside the sandbox reads no key and no configuration. Without Landlock (a kernel before 5.13, or `lsm=` without landlock) restic runs as before; the agent warns when it starts and `mp doctor` shows a "restic sandbox" warning. On Landlock ABI 1 (Ubuntu 22.04's 5.15 kernel) restores run without the sandbox: that version cannot hard-link a file into another directory.
- Clients: a separate unix user and group, home directory `0710`, nginx's access through ACLs for the `monopanel-web` group; FPM pools run as the client; `open_basedir`, separate `tmp`/`session`; `disable_functions` by default; symlink protection (`disable_symlinks if_not_owner` in nginx; the account's Apache runs as the account, so a symlink to someone else's file gets it nothing); the sites in apache mode are served by their account's own Apache (`monopanel-apache@<login>`) — its root parent listens on an address of that account alone on port 81, so no other process takes its sites' requests even while it restarts, and a site's `.htaccess` reaches neither another account's pool nor its files nor a server status page; optionally cgroup limits through isolated pools (see [03-web-stack.md](03-web-stack.md) §6). A database name `<login>_<name>` that the panel has already recorded for a database or a MySQL account of another account (logins may contain `_`) is refused. An ordinary account cannot put a site or a mail domain on a name of the panel itself or of its mail server (a session cookie does not keep to a port, so such a site would receive the administrators' sessions), nor on a name that is, or lies under, a site or a mail domain of another account (this server delivers its domains' mail to itself); an administrator places names anywhere.
- Generated files: a value the panel writes into a file line by line cannot start a line of its own. A cron job is one line without control characters, with a schedule both crons understand; an app service's unit values have no line breaks or other control characters and no trailing `\`, with which systemd continues a line; the names in postfix's maps, dovecot's passwd file and opendkim's tables have no blanks, control characters, `:`, `/` or `,`, and a domain follows the DNS rules. The mail maps are checked on every write, so even a value that got into the database past the API stops the apply instead of adding a line ([06](06-mail.md)). App services' variables live in an `EnvironmentFile` owned by root with mode 0600 (`/etc/monopanel/apps/`), not in the unit's `Environment=`, which every local user can read; an account's own env file is read as the account by the internal `mp app-exec` command rather than by systemd as root, so a symlink there opens no other file.
- Terminal output: names, comments, job messages and logs, log lines and audit entries come from customers, from strangers at the sign-in form and from migration sources, so `mp` and the TUI print them without control characters or terminal escape sequences (`internal/termsafe`): such a string cannot move the cursor, fake output, retitle the window or write to the administrator's clipboard.
- Migration: the source's bundle is input, not configuration. The target holds every value of it to the rule the API applies to the same value typed by a person; the dry run shows one that breaks a rule as a conflict, and the job leaves such an entry out and ends failed. The source's `CREATE USER` is never run as it is but built anew from the plugin and the hash; a dump is loaded as the database's own account, without the `mysql` client's commands or `LOAD DATA LOCAL`; of the tar stream only files, directories and links inside the tree remain, without setuid or device files; the sites' custom nginx directives are not applied but kept for review. Details are in [07](07-migration.md#5-what-already-works).
- SSH/SFTP: a client gets a shell only when the flag is set; SFTP-only through `Match Group monopanel-sftp` + `ChrootDirectory`.
- Panel updates come only as releases signed with ed25519 (§9); stack components only from their developers' official repositories.
- SELinux on EL stays enforcing (see [02-platform-matrix.md](02-platform-matrix.md) §6).
- Secrets in the database are encrypted and the key is kept outside it. Passwords and tokens stay off the command lines every local user can read (`ps`): the CMS installers get the passwords on stdin, `install.sh` hands the private repository's token to `curl` as a config on stdin, and the web UI's console and the TUI give the child `mp` the panel's address and the token through the environment (`MP_SERVER`, `MP_TOKEN`) and typed passwords on stdin through the `--…-stdin` flags. The API returns job parameters without their secrets — passwords, keys and tokens in them are replaced by `[redacted]` (passwords and keys are stored encrypted in the database too) — and a job goes to webhooks without them as well; the audit log and the console's echo hide the values of secret flags.

## 8. Observability

- Panel logs: `slog` → journald (`monopanel-api`, `monopanel-agent`), `mp logs <unit>`. The action log (who, what, from where: sign-ins successful or not, tokens, changes, socket refusals) is the `audit_log` table in the panel database, shown by `mp audit`, `GET /system/audit` and the "Audit log" page (an administrator sees everything, an account only its own entries); the same feed is written as JSON lines to `/var/log/monopanel/audit.log` (`log.audit_file`, rotated by logrotate), and sign-ins and refusals also go to journald. Each job's log is kept with the job (`mp job show <id>`, the "Jobs" page).
- Metrics: a sampler reads `/proc` every 10 s (CPU, load, memory, disk, network) and stores one point per minute for 30 days — `mp metrics` and the charts on the dashboard. Per-site parsing of nginx access logs (requests, traffic, response time) and a `/metrics` endpoint for Prometheus are planned.
- Site logs: `/var/log/monopanel/sites/<user>/<domain>.{access,error}.log`, `<domain>.php.error.log`, `<domain>.php.slow.log`, and for sites in apache mode `<domain>.apache.{access,error}.log` and `apache.error.log` of the account's Apache itself; `mp site logs`, the site's "Logs" tab. The directory is `root:<user>` 0751, outside the home: nginx, Apache and php-fpm open the logs as root, and in the client's own directory a log could be swapped for a symlink. The client reads them through its group. They are rotated by `/etc/logrotate.d/monopanel-sites`: two blocks per account, as root, weekly or sooner once a log passes 100 MB, eight copies, compressed from the second one on; the servers' logs are `root:<login>` 0640 and the PHP error log the account's with mode 0660 (the panel creates them before nginx, Apache and php-fpm open them), and nginx and the account's Apache reopen their logs on USR1 through their pid files (`/run/nginx.pid`, `/run/monopanel-apache/<login>/httpd.pid`). The panel rewrites the file when an account is added or removed and at start-up, and checks it with `logrotate --debug` before writing it. On EL, the panel's policy module permits the traces in the php-fpm slow log ([02](02-platform-matrix.md#6-selinux-el9--el10)).
- Notifications: webhooks signed with HMAC-SHA256 when jobs finish — `job.done`, `job.failed` and `<job type>.done|failed` (`site.apply`, `cert.issue`, `backup.run`…). Mail and Telegram are planned.
- Diagnostics: `mp doctor` and a block on the dashboard — services (and every account's Apache with `apache2 -t` of its configuration), config syntax, disk, memory, certificates, DNS for the panel's hostname, failed jobs, SELinux denials for the web server, drift of generated files, restic's sandbox ("restic sandbox"), a shared `apache2.service` of older versions still running and an Apache port that accounts may bind ("apache port"); some findings have a "Fix" button. `mp doctor` itself adds an `[WARN] api socket` line when `monopanel-api.socket` is enabled but the API listens on the port itself.

## 9. Packaging, installation, updates

- Build: `make packages VERSION=…` → amd64/arm64 binaries → `nfpm` → `monopanel_<ver>_<arch>.deb`, `monopanel-<ver>.<arch>.rpm`, bare binaries and `SHA256SUMS`. The package contains the binary, the unit files `monopanel-api.service`, `monopanel-api.socket`, `monopanel-agent.service` and the template `monopanel-apache@.service`, sysusers/tmpfiles and `/etc/monopanel/config.yaml`; the config templates are embedded in the binary. postinstall enables the socket with the services and brings its address to `web.listen` (`monopanel api-socket-sync`). On an upgrade from 0.8.19 or older, whose API holds the port itself (or when the socket's address moved), it restarts the agent, the socket and the API in one `systemctl restart`: systemd starts the socket between the old API's exit and the new one's start.
- Release: a `v<ver>` tag → CI builds the packages, signs `SHA256SUMS` with an ed25519 key (`SHA256SUMS.sig`) and publishes the release on GitHub; the annotated tag's message becomes the release notes. There is no apt/yum repository of our own — it is planned; packages are taken from the releases.
- Installation: `curl -fsSL https://monopanel.app/install.sh | sh` (the address leads to `packaging/install.sh`): the script detects the OS and architecture, takes the latest release tag from the `/releases/latest` redirect (without the GitHub API and its rate limit), downloads the package, checks the checksum and installs it. Then `mp setup`: the service user, directories, the database, a self-signed certificate, the administrator; the panel's address and the password are printed at the end, and parameters are set with flags (`--hostname`, `--listen`, `--admin-password`…). The stack — nginx, PHP, the database server and the rest — is installed afterwards from the official repositories: `mp stack install`, `mp php install` or from the web UI. A TUI install wizard is planned.
- Panel update: `mp update apply` or a button in "Settings" (the `panel.update` job): finding the release, verifying the ed25519 signature of the checksum list (the key is built into the panel; `update.public_key` in `config.yaml` replaces it for a fork or switches the check off with `none`), downloading the package, installation by the agent in the transient unit `monopanel-update.service`, which survives the restart of the api and the agent and restores the previous binary if the new version does not answer; database migrations are applied at start-up. The agent copies the package out of the API's downloads into its own root-only directory, `/var/lib/monopanel-agent/updates`, without following a symlink, and checks the signature and the checksum of that copy: only a copy whose SHA-256 is the one the signed checksum list gives the release's own package file takes the name `mp update-run` installs, and a request that does not match changes nothing already staged. The agent never installs a version older than the one installed, nor the release a later build was made from (`git describe`, e.g. `0.8.19-5-gabc1234`): a downgrade is done by hand as root. The drop-in `/etc/systemd/system/monopanel-api.service.d/socket.conf` asks the binary before every start of the API: `monopanel api-socket-sync --probe`. Versions 0.8.19 and older do not know the command and do not take the socket. So after a self-update that rolled back (which restores only the binary), or after a package downgrade, the drop-in stops `monopanel-api.socket` and the old API listens on the port itself instead of failing on EADDRINUSE at every start; nothing needs doing, but while the old version runs the port is free again whenever the API restarts. The next update to 0.8.20 or later brings the socket back by itself. After a package downgrade the drop-in can go: `rm /etc/systemd/system/monopanel-api.service.d/socket.conf && systemctl daemon-reload`. Checks run on a schedule (daily), and `--auto-apply` installs what they find on its own. Stack components are updated when the administrator decides to; major database server upgrades are not automated.

## 10. Repository layout

```
monopanel/
├── cmd/monopanel/            # single entry point: api | agent | helper | fsop | CLI/TUI
├── internal/
│   ├── api/                  # HTTP API (huma + chi), auth, SSE, Web UI, the panel's TLS, job handlers
│   ├── apitypes/             # API request and response types, shared by the server, CLI and TUI
│   ├── agent/                # the privileged agent and its operations; agenttest is a fake for tests
│   ├── jobs/                 # job queue, workers, per-entity locks, event broker
│   ├── store/                # SQLite: queries, models, embedded migrations
│   ├── render/               # template rendering + golden tests
│   ├── osprofile/            # Debian/RHEL differences: packages, paths, services, SELinux
│   ├── acme/                 # issuing and renewing certificates (lego)
│   ├── auth/, secrets/       # passwords, tokens, TOTP; encryption of secrets in the database
│   ├── updater/              # finding a release, verifying its signature, installing with rollback
│   ├── setup/                # mp setup
│   ├── cli/, tui/, client/   # the mp commands, the TUI menu, the Go API client
│   ├── parity/               # the parity test: web UI, mp and TUI against the API (§11)
│   ├── sandbox/              # the Landlock sandbox for restic (monopanel sandbox-exec)
│   └── config/, systemd/, sysinfo/, peercred/, buildinfo/
├── templates/                # nginx/, apache/, php/, php-fpm/, mysql/, mail/, systemd/, nftables/, fail2ban/, …
├── web/                      # SvelteKit application (build → embed)
├── site/                     # the monopanel.app site with documentation from the README and docs/
├── packaging/                # nfpm.yaml, units, sysusers/tmpfiles, install.sh
├── scripts/                  # release (key and signature), testbed (test VMs), screenshots (images for the docs)
├── e2e/                      # a scenario against a live panel
└── docs/
```

Our own PHP builds (`build/php/`, stage 2) are planned.

## 11. Testing

- Unit (`make test`, a couple of seconds): the panel's logic is exercised through a fake agent (`internal/agent/agenttest`) without root or systemd — a test sees the generated files and every agent call; templates are checked against golden files (`go test ./internal/render -update` updates them); input validators, the job queue, OS Profile. Separate tests hold the boundaries of §7: a migration scope in an ordinary account's hands, sessions after a password change, secrets in job parameters, the mail maps against planted lines, CMS installer passwords off the command line, the agent's writes and reads under `/var/www` past symlinks, its generic operations past `config.yaml` and `secret.key`, the mount points of shared folders (`sharemount_test.go`), the socket of the panel's port (`apisocket_test.go`, `listener_test.go`), the accounts' Apaches (`apache_test.go`, `confcheck`), the idle php-fpm pools (`php_pools_test.go`), restic's sandbox (`restic_sandbox_test.go`, `internal/sandbox`), migration bundles with planted values from MonoPanel, FASTPANEL and BitrixVM (`migrate_security_test.go`). `make check` adds `gofmt`, `go vet` and `golangci-lint`.
- Client parity (`go test ./internal/parity/`, part of `make test` and CI): the test starts the API behind a fake agent and, through the manifest (`internal/parity/manifest_*.go` — every operation with its `mp` commands, its places in the web UI and its fields), compares four sides: the API's operations and request fields from its OpenAPI description, the `mp` command tree, the TUI's menus and forms for an administrator and for an ordinary account, and the web UI's sources (`web/src`: which paths and fields the pages send, whether administrators' actions are hidden from ordinary accounts, whether anything calls a path the API does not have). It fails when an operation or a field cannot be reached from some client, apart from the deliberate exceptions (`exempt.go` and the lists in `parity_test.go`), and says what to fix where.
- The web UI's console is checked against the real `mp` command tree (`internal/api/console_cobra_test.go`): it finds the command the way cobra does, with any flags in front of it; the commands and flags it refuses or treats as a command's own flags exist in `mp`; and every secret flag that has a `--…-stdin` twin in `mp` is moved to stdin.
- Template regression: `scripts/check-templates.sh` feeds the generated configs to a real `nginx -t` and `apache2 -t` (the whole configuration of an account's Apache with its virtual host; in CI, on every push).
- E2E (`make e2e HOST=<ssh-alias>`): on a live panel — user → site with a preset → database → nginx and PHP answer → removal; `mp doctor` with no errors.
- The OS matrix runs on the Proxmox testbed ([08-testbed.md](08-testbed.md)) rather than in CI: eleven VMs (Debian 12/13, Ubuntu 22.04/24.04/26.04, AlmaLinux 9/10, Rocky Linux 9/10, Oracle Linux 9/10), each rolled back to a clean snapshot. Before a release — `make testbed-full`: the matrix, migrations between panels, CMS installs on every machine, moves from BitrixVM and FASTPANEL, doctor everywhere.
- Planned: HTTPS through Pebble (a test ACME server), backup → restore → suspend → purge in the e2e scenario, web UI e2e with Playwright.
