package agent

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Root's writes, removals, chown and chmod outside the web root go through
// the helpers here. A path is walked from / one directory at a time, each one
// opened relative to the one before it with O_NOFOLLOW, and the change itself
// is made relative to the directory reached last (openat, mkdirat, renameat,
// unlinkat, fchown, fchmod). Checking a path and then using it by name would
// let whoever may rearrange a directory on the way — the API process owns
// /var/lib/monopanel, the accounts own their homes — swap a component for a
// symlink in between and turn the write into one of /etc/cron.d or
// /root/.ssh.
//
// Under the write allow-list every directory on the way must be one nobody
// the API process can act as is able to rearrange (dirTrust), and a symlink
// is followed only when root made it in such a directory (EL's /var/mail ->
// spool/mail): nobody else could have put it there or could change it — and
// only on the way to the tree the path lies in (walkOpts.follow), never
// inside it: there root makes symlinks for the API itself, from archives it
// unpacks into the mail store and Sphinx's directory or from snapshots it
// restores, and one of those followed would carry a write, a removal or a
// chown anywhere.

// errUntrusted marks a directory on the way that someone besides root, or a
// system account of its own, could rearrange: what root does below it could
// be redirected.
var errUntrusted = errors.New("not trusted")

// errNotPlainFile marks a name that root would replace or read but that is
// something else than a plain file: a symlink, a directory, a FIFO.
var errNotPlainFile = errors.New("not a regular file")

// maxSymlinks bounds the symlinks one walk follows, as the kernel does.
const maxSymlinks = 40

// unitSFTPGroup is the group of the SFTP-only accounts (the API's sftpGroup): the
// accounts are its members, so it is one of the panel's own groups.
const unitSFTPGroup = "monopanel-sftp"

// dirTrust decides which directories root may go through on its way to a
// change. A directory is trusted when nobody the API process can act as is
// able to rearrange its entries:
//
//   - its owner is root or a system account (uid < UID_MIN) other than the
//     panel's service user — the API runs as that user, and the accounts it
//     manages are ordinary users;
//   - others may write to it only with the sticky bit: each entry then
//     belongs to whoever made it, and the next step checks that entry;
//   - a group may write to it only when that group is root's or the one the
//     distribution gives the directory (mail for the mail spool, staff for
//     /usr/local on Debian) — never one of the panel's own groups, whose
//     members are the API process and the accounts.
type dirTrust struct {
	// rootUID is root: 0, or the agent's own uid when it runs unprivileged
	// (in tests), whose directories are then as good as root's.
	rootUID int
	// serviceUser is the API process's account (config service_user).
	serviceUser string
	// strict refuses an untrusted directory. Without it (mp setup, which
	// fixes up the API's own data directory as root) such a directory may be
	// walked through, but a symlink is still followed only when root made it
	// in a directory nobody else may write to.
	strict bool
}

// groupWritable are the directories the distributions make writable by a
// group of their own: Debian's /var/mail is root:mail 2775 and EL's
// /var/spool/mail root:mail 0775 (the mail store lives below), Debian's
// /usr/local tree is root:staff 2775 (composer and wp-cli live below),
// Ubuntu's /var/log root:syslog 0775. Their groups are the distribution's
// daemons and administrators, never the panel's accounts.
var groupWritable = []struct{ dir, group string }{
	{"/var/mail", "mail"},
	{"/var/spool/mail", "mail"},
	{"/usr/local", "staff"},
	{"/var/log", "syslog"}, // Ubuntu's /var/log is root:syslog 0775, for rsyslog
}

func (t *dirTrust) rootOwned(uid uint32) bool {
	return uid == 0 || int(uid) == t.rootUID
}

// ownerTrusted: root, or a system account that is not the API's.
func (t *dirTrust) ownerTrusted(uid uint32) bool {
	if t.rootOwned(uid) {
		return true
	}
	if int(uid) >= uidMin() {
		return false
	}
	if t.serviceUser != "" {
		if svc, err := lookupUID(t.serviceUser); err == nil && svc == int(uid) {
			return false
		}
	}
	return true
}

