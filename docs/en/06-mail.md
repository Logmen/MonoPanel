# 06. Mail server

Implemented 2026-09-07, verified on Ubuntu 24.04 (postfix 3.8.6, dovecot 2.3.21, opendkim 2.11, Roundcube 1.7.4).

The panel runs a complete mail server for the domains it serves: receiving and sending (SMTP), mailbox access (IMAP/POP3), message signing (DKIM) and webmail (Roundcube). As everywhere in MonoPanel, the source of truth is the panel database: the postfix and dovecot configuration is regenerated whole on every change, and there is no need to edit it by hand.

## 1. What it is built from

| Component | Role | Why |
|---|---|---|
| **postfix** | SMTP: receiving on 25, submission by clients on 587/465, outbound transport | The de facto standard, a predictable configuration, a set of maps that is cheap to maintain |
| **dovecot** | IMAP, POP3, LMTP delivery, SASL for postfix, quotas, sieve filters | One daemon is responsible for the mailboxes as a whole: passwords, delivery, quotas and filters. postfix does not touch the mail files at all |
| **opendkim** | Signing outgoing and verifying incoming mail | A separate milter instead of built-in solutions: the keys live apart from postfix, and rotation does not touch the MTA |
| **Roundcube** | Webmail | Installed as a regular panel site: its own php-fpm pool, its own certificate, its own owner |

Mail users are **not** unix accounts: all mailboxes belong to the system user `vmail`, while addresses, passwords and quotas live in the panel database.

## 2. What lives where

| Path | What it is |
|---|---|
| `/etc/postfix/main.cf`, `master.cf` | Generated whole by the panel |
| `/etc/postfix/monopanel/domains`\|`mailboxes`\|`aliases`\|`senders` | Maps (`hash:`), built with `postmap` after being written |
| `/etc/postfix/monopanel/submission_header_checks` | Strips `Received` and `X-Originating-IP` from messages sent through 587/465 |
| `/etc/dovecot/dovecot.conf` | Generated whole; the distribution's `conf.d` is deliberately not included |
| `/etc/dovecot/monopanel/users` | passwd file: address, `{BLF-CRYPT}` hash, `userdb_quota_rule` |
| `/etc/opendkim.conf`, `/etc/opendkim/KeyTable`, `SigningTable`, `TrustedHosts` | Signing tables |
| `/etc/opendkim/keys/<domain>/<selector>.private` | Private key (0600, opendkim); a copy is kept encrypted in the panel database |
| `/var/mail/monopanel/<domain>/<mailbox>/` | Maildir, owned by `vmail` |

The panel keeps domains, mailboxes and aliases in the tables `mail_domains`, `mailboxes`, `mail_aliases` (migration `0011_mail.sql`); server settings are in `settings` under the `mail` key.

## 3. Mail flow

**Incoming:** `:25 smtpd` → checks (HELO, sender domain, `reject_unlisted_recipient`, optionally RBL) → `opendkim` verifies the signature → `virtual_transport = lmtp:unix:private/dovecot-lmtp` → dovecot stores the message in the Maildir, applies sieve filters and counts the quota.

**Outgoing from a client:** `:587` (STARTTLS) or `:465` (TLS from the start) → SASL to dovecot over `private/auth` → `reject_sender_login_mismatch` (you can send only from your own address or from an alias that leads to this mailbox) → `submission-header-cleanup` removes traces of the client's IP → opendkim signs → delivery.

**Mail from sites:** PHP `mail()` goes through `/usr/sbin/sendmail` → `pickup` → the same path, DKIM signing included.

## 4. Ports

| Port | What | Notes |
|---|---|---|
| 25 | SMTP | Receiving mail from outside. If the port is taken by another daemon, the installation sees it by the banner and turns receiving off (`port25=false`) instead of bringing postfix down |
| 587, 465 | Submission, SMTPS | Only with authentication and TLS |
| 143, 993 | IMAP, IMAPS | `ssl = required`, plaintext login without TLS is forbidden |
| 110, 995 | POP3, POP3S | Switched off as a whole (`mp mail settings --pop3 false`) |
| 4190 | ManageSieve | Filters from Roundcube |
| 2096 (any) | Webmail | Optional: Roundcube on the mail server's name and certificate |

The ports are opened in the panel's firewall automatically. The panel does not take systemd's word for it: after applying the configuration it checks that the ports really answer (the postfix unit is a wrapper and reports "active" even when the master did not come up).

## 5. TLS

The certificate is taken from the panel's store by the mail server's name (the same ACME as for sites). If there is no certificate, the panel issues a self-signed one so that the daemons start, and says so in the warnings. When an ACME certificate for that name is issued or renewed, the `cert.issue` job regenerates the mail configuration itself and restarts postfix and dovecot.

