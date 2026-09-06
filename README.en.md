# MonoPanel

English · [Русский](README.md)

A web hosting control panel for Debian/Ubuntu and the RHEL family: sites, PHP,
databases, TLS, backups and firewall on a single server — from one static binary,
with no runtime to install, no agents in other languages and no external services.

[![ci](https://github.com/Logmen/MonoPanel/actions/workflows/ci.yml/badge.svg)](https://github.com/Logmen/MonoPanel/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/Logmen/MonoPanel)](https://github.com/Logmen/MonoPanel/releases)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![go](https://img.shields.io/badge/go-1.27-00ADD8)](go.mod)

The panel installs as a single package, serves its own HTTPS port and does not
depend on the system nginx: if a site's configuration breaks, the panel is still
reachable and can fix it. The web UI, the CLI, the SSH menu and any integration
all speak the same REST API — anything you can do with a mouse you can script.

> **The interface is in Russian.** The web UI, the CLI help and the documentation
> are written in Russian; only the code, this file and the security policy are in
> English. An English interface is on the roadmap, not in the product.

Status: **0.6.0**, in daily use on a production server hosting several sites.
Development moves quickly and breaking changes are possible before 1.0.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/Logmen/MonoPanel/main/packaging/install.sh | sh
mp setup
```

The script detects the OS and architecture, downloads the latest release package,
checks it against the published checksums and installs it. You can also do it by
hand — `.deb` and `.rpm` for amd64 and arm64 are attached to every
[release](https://github.com/Logmen/MonoPanel/releases).

`mp setup` creates the service user, the directories, the database, a self-signed
certificate and the administrator account, then prints the panel's address and the
password.

Requires root, systemd and one of: Debian 12/13, Ubuntu 22.04/24.04/26.04,
AlmaLinux/Rocky 9/10. Verified on Ubuntu 24.04; the others are supported by the
OS-profile abstraction but the full matrix has not been run on VMs yet.

## Quick start

```bash
mp setup --admin-password '…'   # or omit it: a password is generated and shown
mp status                       # panel, host, services, jobs
mp stack install nginx
mp config set web.hostname panel.example.com --restart
mp ssl issue panel.example.com  # Let's Encrypt; the panel serves it on :8443 at once

mp user add alex --generate --shell
mp php install 8.4              # alongside 7.4, 5.6 …
mp stack install percona        # Percona Server 8.4, root via auth_socket

mp site add example.com --user alex --www --preset wordpress
mp site add old.example.com --user alex --php 7.4 --mode apache
mp site add app.example.com --user alex --mode proxy --backend http://127.0.0.1:3000
mp db create shop --user alex --generate
mp firewall enable && mp stack install fail2ban
mp backup target add local1 --repo /var/backups/monopanel --schedule daily
mp                              # terminal menu
```

Remotely: `mp --server https://host:8443 --token <token> status`
(mint one with `mp token create`).

## What it does

Everything below goes through one REST API and is available in the web UI, the CLI
and the terminal menu.

| Area | Commands | How it works |
|---|---|---|
| PHP 5.6–8.5 | `mp php list\|install\|remove` | Sury / ondrej PPA / Remi; several branches side by side, one php-fpm master per branch, `99-monopanel.ini` |
| Sites | `mp site add\|set\|apply\|suspend\|rm\|logs` | modes `fpm`, `apache` (loopback 8080 via mod_proxy_fcgi) and `proxy` (nginx → backend); one pool per site, ACLs for the `monopanel-web` group, placeholder page, automatic certificate, suspend serves a 503 page; per-site IP allow-list (`--allow`), HSTS when HTTPS is forced, custom directives in `sites/<domain>.d/*.conf`; `mp site nginx <domain> --set file` validates with `nginx -t` and rolls back; `mp site php <domain>` shows the effective PHP settings; CMS presets `--preset wordpress\|joomla\|bitrix\|opencart` (`mp site presets`) add routing and hardening (pretty URLs, Joomla `/api/`, Bitrix `urlrewrite.php`, OpenCart `_route_`, denied service directories, no PHP execution in uploads) plus sane PHP defaults |
| App services | `mp app add\|set\|start\|stop\|restart\|logs\|rm` | a systemd unit `monopanel-app-<login>-<name>` running as the account (gunicorn, node, bots): command, working directory and env-file confined to the home directory, autostart, logs via journalctl |
| Apache 2.4 | `mp stack install apache` | Debian/Ubuntu: mpm_event + proxy_fcgi, `conf-available/monopanel.conf` |
| Databases | `mp stack install percona\|mysql`, `mp db create\|list\|passwd\|rm` | Percona Server / MySQL 8.4 LTS, root over `auth_socket`, tuning from available RAM, `mysql_native_password` only for PHP < 7.4, databases named `<login>_<name>` |
| TLS | `mp ssl issue\|list\|renew\|rm`, `mp dns-provider add`, `mp web tls` | lego: HTTP-01 through the nginx webroot, DNS-01 (Cloudflare, Hetzner, DigitalOcean, Gandi, deSEC, Namecheap, RFC2136) for wildcards, renewal 30 days ahead, hot-swap of the panel's own certificate; `mp ssl import --cert --key` for certificates issued elsewhere |
| Self-update | `mp update`, `mp update apply` | from this repository's releases: the panel finds a new version, downloads the package for its OS, verifies an ed25519 signature and installs it from a separate systemd unit, restoring the previous binary if the new one does not answer |
| API tokens | `mp token create\|list\|revoke` | a token belongs to an account; an administrator can mint one for another account (`--user`), and root on the local socket gets one for the single administrator with no flags |
| Cron | `mp cron add\|list\|enable\|disable\|rm` | the account's crontab is rendered whole from the database, with `~/data/bin` on PATH (the site's PHP version) |
| Real IP | `mp stack real-ip --cloudflare [--from CIDR]` | trusted proxies for nginx `real_ip` (Cloudflare ranges built in), so allow-lists and logs see the visitor rather than the proxy |
| Firewall | `mp firewall enable\|allow\|deny\|ban\|unban`, `mp stack install fail2ban` | nftables table `inet monopanel`, drop policy, SSH/80/443/panel always open, unit `monopanel-firewall`; fail2ban jails for sshd, nginx and the panel itself |
| Backups | `mp backup target add\|run\|list\|snapshots\|restore` | restic (local/SFTP/S3/B2/REST), MySQL dumps, a copy of panel.db, retention, a daily schedule, restore into `<data>/restore/<snapshot>` or in place |
| Files | `mp files ls\|put\|get\|mkdir\|rm\|mv\|chmod\|extract\|size` | `monopanel fsop` behind a helper that drops privileges irreversibly; paths are relative to the account's home |
| SFTP / SSH | `mp user add`, `mp user set --shell\|--sftp-only --password` | SFTP-only means a chroot into `/var/www/<login>` via `sshd_config.d/monopanel.conf`, with one password for the panel and SFTP; `mp user rm <login> [--purge]` removes sites, databases, cron, app services, certificates and the unix account together |
| Metrics and logs | `mp metrics`, `mp site logs`, `mp logs <unit>`, `mp doctor` | a sampler every 10 s stored as one point per minute for 30 days, site and journald log tails through the agent, 25 doctor checks |
| Security | `mp user totp-reset`, `mp webhook add` | TOTP 2FA (QR in the web UI), Bearer tokens, webhooks signed with HMAC-SHA256 on job events |

### Not there yet

Own PHP builds (Sury/Remi are used instead), tested Apache and database support on
EL, phpMyAdmin, disk quotas, per-site cgroup limits, mail, a DNS server, a WAF,
several servers from one panel, an apt/yum repository (packages ship as releases
and the panel installs them itself).

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

A version is cut by tagging; CI does the rest. The tag `v0.6.0` builds `.deb` and
`.rpm` for amd64 and arm64, signs the checksum list with an ed25519 key held as a
repository secret and publishes the release.

```bash
make keygen                  # once: a signing key pair (the private half becomes the secret)
make release VERSION=0.6.0   # tag and push; CI builds and publishes
make packages VERSION=0.6.0  # the same artefacts locally, without publishing
```

On a server:

```bash
mp update trust --key <public key>   # written to config.yaml, which only root may write
mp update settings --repo owner/name # --token-stdin for a private repository
mp update                            # what is installed and what is available
mp update apply                      # download, verify, install, restart
```

Checks run on a schedule (daily by default) and `--auto-apply` installs what they
find. The panel does not install the package itself: the agent starts a transient
`monopanel-update.service`, which survives the restart of both daemons and restores
the previous binary if the new version fails to answer. While a key is pinned, an
unsigned release will not install.

## Stack

| Layer | Choice |
|---|---|
| Core, CLI, menu | Go 1.27, one static binary (`CGO_ENABLED=0`) |
| HTTP / API | `chi` + `huma` v2 (OpenAPI 3.1 from types), SSE for job progress |
| State | SQLite (WAL) via `modernc.org/sqlite`, embedded SQL migrations |
| Web UI | Svelte 5 + SvelteKit 2 (static), TypeScript, Tailwind 4 — built into `web/build` and embedded in the binary |
| CLI / TUI | `cobra`, Bubble Tea v2 + Lip Gloss v2 |
| ACME | `lego` as a library |
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
needs a live host.

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
packaging/            nfpm.yaml, units, sysusers/tmpfiles, install.sh
scripts/release/      key generation and SHA256SUMS signing for a release
```

## Documentation

The design documents are in Russian, in [docs/](docs/): architecture, the platform
matrix, the web stack, the CLI/TUI/API reference and the roadmap. The API reference
is served by the panel itself at `/api/v1/docs` (OpenAPI 3.1).

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
