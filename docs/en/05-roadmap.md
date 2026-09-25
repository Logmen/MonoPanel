# 05. Roadmap, test matrix, CI

## Stage 0 — foundation (done 2026-09-05, verified on Ubuntu 24.04)

- [x] Repository, Go build (`make build`), `nfpm.yaml`, units, sysusers/tmpfiles, `install.sh`, `mp setup`.
- [x] OS Profile for Debian/Ubuntu and EL9/EL10 (+ generic for dev machines); agent with `ApplyConfigSet`, `EnsureGroup`, `EnsureUnixUser`, `EnsureDirs`, `Service`, `Pkg`; peer-cred on both sockets.
- [x] SQLite schema and migrations; auth (argon2id, sessions, Bearer tokens, CSRF check); job runner with locks; SSE.
- [x] Installing nginx from nginx.org (`mp stack install nginx`), nginx/Apache/php-fpm templates with golden tests, CLI and TUI.
- [x] ACME (lego, HTTP-01 via webroot): `mp ssl issue/list/renew/rm`, the `certificates` table, automatic renewal, hot swap of the panel's certificate (`mp web tls`). Verified by issuing a production Let's Encrypt certificate on the test host.
- [ ] Building the SvelteKit app (needs Node; for now a placeholder `web/build/index.html`).
- [ ] A deb/rpm repository and package signing.
- [ ] Installing PHP (Sury/Remi) and Percona/MySQL 8.4 with `auth_socket` — moved to the start of stage 1.
- [x] VM matrix: a Proxmox testbed with nine machines covering the whole OS matrix, `make testbed-matrix` (2026-09-09, see [docs/08-testbed.md](08-testbed.md)). Not wired into CI yet — that needs a self-hosted runner with access to the host.

## Stage 1 — MVP (done 2026-09-05, verified on Ubuntu 24.04)

- [x] PHP 5.6–8.5 via Sury/Remi (`mp php`), several versions side by side.
- [x] Sites in modes A (nginx → php-fpm), B (nginx → Apache → php-fpm) and proxy; per-site php_value; ACL; placeholder page; automatic certificate.
- [x] Apache 2.4 as a component (Debian/Ubuntu). EL — not verified.
- [x] Databases: Percona/MySQL 8.4 with `auth_socket`, databases and users, legacy authentication for PHP < 7.4. phpMyAdmin — not done.
- [x] Cron, firewall (nftables) + fail2ban, restic backups, metrics, logs, `mp doctor`, DNS-01, TUI parity, web UI (SvelteKit), ru.
- [ ] The full test matrix in CI on 9 OSes (needs a self-hosted runner with VMs).

## Stage 2 — v1.0 (partial, 2026-09-05)

- [x] File manager through the helper (`mp files`, API `/files`), SFTP chroot, unix passwords.
- [x] Webhooks (HMAC), TOTP 2FA, API tokens, config history (`confhistory`), template overrides, `mp doctor`.
- [ ] Own PHP builds, a terminal in the browser, quotas, self-update, a WHMCS module, WebAuthn, a confined SELinux domain.

### Automated testing (2026-09-06)
- [x] Fake agent `internal/agent/agenttest`: a unix socket, typed responses, a record of every call — covers the panel's jobs (sites, users, presets) without root or systemd.
- [x] Site pipeline tests: nginx/pool rendering, CMS presets and their PHP values, `php_ini` taking priority over the preset, allow-list, validation rejections, suspend, removal, custom nginx directives with rollback, cascading user removal. Coverage of `internal/api` 9.5% → 21.2%, 23.3% overall.
- [x] Readiness timeouts moved into `Server.SetReadinessWaits`: tests do not wait for nginx or the php-fpm socket (the suite runs in ~2 s).
- [x] `make check` (fmt + vet + lint + test), `make test-race`, `make cover`, `make web-check`, `make help`.
- [x] `golangci-lint` v2.13.2 with `.golangci.yml`; deliberately ignored errors are marked `//nolint:errcheck` with a reason, real findings (S1017, S1009, ineffassign, unconvert) are fixed.
- [x] `scripts/check-templates.sh`: golden configs are checked by real `nginx -t` and `apachectl -t` (CI installs nginx-core and apache2).
- [x] `e2e/` (tag `e2e`): a scenario against a live panel — account, site with a preset, PHP answers, the ban on PHP in uploads works, database, removal; cleans up after itself. Token or login/password via environment variables.
- [x] Tokens over the local socket: `POST /tokens` accepts `user`, root without an account gets a token for the single administrator (with several — an error with the list), an administrator sees and revokes the tokens of any account; `make e2e HOST=…` mints a token over ssh and revokes it after the run.
- [x] CI: parallel jobs go / lint / templates / web, module and pnpm cache, `-race` with coverage in the summary, amd64 + arm64 builds, e2e via `workflow_dispatch`.