func groupWriteAllowed(dir string, gid uint32) bool {
	if gid == 0 {
		return true
	}
	g, err := user.LookupGroupId(strconv.Itoa(int(gid)))
	if err != nil {
		return false
	}
	for _, w := range groupWritable {
		if g.Name == w.group && (dir == w.dir || strings.HasPrefix(dir, w.dir+"/")) {
			return true
		}
	}
	return false
}

// check tells why root must not go through the directory dir. acl tells
// that it carries an access ACL: the group bits of its mode are then the
// ACL's mask, and a group write bit there may be a named user's or group's
// (the API's), not the owning group's the rules vouch for.
func (t *dirTrust) check(dir string, st *unix.Stat_t, acl bool) error {
	mode := st.Mode & 0o7777
	sticky := mode&unix.S_ISVTX != 0
	switch {
	case !t.ownerTrusted(st.Uid):
		return fmt.Errorf("%w: %s belongs to uid %d, who is neither root nor a system account of its own: root does not write below it", errUntrusted, dir, st.Uid)
	case mode&0o002 != 0 && !sticky:
		return fmt.Errorf("%w: %s (mode %#o) is writable by everyone without the sticky bit", errUntrusted, dir, mode)
	case mode&0o020 != 0 && !sticky && acl:
		return fmt.Errorf("%w: %s (mode %#o) carries an ACL that may let others than its group write to it", errUntrusted, dir, mode)
	case mode&0o020 != 0 && !sticky && !groupWriteAllowed(dir, st.Gid):
		return fmt.Errorf("%w: %s (mode %#o) is writable by the group %d: its members could swap what root writes below it", errUntrusted, dir, mode, st.Gid)
	}
	return nil
}

// aloneOrSticky: root's directory nobody else may write to, or a sticky one
// of root's (in which nobody else may rename or remove root's entries).
func (t *dirTrust) aloneOrSticky(st *unix.Stat_t) bool {
	return t.rootOwned(st.Uid) && (st.Mode&0o022 == 0 || st.Mode&unix.S_ISVTX != 0)
}

// walkOpts says how a walk goes.
type walkOpts struct {
	// trust checks the directories and lets root's own symlinks be
	// followed; nil follows no symlink at all and checks no owner (the web
	// root, whose directories the accounts own).
	trust *dirTrust
	// follow is how many leading components of the path may be symlinks
	// root made (and are then resolved): the directories above the tree of
	// the allow-list the path lies in (managedDepth). Below them none is
	// followed. 0 follows none, followAll every trusted one (paths of the
	// configuration, which the API does not choose).
	follow int
	// create makes the directories that are missing with mode (the umask
	// applies, as for MkdirAll) and, unless -1, uid and gid.
	create   bool
	mode     uint32
	uid, gid int
}

// followAll lets a walk follow root's symlinks anywhere on the way.
const followAll = -1

// walkStep is a component still to go; follow tells whether a symlink root
// made may stand there.
type walkStep struct {
	name   string
	follow bool
}

func walkSteps(names []string, follow func(i int) bool) []walkStep {
	out := make([]walkStep, len(names))
	for i, n := range names {
		out[i] = walkStep{name: n, follow: follow(i)}
	}
	return out
}

// walked is where a walk ended: the directory, opened, and its path with the
// trusted symlinks resolved.
type walked struct {
	fd       int
	resolved string
	// made lists the directories the walk made, topmost first.
	made []string
}

