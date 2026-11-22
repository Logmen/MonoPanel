package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Exit statuses of sandbox-exec itself, as env(1) and timeout(1) have them:
// the program's own status otherwise, since sandbox-exec becomes the program.
const (
	// ExitSetup: the sandbox could not be built (a bad rule, a path that is
	// a symlink, a kernel without Landlock); nothing ran.
	ExitSetup = 125
	// ExitCannotRun: the program was found but could not be executed — in
	// the sandbox that is one outside the --rx trees.
	ExitCannotRun = 126
	// ExitNotFound: there is no such program.
	ExitNotFound = 127
)

// Rules name what a program in the sandbox may reach. A rule is a directory
// (and everything below it) or a single file.
type Rules struct {
	// RO, RW and RX are absolute paths. sandbox-exec opens each without
	// following a symlink in its last component and refuses one that is a
	// symlink: the caller resolves the system's own links first.
	RO, RW, RX []string
	// ROFiles and RWFiles are trees the caller opened itself, the way to
	// them checked and pinned by the descriptor: sandbox-exec inherits them
	// as descriptors 3, 4, … and grants what they refer to, whatever is
	// renamed or swapped on the way to them meanwhile.
	ROFiles, RWFiles []*os.File
	// NoSymlinks are directories (absolute, the caller's symlinks resolved)
	// below which the program follows no symlink at all: Landlock grants a
	// tree whatever way it is reached, so a symlink in one granted tree
	// still leads into another — a home's into the panel's configuration
	// when a backup reads both. A symlink is still read as one (lstat,
	// readlink), as restic stores it. See noSymlinksBelow.
	NoSymlinks []string
}

// Command is the argv of sandbox-exec running program with args under r
// (without the binary itself), and the files to hand it in that order as
// descriptors 3, 4, …: exec.Cmd.ExtraFiles.
func (r *Rules) Command(program string, args []string) ([]string, []*os.File) {
	argv, files := r.argv()
	argv = append(argv, "--", program)
	return append(argv, args...), files
}

// CheckCommand is the argv of sandbox-exec building the sandbox of r and
// exiting without running anything: whether this kernel, and whatever
// confines the caller (a container's profile), allow it.
func (r *Rules) CheckCommand() ([]string, []*os.File) {
	argv, files := r.argv()
	return append(argv, "--check"), files
}

func (r *Rules) argv() ([]string, []*os.File) {
	argv := []string{"sandbox-exec"}
	for _, p := range r.NoSymlinks {
		argv = append(argv, "--nosymfollow", p)
	}
	for _, l := range []struct {
		flag  string
		paths []string
	}{{"--ro", r.RO}, {"--rw", r.RW}, {"--rx", r.RX}} {
		for _, p := range l.paths {
			argv = append(argv, l.flag, p)
		}
	}
	var files []*os.File
	for _, l := range []struct {
		flag  string
		files []*os.File
	}{{"--ro-fd", r.ROFiles}, {"--rw-fd", r.RWFiles}} {
		for _, f := range l.files {
			argv = append(argv, l.flag, strconv.Itoa(3+len(files)))
			files = append(files, f)
		}
	}
	return argv, files
}

// rule is one rule of the command line: a path or an inherited descriptor.
type rule struct {
	access Access
	path   string
	fd     int // -1 for a path
}

func (r rule) String() string {
	if r.fd >= 0 {
		return "descriptor " + strconv.Itoa(r.fd)
	}
	return r.path
}

// cmdline is what sandbox-exec was asked: the rules, the trees without
// symlinks, and the program with its arguments — or, with --check, none.
type cmdline struct {
	rules      []rule
	noSymlinks []string
	check      bool
	argv       []string
}

const usage = "usage: sandbox-exec [--nosymfollow <dir>]… [--ro|--rw|--rx <path>]… [--ro-fd|--rw-fd|--rx-fd <n>]… -- <program> [args…] | --check"

