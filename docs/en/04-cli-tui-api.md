# 04. CLI, TUI and REST API

## 1. Principle

One API. The web UI, CLI, TUI and external integrations (billing, scripts) call the same `/api/v1` endpoints. The CLI and TUI are thin clients: the Go client `internal/client` with types from `internal/apitypes`, and `huma` generates the OpenAPI 3.1 description from the same types; the web UI calls the same endpoints through a small wrapper around `fetch`. A feature is implemented once — in the API — and is therefore available everywhere.

## 2. CLI — `monopanel` (alias `mp`)

Connection: locally — `/run/monopanel/api.sock` (authorisation by the process uid: root is the administrator, a client's uid is that client's account, so a client with SSH access manages their own hosting with the same `mp`); remotely — `--server https://host:8443 --token …` or the `MP_SERVER` / `MP_TOKEN` variables, and `--insecure` for a self-signed certificate.

Global flags: `--json` (machine-readable output), `--no-wait` (do not wait for the job), `--server`, `--token`, `--insecure`, `--config` (path to `config.yaml`), `--version`.

```
mp                                   # TUI menu (in an interactive terminal; otherwise help)
mp setup [--hostname …] [--listen …] [--admin-login …] [--admin-password …|--password-stdin]
mp status                            # panel, host, services, jobs
mp doctor                            # diagnostics
mp version
mp update                            # what is installed and what is available
mp update check | apply [--version v0.8.10]
mp update settings [--repo owner/name] [--channel …] [--check-hours 24] [--auto-apply] [--token-stdin|--clear-token]
mp update trust --key <public key> [--restart] | --clear

mp user add <login> [--password …|--password-stdin|--generate] [--email …] [--role user|admin] [--shell]
mp user set <login> [--password …|--generate] [--email …] [--shell|--sftp-only] [--status active|suspended]
mp user list | show <login> | totp-reset <login>
mp user rm <login> [--purge] [--yes]

mp site add <domain> --user <login> [--php 8.4] [--mode fpm|apache|proxy] [--backend http://127.0.0.1:3000]
            [--alias www.<domain>] [--www] [--preset wordpress|joomla|opencart|bitrix] [--ssl auto|none]
            [--ip …] [--docroot public] [--ini key=value] [--allow CIDR] [--pm …] [--max-children 8]
mp site set <domain> [--php …] [--mode …] [--alias …] [--rm-alias …] [--www|--no-www] [--redirect-www …]
            [--https-redirect=false] [--ini key=value] [--allow CIDR|--allow-all] [--preset …]
            [--sessions files|valkey] [--ssl auto|none] [--http2=false] [--http3] [--max-body 64m] [--allow-exec]
mp site list | show <domain> | presets
mp site apply | fix | suspend | unsuspend <domain>   # fix — owner, permissions, ACLs and SELinux labels of the site's files
mp site rm <domain> [--purge]
mp site logs <domain> [--type access|error|php|slow|apache-access|apache-error] [-n 100]
mp site nginx <domain> [--set file | --clear | --generated]   # custom directives in server {}, checked with nginx -t, with rollback
mp site php <domain>                 # effective PHP settings and where each one comes from
mp site tls <domain> [--dns <provider>] [--staging]           # a certificate for the domain and its aliases; the site switches to HTTPS
mp site move-ip <new-address> [--from <previous>]  # after the host's IP changes: all sites on the vanished addresses at once
mp selinux [enforcing|permissive] [--yes]
mp cms list
mp cms install <domain> wordpress|joomla|opencart|bitrix [--title …] [--admin-login …] [--admin-password …|--password-stdin]
               [--admin-email …] [--force]   # Bitrix: --edition start|standard|small_business|business, --solution clean|demo|<id>

mp php list [--available]
mp php install <version> | remove <version>
mp php ext list <version> | enable <version> <extension> | disable <version> <extension>
mp php ini [set key=value … | unset key …]   # PHP settings for all sites

mp stack list
mp stack install|remove nginx|apache|percona|mysql|fail2ban|memcached|valkey|jpegoptim|git|composer|sphinx
mp stack memcached [--memory-mb 256] [--max-conn 2048]    # without flags — show the settings
mp stack real-ip [--cloudflare|--no-cloudflare] [--from CIDR] [--clear-from]
mp valkey add cache|sessions --user <login> [--memory 128]   # the account's instance: create it or change its memory (restarts it)
mp valkey list [--user <login>] | restart | rm cache|sessions --user <login>   # restart empties the cache

mp db create <name> --user <login> [--password …] [--legacy-auth]   # database and user <login>_<name>; the password is generated if omitted
mp db list | passwd <name> [--password …] | rm <name>
mp db engine                         # the database server; tune — recalculate its settings for the available memory
mp db engine tune

mp ssl list | show <id|name> | rm <id|name>
mp ssl renew <id|name> | --all
mp ssl issue <name> [name…] [--dns <provider>] [--staging] [--rsa] [--email …]
mp ssl import [name] --cert f --key f [--chain f]
mp ssl panel                         # the panel's own certificate and the record behind it
mp ssl panel issue [--dns …] [--staging] | import --cert f --key f | self-signed
mp web tls                           # which certificate the panel is serving right now
mp dns-provider types | list
mp dns-provider add <name> --type cloudflare --cred CLOUDFLARE_DNS_API_TOKEN=… | rm <name>

mp backup target add <name> --type local|sftp|s3|b2|rest --repo … [--password …] [--env KEY=…] [--schedule daily]
                   [--keep-daily 7] [--keep-weekly 4] [--keep-monthly 3]
mp backup target list | rm <name>
mp backup run [--target …] [--scope server|user:<login>|site:<domain>|db:<name>]
mp backup list | snapshots [--target …]
mp backup restore <snapshot> [--target …] [--include path…] [--in-place]

mp cron list | add | enable <id> | disable <id> | rm <id> --user <login>
            [--schedule "*/5 * * * *" --command "…" --comment … --disabled]   # for an administrator, list without --user shows everyone's cron jobs
mp app list | show | add | set | start | stop | restart | logs | rm <name> --user <login>
            [--command … --workdir … --env KEY=… --env-file … --restart … --description …]
mp files ls|get|put|mkdir|rm|mv|chmod|extract|size --user <login> <path…>   # through the helper, as the client

mp firewall status | enable | disable | apply
mp firewall allow|deny --port 8443 [--proto tcp] [--source CIDR] [--comment …]
mp firewall rm <id> | ban <ip> | unban <ip>

mp mail install | status | settings | apply | domain … | box … | alias … | webmail …   # details in 06-mail.md
mp migrate grant user:<login> | plan | run                                             # details in 07-migration.md

mp service status [unit] | start | stop | reload | restart | enable | disable <unit>
mp logs <unit> [-n 100]              # the unit's journal, through the agent
mp metrics [--range 1h|6h|24h|7d|30d]
mp job list [--status …] [--limit …] | show <id> | wait <id>
mp config show | templates
mp config set <key> <value> [--restart]       # web.hostname, web.listen, log.level, log.format, jobs.workers
mp token create [--name …] [--scopes …] [--expires <days>] [--user <login>]
mp token list [--user …] | revoke <id>
mp webhook add --url … [--events job.failed,backup.run.*] [--secret …]
mp webhook list | test <id> | rm <id>
mp completion bash|zsh|fish          # shell completion script
```

