package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/sandbox"
)

// fakeResticEnv makes the test binary a restic of its own (fakeRestic): the
// agent runs restic through sandbox-exec, and the test binary is both.
const fakeResticEnv = "MP_TEST_FAKE_RESTIC"

func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "sandbox-exec":
		os.Exit(sandbox.Main(os.Args[2:]))
	case os.Getenv(fakeResticEnv) != "":
		os.Exit(fakeResticMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeResticMain does what the test asks of restic and prints what came of
// it: a backup reads every file below the paths it backs up (a walk that
// follows no symlink, as restic's), then the paths of MP_FAKE_RESTIC_READ —
// what restic opens when a directory it walks became a symlink meanwhile —
// and writes the files of MP_FAKE_RESTIC_WRITE (its cache, its temporary
// directory, the repository, and where it must not).
func fakeResticMain(args []string) int {
	report := func(op, p string, err error) {
		switch {
		case err == nil:
			fmt.Printf("%s %s: ok\n", op, p)
		case errors.Is(err, fs.ErrPermission):
			fmt.Printf("%s %s: denied\n", op, p)
		default:
			fmt.Printf("%s %s: %v\n", op, p, err)
		}
	}
	read := func(p string) {
		b, err := os.ReadFile(p)
		if err == nil {
			fmt.Printf("read %s: ok %s\n", p, strings.TrimSpace(string(b)))
			return
		}
		report("read", p, err)
	}
	fmt.Printf("argv %s\n", strings.Join(args, " "))
	for _, k := range []string{"TMPDIR", "RESTIC_CACHE_DIR", "RESTIC_REPOSITORY", "RESTIC_PASSWORD"} {
		fmt.Printf("env %s=%s\n", k, os.Getenv(k))
	}
	for len(args) > 1 && args[0] == "-o" {
		if cmd, ok := strings.CutPrefix(args[1], "sftp.command="); ok {
			fakeSFTP(strings.Fields(cmd))
		}
		args = args[2:]
	}
	if len(args) > 6 && args[0] == "backup" {
		for _, root := range args[6:] {
			filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // every error is printed
				if err != nil {
					report("walk", p, err)
					return nil
				}
				if d.Type().IsRegular() {
					read(p)
				}
				return nil
			})
		}
	}
	for _, p := range strings.Fields(os.Getenv("MP_FAKE_RESTIC_READ")) {
		read(p)
	}
	if os.Getenv("MP_FAKE_RESTIC_SYSTEM") != "" {
		fmt.Println(systemProbe())
	}
	for _, p := range strings.Fields(os.Getenv("MP_FAKE_RESTIC_WRITE")) {
		report("write", p, os.WriteFile(p, []byte("restic"), 0o600))
	}
	if len(args) > 0 && args[0] == "backup" {
		fmt.Println(`{"message_type":"summary","snapshot_id":"0123456789abcdef0123456789abcdef","total_files_processed":1}`)
	}
	n, _ := strconv.Atoi(os.Getenv("MP_FAKE_RESTIC_EXIT"))
	return n
}

// caBundles are where Go's crypto/x509 looks for the CA certificates on
// Linux, in its order.
var caBundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "/etc/ssl/cert.pem"}

// systemProbe is what restic needs of the system, as a line: the CA
// certificates of an s3, b2 or rest repository, a name resolved, an owner's
// name, the time zone of the snapshot's time.
func systemProbe() string {
	certs := "no bundle"
	for _, p := range caBundles {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		b, err := os.ReadFile(p)
		switch {
		case err != nil:
			certs = err.Error()
		case bytes.Contains(b, []byte("BEGIN CERTIFICATE")):
			certs = "ok"
		}
		break
	}
	resolved := "ok"
	if _, err := net.DefaultResolver.LookupHost(context.Background(), "localhost"); err != nil {
		resolved = err.Error()
	}
	owner := "?"
	if u, err := user.LookupId("0"); err == nil {
		owner = u.Username
	}
	zone, offset := time.Now().Zone()
	return fmt.Sprintf("system: certs %s, localhost %s, uid 0 %s, zone %s%+d", certs, resolved, owner, zone, offset)
}

// fakeSFTP runs restic's sftp.command and says hello in SFTP: INIT, version
// 3, to which the server answers VERSION.
func fakeSFTP(argv []string) {
	cmd := exec.Command(argv[0], argv[1:]...)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		fmt.Printf("sftp: %v\n", err)
		return
	}
	in.Write([]byte{0, 0, 0, 5, 1, 0, 0, 0, 3}) //nolint:errcheck // what fails shows below
	reply := make([]byte, 9)
	_, err := io.ReadFull(out, reply)
	in.Close()
	cmd.Wait() //nolint:errcheck // the reply is what counts
	if err != nil || reply[4] != 2 {
		fmt.Printf("sftp: %v %q %s\n", err, reply, strings.TrimSpace(stderr.String()))
		return
	}
	fmt.Printf("sftp: version %d\n", reply[8])
}

// sandboxServer is a policy server whose restic is the test binary and
// whose restic directories are the test's; landlock is the ABI it runs
// restic's sandbox with (0: none).
func sandboxServer(t *testing.T, landlock int) (*Server, string) {
	t.Helper()
	s := policyServer(t, debian13)
	withAccounts(t, standardAccounts(s)...)
	s.landlock = landlock
	dir := t.TempDir()
	s.resticCache, s.resticTmp = filepath.Join(dir, "cache"), filepath.Join(dir, "tmp")
	mkdirs(t, s.resticCache, s.resticTmp)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin", "restic")
	mkdirs(t, filepath.Dir(bin))
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	old := toolBinaries["restic"]
	toolBinaries["restic"] = []string{bin}
	t.Cleanup(func() { toolBinaries["restic"] = old })
	return s, bin
}

