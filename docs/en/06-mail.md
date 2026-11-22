# 06. Mail server

Working since 2026-09-07. Verified on Ubuntu 24.04 (postfix 3.8.6, dovecot 2.3.21, opendkim 2.11, Roundcube 1.7.4) and on Ubuntu 26.04 (postfix 3.10.6, dovecot 2.4.2). Since 2026-09-30 on EL too: AlmaLinux, Rocky and Oracle Linux 9 (postfix 3.5, dovecot 2.3.16) and 10 (postfix 3.8, dovecot 2.3.21), SELinux enforcing.

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
| `/etc/dovecot/dovecot.conf` | Generated whole; the distribution's `conf.d` is deliberately not included. The template is chosen by the installed dovecot version: 2.3 (Debian 12, Ubuntu 22.04/24.04) or 2.4 (Debian 13, Ubuntu 26.04) |
| `/etc/dovecot/monopanel/users` | passwd file: address, `{BLF-CRYPT}` hash, the quota (`userdb_quota_rule` for dovecot 2.3, `userdb_quota_storage_size` for 2.4) |
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
mp mail domain add shop.example.com --user alex --send-only   # a provider receives the mail; only DKIM and sending here
mp mail domain dns example.com                # what to publish in DNS and what is already published
mp mail domain dkim example.com               # a new key
mp mail domain set example.com --active false # switch the whole domain off
mp mail box add ivan@example.com --quota 2048 # the password is generated and shown once
mp mail box add info@example.com --quota 0    # no limit; without --quota — 1024 MB
mp mail box set ivan@example.com --password-stdin   # a new password from stdin; with no flags — generate one
mp mail alias add info@example.com ivan@example.com
mp mail alias add @example.com ivan@example.com   # catch-all
mp mail webmail webmail.example.com --user alex   # Roundcube as a separate site
mp mail apply                                  # regenerate the configuration
```

A disabled mailbox (`--active false`) disappears both from the postfix map and from the dovecot passwords: mail to it is not accepted, logging in is impossible, the messages on disk stay. A disabled domain drops out of all the maps at once: its mail is not accepted, its mailboxes cannot log in over IMAP, POP3 or SMTP, and outgoing messages are not signed with DKIM; the messages on disk stay here too.

The same can be done on the "Mail" page of the web UI ([09](09-web-ui.md#mail)). There an administrator installs the mail server, changes its settings (the name, the message size, the blocklists, the webmail port, POP3, DKIM signing, receiving on port 25), applies the configuration again ("Apply now" — `mp mail apply`) and installs the webmail — the domain, the owner, the PHP version and the port — or reinstalls it ("Reinstall / update", §10); the services, the certificate and the ports are shown there too. An ordinary account sees and edits only its own domains, mailboxes and aliases. Adding a domain has the "Send only" and "Lenient domain" boxes and, while DKIM signing is on for the server, "DKIM key" (unticked, it is `--no-dkim`); a domain's row has DNS, a new DKIM key, the send-only and lenient toggles, switching off and deletion. A mailbox is created with a password of your own or without one — then the password is generated and shown once; later its name, quota (0 — no limit), password and state are edited, and the "Password" button issues a new random one; a mailbox is deleted with its messages if you choose. An alias — the catch-all `@domain` too — is created with a list of destinations, edited and deleted. Mailboxes and aliases can be shown for one domain. Before anything that changes how mail is received or signed, the page asks and explains the consequences.

## 8. Send-only domain

Sometimes another server receives the domain's mail — the MX is at a mail provider — while this server hosts only a site that needs to send mail from the domain with a DKIM signature. Such a domain is added with the `--send-only` flag (in the web UI — the "send only" checkbox when adding a domain and a toggle in the list): the panel creates a DKIM key and allows sending, but the domain does not enter postfix's receiving maps. So a message from the site to an address of the same domain goes to the real MX instead of a local mailbox that does not exist. Such a domain has no mailboxes or aliases; the DNS hint does not require an MX pointing at this server and suggests adding the server to the provider's SPF rather than replacing the record.

## 9. Lenient domain

The panel will not deliver to a regular domain a message from a sender with a non-existent domain or a broken HELO — and rightly so. But there are domains for which incoming mail is not correspondence but material for analysis: a diagnostic sink has to see exactly the message the others rejected, otherwise it cannot explain to the sender what is broken on their side.

For such domains there is the "lenient" flag (`mp mail domain add … --lenient`, `mp mail domain set <domain> --lenient true`). Technically it puts the domain into the `check_recipient_access` map, and the strict HELO and sender checks have been moved into the same list as the recipient checks — an `OK` from the map cuts that list short before they are reached. Receiving stays closed for foreign domains (`reject_unauth_destination`) and for non-existent mailboxes.

This is how the mail server and mail-tester coexist on the panel's dev host: postfix listens on port 25, the tester's domain is declared lenient with a catch-all into a separate service mailbox, and the tester itself fetches the messages from that mailbox over IMAP (`[imap] enabled = yes`) and takes the sender IP, HELO and TLS from the `Received` header that postfix added. The tester no longer needs its own SMTP daemon, and the panel no longer has to give up the port.

## 10. Webmail

`mp mail webmail <domain>` creates a regular panel site, downloads the official Roundcube archive (the version and sha256 are built into the panel), unpacks it as root (the code does not belong to the site owner and cannot be rewritten over the web), creates a MySQL database, loads the schema and writes `config/config.inc.php`: IMAP and SMTP go to the mail host over TLS, the plugins `archive`, `zipdownload`, `managesieve`, `newmail_notifier`, and the `installer` directory is removed. From then on it is a regular site: its own certificate, its own php-fpm pool, its own PHP version.

**Reinstalling and updating.** `mp mail webmail <domain>` over an installed webmail ("Reinstall / update" in the web UI) runs the installation again: Roundcube of the version that comes with the panel is unpacked over the site's files, and `config/config.inc.php` is written from the mail server's current settings (name, certificate, port). Everything the webmail keeps stays: the database with the users' contacts, identities and settings, its password and the DES key — a new key would sign everybody out of the webmail — and plugins added to the configuration by hand stay enabled. A database of an older Roundcube has its schema brought up to date by Roundcube's own `bin/updatedb.sh`; a database of a Roundcube newer than the one the panel installs stops the job before a single file is touched. Other manual edits in `config.inc.php` are replaced. An existing site keeps its owner; the webmail does not go into a site that already holds files of its own — only into a new site, an empty one, one with the panel's placeholder page or one with an earlier Roundcube.

**A port instead of a domain.** `--port 2096` (or `mp mail settings --webmail-port 2096`) publishes the same installation on a port of the mail host: `https://<mail host>:2096/`. The panel writes a separate nginx server block to `http.d/monopanel-webmail.conf` with the mail server's name and certificate, opens the port in the firewall and also listens on loopback, so that checks from the host itself work. No separate DNS record and no separate certificate for webmail are needed then — handy when mail is set up on a server that already has a name. `--port 0` removes the publication. The block logs into the site log tree; on an enforcing EL host it waits for that tree's SELinux label rule: without it the block stays as it is, and the rest of a mail change is applied with a warning ([02 §6](02-platform-matrix.md#6-selinux-el9--el10)).