// parse reads the command line of sandbox-exec: the rules, then "--" and the
// program with its arguments, which run as they are given, or --check.
func parse(args []string) (*cmdline, error) {
	c := &cmdline{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--":
			argv := args[i+1:]
			if len(argv) == 0 || !strings.HasPrefix(argv[0], "/") {
				return nil, errors.New("the program after -- must be an absolute path")
			}
			c.argv = argv
			return c, nil
		case "--check":
			if i != len(args)-1 {
				return nil, errors.New("--check ends the command line: it runs no program")
			}
			c.check = true
			return c, nil
		}
		flag, value, inline := strings.Cut(a, "=")
		if !inline {
			if flag != "--nosymfollow" && strings.TrimSuffix(flag, "-fd") != "--ro" && strings.TrimSuffix(flag, "-fd") != "--rx" && strings.TrimSuffix(flag, "-fd") != "--rw" {
				return nil, fmt.Errorf("unknown argument %q (%s)", a, usage)
			}
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", flag)
			}
			i++
			value = args[i]
		}
		if flag == "--nosymfollow" {
			if !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return nil, fmt.Errorf("%s %q: a clean absolute path", flag, value)
			}
			c.noSymlinks = append(c.noSymlinks, value)
			continue
		}
		var access Access
		byFD := strings.HasSuffix(flag, "-fd")
		switch strings.TrimSuffix(flag, "-fd") {
		case "--ro":
			access = ReadOnly
		case "--rx":
			access = ReadExec
		case "--rw":
			access = ReadWrite
		default:
			return nil, fmt.Errorf("unknown argument %q (%s)", a, usage)
		}
		r := rule{access: access, path: value, fd: -1}
		if byFD {
			n, err := strconv.Atoi(value)
			// 0, 1 and 2 are the program's standard streams
			if err != nil || n < 3 {
				return nil, fmt.Errorf("%s %q: a descriptor number from 3", flag, value)
			}
			r.fd, r.path = n, ""
		} else if !strings.HasPrefix(value, "/") {
			return nil, fmt.Errorf("%s %q: an absolute path", flag, value)
		}
		c.rules = append(c.rules, r)
	}
	return nil, errors.New("-- and the program are missing")
}

// Main is `monopanel sandbox-exec`: it builds the Landlock ruleset from its
// arguments, restricts itself and execs the program in its place with its
// environment as it is. It returns only when it fails, with ExitSetup,
// ExitCannotRun or ExitNotFound — or, with --check, once the sandbox is
// built, with 0.
//
// Handled rights that no rule grants are denied: below --ro trees the
// program reads files and lists directories, below --rx it also executes,
// below --rw it makes, writes and removes; outside them it reaches nothing it
// opens after the start — the descriptors it inherited stay what they are.
// Below a --nosymfollow directory it follows no symlink. The network is not
// restricted.
func Main(args []string) int {
	fail := func(code int, err error) int {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		return code
	}
	c, err := parse(args)
	if err != nil {
		return fail(ExitSetup, err)
	}
	abi, err := ABI()
	if err != nil {
		return fail(ExitSetup, err)
	}
	// Landlock, no_new_privs and the mount namespace apply to the thread
	// that asks, and execve hands that thread's to the program: everything
	// from here to the exec runs on one thread.
	runtime.LockOSThread()
	// The mounts first: Landlock refuses every mount to a thread it
	// restricts.
	if len(c.noSymlinks) > 0 {
		if err := noSymlinksBelow(c.noSymlinks); err != nil {
			return fail(ExitSetup, err)
		}
	}
	rs, err := newRuleset(abi)
	if err != nil {
		return fail(ExitSetup, err)
	}
	defer rs.close()
	for _, r := range c.rules {
		if err := addRule(rs, r); err != nil {
			return fail(ExitSetup, err)
		}
	}
	if err := rs.restrictSelf(); err != nil {
		return fail(ExitSetup, err)
	}
	if c.check {
		return 0
	}
	argv := c.argv
	err = unix.Exec(argv[0], argv, os.Environ())
	if errors.Is(err, unix.ENOENT) {
		return fail(ExitNotFound, fmt.Errorf("%s: %w", argv[0], err))
	}
	return fail(ExitCannotRun, fmt.Errorf("%s: %w", argv[0], err))
}

// addRule opens the rule's path — O_PATH, the last component not followed —
// or takes its inherited descriptor, and adds what it grants. A symlink is
// refused rather than resolved: a path the caller did not resolve itself is
// not one to grant whatever it points at. An inherited descriptor is closed
// once added, so that the program does not inherit it in turn.
func addRule(rs *ruleset, r rule) error {
	fd := r.fd
	if fd < 0 {
		var err error
		fd, err = unix.Open(r.path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("--%s %s: %w", r.access, r.path, err)
		}
	}
	defer unix.Close(fd) //nolint:errcheck // a descriptor only the rule needed
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("--%s %s: %w", r.access, r, err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("--%s %s: a symlink; name the file it points to", r.access, r)
	}
	if err := rs.add(fd, r.access.rights(st.Mode&unix.S_IFMT == unix.S_IFDIR, rs.handled)); err != nil {
		return fmt.Errorf("--%s %s: %w", r.access, r, err)
	}
	return nil
}