// needLandlock skips a test that runs restic's sandbox on a kernel without
// Landlock (or with it left out of the lsm= list).
func needLandlock(t *testing.T) int {
	t.Helper()
	abi, err := sandbox.ABI()
	if err != nil {
		lsm, _ := os.ReadFile("/sys/kernel/security/lsm")
		t.Skipf("Landlock is not available (%v; /sys/kernel/security/lsm: %q)", err, strings.TrimSpace(string(lsm)))
	}
	return abi
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	have := strings.Split(out, "\n")
	for _, l := range lines {
		if !slices.Contains(have, l) {
			t.Errorf("no line %q in:\n%s", l, out)
		}
	}
}

// The attack and what the sandbox makes of it. alex's home holds a
// directory that alex swapped for a symlink to /etc while the backup was in
// it: the paths restic opens there now lead to /etc/shadow. Through the
// agent's plan restic reads alex's files, writes its cache, its temporary
// directory and the repository — and reads nothing through the link, and
// writes nothing anywhere else.
func TestResticBackupRunsInTheSandbox(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	etc := t.TempDir()
	repo := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, filepath.Join(home, "data", "www"), repo)
	writeFileT(t, filepath.Join(home, "data", "www", "index.php"), "<?php")
	writeFileT(t, filepath.Join(etc, "shadow"), "root:hash")
	if err := os.Symlink(etc, filepath.Join(home, "a")); err != nil {
		t.Fatal(err)
	}
	call := withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD=pw")
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	if plan.confine == nil || len(plan.closers) != 1 {
		t.Fatalf("a backup must run in the sandbox with its home pinned: confine %v, %d pinned", plan.confine != nil, len(plan.closers))
	}
	t.Setenv(fakeResticEnv, "1")
	t.Setenv("MP_FAKE_RESTIC_READ", filepath.Join(home, "a", "shadow")+" "+filepath.Join(etc, "shadow"))
	t.Setenv("MP_FAKE_RESTIC_WRITE", strings.Join([]string{filepath.Join(s.resticCache, "c"), filepath.Join(s.resticTmp, "t"), filepath.Join(repo, "config"), filepath.Join(home, "x"), filepath.Join(etc, "cron")}, " "))
	t.Setenv("MP_FAKE_RESTIC_EXIT", "3")
	t.Setenv("MP_FAKE_RESTIC_SYSTEM", "1")
	var out bytes.Buffer
	code, err := planRunner(context.Background(), plan, bin, "", &out)(plan.args, nil)
	if err != nil || code != 3 {
		t.Fatalf("restic: %d %v\n%s", code, err, out.String())
	}
	wantLines(t, out.String(),
		// what restic reads of the system it reads in the sandbox as well
		systemProbe(),
		"argv backup --json --tag monopanel --tag scope=user:alex "+home,
		"env TMPDIR="+s.resticTmp,
		"env RESTIC_CACHE_DIR="+s.resticCache,
		"env RESTIC_REPOSITORY="+repo,
		"env RESTIC_PASSWORD=pw",
		"read "+filepath.Join(home, "data", "www", "index.php")+": ok <?php",
		"read "+filepath.Join(home, "a", "shadow")+": denied",
		"read "+filepath.Join(etc, "shadow")+": denied",
		"write "+filepath.Join(s.resticCache, "c")+": ok",
		"write "+filepath.Join(s.resticTmp, "t")+": ok",
		"write "+filepath.Join(repo, "config")+": ok",
		"write "+filepath.Join(home, "x")+": denied",
		"write "+filepath.Join(etc, "cron")+": denied",
	)
	for _, p := range []string{filepath.Join(home, "x"), filepath.Join(etc, "cron")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}

	// Without Landlock restic runs as it is: what the sandbox stops, a
	// plain run does — the race this closes.
	s.landlock, s.landlockErr = 0, unix.ENOSYS
	plan, err = s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	if plan.confine != nil || len(plan.closers) != 0 {
		t.Fatal("without Landlock restic runs as before")
	}
	out.Reset()
	if code, err := planRunner(context.Background(), plan, bin, "", &out)(plan.args, nil); err != nil || code != 3 {
		t.Fatalf("restic without the sandbox: %d %v\n%s", code, err, out.String())
	}
	wantLines(t, out.String(), "read "+filepath.Join(home, "a", "shadow")+": ok root:hash", "env TMPDIR="+os.Getenv("TMPDIR"))
}

