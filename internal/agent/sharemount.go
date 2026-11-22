package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/systemd"
)

// A shared folder is a mount unit that binds a site folder onto a directory
// below the web root (checkMountUnit): <home>/<name> in the root-owned home
// of an SFTP-only guest, <www_root>/.shares/<guest>/<name> for a guest with a
// shell. systemd refuses a Where= that is a symlink when it looks, but
// mount(8) resolves the path once more afterwards and follows a symlink in
// every component: whoever may rename an entry on the way in between — the
// guest in a home of its own, at boot or whenever the unit is started — would
// have the folder, which its owner and the guest write to, bound over
// /etc/systemd/system. So
//
//   - a mount point lies only below root's directories that nobody else may
//     write to, reached without a symlink (checkMountPoint): the agent checks
//     the way when it writes such a unit and whenever it starts, restarts,
//     reloads or enables one;
//   - while such a unit exists, no call of the agent hands a directory on the
//     way to its mount point — the mount point included — to anybody else,
//     lets others write to it, removes it or puts something else in its
//     place (shareGuard): systemd mounts the unit at every boot without
//     asking the agent.

// sharesDirName is the directory of the web root the shared folders of
// accounts with a shell are mounted in (the API's sharesDir): a login never
// begins with a dot, so no home is called that.
const sharesDirName = ".shares"

// mountStartActions are the service actions after which systemd mounts a
// unit, now or at boot.
var mountStartActions = map[string]bool{"start": true, "restart": true, "reload": true, "reload-or-restart": true, "enable": true}

// unescapeUnitPath turns the name of a path unit, without its suffix, back
// into the path, as systemd-escape --unescape --path does: "-" is "/", \xHH
// the byte HH. ok is false for a name systemd would not give a clean path.
func unescapeUnitPath(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if name == "-" {
		return "/", true
	}
	var b strings.Builder
	b.WriteByte('/')
	for i := 0; i < len(name); i++ {
		switch c := name[i]; c {
		case '-':
			b.WriteByte('/')
		case '\\':
			if i+3 >= len(name) || name[i+1] != 'x' {
				return "", false
			}
			v, err := strconv.ParseUint(name[i+2:i+4], 16, 8)
			if err != nil {
				return "", false
			}
			b.WriteByte(byte(v))
			i += 3
		default:
			b.WriteByte(c)
		}
	}
	p := b.String()
	return p, filepath.Clean(p) == p
}

// isShareMountUnit tells whether unit (a full name) is a mount unit of a
// path below the web root root, and which path when its name can be read.
func isShareMountUnit(unit, root string) (where string, ours, ok bool) {
	name, isMount := strings.CutSuffix(unit, ".mount")
	if !isMount || root == "/" || !strings.HasPrefix(unit, strings.TrimSuffix(mountUnitName(root), ".mount")+"-") {
		return "", false, false
	}
	where, ok = unescapeUnitPath(name)
	return where, true, ok && below(where, root)
}

// mountedOn reports whether the directory fd is the root of a mount:
// something is mounted on it already (statx, Linux 5.8). Tests, which cannot
// mount, replace it.
var mountedOn = func(fd int) bool {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_TYPE, &stx); err != nil {
		return false
	}
	return stx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 && stx.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0
}

// mountOwner is the owner the mount point checks see for the directory dir:
// its own. Tests, which cannot give a directory to another account, stand a
// guest's ownership in here.
var mountOwner = func(dir string, st *unix.Stat_t) uint32 { return st.Uid }

