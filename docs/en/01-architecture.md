# 01. MonoPanel architecture

As of September 2026 (MonoPanel 0.8.10). Component versions are given as of that date; before a release, a run of the OS matrix on the [testbed](08-testbed.md) checks that they are still current.

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

**Out of scope for v1** (see the roadmap): a DNS server, multi-server, containerised applications, a WAF, a reseller role. Mail (postfix + dovecot + opendkim + Roundcube) arrived after v1 and is described separately — [06-mail.md](06-mail.md).

## 2. Architecture decisions (ADR, in brief)

| # | Decision | Why |
|---|---|---|
| A1 | The panel core is one static binary written in **Go** | No runtime dependencies on the host (2 OS families × 9 releases), one artefact for daemon + CLI + TUI, simple packaging into .deb/.rpm, fast CLI start-up, built-in HTTP server and TLS |
| A2 | **Privilege separation**: `monopaneld api` (user `monopanel`) ↔ `monopaneld agent` (root) over a unix socket | The code that parses untrusted input and serves the web UI does not run as root. The agent accepts only typed commands, with an allow-list of paths and operations |
| A3 | **One API for all clients**: the web UI, CLI, TUI and external integrations call the same REST API | No divergence in logic; anything you can do in the web UI you can do in the CLI, and vice versa |
| A4 | **Declared state → render → validate → apply atomically → reload, with rollback** | The panel is the source of truth; manual edits live in include directories; a broken config never reaches a reload |
| A5 | The panel serves its **own** HTTPS port (8443) with Go's `net/http` | The panel stays reachable even if the system nginx is broken or not installed yet |
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
                        nginx      apache     php-fpm×N    mysqld    systemd / apt / dnf / nft
```

### 3.1 `monopaneld api` — the unprivileged process

- The panel's HTTPS server (port 8443, optionally bound to an IP of your choice; certificate: self-signed at installation → ACME for the panel's hostname).
- Serves the web UI (a static SvelteKit build embedded via `embed.FS`).
- REST API `/api/v1`: OpenAPI 3.1 is generated from Go types (`huma`), SSE for job progress. A WebSocket web terminal is planned; for now the web UI has the "Console" page: `mp` commands with output streamed over HTTP.
- Authentication: sessions (cookie `Secure; HttpOnly; SameSite=Strict`), Bearer tokens, TOTP; WebAuthn (passkeys) is planned. RBAC: `admin`, `user`. A token's scope restricts only migration tokens (`migrate:user:<login>`); any other scope is just a label.
- Unix socket `/run/monopanel/api.sock` for the CLI: uid 0 → admin without a password; the uid of a panel user → that user's rights (a client on SSH manages their own sites with `mp`).
- Job runner: a job queue in SQLite, N workers; each job is an idempotent sequence of steps with a log and progress; one job per entity at a time (a lock on `site_id`/`user_id`).
- Configuration rendering (`text/template`) from built-in templates, which can be overridden in `/etc/monopanel/templates/`.
- A scheduler for internal tasks: certificate renewal, scheduled backups, metrics collection, update checks.
- Stores ACME keys and client secrets encrypted (see 3.6).

### 3.2 `monopaneld agent` — the privileged process

- The same binary in `agent` mode, unit `monopanel-agent.service`, root.
- Socket `/run/monopanel/agent.sock` (0660 root:monopanel). A `SO_PEERCRED` check: only the `monopanel` uid and root are accepted.
- Operations are a closed, typed set (there is no "run this string in a shell"):
  - `config/apply` — a transactional write of a set of files: the previous versions go to `confhistory` → write to a temporary file + `rename` → validators (`nginx -t`, `apachectl -t`, `php-fpm -t`) → on error, restore and return stderr; on success, reload the services.
  - `user/ensure|remove|password|shadow`, `group/ensure`, `dirs/ensure`, `file/ensure|read`, `symlink/ensure`, `acl/set`, `chown`, `paths/remove`, `dir/list`, `stat`.
  - `service` — start/stop/reload/restart/enable/disable/status and daemon-reload through systemd D-Bus (`go-systemd`) rather than `systemctl`.
  - `pkg` — install/remove/query/available through apt/dnf according to the OS Profile, with a global lock and output logged to the job.
  - `tool` — a closed list of programs with argument checks: `mysql`/`mysqldump` (root via `auth_socket`, the root password is not stored), `restic`, `nft`, `crontab`, `semanage`/`restorecon`/`setsebool`, `firewall-cmd`, `postfix`/`doveadm` and others.
  - `runas` — the privilege-dropping helper for a client's file operations; `stream/in|out` — tar streams for migration; `panel/install` — installing the panel package during an update.
- Allow-list of write paths (`DefaultAllowedWritePrefixes` in `internal/config`): `/etc/monopanel/`, the panel's directories in the nginx and Apache configuration, the configs of PHP (`/etc/php/`, `/etc/opt/remi/`), MySQL, mail, memcached and Sphinx, package sources, `/etc/nftables.d/`, `/etc/logrotate.d/`, `/etc/fail2ban/jail.d/`, Valkey snapshots `/var/lib/monopanel-valkey/`, `/var/lib/monopanel/`; crontab goes through `crontab -u`; client files `/var/www/<user>/...` only through `runas`.

### 3.3 Helper (drop-privileges)

`monopaneld helper --uid N --gid N --groups ... -- <op> <args>`: the agent forks a process that irreversibly drops its privileges (`setgroups` → `setgid` → `setuid`) before running the operation and works with the files as the client. The file manager, uploads and archive extraction, and the CMS installers (`wp-cli` and others) go through it; git deploy and the terminal are planned. The helper itself has no privileged code paths.

### 3.4 Web UI

- SvelteKit 2 (Svelte 5, runes), `adapter-static`, TypeScript strict, Tailwind CSS 4. Built on this set without third-party UI libraries: the components, charts and tables are our own, and the only external runtime dependency is `qrcode` (the QR code for TOTP).
- Two languages without a library: the dictionary in `web/src/lib/i18n` (one fragment per page; `frag({ en, ru })` — the type requires the same set of keys in both, and `t()`/`tn()` read the current language from rune state, so switching redraws everything at once). The default language comes from `navigator.languages`: CIS languages → Russian, everything else → English; a choice made in Settings is stored in `localStorage.lang`, and `<html lang>` is set before the app loads. API, job and diagnostic messages are in English; the CLI and the TUI pick the language from the terminal locale by the same rule (`internal/i18n`; `MP_LANG=ru|en` sets it explicitly).
- Planned: a component library (shadcn-svelte / Bits UI), TanStack Query + Table, CodeMirror 6 for the config editor, xterm.js for the terminal, client generation from OpenAPI (`@hey-api/openapi-ts`).
- Built into `web/build` and embedded in the binary; an SPA, one HTML file, target JS size ≈ 200–300 KB gzipped.
- Two interface modes: administrator (the whole server) and client (their own sites/databases/files/cron).

### 3.5 CLI and TUI

- `monopanel` (alias `mp`) — Cobra commands, output as a table or `--json`.
- Without arguments in an interactive terminal it opens the TUI menu (Bubble Tea v2 + Lip Gloss): sites, users, PHP, databases, SSL, firewall, services and jobs as tables to browse; for backups and settings the menu suggests `mp` commands. Works over SSH. Forms, a dashboard and job progress in the TUI are planned.
- The CLI/TUI are thin API clients (locally `/run/monopanel/api.sock`, remotely `https://host:8443` with a token). No operation is implemented "in the CLI only".
- Details are in [04-cli-tui-api.md](04-cli-tui-api.md).