func writeFileT(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The rule list of each restic the panel runs: the system's files, the
// cache and temporary directory, /dev/null and the binary for all of them;
// the backed-up paths for a backup alone; the target below RestoreDir for a
// restore; a local repository written to; for sftp the ssh binary and no
// key, configuration or known hosts of root's — the master has those.
func TestResticRulesPerPlan(t *testing.T) {
	s, bin := sandboxServer(t, 5)
	local := filepath.Join(t.TempDir(), "repo")
	sftp := "sftp://backup@backup.example.com:2222//srv/restic"
	s3 := "s3:https://s3.amazonaws.com/bucket/monopanel"
	aside := filepath.Join(RestoreDir, "1a2b3c4d")
	staging := filepath.Join(RestoreDir, ".in-place-0123456789abcdef")
	for _, c := range []struct {
		name    string
		args    []string
		repo    string
		sources bool
		target  string
		sftp    bool
	}{
		{"backup local", []string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=server", "/var/www"}, local, true, "", false},
		{"backup sftp", []string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", "/var/www/alex"}, sftp, true, "", true},
		{"backup s3", []string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=db:alex_wp", "/x.sql"}, s3, true, "", false},
		{"restore aside", []string{"restore", strings.Repeat("ab", 32), "--target", aside, "--include", "/var/www/alex"}, local, false, aside, false},
		{"restore in place", []string{"restore", strings.Repeat("ab", 32), "--target", staging, "--include", "/var/www/alex"}, sftp, false, staging, true},
		{"ls", []string{"ls", "--json", "latest"}, s3, false, "", false},
		{"snapshots", []string{"snapshots", "--json", "--tag", "monopanel"}, local, false, "", false},
		{"cat config", []string{"cat", "config"}, sftp, false, "", true},
		{"init", []string{"init"}, local, false, "", false},
		{"forget", []string{"forget", "--prune", "--tag", "monopanel", "--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "3"}, s3, false, "", false},
	} {
		acc, err := s.resticRules(bin, c.args, c.repo)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		self, _ := filepath.EvalSymlinks(bin)
		for _, want := range []struct {
			list []string
			p    string
		}{{acc.rw, s.resticCache}, {acc.rw, s.resticTmp}, {acc.rw, "/dev/null"}, {acc.rx, self}, {acc.ro, "/etc/passwd"}} {
			if !slices.Contains(want.list, want.p) {
				t.Errorf("%s: %s missing", c.name, want.p)
			}
		}
		if acc.sources != c.sources || acc.target != c.target || (acc.sftp != nil) != c.sftp {
			t.Errorf("%s: sources %v target %q sftp %v", c.name, acc.sources, acc.target, acc.sftp)
		}
		if want := ""; strings.HasPrefix(c.repo, "/") {
			if acc.repo != c.repo {
				t.Errorf("%s: repository %q", c.name, acc.repo)
			}
		} else if acc.repo != want {
			t.Errorf("%s: a remote repository is no local one: %q", c.name, acc.repo)
		}
		if c.sftp {
			if r := acc.sftp; r.user != "backup" || r.host != "backup.example.com" || r.port != "2222" {
				t.Errorf("%s: the server %+v", c.name, *r)
			}
			if ssh, err := filepath.EvalSymlinks(sshBinaries[0]); err == nil && !slices.Contains(acc.rx, ssh) {
				t.Errorf("%s: ssh is not executable: %q", c.name, acc.rx)
			}
		}
		// what restic must never reach, whatever a home points at
		for _, list := range [][]string{acc.ro, acc.rw, acc.rx} {
			for _, p := range list {
				for _, never := range []string{"/", "/etc", "/etc/shadow", "/etc/ssh", "/etc/ssl", "/etc/ssl/private", "/etc/pki", "/etc/pki/tls/private", "/root", "/etc/monopanel", "/var/lib/monopanel-agent", "/var/lib/monopanel", "/var/www", "/home", "/usr", "/usr/local", "/proc", "/sys", "/tmp"} {
					if p == never || below(never, p) {
						t.Errorf("%s: %s grants %s", c.name, p, never)
					}
				}
			}
		}
		for _, p := range acc.ro {
			if st, err := os.Lstat(p); err != nil || st.Mode()&os.ModeSymlink != 0 {
				t.Errorf("%s: %s is no resolved path of the system's (%v)", c.name, p, err)
			}
		}
	}
	// a restore writes below RestoreDir alone; a command the panel never
	// runs has no rules at all
	for _, args := range [][]string{
		{"restore", "latest", "--target", "/"},
		{"restore", "latest", "--target", RestoreDir},
		{"restore", "latest", "--target", RestoreDir + "/../../etc"},
		{"restore", "latest"},
		{"mount", "/mnt"},
		{},
	} {
		if _, err := s.resticRules(bin, args, local); err == nil {
			t.Errorf("%q: rules", args)
		}
	}
}

// What runs in the sandbox and how: sandbox-exec of the agent's own binary,
// the pinned paths handed over as descriptors, restic's arguments after
// "--" as the policy accepted them, its temporary directory in TMPDIR. A
// restore stays out of the sandbox on ABI 1, which cannot let restic link
// a snapshot's hard links between directories; a backup does not.
func TestResticPlanThroughTheSandbox(t *testing.T) {
	for _, abi := range []int{1, 2, 5} {
		s, bin := sandboxServer(t, abi)
		home := filepath.Join(s.cfg.WWWRoot, "alex")
		mkdirs(t, home)
		env := []string{"RESTIC_REPOSITORY=s3:https://s3.example.com/b", "RESTIC_PASSWORD=pw", "AWS_ACCESS_KEY_ID=k", "AWS_SECRET_ACCESS_KEY=s"}
		call := withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home), env...)
		plan, err := s.planTool(&call)
		if err != nil {
			t.Fatal(err)
		}
		c, err := plan.confine(context.Background(), bin, plan.args, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("ABI %d: %v", abi, err)
		}
		cmd := strings.Join(c.args, " ")
		if c.path != "/proc/self/exe" || !strings.HasPrefix(cmd, "sandbox-exec ") || !strings.Contains(cmd, " --ro-fd 3 ") ||
			!strings.HasSuffix(cmd, " -- "+bin+" backup --json --tag monopanel --tag scope=user:alex "+home) ||
			len(c.files) != 1 || !slices.Equal(c.env, []string{"TMPDIR=" + s.resticTmp}) {
			t.Fatalf("ABI %d: %s %q files %d env %q", abi, c.path, c.args, len(c.files), c.env)
		}
		var st, want unix.Stat_t
		if err := unix.Fstat(int(c.files[0].Fd()), &st); err != nil || unix.Stat(home, &want) != nil || st.Ino != want.Ino {
			t.Fatalf("ABI %d: the descriptor is not the home: %v", abi, err)
		}
		c.done()
		plan.close()

		call = withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d", "--target", "/", "--include", home), env...)
		plan, err = s.planTool(&call)
		if err != nil {
			t.Fatal(err)
		}
		if (plan.confine != nil) != (abi >= 2) || plan.wrap == nil {
			t.Fatalf("ABI %d: a restore confined %v", abi, plan.confine != nil)
		}
		plan.close()
	}
	// Without Landlock a backup runs in its view without symlinks where the
	// agent may mount — sandbox-exec --no-landlock, no rules, nothing
	// pinned —, and everything else as it is.
	for _, view := range []bool{false, true} {
		s, bin := sandboxServer(t, 0)
		s.noSymfollow = view
		home := filepath.Join(s.cfg.WWWRoot, "alex")
		mkdirs(t, home)
		env := []string{"RESTIC_REPOSITORY=sftp:backup@203.0.113.7:/srv/restic", "RESTIC_PASSWORD=pw"}
		for _, args := range [][]string{
			{"backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home},
			{"restore", "1a2b3c4d", "--target", "/", "--include", home},
			{"snapshots", "--json", "--tag", "monopanel"},
		} {
			call := withEnv(tc(modeTool, "restic", args...), env...)
			plan, err := s.planTool(&call)
			if err != nil {
				t.Fatal(err)
			}
			if want := view && args[0] == "backup"; (plan.confine != nil) != want || len(plan.closers) != 0 {
				t.Fatalf("no Landlock, view %v: %s confined %v, %d pinned", view, args[0], plan.confine != nil, len(plan.closers))
			}
			if plan.confine != nil {
				c, err := plan.confine(context.Background(), bin, plan.args, &bytes.Buffer{})
				if err != nil {
					t.Fatal(err)
				}
				// restic's own ssh, as without a sandbox: no master to go through
				if want := "sandbox-exec --no-landlock --nosymfollow " + s.cfg.WWWRoot + " -- " + bin + " backup"; !strings.HasPrefix(strings.Join(c.args, " "), want) || len(c.files) != 0 || len(c.env) != 0 {
					t.Fatalf("no Landlock: %q %d %q", c.args, len(c.files), c.env)
				}
				c.done()
			}
			plan.close()
		}
	}
}

// With Landlock and where the agent may mount, restic reads the system's
// files of /etc in a private /etc: its rules name them there instead of in
// the system's /etc, and a backup of the panel's configuration below /etc
// binds it in as it is — unless it would hold the system's entries, and the
// run then reads /etc by the rules of its files. What restic needs of the
// system — the CA certificates, name resolution, owner names, the time zone
// — it finds in it as on the host, and /etc/shadow not at all.
func TestResticPrivateEtc(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	s.privateEtc = true
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	backup := func(paths ...string) []string {
		return append([]string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=server"}, paths...)
	}
	acc, err := s.resticRules(bin, backup(home), "s3:https://s3.example.com/b")
	if err != nil || !slices.Equal(acc.etc, existingPaths(resticEtcReads)) || len(acc.etc) == 0 || acc.etcBind != nil {
		t.Fatalf("private /etc: %+v %v", acc, err)
	}
	for _, p := range acc.ro {
		if below(p, "/etc") {
			t.Errorf("a rule of the system's /etc beside the private one: %s", p)
		}
	}
	conf := s.cfg.ConfigDir
	s.cfg.ConfigDir = "/etc/monopanel-test"
	if acc, err := s.resticRules(bin, backup(home, s.cfg.ConfigDir), ""); err != nil || !slices.Equal(acc.etcBind, []string{s.cfg.ConfigDir}) || acc.etc == nil {
		t.Errorf("the configuration below /etc: %+v %v", acc, err)
	}
	s.cfg.ConfigDir = "/etc/ssl"
	if acc, err := s.resticRules(bin, backup(s.cfg.ConfigDir), ""); err != nil || acc.etc != nil || acc.etcBind != nil || !slices.Contains(acc.ro, "/etc/hosts") {
		t.Errorf("a configuration that holds the system's entries: %+v %v", acc, err)
	}
	s.cfg.ConfigDir = conf
	// resolv.conf as a symlink straight into /run stays one, live; any
	// other is copied
	for _, c := range []struct {
		link, target, resolved, dir string
	}{
		{"/etc/resolv.conf", "../run/systemd/resolve/stub-resolv.conf", "/run/systemd/resolve/stub-resolv.conf", "/run/systemd/resolve"},
		{"/etc/resolv.conf", "/run/NetworkManager/resolv.conf", "/run/NetworkManager/resolv.conf", "/run/NetworkManager"},
		{"/etc/resolv.conf", "/etc/resolvconf/run/resolv.conf", "/run/resolvconf/resolv.conf", ""},
		{"/etc/resolv.conf", "resolv.conf.dhcp", "/etc/resolv.conf.dhcp", ""},
		{"/etc/resolv.conf", "/run/x/resolv.conf", "/var/lib/x/resolv.conf", ""},
	} {
		if dir, ok := linkDirIn(c.link, c.target, c.resolved, "/run"); dir != c.dir || ok != (c.dir != "") {
			t.Errorf("%s -> %s: %q %v", c.link, c.target, dir, ok)
		}
	}
	if acc, err := s.resticRules(bin, backup(home), ""); err == nil {
		if dir, ok := runLinkDir("/etc/resolv.conf"); ok != slices.Contains(acc.etcBind, "/etc/resolv.conf") || ok == slices.Contains(acc.etc, "/etc/resolv.conf") || ok && !slices.Contains(acc.ro, dir) {
			t.Errorf("this host's resolv.conf: %+v", acc)
		}
	}

	repo := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, repo)
	call := withEnv(tc(modeTool, "restic", backup(home)...), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD=pw")
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	t.Setenv(fakeResticEnv, "1")
	t.Setenv("MP_FAKE_RESTIC_SYSTEM", "1")
	t.Setenv("MP_FAKE_RESTIC_READ", "/etc/shadow /etc/machine-id")
	got, code := runConfined(t, plan, bin, userNS(t))
	if code != 0 {
		t.Fatalf("restic: exit %d\n%s", code, got)
	}
	wantLines(t, got, systemProbe(), "read /etc/shadow: open /etc/shadow: no such file or directory", "read /etc/machine-id: open /etc/machine-id: no such file or directory")
}

// Where Landlock is built in but the boot left it out (EOPNOTSUPP: Oracle
// Linux's UEK), the agent says how to turn it on: the kernel's LSM list with
// landlock added, as EL or Debian set a boot parameter, and on UEK the Red
// Hat compatible kernel; a kernel without it (ENOSYS) has nothing to turn on.
func TestLandlockHint(t *testing.T) {
	el, deb := policyServer(t, oracle10), policyServer(t, debian13)
	h := landlockHint(el.profile, "6.12.0-202.76.4.4.el10uek.x86_64", "lockdown,capability,yama,selinux,bpf")
	if !strings.Contains(h, "grubby --update-kernel=ALL --args=lsm=lockdown,capability,yama,selinux,bpf,landlock") || !strings.Contains(h, "Red Hat compatible kernel") || !strings.Contains(h, "dnf install kernel") {
		t.Errorf("UEK: %s", h)
	}
	if h := landlockHint(el.profile, "5.14.0-570.el9.x86_64", "lockdown,capability,yama,selinux,bpf"); strings.Contains(h, "Red Hat compatible") || !strings.Contains(h, "grubby") {
		t.Errorf("EL: %s", h)
	}
	if h := landlockHint(deb.profile, "6.1.0-27-amd64", "lockdown,capability,apparmor"); !strings.Contains(h, "lsm=lockdown,capability,apparmor,landlock in GRUB_CMDLINE_LINUX") || !strings.Contains(h, "update-grub") {
		t.Errorf("Debian: %s", h)
	}
	if h := landlockHint(deb.profile, "6.1.0", ""); !strings.Contains(h, "lsm=landlock ") {
		t.Errorf("no list: %s", h)
	}
	el.landlockErr = fmt.Errorf("landlock: %w", unix.ENOSYS)
	if h := el.landlockHint(); h != "" {
		t.Errorf("ENOSYS: %s", h)
	}
	el.landlockErr = fmt.Errorf("landlock: %w", unix.EOPNOTSUPP)
	if h := el.landlockHint(); !strings.Contains(h, "lsm=") {
		t.Errorf("EOPNOTSUPP: %s", h)
	}
}

// Without Landlock the agent says once, as it starts, that restic runs
// without its sandbox and what that risks, and so does its ping, for the
// doctor; with Landlock the ping names the ABI.
func TestNoLandlockIsReported(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logs, nil)), landlockErr: fmt.Errorf("landlock: %w", unix.EOPNOTSUPP)}
	s.logSandbox()
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "without its sandbox") || !strings.Contains(logs.String(), "symlink") {
		t.Fatalf("log: %s", logs.String())
	}
	p, err := s.ping(context.Background(), nil)
	if err != nil || p.Sandbox == nil || p.Sandbox.Landlock != 0 || !strings.Contains(p.Sandbox.Error, "not supported") || !strings.Contains(p.Sandbox.Hint, "lsm=") {
		t.Fatalf("ping %+v %v", p.Sandbox, err)
	}
	if !strings.Contains(logs.String(), "lsm=") {
		t.Fatalf("log without how to turn Landlock on: %s", logs.String())
	}
	// without Landlock the view still covers a backup: said so
	logs.Reset()
	s.noSymfollow = true
	s.logSandbox()
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "follows no symlink") || !strings.Contains(logs.String(), "nothing else confines restic") {
		t.Fatalf("log: %s", logs.String())
	}
	s.noSymfollow = false
	logs.Reset()
	s.landlock, s.landlockErr = 4, nil
	s.logSandbox()
	if strings.Contains(logs.String(), "WARN") {
		t.Fatalf("log: %s", logs.String())
	}
	if p, _ := s.ping(context.Background(), nil); p.Sandbox.Landlock != 4 || p.Sandbox.Error != "" {
		t.Fatalf("ping %+v", p.Sandbox)
	}
	// Landlock without the view that follows no symlink: said as well,
	// with what it leaves open
	logs.Reset()
	s.noSymfollowErr = errors.New("open_tree: operation not permitted")
	s.logSandbox()
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "follows the symlinks in the trees it reads") || !strings.Contains(logs.String(), "operation not permitted") {
		t.Fatalf("log: %s", logs.String())
	}
	if p, _ := s.ping(context.Background(), nil); p.Sandbox.NoSymfollow || !strings.Contains(p.Sandbox.NoSymfollowError, "operation not permitted") {
		t.Fatalf("ping %+v", p.Sandbox)
	}
	// and without the private /etc: restic then reads resolv.conf, hosts
	// and passwd by their inodes, which the host's renames leave behind
	logs.Reset()
	s.noSymfollow, s.noSymfollowErr = true, nil
	s.privateEtcErr = errors.New("--etc: a tmpfs on /etc: permission denied")
	s.logSandbox()
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "by inode") || !strings.Contains(logs.String(), "a tmpfs on /etc") {
		t.Fatalf("log: %s", logs.String())
	}
	if p, _ := s.ping(context.Background(), nil); p.Sandbox.PrivateEtc || !strings.Contains(p.Sandbox.PrivateEtcError, "a tmpfs on /etc") || !p.Sandbox.NoSymfollow {
		t.Fatalf("ping %+v", p.Sandbox)
	}
	logs.Reset()
	s.privateEtc, s.privateEtcErr = true, nil
	s.logSandbox()
	if strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "private_etc=true") {
		t.Fatalf("log: %s", logs.String())
	}
	// the agent finds out as it starts; it needs root to mount, which a
	// test is not — with Landlock and without: the view needs none
	s.probeNoSymfollow(context.Background())
	if os.Geteuid() != 0 && (s.noSymfollow || s.noSymfollowErr == nil || !strings.Contains(s.noSymfollowErr.Error(), "root") || s.privateEtc || s.privateEtcErr == nil) {
		t.Fatalf("probe as a user: %v %v %v %v", s.noSymfollow, s.noSymfollowErr, s.privateEtc, s.privateEtcErr)
	}
	s.landlock = 0
	s.probeNoSymfollow(context.Background())
	if os.Geteuid() != 0 && (s.noSymfollow || s.noSymfollowErr == nil || s.privateEtc || s.privateEtcErr != nil) {
		t.Fatalf("probe without Landlock: %v %v %v %v", s.noSymfollow, s.noSymfollowErr, s.privateEtc, s.privateEtcErr)
	}
	// the agent as it runs knows its kernel
	abi, err := sandbox.ABI()
	if n := NewServer(s.cfg, nil, nil); n.landlock != abi || (err == nil) != (n.landlockErr == nil) {
		t.Fatalf("NewServer: %d %v, the kernel %d %v", n.landlock, n.landlockErr, abi, err)
	}
}

