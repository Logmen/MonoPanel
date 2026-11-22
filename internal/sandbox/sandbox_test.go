package sandbox

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// childEnv makes the test binary the program the sandbox runs (child).
const childEnv = "MP_SANDBOX_TEST_CHILD"

// The test binary is both sandbox-exec and the program it runs: a Go program
// that tries what it is told and prints what came of it.
func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "sandbox-exec":
		os.Exit(Main(os.Args[2:]))
	case os.Getenv(childEnv) != "":
		os.Exit(child(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// child runs the operations of its arguments one after another and prints a
// line for each: "<op> <arg>: ok|denied|<error>".
func child(args []string) int {
	next := func() string {
		if len(args) == 0 {
			return ""
		}
		a := args[0]
		args = args[1:]
		return a
	}
	report := func(op, arg string, err error) {
		switch {
		case err == nil:
			fmt.Printf("%s %s: ok\n", op, arg)
		case errors.Is(err, fs.ErrPermission):
			fmt.Printf("%s %s: denied\n", op, arg)
		default:
			fmt.Printf("%s %s: %v\n", op, arg, err)
		}
	}
	stdin := bufio.NewReader(os.Stdin)
	for len(args) > 0 {
		switch op := next(); op {
		case "read":
			p := next()
			b, err := os.ReadFile(p)
			if err == nil {
				fmt.Printf("read %s: ok %s\n", p, strings.TrimSpace(string(b)))
				continue
			}
			report(op, p, err)
		case "list":
			p := next()
			_, err := os.ReadDir(p)
			report(op, p, err)
		case "readlink":
			p := next()
			target, err := os.Readlink(p)
			if err == nil {
				fmt.Printf("readlink %s: ok %s\n", p, target)
				continue
			}
			report(op, p, err)
		case "write":
			p := next()
			report(op, p, os.WriteFile(p, []byte("written"), 0o600))
		case "mkdir":
			p := next()
			report(op, p, os.Mkdir(p, 0o700))
		case "remove":
			p := next()
			report(op, p, os.Remove(p))
		case "exec":
			p := next()
			report(op, p, exec.Command(p).Run())
		case "connect":
			addr := next()
			c, err := net.Dial("tcp", addr)
			if err == nil {
				c.Close()
			}
			report(op, addr, err)
		case "fd":
			arg := next()
			n, _ := strconv.Atoi(arg)
			if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); errors.Is(err, unix.EBADF) {
				fmt.Printf("fd %s: closed\n", arg)
			} else {
				fmt.Printf("fd %s: open\n", arg)
			}
		case "wait":
			// the test does something meanwhile and says "go"
			fmt.Println("waiting")
			stdin.ReadString('\n') //nolint:errcheck // any line, or none, goes on
		case "nnp":
			v, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
			fmt.Printf("nnp: %d %v\n", v, err)
		case "env":
			env := os.Environ()
			slices.Sort(env)
			b, _ := json.Marshal(env)
			fmt.Printf("env: %s\n", b)
		case "argv":
			// the rest of the command line, as it came
			b, _ := json.Marshal(args)
			fmt.Printf("argv: %s\n", b)
			return 0
		case "exit":
			n, _ := strconv.Atoi(next())
			return n
		default:
			fmt.Printf("unknown op %q\n", op)
			return 99
		}
	}
	return 0
}

// needLandlock skips a test that runs the sandbox on a kernel without
// Landlock: it is not in the kernel or not in its lsm= list.
func needLandlock(t *testing.T) int {
	t.Helper()
	abi, err := ABI()
	if err != nil {
		lsm, _ := os.ReadFile("/sys/kernel/security/lsm")
		t.Skipf("Landlock is not available (%v; /sys/kernel/security/lsm: %q): nothing to test the sandbox on", err, strings.TrimSpace(string(lsm)))
	}
	return abi
}

// selfRules are what the test binary needs to run as the sandbox's program:
// the binary itself, and the libraries and the loader should it be linked
// dynamically (the race detector's runtime is).
func selfRules(t *testing.T) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--rx", self}
	for _, d := range []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64"} {
		if st, err := os.Lstat(d); err == nil && st.IsDir() {
			args = append(args, "--rx", d)
		}
	}
	return args
}