// checkMountPoint tells why where must not be mounted over: every directory
// from the web root down to it, where included, must be root's, writable by
// nobody else (dirTrust, an access ACL's mask read as what it is) and reached
// without a symlink. A where something is mounted on already (the share is
// applied again) shows the mounted folder: then only the way to it counts.
func (s *Server) checkMountPoint(where string) error {
	root := filepath.Clean(s.cfg.WWWRoot)
	p := filepath.Clean(where)
	rel, ok := strings.CutPrefix(p, root+"/")
	if !ok || !filepath.IsAbs(where) {
		return fmt.Errorf("%s is not below the web root %s", where, root)
	}
	t := s.trust()
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() { unix.Close(fd) }() //nolint:errcheck // a directory opened for reading
	if err := mountPathDir(t, root, fd, true); err != nil {
		return err
	}
	parts := strings.Split(rel, "/")
	cur := root
	for i, name := range parts {
		cur = filepath.Join(cur, name)
		next, err := unix.Openat(fd, name, dirFlags, 0)
		switch {
		case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
			return fmt.Errorf("%w: %s is a symlink or no directory", errSymlinkInPath, cur)
		case errors.Is(err, unix.ENOENT):
			return fmt.Errorf("%s does not exist: the panel makes the mount point before the unit", cur)
		case err != nil:
			return &os.PathError{Op: "open", Path: cur, Err: err}
		}
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		fd = next
		last := i == len(parts)-1
		if last && mountedOn(fd) {
			return nil
		}
		if err := mountPathDir(t, cur, fd, !last); err != nil {
			return err
		}
	}
	return nil
}

// mountPathDir tells why the directory fd, which dir names, cannot lead to a
// mount point: it must be root's and writable by nobody else — on the way
// (latent) not even through an ACL entry the mask holds back now, which
// writes again after the next setfacl recalculates the mask (a guest with a
// shell may have given itself one on its own home before it became
// SFTP-only). The mount point itself only needs the mode: whoever may write
// into it cannot move it, and a restored one carries the folder's entries.
func mountPathDir(t *dirTrust, dir string, fd int, latent bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: dir, Err: err}
	}
	st.Uid = mountOwner(dir, &st)
	if !t.rootOwned(st.Uid) {
		return fmt.Errorf("%w: %s belongs to uid %d, not to root", errUntrusted, dir, st.Uid)
	}
	acl := hasAccessACL(fd)
	if err := t.check(dir, &st, acl); err != nil {
		return err
	}
	if latent && acl && aclLetsWrite(fd) {
		return fmt.Errorf("%w: %s carries ACL entries that let others write to it once the mask is recalculated (setfacl -b %s clears them)", errLatentWrite, dir, dir)
	}
	return nil
}

// errLatentWrite marks a directory on the way that nobody else may write to
// now, but that carries an ACL entry the mask holds back: refused for a new
// mount, yet no reason to take an existing one away (only setfacl as root
// would bring the entry back, and the agent refuses that on the way).
var errLatentWrite = fmt.Errorf("%w (an ACL entry held back by the mask)", errUntrusted)

// mountPointErr is the refusal of a mount point checkMountPoint turned down.
func (s *Server) mountPointErr(where string, err error) *Error {
	return &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("the mount point %s is refused: %v. A shared folder is mounted only below directories of root's that nobody else may write to — %s/<login>/<name> for an account with a shell, the root-owned home of an SFTP-only account; apply the shared folder again (mp site fix <its site>, or a restart of the panel) to move it there",
		where, err, filepath.Join(s.cfg.WWWRoot, sharesDirName))}
}

// vetMountStart checks the mount point of a mount unit below the web root
// before the agent has systemd mount it, now (start, restart, reload) or at
// boot (enable); unit is a full name.
func (s *Server) vetMountStart(unit, action string) error {
	if !mountStartActions[action] {
		return nil
	}
	where, ours, ok := isShareMountUnit(unit, filepath.Clean(s.cfg.WWWRoot))
	switch {
	case !ours:
		return nil
	case !ok:
		return notAllowed(action+" of "+unit, "it names no clean path below "+s.cfg.WWWRoot+": its mount point cannot be checked")
	}
	if err := s.checkMountPoint(where); err != nil {
		return s.mountPointErr(where, err)
	}
	return nil
}

