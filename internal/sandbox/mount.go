package sandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// noSymlinksBelow has the program follow no symlink below dirs. Landlock
// decides on the file a path ends at and grants a tree however it is
// reached: when a program may read two trees, a symlink in the one leads
// into the other. restic backing up the web root and the panel's own
// configuration, certificates and dumps in one run reads all of them, and an
// account that swaps a directory of its home for a symlink to
// /etc/monopanel while restic is inside it would have the panel's secret key
// stored as its own file — or another account's files, which the web root
// holds as well.
//
// In a mount namespace of the program's own, each directory is mounted over
// itself, its submounts with it (the shared folders in the homes), with
// nosymfollow: a path that runs into a symlink there ends with ELOOP,
// whether the link is on the way or at its end, while lstat and readlink
// still see the link — which is all restic, which stores a symlink as one,
// ever does with them. Nothing of this reaches the caller's mounts: the new
// namespace is a slave of the old one (what the system mounts later still
// comes in, an automounted repository disk say), and dies with the program.
//
// It needs the new mount API with MOUNT_ATTR_NOSYMFOLLOW (Linux 5.14) and the
// right to mount; the agent finds out once, as it starts (--check), and the
// doctor says when a backup has to go without.
func noSymlinksBelow(dirs []string) error {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("--nosymfollow: a mount namespace (unshare): %w", err)
	}
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("--nosymfollow: making the mounts slaves: %w", err)
	}
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
	return nil
}
