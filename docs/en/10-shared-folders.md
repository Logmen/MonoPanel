# 10. Shared folders: a contractor's access to one folder of a site

A common request: "give the contractor (an exchange service, the accountants, an agency) SFTP access to one folder of our site". The first idea is to create a user with its home directory inside the site. In MonoPanel that is neither needed nor possible:

- sshd chroots an SFTP user into its home and insists that the directory is owned by root and not writable by the group or others. A site folder belongs to the site owner, and if root took it, the site itself could not write there.
- The panel expects every account's home at `/var/www/<login>` with `data/www`, logs, tmp and the web server's ACLs. A foreign path in `home` sooner or later breaks `mp site fix`, backups or a migration.
- One unix user per account and a `0710` home is what keeps clients apart.

Instead the panel hands out a **shared folder**: the guest stays an ordinary account with its own home, and the site folder is mounted inside its chroot (for a guest with a shell, in `/var/www/.shares`, with a symlink in its home). After signing in the guest sees only that folder (and its own empty `data`), both sides and the web server read and write the files, and the site can be told never to execute PHP from the folder.

## 1. How it works

On `mp user share add` the panel:

1. creates the folder inside the site's docroot (if missing) — owned by the site owner, mode `2770`, setgid keeps the owner's group on new files;
2. sets ACLs on the folder: `rwx` for the guest and the owner, `r-x` for the web group, inherited by everything created inside;
3. makes a root-owned mount point. For an SFTP-only guest it lies in the guest's home (`/var/www/<guest>/<name>`): the home is root's anyway — the chroot stays root-owned, sshd is happy. A guest with a shell owns its home, so the folder is mounted in `/var/www/.shares/<guest>/<name>` (`.shares` root 0711, `.shares/<guest>` root:<guest> 0750, the mount point root 0755), and the home gets the symlink `/var/www/<guest>/<name>` pointing there;
4. writes and enables a systemd mount unit that bind-mounts the folder there — it survives a reboot;
5. with `--no-php` adds a `location` to the site configuration that answers 403 to any `.php`, `.phtml` or `.phar` in the folder; for a site in apache mode the account's Apache also reads no `.htaccess` in it (otherwise the guest could hand a file of any name to PHP or switch off the owner check of symlinks), runs no PHP from it — also for a request that goes past nginx — and follows a symlink in it only to a file of the link's own owner. A `.htaccess` in such a folder has no effect, as in php-fpm mode;
6. with `--entry` writes a `Match User <guest> Group monopanel-sftp` block with `internal-sftp -d /<name>` into the sshd drop-in: the SFTP session starts right inside the folder rather than at the chroot root. The block applies to accounts without a shell only.

Why not in the home of a guest with a shell: systemd checks the unit's `Where=`, and mount(8) then resolves the path once more, following symlinks. Whoever may rename an entry of a directory on that way (and a guest may in its own home) could swap it in between and have the folder bound over `/etc/systemd/system`. So the agent writes and starts a shared folder's unit only when every directory from the web root down to the mount point is root's, not writable by others (the ACL mask and the entries it holds back now counted) and no symlink; any other mount point it refuses (403). While the unit exists, no operation of the agent hands a directory on that way to an account, makes it writable, removes it or puts something else in its place: a home that holds a mount point is not handed to its account (409 "remove or move the shared folders of <login> first").

There is no way out of the chroot of an SFTP-only guest: symlinks pointing outside do not resolve inside the chroot, `../` stops at the guest's home. A guest with a shell moves around the server with its account's rights anyway; to it the folder is one more directory, behind a symlink in its home. On EL the site's SELinux labels are kept; nothing extra to set up.

Switching a guest between a shell and SFTP-only moves its folders within the same job (`user.provision`): out of the home before the home becomes the account's, back into it after it is root's again.