// An sftp repository is reached through a master connection outside the
// sandbox: when it cannot log in, restic does not start, and the agent says
// what ssh said.
func TestResticSFTPMasterFails(t *testing.T) {
	if _, err := os.Stat(sshBinaries[0]); err != nil {
		t.Skip("no ssh here")
	}
	s, bin := sandboxServer(t, 5)
	// nothing of the configuration of whoever runs the test
	old := sshMasterOptions
	sshMasterOptions = []string{"-F", "none", "-o", "ConnectTimeout=10"}
	t.Cleanup(func() { sshMasterOptions = old })
	env := []string{"RESTIC_REPOSITORY=sftp://nobody@127.0.0.1:1//srv/restic", "RESTIC_PASSWORD=pw"}
	call := withEnv(tc(modeTool, "restic", "snapshots", "--json", "--tag", "monopanel"), env...)
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	before, _ := filepath.Glob(filepath.Join(sshMasterBase(), "mp-ssh-*"))
	var out bytes.Buffer
	code, err := planRunner(context.Background(), plan, bin, "", &out)(plan.args, nil)
	if err == nil || !strings.Contains(err.Error(), "ssh to 127.0.0.1") || code != 0 || strings.Contains(out.String(), "argv") {
		t.Fatalf("a master that cannot log in: %d %v\n%s", code, err, out.String())
	}
	if after, _ := filepath.Glob(filepath.Join(sshMasterBase(), "mp-ssh-*")); len(after) > len(before) {
		t.Fatalf("the master's directory stayed: %q", after)
	}
}