func (w *walked) close() {
	if w != nil && w.fd >= 0 {
		unix.Close(w.fd) //nolint:errcheck // a directory opened for reading
		w.fd = -1
	}
}

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// pathParts are the names of p, without the empty and "." ones.
func pathParts(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

func joinResolved(cur []string) string { return "/" + strings.Join(cur, "/") }

// walkDir opens the directory rel below start. start is opened by its path:
// it is "/" or a setting of the panel (the web root), not a name anyone else
// chose. Every component of rel is opened relative to the directory before
// it without following a symlink.
func walkDir(start, rel string, o walkOpts) (*walked, error) {
	fd, err := unix.Open(start, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: start, Err: err}
	}
	w := &walked{fd: fd}
	cur := pathParts(filepath.Clean(start))
	if o.trust != nil {
		if err := w.checkDir(o.trust, joinResolved(cur)); err != nil {
			w.close()
			return nil, err
		}
	}
	queue := walkSteps(pathParts(rel), func(i int) bool { return o.follow == followAll || i < o.follow })
	links := 0
	for len(queue) > 0 {
		name, mayFollow := queue[0].name, queue[0].follow
		queue = queue[1:]
		if name == ".." {
			if len(cur) > 0 {
				cur = cur[:len(cur)-1]
			}
		} else {
			cur = append(cur, name)
		}
		here := joinResolved(cur)
		next, err := unix.Openat(w.fd, name, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && o.create && name != ".." {
			made := false
			switch merr := unix.Mkdirat(w.fd, name, o.mode); {
			case merr == nil:
				made = true
				w.made = append(w.made, here)
			case !errors.Is(merr, unix.EEXIST):
				w.close()
				return nil, &os.PathError{Op: "mkdir", Path: here, Err: merr}
			}
			next, err = unix.Openat(w.fd, name, dirFlags, 0)
			if err == nil && made && (o.uid >= 0 || o.gid >= 0) {
				if cerr := unix.Fchown(next, o.uid, o.gid); cerr != nil {
					unix.Close(next) //nolint:errcheck // a directory opened for reading
					w.close()
					return nil, &os.PathError{Op: "chown", Path: here, Err: cerr}
				}
			}
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			if !mayFollow {
				var st unix.Stat_t
				link := unix.Fstatat(w.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
				w.close()
				if o.trust != nil && link {
					return nil, fmt.Errorf("%w: refusing %s: inside the trees it writes the agent follows no symlink, not even root's", errSymlinkInPath, here)
				}
				return nil, fmt.Errorf("%w: refusing %s", errSymlinkInPath, here)
			}
			target, ok, lerr := w.trustedLink(o.trust, name)
			if lerr != nil {
				w.close()
				return nil, lerr
			}
			if !ok {
				w.close()
				return nil, fmt.Errorf("%w: refusing %s", errSymlinkInPath, here)
			}
			if links++; links > maxSymlinks {
				w.close()
				return nil, &os.PathError{Op: "open", Path: here, Err: unix.ELOOP}
			}
			cur = cur[:len(cur)-1]
			if strings.HasPrefix(target, "/") {
				root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
				if err != nil {
					w.close()
					return nil, &os.PathError{Op: "open", Path: "/", Err: err}
				}
				unix.Close(w.fd) //nolint:errcheck // a directory opened for reading
				w.fd, cur = root, nil
				if err := w.checkDir(o.trust, "/"); err != nil {
					w.close()
					return nil, err
				}
			}
			// The target resolves a component that may be a link: the links
			// on its own way are the system's too.
			queue = append(walkSteps(pathParts(target), func(int) bool { return true }), queue...)
			continue
		}
		if errors.Is(err, unix.ENOENT) {
			w.close()
			return nil, &os.PathError{Op: "open", Path: here, Err: os.ErrNotExist}
		}
		if err != nil {
			w.close()
			return nil, &os.PathError{Op: "open", Path: here, Err: err}
		}
		unix.Close(w.fd) //nolint:errcheck // a directory opened for reading
		w.fd = next
		if o.trust != nil {
			if err := w.checkDir(o.trust, here); err != nil {
				w.close()
				return nil, err
			}
		}
	}
	w.resolved = joinResolved(cur)
	return w, nil
}

// checkDir applies the trust rules to the directory the walk stands in.
func (w *walked) checkDir(t *dirTrust, here string) error {
	var st unix.Stat_t
	if err := unix.Fstat(w.fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: here, Err: err}
	}
	if err := t.check(here, &st, hasAccessACL(w.fd)); err != nil && t.strict {
		return err
	}
	return nil
}

// hasAccessACL reports whether the file fd carries an access ACL beyond its
// mode bits.
func hasAccessACL(fd int) bool {
	n, err := unix.Fgetxattr(fd, "system.posix_acl_access", nil)
	return err == nil && n > 0
}

// trustedLink tells whether the entry name of the current directory is a
// symlink root may follow, and where it points: one root owns, in a
// directory nobody else may write to (or a sticky one of root's) — nobody
// but root could have made it or could change it. Without trust rules no
// symlink is followed.
func (w *walked) trustedLink(t *dirTrust, name string) (string, bool, error) {
	if t == nil {
		return "", false, nil
	}
	var st unix.Stat_t
	if err := unix.Fstatat(w.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", false, nil //nolint:nilerr // vanished meanwhile: refused like a link
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK || !t.rootOwned(st.Uid) {
		return "", false, nil
	}
	var dir unix.Stat_t
	if err := unix.Fstat(w.fd, &dir); err != nil || !t.aloneOrSticky(&dir) {
		return "", false, nil //nolint:nilerr // refused like any link root does not vouch for
	}
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(w.fd, name, buf)
	if err != nil {
		return "", false, &os.PathError{Op: "readlink", Path: name, Err: err}
	}
	if n == 0 || n >= len(buf) {
		return "", false, nil
	}
	return string(buf[:n]), true, nil
}

// readFileAt reads the plain file name of the directory dir. It opens the
// name itself without following a symlink and without blocking on a FIFO
// planted there; exists is false when there is no such entry.
func readFileAt(dir int, name string) (data []byte, st unix.Stat_t, exists bool, err error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, st, false, nil
	}
	if errors.Is(err, unix.ELOOP) {
		return nil, st, true, errNotPlainFile
	}
	if err != nil {
		return nil, st, true, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, st, true, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, st, true, errNotPlainFile
	}
	data, err = io.ReadAll(f)
	return data, st, true, err
}

// writeFileAt replaces the entry name of the directory dir atomically: a
// new file made next to it (O_EXCL, never through a symlink) gets data, the
// mode and the owner, is synced, and takes the name over with renameat — an
// entry planted under the name, a symlink too, is replaced itself, never
// written through.
func writeFileAt(dir int, dirPath, name string, data []byte, mode uint32, uid, gid int) (err error) {
	tmp := fmt.Sprintf(".%s.mp-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	if len(tmp) > 255 {
		tmp = fmt.Sprintf(".mp-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &os.PathError{Op: "create", Path: filepath.Join(dirPath, tmp), Err: err}
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() {
		if err != nil {
			f.Close()
			unix.Unlinkat(dir, tmp, 0) //nolint:errcheck // best effort: the temporary file of a failed write
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = unix.Fchmod(fd, mode); err != nil {
		return &os.PathError{Op: "chmod", Path: filepath.Join(dirPath, tmp), Err: err}
	}
	if uid >= 0 || gid >= 0 {
		if err = unix.Fchown(fd, uid, gid); err != nil {
			return &os.PathError{Op: "chown", Path: filepath.Join(dirPath, tmp), Err: err}
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = unix.Renameat(dir, tmp, dir, name); err != nil {
		return &os.PathError{Op: "rename", Path: filepath.Join(dirPath, name), Err: err}
	}
	return nil
}

// setupTrust is how mp setup, as root, goes through directories: those of
// the API's data directory too (it fixes them up), but following only the
// symlinks root made in directories nobody else may write to.
func setupTrust() *dirTrust { return &dirTrust{rootUID: os.Geteuid()} }

// InstallFile writes data as path for mp setup, which runs as root and keeps
// files in a directory the API owns (the panel's certificate in its data
// directory): the directories are reached as EnsureDir reaches them, and the
// file is made next to its name and renamed over it — a symlink the API left
// under the name is replaced, never written through. The file is root's
// until the caller hands it on (EnsureDirOwner). Every directory must exist.
func InstallFile(path string, data []byte, mode os.FileMode) error {
	p := filepath.Clean(path)
	w, err := walkDir("/", filepath.Dir(p), walkOpts{trust: setupTrust(), follow: followAll, uid: -1, gid: -1})
	if err != nil {
		return err
	}
	defer w.close()
	return writeFileAt(w.fd, w.resolved, filepath.Base(p), data, uint32(mode.Perm()), -1, -1)
}

// PlainFile tells whether path is there, reached as InstallFile reaches it;
// something under the name that is no plain file — a symlink above all — is
// an error (errNotPlainFile), and so is a symlink on the way root did not
// make.
func PlainFile(path string) (bool, error) {
	p := filepath.Clean(path)
	w, err := walkDir("/", filepath.Dir(p), walkOpts{trust: setupTrust(), follow: followAll, uid: -1, gid: -1})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer w.close()
	var st unix.Stat_t
	switch err := unix.Fstatat(w.fd, filepath.Base(p), &st, unix.AT_SYMLINK_NOFOLLOW); {
	case errors.Is(err, unix.ENOENT):
		return false, nil
	case err != nil:
		return false, &os.PathError{Op: "stat", Path: p, Err: err}
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return true, fmt.Errorf("%w: %s", errNotPlainFile, p)
	}
	return true, nil
}

// removeEntryAt removes the entry name of the directory dir: a file or a
// symlink itself, a directory only with recursive and then with everything
// below it, never following a symlink (removeAt).
func removeEntryAt(dir int, name, p string, recursive bool) (bool, error) {
	var st unix.Stat_t
	err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR && !recursive {
		return false, errIsDir
	}
	return removeAt(dir, name, p)
}

// errIsDir marks a directory a removal without recursive met.
var errIsDir = errors.New("is a directory")

// loginDefsGIDMin is the first gid of ordinary groups: GID_MIN of /etc/login.defs,
// 1000 by default.
func loginDefsGIDMin() int {
	b, err := os.ReadFile("/etc/login.defs")
	if err != nil {
		return 1000
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "GID_MIN" {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1000
}

// rootTrees are the trees config/apply, paths/remove and dirs/ensure never
// touch, whatever agent.extra_write_prefixes says: systemd's unit, drop-in
// and generator directories (a unit or a generator runs anything as root —
// the panel's units go through /v1/units/* and their checks), the classic
// places whose content root runs or trusts at login (cron, sudoers, PAM,
// ld.so.preload, the account databases, root's home), and the agent's own
// state (the configuration history).
var rootTrees = []string{
	"/etc/systemd", "/run/systemd", "/lib/systemd", "/usr/lib/systemd", "/usr/local/lib/systemd",
	"/etc/cron.d", "/etc/cron.hourly", "/etc/cron.daily", "/etc/cron.weekly", "/etc/cron.monthly",
	"/etc/crontab", "/etc/anacrontab", "/var/spool/cron",
	"/etc/sudoers", "/etc/sudoers.d", "/etc/pam.d", "/etc/security",
	"/etc/ld.so.preload", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow",
	"/root",
}

// inRootTree reports whether p is one of rootTrees, lies below one, or is a
// directory above one (a removal or re-moding of /etc would reach them).
func (s *Server) inRootTree(p string) bool {
	p = filepath.Clean(p)
	trees := append([]string{filepath.Clean(s.cfg.AgentStateDir())}, rootTrees...)
	for _, t := range trees {
		if p == t || strings.HasPrefix(p, t+"/") || p == "/" || strings.HasPrefix(t, p+"/") {
			return true
		}
	}
	return false
}

// managedDepth is how many leading components of p lie above the tree of
// the write allow-list p belongs to — the deepest entry holding it: a
// directory entry ("/etc/nginx/monopanel/") is that directory, an exact file
// entry ("/etc/nginx/nginx.conf") its directory — or above one of roots (the
// run directory, the web root itself). A walk to p may resolve root's
// symlinks there (EL's /var/mail -> spool/mail above /var/mail/monopanel)
// and none below: inside, root makes symlinks for the API itself (archives
// unpacked into the mail store or Sphinx's directory, restored snapshots).
// 0 when nothing holds p: no symlink is followed at all then.
func (s *Server) managedDepth(p string, roots ...string) int {
	p = filepath.Clean(p)
	best := 0
	holds := func(root string) {
		root = filepath.Clean(root)
		if n := len(pathParts(root)); (p == root || strings.HasPrefix(p, root+"/")) && n > best {
			best = n
		}
	}
	for _, pre := range s.cfg.WritePrefixes() {
		switch {
		case strings.HasSuffix(pre, "/"):
			holds(pre)
		case p == filepath.Clean(pre):
			holds(filepath.Dir(p))
		}
	}
	for _, r := range roots {
		holds(r)
	}
	return best
}

// rootTreeErr is the refusal for a path in one of them.
func rootTreeErr(p string) *Error {
	return &Error{Status: http.StatusForbidden, Message: "path not allowed: " + p + " is in a tree only root manages (systemd units, cron, sudoers, PAM, the account databases, root's home, the agent's own state); units go through /v1/units/apply"}
}