## 11. Limitations

- **EL is built a little differently.** The packages are `postfix`, `dovecot`, `dovecot-pigeonhole` and `opendkim` from EPEL; on EL 10 its libraries are in EPEL too, while on EL 9 only CodeReady Builder has them (`crb`, `ol9_codeready_builder` on Oracle Linux). The panel does not switch that repository on: when the packages do not install without it, it serves that one transaction (`dnf install --enablerepo=…`). Enabled for good, it would feed its own packages into a plain `dnf upgrade`. The flip side: `sendmail-milter` and `libmemcached-awesome` on EL 9 get no updates while the repository is off. Version 0.8.15 switched it on for good; if it is still enabled, `mp doctor` says so and shows the command. The rest the panel adapts itself:
  - postfix 3.5 in EL 9 knows neither `compatibility_level = 3.6` nor `>=TLSv1.2` — it gets `2` and an exclusion list;
  - postfix in EL 10 is built without Berkeley DB — the maps and the TLS session caches are `lmdb:` there instead of `hash:` and `btree:` (the panel asks `postconf -m`);
  - opendkim listens on `127.0.0.1:8891` rather than a unix socket: the EL 10 policy has no domain for it and smtpd is kept away from its socket, but not from the port;
  - the panel's certificate directory is labelled `cert_t`, otherwise smtpd cannot read the certificate and STARTTLS answers "TLS not available";
  - the webmail port (`--port`) is labelled `http_port_t` and opened in firewalld.
- **Anti-spam** — only postfix's own tools: HELO/sender checks, connection limits and optional RBLs. There is no full content filter (rspamd) yet; incoming messages go through DKIM verification, but nothing is rejected based on its result.
- **Quotas** are counted by dovecot (`maildir:User quota` on 2.3, the `count` driver on 2.4); going over is reported to the sender as `552 5.2.2 Mailbox is full`. A quota of 0 means no limit (in the passwd file it is `userdb_quota_rule=*:storage=0` for dovecot 2.3 and `userdb_quota_storage_size=unlimited` for 2.4); a mailbox created without a quota gets 1024 MB.
- There is no mailbox password change from webmail: passwords are changed in the panel. A mailbox password is at most 72 bytes, and the panel refuses a longer one: bcrypt reads no further (a Latin letter is one byte, a Cyrillic one two).
- **The maps are checked on every write.** A domain that does not follow the DNS name rules; the mail server's hostname, a mailbox's address or local part, a DKIM selector, an alias's address or recipient that is empty or has a blank, a control character, `:`, `/` or `,`; a mailbox whose address is not "local part@domain"; a password hash with a colon or a control character — any of these stops the apply: the panel names the entry, and no file is written. So a value that got into the panel database past the API (a move from another server, a hand edit of the database) cannot add lines of its own to the maps — a password for somebody else's address or a mailbox in somebody else's Maildir — and deleting such a mailbox with its messages leaves the Maildir in place.
- The DKIM key starts signing messages at once, while recipients verify it against the DNS record — until that record is published, the signature will show as "not matching". Publish the TXT record right after adding the domain (`mp mail domain dns`) or add the domain with `--no-dkim` if there is nowhere to put the key yet.