### Managing PHP extensions (2026-09-06)
- [x] `GET|POST /php/versions/{v}/extensions`, `mp php ext list|enable|disable`, toggles in the branch card on the PHP page. The state is read from the branch's `conf.d` (phpquery answers from the Debian registry and keeps showing a module after `phpdismod`), the list of available ones from `mods-available`. After a toggle the branch's php-fpm is restarted.
- [x] Only for a whole branch: php-fpm is one master per version, pools inherit its extensions, an extension cannot be disabled for a single site. The interface says so plainly.
- [x] Extensions a typical site does not work without are marked and need confirmation; nothing is forbidden — the administrator is root anyway.
- [x] The agent gained a `dir/list` operation with an allow-list of directories (`/etc/php/*/mods-available`, `*/conf.d`, `/etc/opt/remi/php*/php.d`): without it the panel sees only the loaded modules, not the disabled ones.

### PHP 8.5 (2026-09-06)
- [x] Installing 8.5 failed: the panel asked for `php8.5-opcache`, which Sury does not have — starting with 8.5, opcache is built into the core (`php8.5 -m` shows Zend OPcache, `opcache.enable => On`). A separate package exists only for 7.0 through 8.4; the rule and a test for both boundaries are in `internal/osprofile/php.go`.

### Adapting the web UI to phones and tablets (2026-09-06)
- [x] Layout: below `lg` the 240px-wide sidebar turns into a slide-out drawer with a backdrop and a header with a button; above it, the old column stays. Following an item closes the menu, tap targets are larger.
- [x] Tables: one technique for all nine pages, because they all use the `.tbl` class. Below 40rem a row becomes a card, the column headers move into labels on the left (`data-label` on the cells), and the table stops being a table (`display: block`) — otherwise it sizes its width by the content and long values run off the edge. Cells without a label — the action buttons — stay on the right.
- [x] Small things: a full-width toast on phones, wrapping of a long docroot, the file manager toolbar on two lines, smaller page padding on a narrow screen.
- [x] Checked at 375×812 and 768×1024 on every page: nothing runs off the screen horizontally anywhere, including the Monaco editor and the site page with its tabs.

### File manager in the web UI (2026-09-06)
- [x] `FileManager.svelte`: browsing the account's home directory, breadcrumbs, jumping to a site's directory, creating a folder and a file, upload by drag-and-drop and by picking files, download, rename, permissions, archive extraction, multiple selection and deletion — all on top of the existing `/files` API, that is, as the owner through the helper.
- [x] Text file editor: line numbers, Ctrl+S, Tab, a guard against leaving with unsaved changes. Binary files (by extension or a zero byte) and files over 1 MB open for download only. No syntax highlighting — it would mean a new npm dependency.
- [x] The `/files` page (with an account picker for the administrator) and the "Files" tab on the site page, which opens in the site's docroot.
- [x] The editor is Monaco, the same core as in VS Code (`web/static/monaco`, MIT): highlighting for php/html/css/js/ts/json/xml/ini/shell/sql/yaml/markdown/python/dockerfile, find and replace, multiple cursors, folding, the command palette, the theme follows the panel's theme, the height follows the content. The build is trimmed from 24 MB to 4.7: no language services (the json/css/html/ts IntelliSense workers), no interface translations and no unneeded modes; the binary grew from 26 to 31 MB. It loads lazily; if loading fails, a plain field remains. It is vendored into the repository rather than pulled from npm or a CDN: the panel must install on a server without internet access, and the repository has no node tooling. `font-src data:` was added to the CSP — the icon font is embedded in Monaco's CSS.
- [x] API: a `touch` operation (`fsop touch` with `O_EXCL`) — huma does not accept an empty PUT body, and creating a file must not overwrite an existing one; `force` empties the file. Tests in `internal/cli/fsop_test.go` for touch and for ".." stopping at the home directory.

