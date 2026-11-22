package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent/confcheck"
)

// restoreMerge moves an in-place restore from the agent's staging directory
// into place.
type restoreMerge struct {
	s   *Server
	ctx context.Context
	out io.Writer

	moved, skipped, failed int
	why                    map[string]int
	// protected are config.yaml and the secret key as configured and with
	// their directories resolved (Server.protectedFiles), looked up once:
	// a restore of a home visits every file.
	protected []string
	// svcUID and svcGID are the service user's, the owner of what is
	// restored into the data directory (looked up by sanitize).
	svcUID, svcGID int
	// aside is a restore aside: the tree stays where restic put it, and only
	// what is unsafe anywhere is taken out of it (sanitize).
	aside bool
	// guard holds the shared folders' mount points while the merge runs —
	// an in-place restore holds it from its first restic run on: the way
	// to one keeps its own owner and mode, nothing but a directory stands
	// there, and a mount point itself is left alone — the folder it shows
	// comes back with its site (sharemount.go).
	guard *shareGuard
}

// isProtected tells config.yaml and the secret key.
func (m *restoreMerge) isProtected(p string) bool {
	if m.protected == nil {
		m.protected = m.s.protectedFiles()
	}
	for _, f := range m.protected {
		if p == f {
			return true
		}
	}
	return false
}

// coversProtected tells a directory above them.
func (m *restoreMerge) coversProtected(p string) bool {
	if m.protected == nil {
		m.protected = m.s.protectedFiles()
	}
	for _, f := range m.protected {
		if p == f || below(f, p) {
			return true
		}
	}
	return false
}

func (m *restoreMerge) skip(kind string) {
	if m.why == nil {
		m.why = map[string]int{}
	}
	m.skipped++
	m.why[kind]++
}

func (m *restoreMerge) skippedWhy() string {
	if len(m.why) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(m.why))
	for k, n := range m.why {
		parts = append(parts, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func (m *restoreMerge) fail(p string, err error) {
	m.failed++
	if m.failed <= 20 {
		fmt.Fprintf(m.out, "in place: %s: %v\n", p, err)
	}
}

// sanitize makes the staged tree what may be written over the system: no
// device nodes, FIFOs or sockets, no set-user-ID or set-group-ID file, no
// file capabilities; below the configuration directory nothing but root's
// files and directories, writable by root alone, that pass the content checks
// of config/apply; below the data directory the service user's. The staging
// directory is root's and 0700, so nothing else reaches the tree while this
// runs.
func (m *restoreMerge) sanitize(staging string) error {
	m.svcUID, m.svcGID = -1, -1
	if u, err := lookupAccount(m.s.cfg.ServiceUser); err == nil {
		m.svcUID, _ = strconv.Atoi(u.Uid)
		m.svcGID, _ = strconv.Atoi(u.Gid)
	}
	return filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := m.ctx.Err(); err != nil {
			return err
		}
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		mode := st.Mode()
		// where the entry goes: staging holds the snapshot's absolute paths
		dst := "/" + strings.TrimPrefix(strings.TrimPrefix(p, staging), "/")
		root, inRoot := m.s.inPlaceRoot(dst)
		inRoot = inRoot && !m.aside
		switch {
		case mode&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeIrregular) != 0:
			m.skip("device nodes, FIFOs or sockets")
			return os.Remove(p)
		case mode&fs.ModeSymlink != 0 && inRoot && root.kind == inPlaceConfig:
			// the panel makes no symlink there, and one would carry a later
			// write of a configuration file elsewhere
			m.skip("symlinks in the panel's configuration")
			return os.Remove(p)
		case mode&fs.ModeSymlink != 0 && inRoot && root.kind == inPlaceHomes && filepath.Dir(dst) == root.dir:
			// a home is a directory: a symlink in its place would make the
			// account's home whatever it points at
			m.skip("symlinks where a home belongs")
			return os.Remove(p)
		case mode.IsRegular():
			if mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
				m.skip("set-user-ID or set-group-ID bits")
				if err := os.Chmod(p, mode.Perm()); err != nil {
					return err
				}
			}
			// Reading the attribute needs no privilege, removing one does:
			// look first, so only a file that has capabilities loses them.
			if _, err := unix.Lgetxattr(p, "security.capability", nil); err == nil {
				if err := unix.Lremovexattr(p, "security.capability"); err != nil {
					return &os.PathError{Op: "removexattr", Path: p, Err: err}
				}
				m.skip("file capabilities")
			} else if !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
				return &os.PathError{Op: "getxattr", Path: p, Err: err}
			}
		case mode.IsDir() && mode&fs.ModeSetuid != 0:
			if err := os.Chmod(p, mode&(fs.ModePerm|fs.ModeSetgid|fs.ModeSticky)); err != nil {
				return err
			}
		}
		if !inRoot {
			return nil // a directory on the way to an include: not moved
		}
		switch root.kind {
		case inPlaceConfig:
			return m.sanitizeConfig(p, dst, st)
		case inPlaceData:
			if m.svcUID < 0 {
				return fmt.Errorf("the service user %s is unknown: %s cannot be handed to it", m.s.cfg.ServiceUser, dst)
			}
			if err := os.Lchown(p, m.svcUID, m.svcGID); err != nil {
				return err
			}
		}
		return nil
	})
}

