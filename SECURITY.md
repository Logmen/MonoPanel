# Security Policy

Reports in Russian are equally welcome — сообщения об уязвимостях на русском языке тоже приветствуются.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private reporting:
[Security → Report a vulnerability](https://github.com/Logmen/MonoPanel/security/advisories/new).
If that is unavailable to you, write to <logmen11@gmail.com> with `MonoPanel security`
in the subject.

Please include:

- the version (`mp version`) and the OS,
- what an attacker gains and what access they need to start,
- the smallest sequence of steps that shows it — a diff against a fresh
  `mp setup` is ideal,
- any logs from `journalctl -u monopanel-api -u monopanel-agent`.

MonoPanel is maintained by one person. Expect an acknowledgement within three
working days. For anything that crosses a privilege boundary, expect a fix or a
concrete plan within fourteen days; smaller issues may take longer. You will be
credited in the advisory and the release notes unless you ask otherwise.

Please test against your own installation. The panel installs on a throwaway VM
in a couple of minutes — never probe someone else's server.

## Supported versions

Only the latest release receives fixes; there are no maintenance branches before
1.0. Update with `mp update apply`.

| Version | Supported |
| --- | --- |
| latest release (0.8.x) | yes |
| anything older | no — update first |

## What the panel considers a boundary

The panel's whole design is a set of privilege boundaries. A report that crosses
one of these is in scope, and the more precisely it names the boundary the faster
it gets fixed.

- **API process → root.** `monopanel api` runs as an unprivileged user and reaches
  root only through `monopanel agent`, over a unix socket, with a closed set of
  typed operations and a path allow-list. Anything that makes the agent run,
  write, or install something outside that set is the most serious class of bug.
  That includes `config.yaml` and `secret.key`, which no generic agent operation
  writes, and the directories and files under `/var/www` that the agent makes or
  reads as root: a symlink a client plants there must not redirect such a write
  or read. The agent treats the API as untrusted: a program, an argument shape,
  a configuration directive, a systemd unit or a path the panel itself never
  sends, accepted by the agent and turned into root, is in scope — so is a path
  the API could swap between the agent's check and its use. restic, which reads
  the web root and the panel's trees as root for a backup and writes a snapshot
  for a restore, runs in a Landlock sandbox: a directory swapped for a symlink
  while it works makes root neither read nor write outside those trees.
- **The panel's port.** `monopanel-api.socket` (PID 1) holds the panel's HTTPS
  port and hands it to the API, so the port is never free while the API
  restarts, is stopped or has not started yet at boot. A way for a local account
  to bind that port is in scope, and so is a way for the API to make PID 1 bind
  a port the API could not bind itself (below
  `net.ipv4.ip_unprivileged_port_start`) or to write a socket drop-in of its own
  choosing.
- **User → user, and user → administrator.** A panel account may only act on its
  own sites, databases, files and cron. Escaping that, or reaching an
  administrator-only endpoint, is in scope. Migration scopes, which read a whole
  account (password hashes, keys, files, databases), belong to administrators'
  tokens only; one that works for an ordinary account is in scope as well. So is
  a password or token of the panel that another local user can read on a command
  line (`ps`): the panel hands them to installers, tools and child `mp` processes
  on stdin or in the environment.
- **Migration source → this panel.** An account being moved in comes from
  another server: FASTPANEL or BitrixVM read over ssh, or a MonoPanel with an
  administrator of its own. The receiving panel holds every value of the bundle
  to the rules of its own API, rebuilds the database accounts instead of running
  the source's statements, loads the dumps as the database's own account and
  keeps only plain files, directories and links inside the tree from the file
  stream. A bundle, a dump or an archive that gets something past that — a file
  or a grant outside the account, a line of its own in a configuration — is in
  scope.
- **Client → host.** `monopanel helper` drops privileges irreversibly before
  touching client files, and SFTP-only accounts are chrooted. Escaping either is
  in scope, as is one site's php-fpm pool reading another site's files, one
  account's site in Apache mode reaching another account's files, php-fpm pools
  or requests (through its .htaccess, a symlink, a status page, a request sent
  straight to another account's Apache for its dotfiles, or the port of an Apache
  that restarts), or the guest of a shared folder with `--no-php` running code as
  the site's owner or reading the owner's files outside the folder. So is a guest
  that gets its shared folder bound anywhere but its mount point, and making a
  backup store a file outside the account's home — or another account's — as the
  account's own (restic's Landlock sandbox, in which a backup follows no symlink
  in the web root; `mp doctor` warns on a kernel without Landlock or where the
  agent may not mount).