A folder in use (a session with its working directory inside) cannot be unmounted. For an SFTP-only guest such a folder blocks the removal of the folder, of the site or of the guest, and the switch to a shell: end the guest's sessions (`loginctl terminate-user <guest>`) and try again. When the folder of a guest being switched to SFTP-only is in use, the switch goes through and the folder stays in `.shares`: a warning in the job's log, the error of the last apply on the folder (in the list of folders; `status: failed` in the JSON). For a guest with a shell whose folder still sits in its home (where a panel before 0.8.20 left it), the unit is removed even while the folder is in use — so that systemd never mounts it in the home again at boot —, the folder gets the same error, and the move finishes once the folder is free. Either way the panel tries again at its next start or with `mp site fix <domain>` (the domain of the folder's site); the empty old mount point in the home gives way to the symlink.

Upgrading from 0.8.19: as it starts, the agent removes the units an older panel left in the home of an account with a shell (a folder mounted at that moment stays mounted), and the panel, at its own start, mounts those folders in `/var/www/.shares/<guest>/<name>` and puts the symlink in the home. The panel also applies again, at every start, a shared folder whose unit is missing or whose last apply failed.

## 2. Step by step

Example: the site `vstrade.kz` of the account `bitrix`; the contractor needs the folder `kaspy` for file exchange.

**Step 1. The guest.** If the contractor has no account yet, create one without a shell — SFTP is enough:

```bash
mp user add userkaspy --generate        # the password is printed once
```

The password is for SFTP (and for the panel, where the guest sees only its own folders).

**Step 2. The folder.** Hand out the site folder. The path is relative to the site's docroot; the folder is created if missing:

```bash
mp user share add userkaspy --site vstrade.kz --path kaspy --no-php --entry
```

Keep `--no-php` whenever the folder is for files rather than code: without it the contractor could drop a `.php` there and the site would execute it as the `bitrix` account — with all the site's rights. `--name` sets the folder name in the guest's home when it should differ from the last path segment.

`--entry` makes the folder the entry point: after connecting the contractor lands straight in `kaspy`, as if it were the home directory. Without the flag the session starts in the home, where `data` and `kaspy` are visible. The entry point can be set later, and taken back:

```bash
mp user share entry userkaspy kaspy        # sign in straight into kaspy
mp user share entry userkaspy kaspy --off  # in the home again
```

An account has one entry point: setting a new one clears the previous. An account with a shell gets none — it goes wherever it likes anyway.

**Step 3. Check.**

```bash
mp user share list userkaspy
```

The `MOUNTED` column should say `yes`, `PHP` — `denied`, `ENTRY` — `yes`. `IN HOME` is the folder's path in the guest's home (a symlink for a guest with a shell); where it is really mounted, `--json` gives as `mount_point`. The contractor connects over SFTP as `userkaspy` with the password from step 1 and lands straight in the folder `kaspy` — that is the site's `/var/www/bitrix/data/www/vstrade.kz/kaspy`. One level up is the home with an empty `data`; there is no way further.

The same in the web UI: Users → the "folders" button on the account → site, folder, the "no PHP" and "entry point" boxes → Add. The "entry point" button in the table sets and clears it on a folder already handed out.

**Step 4. When the access is no longer needed.**

```bash
mp user share rm userkaspy kaspy
```

The mount and the unit go, the guest's ACLs are dropped, the PHP denial leaves the site configuration and the entry point leaves the sshd drop-in; for a guest with a shell the symlink in its home goes too, and so does its directory in `.shares` once no folder is left in it. The files stay in the site folder. Deleting the guest account (`mp user rm`) or the site does the same by itself.

## 3. File permissions

- Everything the guest creates belongs to the guest, everything the site creates to the site owner; thanks to the inherited ACLs both sides read each other's files and nginx serves them to visitors.
- A file uploaded over SFTP with mode `0644` (the way clients usually upload) can be read and deleted by the other side but not rewritten in place: the ACL mask limits the rights to the file's group bits. For a "drop it — they pick it up" exchange that is enough. If both sides need to edit each other's files in place, `mp site fix <domain>` recalculates the masks of everything already in the folder.
- `mp site fix` knows about shared folders: after recalculating the ACLs of the whole site tree it restores the guests' entries. It no longer resets the folder's owner and mode through its mounted mount point: before 0.8.20 it left the folder root:root 0755, and neither the guest nor the owner could write to it.
- A panel-to-panel migration does not carry shared folders: the files travel with the site, and the guest gets the folder on the new server with the same command. A guest with a shell finds no symlink to the folder in its home: the migration stream leaves out a symlink that leads from the home to `/var/www/.shares` (the job's log mentions it).
- A server backup reads `/var/www` with its mount points, so a shared folder's content lands in the snapshot a second time — in the home of an SFTP-only guest or in `/var/www/.shares`. An in-place restore leaves the mount points and the way to them alone: the folder comes back with its site.

## 4. What not to do

- Do not change the guest's `home` by hand in `/etc/passwd` — see the top of this page.
- Do not hand out a folder without `--no-php` to someone you would not trust to write code as the site.
- Do not hand out the whole docroot: the guest would get write access to the entire site, `bitrix/` and `.settings.php` included. Someone who edits the site itself needs an account with access to the site, not a guest.
- Do not change the owner, mode or ACL of a guest's home or of `/var/www/.shares` by hand: while a shared folder exists the agent refuses such changes (409), and it does not start a unit whose way to the mount point is no longer root's. When the check complains about ACL entries the mask holds back, `setfacl -b <directory>` clears them.
- Do not bind-mount anything yourself (your own units, fstab) below directories that accounts own: the same race between systemd and mount(8). The agent treats your own mount units below the web root in `/etc/systemd/system` as shared folders: it does not start one whose way to the mount point is not root's, answers a chown or an ACL change over a tree that holds one with 409 (so `mp site fix` fails for that site), and warns at its start about units not in the panel's form with such a mount point.