// sandboxed is the command that runs the test binary as the child with ops
// under rules.
func sandboxed(t *testing.T, rules []string, ops ...string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"sandbox-exec"}, rules...)
	args = append(args, selfRules(t)...)
	args = append(args, "--", self)
	cmd := exec.Command(self, append(args, ops...)...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	return cmd
}

// run runs the command and returns its output and exit status.
func run(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("%v: %v", cmd.Args, err)
	}
	return string(out), cmd.ProcessState.ExitCode()
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !slices.Contains(strings.Split(out, "\n"), l) {
			t.Errorf("no line %q in:\n%s", l, out)
		}
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A tree a program may read and the rest of the filesystem: a file inside
// reads, one outside does not, and neither does anything a symlink inside
// the tree leads to outside it — Landlock looks at the file the path
// resolves to.
func TestReadOnlyBelowTheTree(t *testing.T) {
	needLandlock(t)
	dir := t.TempDir()
	tree, outside := filepath.Join(dir, "tree"), filepath.Join(dir, "outside")
	writeFile(t, filepath.Join(tree, "sub", "inside"), "inside")
	writeFile(t, filepath.Join(outside, "secret"), "secret")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/inside", filepath.Join(tree, "near")); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, sandboxed(t, []string{"--ro", tree},
		"read", filepath.Join(tree, "sub", "inside"),
		"read", filepath.Join(tree, "near"),
		"list", filepath.Join(tree, "sub"),
		"read", filepath.Join(outside, "secret"),
		"list", outside,
		"read", filepath.Join(tree, "link"),
		"read", filepath.Join(tree, "dirlink", "secret"),
		"list", filepath.Join(tree, "dirlink"),
		"write", filepath.Join(tree, "sub", "new"),
		"write", filepath.Join(tree, "sub", "inside"),
	))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	wantLines(t, out,
		"read "+filepath.Join(tree, "sub", "inside")+": ok inside",
		"read "+filepath.Join(tree, "near")+": ok inside",
		"list "+filepath.Join(tree, "sub")+": ok",
		"read "+filepath.Join(outside, "secret")+": denied",
		"list "+outside+": denied",
		"read "+filepath.Join(tree, "link")+": denied",
		"read "+filepath.Join(tree, "dirlink", "secret")+": denied",
		"list "+filepath.Join(tree, "dirlink")+": denied",
		"write "+filepath.Join(tree, "sub", "new")+": denied",
		"write "+filepath.Join(tree, "sub", "inside")+": denied",
	)
	if b, _ := os.ReadFile(filepath.Join(tree, "sub", "inside")); string(b) != "inside" {
		t.Fatalf("the file changed: %q", b)
	}
}