// The whole way to an sftp repository, against a server of the test's own
// (MP_TEST_SSHD=1, with OpenSSH's sshd and ssh-keygen): the agent's master
// logs in with its key and known hosts, and the ssh restic runs in the
// sandbox — no key, no configuration, no known hosts readable there — gets
// an SFTP session through it.
func TestResticSFTPThroughTheMaster(t *testing.T) {
	if os.Getenv("MP_TEST_SSHD") == "" {
		t.Skip("MP_TEST_SSHD=1 runs an sshd of the test's own")
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		for _, p := range []string{"/usr/sbin/sshd", "/usr/bin/sshd"} {
			if _, err := os.Stat(p); err == nil {
				sshd = p
			}
		}
	}
	if sshd == "" {
		t.Skip("no sshd")
	}
	s, bin := sandboxServer(t, needLandlock(t))
	dir := t.TempDir()
	for _, k := range []string{"host", "client"} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, k)).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	pub, _ := os.ReadFile(filepath.Join(dir, "client.pub"))
	hostPub, _ := os.ReadFile(filepath.Join(dir, "host.pub"))
	writeFileT(t, filepath.Join(dir, "authorized_keys"), string(pub))
	writeFileT(t, filepath.Join(dir, "known_hosts"), "[127.0.0.1]:"+port+" "+string(hostPub))
	cfg := strings.Join([]string{"Port " + port, "ListenAddress 127.0.0.1", "HostKey " + filepath.Join(dir, "host"),
		"AuthorizedKeysFile " + filepath.Join(dir, "authorized_keys"), "StrictModes no", "UsePAM no", "PidFile none",
		"PasswordAuthentication no", "KbdInteractiveAuthentication no", "Subsystem sftp internal-sftp", "LogLevel ERROR"}, "\n") + "\n"
	writeFileT(t, filepath.Join(dir, "sshd_config"), cfg)
	daemon := exec.Command(sshd, "-D", "-e", "-f", filepath.Join(dir, "sshd_config"))
	var daemonOut bytes.Buffer
	daemon.Stdout, daemon.Stderr = &daemonOut, &daemonOut
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Process.Kill(); daemon.Wait() })
	old := sshMasterOptions
	sshMasterOptions = []string{"-F", "none", "-i", filepath.Join(dir, "client"), "-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile=" + filepath.Join(dir, "known_hosts"), "-o", "StrictHostKeyChecking=yes"}
	t.Cleanup(func() { sshMasterOptions = old })

	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	me, login := u.HomeDir, u.Username
	call := withEnv(tc(modeTool, "restic", "snapshots", "--json", "--tag", "monopanel"), "RESTIC_REPOSITORY=sftp://"+login+"@127.0.0.1:"+port+"//srv/restic", "RESTIC_PASSWORD=pw")
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	t.Setenv(fakeResticEnv, "1")
	// the sandbox lets restic read neither the key nor the known hosts
	t.Setenv("MP_FAKE_RESTIC_READ", filepath.Join(dir, "client")+" "+filepath.Join(me, ".ssh"))
	var out bytes.Buffer
	for try := 0; ; try++ {
		out.Reset()
		code, err := planRunner(context.Background(), plan, bin, "", &out)(plan.args, nil)
		if err == nil && code == 0 {
			break
		}
		if try == 20 {
			t.Fatalf("restic over sftp: %d %v\n%s\nsshd: %s", code, err, out.String(), daemonOut.String())
		}
		time.Sleep(100 * time.Millisecond) // sshd may not listen yet
	}
	t.Log(out.String())
	wantLines(t, out.String(), "sftp: version 3", "read "+filepath.Join(dir, "client")+": denied")
	if !strings.Contains(out.String(), "argv -o sftp.command=/usr/bin/ssh -F none ") {
		t.Fatalf("restic's ssh goes through the master: %s", out.String())
	}
	// and so through a private /etc, which an agent that may mount gives
	// restic: the ssh in the sandbox finds there what it reads of /etc
	s.privateEtc = true
	plan, err = s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	got, code := runConfined(t, plan, bin, userNS(t))
	if code != 0 {
		t.Fatalf("restic over sftp in a private /etc: exit %d\n%s", code, got)
	}
	wantLines(t, got, "sftp: version 3", "read "+filepath.Join(dir, "client")+": denied")
}