## 6. DNS

`mp mail domain dns <domain>` (and the "DNS" button in the web UI) shows the required records and what is published right now — the queries go to public resolvers (1.1.1.1, 8.8.8.8), not to the system one:

- `A` of the mail host → the server's address;
- `MX` of the domain → the mail host;
- `TXT` SPF: `v=spf1 mx a:<host> -all`;
- `TXT` `<selector>._domainkey.<domain>` — the DKIM public key (RSA 2048);
- `TXT` `_dmarc.<domain>`;
- `PTR` for the IP — set up by the hosting provider; without it, mail ends up in spam more often.

## 7. Commands

```bash
mp mail install --hostname mail.example.com   # postfix + dovecot + opendkim, certificate, ports
mp mail status                                # services, ports, TLS, warnings
mp mail settings --pop3 false --max-size 25 --rbl zen.spamhaus.org
mp mail domain add example.com --user alex    # + a DKIM key
mp mail domain dns example.com                # what to publish in DNS and what is already published
mp mail domain dkim example.com               # a new key
mp mail box add ivan@example.com --quota 2048 # the password is generated and shown once
mp mail box set ivan@example.com --password … # or with no flags — generate a new one
mp mail alias add info@example.com ivan@example.com
mp mail alias add @example.com ivan@example.com   # catch-all
mp mail webmail webmail.example.com --user alex   # Roundcube as a separate site
mp mail apply                                  # regenerate the configuration
```

A disabled mailbox (`--active false`) disappears both from the postfix map and from the dovecot passwords: mail to it is not accepted, logging in is impossible, the messages on disk stay.

## 8. Lenient domain

The panel will not deliver to a regular domain a message from a sender with a non-existent domain or a broken HELO — and rightly so. But there are domains for which incoming mail is not correspondence but material for analysis: a diagnostic sink has to see exactly the message the others rejected, otherwise it cannot explain to the sender what is broken on their side.

For such domains there is the "lenient" flag (`mp mail domain add … --lenient`, `mp mail domain set <domain> --lenient true`). Technically it puts the domain into the `check_recipient_access` map, and the strict HELO and sender checks have been moved into the same list as the recipient checks — an `OK` from the map cuts that list short before they are reached. Receiving stays closed for foreign domains (`reject_unauth_destination`) and for non-existent mailboxes.

This is how the mail server and mail-tester coexist on the panel's dev host: postfix listens on port 25, the tester's domain is declared lenient with a catch-all into a separate service mailbox, and the tester itself fetches the messages from that mailbox over IMAP (`[imap] enabled = yes`) and takes the sender IP, HELO and TLS from the `Received` header that postfix added. The tester no longer needs its own SMTP daemon, and the panel no longer has to give up the port.

## 9. Webmail

`mp mail webmail <domain>` creates a regular panel site, downloads the official Roundcube archive (the version and sha256 are built into the panel), unpacks it as root (the code does not belong to the site owner and cannot be rewritten over the web), creates a MySQL database, loads the schema and writes `config/config.inc.php`: IMAP and SMTP go to the mail host over TLS, the plugins `archive`, `zipdownload`, `managesieve`, `newmail_notifier`, and the `installer` directory is removed. From then on it is a regular site: its own certificate, its own php-fpm pool, its own PHP version.

**A port instead of a domain.** `--port 2096` (or `mp mail settings --webmail-port 2096`) publishes the same installation on a port of the mail host: `https://<mail host>:2096/`. The panel writes a separate nginx server block to `http.d/monopanel-webmail.conf` with the mail server's name and certificate, opens the port in the firewall and also listens on loopback, so that checks from the host itself work. No separate DNS record and no separate certificate for webmail are needed then — handy when mail is set up on a server that already has a name. `--port 0` removes the publication.

## 10. Limitations

- **Debian/Ubuntu.** On EL the packages exist, but the configuration has not been verified — the installation refuses right away instead of breaking halfway.
- **dovecot 2.3.** In 2.4 (Debian 13) the configuration syntax changed; the panel sees this from the package version and refuses to write a configuration that has not been verified. A template for 2.4 is the next step.
- **Anti-spam** — only postfix's own tools: HELO/sender checks, connection limits and optional RBLs. There is no full content filter (rspamd) yet; incoming messages go through DKIM verification, but nothing is rejected based on its result.
- **Quotas** are counted by dovecot (`maildir:User quota`); going over is reported to the sender as `552 5.2.2 Mailbox is full`.
- There is no mailbox password change from webmail: passwords are changed in the panel.
- The DKIM key starts signing messages at once, while recipients verify it against the DNS record — until that record is published, the signature will show as "not matching". Publish the TXT record right after adding the domain (`mp mail domain dns`) or add the domain with `--no-dkim` if there is nowhere to put the key yet.