### Updating the panel from releases (2026-09-06)
- [x] `internal/updater`: GitHub releases (or GitHub Enterprise via `api`), chosen by version rather than by publication date; stable/beta channel; fixed artefact names (`monopanel_<v>_<arch>.deb`, `monopanel-<v>.<arch>.rpm`, `monopanel-linux-<arch>`, `SHA256SUMS`, `SHA256SUMS.sig`).
- [x] Release signature with ed25519: `scripts/release keygen|sign|verify`, the private key is a repository secret, the public one is `update.public_key` in `config.yaml` (root-only, cannot be changed from the panel). With a key set, an unsigned release is not installed; the agent verifies the signature again rather than trusting the hash from the API process.
- [x] Installing outside the panel: the agent's `POST /v1/panel/install` starts the transient unit `monopanel-update.service` (`StartTransientUnit`), which survives the restart of the API and the agent; `mp update-run` installs the package, restarts the units, waits for `/health` with the new version and restores the previous binary if it does not answer. The outcome is written to `<data>/updates/state.json` and goes into the audit log after the restart.
- [x] API `GET|PUT /system/update`, `POST /system/update/check|apply` (administrator only), the `panel.update` job, a scheduled daily check and `auto_apply`; the repository token is encrypted with secretbox.
- [x] `mp update`, `mp update check|apply|settings|trust`; the "Panel update" card in the web UI's Settings waits for the restart and reloads the page; an `update` check in `mp doctor`.
- [x] Installing the latest version counts as a check in itself (2026-09-24): the `panel.update` job remembers the release it found, and after `mp update apply` the status no longer reports the release seen by the previous check as the latest.
- [x] Packages: `make packages` (deb+rpm × amd64+arm64 + binaries + SHA256SUMS), `make release VERSION=…`, the `release.yml` workflow on a `v*` tag; `preremove` no longer stops the panel during an update, `postinstall` restarts the units only on an upgrade.

