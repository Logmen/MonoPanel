# 04. CLI, TUI and REST API

## 1. Principle

One API. The web UI, CLI, TUI and external integrations (billing, scripts) call the same `/api/v1` endpoints. The CLI and TUI are thin clients: the Go client `internal/client` with types from `internal/apitypes`, and `huma` generates the OpenAPI 3.1 description from the same types; the web UI calls the same endpoints through a small wrapper around `fetch`. A feature is implemented once — in the API — and is therefore available everywhere.

The three clients can do the same things. Every API operation has an `mp` command and a place in the web UI, the TUI menus offer every `mp` command, and every request field can be set with an `mp` flag or argument (and so with a field of a TUI form) and is part of the request the web UI page builds. Neither the web UI nor the TUI offers an ordinary account the actions and flags of an administrator. `go test ./internal/parity/` checks all of this, as part of `make test` and CI: the test compares the server's OpenAPI description, the `mp` command tree, the TUI menus for an administrator and for an ordinary account and the sources under `web/src` with the manifest in `internal/parity/manifest_*.go`, and names what is missing where.

The exceptions are deliberate, and there are few of them (`internal/parity/exempt.go` and the lists in the test itself):

- browser sign-in and sign-out (`POST /auth/login`, `/auth/logout`) — web UI only: `mp` and the TUI authenticate every request by the process uid on the socket or with an API token;
- the console (`POST /system/console`) — web UI only: it runs `mp` itself, and a terminal has `mp` and the TUI already;
- the API reference (`/api/v1/docs`, `openapi.json`, `/schemas`) is a set of pages, not operations; `GET /health` is the clients' own probe (the sign-in screen, `mp setup`), and `mp status` and the TUI header show the version;
- the server-to-server half of a migration (`GET /migrate/plan`, `state`, `files`, `dump`) — the receiving panel's job calls it with the migration token, never a person;
- `mp update trust` — root on the server only: the release signing key never passes through the panel process, and the web UI shows its state read-only; `mp config show|set --local` — root's recovery path when the API does not start, while the normal path goes through the API and is in the web UI too;
- installation and the binary's daemon roles (`mp setup`, `api`, `agent`, `helper`, `fsop`, `update-run`, `app-exec`, `sandbox-exec`, `demo`) and shell conveniences (`completion`, `help`, `version`);
- three reads the API allows an ordinary account too — `mp php list`, `mp web tls` and `mp dns-provider types` — are not in its TUI menus: the web UI shows them to administrators only (an ordinary account meets the PHP branches as the choices of the site form).

The `mp` flags that set no request field — output (`--json`, `--no-wait`), secret passing (`--…-stdin`) and paths to local files (`--cert`, `--key`, `--chain`, `--set`, `mp files get|put`) — need no counterpart in the web UI: it takes secrets and file contents through its own fields, file pickers and uploads.

## 2. CLI — `monopanel` (alias `mp`)

Connection: locally — `/run/monopanel/api.sock` (authorisation by the process uid: root is the administrator, a client's uid is that client's account, so a client with SSH access manages their own hosting with the same `mp`; the socket is open only to root and the `monopanel-cli` group, which the panel fills with shell accounts, and requests coming out of php-fpm, Apache and nginx processes are refused); remotely — `--server https://host:8443 --token …` or the `MP_SERVER` / `MP_TOKEN` variables, and `--insecure` or `MP_INSECURE=1` for a self-signed certificate. Only `help`, `completion` and `version` work without the API, and, as root on the server, also `setup`, `config show|set --local` and `update trust`.

Global flags: `--json` (machine-readable output), `--no-wait` (do not wait for the job), `--server`, `--token`, `--insecure`, `--config` (path to `config.yaml`, by default `/etc/monopanel/config.yaml` or `$MONOPANEL_CONFIG`), `--version`.

