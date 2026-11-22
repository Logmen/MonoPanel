package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// etcDir is the directory a private /etc takes the place of.
const etcDir = "/etc"

// maxEtcFile bounds a file the private /etc holds a copy of: the account
// databases of a large host are a few megabytes, and nothing a program reads
// from /etc as it starts is anywhere near this.
const maxEtcFile = 64 << 20

// buildMounts makes the program's mount namespace: the private /etc
// (privateEtc), then the views without symlinks (noSymlinksBelow) — a view
// below /etc is one of what the private /etc holds there.
//
// The namespace is the program's own and a slave of the caller's: nothing
// mounted here reaches the caller, what the system mounts later still comes
// in (an automounted repository disk, say), and all of it dies with the
// program. The trees mounted here take nothing in, though: they are private.
func buildMounts(c *cmdline) error {
	what := "--nosymfollow"
	if len(c.noSymlinks) == 0 {
		what = "--etc"
	}
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("%s: a mount namespace (unshare): %w", what, err)
	}
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("%s: making the mounts slaves: %w", what, err)
	}
	if c.privateEtc() {
		if err := privateEtc(c.etc, c.etcBind); err != nil {
			return fmt.Errorf("--etc: %w", err)
		}
	}
	return noSymlinksBelow(c.noSymlinks)
}

// noSymlinksBelow has the program follow no symlink below dirs. Landlock
// decides on the file a path ends at and grants a tree however it is
// reached: when a program may read two trees, a symlink in the one leads
// into the other. restic backing up the web root and the panel's own
// configuration, certificates and dumps in one run reads all of them, and an
// account that swaps a directory of its home for a symlink to
// /etc/monopanel while restic is inside it would have the panel's secret key
// stored as its own file — or another account's files, which the web root
// holds as well. Without Landlock this view is what keeps such a symlink from
// leading to /etc/shadow.
//
// Each directory is mounted over itself, its submounts with it (the shared
// folders in the homes), with nosymfollow: a path that runs into a symlink
// there ends with ELOOP, whether the link is on the way or at its end, while
// lstat and readlink still see the link — which is all restic, which stores
// a symlink as one, ever does with them.
//
// It needs the new mount API with MOUNT_ATTR_NOSYMFOLLOW (Linux 5.14) and the
// right to mount; the agent finds out once, as it starts (--check), and the
// doctor says when a backup has to go without.
func noSymlinksBelow(dirs []string) error {
	for _, d := range dirs {
		if err := mountNoSymfollow(d); err != nil {
			return fmt.Errorf("--nosymfollow %s: %w", d, err)
		}
	}
	return nil
}

// mountNoSymfollow mounts a nosymfollow copy of the mount tree at dir, a
// directory, over dir.
func mountNoSymfollow(dir string) error {
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck // a descriptor of a path
	tree, err := unix.OpenTree(fd, "", unix.OPEN_TREE_CLONE|unix.O_CLOEXEC|unix.AT_RECURSIVE|unix.AT_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("open_tree: %w", err)
	}
	defer unix.Close(tree) //nolint:errcheck // the tree is attached or abandoned
	if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_NOSYMFOLLOW}); err != nil {
		return fmt.Errorf("mount_setattr(nosymfollow): %w", err)
	}
	if err := unix.MoveMount(tree, "", fd, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount: %w", err)
	}
	// The copy is a slave of the system's mount, as the namespace's mounts
	// are: a mount that appears below dir while the program runs — a shared
	// folder started during a backup — would come in as the system has it,
	// without nosymfollow, and a directory swapped for a symlink inside it
	// would be followed again. Private, the copy takes nothing in; and one
	// that came in before this call is part of the tree it applies to, so it
	// gets nosymfollow along with the rest.
	if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_NOSYMFOLLOW, Propagation: unix.MS_PRIVATE}); err != nil {
		return fmt.Errorf("mount_setattr(private): %w", err)
	}
	return nil
}

// etcEntry is what the private /etc holds at one path: a copy of a file, a
// tree cloned from the system to be bound there, or a symlink as it is.
type etcEntry struct {
	path string
	data []byte
	mode uint32
	// tree is the detached clone to bind at path, -1 for a copy
	tree int
	dir  bool
	// link is the target of a symlink bound as it is
	link string
}