// userNS is the attribute that starts a program as root of a user namespace
// of its own (the test's uid and gid mapped to 0) — what mounting takes, which
// the agent may as root and a test may not; nil, and the test skipped, where
// the kernel or its settings refuse such namespaces.
func userNS(t *testing.T) *syscall.SysProcAttr {
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
		t.Skipf("no user namespace for the test (%v): the view without symlinks needs one", err)
	}
	return attr()
}

// runConfined runs the plan's restic as planRunner does, through what
// confine made of it, but started with attr.
func runConfined(t *testing.T, plan *toolPlan, bin string, attr *syscall.SysProcAttr) (string, int) {
	t.Helper()
	var out bytes.Buffer
	c, err := plan.confine(context.Background(), bin, plan.args, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer c.done()
	cmd := exec.Command(c.path, c.args...)
	cmd.Env = append(append(append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8"), plan.env...), c.env...)
	cmd.ExtraFiles = c.files
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = attr
	code, err := exitCode(cmd.Run())
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return out.String(), code
}

// A server backup reads the panel's configuration and every home besides
// alex's: Landlock grants those trees however they are reached, so with it
// alone a directory alex swapped for a symlink to the configuration or to
// bob's home has restic read the secret key or bob's files as alex's own —
// and the API, whose certificates and dumps the backup reads as well, can do
// the same with a directory of its own there. In the views without symlinks
// of the trees a backup reads — what the agent runs a backup with when it may
// mount — they lead nowhere, while the trees themselves are read as before;
// and so without Landlock, where the view is all there is.
func TestResticServerBackupFollowsNoSymlinkInTheWebRoot(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	home, bob := filepath.Join(s.cfg.WWWRoot, "alex"), filepath.Join(s.cfg.WWWRoot, "bob")
	certs := filepath.Join(s.cfg.DataDir, "certs")
	mkdirs(t, filepath.Join(home, "data"), bob, certs)
	writeFileT(t, filepath.Join(home, "data", "index.php"), "<?php")
	writeFileT(t, filepath.Join(bob, "wp-config.php"), "bob's password")
	writeFileT(t, filepath.Join(s.cfg.ConfigDir, "secret.key"), "panel-secret")
	writeFileT(t, filepath.Join(certs, "example.com.pem"), "a certificate")
	for link, to := range map[string]string{filepath.Join(home, "a"): s.cfg.ConfigDir, filepath.Join(home, "b"): bob, filepath.Join(certs, "x"): s.cfg.ConfigDir} {
		if err := os.Symlink(to, link); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, repo)
	call := withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", s.cfg.WWWRoot, s.cfg.ConfigDir, certs), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD=pw")
	t.Setenv(fakeResticEnv, "1")
	through := []string{filepath.Join(home, "a", "secret.key"), filepath.Join(home, "b", "wp-config.php"), filepath.Join(certs, "x", "secret.key")}
	t.Setenv("MP_FAKE_RESTIC_READ", strings.Join(through, " "))
	stolen := []string{"read " + through[0] + ": ok panel-secret", "read " + through[1] + ": ok bob's password", "read " + through[2] + ": ok panel-secret"}

	// Landlock alone: what the view is for
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	var out bytes.Buffer
	if code, err := planRunner(context.Background(), plan, bin, "", &out)(plan.args, nil); err != nil || code != 0 {
		t.Fatalf("restic: %d %v\n%s", code, err, out.String())
	}
	wantLines(t, out.String(), stolen...)

	s.noSymfollow = true
	if acc, err := s.resticRules(bin, call.args, repo); err != nil || !slices.Equal(acc.noSymlinks, []string{s.cfg.WWWRoot, s.cfg.ConfigDir, s.cfg.DataDir}) {
		t.Fatalf("a server backup without its views: %+v %v", acc, err)
	}
	attr := userNS(t)
	for _, landlock := range []int{s.landlock, 0} {
		s.landlock = landlock
		plan, err = s.planTool(&call)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.close()
		if plan.confine == nil {
			t.Fatalf("Landlock %d: a backup runs in its view", landlock)
		}
		if landlock == 0 {
			c, err := plan.confine(context.Background(), bin, plan.args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			want := "sandbox-exec --no-landlock --nosymfollow " + s.cfg.WWWRoot + " --nosymfollow " + s.cfg.ConfigDir + " --nosymfollow " + s.cfg.DataDir + " -- " + bin + " backup"
			if !strings.HasPrefix(strings.Join(c.args, " "), want) || len(c.files) != 0 || len(c.env) != 0 || len(plan.closers) != 0 {
				t.Fatalf("without Landlock: %q files %d env %q pinned %d", c.args, len(c.files), c.env, len(plan.closers))
			}
			c.done()
		}
		got, code := runConfined(t, plan, bin, attr)
		if code != 0 {
			t.Fatalf("Landlock %d: restic: exit %d\n%s", landlock, code, got)
		}
		for _, l := range stolen {
			if strings.Contains(got, l) {
				t.Errorf("Landlock %d: read through a symlink: %s", landlock, l)
			}
		}
		wantLines(t, got,
			"read "+filepath.Join(home, "data", "index.php")+": ok <?php",
			"read "+filepath.Join(bob, "wp-config.php")+": ok bob's password",
			"read "+filepath.Join(s.cfg.ConfigDir, "secret.key")+": ok panel-secret",
			"read "+filepath.Join(certs, "example.com.pem")+": ok a certificate",
		)
		for _, p := range through {
			wantLines(t, got, "read "+p+": open "+p+": too many levels of symbolic links")
		}
	}
}

// The views are a backup's, one for each tree it reads — the web root, the
// configuration directory, the data directory with the certificates, the
// ACME account and the run's dumps —, nothing else's: not a restore, not a
// listing. The data directory is the API's, and so are the certificates and
// dumps in it: a view on those would leave with them when the API renames
// them, and the view goes on the data directory, whose name in /var/lib only
// root may change.
func TestResticNoSymlinksPerPlan(t *testing.T) {
	s, bin := sandboxServer(t, 5)
	s.noSymfollow = true
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	run := filepath.Join(s.backupTmpDir(), "run-1")
	dump := filepath.Join(run, "alex_wp.sql")
	mkdirs(t, home, run, filepath.Join(s.cfg.DataDir, "certs"))
	www, conf, data := s.cfg.WWWRoot, s.cfg.ConfigDir, s.cfg.DataDir
	for _, c := range []struct {
		args []string
		view []string
	}{
		{[]string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home, dump}, []string{www, data}},
		{[]string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=site:example.com", filepath.Join(home, "example.com")}, []string{www}},
		{[]string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=db:alex_wp", dump}, []string{data}},
		{[]string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=server", www, conf, filepath.Join(data, "certs"), filepath.Join(data, "acme"), dump, filepath.Join(run, "panel.db"), "--exclude=" + s.restoreArea()}, []string{www, conf, data}},
		{[]string{"restore", strings.Repeat("ab", 32), "--target", filepath.Join(RestoreDir, "1a2b3c4d"), "--include", home}, nil},
		{[]string{"snapshots", "--json", "--tag", "monopanel"}, nil},
	} {
		acc, err := s.resticRules(bin, c.args, "s3:https://s3.example.com/b")
		if err != nil || !slices.Equal(acc.noSymlinks, c.view) {
			t.Errorf("%q: views %q, want %q (%v)", c.args, acc.noSymlinks, c.view, err)
		}
	}
	s.noSymfollow = false
	if acc, err := s.resticRules(bin, []string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home}, ""); err != nil || acc.noSymlinks != nil {
		t.Errorf("an agent that cannot mount: %v %v", acc.noSymlinks, err)
	}
	// a tree in a directory somebody else may rearrange gets its view one
	// level up, where the name is root's alone
	s.noSymfollow = true
	if err := os.Chmod(filepath.Dir(data), 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(data), 0o755) })
	if acc, err := s.resticRules(bin, []string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=db:alex_wp", dump}, ""); err != nil || !slices.Equal(acc.noSymlinks, []string{filepath.Dir(data)}) {
		t.Errorf("a data directory others may rename: %q %v", acc.noSymlinks, err)
	}
}

// A local repository is a directory of restic's own: the sandbox lets restic
// read all of it, and in one that holds other files — Debian's /var/backups,
// with its copies of /etc/shadow — a symlink that leads there would have them
// backed up. One that holds what restic makes, or a restic init left, or a
// disk's lost+found is restic's; a missing one is restic's to report. restic
// init makes the directory the way the agent walks: not through a symlink
// the API put in its data directory.
func TestResticLocalRepositoryIsRestics(t *testing.T) {
	s, bin := sandboxServer(t, 5)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	confine := func(repo string, args ...string) error {
		t.Helper()
		call := withEnv(tc(modeTool, "restic", args...), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD=pw")
		plan, err := s.planTool(&call)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.close()
		c, err := plan.confine(context.Background(), bin, plan.args, &bytes.Buffer{})
		if err == nil {
			c.done()
		}
		return err
	}
	backup := []string{"backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home}

	shared := filepath.Join(t.TempDir(), "backups")
	mkdirs(t, shared)
	writeFileT(t, filepath.Join(shared, "shadow.bak"), "root:hash")
	for _, args := range [][]string{backup, {"init"}, {"snapshots", "--json", "--tag", "monopanel"}} {
		err := confine(shared, args...)
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != 403 || !strings.Contains(ae.Message, "a directory of its own") || strings.Contains(ae.Message, "shadow.bak") {
			t.Errorf("%s into a directory of other files: %v", args[0], err)
		}
	}

	own := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, filepath.Join(own, "data", "00"), filepath.Join(own, "keys"), filepath.Join(own, "lost+found"))
	writeFileT(t, filepath.Join(own, "config"), "restic")
	writeFileT(t, filepath.Join(own, "config-tmp-123456"), "restic")
	if err := confine(own, backup...); err != nil {
		t.Errorf("restic's own repository: %v", err)
	}
	if err := confine(filepath.Join(t.TempDir(), "missing"), backup...); err != nil {
		t.Errorf("a missing repository is restic's to report: %v", err)
	}

	fresh := filepath.Join(t.TempDir(), "disk", "monopanel")
	if err := confine(fresh, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if st, err := os.Stat(fresh); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("init made %v %v", st, err)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.cfg.DataDir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := confine(filepath.Join(s.cfg.DataDir, "link", "repo"), "init"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("init through the API's symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "repo")); err == nil {
		t.Error("init made the repository through the API's symlink")
	}
}