```
mp                                   # TUI menu (in an interactive terminal; otherwise help)
mp setup [--hostname …] [--listen …] [--admin-login …] [--admin-password …|--password-stdin]
         [--no-start] [--install-units] [--binary …]   # as root; running it again only creates what is missing
mp status                            # panel, host, services, jobs
mp doctor                            # diagnostics (restic's sandbox, the accounts' Apaches and the panel port's socket included)
mp whoami                            # who you are to the panel: account, role, how you are authenticated, token, 2FA
mp version
mp update                            # what is installed and what is available
mp update check | apply [--version v0.8.19]
mp update settings [--repo owner/name] [--channel stable|beta] [--check-hours 24] [--auto-apply] [--api …]
                   [--token …|--token-stdin|--clear-token]   # --repo "-" turns updates off
mp update trust [--key <fork key>] [--restart] | --clear | --off   # root on the server only; no flags — which key checks the signature
mp config show [--local]             # name, HTTPS address, logging, job workers, directories
mp config set <key> <value> [--restart] [--local]       # web.hostname, web.listen, log.level, log.format, jobs.workers
mp config templates                  # the built-in configuration templates and their overrides

mp user add <login> [--password …|--password-stdin|--generate] [--email …] [--role user|admin] [--shell] [--quota MB]   # the quota is only stored for now
mp user set <login> [--password …|--password-stdin|--generate] [--current-password …|--current-password-stdin] [--email …] [--shell|--sftp-only] [--status active|suspended]   # one's own password is changed with the current one (asked on a terminal)
mp user list | show <login> | totp-reset <login>
mp user rm <login> [--purge] [--yes]
mp user share list <login> | add <login> --site <domain> --path <folder> [--name …] [--no-php] [--entry] | entry <login> <name> [--off] | rm <login> <name>   # a folder of another account's site over SFTP; entry — the session starts inside it; IN HOME — the path in the home, --json adds mount_point
mp totp status | setup [--qr] | enable <code> | disable [--password-stdin]   # 2FA of your own account; set it up again after disable

mp site add <domain> --user <login> [--php 8.4] [--mode fpm|apache|proxy] [--backend http://127.0.0.1:3000]
            [--alias www.<domain>] [--www] [--redirect-www none|to_www|to_root] [--preset wordpress|joomla|opencart|bitrix]
            [--ssl auto|none] [--no-https-redirect] [--http2=false] [--http3] [--ip …] [--ipv6 …|none] [--docroot public]
            [--ini key=value] [--allow CIDR] [--pm ondemand|dynamic|static] [--max-children 8] [--max-body 64m]
            [--static-by-nginx=false] [--allow-exec] [--security-headers] [--rate-limit 10]
mp site set <domain> [the flags of site add but --user and --no-https-redirect] [--rm-alias …] [--no-www]
            [--https-redirect=false] [--allow-all] [--sessions files|valkey]   # --docroot '' — the site root, --preset '' — generic
mp site list | show <domain> | presets
mp site apply | fix | suspend | unsuspend <domain>   # fix — owner, permissions, ACLs and SELinux labels of the site's files
mp site rm <domain> [--purge]
mp site logs <domain> [--type access|error|php|slow|apache-access|apache-error] [-n 100]
mp site nginx <domain> [--set file | --clear | --generated]   # custom directives in server {}: the agent's check, nginx -t, rollback
mp site php <domain>                 # effective PHP settings and where each one comes from
mp site tls <domain> [--dns <provider>] [--staging] [--email …]   # a certificate for the domain and its aliases; the site switches to HTTPS
mp site move-ip <new-address> [--from <previous>]  # after the host's IP changes: all sites on the vanished addresses at once
mp selinux [enforcing|permissive] [--yes]
mp cms list
mp cms install <domain> wordpress|joomla|opencart|bitrix [--title …] [--admin-login …] [--admin-password …|--password-stdin]
               [--admin-email …] [--force]   # Bitrix: --edition start|standard|small_business|business, --solution clean|demo|<id>

mp php list [--available]
mp php install <version> | remove <version>
mp php ext list <version> | enable <version> <extension> | disable <version> <extension>
mp php ini [set key=value … | unset key …]   # PHP settings for all sites

mp stack list                        # apache shows "N of M running": an Apache per account with sites in that mode
mp stack install nginx|apache|percona|mysql|fail2ban|memcached|valkey|jpegoptim|git|composer|sphinx
mp stack remove memcached|valkey|jpegoptim|git|composer|sphinx
mp stack memcached [--memory-mb 256] [--max-conn 2048]    # without flags — show the settings
mp stack real-ip [--cloudflare|--no-cloudflare] [--from CIDR] [--clear-from]
mp valkey add cache|sessions [--user <login>] [--memory 128]   # the account's instance: create it or change its memory (restarts it)
mp valkey list [--user <login>] | restart cache|sessions | rm cache|sessions   # restart empties the cache

mp db create <name> --user <login> [--password …|--password-stdin] [--legacy-auth] [--remote]   # database and user <login>_<name>; the password is generated if omitted; the API refuses --remote for now
mp db list | passwd <name> [--password …|--password-stdin] | rm <name>
mp db engine                         # the database server; tune — recalculate its settings for the available memory
mp db engine tune
mp db config [set key=value … | unset key …]   # MySQL server settings; if it fails to start, the previous ones come back

mp ssl list | show <id|name> | rm <id|name>
mp ssl renew <id|name> | --all
mp ssl issue <name> [name…] [--dns <provider>] [--staging] [--rsa] [--email …] [--no-auto-renew] [--directory URL]
mp ssl import [name] --cert f --key f [--chain f] [--no-auto-renew]
mp ssl panel                         # the panel's own certificate and the record behind it
mp ssl panel issue [--dns …] [--staging] [--rsa] [--email …] | import --cert f --key f | self-signed
mp web tls                           # which certificate the panel is serving right now
mp dns-provider types | list
mp dns-provider add <name> --type cloudflare --cred CLOUDFLARE_DNS_API_TOKEN=…|--cred-stdin | rm <name>

mp backup target add <name> --type local|sftp|s3|b2|rest --repo … [--password …|--password-stdin] [--env KEY=…|--env-stdin]
                   [--schedule daily] [--keep-daily 7] [--keep-weekly 4] [--keep-monthly 3]
mp backup target list | rm <name>
mp backup run [--target …] [--scope server|user:<login>|site:<domain>|db:<name>]
mp backup list [--target …] [-n 50] | snapshots [--target …]   # status: running, done, partial (a snapshot without some files), failed
mp backup restore <snapshot> [--target …] [--include path…] [--in-place]

mp cron list | add --schedule "*/5 * * * *" --command "…" [--comment …] [--disabled]
mp cron set <id> [--schedule …] [--command …] [--comment …] [--enable|--disable] | enable <id> | disable <id> | rm <id>
mp app add <name> --command … [--workdir …] [--env KEY=…|--env-stdin] [--env-file …] [--restart always|on-failure|no]
           [--description …] [--disabled]
mp app set <name> [--command …] [--workdir …] [--env …|--env-stdin] [--env-file …] [--restart …] [--description …]
           [--enable|--disable]
mp app list | show | start | stop | restart | logs [-n 100] | rm <name>   # cron, app and valkey: [--user <login>], your own account without it
mp files ls|get|put|mkdir|touch|rm|mv|chmod|extract|size --user <login> <path…>   # through the helper, as the client; touch --force empties a file

mp firewall status | enable | disable | apply
mp firewall allow|deny --port 8443 [--proto tcp] [--source CIDR] [--comment …]
mp firewall rm <id> | ban <ip> | unban <ip> | trust <ip|cidr> | untrust <ip|cidr>

mp mail install [--hostname …] [--no-pop3] | status | apply   # details in 06-mail.md
mp mail settings [--hostname …] [--max-size MB] [--pop3 true|false] [--port25 true|false] [--dkim true|false]
                 [--rbl …] [--webmail-port 2096]
mp mail domain list | add <domain> --user <login> [--send-only] [--lenient] [--no-dkim] | dkim <domain> | dns <domain> | rm <domain>
mp mail domain set <domain> [--active true|false] [--send-only true|false] [--lenient true|false]
mp mail box list [--domain …] | add <mailbox> [--password …|--password-stdin] [--name …] [--quota 1024] | rm <mailbox> [--purge]
mp mail box set <mailbox> [--password …|--password-stdin] [--name …] [--quota …] [--active true|false]   # no flags — a new password; --quota 0 — no limit
mp mail alias list [--domain …] | add <alias> <dest> [dest…] | rm <alias>   # @example.com — catch-all
mp mail webmail <domain> --user <login> [--php …] [--port 2096]   # Roundcube as a site of its own

mp migrate grant user:<login> [--hours 24]   # on the source: a token for one account, read-only; details in 07-migration.md
mp migrate plan | run --source https://old:8443 --token …|--token-stdin --scope user:<login> [--as <login>] [--insecure]
mp migrate plan | run --from fastpanel|bitrixvm --source root@old.example.com[:22] [--key …|--password-stdin]
                      [--scope user:<login>] [--domain …] [--as <login>]   # --domain — the main BitrixVM site

mp service status [unit] | start | stop | reload | restart | reload-or-restart | enable | disable <unit>
                                     # template instances (but monopanel-apache@<login>), targets and shell/power units: status only
mp service status|stop|start monopanel-apache@<login>   # an account's Apache
mp logs <unit> [-n 100]              # the unit's journal, through the agent: mp logs monopanel-apache@<login>
mp metrics [--range 1h|6h|24h|7d|30d]
mp job list [--status …] [--limit …] | show <id> | wait <id>
mp audit [-n 50] [--actor <login>] [--action auth.|token.|site.]   # audit log: sign-ins, tokens, changes
mp token create [--name …] [--scopes …] [--expires <days>] [--user <login>]
mp token list [--user …] | revoke <id>
mp webhook add --url … [--events job.failed,backup.run.*] [--secret …|--secret-stdin]
mp webhook list | test <id> | rm <id>
mp completion bash|zsh|fish|powershell   # shell completion script
```