// vetMountReloads checks the mount units among what config/apply and
// paths/remove reload or restart (a restart starts a unit that is stopped).
func (s *Server) vetMountReloads(units ...[]string) error {
	for _, list := range units {
		for _, u := range list {
			if err := s.vetMountStart(fullUnit(u), "restart"); err != nil {
				return err
			}
		}
	}
	return nil
}

// shareUnitWait bounds how long the write of a shared folder's unit waits for
// the calls under a guard: an extraction, a restore or a recursive chown below
// the web root holds one for as long as it runs. A variable for tests.
var shareUnitWait = 30 * time.Second

// lockShareUnits takes shareMu for writing a shared folder's unit without
// holding back the calls that take it for reading meanwhile: a waiting Lock
// would make every new one queue behind it, all of dirs/ensure for as long as
// an import's extraction runs. It tries again for a while instead and then
// gives up.
func (s *Server) lockShareUnits(ctx context.Context) (func(), error) {
	deadline := time.Now().Add(shareUnitWait)
	for !s.shareMu.TryLock() {
		if time.Now().After(deadline) {
			return nil, &Error{Status: http.StatusServiceUnavailable, Message: "a shared folder's unit cannot be written now: an extraction or a restore below the web root is under way; try again when it is done"}
		}
		select {
		case <-ctx.Done():
			return nil, &Error{Message: "waiting to write a shared folder's unit", Output: ctx.Err().Error()}
		case <-time.After(20 * time.Millisecond):
		}
	}
	return s.shareMu.Unlock, nil
}

// reloadSystemd has systemd read its unit files again (daemon-reload).
// Tests, which have no systemd to talk to, replace it.
var reloadSystemd = func(ctx context.Context) error {
	conn, err := systemd.Connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.DaemonReload(ctx)
}

// removeShareUnit removes the unit of a shared folder (/v1/units/remove) and
// has systemd forget it before anything may change the way to its mount
// point again. systemd keeps a unit whose file is gone as it was loaded, and
// starts it when something that needs its mount point starts — the
// WorkingDirectory= of an app, the RequiresMountsFor= of another folder —
// while shareGuard, which goes by the files, would no longer keep that way
// root's: the home handed to its account, a rename by the account between
// systemd's check and mount(8) would bind the folder over
// /etc/systemd/system. So the file goes and systemd reloads while shareMu is
// held for writing, and when the reload fails the file is put back. A folder
// still mounted stays mounted (stopping it is the caller's): systemd then
// knows the mount, no longer a unit that could mount it again.
func (s *Server) removeShareUnit(ctx context.Context, name, p string) (*RemoveUnitResponse, error) {
	unlock, err := s.lockShareUnits(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	w, err := walkDir("/", filepath.Dir(p), walkOpts{trust: s.trust(), follow: s.unitFollow(), uid: -1, gid: -1})
	if errors.Is(err, os.ErrNotExist) {
		return &RemoveUnitResponse{Path: p}, nil
	}
	if err != nil {
		return nil, walkErr("remove "+p, err)
	}
	defer w.close()
	base := filepath.Base(p)
	var st unix.Stat_t
	switch err := unix.Fstatat(w.fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case errors.Is(err, unix.ENOENT):
		return &RemoveUnitResponse{Path: p}, nil
	case err != nil:
		return nil, &Error{Message: "stat " + p, Output: err.Error()}
	}
	// What is there now is what goes back should systemd not reload.
	var restore func() error
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		old, fst, _, err := readFileAt(w.fd, base)
		if err != nil {
			return nil, walkErr("read "+p, err)
		}
		restore = func() error {
			return writeFileAt(w.fd, w.resolved, base, old, fst.Mode&0o777, int(fst.Uid), int(fst.Gid))
		}
	case unix.S_IFLNK:
		target, err := readlinkat(w.fd, base)
		if err != nil {
			return nil, &Error{Message: "readlink " + p, Output: err.Error()}
		}
		restore = func() error { return unix.Symlinkat(target, w.fd, base) }
	default:
		return nil, &Error{Status: http.StatusBadRequest, Message: p + " is no unit file"}
	}
	if err := unix.Unlinkat(w.fd, base, 0); err != nil {
		return nil, &Error{Message: "remove " + p, Output: err.Error()}
	}
	if err := reloadSystemd(ctx); err != nil {
		if rerr := restore(); rerr != nil {
			s.log.Error("a shared folder's unit is gone while systemd may still mount it: run systemctl daemon-reload", "unit", name, "reload", err, "restore", rerr)
			return nil, &Error{Message: "removing " + name + ": systemd did not reload and the unit could not be put back; run systemctl daemon-reload", Output: errors.Join(err, rerr).Error()}
		}
		return nil, &Error{Message: "removing " + name + ": systemd did not reload, so the unit is back (systemd would still mount it, and the way to its mount point stays root's while it does)", Output: err.Error()}
	}
	s.log.Info("unit removed", "unit", name)
	return &RemoveUnitResponse{Path: p, Removed: true}, nil
}

