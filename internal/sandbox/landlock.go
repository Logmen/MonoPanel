// Package sandbox confines a program with Landlock before it starts:
// `monopanel sandbox-exec` restricts itself to the trees named on its command
// line and then execs the program, which inherits the restriction and passes
// it on to everything it runs.
//
// The agent runs restic through it (internal/agent/restic_sandbox.go). restic
// walks a backed-up home as root by path, and O_NOFOLLOW guards only the last
// component of a path: an account that swaps a directory of its own for a
// symlink to / while restic is inside it would have root read /etc/shadow
// through the link and store it as the account's file. Landlock decides on
// the file a path resolves to, not on the path: below the trees the sandbox
// grants, a link that leads out of them leads nowhere.
package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The filesystem rights of the first Landlock ABI (Linux 5.13).
const rightsV1 = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM

// fileRights are the rights a rule on a file rather than a directory may
// carry: the kernel refuses a file rule with any other.
const fileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
	unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

const readRights = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR

// nodeRights make device nodes, FIFOs and sockets: handled, and granted by
// no rule. restic makes none of them for a backup — its cache, temporary
// files and repository are files and directories, and the ssh it runs
// connects to the agent's master, it binds no socket — and a restore makes
// none either: the agent refuses a snapshot that would bring one
// (vetSnapshot). So a snapshot the API composed cannot have restic make a
// twin of /dev/sda even where that check had a gap.
const nodeRights = unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK

// ABI is the Landlock ABI version of the running kernel. An error means the
// kernel has none: older than 5.13, built without it (ENOSYS), or booted
// with an lsm= list that leaves it out (EOPNOTSUPP).
func ABI() (int, error) {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, fmt.Errorf("landlock: %w", errno)
	}
	return int(v), nil
}

// Handled are the filesystem rights a ruleset handles on a kernel of ABI
// version abi: those of version 1, REFER from version 2 (moving or linking
// a file into another directory), TRUNCATE from 3 and IOCTL_DEV from 5. A
// ruleset that names a right the kernel does not know is refused as a whole,
// so a newer kernel's rights are left out until this list knows them; the
// network rights (4) and scopes (6) stay unhandled on purpose: restic talks
// to its remote repositories, and a right that is not handled is not
// restricted.
func Handled(abi int) uint64 {
	h := uint64(rightsV1)
	if abi >= 2 {
		h |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		h |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		h |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return h
}

// Access is what a rule grants below a tree.
type Access int

const (
	// ReadOnly: read files, list directories.
	ReadOnly Access = iota
	// ReadExec: read and execute (programs, the dynamic loader, libraries).
	ReadExec
	// ReadWrite: every handled right but execution and nodeRights: make,
	// remove, move, write and truncate files, directories and symlinks.
	ReadWrite
)

func (a Access) String() string {
	switch a {
	case ReadExec:
		return "rx"
	case ReadWrite:
		return "rw"
	}
	return "ro"
}

// rights is the access mask of a rule of kind a on a directory or a file,
// within what the ruleset handles.
func (a Access) rights(dir bool, handled uint64) uint64 {
	var r uint64
	switch a {
	case ReadOnly:
		r = readRights
	case ReadExec:
		r = readRights | unix.LANDLOCK_ACCESS_FS_EXECUTE
	case ReadWrite:
		r = handled &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE | nodeRights)
	}
	if !dir {
		r &= fileRights
	}
	return r & handled
}

// ruleset is a Landlock ruleset being built.
type ruleset struct {
	fd      int
	handled uint64
}

func newRuleset(abi int) (*ruleset, error) {
	h := Handled(abi)
	attr := unix.LandlockRulesetAttr{Access_fs: h}
	// Only handled_access_fs is passed: every ABI version reads that much,
	// and the network and scope fields that follow it stay unhandled.
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Access_fs), 0)
	if errno != 0 {
		return nil, fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	return &ruleset{fd: int(fd), handled: h}, nil
}

// add grants access below the file or directory fd refers to.
func (r *ruleset) add(fd int, access uint64) error {
	if access == 0 {
		return nil
	}
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(r.fd), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_add_rule: %w", errno)
	}
	return nil
}

// restrictSelf puts the calling thread under the ruleset. Landlock wants
// no_new_privs first (or CAP_SYS_ADMIN): a set-user-ID program started in the
// sandbox must not regain what the sandbox took away. Both are the thread's
// own and survive execve, which is how the program gets them.
func (r *ruleset) restrictSelf() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(r.fd), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

func (r *ruleset) close() { unix.Close(r.fd) } //nolint:errcheck // the ruleset is applied or abandoned
