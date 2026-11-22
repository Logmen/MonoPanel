package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The agent binds a shared folder itself: the folder it opened, onto the
// mount point it opened, in the host's mount namespace. Up to 0.8.20 a
// systemd mount unit did, What= <the folder> Where= <the mount point>, and
// systemd — at every boot, and whenever the agent started the unit — had
// mount(8) resolve What= by its path as root. The folder lies in a tree its
// owner controls: an owner who swaps it, or a directory above it, for a
// symlink has the guest's chroot show whatever the link points to —
// another account's home behind its 0710, RestoreDir, the staging of an
// in-place restore whose snapshot the API composed. A look at the path
// before the mount does not help: the owner swaps it in between, and at boot
// nobody looks. So
//
//   - shareBind opens the folder from the web root one directory at a time
//     without following a symlink, and checks it is a directory of the
//     owner's in the owner's home (openShareSource); it opens the mount point
//     the way checkMountPoint walks it, below root's directories alone
//     (openMountPoint); and it mounts a clone of the very directory it
//     opened on the very directory it opened (open_tree(OPEN_TREE_CLONE),
//     move_mount). Whatever is renamed afterwards, the guest sees the folder
//     that was checked.
//   - the agent's own mount namespace is a slave copy of the host's
//     (ProtectHome=, PrivateTmp= in its unit): a mount made there reaches
//     neither sshd's chroots nor the guests' shells. The bind is made by a
//     thread of its own that joined PID 1's mount namespace
//     (inHostMountNS) and opens both paths there itself — open_tree does not
//     clone a directory of another namespace anyway — and so is the unmount
//     (shareUnbind).
//   - nothing mounts a shared folder at boot. The agent keeps a record of
//     every folder it bound (shareRecordDir): the guard keeps the way to the
//     mount point root's by it (loadShareGuard), and the API binds the
//     folders again through the agent at its start (refreshShareMounts) —
//     and unbinds those no share of its database names any more
//     (listShareRecords).
//     The units of the panels up to 0.8.20 are taken over as the agent
//     starts (retireUnsafeShareUnits): recorded, disabled and removed, and what
//     they mounted checked against the folder.

// shareRecordSuffix ends the name of the agent's record of a shared folder:
// its mount point escaped as systemd escapes a path (mountUnitName), so that
// the guard reads the mount points from the names, as it reads them from the
// units.
const shareRecordSuffix = ".share"

// shareRecord is what the agent keeps of a shared folder it bound: the
// folder, its mount point and, once the bind is made, the directory it
// bound (device and inode). A mount of the agent's own stays what it bound,
// whatever is renamed afterwards; one without them — a unit's of an older
// panel, taken over — is checked by the folder's path (verifyShareMounts).
type shareRecord struct {
	What  string `json:"what"`
	Where string `json:"where"`
	Dev   uint64 `json:"dev,omitempty"`
	Ino   uint64 `json:"ino,omitempty"`
}

// shareRecordDir holds a record per shared folder the agent bound, root's
// alone in the agent's own directory.
func (s *Server) shareRecordDir() string {
	return filepath.Join(s.cfg.AgentStateDir(), "shares")
}

func shareRecordName(where string) string {
	return strings.TrimSuffix(mountUnitName(where), ".mount") + shareRecordSuffix
}

// recordedMountPoints are the mount points the records name, below root.
func (s *Server) recordedMountPoints(root string) ([]string, error) {
	entries, err := os.ReadDir(s.shareRecordDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), shareRecordSuffix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if where, ok := unescapeUnitPath(name); ok && below(where, root) {
			out = append(out, where)
		}
	}
	return out, nil
}

// shareRecords reads the records; one whose content does not name the mount
// point of its name is left out (and still guards that mount point).
func (s *Server) shareRecords() ([]shareRecord, error) {
	return s.readShareRecords(func(where string, err error) {
		s.log.Warn("a record of a shared folder that cannot be read: the way to its mount point stays root's, and the panel binds the folder again at its start", "dir", s.shareRecordDir(), "mount_point", where, "err", err)
	})
}