// The race the sandbox is for: the program lists a directory of the tree,
// its owner swaps the directory for a symlink to the outside, and the
// program opens a name it listed. Outside the sandbox the open follows the
// link to the secret; inside it is refused.
func TestDirectorySwappedForASymlink(t *testing.T) {
	needLandlock(t)
	for _, confined := range []bool{false, true} {
		dir := t.TempDir()
		tree, outside := filepath.Join(dir, "home"), filepath.Join(dir, "etc")
		writeFile(t, filepath.Join(tree, "a", "shadow"), "the account's own")
		writeFile(t, filepath.Join(outside, "shadow"), "root:hash")
		ops := []string{"list", filepath.Join(tree, "a"), "wait", "read", filepath.Join(tree, "a", "shadow")}
		var cmd *exec.Cmd
		if confined {
			cmd = sandboxed(t, []string{"--ro", tree}, ops...)
		} else {
			self, _ := os.Executable()
			cmd = exec.Command(self, ops...)
			cmd.Env = append(os.Environ(), childEnv+"=1")
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(stdout)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
			if sc.Text() == "waiting" {
				break
			}
		}
		// the swap, between the listing and the open
		if err := os.Rename(filepath.Join(tree, "a"), filepath.Join(tree, "a.old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(tree, "a")); err != nil {
			t.Fatal(err)
		}
		io.WriteString(stdin, "go\n")
		stdin.Close()
		rest, _ := io.ReadAll(stdout)
		cmd.Wait()
		out := strings.Join(lines, "\n") + "\n" + string(rest)
		want := "read " + filepath.Join(tree, "a", "shadow") + ": denied"
		if !confined {
			want = "read " + filepath.Join(tree, "a", "shadow") + ": ok root:hash"
		}
		wantLines(t, out, "list "+filepath.Join(tree, "a")+": ok", want)
	}
}

// Writes go only below --rw: there the program makes, writes and removes
// files and directories; in a --ro tree and outside it does not.
func TestWritesOnlyBelowRW(t *testing.T) {
	needLandlock(t)
	dir := t.TempDir()
	rw, ro, outside := filepath.Join(dir, "rw"), filepath.Join(dir, "ro"), filepath.Join(dir, "outside")
	writeFile(t, filepath.Join(rw, "old"), "old")
	writeFile(t, filepath.Join(ro, "file"), "ro")
	writeFile(t, filepath.Join(outside, "file"), "outside")
	devNull := []string{"--rw", "/dev/null"}
	out, code := run(t, sandboxed(t, append([]string{"--rw", rw, "--ro", ro}, devNull...),
		"write", filepath.Join(rw, "new"),
		"mkdir", filepath.Join(rw, "d"),
		"write", filepath.Join(rw, "d", "f"),
		"remove", filepath.Join(rw, "old"),
		"read", filepath.Join(rw, "new"),
		"write", filepath.Join(ro, "new"),
		"write", filepath.Join(ro, "file"),
		"remove", filepath.Join(ro, "file"),
		"write", filepath.Join(outside, "new"),
		"mkdir", filepath.Join(outside, "d"),
		"write", "/dev/null",
	))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	wantLines(t, out,
		"write "+filepath.Join(rw, "new")+": ok",
		"mkdir "+filepath.Join(rw, "d")+": ok",
		"write "+filepath.Join(rw, "d", "f")+": ok",
		"remove "+filepath.Join(rw, "old")+": ok",
		"read "+filepath.Join(rw, "new")+": ok written",
		"write "+filepath.Join(ro, "new")+": denied",
		"write "+filepath.Join(ro, "file")+": denied",
		"remove "+filepath.Join(ro, "file")+": denied",
		"write "+filepath.Join(outside, "new")+": denied",
		"mkdir "+filepath.Join(outside, "d")+": denied",
		"write /dev/null: ok",
	)
	for _, p := range []string{filepath.Join(ro, "new"), filepath.Join(outside, "new"), filepath.Join(outside, "d")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was made", p)
		}
	}
}