// sanitizeConfig makes a staged entry of the configuration directory root's,
// in root's group or the service group, writable by root alone, and puts a
// file through the checks config/apply puts it through (confcheck): daemons
// running as root read files of this directory, and an in-place restore must
// not be the way around those checks. A file that fails them stays out.
func (m *restoreMerge) sanitizeConfig(p, dst string, st fs.FileInfo) error {
	if m.isProtected(dst) {
		return nil // never moved into place (entry)
	}
	gid := os.Getegid()
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && m.svcGID >= 0 && int(sys.Gid) == m.svcGID {
		gid = m.svcGID
	}
	if err := os.Lchown(p, os.Geteuid(), gid); err != nil {
		return err
	}
	// ACLs of the snapshot's would grant what the mode does not; a default
	// ACL would reach the files config/apply writes there later
	for _, acl := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		if err := unix.Lremovexattr(p, acl); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
			return &os.PathError{Op: "removexattr", Path: p, Err: err}
		}
	}
	if err := os.Chmod(p, st.Mode().Perm()&^0o022); err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	const limit = 16 << 20
	content, err := io.ReadAll(io.LimitReader(f, limit+1))
	f.Close()
	switch {
	case err != nil:
		return err
	case len(content) > limit:
		err = fmt.Errorf("larger than %d MB, not a configuration file", limit>>20)
	default:
		err = restoreConfCheck(dst, content)
	}
	if err != nil {
		m.fail(dst, err)
		return os.Remove(p)
	}
	return nil
}

// restoreConfCheck is confcheck.Check; tests put a check of their own here.
var restoreConfCheck = confcheck.Check

// merge moves each included path from the staging tree into place. The
// directories on the way to the destination are opened one by one without
// following a symlink below the tree they are in (openDirBelow), and every
// move happens relative to opened directories: a symlink an account left in
// its home is replaced, never followed. config.yaml and the secret key are
// never written; the web root and the panel's configuration directory stay
// the directories they are, with their own owner and mode.
func (m *restoreMerge) merge(stagingFd int, staging string, includes []string) {
	if m.guard == nil {
		// the caller holds none (restoreInPlace holds one from the first
		// restic run on)
		g, release, err := m.s.holdShareGuard()
		if err != nil {
			m.fail(m.s.cfg.WWWRoot, err)
			return
		}
		defer release()
		m.guard = g
	}
	sorted := append([]string{}, includes...)
	sort.Strings(sorted)
	var done []string
	for _, inc := range sorted {
		covered := false
		for _, d := range done {
			covered = covered || atOrBelow(inc, d)
		}
		if covered {
			continue
		}
		done = append(done, inc)
		root, ok := m.s.inPlaceRoot(inc)
		if !ok {
			m.fail(inc, errors.New("not a tree the panel restores in place"))
			continue
		}
		rel := strings.TrimPrefix(inc, "/")
		sp, err := openDirAt(stagingFd, staging, filepath.Dir(rel), false, 0)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(m.out, "in place: the snapshot has nothing at %s\n", inc)
				continue
			}
			m.fail(inc, err)
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(sp, filepath.Base(rel), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			unix.Close(sp) //nolint:errcheck // a directory opened for reading
			fmt.Fprintf(m.out, "in place: the snapshot has nothing at %s\n", inc)
			continue
		}
		if inc == root.dir {
			m.root(sp, filepath.Base(rel), inc, &st)
			unix.Close(sp) //nolint:errcheck // a directory opened for reading
			continue
		}
		// Missing directories on the way are made as restic makes them,
		// root's and 0700; in the configuration directory as setup makes
		// them, root's and the service group's; in the data directory the
		// service user's.
		mode, uid, gid := uint32(0o700), -1, -1
		switch root.kind {
		case inPlaceConfig:
			if m.svcGID >= 0 {
				mode, gid = 0o750, m.svcGID
			}
		case inPlaceData:
			uid, gid = m.svcUID, m.svcGID
		}
		dp, err := openDirBelowOwned(root.dir, filepath.Dir(inc), true, mode, uid, gid)
		if err != nil {
			unix.Close(sp) //nolint:errcheck // a directory opened for reading
			m.fail(inc, err)
			continue
		}
		m.entry(sp, dp, filepath.Base(rel), inc)
		unix.Close(sp) //nolint:errcheck // a directory opened for reading
		unix.Close(dp) //nolint:errcheck // a directory opened for reading
	}
}