// readShareRecords is shareRecords that tells unreadable of each record it
// leaves out, instead of the log.
func (s *Server) readShareRecords(unreadable func(where string, err error)) ([]shareRecord, error) {
	dir := s.shareRecordDir()
	points, err := s.recordedMountPoints(filepath.Clean(s.cfg.WWWRoot))
	if err != nil || len(points) == 0 {
		return nil, err
	}
	fd, err := openDirTrusted(dir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
	var out []shareRecord
	for _, where := range points {
		data, _, _, err := readFileAt(fd, shareRecordName(where))
		var rec shareRecord
		if err != nil || json.Unmarshal(data, &rec) != nil || rec.Where != where {
			unreadable(where, err)
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// shareRecordAt is the agent's record of the shared folder at where; nil
// without one, or one that cannot be read.
func (s *Server) shareRecordAt(where string) *shareRecord {
	recs, err := s.readShareRecords(func(string, error) {})
	if err != nil {
		return nil
	}
	for _, r := range recs {
		if r.Where == where {
			return &r
		}
	}
	return nil
}

// listShareRecords lists the agent's records of the shared folders
// (/v1/shares/records), each with whether something is mounted at its mount
// point in the host's mount namespace. A record guards the way to its mount
// point for as long as it stays (shareGuard), and only an unbind removes it:
// the API unbinds, at its start, each one whose mount point no share of its
// database names any more — one a restore of the database lost, say — which
// nothing in the panel would otherwise ever remove.
func (s *Server) listShareRecords(_ context.Context, _ *struct{}) (*ShareRecordsResponse, error) {
	resp := &ShareRecordsResponse{Records: []ShareRecordState{}}
	recs, err := s.readShareRecords(func(where string, _ error) {
		resp.Records = append(resp.Records, ShareRecordState{Where: where})
	})
	if err != nil {
		return nil, &Error{Message: "reading the agent's records of the shared folders", Output: err.Error()}
	}
	for _, r := range recs {
		resp.Records = append(resp.Records, ShareRecordState{Where: r.Where, What: r.What})
	}
	slices.SortFunc(resp.Records, func(a, b ShareRecordState) int { return strings.Compare(a.Where, b.Where) })
	err = inHostMountNS(func() error {
		for i := range resp.Records {
			if fd, err := s.openShareMount(resp.Records[i].Where); err == nil {
				resp.Records[i].Mounted = mountedOn(fd)
				unix.Close(fd) //nolint:errcheck // a directory opened for a look
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// writeShareRecord records a shared folder before the agent binds it —
// should the agent stop between the bind and the record, the guard would not
// know the mount point — and again with the directory bound once it is.
func (s *Server) writeShareRecord(rec shareRecord) error {
	dir := s.shareRecordDir()
	fd, err := ensureRootDirAs(dir, 0o700, s.rootUID)
	if err != nil {
		return &Error{Message: "the agent's records of the shared folders, " + dir, Output: err.Error()}
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for writing into
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeFileAt(fd, dir, shareRecordName(rec.Where), append(data, '\n'), 0o600, -1, -1); err != nil {
		return &Error{Message: "recording the shared folder at " + rec.Where, Output: err.Error()}
	}
	return nil
}

// removeShareRecord forgets the shared folder at where.
func (s *Server) removeShareRecord(where string) error {
	fd, err := openDirTrusted(s.shareRecordDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for writing into
	if err := unix.Unlinkat(fd, shareRecordName(where), 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "remove", Path: filepath.Join(s.shareRecordDir(), shareRecordName(where)), Err: err}
	}
	return nil
}

// hostMountNS is the mount namespace the shared folders are bound in: PID
// 1's, where systemd, sshd and every service without a namespace of its own
// see them. Tests point it at a namespace of their own.
var hostMountNS = "/proc/1/ns/mnt"

// onMainThread tells whether the calling thread is the process's main one.
// Tests replace it.
var onMainThread = func() bool { return unix.Gettid() == unix.Getpid() }

// inHostMountNS runs fn on a thread of its own that has joined hostMountNS.
// A thread joins a mount namespace only with file system attributes (root,
// working directory, umask) of its own — Go's threads share theirs, so the
// thread unshares them first — and it stays locked to fn's goroutine and
// ends with it: no other code ever runs in that namespace. fn makes no
// goroutine of its own: one would run on another thread, in the agent's own
// namespace. Tests replace it.
//
// Never the process's main thread, though: Go does not end that one when a
// goroutine locked to it ends, it parks it for good in the state the
// goroutine left — the host's namespace, which /proc/<pid>/ns/mnt,
// /proc/self/mountinfo (mountedBelow) and nsenter -t <pid> would show for
// the whole agent from then on. Such a goroutine hands the work to another
// one, and holds the main thread meanwhile, so that the other cannot run
// there.
var inHostMountNS = func(fn func() error) error {
	errc := make(chan error, 1)
	var run func()
	run = func() {
		// never unlocked, but on the main thread: the runtime ends the
		// thread with the goroutine
		runtime.LockOSThread()
		if onMainThread() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				run()
			}()
			<-done
			runtime.UnlockOSThread()
			return
		}
		errc <- func() error {
			ns, err := unix.Open(hostMountNS, unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				return &Error{Message: "the host's mount namespace " + hostMountNS, Output: err.Error()}
			}
			defer unix.Close(ns) //nolint:errcheck // a namespace joined or not
			if err := unix.Unshare(unix.CLONE_FS); err != nil {
				return &Error{Message: "a thread of its own for the host's mount namespace (unshare CLONE_FS)", Output: err.Error()}
			}
			if err := unix.Setns(ns, unix.CLONE_NEWNS); err != nil {
				return &Error{Message: "joining the host's mount namespace " + hostMountNS, Output: err.Error()}
			}
			return fn()
		}()
	}
	go run()
	return <-errc
}

// bindTree mounts a clone of the directory src — of the mount it lies on,
// from that directory down, without the mounts below it, as mount --bind
// does — on the directory dst. The clone carries the flags of src's mount
// (nosuid, nodev of the web root's file system); attached below a shared
// mount it is propagated as one mounted there is, into the namespaces of the
// services that are slaves of the host's. Tests, which cannot mount, replace
// it.
//
// The clone is made private before it is attached, so that it gets a peer
// group of its own there. A bind of a shared mount — the host's / is one —
// is that mount's peer: a mount the guest makes inside its folder (a guest
// with a shell may fusermount on a directory of its own there) would show
// inside the owner's site as well, in every namespace — before the owner's
// PHP, the web server and the agent's walks of the site, which a FUSE mount
// that never answers holds up and one without allow_other refuses.
var bindTree = func(src, dst int) error {
	tree, err := unix.OpenTree(src, "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("open_tree: %w", err)
	}
	defer unix.Close(tree) //nolint:errcheck // the clone is attached or abandoned
	if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, &unix.MountAttr{Propagation: unix.MS_PRIVATE}); err != nil {
		return fmt.Errorf("mount_setattr: %w", err)
	}
	if err := unix.MoveMount(tree, "", dst, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount: %w", err)
	}
	return nil
}

// unmountAt unmounts what is mounted on the entry name of the directory dir:
// umount2 by dir's own entry in /proc, which leads to that very directory
// whatever lies on the path to it by now, and name taken as it is, never
// followed should it be a symlink. The caller holds no descriptor of the
// mount itself — one would keep it busy — so a folder in use is EBUSY. Tests
// replace it.
var unmountAt = func(dir int, name string, flags int) error {
	return unix.Unmount("/proc/thread-self/fd/"+strconv.Itoa(dir)+"/"+name, flags|unix.UMOUNT_NOFOLLOW)
}

// detachAt takes what is mounted on the entry name of dir off at once, in
// use or not (MNT_DETACH): what it shows is not the folder, and whoever has
// something of it open keeps only that.
func detachAt(dir int, name string) error {
	return unmountAt(dir, name, unix.MNT_DETACH)
}

// maxStacked bounds the mounts the agent takes off one mount point before it
// gives up: a share is bound once, and a unit of an older panel's mounted it
// once more at most.
const maxStacked = 8

// sameDir tells whether the descriptors a and b are one directory.
func sameDir(a, b int) bool {
	var sa, sb unix.Stat_t
	return unix.Fstat(a, &sa) == nil && unix.Fstat(b, &sb) == nil && sa.Dev == sb.Dev && sa.Ino == sb.Ino
}

// shareSourceErr is the refusal of the folder what.
func shareSourceErr(what, why string) *Error {
	return &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("the shared folder %s is refused: %s. A shared folder is a directory of its site owner's in the owner's home, reached from the web root without a symlink; mp site fix <its site> makes it one again", what, why)}
}

// mountIDOf is the mount the directory fd lies on (statx STATX_MNT_ID, Linux
// 5.8): it stays the mount the descriptor was opened on, whatever is
// unmounted or detached since. Tests replace it.
var mountIDOf = func(fd int) (uint64, error) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx); err != nil {
		return 0, err
	}
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("the kernel does not tell the mount (statx STATX_MNT_ID)")
	}
	return stx.Mnt_id, nil
}

// openShareSource opens the folder a shared folder shows, as the panel lays
// it out: a directory below the home <www_root>/<login> of a panel account —
// never in .restore or .shares, which are the agent's — reached from the web
// root one directory at a time without following a symlink, the account's
// own (the API makes the folder its site owner's before it binds it), and
// not at or below the mount point of another shared folder, which shows
// another account's tree under the name. The descriptor is what gets bound:
// what the owner renames afterwards changes nothing.
//
// The directory right below the home lies on the home's mount. That is
// where the mount point of a shared folder in a home is — its owner an
// SFTP-only guest of another account's, or a guest with a shell of a panel
// older than 0.8.20 — and it leads into that account's tree, which the
// records may not tell of as the walk crosses it (one not written yet as the
// guard was read, one gone since: shareBind). Mounts deeper in the home are
// an administrator's (a volume or a dataset for a site's files) or the
// owner's own FUSE mounts, which show only what the owner may read: a folder
// on one is shared as before.
func (s *Server) openShareSource(what string, g *shareGuard) (int, error) {
	root := filepath.Clean(s.cfg.WWWRoot)
	rel, ok := strings.CutPrefix(what, root+"/")
	if !ok || !cleanAbs(what) {
		return -1, shareSourceErr(what, "it is not below the web root "+root)
	}
	parts := strings.Split(rel, "/")
	login := parts[0]
	switch {
	case strings.HasPrefix(login, "."):
		return -1, shareSourceErr(what, filepath.Join(root, login)+" is the agent's own, no home")
	case len(parts) < 2:
		return -1, shareSourceErr(what, "it is a home itself, not a folder in one")
	}
	acct, err := lookupUnitAccount(s, login)
	if err != nil {
		return -1, shareSourceErr(what, fmt.Sprintf("%s is not the home of a panel account: %v", filepath.Join(root, login), err))
	}
	if filepath.Clean(acct.Home) != filepath.Join(root, login) {
		return -1, shareSourceErr(what, fmt.Sprintf("the home of %s is %s, not %s", login, acct.Home, filepath.Join(root, login)))
	}
	if m, ok := g.covering(what); ok {
		return -1, shareSourceErr(what, "it lies at or below "+m+", the mount point of another shared folder")
	}
	fd, err := openDirTrusted(root)
	if err != nil {
		return -1, walkErr("open "+root, err)
	}
	home := filepath.Join(root, login)
	var homeMount uint64
	cur := root
	for i, name := range parts {
		cur = filepath.Join(cur, name)
		next, err := unix.Openat(fd, name, dirFlags, 0)
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		switch {
		case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
			return -1, shareSourceErr(what, cur+" is a symlink or no directory")
		case errors.Is(err, unix.ENOENT):
			return -1, shareSourceErr(what, cur+" does not exist")
		case err != nil:
			return -1, &Error{Message: "open " + cur, Output: err.Error()}
		}
		fd = next
		switch i {
		case 0:
			if homeMount, err = mountIDOf(fd); err != nil {
				unix.Close(fd) //nolint:errcheck // a directory opened for reading
				return -1, &Error{Message: "the mount of " + home, Output: err.Error()}
			}
		case 1:
			switch mnt, err := mountIDOf(fd); {
			case err != nil:
				unix.Close(fd) //nolint:errcheck // a directory opened for reading
				return -1, &Error{Message: "the mount of " + cur, Output: err.Error()}
			case mnt != homeMount:
				unix.Close(fd) //nolint:errcheck // a directory opened for reading
				return -1, shareSourceErr(what, cur+" lies on another mount than its home "+home+": the mount point of another shared folder, which shows that folder's tree")
			}
		}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return -1, &Error{Message: "stat " + what, Output: err.Error()}
	}
	if uid := mountOwner(what, &st); uid != acct.UID {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return -1, shareSourceErr(what, fmt.Sprintf("it belongs to uid %d, not to %s (uid %d), the owner of the home it lies in", uid, login, acct.UID))
	}
	return fd, nil
}

// bindShareHere binds the folder src, which what names, at the mount point
// where, opened by this thread in its namespace (inHostMountNS), and says
// whether it mounted anything: the folder mounted there already — the share
// applied again — stays. Something else mounted there, a unit of an older
// panel's that bound whatever its What= named by then, is taken off first,
// in use or not.
func (s *Server) bindShareHere(src int, what, where string) (bool, error) {
	dst, err := s.openMountPoint(where)
	if err != nil {
		return false, s.mountPointErr(where, err)
	}
	defer func() { unix.Close(dst) }() //nolint:errcheck // the mount point bound or not
	for i := 0; mountedOn(dst); i++ {
		if sameDir(src, dst) {
			return false, nil
		}
		if i == maxStacked {
			return false, &Error{Message: fmt.Sprintf("%s holds more than %d mounts, none of them the folder %s: not the agent's; unmount them", where, maxStacked, what)}
		}
		unix.Close(dst) //nolint:errcheck // the mount point opened again below
		dst = -1
		// on the way checked just now, root's alone
		parent, err := s.openShareMount(filepath.Dir(where))
		if err != nil {
			return false, shareMountErr(filepath.Dir(where), err)
		}
		err = detachAt(parent, filepath.Base(where))
		unix.Close(parent) //nolint:errcheck // a directory opened for the unmount
		if err != nil {
			return false, &Error{Message: "unmounting what is mounted at " + where + " instead of the folder " + what, Output: err.Error()}
		}
		s.log.Warn("shared folder: a mount that showed another directory than the folder was taken off its mount point", "mount_point", where, "folder", what)
		if dst, err = s.openMountPoint(where); err != nil {
			return false, s.mountPointErr(where, err)
		}
	}
	if err := bindTree(src, dst); err != nil {
		return false, &Error{Message: "binding " + what + " at " + where, Output: err.Error()}
	}
	return true, nil
}

// openShareMount opens the directory where below the web root — a mount
// point, or the directory it lies in — for a look or an unmount, from the
// web root one directory at a time without following a symlink, whoever's
// directories they are: the home of a guest with a shell, which the guest
// owns, holds the mount points a panel up to 0.8.19 put there, and the agent
// only looks at what is mounted there, and takes it off.
func (s *Server) openShareMount(where string) (int, error) {
	root := filepath.Clean(s.cfg.WWWRoot)
	rel, ok := strings.CutPrefix(where, root+"/")
	if !ok || !cleanAbs(where) {
		return -1, fmt.Errorf("%s is not below the web root %s", where, root)
	}
	rfd, err := openDirTrusted(root)
	if err != nil {
		return -1, err
	}
	defer unix.Close(rfd) //nolint:errcheck // a directory opened for reading
	return openDirAt(rfd, root, rel, false, 0)
}

// shareMountErr is why the mount point where could not be opened: a symlink
// on the way is refused, the rest is the host's.
func shareMountErr(where string, err error) *Error {
	if errors.Is(err, errSymlinkComponent) {
		return &Error{Status: http.StatusForbidden, Message: "the mount point " + where + " is refused: " + err.Error()}
	}
	return &Error{Message: "open " + where, Output: err.Error()}
}

// mountedAt tells whether something is mounted on the entry name of dir;
// a name that is missing or no directory has nothing mounted on it.
func mountedAt(dir int, name string) bool {
	fd, err := unix.Openat(dir, name, dirFlags, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd) //nolint:errcheck // a directory opened for a look
	return mountedOn(fd)
}

// unbindShareHere unmounts whatever is mounted at the mount point where and
// says whether something was; a folder in use stays mounted (EBUSY).
func (s *Server) unbindShareHere(where string) (bool, error) {
	dir, name := filepath.Dir(where), filepath.Base(where)
	parent, err := s.openShareMount(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, shareMountErr(dir, err)
	}
	defer unix.Close(parent) //nolint:errcheck // a directory opened for the unmount
	unmounted := false
	for i := 0; mountedAt(parent, name); i++ {
		if i == maxStacked {
			return unmounted, &Error{Message: fmt.Sprintf("%s still holds a mount after %d unmounts", where, maxStacked)}
		}
		if err := unmountAt(parent, name, 0); err != nil {
			if errors.Is(err, unix.EBUSY) {
				return unmounted, &Error{Status: http.StatusConflict, Message: "unmounting the shared folder at " + where + ": it is in use (a session whose working directory lies inside, an open file)"}
			}
			return unmounted, &Error{Message: "unmounting the shared folder at " + where, Output: err.Error()}
		}
		unmounted = true
	}
	return unmounted, nil
}

// shareBind binds a shared folder (/v1/shares/bind): the text first
// (CheckShareBind), then the folder opened in the host's mount namespace,
// and, while no call that changes directories below the web root is under
// way (shareMu), the record and the bind there. The folder is opened before
// shareMu is taken: the walk goes through the owner's tree, which may hold
// a FUSE mount of the owner's that keeps a lookup waiting, and the calls
// under a guard must not wait with it.
//
// The guard the walk gets may not know of a shared folder bound since on
// the way to the folder, and the walk would cross its mount. The records
// read again under the lock do not always tell of it either. An unbind
// cannot take the mount away meanwhile: it unmounts without MNT_DETACH, and
// the kernel refuses (EBUSY, 409 "in use") while the walk's descriptor below
// the mount is open, until the bind ends. But a bind of another folder at
// that mount point detaches the mount, in use or not (bindShareHere), and an
// unbind then takes that folder and the record away. So the walk refuses a
// folder on another mount than the home it was reached from
// (openShareSource): a descriptor stays on the mount it was opened on,
// whatever goes before the lock. (open_tree would not clone a directory of
// a detached mount either: EINVAL.)
func (s *Server) shareBind(ctx context.Context, req *ShareBindRequest) (*ShareBindResponse, error) {
	if err := CheckShareBind(req.What, req.Where, s.unitHost()); err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: "binding " + req.What + " at " + req.Where + " is not allowed: " + err.Error()}
	}
	g, err := s.loadShareGuard()
	if err != nil {
		return nil, err
	}
	src := -1
	if err := inHostMountNS(func() error {
		var err error
		src, err = s.openShareSource(req.What, g)
		return err
	}); err != nil {
		return nil, err
	}
	defer unix.Close(src) //nolint:errcheck // the folder bound or not
	unlock, err := s.lockShareUnits(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// what is mounted meanwhile counts too
	if g, err = s.loadShareGuard(); err != nil {
		return nil, err
	}
	if m, ok := g.covering(req.What); ok {
		return nil, shareSourceErr(req.What, "it lies at or below "+m+", the mount point of another shared folder")
	}
	points, err := s.recordedMountPoints(filepath.Clean(s.cfg.WWWRoot))
	if err != nil {
		return nil, &Error{Message: "reading the agent's records of the shared folders", Output: err.Error()}
	}
	recorded := slices.Contains(points, req.Where)
	// The record as it was goes back should the bind fail: it names the
	// folder still bound there, and the directory bound, which the records
	// listing tells the API and the agent's start checks the mount by.
	var prev *shareRecord
	if recorded {
		prev = s.shareRecordAt(req.Where)
	}
	if err := s.writeShareRecord(shareRecord{What: req.What, Where: req.Where}); err != nil {
		return nil, err
	}
	var changed bool
	err = inHostMountNS(func() error {
		var err error
		changed, err = s.bindShareHere(src, req.What, req.Where)
		return err
	})
	if err != nil {
		switch {
		case !recorded:
			// nothing of it is mounted: forgotten again
			if rerr := s.removeShareRecord(req.Where); rerr != nil {
				s.log.Warn("shared folder: the record of a folder not bound stays", "mount_point", req.Where, "err", rerr)
			}
		case prev != nil:
			if rerr := s.writeShareRecord(*prev); rerr != nil {
				s.log.Warn("shared folder: the record names the folder that was not bound; the agent checks the mount by that folder's path as it starts", "mount_point", req.Where, "folder", req.What, "err", rerr)
			}
		}
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(src, &st); err == nil {
		if err := s.writeShareRecord(shareRecord{What: req.What, Where: req.Where, Dev: st.Dev, Ino: st.Ino}); err != nil {
			s.log.Warn("shared folder: the directory bound is not recorded; the agent checks the mount by the folder's path as it starts", "mount_point", req.Where, "err", err)
		}
	}
	if changed {
		s.log.Info("shared folder bound", "folder", req.What, "mount_point", req.Where, "origin", req.Origin)
	}
	return &ShareBindResponse{Mounted: true, Changed: changed}, nil
}

// shareUnbind unmounts a shared folder and forgets it (/v1/shares/unbind). A
// unit a panel up to 0.8.20 wrote for the mount point goes first, whatever
// comes next: systemd would mount the folder again by its path at boot. A
// folder in use stays mounted, and recorded: the way to it stays guarded
// while it is.
func (s *Server) shareUnbind(ctx context.Context, req *ShareUnbindRequest) (*ShareUnbindResponse, error) {
	where := req.Where
	if err := s.unbindAllowed(where); err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: "unbinding " + where + " is not allowed: " + err.Error()}
	}
	unlock, err := s.lockShareUnits(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	resp := &ShareUnbindResponse{}
	name := mountUnitName(where)
	unit := s.unitPath(name)
	if st, err := os.Lstat(unit); err == nil && !st.IsDir() {
		if err := disableUnit(ctx, name); err != nil {
			s.log.Warn("shared folder unit not disabled", "unit", name, "err", err)
		}
		res, err := s.removeShareUnitLocked(ctx, name, unit)
		if err != nil {
			return nil, err
		}
		resp.UnitRemoved = res.Removed
	}
	err = inHostMountNS(func() error {
		var err error
		resp.Unmounted, err = s.unbindShareHere(where)
		return err
	})
	if err != nil {
		var ae *Error
		if resp.UnitRemoved && errors.As(err, &ae) {
			ae.Message += "; its mount unit of an older panel is gone all the same, so that systemd mounts nothing there at boot"
		}
		return nil, err
	}
	if err := s.removeShareRecord(where); err != nil {
		return nil, &Error{Message: "forgetting the shared folder at " + where, Output: err.Error()}
	}
	if resp.Unmounted {
		s.log.Info("shared folder unbound", "mount_point", where)
	}
	return resp, nil
}

// unbindAllowed tells why the agent must not unbind the mount point where.
// It must look like one of the panel's by its text — a clean path,
// <www_root>/<login>/<name> or <www_root>/.shares/<login>/<name> — and,
// unless the agent knows a shared folder at exactly that mount point (its
// record of one, or a unit of an older panel's in the panel's form), lie
// where a panel account has its shared folders (mountPointOf, the account
// checks of a bind). An unmount only ever takes something away from what a
// guest sees; with the account checks, the folders of a guest the agent no
// longer counts as a panel account — one added to a group of the system's,
// or a privileged one, since its folders were bound — would stay bound and
// recorded for good, and mp user rm of that guest would fail at them. The
// look is taken before shareMu, as the account checks always were: a record
// gone meanwhile leaves an unbind of a mount point of the panel's shape,
// which takes nothing but what is mounted there.
func (s *Server) unbindAllowed(where string) error {
	h := s.unitHost()
	// the text alone first: records and units are looked up only for a
	// mount point of the panel's shape
	if err := CheckShareUnbind(where, true, h); err != nil {
		return err
	}
	return CheckShareUnbind(where, s.knowsShareAt(where), h)
}

// knowsShareAt tells whether the agent knows a shared folder at exactly the
// mount point where: its record of one, or the mount unit an older panel
// wrote for it, in the panel's form (legacyShareUnit) — an administrator's
// own unit there is not one.
func (s *Server) knowsShareAt(where string) bool {
	if points, err := s.recordedMountPoints(filepath.Clean(s.cfg.WWWRoot)); err == nil && slices.Contains(points, where) {
		return true
	}
	name := mountUnitName(where)
	p := s.unitPath(name)
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
		return false
	}
	content, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	_, w, err := legacyShareUnit(name, content, s.unitHost())
	return err == nil && w == where
}

// shareStatus tells what lies at the mount points (/v1/shares/status): a
// mount in the host's namespace, the agent's record of a folder, a unit of
// an older panel's.
func (s *Server) shareStatus(_ context.Context, req *ShareStatusRequest) (*ShareStatusResponse, error) {
	root := filepath.Clean(s.cfg.WWWRoot)
	for _, p := range req.Paths {
		if !cleanAbs(p) || !below(p, root) {
			return nil, &Error{Status: http.StatusBadRequest, Message: p + " is no mount point below the web root " + root}
		}
	}
	recorded, err := s.recordedMountPoints(root)
	if err != nil {
		return nil, &Error{Message: "reading the agent's records of the shared folders", Output: err.Error()}
	}
	resp := &ShareStatusResponse{Entries: make([]ShareMountState, len(req.Paths))}
	for i, p := range req.Paths {
		e := ShareMountState{Where: p}
		for _, r := range recorded {
			e.Recorded = e.Recorded || r == p
		}
		if st, err := os.Lstat(s.unitPath(mountUnitName(p))); err == nil && !st.IsDir() {
			e.Unit = true
		}
		resp.Entries[i] = e
	}
	err = inHostMountNS(func() error {
		for i, p := range req.Paths {
			if fd, err := s.openShareMount(p); err == nil {
				resp.Entries[i].Mounted = mountedOn(fd)
				unix.Close(fd) //nolint:errcheck // a directory opened for a look
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// retireUnsafeShareUnits takes over, as the agent starts, the mount units of
// shared folders that the panels up to 0.8.20 wrote: systemd mounts them by
// path at every boot. Each one in the panel's form (legacyShareUnit) is
// recorded, disabled and removed (systemd forgets it); a folder it mounted
// stays mounted, and the API binds every folder that is not at its start
// (refreshShareMounts), after the agent's. A mount unit below the web root
// in another form, an administrator's own, stays as it is: the agent warns,
// never starts it and keeps the way to its mount point root's.
//
// Every unit is recorded first, then every recorded mount is checked
// against its folder (verifyShareMounts), and only then do the units go:
// each removal reloads systemd, twice with its disable, and on a host with
// many units the reloads may take the whole start-up budget. Checked
// afterwards, what a unit mounted through a symlink its owner put there
// before the upgrade would stay bound — looking mounted to the panel, which
// binds only what is not. A unit not removed this time stays recorded, and
// what it mounts is checked again at the next start.
func (s *Server) retireUnsafeShareUnits(ctx context.Context) {
	root := filepath.Clean(s.cfg.WWWRoot)
	dir := s.unitDirectory()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("the mount units of the shared folders cannot be listed: those of an older panel stay as they are, and systemd mounts them by path at boot", "dir", dir, "err", err)
	}
	h := s.unitHost()
	type unit struct{ name, what, where string }
	var recorded []unit
	for _, e := range entries {
		name := e.Name()
		where, _, ok := isShareMountUnit(name, root)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			s.log.Warn("a mount unit of a shared folder cannot be read: systemd mounts it by path at boot", "unit", name, "err", err)
			continue
		}
		what, _, err := legacyShareUnit(name, content, h)
		if err != nil {
			s.log.Warn("a mount unit below the web root that is not one of the panel's: systemd mounts it by path at boot, following any symlink on the way; the agent never starts it and keeps the way to its mount point as it is", "unit", name, "mount_point", where, "why", err)
			continue
		}
		if err := s.recordShareUnit(ctx, what, where); err != nil {
			s.log.Error("the mount unit of a shared folder was not taken over: systemd mounts it by path at boot, through whatever symlink its owner put on the way to the folder", "unit", name, "folder", what, "mount_point", where, "err", err)
			continue
		}
		recorded = append(recorded, unit{name, what, where})
	}
	s.verifyShareMounts(ctx)
	for _, u := range recorded {
		if err := s.removeRecordedShareUnit(ctx, u.name); err != nil {
			s.log.Error("the mount unit of a shared folder was not removed: systemd mounts it by path at boot, through whatever symlink its owner put on the way to the folder; the agent checks what it mounted at its next start", "unit", u.name, "folder", u.what, "mount_point", u.where, "err", err)
			continue
		}
		s.log.Info("the mount unit of a shared folder was taken over: the agent binds the folder itself now, and the panel does at its start", "unit", u.name, "folder", u.what, "mount_point", u.where)
	}
}

// recordShareUnit records the shared folder a unit of an older panel
// mounts, while shareMu is held for writing: from then on the guard sees
// its mount point by the record, whenever the unit goes.
func (s *Server) recordShareUnit(ctx context.Context, what, where string) error {
	unlock, err := s.lockShareUnits(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.writeShareRecord(shareRecord{What: what, Where: where})
}

// removeRecordedShareUnit disables and removes the unit name of a shared
// folder recorded already (recordShareUnit), while shareMu is held for
// writing.
func (s *Server) removeRecordedShareUnit(ctx context.Context, name string) error {
	unlock, err := s.lockShareUnits(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := disableUnit(ctx, name); err != nil {
		s.log.Warn("shared folder unit not disabled", "unit", name, "err", err)
	}
	_, err = s.removeShareUnitLocked(ctx, name, filepath.Join(s.unitDirectory(), name))
	return err
}

// shareCheckWait bounds the check of one mount as the agent starts
// (verifyShareMounts): a lookup in the owner's tree that does not end — an
// NFS server gone away, a FUSE mount of the owner's that never answers —
// leaves that mount as it is, not the ones after it. A variable for tests.
var shareCheckWait = 10 * time.Second

// verifyShareMounts checks, as the agent starts, what is mounted at the
// mount points of its records: a unit of an older panel mounted its folder
// by path, through whatever symlink the folder's owner had put there by
// then. A mount that is neither the directory the agent bound there (its
// record says which) nor the folder as shareBind opens it now is taken off,
// in use or not — and so is one whose folder shareBind would refuse; the API
// binds the folder again at its start if it passes, and marks the share
// failed, with why, if not.
// It runs before the agent listens, so it takes no lock. A check that does
// not end within shareCheckWait, or within ctx, is left behind and the agent
// goes on, and starts; should that check end later, it takes nothing off
// (checkShareMount): by then the API may have bound the folder there.
func (s *Server) verifyShareMounts(ctx context.Context) {
	recs, err := s.shareRecords()
	if err != nil {
		s.log.Warn("the agent's records of the shared folders cannot be read: what is mounted at their mount points is not checked", "dir", s.shareRecordDir(), "err", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	g, err := s.loadShareGuard()
	if err != nil {
		s.log.Warn("the mounts of the shared folders are not checked", "err", err)
		return
	}
	for _, rec := range recs {
		if ctx.Err() != nil {
			s.log.Error("the check of what is mounted at the mount points of the shared folders did not end: those not checked yet stay as they are", "mount_point", rec.Where, "folder", rec.What, "err", ctx.Err())
			return
		}
		cctx, cancel := context.WithTimeout(ctx, shareCheckWait)
		done := make(chan shareCheck, 1)
		go func() { done <- s.checkShareMount(cctx, rec, g) }()
		var c shareCheck
		select {
		case c = <-done:
			cancel()
		case <-cctx.Done():
			cancel()
			s.log.Error("the check of what is mounted at the mount point of a shared folder did not end: that mount stays as it is", "mount_point", rec.Where, "folder", rec.What, "err", cctx.Err())
			continue
		}
		switch {
		case c.err != nil:
			s.log.Error("a mount at the mount point of a shared folder that may show another directory than the folder could not be taken off: unmount it (umount -l)", "mount_point", rec.Where, "folder", rec.What, "why", c.why, "err", c.err)
		case c.taken:
			s.log.Error("a mount at the mount point of a shared folder was taken off: it did not show the folder as the agent binds it; the panel binds the folder again at its start if it passes", "mount_point", rec.Where, "folder", rec.What, "why", c.why)
		}
	}
}

// shareCheck is what came of checkShareMount: a mount taken off, why, and
// what failed.
type shareCheck struct {
	taken    bool
	why, err error
}

// checkShareMount takes off what is mounted at rec's mount point unless it
// is rec's folder, opened as shareBind opens it, in the host's mount
// namespace — and only while ctx lasts: a check that ends after its time
// knows nothing of a bind the API made since.
func (s *Server) checkShareMount(ctx context.Context, rec shareRecord, g *shareGuard) shareCheck {
	var c shareCheck
	c.err = inHostMountNS(func() error {
		dir, name := filepath.Dir(rec.Where), filepath.Base(rec.Where)
		parent, err := s.openShareMount(dir)
		if err != nil {
			return nil // nothing there to check
		}
		defer unix.Close(parent) //nolint:errcheck // a directory opened for a look
		for i := 0; i < maxStacked; i++ {
			fd, err := unix.Openat(parent, name, dirFlags, 0)
			if err != nil {
				return nil
			}
			if !mountedOn(fd) {
				unix.Close(fd) //nolint:errcheck // a directory opened for a look
				return nil
			}
			var st unix.Stat_t
			if rec.Ino != 0 && unix.Fstat(fd, &st) == nil && st.Dev == rec.Dev && st.Ino == rec.Ino {
				// what the agent bound, whatever its owner renamed since
				unix.Close(fd) //nolint:errcheck // a directory opened for a look
				return nil
			}
			src, serr := s.openShareSource(rec.What, g)
			if serr == nil {
				same := sameDir(src, fd)
				unix.Close(src) //nolint:errcheck // a directory opened for a look
				if same {
					unix.Close(fd) //nolint:errcheck // a directory opened for a look
					return nil
				}
				serr = errors.New("the mount shows another directory than the folder")
			}
			unix.Close(fd) //nolint:errcheck // a directory opened for a look
			c.why = serr
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := detachAt(parent, name); err != nil {
				return err
			}
			c.taken = true
		}
		return nil
	})
	return c
}