Conventions:
- Commands backed by a job wait for it to finish by default and print its progress and log (`--no-wait` returns the job number at once).
- Exit codes: 0 — ok, 1 — error, 2 — invalid arguments, 3 — the job failed, 4 — permission denied.
- `--json` prints a single JSON object (or array), without progress or hints.
- `mp completion <shell>` prints the completion script; it is not part of the package yet.

## 3. TUI menu

- Start: `mp` without arguments in an interactive terminal (otherwise it prints help).
- Menu: sites, users, PHP, databases, SSL, firewall, services and jobs as tables to browse, through the same API as the CLI. "Backups" and "Settings" suggest the `mp backup` and `mp config` commands.
- Keys: ↑/↓ or j/k, Enter — open, r — refresh, q or Esc — back, Ctrl+C — quit.
- Planned: forms validated by the same rules as the API, a dashboard, job progress and a hint with the equivalent CLI command for every action.

## 4. REST API

- Base path `/api/v1`, specification `/api/v1/openapi.json`, interactive documentation `/api/v1/docs` (Stoplight Elements, embedded in the binary). The documentation, the specification and `/schemas/*` are served only to signed-in callers (a session or an API token; a migration token does not count): `curl -H "Authorization: Bearer $TOKEN" https://host:8443/api/v1/openapi.json`.
- Authentication: `Authorization: Bearer <token>`, a cookie session for the UI (+ CSRF), unix peer-cred for the CLI on the local socket. A token acts with the rights of its account (`admin` or `user`); the `migrate:user:<login>` scope restricts it to reading what a migration of one account needs, and any other scope is just a label.
- Mutations that need work on the server are asynchronous: `202 Accepted` with a `job_id` in the response; `GET /jobs/{id}`, `GET /jobs/{id}/events` (SSE: progress, log lines, completion). Fast operations (a token, a setting, a Valkey instance) are synchronous `200/201/204`.
- Errors are RFC 9457 `application/problem+json` with an `errors[]` field for per-field validation.
- Lists are returned whole; jobs support `?limit` and `?status`. Cursor pagination, filters, field selection and `Idempotency-Key` are planned.
- Webhooks: a JSON `POST` to the URL when jobs finish — events `job.done`, `job.failed` and `<job type>.done|failed` (`site.apply.done`, `cert.issue.failed`, `backup.run.*`, `*`), headers `X-MonoPanel-Event` and `X-MonoPanel-Signature: sha256=<HMAC of the body>`, three attempts. Events outside jobs (`service.down`, an expiring certificate) are planned.