// root merges the staged copy of a tree itself — the web root, the
// configuration directory — into the live one. That directory stays what it
// is, with its owner and mode, and a snapshot never replaces it: a symlink of
// the snapshot's there would send every later walk into the tree elsewhere.
func (m *restoreMerge) root(sp int, name, dir string, st *unix.Stat_t) {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		m.fail(dir, errors.New("the snapshot has no directory there"))
		return
	}
	sfd, err := unix.Openat(sp, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		m.fail(dir, err)
		return
	}
	defer unix.Close(sfd) //nolint:errcheck // a directory opened for reading
	dfd, err := openDirTrusted(dir)
	if err != nil {
		m.fail(dir, err)
		return
	}
	defer unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	m.children(sfd, dfd, dir)
}

// keepsOwnMeta tells the directories whose owner and mode a snapshot does
// not set: the web root, the configuration directory with config.yaml and
// the secret key in it, and the way to a shared folder's mount point.
func (m *restoreMerge) keepsOwnMeta(p string) bool {
	if _, onWay := m.guard.holding(p); onWay {
		return true
	}
	return p == filepath.Clean(m.s.cfg.WWWRoot) || m.coversProtected(p)
}

// entry moves the staged name (in directory sp) to the same name in dp.
func (m *restoreMerge) entry(sp, dp int, name, dst string) {
	if m.ctx.Err() != nil {
		return
	}
	if m.isProtected(dst) {
		m.skip("files that are root's (config.yaml, the secret key)")
		return
	}
	if m.s.inRestoreArea(dst) {
		// the staging of in-place restores, which a snapshot made by hand
		// may hold: the agent's own, never one of the snapshot's
		m.skip("the agent's restore area")
		return
	}
	if m.guard.isMount(dst) {
		m.skip("mount points of shared folders (the folder comes back with its site)")
		return
	}
	var sst, dstSt unix.Stat_t
	if err := unix.Fstatat(sp, name, &sst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		m.fail(dst, err)
		return
	}
	if _, onWay := m.guard.holding(dst); onWay && sst.Mode&unix.S_IFMT != unix.S_IFDIR {
		m.skip("what is no directory on the way to a shared folder's mount point")
		return
	}
	derr := unix.Fstatat(dp, name, &dstSt, unix.AT_SYMLINK_NOFOLLOW)
	exists := derr == nil
	dstIsDir := exists && dstSt.Mode&unix.S_IFMT == unix.S_IFDIR
	switch sst.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if exists && !dstIsDir {
			if err := unix.Unlinkat(dp, name, 0); err != nil {
				m.fail(dst, err)
				return
			}
			exists = false
		}
		if !exists && !m.keepsOwnMeta(dst) {
			err := unix.Renameat(sp, name, dp, name)
			if err == nil {
				m.moved++
				return
			}
			if !errors.Is(err, unix.EXDEV) {
				m.fail(dst, err)
				return
			}
		}
		if !exists {
			if err := unix.Mkdirat(dp, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				m.fail(dst, err)
				return
			}
		}
		m.dir(sp, dp, name, dst, &sst)
	case unix.S_IFREG, unix.S_IFLNK:
		if dstIsDir {
			m.fail(dst, errors.New("a directory is in the way of the restored file"))
			return
		}
		err := unix.Renameat(sp, name, dp, name)
		if err == nil {
			m.moved++
			return
		}
		if !errors.Is(err, unix.EXDEV) {
			m.fail(dst, err)
			return
		}
		if err := m.copyLeaf(sp, dp, name, &sst); err != nil {
			m.fail(dst, err)
			return
		}
		m.moved++
	default:
		m.skip("device nodes, FIFOs or sockets")
	}
}