// A program runs only from --rx: a copy of it in a --ro or --rw tree, or
// anywhere else, does not; and sandbox-exec says so with its exit status
// when the program itself is not in an --rx tree.
func TestExecOnlyBelowRX(t *testing.T) {
	needLandlock(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	progs := map[string]string{}
	for _, d := range []string{"rx", "ro", "rw", "outside"} {
		p := filepath.Join(dir, d, "prog")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		progs[d] = p
	}
	out, code := run(t, sandboxed(t, []string{"--rx", filepath.Join(dir, "rx"), "--ro", filepath.Join(dir, "ro"), "--rw", filepath.Join(dir, "rw"), "--rw", "/dev/null"},
		"exec", progs["rx"], "exec", progs["ro"], "exec", progs["rw"], "exec", progs["outside"]))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	wantLines(t, out, "exec "+progs["rx"]+": ok", "exec "+progs["ro"]+": denied", "exec "+progs["rw"]+": denied", "exec "+progs["outside"]+": denied")

	// sandbox-exec itself runs a program outside --rx no more than the
	// program does
	cmd := exec.Command(self, "sandbox-exec", "--ro", filepath.Join(dir, "outside"), "--", progs["outside"])
	cmd.Env = append(os.Environ(), childEnv+"=1")
	if out, code := run(t, cmd); code != ExitCannotRun || !strings.Contains(out, "permission denied") {
		t.Fatalf("a program outside --rx: exit %d: %s", code, out)
	}
	cmd = exec.Command(self, "sandbox-exec", "--rx", dir, "--", filepath.Join(dir, "missing"))
	if out, code := run(t, cmd); code != ExitNotFound {
		t.Fatalf("a missing program: exit %d: %s", code, out)
	}
}

// The program gets its arguments and the environment exactly as given, its
// exit status is the sandbox's, it runs with no_new_privs, and the network
// stays open to it.
func TestArgvEnvExitAndNetwork(t *testing.T) {
	needLandlock(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	cmd := sandboxed(t, nil, "nnp", "connect", ln.Addr().String(), "env", "argv", "a b", "", "--ro", "x=y", "--", "ü")
	cmd.Env = append(cmd.Env, "MP_TEST_VALUE=two words", "MP_TEST_EMPTY=")
	out, code := run(t, cmd)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	// the environment as exec hands it over: the last of a name wins
	last := map[string]string{}
	for _, e := range cmd.Env {
		k, _, _ := strings.Cut(e, "=")
		last[k] = e
	}
	env := []string{}
	for _, e := range last {
		env = append(env, e)
	}
	slices.Sort(env)
	wantEnv, _ := json.Marshal(env)
	wantArgv, _ := json.Marshal([]string{"a b", "", "--ro", "x=y", "--", "ü"})
	wantLines(t, out, "nnp: 1 <nil>", "connect "+ln.Addr().String()+": ok", "env: "+string(wantEnv), "argv: "+string(wantArgv))

	for _, n := range []int{0, 3, 42} {
		if out, code := run(t, sandboxed(t, nil, "exit", strconv.Itoa(n))); code != n {
			t.Errorf("exit %d came back as %d: %s", n, code, out)
		}
	}
}

// A tree may come as a descriptor the caller opened: what it refers to is
// granted, and the program does not inherit the descriptor.
func TestRuleByDescriptor(t *testing.T) {
	needLandlock(t)
	dir := t.TempDir()
	tree, outside := filepath.Join(dir, "tree"), filepath.Join(dir, "outside")
	writeFile(t, filepath.Join(tree, "inside"), "inside")
	writeFile(t, filepath.Join(outside, "secret"), "secret")
	fd, err := unix.Open(tree, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), tree)
	defer f.Close()
	rules := Rules{ROFiles: []*os.File{f}}
	argv, files := rules.Command("/x", nil)
	if strings.Join(argv, " ") != "sandbox-exec --ro-fd 3 -- /x" || len(files) != 1 || files[0] != f {
		t.Fatalf("command %q %v", argv, files)
	}
	// the descriptor is looked at first: the program's own opens reuse
	// the lowest free number
	cmd := sandboxed(t, []string{"--ro-fd", "3"}, "fd", "3", "read", filepath.Join(tree, "inside"), "read", filepath.Join(outside, "secret"))
	cmd.ExtraFiles = files
	out, code := run(t, cmd)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	wantLines(t, out, "read "+filepath.Join(tree, "inside")+": ok inside", "read "+filepath.Join(outside, "secret")+": denied", "fd 3: closed")
}

// A rule is refused, and nothing runs, when it is a symlink, missing,
// relative, a standard stream, or unknown; so is a command line without the
// program after "--".
func TestBadRules(t *testing.T) {
	needLandlock(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real", "f"), "x")
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--ro", filepath.Join(dir, "link"), "--", self}, "a symlink"},
		{[]string{"--ro", filepath.Join(dir, "missing"), "--", self}, "no such file"},
		{[]string{"--ro", "relative", "--", self}, "an absolute path"},
		{[]string{"--ro-fd", "1", "--", self}, "a descriptor number from 3"},
		{[]string{"--ro-fd", "1000", "--", self}, "bad file descriptor"},
		{[]string{"--rwx", dir, "--", self}, "unknown argument"},
		{[]string{"--ro"}, "needs a value"},
		{[]string{"--ro", dir}, "-- and the program are missing"},
		{[]string{"--ro", dir, "--"}, "an absolute path"},
		{[]string{"--ro", dir, "--", "prog"}, "an absolute path"},
		{[]string{"--nosymfollow", "relative", "--", self}, "a clean absolute path"},
		{[]string{"--nosymfollow", dir + "/../x", "--", self}, "a clean absolute path"},
		{[]string{"--nosymfollow"}, "needs a value"},
		{[]string{"--check", "--", self}, "--check ends the command line"},
	} {
		cmd := exec.Command(self, append([]string{"sandbox-exec"}, c.args...)...)
		cmd.Env = append(os.Environ(), childEnv+"=1")
		out, code := run(t, cmd)
		if code != ExitSetup || !strings.Contains(out, c.says) {
			t.Errorf("%q: exit %d, want %d saying %q: %s", c.args, code, ExitSetup, c.says, out)
		}
	}
}

// The rights a ruleset handles follow the kernel's ABI version, and a rule
// grants within them: read on --ro, read and execute on --rx, everything but
// execute on --rw, and a file only the rights a file can have.
func TestRights(t *testing.T) {
	v1 := uint64(rightsV1)
	for abi, want := range map[int]uint64{
		1: v1,
		2: v1 | unix.LANDLOCK_ACCESS_FS_REFER,
		3: v1 | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE,
		4: v1 | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE,
		5: v1 | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV,
		8: v1 | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV,
	} {
		if got := Handled(abi); got != want {
			t.Errorf("ABI %d: handled %#x, want %#x", abi, got, want)
		}
	}
	h := Handled(5)
	for _, c := range []struct {
		access Access
		dir    bool
		want   uint64
	}{
		{ReadOnly, true, unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR},
		{ReadOnly, false, unix.LANDLOCK_ACCESS_FS_READ_FILE},
		{ReadExec, true, unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_EXECUTE},
		{ReadExec, false, unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_EXECUTE},
		{ReadWrite, true, h &^ unix.LANDLOCK_ACCESS_FS_EXECUTE},
		{ReadWrite, false, unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV},
	} {
		if got := c.access.rights(c.dir, h); got != c.want {
			t.Errorf("%s dir=%v: %#x, want %#x", c.access, c.dir, got, c.want)
		}
	}
	// a right the kernel does not handle is not asked for
	if got := ReadWrite.rights(true, Handled(1)); got&(unix.LANDLOCK_ACCESS_FS_REFER|unix.LANDLOCK_ACCESS_FS_TRUNCATE) != 0 {
		t.Errorf("ABI 1 rw: %#x", got)
	}
}

// Command lays the rules out for sandbox-exec: paths, then the descriptors
// numbered from 3 in the order of the files it returns.
func TestCommand(t *testing.T) {
	a, b := os.NewFile(100, "a"), os.NewFile(101, "b")
	r := Rules{RO: []string{"/etc/passwd"}, RW: []string{"/var/cache/x"}, RX: []string{"/usr/bin/restic"}, ROFiles: []*os.File{a}, RWFiles: []*os.File{b}, NoSymlinks: []string{"/var/www"}}
	argv, files := r.Command("/usr/bin/restic", []string{"backup", "--", "/x", "--check"})
	want := "sandbox-exec --nosymfollow /var/www --ro /etc/passwd --rw /var/cache/x --rx /usr/bin/restic --ro-fd 3 --rw-fd 4 -- /usr/bin/restic backup -- /x --check"
	if strings.Join(argv, " ") != want || len(files) != 2 || files[0] != a || files[1] != b {
		t.Fatalf("%q %v", argv, files)
	}
	c, err := parse(argv[1:])
	if err != nil || len(c.rules) != 5 || c.check || !slices.Equal(c.noSymlinks, []string{"/var/www"}) || strings.Join(c.argv, " ") != "/usr/bin/restic backup -- /x --check" {
		t.Fatalf("parse: %+v %v", c, err)
	}
	if c.rules[3].fd != 3 || c.rules[3].access != ReadOnly || c.rules[4].fd != 4 || c.rules[4].access != ReadWrite {
		t.Fatalf("descriptors: %+v", c.rules)
	}
	// the check builds the same sandbox and runs nothing
	argv, files = r.CheckCommand()
	if strings.Join(argv, " ") != "sandbox-exec --nosymfollow /var/www --ro /etc/passwd --rw /var/cache/x --rx /usr/bin/restic --ro-fd 3 --rw-fd 4 --check" || len(files) != 2 {
		t.Fatalf("check: %q %v", argv, files)
	}
	if c, err := parse(argv[1:]); err != nil || !c.check || c.argv != nil || len(c.rules) != 5 || len(c.noSymlinks) != 1 {
		t.Fatalf("parse --check: %+v %v", c, err)
	}
}

// inUserNS has cmd start as root of a user namespace of its own (the test's
// uid and gid mapped to 0): that is what it takes to mount, as the agent,
// which is root, may — the test is not. It skips the test where the kernel
// or its settings refuse such namespaces.
func inUserNS(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	attr := func() *syscall.SysProcAttr {
		return &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
	}
	probe := exec.Command("/bin/sh", "-c", "exit 0")
	probe.SysProcAttr = attr()
	if err := probe.Run(); err != nil {
		t.Skipf("no user namespace for the test (%v): mounting needs one", err)
	}
	cmd.SysProcAttr = attr()
}

// raceRun starts cmd, whose ops include "wait", does swap once the program
// waits, lets it go on and returns all it printed.
func raceRun(t *testing.T, cmd *exec.Cmd, swap func()) string {
	t.Helper()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "waiting" {
			break
		}
	}
	swap()
	io.WriteString(stdin, "go\n") //nolint:errcheck // a program that is gone says so below
	stdin.Close()
	rest, _ := io.ReadAll(stdout)
	cmd.Wait() //nolint:errcheck // the output is what counts
	return strings.Join(lines, "\n") + "\n" + string(rest) + stderr.String()
}