Resources:

```
POST            /auth/login | logout              GET /auth/me
GET             /auth/totp                        POST /auth/totp/setup | enable | disable
GET/POST        /users                            GET/PATCH/DELETE /users/{login}     POST /users/{login}/totp/reset
GET/POST        /users/{login}/cron               PATCH/DELETE /users/{login}/cron/{id}         GET /cron
GET/POST        /users/{login}/apps               GET/PATCH/DELETE /users/{login}/apps/{name}   GET /apps
GET             /users/{login}/apps/{name}/logs   POST /users/{login}/apps/{name}/{start|stop|restart}
GET             /users/{login}/valkey             PUT/DELETE /users/{login}/valkey/{cache|sessions}   GET /valkey
POST            /users/{login}/valkey/{purpose}/restart
GET/POST        /sites                            GET/PATCH/DELETE /sites/{domain}   GET /sites/presets
POST            /sites/{domain}/apply | fix | suspend | unsuspend | cms | tls/issue    POST /sites/move-ip
GET             /sites/{domain}/logs/{type}       GET/PUT /sites/{domain}/nginx      GET /sites/{domain}/php
GET             /cms
GET/POST        /php/versions                     DELETE /php/versions/{version}     GET/PUT /php/settings
GET/POST        /php/versions/{version}/extensions
GET/POST        /databases                        DELETE /databases/{name}           POST /databases/{name}/password
GET             /db/engine                        POST /db/engine/tune
GET             /stack                            POST /stack/install                DELETE /stack/{component}
GET/PUT         /stack/memcached | /stack/nginx/real-ip
GET/POST        /certificates                     GET/DELETE /certificates/{id}      POST /certificates/import | /{id}/renew
GET/DELETE      /ssl/panel                        POST /ssl/panel/issue | import      GET /web/tls
GET/POST        /dns-providers                    DELETE /dns-providers/{name}        GET /dns-providers/types
GET/POST        /backups/targets                  DELETE /backups/targets/{ref}       GET /backups/targets/{ref}/snapshots
GET             /backups                          POST /backups/run | restore
GET             /firewall                         POST /firewall/enable | disable | apply | rules | ban | unban
DELETE          /firewall/rules/{id}
GET             /mail                             POST /mail/install | apply | webmail    PUT /mail/settings
GET/POST        /mail/domains | mailboxes | aliases    PATCH/DELETE /mail/domains/{name} | mailboxes/{address}
POST            /mail/domains/{name}/dkim         GET /mail/domains/{name}/dns        DELETE /mail/aliases/{address}
GET             /files?user=&path=                GET/PUT /files/content              POST /files/op   (through the helper)
POST            /migrate/grant | plan | run       GET /migrate/plan | state | files | dump   (on the source: migration token only)
GET             /services                         GET/POST /services/{unit}
GET             /jobs | /jobs/{id} | /jobs/{id}/events
GET/POST        /tokens                           DELETE /tokens/{id}
GET/POST        /webhooks                         DELETE /webhooks/{id}               POST /webhooks/{id}/test
GET             /system/status | doctor | metrics | logs/{unit}    GET/PUT /system/selinux
GET/PUT         /system/update                    POST /system/update/check | apply
POST            /system/console                   (mp commands with streamed output, administrator only)
GET             /health
```

## 5. Billing integration (WHMCS and others)

The WHMCS module has not been written yet; the API covers its actions as follows:

| WHMCS action | API calls |
|---|---|
| `CreateAccount` | `POST /users` → `POST /sites` (+ `POST /databases` according to the package) |
| `SuspendAccount` / `UnsuspendAccount` | `PATCH /users/{login}` with `status: suspended\|active` (sign-in to the panel and SFTP/SSH); the 503 placeholder on the sites — `POST /sites/{domain}/suspend` / `unsuspend` |
| `TerminateAccount` | `DELETE /users/{login}?purge=true` |
| `ChangePassword` | `PATCH /users/{login}` with `password` |
| `ChangePackage` | `PATCH /sites/{domain}` (PHP branch, `php_ini`, `fpm_max_children`…); the package's quotas and limits are planned |
| `ServiceSingleSignOn` | planned (a one-time sign-in link) |
| `UsageUpdate` | planned (disk and traffic accounting per account) |

The API requirements this covers: tokens with an account's rights, webhooks when jobs finish, stable identifiers (`login`, `domain`).
