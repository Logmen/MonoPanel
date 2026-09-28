# 10. Shared folders: a contractor's access to one folder of a site

A common request: "give the contractor (an exchange service, the accountants, an agency) SFTP access to one folder of our site". The first idea is to create a user with its home directory inside the site. In MonoPanel that is neither needed nor possible:

- sshd chroots an SFTP user into its home and insists that the directory is owned by root and not writable by the group or others. A site folder belongs to the site owner, and if root took it, the site itself could not write there.
- The panel expects every account's home at `/var/www/<login>` with `data/www`, logs, tmp and the web server's ACLs. A foreign path in `home` sooner or later breaks `mp site fix`, backups or a migration.
- One unix user per account and a `0710` home is what keeps clients apart.

Instead the panel hands out a **shared folder**: the guest stays an ordinary account with its own home, and the site folder is mounted inside its chroot. After signing in the guest sees only that folder (and its own empty `data`), both sides and the web server read and write the files, and the site can be told never to execute PHP from the folder.

## 1. How it works

On `mp user share add` the panel:

1. creates the folder inside the site's docroot (if missing) — owned by the site owner, mode `2770`, setgid keeps the owner's group on new files;
2. sets ACLs on the folder: `rwx` for the guest and the owner, `r-x` for the web group, inherited by everything created inside;
3. makes a root-owned mount point in the guest's home (`/var/www/<guest>/<name>`) — the chroot stays root-owned, sshd is happy;
4. writes and enables a systemd mount unit that bind-mounts the folder there — it survives a reboot;
5. with `--no-php` adds a `location` to the site configuration that answers 403 to any `.php`, `.phtml` or `.phar` in the folder.

There is no way out of the bind mount: symlinks pointing outside do not resolve inside the chroot, `../` stops at the guest's home. On EL the site's SELinux labels are kept; nothing extra to set up.

## 2. Step by step

Example: the site `vstrade.kz` of the account `bitrix`; the contractor needs the folder `kaspy` for file exchange.

**Step 1. The guest.** If the contractor has no account yet, create one without a shell — SFTP is enough:

```bash
mp user add userkaspy --generate        # the password is printed once
```

The password is for SFTP (and for the panel, where the guest sees only its own folders).

**Step 2. The folder.** Hand out the site folder. The path is relative to the site's docroot; the folder is created if missing:

```bash
mp user share add userkaspy --site vstrade.kz --path kaspy --no-php
```

Keep `--no-php` whenever the folder is for files rather than code: without it the contractor could drop a `.php` there and the site would execute it as the `bitrix` account — with all the site's rights. `--name` sets the folder name in the guest's home when it should differ from the last path segment.

**Step 3. Check.**

```bash
mp user share list userkaspy
```

The `MOUNTED` column should say `yes` and `PHP` — `denied`. The contractor connects over SFTP as `userkaspy` with the password from step 1 and sees the folder `kaspy` — that is the site's `/var/www/bitrix/data/www/vstrade.kz/kaspy`.

The same in the web UI: Users → the "folders" button on the account → site, folder, the "no PHP" box → Add.

**Step 4. When the access is no longer needed.**

```bash
mp user share rm userkaspy kaspy
```

The mount and the unit go, the guest's ACLs are dropped, the PHP denial leaves the site configuration. The files stay in the site folder. Deleting the guest account (`mp user rm`) or the site does the same by itself.

## 3. File permissions

- Everything the guest creates belongs to the guest, everything the site creates to the site owner; thanks to the inherited ACLs both sides read each other's files and nginx serves them to visitors.
- A file uploaded over SFTP with mode `0644` (the way clients usually upload) can be read and deleted by the other side but not rewritten in place: the ACL mask limits the rights to the file's group bits. For a "drop it — they pick it up" exchange that is enough. If both sides need to edit each other's files in place, `mp site fix <domain>` recalculates the masks of everything already in the folder.
- `mp site fix` knows about shared folders: after recalculating the ACLs of the whole site tree it restores the guests' entries.
- A panel-to-panel migration does not carry shared folders: the files travel with the site, and the guest gets the folder on the new server with the same command.

## 4. What not to do

- Do not change the guest's `home` by hand in `/etc/passwd` — see the top of this page.
- Do not hand out a folder without `--no-php` to someone you would not trust to write code as the site.
- Do not hand out the whole docroot: the guest would get write access to the entire site, `bitrix/` and `.settings.php` included. Someone who edits the site itself needs an account with access to the site, not a guest.