// Landlock grants a tree however it is reached: a backup that reads a home
// and the panel's configuration reads the configuration through a symlink
// the account swapped in for a directory of its home as well, and stores it
// as the account's file. Below --nosymfollow no symlink is followed — not on
// the way, not at the end — while the program still sees a link as a link,
// and one outside that tree is followed as before.
func TestNoSymlinksBelow(t *testing.T) {
	needLandlock(t)
	for _, nosym := range []bool{false, true} {
		dir := t.TempDir()
		www, conf := filepath.Join(dir, "www"), filepath.Join(dir, "etc")
		home := filepath.Join(www, "alex")
		writeFile(t, filepath.Join(home, "a", "secret.key"), "the account's own")
		writeFile(t, filepath.Join(home, "file"), "a file")
		writeFile(t, filepath.Join(conf, "secret.key"), "panel-secret")
		writeFile(t, filepath.Join(conf, "real", "f"), "conf file")
		if err := os.Symlink("file", filepath.Join(home, "near")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(conf, "link")); err != nil {
			t.Fatal(err)
		}
		rules := []string{"--ro", www, "--ro", conf}
		if nosym {
			rules = append([]string{"--nosymfollow", www}, rules...)
		}
		cmd := sandboxed(t, rules, "list", filepath.Join(home, "a"), "wait",
			"read", filepath.Join(home, "a", "secret.key"),
			"readlink", filepath.Join(home, "near"),
			"read", filepath.Join(home, "near"),
			"read", filepath.Join(home, "file"),
			"read", filepath.Join(conf, "link", "f"))
		inUserNS(t, cmd)
		out := raceRun(t, cmd, func() {
			if err := os.Rename(filepath.Join(home, "a"), filepath.Join(home, "a.old")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(conf, filepath.Join(home, "a")); err != nil {
				t.Fatal(err)
			}
		})
		stolen := "read " + filepath.Join(home, "a", "secret.key") + ": ok panel-secret"
		if !nosym {
			// what Landlock alone lets through
			wantLines(t, out, stolen, "read "+filepath.Join(home, "near")+": ok a file")
			continue
		}
		if strings.Contains(out, stolen) {
			t.Errorf("the configuration was read through the account's symlink:\n%s", out)
		}
		wantLines(t, out,
			"list "+filepath.Join(home, "a")+": ok",
			"read "+filepath.Join(home, "a", "secret.key")+": open "+filepath.Join(home, "a", "secret.key")+": too many levels of symbolic links",
			"readlink "+filepath.Join(home, "near")+": ok file",
			"read "+filepath.Join(home, "near")+": open "+filepath.Join(home, "near")+": too many levels of symbolic links",
			"read "+filepath.Join(home, "file")+": ok a file",
			"read "+filepath.Join(conf, "link", "f")+": ok conf file",
		)
	}
}

// --check builds the sandbox, the view without symlinks included, and runs
// nothing: the agent's way to learn, once, whether it can. Without the right
// to mount it fails as a run would.
func TestCheck(t *testing.T) {
	needLandlock(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(self, "sandbox-exec", "--nosymfollow", dir, "--ro", dir, "--check")
	inUserNS(t, cmd)
	if out, code := run(t, cmd); code != 0 || out != "" {
		t.Fatalf("check: exit %d: %s", code, out)
	}
	if os.Geteuid() != 0 {
		cmd = exec.Command(self, "sandbox-exec", "--nosymfollow", dir, "--check")
		if out, code := run(t, cmd); code != ExitSetup || !strings.Contains(out, "--nosymfollow: a mount namespace") {
			t.Fatalf("check without the right to mount: exit %d: %s", code, out)
		}
	}
}