Conventions:
- Commands backed by a job wait for it to finish by default and print its progress and log (`--no-wait` returns the job number at once).
- Exit codes: 0 — ok, 1 — error (an unknown command or flag and a missing argument too), 2 — invalid input (a value the command or the API refused with 400/422, nothing on stdin, no answer to a question), 3 — the job failed (also `mp doctor` with a failed check and `mp migrate plan` when the move is not possible yet), 4 — permission denied (the socket is closed, the API answered 401/403, `--local` not as root).
- `--json` prints a single JSON object (or array), without progress or hints.
- A secret need not go on the command line, where `ps` and the shell history show it: every password, token, webhook secret, DNS provider credential and the variables of a backup repository and of an app service have a `--…-stdin` twin (`--password-stdin`, `--token-stdin`, `--secret-stdin`, `--cred-stdin`, `--env-stdin`). The value is the first line of stdin, and a terminal asks for it without echo; `KEY=VALUE` lists come one per line up to an empty line or the end — after the password line when the command reads one too. Your own API token for `--server` goes in `MP_TOKEN`.
- `--user <login>` says whose object it is. An administrator names the account (required for `site add`, `db create`, `mail domain add`, `mail webmail` and `files`), an ordinary account works with its own. For `cron`, `app` and `valkey`, no `--user` means your own account, and an administrator's `list` shows everyone's; root on the socket has no panel account and names one every time except for `list`.
- Commands that ask for confirmation (`user rm`, `selinux permissive`) take `--yes`; with nobody to answer (stdin closed) they exit with code 2.
- For `migrate plan|run` the `--token` and `--insecure` flags belong to the source, and for `update settings` `--token` is the release repository's token; so these commands get your own token for `--server` in `MP_TOKEN`, and `migrate plan|run` also gets `MP_INSECURE=1` instead of `--insecure`.
- The CLI and the TUI speak the language of the terminal locale, the way the web UI follows the browser: a CIS language in `LC_ALL`, `LC_MESSAGES`, `LANG` or `LANGUAGE` gives Russian, anything else, including `C`/`POSIX`, English; `MP_LANG=ru` or `MP_LANG=en` sets it explicitly. The server's own messages are in English.
- `mp completion <shell>` prints the completion script; it is not part of the package yet.
- The agent checks custom nginx directives (`mp site nginx --set`) like generated ones: `fastcgi_pass` only to a site's pool socket, no `fastcgi_param PHP_VALUE/PHP_ADMIN_VALUE`, `proxy_method` a plain word, `uwsgi_modifier1` 0, and `include` never of `/etc/nginx/default.d` and with a wildcard in the file name only (see [03 §3](03-web-stack.md#3-nginx-template-mode-a-abridged)). The API returns such a refusal as a 422 "the panel does not take these directives (nothing was changed)" — before anything is written and without `nginx -t`; "nginx rejected the configuration" means the directives were written, `nginx -t` refused them and the previous version was restored.
- `mp doctor` warns with `[WARN] api socket` when `monopanel-api.socket` is enabled but the API listens on the port itself: while the API restarts, any local account can take the port. Look for the cause in `journalctl -u monopanel-api -u monopanel-api.socket`; `mp setup` fixes it. For a port below `net.ipv4.ip_unprivileged_port_start` the socket is kept off on purpose, and there is no warning.
- An action on a unit the agent does not take (template instances, targets, shells and power units only report their state) reaches `mp service` as the API's refusal with the agent's reason and exit code 4, not as an agent failure.