// disableUnit drops a unit's [Install] links (systemctl disable). Tests,
// which have no systemd to talk to, replace it.
var disableUnit = func(ctx context.Context, unit string) error {
	conn, err := systemd.Connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Disable(ctx, unit)
}

// retireUnsafeShareUnits takes away, as the agent starts, the units of
// shared folders whose mount point the agent refuses: those a panel up to
// 0.8.19 put in the home of an account with a shell, which the account owns.
// systemd mounts such a unit at boot and also whenever something that needs
// it starts — an app whose WorkingDirectory= lies in the folder, a folder
// whose RequiresMountsFor= does, a unit that wants multi-user.target — and
// the agent's look at /v1/service sees none of these, while the account may
// rename what lies on the way between systemd's check and mount(8). So the
// unit is disabled and its file goes (removeShareUnit: systemd forgets it);
// a folder mounted now stays mounted, and the API moves it to
// <www_root>/.shares at its start (refreshShareMounts), which comes after the
// agent's. Only a unit in the panel's own form (CheckUnit) whose way someone
// else owns, may write to now or has a symlink in it goes: a directory that
// cannot be read, one missing, or an ACL entry the mask holds back leaves it
// be.
func (s *Server) retireUnsafeShareUnits(ctx context.Context) {
	root := filepath.Clean(s.cfg.WWWRoot)
	dir := s.unitDirectory()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	h := s.unitHost()
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		where, _, ok := isShareMountUnit(name, root)
		if !ok {
			continue
		}
		why := s.checkMountPoint(where)
		if why == nil || errors.Is(why, errLatentWrite) || !(errors.Is(why, errUntrusted) || errors.Is(why, errSymlinkInPath)) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if err := CheckUnit(name, content, h); err != nil {
			s.log.Warn("a mount unit below the web root that is not one of the panel's has an unsafe mount point: systemd may mount it elsewhere at boot; the agent neither starts it nor touches the way to it", "unit", name, "mount_point", where, "why", why)
			continue
		}
		if err := disableUnit(ctx, name); err != nil {
			s.log.Warn("shared folder unit not disabled", "unit", name, "err", err)
		}
		if _, err := s.removeShareUnit(ctx, name, filepath.Join(dir, name)); err != nil {
			s.log.Error("shared folder unit with an unsafe mount point not removed: systemd may mount it there again", "unit", name, "mount_point", where, "why", why, "err", err)
			continue
		}
		s.log.Warn("shared folder unit removed: its mount point lies below a directory others may rearrange; the panel mounts the folder in "+filepath.Join(root, sharesDirName)+" at its start", "unit", name, "mount_point", where, "why", why)
	}
}

// shareGuard is what the agent knows about the shared folders while a call
// changes something below the web root: the mount points of the mount units
// in its unit directory.
type shareGuard struct {
	root   string
	trust  *dirTrust
	mounts []string
}