// privateEtc puts a tmpfs over /etc that holds the entries named and nothing
// else: of copies a file's content and mode as they are now (read through
// its symlinks — systemd-resolved keeps resolv.conf in /run), a directory
// bound as it is; every entry of binds bound as it is, a file too, and a
// symlink made again. A program in it reads /etc/resolv.conf, /etc/hosts and
// /etc/passwd as they were when it started, whatever the host renames over
// them meanwhile — NetworkManager and dhclient replace resolv.conf that way,
// useradd the account databases — where a Landlock rule on such a file stays
// with the old inode and refuses the new one: name resolution, and with it a
// remote repository, would go halfway through a backup. A name the system's
// /etc does not have is not here either, and neither is what is not named
// (/etc/shadow, the host keys).
//
// The entries are read and cloned before the tmpfs covers the system's /etc;
// the mounts are private and all of it read-only.
func privateEtc(copies, binds []string) error {
	var entries []etcEntry
	defer func() {
		for _, e := range entries {
			if e.tree >= 0 {
				unix.Close(e.tree) //nolint:errcheck // attached or abandoned
			}
		}
	}()
	seen := map[string]bool{}
	for _, l := range []struct {
		paths []string
		bind  bool
	}{{copies, false}, {binds, true}} {
		for _, p := range l.paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			e, ok, err := etcSource(p, l.bind)
			if err != nil {
				return err
			}
			if ok {
				entries = append(entries, e)
			}
		}
	}
	if err := unix.Mount("tmpfs", etcDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
		return fmt.Errorf("a tmpfs on %s: %w", etcDir, err)
	}
	slices.SortFunc(entries, func(a, b etcEntry) int { return strings.Compare(a.path, b.path) })
	for _, e := range entries {
		// no entry lies below another (cmdline.valid): what is made here is
		// made in the tmpfs, never in a directory bound from the system
		if err := os.MkdirAll(filepath.Dir(e.path), 0o755); err != nil {
			return err
		}
		if e.link != "" {
			if err := os.Symlink(e.link, e.path); err != nil {
				return err
			}
			continue
		}
		if e.tree < 0 {
			if err := writeEtcCopy(e); err != nil {
				return err
			}
			continue
		}
		if e.dir {
			if err := unix.Mkdir(e.path, 0o755); err != nil {
				return &os.PathError{Op: "mkdir", Path: e.path, Err: err}
			}
		} else if err := writeEtcCopy(etcEntry{path: e.path, mode: 0o600}); err != nil {
			return err
		}
		if err := unix.MoveMount(e.tree, "", unix.AT_FDCWD, e.path, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
			return fmt.Errorf("binding %s: move_mount: %w", e.path, err)
		}
	}
	// what is bound here is a copy of the system's mount, and a slave of it:
	// private, it takes in nothing the host mounts below it later
	if err := unix.Mount("none", etcDir, "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making %s private: %w", etcDir, err)
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, etcDir, unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
		return fmt.Errorf("mount_setattr(%s, read-only): %w", etcDir, err)
	}
	return nil
}

// etcSource reads what the private /etc holds at p: a file's content, or a
// clone of the directory (with bind, of the file too) as it is — and with
// bind a symlink as a symlink, which the program then sees as the system has
// it. ok is false when the system has nothing there.
func etcSource(p string, bind bool) (etcEntry, bool, error) {
	e := etcEntry{path: p, tree: -1}
	if bind {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); errors.Is(err, unix.ENOENT) {
			return e, false, nil
		} else if err != nil {
			return e, false, &os.PathError{Op: "stat", Path: p, Err: err}
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			target, err := os.Readlink(p)
			if err != nil {
				return e, false, err
			}
			e.link = target
			return e, true, nil
		}
	}
	// O_NONBLOCK: a FIFO under the name would hang the open
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return e, false, nil
	}
	if err != nil {
		return e, false, &os.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return e, false, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	switch t := st.Mode & unix.S_IFMT; {
	case t == unix.S_IFDIR || bind && t == unix.S_IFREG:
		// the clone is of what was opened, whatever is renamed meanwhile
		tree, err := unix.OpenTree(fd, "", unix.OPEN_TREE_CLONE|unix.O_CLOEXEC|unix.AT_RECURSIVE|unix.AT_EMPTY_PATH)
		if err != nil {
			return e, false, fmt.Errorf("%s: open_tree: %w", p, err)
		}
		e.tree, e.dir = tree, t == unix.S_IFDIR
	case t == unix.S_IFREG:
		data, err := io.ReadAll(io.LimitReader(f, maxEtcFile+1))
		if err != nil {
			return e, false, &os.PathError{Op: "read", Path: p, Err: err}
		}
		if len(data) > maxEtcFile {
			return e, false, fmt.Errorf("%s: larger than %d MB, not a system file to copy", p, maxEtcFile>>20)
		}
		e.data, e.mode = data, st.Mode&0o777
	default:
		return e, false, fmt.Errorf("%s: neither a file nor a directory: %w", p, fs.ErrInvalid)
	}
	return e, true, nil
}

// writeEtcCopy makes the file of a copy in the private /etc.
func writeEtcCopy(e etcEntry) error {
	fd, err := unix.Open(e.path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &os.PathError{Op: "create", Path: e.path, Err: err}
	}
	f := os.NewFile(uintptr(fd), e.path)
	_, err = f.Write(e.data)
	if err == nil {
		// the umask has no say: the copy has the mode of what it copies
		err = unix.Fchmod(fd, e.mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &os.PathError{Op: "write", Path: e.path, Err: err}
	}
	return nil
}