### 3.6 State storage

- `/var/lib/monopanel/panel.db` — SQLite, WAL, `foreign_keys=ON`, `busy_timeout`. The `modernc.org/sqlite` driver (pure Go → a CGO-free static binary). Queries are hand-written in `internal/store`; migrations are embedded SQL files applied at start-up.
- Secrets (clients' database passwords, DNS provider tokens, ACME keys, SFTP/S3 backup passwords) are encrypted with AES-256-GCM using the key from `/etc/monopanel/secret.key` (0640 root:monopanel). A backup of the SQLite database is useless to an attacker without the key.
- Config history: `/var/lib/monopanel/confhistory/` — previous versions of the generated files, saved by the agent when it writes; a diff in the UI and manual rollback are planned.
- A snapshot of the state is part of the server backup (`mp backup run`, scope `server`): a copy of `panel.db` (`VACUUM INTO`), `/etc/monopanel` together with the secrets key, `/var/lib/monopanel/{certs,acme}`, `/var/www` and dumps of all databases. Restoring the panel from it on a new server is done by hand; to move accounts between live servers, `mp migrate` is more convenient ([07](07-migration.md)).

## 4. Panel stack and rationale

### 4.1 Backend / core

| Candidate | Pros | Cons | Verdict |
|---|---|---|---|
| **Go 1.26+** | Static binary, fast start-up, goroutines for jobs, stdlib HTTP/TLS, mature libraries for systemd/ACME/nftables/TUI, cross-compilation for amd64/arm64, nfpm | GC pauses do not matter for a panel | **chosen** |
| Rust | Maximum performance and memory safety | Development takes 2–3 times longer; the panel's bottleneck is nginx/PHP, not the core | no |
| Python (FastAPI) | Development speed | An interpreter on the host: version conflicts across 9 OS releases, venv, slow CLI start-up | no |
| PHP / Node.js | Familiar to web developers | A runtime on the host, a poor fit for a system daemon with root operations | no |

Libraries: `go-chi/chi` (router), `danielgtaylor/huma/v2` (OpenAPI 3.1 from types, validation), `modernc.org/sqlite`, `coreos/go-systemd/v22` (D-Bus), `go-acme/lego/v4` (ACME), `x/crypto/argon2`, `pquerna/otp` (TOTP), `spf13/cobra`, `charmbracelet/bubbletea` v2 + `lipgloss`, `miekg/dns`, `robfig/cron`, `log/slog`, `goccy/go-yaml`, `goreleaser/nfpm` (deb/rpm, a separate tool). Planned: `go-webauthn/webauthn`, `google/nftables` (nftables is currently managed through a rules file and `nft`), `yookoala/gofast` (a FastCGI client for phpMyAdmin through the panel).

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
- WebSocket only for the interactive terminal.
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
        2) render: nginx sites/<domain>.conf, (apache sites/<domain>.conf), the FPM pool, (default-server IP)
        3) agent.ApplyConfigSet(
              files    = [...],
              validate = ["nginx -t", "apachectl -t"?, "php-fpm -t -y <fpm.conf>"],
              reload   = [nginx, apache?, php-fpm@X.Y (or the old and the new master when the version changes)])
           agent: back up the old files → write temporary files → rename → validators;
                  error  → restore the backup → return the validator's stderr to the job log;
                  ok     → reload via D-Bus → confhistory
        4) post-steps: restorecon (SELinux), ACL, the data/bin/php symlink when the version changes
        5) job=done, SSE event ⇒ the UI refreshes the site page