### Added while migrating sites from FASTPANEL (2026-09-05)
- [x] Per-site IP allow-list (`sites.allow_from`, `mp site add|set --allow`), the ACME challenge stays reachable.
- [x] Importing ready-made certificates (`POST /certificates/import`, `mp ssl import`); Let's Encrypt certificates keep renewing through ACME.
- [x] Trusted proxies for `real_ip` (`mp stack real-ip --cloudflare`, `--from`), the file `http.d/10-real-ip.conf`.
- [x] User app services (`apps`, `mp app`): a systemd unit running as the user, `ProtectSystem=full`, `PrivateTmp`.
- [x] HSTS in the server block and in the static location when HTTPS is forced.
- [x] Web UI: allow-list in the site settings, certificate import, app services and cron in the user card, real-ip in Settings.
- [x] User removal (`DELETE /users/{login}?purge=`, job `user.delete`, `mp user rm`): sites → certificates → databases → app services → crontab → unix account (agent `user/remove`: kills the uid's processes, `userdel [-r]` only for a home under www_root, uid ≥ 1000).
- [x] Editing a site's nginx directives (`sites/<domain>.d/custom.conf`, `GET|PUT /sites/{domain}/nginx`, `mp site nginx`): `nginx -t`, rollback and the error text in the response; the generated server block is shown read-only.
- [x] A site's PHP settings: `GET /sites/{domain}/php` (effective values + allowed keys), an overrides editor in the PHP tab; the key list is extended (opcache.jit, session.cookie_*, max_file_uploads …).
- [x] CMS presets (`sites.preset`, `templates/nginx/presets/*.conf.tmpl` as `{{ define "preset-…" }}` in the shared template set, `presetIni` in `ops_presets.go`): WordPress, Joomla, 1C-Bitrix, OpenCart; chosen in the creation form and in the site settings, `GET /sites/presets`; verified on nginx with real requests (pretty URLs, deny rules, /api, urlrewrite, _route_).
- [x] Web UI design 0.4: light/dark theme tokens (`prefers-color-scheme` + `data-theme` from localStorage, initialised before the first paint, with a hash in the CSP), animations (page transitions, row stagger, job progress, skeleton), icons, modal confirmations.

### Mail server (2026-09-07)
- [x] `mp mail install`: postfix + dovecot + opendkim in one job — packages, the `vmail` user, configuration, certificate, firewall ports. Verified on Ubuntu 24.04: submission with a real certificate, SASL through dovecot, LMTP delivery, IMAP/POP3/ManageSieve, DKIM signing, rejection of sender spoofing.
- [x] Domains, mailboxes and aliases (including catch-all) in the panel database; postfix reads `hash:` maps, dovecot a passwd file with `{BLF-CRYPT}` and `userdb_quota_rule`. A disabled mailbox disappears from both maps: mail is not accepted, logging in is impossible, the messages on disk stay.
- [x] DKIM: an RSA 2048 key is generated by the panel, the private key is stored encrypted in the database and as a file for opendkim; `mp mail domain dns` shows MX/SPF/DKIM/DMARC/PTR and checks them against public resolvers (1.1.1.1, 8.8.8.8).
- [x] The mail certificate comes from the same store as the sites' certificates; on issue and renewal the `cert.issue` job restarts postfix and dovecot itself. While there is no certificate, a self-signed one is put in place, and the panel warns about it.
- [x] A busy port 25 (on the dev host it is held by the mail-tester receiver) does not break the installation: the panel identifies the foreign daemon by its banner, turns off receiving from outside and says so. After the configuration is applied, the ports are actually probed — the postfix unit reports "active" even when the master did not come up.
- [x] Webmail: `mp mail webmail <domain>` installs Roundcube 1.7.4 (an archive with a sha256 check) as a regular panel site — its own database, its own php-fpm pool, its own certificate; `installer` is removed.
- [x] Web UI `/mail`: status, ports, domains with DNS hints, mailboxes, aliases, webmail installation; CLI `mp mail`; tests for configuration rendering, maps, catch-all and yielding port 25.
- [x] Webmail on a port: `--port 2096` publishes Roundcube on the mail server's name and certificate (its own nginx server block in `http.d`, the port in the firewall, listens on loopback too) — without a separate DNS record or a second certificate.
- [x] Lenient domain (`--lenient`): the strict HELO and sender checks moved into one list with the recipient checks, and the `check_recipient_access` map cuts that list short with `OK`. This is how postfix took over port 25 on the dev host, while mail-tester fetches mail from a service mailbox over IMAP and takes the sender IP from `Received` — verified with a real message from an external address and a non-existent sender domain (the tester's report: "IP отправителя 83.97.77.254 (по заголовкам)", i.e. sender IP 83.97.77.254, taken from the headers).
- [x] The mail page took 4.3 s to load: eight ports were probed one after another, and twice, and on the TLS-wrapped ports the probe waited for a banner that never comes. Probing all at once, once per request → 0.02 s; the web UI fetches the four lists in parallel and shows a skeleton.
- [ ] dovecot 2.4 (Debian 13, Ubuntu 26.04) — a different configuration syntax; the installation refuses until the template is written and verified.
- [ ] EL 9/10: the packages exist (opendkim from EPEL), the configuration has not been verified.
- [ ] Anti-spam beyond the postfix checks (rspamd), changing a mailbox password from Roundcube, mail client autoconfiguration (autoconfig/autodiscover).

### Migration between panels (2026-09-09)
- [x] Direct migration MonoPanel → MonoPanel: `mp migrate grant` on the source, `mp migrate plan` (a dry run listing conflicts, changes nothing) and `mp migrate run` on the target. The design — [07-migration.md](07-migration.md).
- [x] The source is only read: state, a file stream (`tar`) and a dump stream. A token with the scope `migrate:user:<login>` lets through only GET under `/migrate` and only for its own account; the other scopes remain labels so as not to break tokens issued earlier.
- [x] Passwords move as hashes: the panel's argon2id, yescrypt from shadow (`chpasswd -e`), `SHOW CREATE USER ... AS '<hash>'` for MySQL, `{BLF-CRYPT}` for mailboxes. Users do not notice the move, and the panel never sees plain text anywhere.
- [x] The streams go straight through two new agent operations (`/v1/stream/out`, `/v1/stream/in`) and a recursive `chown`: the home directory is never stored whole on disk anywhere.
- [x] A test with two panels at once: a source fixture and a target fixture with real HTTP between them — it checks the token scope, the dry run, moving an account, a site and cron, the password hash and both tar streams. On a live server the sending half was verified: the file stream, the state with secrets and a certificate.
- [ ] A resync (`resync`) and a finish (`finish`) that orders certificates after the DNS switch.
- [x] Adapters for foreign panels (2026-09-12): `--from bitrixvm` and `--from fastpanel` read the old server over ssh and produce the same bundle; verified on the testbed against bitrix-env 9 and FASTPANEL 1.11 (`scripts/testbed/sources.sh`).
- [ ] The `site:` and `server:` scopes, a package file, the `plain` adapter.

### Web UI in two languages (2026-09-13)
- [x] The whole web UI is translated into English; Russian and English live in the dictionary `web/src/lib/i18n/msgs/*.ts` (one fragment per page), `t()`/`tn()` take the current language from the state — switching redraws the interface without a reload.
- [x] The default language comes from the browser language: Russian for the languages of CIS countries (ru, uk, be, kk, ky, uz, tg, tk, hy, az, ka, ro-MD…), English for everyone else; the same list is in `app.html`, so that `<html lang>` is correct before the app starts.
- [x] The "Auto / Русский / English" switch in Settings and on the sign-in screen; the choice is kept in `localStorage.lang`; dates and units (`when()`, `bytes()`) follow the language.
- [ ] API messages, job logs, doctor and the CLI are Russian-only for now — they need a dictionary on the Go side and a language on the job.

### Firewall: rule order and lockout protection (2026-09-14)
- [x] The chain is assembled in tiers: allow with a source → deny → always-open ports and allow without a source. Previously deny came first, and "deny 8443 + allow 8443 from the VPN" closed the panel to everyone, the VPN included.
- [x] Guard: a deny without a source on SSH or the panel port is accepted only when an allow with a source exists; removing the last such allow, as well as a deny or ban that covers the request's address, is refused. The status shows the closed ports and whom they are open to (`restricted`, a line in `mp firewall status`, tags on the page).

### Static files: compression and caching (2026-09-15)
- [x] The editor was slow to open: http.FileServer served the embedded static files without gzip, ETag or Cache-Control, and every page reload fetched the 4.4 MB of Monaco again in six sequential requests. Now a file is gzipped once into memory (Monaco 4.4 → 1.2 MB), hashed chunks (`_app/immutable/*`, `name-<hash>.js`) are cached for a year as immutable, everything else is `no-cache` with a weak ETag and 304. FileManager preloads Monaco during idle time as soon as the files page opens, not on a click.

### API documentation without a third-party origin (2026-09-21)
- [x] The `/api/v1/docs` page loaded Stoplight Elements from unpkg.com, and the panel sets no CSP for `/api/` paths: a third-party script ran on the panel's origin next to the administrator's session. Now Elements 9.0.15 is embedded in the binary (`internal/api/elements`, Apache-2.0), and the page and its files are served by the panel itself with the CSP `default-src 'none'; script-src 'self'`, without inline scripts or eval; a test pins the files' sha384 to the npm release and checks that the page has no third-party addresses.
- [x] The documentation, the OpenAPI specification (`/openapi*`) and the JSON schemas (`/schemas/*`) are behind authentication: a session or an API token of any account; a migration token gets 403, an anonymous visitor 401 (a browser following a link to `/docs` is redirected to sign-in). Only `/health` and `/auth/login` remain available without signing in.
- [x] `/health` returns the panel version only to a signed-in caller (session, token, local socket); an anonymous one gets `status` and `time`. The sign-in screen no longer shows the version.

### Global PHP settings (2026-09-23)
- [x] php.ini values had a "panel" source that could only be changed one site at a time. Now there are layers: panel → global → preset → site. The global layer is `GET|PUT /php/settings` (admin), `mp php ini [set k=v|unset k]` and the "PHP settings for all sites" card on the PHP page; saving rebuilds the pools of all PHP sites (`site.apply` for each). A site's PHP tab shows the "global" source and the order of the layers.

### Site log rotation (2026-09-24)
- [x] Nobody rotated the logs in `/var/www/<login>/data/logs`: the panel wrote no logrotate configuration, and the distribution's configs do not cover that path. Now `/etc/logrotate.d/monopanel-sites` has a block per account with `su <login>` (the directory belongs to the client, root does nothing in it): weekly or at 100 MB, eight copies, compression from the second one, `create 0660 <login> <login>`. All of a site's logs belong to the account with mode 0660: the panel creates them before nginx, Apache and php-fpm open them, and hands over to the account the ones those daemons created earlier — via a new `mode` flag of the agent's `chown` operation, through an open file (`O_NOFOLLOW`, `fchown`, `fchmod`). Otherwise logrotate 3.18 on EL9 could not cope: when rotating it opens the log, and when compressing it opens it for writing. The file is rewritten when an account is created or removed (the removed account's entry goes before `userdel`, so that logrotate does not meet a non-existent user) and when the panel starts; before writing, it is checked with `logrotate --debug`.
- [x] nginx worker processes could not reopen the site logs after USR1 — neither after this rotation nor after nginx's own nightly rotation: `data/logs` had no traverse permission for the `monopanel-web` group, so every night `emerg … Permission denied` landed in the nginx log and writing continued into the old file. `data/logs` got the ACL `g:monopanel-web:x` on site creation and on `mp site fix`, and existing accounts got it at panel start. Apache reopens its logs with a graceful restart on USR1 via the pid file: `systemctl` called from logrotate runs into SELinux on EL.
- [x] The php-fpm slow log on EL was empty: php-fpm takes the trace via `ptrace` of the worker process, and the stock policy silently denies `httpd_t` the `sys_ptrace` capability. The panel installs its own policy module `monopanel` (CIL, `semodule -i`) with `allow httpd_t self:capability sys_ptrace` and `self:process ptrace` — together with preparing the host for hosting, and on already prepared hosts at startup, without another relabel.
- [x] Verified on Debian 13 (with Apache), Ubuntu 22.04 (logrotate 3.19), Rocky 9 (3.18) and AlmaLinux 10 in enforcing: after rotation nginx, Apache, PHP and php-fpm write to the new files, the second rotation compresses the first as the account, the slow log on EL has traces, `logrotate.service` succeeds, no SELinux denials.

### Valkey per account (2026-09-24)
- [x] `mp stack install valkey` installs Valkey from the distribution (Debian 13, Ubuntu 24.04/26.04, EL9/EL10 AppStream, Debian 12 — from backports, if they are enabled), and where it has none — Ubuntu 22.04, Debian 12 without backports — Redis (6.0 and 7.0) with the same protocol; the package's shared instance (127.0.0.1:6379 without a password, for everyone) is stopped and disabled. The server cannot be removed while there are instances on it.
- [x] An account has two separate instances: `cache` (allkeys-lru, `--save ""`) and `sessions` (volatile-lru, a snapshot every minute). Each one is `monopanel-valkey-<login>-<purpose>.service` running as the user, `Type=notify` (a restart returns once the snapshot is loaded and the socket accepts connections), `MemoryMax` at twice maxmemory for headroom, the whole configuration in the arguments (the account has no access to `/etc/monopanel`). It listens only on a unix socket with mode 600 in a 0700 directory (`RuntimeDirectory=`), snapshots go to `StateDirectory=`; `mp valkey add|list|restart|rm`, `/users/{login}/valkey[/{purpose}]`, the valkey button on the "Users" page. Removing an account takes its instances with it; migration between panels carries over their settings without the data.
- [x] A site's PHP sessions in Valkey: `mp site set <domain> --sessions valkey|files`, the "PHP sessions" field in the site settings; the pool gets `session.save_handler = redis` and the path `unix://…/valkey.sock` through php_admin_value. This needs the sessions instance and the redis extension on the site's PHP branch; while a site keeps its sessions there, neither the instance nor the extension can be removed (409); a site in proxy mode keeps no sessions.
- [x] SELinux (EL): the sockets are `redis_var_run_t`, the snapshot directories `redis_var_lib_t`, while `/var/lib/monopanel-valkey` itself stays `var_lib_t`: init_t cannot add entries to a `redis_var_lib_t` directory, and `StateDirectory=` failed with EACCES without a single AVC. The unit has no `ProtectKernelTunables=`, `ProtectKernelModules=` or `RestrictAddressFamilies=`: for a non-root `User=` they turn on NoNewPrivileges, and under it the EL9 policy does not allow the init_t → redis_t transition, so the server stayed in init_t without the right to write snapshots. Live check on Debian 13, Ubuntu 22.04, AlmaLinux 10 and Rocky 9 (enforcing): another account and the web server get "Permission denied", a PHP session through php-fpm survives a restart, no AVCs.





## Stage 3 — v1.x

- [x] `proxy` mode for Node/Python/Docker applications.
- [x] Mail: postfix + dovecot + opendkim + Roundcube (Debian/Ubuntu).
- [x] Migrating accounts between panels (MonoPanel → MonoPanel).
- [ ] Isolated pools with cgroup limits, FTP, DNS (PowerDNS), WAF, a reseller role, an aarch64 build (cross-compilation is ready: `make build-arm64`), multi-server.

## OS matrix

| OS | Modes | PHP (sample) | Database |
|---|---|---|---|
| Debian 12, 13 | A, B | 5.6, 7.4, 8.2, 8.5 | Percona 8.4, MySQL 8.4 |
| Ubuntu 22.04, 24.04, 26.04 | A, B | 7.4, 8.3, 8.5 | Percona 8.4 |
| AlmaLinux, Rocky Linux, Oracle Linux 9, 10 | A, B | 7.4, 8.4, 8.5 (+ 5.6 on EL9) | Percona 8.4, MySQL 8.4 |

Runner: the [testbed](08-testbed.md) on Proxmox — eleven VMs from cloud images with cloud-init and a rollback to a clean snapshot; it is run from a workstation, not in CI. Containers are not enough for SELinux, nftables, quotas and systemd slices.

The `make testbed-matrix` scenario: package installation → `mp setup` → nginx, PHP, database server → e2e (user → site with a preset → database → nginx and PHP answer → removal) → `mp doctor` without errors. Before a release, `make testbed-full` adds migrations between panels, CMSes on every machine and moves from BitrixVM and FASTPANEL. Planned: HTTPS via Pebble, backup → restore → suspend → purge.

A periodic job (weekly) is planned: package availability in the vendor repositories across the matrix and the appearance of new versions (PHP 8.6/9.0, nginx stable, Percona 8.4.x, OS releases).

## Risks and how they are addressed

| Risk | Mitigation |
|---|---|
| Sury/Remi disappear or break compatibility | Stage 2: own builds and repository; OS Profile allows keeping both sources at once |
| MySQL 9.x removes `mysql_native_password` | We stay on 8.4 LTS until 2032; for legacy PHP — 8.4 only; upgrading to 9.x is a manual procedure |
| A new OS release without vendor packages | Support is announced only after the matrix passes; a weekly check |
| Manual config edits by the administrator | Include directories, `confhistory`, `mp doctor` shows drift, `mp site apply` regenerates the site |
| Compromise of the web layer | api without privileges; agent with an allow-list; helper with setuid; secrets are encrypted |
| Growth in the number of sites (hundreds) | `pm=ondemand`, one master per version, SQLite with WAL holds tens of thousands of entities; metrics are rollups |