## 3. TUI menu

- Start: `mp` without arguments in an interactive terminal (otherwise it prints help), over SSH too. The TUI is a client of the same API: locally over the socket with the process's rights (root is the administrator, an account with a shell is that account), remotely with `mp --server https://host:8443 --token …` (or `MP_SERVER` / `MP_TOKEN`, `--insecure`).
- The header: the panel version, the host, its load and memory, who is signed in (login and role, and the panel's address when remote); it refreshes every 15 seconds and says so when the panel or the agent is unreachable.

The areas are those of the web UI navigation, in the same order. Each has its own screens and every command of its `mp` groups (`*`), so every command is in the menus. There is no console in the TUI: in a terminal the commands are at hand anyway — the `:` palette. A command group no area owns would land in an "Other" area; there is none now, so the menu has no such area.

| Area | What is there | `mp` groups |
|---|---|---|
| Dashboard | the summary, diagnostics with "fix the site", "fix every site" and "switch to enforcing", metrics, SELinux | `status`, `metrics`, `doctor`, `selinux` |
| Sites | the site list; the site page: settings, PHP values, certificate, CMS, logs, the custom and the generated nginx directives, apply, fix, suspend, delete, the site's files | `site`, `cms` |
| Users | the account list; the account page: change, password, shell or SFTP only, 2FA reset, delete; its cron jobs, app services, Valkey, shared folders, API tokens and files | `user`, `cron`, `app`, `valkey`, `token` |
| PHP | branches: install, remove, extensions; the server's php.ini | `php` |
| Extensions | stack components: install, reinstall, remove; memcached, real-ip | `stack` |
| Databases | databases and their passwords, the database server and its settings | `db` |
| Mail | status, domains, mailboxes, aliases, settings, webmail | `mail` |
| Files | an account's file manager | `files` |
| SSL | certificates, the panel certificate, DNS providers | `ssl`, `web`, `dns-provider` |
| Jobs | jobs with a filter, a job's log, waiting for one | `job` |
| Audit log | the audit log with a filter | `audit` |
| Firewall | status, rules, bans, trusted addresses | `firewall` |
| Services | the units of the panel and the stack: start, stop, restart, autostart, journal; any unit by name | `service`, `logs` |
| Backups | repositories, runs, snapshots, restore | `backup` |
| Migration | a token for another panel, the dry run, the move | `migrate` |
| Settings | API tokens, 2FA, `whoami`; for an administrator webhooks, updates, the panel configuration, templates | `token`, `totp`, `whoami`, `webhook`, `update`, `config` |

- Screens are tables and cards of API data, read through `internal/client` like the CLI does. Actions are bound to the selected row and run with a letter (hints at the bottom); Enter opens the row.
- Every action is an `mp` command. Its form is built from the command's arguments and flags, with the selected row's values filled in; a flag with a list of values becomes a choice, `--php` offers the installed branches. An edit form opens with the current values, and only what changed goes to the command — such fields are marked with ●. While the form is filled in, the `mp …` line that will run is shown under it, secrets as `•••`.
- Passwords, tokens and credentials go to the command through stdin (`--password-stdin` and the other twins), never on its command line; for a flag without such a twin the form warns that `ps` shows the value.
- The command runs as a child process of the same binary, which gets the panel address and the token through the environment (`MP_SERVER`, `MP_TOKEN`, `MP_INSECURE`), not as arguments. The output streams into a window as it comes, and commands backed by a job wait for it, as in the CLI. Views (`show`, `status`, logs) run at once, without a form, when nothing is missing.
- Confirmations come wherever the web UI asks, and before whatever deletes, stops or interrupts something: the whole command, a note on what will happen and `y/N`. Deleting an account, deleting a site with its files (`--purge`) and restoring a snapshot over the files (`--in-place`) need the login, the domain or the snapshot ID typed. A command that asks by itself (`user rm`) gets `--yes` from the TUI after the TUI has asked.
- Files and a site's custom nginx directives are edited in `$VISUAL` or `$EDITOR` (without them — `editor`, nano or vi): `mp files get` → the editor → `mp files put` (or `mp site nginx --set`, checked with `nginx -t`). If saving fails, `e` opens the edit again; a copy that may not have been saved is named on the main menu at the next start and removed after a day. `mp totp setup` runs in the terminal itself to show the QR code. After `mp migrate plan` the output window offers `mp migrate run` with the same values.
- `?` or F1 — help: the screen's actions with their `mp` commands, every command of the area and the keys; Enter runs the chosen one. In a form F1 is the help of the command and of each field. `*` — every command of the area. `:` — the palette: any `mp` line, with Tab completing commands and flags; a form asks for whatever the line lacks, and a group (`site`) opens the list of its commands.
- Roles: an ordinary account sees the areas the web UI shows it — Dashboard, Sites, Account (its own cron jobs, app services, Valkey, shared folders, tokens, password and 2FA), Databases, Mail, Files, Jobs, Audit log and Settings — and the administrators' commands and flags (`--user` and others) are not in its menus and forms. The API decides anyway.
- Local and remote: with `--server` the menus do not offer what changes the machine directly, past the API — `mp update trust` and the `--local` flag of `mp config show|set`.

Keys:

| Where | Keys |
|---|---|
| Menus and lists | ↑/↓ or j/k, Home/End, in lists PgUp/PgDn too — move; Enter — open (in files — enter a directory, Backspace — one level up); a letter — an action; `/` — search the list; `f` — filter (jobs, audit log, metrics); `r` — refresh; `*` — every command of the area; Esc or q — back, quit on the main menu |
| Form | Tab, Shift+Tab, ↑/↓ — fields; space, ←/→ — toggle and choose; Enter — next field, run on the last one; Ctrl+S — run; Ctrl+R — reveal a secret; Ctrl+P — pass a flag even if its value has not changed; F1 — the command's help; Esc — cancel. Letters are text in a form, so `?`, `:` and `q` do nothing there |
| Output | ↑/↓, PgUp/PgDn, Home/End — scroll; Ctrl+C — interrupt the command (a job on the server goes on); r — run again (views); e — edit again; n — the next command (`migrate run` after `plan`); Esc, q or Enter — back, to the form when the command failed |
| Confirmation | y or Enter — yes, n or Esc — no; where the login, the domain or the snapshot ID is asked for, type it and press Enter |
| Help and palette | ↑/↓ — choose, Enter — run; in the palette Tab, or Enter on a suggestion, completes the line; Esc — close |
| Everywhere | `?` or F1 — help, `:` — palette, Ctrl+C — quit (in the window of a running command — interrupt it) |

## 4. REST API

- Base path `/api/v1`, specification `/api/v1/openapi.json`, interactive documentation `/api/v1/docs` (Stoplight Elements, embedded in the binary). The documentation, the specification and `/schemas/*` are served only to signed-in callers (a session or an API token; a migration token does not count): `curl -H "Authorization: Bearer $TOKEN" https://host:8443/api/v1/openapi.json`.
- Authentication: `Authorization: Bearer <token>`, a cookie session for the UI (+ CSRF), unix peer-cred for the CLI on the local socket. A token acts with the rights of its account (`admin` or `user`); the `migrate:user:<login>` scope restricts it to reading what a migration of one account needs, and any other scope is just a label. Only an administrator's token can carry a migration scope (usually issued with `mp migrate grant`); the panel revokes such tokens of ordinary accounts when it starts.
- Mutations that need work on the server are asynchronous: `202 Accepted` with a `job_id` in the response; `GET /jobs/{id}`, `GET /jobs/{id}/events` (SSE: progress, log lines, completion). Fast operations (a token, a setting, a Valkey instance) are synchronous `200/201/204`. An administrator sees every job, any other account the jobs it started; the secrets in a job's parameters (passwords, tokens, secrets, the encrypted `*_enc` fields) come back as `[redacted]`.
- Actions that restart the panel itself are carried out after the answer: `PUT /system/config` with `restart: true`, and stopping or restarting `monopanel-api` and `monopanel-agent` and restarting `monopanel-api.socket` with `POST /services/{unit}`. The client reconnects a few seconds later — at the new address when `web.listen` changed.
- `monopanel-api.socket` holds the panel's port ([01 §3.1](01-architecture.md#31-monopaneld-api--the-unprivileged-process)). A restart that keeps the address restarts `monopanel-api.service` alone — the socket holds the port throughout. A restart that applies a new `web.listen` (with the change or later, on its own) and a restart of the API on the Services page stop the socket first, and it comes back with the API on the new address. Stopping `monopanel-api` or `monopanel-agent` leaves the socket be: the next connection to the panel's port starts the API again, and through `Requires=` the agent with it. To stop the panel completely: `systemctl stop monopanel-api.socket monopanel-api monopanel-agent`. A restart of `monopanel-api.socket` (`mp service restart monopanel-api.socket`, the Services page) is carried out after the answer as a stop of the socket and a restart of the API; `mp` waits until the panel answers again. `mp config set --local web.listen …` writes the socket's drop-in too; without `--restart`, apply it with `systemctl stop monopanel-api.socket && systemctl restart monopanel-api`. `monopanel api` started by hand gets no port while the socket listens. `GET /system/status` gives where the port comes from in `panel.listen_via`: `socket` or `itself`. The drop-in `/etc/systemd/system/monopanel-api.service.d/socket.conf` asks the binary before every start of the API (`monopanel api-socket-sync --probe`), and a version 0.8.19 or older — after a self-update that rolled back or a package downgrade — takes the port itself, without the socket ([01 §9](01-architecture.md#9-packaging-installation-updates)).
- `PUT /system/config` changes five keys of `config.yaml` — `web.hostname`, `web.listen`, `log.level`, `log.format`, `jobs.workers`; the agent writes the file, and the release signing key is not among them. A new `web.listen` must be an address the server can listen on, and its port is opened in the firewall before anything is written. Changes take effect once monopanel-api restarts; `restart_pending` in `GET /system/config` says the file no longer matches what the running process loaded.
- Errors are RFC 9457 `application/problem+json` with an `errors[]` field for per-field validation. `/services/{unit}` passes an agent's refusal on with its own status (403, 409, 400, 422) and reason; 502 means the agent is unreachable or the action itself failed.
- Lists are returned whole; jobs support `?limit` and `?status`, the audit log `?limit`, `?actor` and `?action`, backups `?limit` and `?target`. Cursor pagination, filters, field selection and `Idempotency-Key` are planned.
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
GET/POST        /users/{login}/shares             PATCH/DELETE /users/{login}/shares/{name}   (mount_point — where the folder is mounted)
POST            /users/{login}/valkey/{purpose}/restart
GET/POST        /sites                            GET/PATCH/DELETE /sites/{domain}   GET /sites/presets
POST            /sites/{domain}/apply | fix | suspend | unsuspend | cms | tls/issue    POST /sites/move-ip
GET             /sites/{domain}/logs/{type}       GET/PUT /sites/{domain}/nginx      GET /sites/{domain}/php
GET             /cms
GET/POST        /php/versions                     DELETE /php/versions/{version}     GET/PUT /php/settings
GET/POST        /php/versions/{version}/extensions
GET/POST        /databases                        DELETE /databases/{name}           POST /databases/{name}/password
GET             /db/engine                        POST /db/engine/tune               GET/PUT /db/engine/config
GET             /stack                            POST /stack/install                DELETE /stack/{component}
GET/PUT         /stack/memcached | /stack/nginx/real-ip
GET/POST        /certificates                     GET/DELETE /certificates/{id}      POST /certificates/import | /{id}/renew
GET/DELETE      /ssl/panel                        POST /ssl/panel/issue | import      GET /web/tls
GET/POST        /dns-providers                    DELETE /dns-providers/{name}        GET /dns-providers/types
GET/POST        /backups/targets                  DELETE /backups/targets/{ref}       GET /backups/targets/{ref}/snapshots
GET             /backups                          POST /backups/run | restore
GET             /firewall                         POST /firewall/enable | disable | apply | rules | ban | unban | trust | untrust
DELETE          /firewall/rules/{id}
GET             /mail                             POST /mail/install | apply | webmail    PUT /mail/settings
GET/POST        /mail/domains | mailboxes | aliases    PATCH/DELETE /mail/domains/{name} | mailboxes/{address}
POST            /mail/domains/{name}/dkim         GET /mail/domains/{name}/dns        DELETE /mail/aliases/{address}
GET             /files?user=&path=                GET/PUT /files/content              POST /files/op   (through the helper)
POST            /migrate/grant | plan | run       GET /migrate/plan | state | files | dump   (on the source: migration token only)
GET             /services                         GET/POST /services/{unit}   (start | stop | reload | restart | reload-or-restart | enable | disable)
GET             /jobs | /jobs/{id} | /jobs/{id}/events
GET/POST        /tokens                           DELETE /tokens/{id}
GET/POST        /webhooks                         DELETE /webhooks/{id}               POST /webhooks/{id}/test
GET             /system/status | doctor | metrics | logs/{unit} | audit    GET/PUT /system/selinux
GET/PUT         /system/update                    POST /system/update/check | apply
GET/PUT         /system/config                    GET /system/templates   (the panel's settings in config.yaml, the templates and their overrides)
POST            /system/console                   (mp commands with streamed output, administrator only)
GET             /health
```

## 5. Billing integration (WHMCS and others)

The WHMCS module has not been written yet; the API covers its actions as follows:

| WHMCS action | API calls |
|---|---|
| `CreateAccount` | `POST /users` → `POST /sites` (+ `POST /databases` according to the package) |
| `SuspendAccount` / `UnsuspendAccount` | `PATCH /users/{login}` with `status: suspended\|active` (sign-in to the panel and SFTP/SSH; a suspension ends the account's browser sessions); the 503 placeholder on the sites — `POST /sites/{domain}/suspend` / `unsuspend` |
| `TerminateAccount` | `DELETE /users/{login}?purge=true` |
| `ChangePassword` | `PATCH /users/{login}` with `password` (the account's browser sessions end, API tokens stay) |
| `ChangePackage` | `PATCH /sites/{domain}` (PHP branch, `php_ini`, `fpm_max_children`…); the package's quotas and limits are planned |
| `ServiceSingleSignOn` | planned (a one-time sign-in link) |
| `UsageUpdate` | planned (disk and traffic accounting per account) |

The API requirements this covers: tokens with an account's rights, webhooks when jobs finish, stable identifiers (`login`, `domain`).