- **Authentication.** Session handling (a password change ends the account's
  other browser sessions, a suspension all of them), Bearer tokens, TOTP (setting
  it up anew takes switching it off with the password first), the login rate
  limiter, the Origin check that stands in for CSRF tokens.
- **Generated configuration.** Values that reach nginx, Apache, php-fpm, cron or
  systemd templates and the mail maps come from user input. Anything that escapes
  its context there — a domain name that becomes a directive, a value that starts
  a line of its own in a crontab, a unit or a mail map, a custom snippet that
  survives validation and rollback — is in scope. nginx configurations hand
  FastCGI requests to the panel's own site pools alone
  (`fastcgi_pass unix:<run_dir>/php/<domain>.sock`, no
  `PHP_VALUE`/`PHP_ADMIN_VALUE`, no include of EL's `/etc/nginx/default.d`) and
  never let a client choose the first bytes nginx sends to a socket
  (`proxy_method` a plain word, `uwsgi_modifier1` 0, no stream relay to a unix
  socket by `proxy_pass` or an upstream). Every PHP branch's stock `www` pool is
  kept idle (nobody, a root-only socket) and no other file may add to it.
  Include patterns of nginx, php-fpm and dovecot are checked the way glob(3)
  expands them (a wildcard in the file name only, no backslash or braces), and
  an account's Apache includes nothing but its own virtual hosts and their
  directives. Neither the API nor a client can run code as `www-data`, `apache`
  or `nobody` through php-fpm.
- **The update chain.** The panel installs packages into itself. Making it accept
  a package the release key did not sign, or one that does not match the signed
  checksums, is in scope. So is anything that lets a panel administrator (not
  root) change the trust anchor: the key built into the panel, or the one
  `/etc/monopanel/config.yaml` names in its place (`update.public_key`).
  The panel changes only its own five settings in that file (`web.hostname`,
  `web.listen`, `log.level`, `log.format`, `jobs.workers`), through a dedicated
  agent operation that refuses every other key; the key itself is changed only
  by root on the server with `mp update trust`, and the web console refuses that
  command.
- **Secrets at rest.** DNS provider credentials, backup passwords and TOTP secrets
  are encrypted with the key in `/etc/monopanel/secret.key`. Recovering them
  without that file is in scope, and so is reading back through the API or a
  webhook a secret a job carries: both show `[redacted]` in its place.

## What is not a vulnerability

- Actions by an administrator. The panel administrator is root on that server by
  design: installing packages, running the database, writing nginx configuration.
  There is no boundary between them and the host, and there is not meant to be.
- Anything that already requires root on the host.
- Bugs in nginx, Apache, PHP, MySQL/Percona, restic or fail2ban themselves —
  report those upstream. Do tell us if MonoPanel's default configuration makes an
  upstream weakness reachable when it otherwise would not be.
- The self-signed certificate warning right after `mp setup`. It is documented and
  is replaced by `mp ssl issue <hostname>`.
- Missing hardening headers on a hosted site: the site owner controls those, and
  the panel provides a place to set them.
- Resource exhaustion caused by an authenticated administrator.
- Scanner output with no demonstrated impact.

## How updates are protected

Releases are built by GitHub Actions from a tag. The workflow signs `SHA256SUMS`
with an ed25519 key held as a repository secret; the public half is built into the
panel (a fork builds in its own, or pins it on each server with `mp update trust`).
Before an update is installed, the API process verifies the signature and the
package digest, and the agent — which runs as root — verifies both again itself
rather than trusting the unprivileged process that downloaded them. The agent
copies the package into its own root-only directory
(`/var/lib/monopanel-agent/updates`) without following a symlink, and only a
copy whose SHA-256 is the one the signed checksum list gives the release's own
package file takes the name `update-run` installs; a request that does not match
changes nothing already staged. A panel with a key, built in or pinned, refuses
an unsigned release outright. The agent also refuses to go back: an older
release, or the release a later build (`git describe`, e.g. `0.8.19-5-gabc1234`)
was made from, is a downgrade — an older signed release would bring back what
later ones fixed — and is installed only by hand as root.

If you believe the signing key or a published release has been tampered with,
report it the same way as any other vulnerability.