// dir merges the staged directory into the existing one of that name.
func (m *restoreMerge) dir(sp, dp int, name, dst string, sst *unix.Stat_t) {
	sfd, err := unix.Openat(sp, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		m.fail(dst, err)
		return
	}
	defer unix.Close(sfd) //nolint:errcheck // a directory opened for reading
	dfd, err := unix.Openat(dp, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		m.fail(dst, fmt.Errorf("%w: %v", errSymlinkComponent, err))
		return
	}
	defer unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	m.children(sfd, dfd, dst)
	if m.keepsOwnMeta(dst) {
		return
	}
	if err := setMeta(dfd, dp, name, sst); err != nil {
		m.fail(dst, err)
		return
	}
	copyXattrs(sfd, dfd)
	m.moved++
}

// children merges the entries of the staged directory sfd into the live
// directory dfd, which dst names.
func (m *restoreMerge) children(sfd, dfd int, dst string) {
	names, err := readDirNames(sfd)
	if err != nil {
		m.fail(dst, err)
		return
	}
	for _, n := range names {
		m.entry(sfd, dfd, n, filepath.Join(dst, n))
	}
}

// readDirNames lists the directory fd without consuming it.
func readDirNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	return f.Readdirnames(-1)
}

// setMeta gives the opened directory or file fd, the entry name in the
// directory dp, the staged owner, mode (without set-user-ID) and
// modification time. Owner and mode go through the descriptor; the time,
// which the kernel only sets by name on older systems, goes by name without
// following a symlink — whatever is there by then only gets a timestamp.
func setMeta(fd, dp int, name string, st *unix.Stat_t) error {
	if err := unix.Fchown(fd, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if err := unix.Fchmod(fd, st.Mode&0o3777); err != nil {
		return err
	}
	ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, st.Mtim}
	return unix.UtimesNanoAt(dp, name, ts, unix.AT_SYMLINK_NOFOLLOW)
}

// copyLeaf copies a staged file or symlink across file systems: into a fresh
// name in dp, owned and moded as staged, then renamed over the old one.
func (m *restoreMerge) copyLeaf(sp, dp int, name string, sst *unix.Stat_t) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tmp := ".mp-restore-" + hex.EncodeToString(b)
	if sst.Mode&unix.S_IFMT == unix.S_IFLNK {
		target, err := readlinkat(sp, name)
		if err != nil {
			return err
		}
		if err := unix.Symlinkat(target, dp, tmp); err != nil {
			return err
		}
		if err := unix.Fchownat(dp, tmp, int(sst.Uid), int(sst.Gid), unix.AT_SYMLINK_NOFOLLOW); err != nil {
			unix.Unlinkat(dp, tmp, 0) //nolint:errcheck // the temporary name of a failed copy
			return err
		}
		ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, sst.Mtim}
		unix.UtimesNanoAt(dp, tmp, ts, unix.AT_SYMLINK_NOFOLLOW) //nolint:errcheck // a symlink's time is cosmetic
		return renameOrDrop(dp, tmp, name)
	}
	src, err := unix.Openat(sp, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	in := os.NewFile(uintptr(src), name)
	defer in.Close()
	dfd, err := unix.Openat(dp, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	outF := os.NewFile(uintptr(dfd), tmp)
	_, err = io.Copy(outF, in)
	if err == nil {
		err = setMeta(dfd, dp, tmp, sst)
	}
	if err == nil {
		copyXattrs(src, dfd)
	}
	if cerr := outF.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		unix.Unlinkat(dp, tmp, 0) //nolint:errcheck // the temporary name of a failed copy
		return err
	}
	return renameOrDrop(dp, tmp, name)
}

func renameOrDrop(dp int, tmp, name string) error {
	if err := unix.Renameat(dp, tmp, dp, name); err != nil {
		unix.Unlinkat(dp, tmp, 0) //nolint:errcheck // the temporary name of a failed copy
		return err
	}
	return nil
}

func readlinkat(dirfd int, name string) (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(dirfd, name, buf)
		if err != nil {
			return "", err
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// copyXattrs carries the ACLs and user attributes over; security.* and
// trusted.* stay behind (file capabilities live there).
func copyXattrs(src, dst int) {
	sz, err := unix.Flistxattr(src, nil)
	if err != nil || sz <= 0 {
		return
	}
	buf := make([]byte, sz)
	if sz, err = unix.Flistxattr(src, buf); err != nil {
		return
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
		if !strings.HasPrefix(name, "system.posix_acl_") && !strings.HasPrefix(name, "user.") {
			continue
		}
		vsz, err := unix.Fgetxattr(src, name, nil)
		if err != nil || vsz < 0 {
			continue
		}
		val := make([]byte, vsz)
		if vsz, err = unix.Fgetxattr(src, name, val); err != nil {
			continue
		}
		unix.Fsetxattr(dst, name, val[:vsz], 0) //nolint:errcheck // an attribute the destination does not take
	}
}