```

Principles:
- The panel owns only the files in its own directories and the main `nginx.conf` (from a template, with `include conf.d/*.conf` for manual edits). Configs it does not own are never edited line by line.
- User edits go only into the include directories `<domain>.d/*.conf`; the panel does not overwrite them but validates them together with the whole set.
- A template is overridden by a file with the same path under `/etc/monopanel/templates/` (for example `nginx/site.conf.tmpl`); `mp config templates` lists the built-in ones. Commands that copy a built-in template and show how it diverges from a new panel version are planned.
- A site is regenerated from the database as a whole with `mp site apply <domain>` (`mp site fix` also fixes the owner, permissions and labels of its files). A full reconcile of the whole server with one command is planned.
- Changing the PHP version: the new pool is created before the old one is removed, both masters are reloaded, then nginx/Apache is switched over — no downtime.

## 7. Security

- Panel: TLS 1.2/1.3, HSTS, CSP without inline scripts, a CSRF token on mutations, sign-in rate limiting + a fail2ban jail on the panel log, argon2id, TOTP/WebAuthn, an audit of all mutations, API tokens with a scope and an expiry, optional binding of the panel to a separate IP/VPN.
- Processes: api — `User=monopanel`, `ProtectSystem=strict`, `ProtectHome=yes`, `NoNewPrivileges=yes`, `CapabilityBoundingSet=` (empty), `ReadWritePaths=/var/lib/monopanel /run/monopanel`; agent — root, but `ProtectHome=read-only` except `/var/www` via `ReadWritePaths`, an allow-list of operations and paths, every exec is an `argv` array without a shell.
- Clients: a separate unix user and group, home directory `0710`, web server access through ACLs for the `monopanel-web` group; FPM pools run as the client; `open_basedir`, separate `tmp`/`session`; `disable_functions` by default; symlink protection (`disable_symlinks if_not_owner` in nginx, `SymLinksIfOwnerMatch` in Apache); optionally cgroup limits through isolated pools (see [03-web-stack.md](03-web-stack.md) §6).
- SSH/SFTP: a client gets a shell only when the flag is set; SFTP-only through `Match Group monopanel-sftp` + `ChrootDirectory`.
- Panel updates and our own packages come only from a signed (GPG) repository; stack components only from the vendors' official repositories.
- SELinux on EL stays enforcing (see [02-platform-matrix.md](02-platform-matrix.md) §6).
- Secrets in the database are encrypted and the key is kept outside it; logs contain no passwords/tokens (a `slog` redactor).

## 8. Observability

- Panel logs: `slog` → journald (`monopanel-api`, `monopanel-agent`), `mp logs <unit>`. The action log (who, what, from where) is the `audit_log` table in the panel database; each job's log is kept with the job (`mp job show <id>`, the "Jobs" page).
- Metrics: a sampler reads `/proc` every 10 s (CPU, load, memory, disk, network) and stores one point per minute for 30 days — `mp metrics` and the charts on the dashboard. Per-site parsing of nginx access logs (requests, traffic, response time) and a `/metrics` endpoint for Prometheus are planned.
- Site logs: `/var/www/<user>/data/logs/<domain>.{access,error}.log`, `<domain>.php.error.log`, `<domain>.php.slow.log`, and for sites in apache mode `<domain>.apache.{access,error}.log`; `mp site logs`, the site's "Logs" tab. They are rotated by `/etc/logrotate.d/monopanel-sites`: one block per account with `su <login>`, weekly or sooner once a log passes 100 MB, eight copies, compressed from the second one on; all logs belong to the account with mode 0660 (the panel creates them before nginx, Apache and php-fpm open them and hands them over to the account ready-made), and nginx and Apache reopen their logs on USR1 through their pid files. The panel rewrites the file when an account is added or removed and at start-up, and checks it with `logrotate --debug` before writing it. On EL, the panel's policy module permits the traces in the php-fpm slow log ([02](02-platform-matrix.md#6-selinux-el9--el10)).
- Notifications: webhooks signed with HMAC-SHA256 when jobs finish — `job.done`, `job.failed` and `<job type>.done|failed` (`site.apply`, `cert.issue`, `backup.run`…). Mail and Telegram are planned.
- Diagnostics: `mp doctor` and a block on the dashboard — services, config syntax, disk, memory, certificates, DNS for the panel's hostname, failed jobs, SELinux denials for the web server, drift of generated files; some findings have a "Fix" button.

## 9. Packaging, installation, updates

- Build: `make packages VERSION=…` → amd64/arm64 binaries → `nfpm` → `monopanel_<ver>_<arch>.deb`, `monopanel-<ver>.<arch>.rpm`, bare binaries and `SHA256SUMS`. The package contains the binary, the unit files `monopanel-api.service` and `monopanel-agent.service`, sysusers/tmpfiles and `/etc/monopanel/config.yaml`; the config templates are embedded in the binary.
- Release: a `v<ver>` tag → CI builds the packages, signs `SHA256SUMS` with an ed25519 key (`SHA256SUMS.sig`) and publishes the release on GitHub; the annotated tag's message becomes the release notes. There is no apt/yum repository of our own — it is planned; packages are taken from the releases.
- Installation: `curl -fsSL https://monopanel.app/install.sh | sh` (the address leads to `packaging/install.sh`): the script detects the OS and architecture, takes the latest release tag from the `/releases/latest` redirect (without the GitHub API and its rate limit), downloads the package, checks the checksum and installs it. Then `mp setup`: the service user, directories, the database, a self-signed certificate, the administrator; the panel's address and the password are printed at the end, and parameters are set with flags (`--hostname`, `--listen`, `--admin-password`…). The stack — nginx, PHP, the database server and the rest — is installed afterwards from the official repositories: `mp stack install`, `mp php install` or from the web UI. A TUI install wizard is planned.
- Panel update: `mp update apply` or a button in "Settings" (the `panel.update` job): finding the release, verifying the signature of the checksum list (if `update.public_key` is set in `config.yaml`), downloading the package, installation by the agent in the transient unit `monopanel-update.service`, which survives the restart of the api and the agent and restores the previous binary if the new version does not answer; database migrations are applied at start-up. Checks run on a schedule (daily), and `--auto-apply` installs what they find on its own. Stack components are updated when the administrator decides to; major database server upgrades are not automated.

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

- Unit (`make test`, a couple of seconds): the panel's logic is exercised through a fake agent (`internal/agent/agenttest`) without root or systemd — a test sees the generated files and every agent call; templates are checked against golden files (`go test ./internal/render -update` updates them); input validators, the job queue, OS Profile. `make check` adds `gofmt`, `go vet` and `golangci-lint`.
- Template regression: `scripts/check-templates.sh` feeds the generated configs to a real `nginx -t` and `apachectl -t` (in CI, on every push).
- E2E (`make e2e HOST=<ssh-alias>`): on a live panel — user → site with a preset → database → nginx and PHP answer → removal; `mp doctor` with no errors.
- The OS matrix runs on the Proxmox testbed ([08-testbed.md](08-testbed.md)) rather than in CI: eleven VMs (Debian 12/13, Ubuntu 22.04/24.04/26.04, AlmaLinux 9/10, Rocky Linux 9/10, Oracle Linux 9/10), each rolled back to a clean snapshot. Before a release — `make testbed-full`: the matrix, migrations between panels, CMS installs on every machine, moves from BitrixVM and FASTPANEL, doctor everywhere.
- Planned: HTTPS through Pebble (a test ACME server), backup → restore → suspend → purge in the e2e scenario, web UI e2e with Playwright.