// holdShareGuard reads the mount points of the shared folders and holds them
// until release: no unit of a shared folder is written in between
// (applyUnit, lockShareUnits), so none can appear below a directory the call
// is changing.
func (s *Server) holdShareGuard() (*shareGuard, func(), error) {
	s.shareMu.RLock()
	g, err := s.loadShareGuard()
	if err != nil {
		s.shareMu.RUnlock()
		return nil, func() {}, err
	}
	var once sync.Once
	return g, func() { once.Do(s.shareMu.RUnlock) }, nil
}

// loadShareGuard lists the mount points: the Where of every *.mount unit of
// the unit directory below the web root, read from its name, which systemd
// holds to the path. A directory that cannot be read refuses the call: the
// agent would not know what it may change.
func (s *Server) loadShareGuard() (*shareGuard, error) {
	g := &shareGuard{root: filepath.Clean(s.cfg.WWWRoot), trust: s.trust()}
	entries, err := os.ReadDir(s.unitDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return nil, &Error{Message: "reading the mount units of the shared folders", Output: err.Error()}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if where, _, ok := isShareMountUnit(e.Name(), g.root); ok {
			g.mounts = append(g.mounts, where)
		}
	}
	return g, nil
}

// holding returns a mount point at or below p: p is one, or a directory on
// the way to one.
func (g *shareGuard) holding(p string) (string, bool) {
	if g == nil {
		return "", false
	}
	p = filepath.Clean(p)
	for _, m := range g.mounts {
		if atOrBelow(m, p) {
			return m, true
		}
	}
	return "", false
}

// below returns a mount point strictly below p.
func (g *shareGuard) below(p string) (string, bool) {
	if g == nil {
		return "", false
	}
	p = filepath.Clean(p)
	for _, m := range g.mounts {
		if below(m, p) {
			return m, true
		}
	}
	return "", false
}

// isMount tells whether p is a mount point itself.
func (g *shareGuard) isMount(p string) bool {
	if g == nil {
		return false
	}
	p = filepath.Clean(p)
	for _, m := range g.mounts {
		if m == p {
			return true
		}
	}
	return false
}

// guestOf names the account a mount point is the shared folder of:
// <root>/<login>/… or <root>/.shares/<login>/….
func (g *shareGuard) guestOf(mount string) string {
	parts := strings.Split(strings.TrimPrefix(mount, g.root+"/"), "/")
	if parts[0] == sharesDirName && len(parts) > 1 {
		return parts[1]
	}
	return parts[0]
}

// refuse is the answer to a call that would change what lies on the way to
// the mount point mount, or the mount point itself.
func (g *shareGuard) refuse(what, mount string) *Error {
	return &Error{Status: http.StatusConflict, Message: fmt.Sprintf("%s is not allowed: the shared folder mounted at %s lies there, and the way to a mount point stays root's alone while its unit exists; remove or move the shared folders of %s first",
		what, mount, g.guestOf(mount))}
}

// dirChange vets what ensureHomeDir is about to make of the directory dir:
// on the way to a mount point it stays root's and writable by nobody else, as
// checkMountPoint wants it. cur is how it is now (nil for a directory about to
// be made, then root's); a change that leaves it as it is passes, so that a
// home handed to its account before the panel checked mount points (up to
// 0.8.19) can still be set up while the folders are moved out of it.
func (g *shareGuard) dirChange(dir string, cur *unix.Stat_t, acl bool, uid, gid int, mode uint32) error {
	m, ok := g.holding(dir)
	if !ok {
		return nil
	}
	next := unix.Stat_t{Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid()), Mode: mode & 0o7777}
	if cur != nil {
		next.Uid, next.Gid = cur.Uid, cur.Gid
	}
	if uid >= 0 {
		next.Uid = uint32(uid)
	}
	if gid >= 0 {
		next.Gid = uint32(gid)
	}
	if cur != nil && next.Uid == cur.Uid && next.Gid == cur.Gid && next.Mode == cur.Mode&0o7777 {
		return nil
	}
	if g.trust.rootOwned(next.Uid) && g.trust.check(dir, &next, acl) == nil {
		return nil
	}
	return g.refuse(fmt.Sprintf("making %s uid %d gid %d mode %#o", dir, next.Uid, next.Gid, next.Mode), m)
}

