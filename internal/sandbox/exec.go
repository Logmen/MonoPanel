package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	// Etc are the system's files and directories below /etc the program
	// reads (the account databases, name resolution, CA certificates): it
	// sees them in a private /etc that holds nothing else — a file as a
	// copy of what it is as the sandbox is built, a directory bound as it
	// is — which Landlock grants as one tree. A rule on a file holds its
	// inode, and the host replaces /etc/resolv.conf, /etc/hosts or
	// /etc/passwd by renaming another file over it (NetworkManager,
	// dhclient, useradd): the program would lose them halfway through a
	// run. See privateEtc.
	Etc []string
	// EtcBind are paths below /etc the program sees as they are — the
	// panel's configuration that a backup saves, a resolv.conf that is a
	// symlink into /run: bound into the private /etc, a file too, with its
	// owner, mode and times, and a symlink made again.
	EtcBind []string
	// NoLandlock builds the mounts alone — the view without symlinks, the
	// private /etc — on a kernel that has no Landlock: RO, RW, RX and the
	// descriptors are then refused, since nothing would enforce them.
	NoLandlock bool
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
	if r.NoLandlock {
		argv = append(argv, "--no-landlock")
	}
	for _, l := range []struct {
		flag  string
		paths []string
	}{{"--etc", r.Etc}, {"--etc-bind", r.EtcBind}, {"--nosymfollow", r.NoSymlinks}} {
		for _, p := range l.paths {
			argv = append(argv, l.flag, p)
		}
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
// symlinks, the private /etc, and the program with its arguments — or, with
// --check, none.
type cmdline struct {
	rules      []rule
	noSymlinks []string
	etc        []string
	etcBind    []string
	noLandlock bool
	check      bool
	argv       []string
}

// privateEtc tells whether the program gets a private /etc.
func (c *cmdline) privateEtc() bool { return len(c.etc)+len(c.etcBind) > 0 }

const usage = "usage: sandbox-exec [--no-landlock] [--etc|--etc-bind <path below /etc>]… [--nosymfollow <dir>]… [--ro|--rw|--rx <path>]… [--ro-fd|--rw-fd|--rx-fd <n>]… -- <program> [args…] | --check"

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
			return c, c.valid()
		case "--check":
			if i != len(args)-1 {
				return nil, errors.New("--check ends the command line: it runs no program")
			}
			c.check = true
			return c, c.valid()
		case "--no-landlock":
			c.noLandlock = true
			continue
		}
		flag, value, inline := strings.Cut(a, "=")
		if !inline {
			if flag != "--nosymfollow" && flag != "--etc" && flag != "--etc-bind" && strings.TrimSuffix(flag, "-fd") != "--ro" && strings.TrimSuffix(flag, "-fd") != "--rx" && strings.TrimSuffix(flag, "-fd") != "--rw" {
				return nil, fmt.Errorf("unknown argument %q (%s)", a, usage)
			}
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", flag)
			}
			i++
			value = args[i]
		}
		switch flag {
		case "--nosymfollow":
			if !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return nil, fmt.Errorf("%s %q: a clean absolute path", flag, value)
			}
			c.noSymlinks = append(c.noSymlinks, value)
			continue
		case "--etc", "--etc-bind":
			if filepath.Clean(value) != value || !strings.HasPrefix(value, etcDir+"/") {
				return nil, fmt.Errorf("%s %q: a clean path below %s", flag, value, etcDir)
			}
			if flag == "--etc" {
				c.etc = append(c.etc, value)
			} else {
				c.etcBind = append(c.etcBind, value)
			}
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

// valid refuses what cannot be built as asked: rules without Landlock to
// enforce them, and an entry of the private /etc below another one — a
// directory is bound as it is, and what is made for an entry below it would
// be made in the system's own /etc.
func (c *cmdline) valid() error {
	if c.noLandlock && len(c.rules) > 0 {
		return fmt.Errorf("--no-landlock builds no ruleset: --%s %s would not be enforced", c.rules[0].access, c.rules[0])
	}
	all := append(append([]string{}, c.etc...), c.etcBind...)
	for _, a := range all {
		for _, b := range all {
			if strings.HasPrefix(a, b+"/") {
				return fmt.Errorf("--etc %s lies below --etc %s: the private /etc takes no entry below another one", a, b)
			}
		}
	}
	// a view below /etc is made in the private /etc, of what is there
	for _, d := range c.noSymlinks {
		if !c.privateEtc() || !strings.HasPrefix(d, etcDir+"/") {
			continue
		}
		if !slices.ContainsFunc(all, func(e string) bool { return d == e || strings.HasPrefix(d, e+"/") }) {
			return fmt.Errorf("--nosymfollow %s: below %s, which is a private one, it lies in no --etc-bind entry", d, etcDir)
		}
	}
	return nil
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
// Below a --nosymfollow directory it follows no symlink. With --etc or
// --etc-bind its /etc is a private one, granted read-only. The network is
// not restricted.
//
// With --no-landlock only the mounts are made: the view without symlinks
// and the private /etc need a mount namespace, not Landlock, and a kernel
// that has none (or a boot that left it out) still gets them.
func Main(args []string) int {
	fail := func(code int, err error) int {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		return code
	}
	c, err := parse(args)
	if err != nil {
		return fail(ExitSetup, err)
	}
	abi := 0
	if !c.noLandlock {
		if abi, err = ABI(); err != nil {
			return fail(ExitSetup, err)
		}
	}
	// Landlock, no_new_privs and the mount namespace apply to the thread
	// that asks, and execve hands that thread's to the program: everything
	// from here to the exec runs on one thread.
	runtime.LockOSThread()
	// The mounts first: Landlock refuses every mount to a thread it
	// restricts.
	if c.privateEtc() || len(c.noSymlinks) > 0 {
		if err := buildMounts(c); err != nil {
			return fail(ExitSetup, err)
		}
	}
	if !c.noLandlock {
		rs, err := newRuleset(abi)
		if err != nil {
			return fail(ExitSetup, err)
		}
		defer rs.close()
		rules := c.rules
		if c.privateEtc() {
			// the private /etc is the program's own and holds only what it
			// was asked to: granted as one tree, its bind mounts with it
			rules = append([]rule{{access: ReadOnly, path: etcDir, fd: -1}}, rules...)
		}
		for _, r := range rules {
			if err := addRule(rs, r); err != nil {
				return fail(ExitSetup, err)
			}
		}
		if err := rs.restrictSelf(); err != nil {
			return fail(ExitSetup, err)
		}
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
