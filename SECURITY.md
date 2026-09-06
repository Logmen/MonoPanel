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
| latest release (0.6.x) | yes |
| anything older | no — update first |

## What the panel considers a boundary

The panel's whole design is a set of privilege boundaries. A report that crosses
one of these is in scope, and the more precisely it names the boundary the faster
it gets fixed.

- **API process → root.** `monopanel api` runs as an unprivileged user and reaches
  root only through `monopanel agent`, over a unix socket, with a closed set of
  typed operations and a path allow-list. Anything that makes the agent run,
  write, or install something outside that set is the most serious class of bug.
- **User → user, and user → administrator.** A panel account may only act on its
  own sites, databases, files and cron. Escaping that, or reaching an
  administrator-only endpoint, is in scope.
- **Client → host.** `monopanel helper` drops privileges irreversibly before
  touching client files, and SFTP-only accounts are chrooted. Escaping either is
  in scope, as is one site's php-fpm pool reading another site's files.
- **Authentication.** Session handling, Bearer tokens, TOTP, the login rate
  limiter, the Origin check that stands in for CSRF tokens.
- **Generated configuration.** Values that reach nginx, Apache, php-fpm, cron or
  systemd templates come from user input. Anything that escapes its context there
  — a domain name that becomes a directive, a custom snippet that survives
  validation and rollback — is in scope.
- **The update chain.** The panel installs packages into itself. Making it accept
  a package the release key did not sign, or one that does not match the signed
  checksums, is in scope. So is anything that lets a panel administrator (not
  root) change the trust anchor: the release key lives in `/etc/monopanel/config.yaml`,
  which only root may write, precisely so that it cannot be changed from the panel.
- **Secrets at rest.** DNS provider credentials, backup passwords and TOTP secrets
  are encrypted with the key in `/etc/monopanel/secret.key`. Recovering them
  without that file is in scope.

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
with an ed25519 key held as a repository secret; the public half is pinned on each
server with `mp update trust`. Before an update is installed, the API process
verifies the signature and the package digest, and the agent — which runs as root —
verifies both again itself rather than trusting the unprivileged process that
downloaded them. A panel with a pinned key refuses an unsigned release outright.

If you believe the signing key or a published release has been tampered with,
report it the same way as any other vulnerability.