// aclEntryWrites tells an entry of acl/set that gives write: u:alex:rwx,
// d:g::rw-, m::rwx, o::-w-.
func aclEntryWrites(e string) bool {
	i := strings.LastIndexByte(e, ':')
	return i >= 0 && strings.Contains(e[i+1:], "w")
}

// aclStaysClosed refuses an ACL change of the directory top (p names it) on
// the way to the mount point m when an entry there writes already and only
// the mask holds it back: setfacl recalculates the mask, and the entry would
// write again. A directory that is not root's (a home handed to its account
// up to 0.8.19) is its owner's anyway: the change gives nobody more.
func (g *shareGuard) aclStaysClosed(top *treeEntry, p, m string) error {
	fd, err := unix.Openat(top.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &Error{Message: "open " + p, Output: err.Error()}
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &Error{Message: "stat " + p, Output: err.Error()}
	}
	if g.trust.rootOwned(mountOwner(p, &st)) && aclLetsWrite(fd) {
		return g.refuse("changing the ACL of "+p+", whose entries let others write (setfacl -b clears them)", m)
	}
	return nil
}

// The POSIX ACL of an inode as the kernel keeps it in system.posix_acl_access
// (linux/posix_acl_xattr.h): a version, then tag, permissions and id per
// entry.
const (
	aclXattrVersion = 2
	aclUser         = 0x02
	aclGroupObj     = 0x04
	aclGroup        = 0x08
	aclOther        = 0x20
	aclWrite        = 0x02
)

// aclLetsWrite reports whether the access ACL of fd gives write to a named
// user, a named group, the owning group or others: setfacl recalculates the
// mask from those on every change, so such an entry the mask holds back now
// writes again after the next one. An ACL that cannot be read counts as one.
func aclLetsWrite(fd int) bool {
	n, err := unix.Fgetxattr(fd, "system.posix_acl_access", nil)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || (err == nil && n == 0) {
		return false
	}
	if err != nil {
		return true
	}
	buf := make([]byte, n)
	if n, err = unix.Fgetxattr(fd, "system.posix_acl_access", buf); err != nil || n < 4 {
		return true
	}
	buf = buf[:n]
	if binary.LittleEndian.Uint32(buf) != aclXattrVersion || (n-4)%8 != 0 {
		return true
	}
	for off := 4; off+8 <= n; off += 8 {
		tag, perm := binary.LittleEndian.Uint16(buf[off:]), binary.LittleEndian.Uint16(buf[off+2:])
		switch tag {
		case aclUser, aclGroupObj, aclGroup, aclOther:
			if perm&aclWrite != 0 {
				return true
			}
		}
	}
	return false
}

// mountedBelow returns a mount point below dir as the kernel lists them
// (/proc/self/mountinfo): a mount whose unit is gone already, but not the
// mount itself.
func mountedBelow(dir string) (string, bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", false
	}
	defer f.Close()
	dir = filepath.Clean(dir)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		// 36 35 98:0 /mnt1 /mnt/parent rw,noatime master:1 - ext3 /dev/root rw
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if mp := unescapeMountinfo(fields[4]); below(mp, dir) {
			return mp, true
		}
	}
	return "", false
}

// unescapeMountinfo undoes the octal escapes of /proc/self/mountinfo (\040
// for a blank and the like).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// closerFunc runs a function as an io.Closer (a plan's lock released when the
// plan is done).
type closerFunc func()

func (f closerFunc) Close() error {
	f()
	return nil
}
