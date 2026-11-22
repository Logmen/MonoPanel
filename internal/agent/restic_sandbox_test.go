package agent

import (
	"bytes"
	"context"
	"encoding/json"
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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
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
// directory, the repository, and where it must not). Before all that it
// starts as a restic of Red Hat's Go does (MP_FAKE_RESTIC_GOFIPS,
// fakeRedHatGo), and restic version prints its line first.
func fakeResticMain(args []string) int {
	if d, err := time.ParseDuration(os.Getenv("MP_FAKE_RESTIC_SLEEP")); err == nil {
		time.Sleep(d)
	}
	fips, ok := fakeRedHatGo()
	if !ok {
		return 1
	}
	if len(args) > 0 && args[0] == "version" {
		fmt.Printf("restic 0.0.0-test compiled with %s on %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	}
	if fips != "" {
		fmt.Println(fips)
	}
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

// fakeRedHatGo starts as a program built with Red Hat's Go does
// (golang-fips, crypto/internal/fips140deps/hostfips): it reads the kernel's
// FIPS switch, the file MP_FAKE_RESTIC_GOFIPS names, and says so in its line;
// a switch that is not there is "off", and any other failure ends the
// program with Red Hat's own words — the testbed's restic init said them.
// ok is false then.
func fakeRedHatGo() (string, bool) {
	p := os.Getenv("MP_FAKE_RESTIC_GOFIPS")
	if p == "" {
		return "", true
	}
	data, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "fips " + p + ": none", true
	case err != nil:
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", p, err)
		return "", false
	}
	return "fips " + p + ": " + strings.TrimSpace(string(data)), true
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
	// resolv.conf as a symlink into /run (systemd-resolved) stays one: bound
	// in, its directory granted, instead of a copy in the private /etc
	wantEtc, wantBind := existingPaths(resticEtcReads), []string(nil)
	if _, ok := runLinkDir(resolvConf); ok {
		wantEtc = slices.DeleteFunc(wantEtc, func(p string) bool { return p == resolvConf })
		wantBind = []string{resolvConf}
	}
	if err != nil || !slices.Equal(acc.etc, wantEtc) || len(acc.etc) == 0 || !slices.Equal(acc.etcBind, wantBind) {
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

// fipsSwitchHere is the kernel's FIPS switch where this kernel has one and,
// where it has none (built without CONFIG_CRYPTO_FIPS, as this workstation's
// may be), a sysctl that stands in for it: the rules grant the stand-in in
// the switch's place for the test — a single file of procfs, read-only,
// which is what is tested.
func fipsSwitchHere(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(fipsSwitch); err == nil {
		return fipsSwitch
	}
	standIn := "/proc/sys/kernel/osrelease"
	if _, err := os.Stat(standIn); err != nil {
		t.Skipf("neither %s nor %s here", fipsSwitch, standIn)
	}
	t.Logf("this kernel has no %s (CONFIG_CRYPTO_FIPS): %s stands in for it", fipsSwitch, standIn)
	withSystemReads(t, func(p string) string {
		if p == fipsSwitch {
			return standIn
		}
		return p
	})
	return standIn
}

// withSystemReads has resticSystemReads, and with them resticEtcReads and
// resticOtherReads, as f maps their entries ("" leaves one out) until the
// test ends or the returned function puts them back.
func withSystemReads(t *testing.T, f func(string) string) (restore func()) {
	t.Helper()
	old, oldEtc, oldOther := resticSystemReads, resticEtcReads, resticOtherReads
	var reads []string
	for _, p := range old {
		if q := f(p); q != "" {
			reads = append(reads, q)
		}
	}
	resticSystemReads = reads
	resticEtcReads, resticOtherReads = splitEtc(reads)
	restore = func() { resticSystemReads, resticEtcReads, resticOtherReads = old, oldEtc, oldOther }
	t.Cleanup(restore)
	return restore
}

// Of /proc restic's rules grant the kernel's FIPS switch alone, read-only —
// Red Hat's Go reads it as every program it built starts and exits when it
// cannot — and below /etc the FIPS provider's settings, which EL's libcrypto
// loads in FIPS mode; nothing else of /proc, nothing of /proc/self, nor the
// third-party providers of openssl.d. A kernel without the switch gets no
// rule for it: Red Hat's Go takes the missing file for "off".
func TestResticRulesGrantTheFIPSSwitchAlone(t *testing.T) {
	s, bin := sandboxServer(t, 5)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	const local = "/etc/pki/tls/fips_local.cnf"
	if !slices.Contains(resticOtherReads, fipsSwitch) || !slices.Contains(resticEtcReads, local) {
		t.Fatalf("the FIPS switch and fips_local.cnf: %q %q", resticOtherReads, resticEtcReads)
	}
	for _, p := range resticSystemReads {
		if atOrBelow(p, "/proc") && p != fipsSwitch || atOrBelow(p, "/etc/pki/tls/openssl.d") {
			t.Errorf("restic reads %s", p)
		}
	}
	_, err := os.Stat(fipsSwitch)
	haveSwitch := err == nil
	localTarget, err := filepath.EvalSymlinks(local)
	haveLocal := err == nil
	repo := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, repo)
	for _, private := range []bool{false, true} {
		s.privateEtc = private
		for _, args := range [][]string{
			{"backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home},
			{"restore", strings.Repeat("ab", 32), "--target", filepath.Join(RestoreDir, "1a2b3c4d"), "--include", home},
			{"snapshots", "--json", "--tag", "monopanel"},
			{"version"},
		} {
			acc, err := s.resticRules(bin, args, repo)
			if err != nil {
				t.Fatalf("%s: %v", args[0], err)
			}
			if slices.Contains(acc.ro, fipsSwitch) != haveSwitch {
				t.Errorf("private /etc %v, %s: the switch, here %v, in %q", private, args[0], haveSwitch, acc.ro)
			}
			for _, p := range acc.ro {
				if atOrBelow(p, "/proc") && p != fipsSwitch {
					t.Errorf("private /etc %v, %s: %s granted", private, args[0], p)
				}
			}
			for _, list := range [][]string{acc.rw, acc.rx, acc.etc, acc.etcBind, acc.noSymlinks} {
				if i := slices.IndexFunc(list, func(p string) bool { return atOrBelow(p, "/proc") }); i >= 0 {
					t.Errorf("private /etc %v, %s: %s granted beyond reading", private, args[0], list[i])
				}
			}
			switch {
			case private && slices.Contains(acc.etc, local) != haveLocal:
				t.Errorf("%s: fips_local.cnf, here %v, in the private /etc %q", args[0], haveLocal, acc.etc)
			case !private && haveLocal && !slices.ContainsFunc(acc.ro, func(p string) bool { return atOrBelow(localTarget, p) }):
				// the link's target, which a rule of the crypto policies holds
				t.Errorf("%s: fips_local.cnf (%s) out of reach: %q", args[0], localTarget, acc.ro)
			}
		}
	}
	// and so the command line of a run: the switch by --ro alone
	s.privateEtc = false
	c, err := s.confineRestic("", nil)(context.Background(), bin, []string{"version"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.done()
	argv := strings.Join(c.args, " ") + " "
	if strings.Contains(argv, " --ro "+fipsSwitch+" ") != haveSwitch || strings.Contains(argv, " --rw /proc") || strings.Contains(argv, " --rx /proc") {
		t.Errorf("the switch, here %v: %s", haveSwitch, argv)
	}
}

// restic built with Red Hat's Go reads the kernel's FIPS switch as it starts
// (fakeRedHatGo): in its sandbox it reads it, by the rule of the file and in
// the private /etc too, whose mount namespace leaves /proc as it is, and goes
// on — without the rule it ends right there, as every restic of the panel's
// did on Alma and Rocky Linux before it. Where this kernel has no switch,
// another sysctl stands in for it (fipsSwitchHere).
func TestResticStartsWithRedHatsGo(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	sw := fipsSwitchHere(t)
	value, err := os.ReadFile(sw)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	repo := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, home, repo)
	writeFileT(t, filepath.Join(home, "index.php"), "<?php")
	call := withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD=pw")
	t.Setenv(fakeResticEnv, "1")
	t.Setenv("MP_FAKE_RESTIC_GOFIPS", sw)
	started := "fips " + sw + ": " + strings.TrimSpace(string(value))
	backup := func() (string, int) {
		t.Helper()
		plan, err := s.planTool(&call)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.close()
		if plan.confine == nil {
			t.Fatal("a backup runs in the sandbox")
		}
		var out bytes.Buffer
		code, err := planRunner(context.Background(), plan, bin, "", &out)(plan.args, nil)
		if err != nil {
			t.Fatalf("restic: %v\n%s", err, out.String())
		}
		return out.String(), code
	}
	got, code := backup()
	if code != 0 {
		t.Fatalf("restic: exit %d\n%s", code, got)
	}
	wantLines(t, got, started, "read "+filepath.Join(home, "index.php")+": ok <?php")

	// without the rule: what the testbed saw
	restore := withSystemReads(t, func(p string) string {
		if p == sw {
			return ""
		}
		return p
	})
	got, code = backup()
	if want := "error reading " + sw + ": open " + sw + ": permission denied"; code != 1 || !strings.Contains(got, want) || strings.Contains(got, "argv ") {
		t.Fatalf("without the rule: exit %d, want 1 and %q:\n%s", code, want, got)
	}
	restore()

	// in the private /etc
	s.privateEtc = true
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	got, code = runConfined(t, plan, bin, userNS(t))
	if code != 0 {
		t.Fatalf("restic in the private /etc: exit %d\n%s", code, got)
	}
	wantLines(t, got, started, "read "+filepath.Join(home, "index.php")+": ok <?php")
}

// As the agent starts it runs the installed restic's restic version in the
// sandbox a backup gets, and its ping says what came of it, for the doctor:
// the version, or restic's own error line and exit status, which the agent's
// log says as well. A ping tests again once the binary is another one or a
// failure has stood long enough, and keeps what it found otherwise. A restic
// that does not start in its sandbox still runs there, never outside it.
// Nothing is said where restic is not installed or runs without a sandbox.
func TestResticSelfTest(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	sw := fipsSwitchHere(t)
	t.Setenv(fakeResticEnv, "1")
	t.Setenv("MP_FAKE_RESTIC_GOFIPS", sw)
	ctx := context.Background()
	version := fmt.Sprintf("restic 0.0.0-test compiled with %s on %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	r := s.selfTestRestic(ctx)
	if r == nil || r.err != nil || r.timedOut || r.version != version || r.bin != bin {
		t.Fatalf("the self-test: %+v", r)
	}
	if l := logs.String(); !strings.Contains(l, "level=INFO") || !strings.Contains(l, `msg="restic starts in its sandbox"`) || !strings.Contains(l, version) {
		t.Fatalf("log: %s", l)
	}
	p, err := s.ping(ctx, nil)
	if err != nil || p.Sandbox.Restic != bin || p.Sandbox.ResticVersion != version || p.Sandbox.ResticError != "" || p.Sandbox.ResticTimeout {
		t.Fatalf("ping %+v %v", p.Sandbox, err)
	}
	if b, _ := json.Marshal(p.Sandbox); !bytes.Contains(b, []byte(`"restic":"`+bin+`"`)) || !bytes.Contains(b, []byte(`"restic_version":"`+version+`"`)) || bytes.Contains(b, []byte("restic_error")) || bytes.Contains(b, []byte("restic_timeout")) {
		t.Fatalf("ping: %s", b)
	}

	// the same binary is not tested again: what would fail now does not run
	t.Setenv("MP_FAKE_RESTIC_EXIT", "1")
	logs.Reset()
	if again := s.selfTestRestic(ctx); again != r || logs.Len() != 0 {
		t.Fatalf("tested again: %+v\n%s", again, logs.String())
	}
	t.Setenv("MP_FAKE_RESTIC_EXIT", "")

	// another binary is — a restic in /usr/local/bin in front of the
	// distribution's, say —: here one whose sandbox keeps the switch from it
	other := filepath.Join(filepath.Dir(bin), "restic.other")
	if err := os.Symlink(bin, other); err != nil {
		t.Fatal(err)
	}
	toolBinaries["restic"] = []string{other}
	restore := withSystemReads(t, func(p string) string {
		if p == sw {
			return ""
		}
		return p
	})
	r = s.selfTestRestic(ctx)
	wantErr := "error reading " + sw + ": open " + sw + ": permission denied (exit 1)"
	if r == nil || r.err == nil || r.err.Error() != wantErr || r.timedOut || r.version != "" || r.bin != other {
		t.Fatalf("a restic that does not start in its sandbox: %+v, want %q", r, wantErr)
	}
	if l := logs.String(); !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "restic does not start in its sandbox") || !strings.Contains(l, "upgrade the panel") || !strings.Contains(l, wantErr) {
		t.Fatalf("log: %s", l)
	}
	if p, _ := s.ping(ctx, nil); p.Sandbox.Restic != other || p.Sandbox.ResticError != wantErr || p.Sandbox.ResticTimeout || p.Sandbox.ResticVersion != "" {
		t.Fatalf("ping %+v", p.Sandbox)
	}
	// and it changes nothing else: a backup runs in the sandbox all the same,
	// and fails as restic fails there
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	call := withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home), "RESTIC_REPOSITORY=s3:https://s3.example.com/b", "RESTIC_PASSWORD=pw")
	plan, err := s.planTool(&call)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	if plan.confine == nil {
		t.Fatal("a restic that failed its self-test runs outside the sandbox")
	}
	var out bytes.Buffer
	if code, err := planRunner(ctx, plan, other, "", &out)(plan.args, nil); err != nil || code != 1 || !strings.Contains(out.String(), "error reading "+sw) {
		t.Fatalf("the backup: %d %v\n%s", code, err, out.String())
	}

	// a failure stands, not said again, until it is ResticRetestAfter old
	logs.Reset()
	if again := s.selfTestRestic(ctx); again != r || logs.Len() != 0 {
		t.Fatalf("a fresh failure tested again: %+v\n%s", again, logs.String())
	}
	restore()
	r.at = time.Now().Add(-ResticRetestAfter - time.Minute)
	if r = s.selfTestRestic(ctx); r == nil || r.err != nil || r.version != version || !strings.Contains(logs.String(), "restic starts in its sandbox") {
		t.Fatalf("tested again: %+v\n%s", r, logs.String())
	}

	// no restic installed: nothing tested, nothing said
	toolBinaries["restic"] = []string{filepath.Join(t.TempDir(), "restic")}
	logs.Reset()
	if r := s.selfTestRestic(ctx); r != nil || s.resticTest.Load() != nil || logs.Len() != 0 {
		t.Fatalf("no restic: %+v %+v\n%s", r, s.resticTest.Load(), logs.String())
	}
	if p, _ := s.ping(ctx, nil); p.Sandbox.Restic != "" || p.Sandbox.ResticVersion != "" || p.Sandbox.ResticError != "" {
		t.Fatalf("ping without restic %+v", p.Sandbox)
	}
	// nor without a sandbox, where restic runs as it is
	toolBinaries["restic"] = []string{bin}
	s.landlock, s.landlockErr = 0, unix.ENOSYS
	if r := s.selfTestRestic(ctx); r != nil {
		t.Fatalf("no sandbox: %+v", r)
	}
}

// The self-test's plan is a backup's sandbox, the private /etc and its mount
// namespace included where the agent may mount, and restic version — which
// starts in there and reads the FIPS switch — gives its line; one that does
// not finish in time is said to have not.
func TestResticSelfTestPlan(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	sw := fipsSwitchHere(t)
	t.Setenv(fakeResticEnv, "1")
	t.Setenv("MP_FAKE_RESTIC_GOFIPS", sw)
	s.privateEtc = true
	plan := s.resticSelfTestPlan()
	defer plan.close()
	c, err := plan.confine(context.Background(), bin, plan.args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if argv := strings.Join(c.args, " "); !strings.HasPrefix(argv, "sandbox-exec --etc ") || !strings.HasSuffix(argv, " -- "+bin+" version") || !slices.Equal(c.env, []string{"TMPDIR=" + s.resticTmp}) || !slices.Equal(plan.env, []string{"RESTIC_CACHE_DIR=" + s.resticCache}) {
		t.Fatalf("the self-test's run: %s %q %q", argv, c.env, plan.env)
	}
	c.done()
	got, code := runConfined(t, plan, bin, userNS(t))
	version, timedOut, err := resticSelfTestOutcome(context.Background(), code, nil, got)
	if err != nil || timedOut || !strings.HasPrefix(version, "restic 0.0.0-test compiled with ") {
		t.Fatalf("restic version in the private /etc: %q %v %v\n%s", version, timedOut, err, got)
	}
	value, _ := os.ReadFile(sw)
	wantLines(t, got, "fips "+sw+": "+strings.TrimSpace(string(value)))

	s.privateEtc = false
	old := resticSelfTestTimeout
	resticSelfTestTimeout = 500 * time.Millisecond
	t.Cleanup(func() { resticSelfTestTimeout = old })
	t.Setenv("MP_FAKE_RESTIC_SLEEP", "5s")
	start := time.Now()
	if _, timedOut, err := s.runResticSelfTest(context.Background(), bin); err == nil || !timedOut || err.Error() != "restic version did not finish within 500ms" || time.Since(start) > 4*time.Second {
		t.Fatalf("a restic that hangs: %v (timed out: %v) after %s", err, timedOut, time.Since(start))
	}
}

// What the self-test makes of restic's output: the first line said, cut
// short, or what restic did not say — and whether it did not finish in time,
// which the deadline of its run tells, not the words. A restic the deadline
// killed timed out; one that exited with an error, whose sandbox could not
// be set up (sandbox.ExitSetup), that could not be started or that something
// else killed (the OOM killer) failed; a run its caller gave up on (the
// agent stopping) did neither.
func TestResticSelfTestOutcome(t *testing.T) {
	ctx := context.Background()
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	stopped, stop := context.WithCancel(ctx)
	stop()
	for _, c := range []struct {
		name     string
		ctx      context.Context
		code     int
		err      error
		out      string
		version  string
		timedOut bool
		wantErr  string
	}{
		{"a restic that starts", ctx, 0, nil, "\n  restic 0.19.1 compiled with go1.26.4 (Red Hat 1.26.4-1.el9_8) on linux/amd64\n",
			"restic 0.19.1 compiled with go1.26.4 (Red Hat 1.26.4-1.el9_8) on linux/amd64", false, "<nil>"},
		{"Red Hat's Go", ctx, 1, nil, "error reading /proc/sys/crypto/fips_enabled: open /proc/sys/crypto/fips_enabled: permission denied\n",
			"", false, "error reading /proc/sys/crypto/fips_enabled: open /proc/sys/crypto/fips_enabled: permission denied (exit 1)"},
		{"a panic", ctx, 2, nil, "panic: opensslcrypto: can't initialize OpenSSL libcrypto.so.3\n\ngoroutine 1 [running]:\nmain.main()\n",
			"", false, "panic: opensslcrypto: can't initialize OpenSSL libcrypto.so.3 (exit 2)"},
		{"nothing said", ctx, sandbox.ExitSetup, nil, "", "", false, "it said nothing (exit 125)"},
		{"killed, but not by the deadline", ctx, -1, nil, "", "", false, "it said nothing (exit -1)"},
		{"not run", ctx, 0, errors.New("fork/exec /proc/self/exe: permission denied"), "", "", false, "fork/exec /proc/self/exe: permission denied"},
		{"killed by the deadline", expired, -1, nil, "", "", true, "restic version did not finish within " + resticSelfTestTimeout.String()},
		{"not started before the deadline", expired, 0, context.DeadlineExceeded, "", "", true, "restic version did not finish within " + resticSelfTestTimeout.String()},
		{"given up on", stopped, -1, nil, "", "", false, context.Canceled.Error()},
	} {
		v, timedOut, err := resticSelfTestOutcome(c.ctx, c.code, c.err, c.out)
		if v != c.version || timedOut != c.timedOut || fmt.Sprint(err) != c.wantErr {
			t.Errorf("%s: %q, timed out %v, %v; want %q, %v, %s", c.name, v, timedOut, err, c.version, c.timedOut, c.wantErr)
		}
	}
	if l := firstLine("a"+strings.Repeat("é", 300), ""); len(l) > 403 || !strings.HasSuffix(l, "…") || !utf8.ValidString(l) {
		t.Errorf("a long line: %d bytes %q", len(l), l)
	}
}

// An upgrade renames a new restic over the old one: it is another binary,
// and so is one written anew; the same file stays the same.
func TestResticBinaryID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "restic")
	writeFileT(t, p, "one")
	a, err := statBinary(p)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := statBinary(p); err != nil || b != a {
		t.Fatalf("the same file: %v %v", b, err)
	}
	writeFileT(t, p+".new", "one")
	if err := os.Rename(p+".new", p); err != nil {
		t.Fatal(err)
	}
	b, err := statBinary(p)
	if err != nil || b == a {
		t.Fatalf("renamed over: %v %v", b, err)
	}
	writeFileT(t, p, "two!")
	if c, err := statBinary(p); err != nil || c == b {
		t.Fatalf("written anew: %v %v", c, err)
	}
	if _, err := statBinary(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}

// The agent tests restic in its sandbox as it starts — after the probes —,
// and its log says what came of it. The API does not wait for that: the
// agent listens while restic version still runs, as a restic that hangs
// would until resticSelfTestTimeout.
func TestAgentStartTestsRestic(t *testing.T) {
	needLandlock(t)
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	grp, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Skip(err)
	}
	run, err := os.MkdirTemp("", "mpsock") // a socket's path is capped near 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	cfg := config.Default()
	cfg.RunDir, cfg.ServiceUser, cfg.ServiceGroup = run, me.Username, grp.Name
	rel, _ := osprofile.ParseOSRelease(strings.NewReader(alma9))
	p, _ := osprofile.FromRelease(rel)
	var logs lockedBuffer
	s := NewServer(cfg, p, slog.New(slog.NewTextHandler(&logs, nil)))
	dir := t.TempDir()
	s.resticCache, s.resticTmp = filepath.Join(dir, "cache"), filepath.Join(dir, "tmp")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "restic")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	old := toolBinaries["restic"]
	toolBinaries["restic"] = []string{bin}
	t.Cleanup(func() { toolBinaries["restic"] = old })
	t.Setenv(fakeResticEnv, "1")
	// serve starts the agent and returns how long it took to listen and
	// what stops it again, with how long that took
	serve := func() (time.Duration, func() time.Duration) {
		t.Helper()
		os.Remove(cfg.AgentSocket())
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- s.ListenAndServe(ctx) }()
		var listening time.Duration
		for deadline := time.Now().Add(10 * time.Second); listening == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if st, err := os.Lstat(cfg.AgentSocket()); err == nil && st.Mode()&os.ModeSocket != 0 {
				listening = time.Since(start)
			}
		}
		return listening, func() time.Duration {
			start := time.Now()
			cancel()
			if err := <-done; err != nil {
				t.Errorf("the agent: %v", err)
			}
			return time.Since(start)
		}
	}
	const slow = 2 * time.Second
	t.Setenv("MP_FAKE_RESTIC_SLEEP", slow.String())
	listening, stop := serve()
	// what the start's own run found, in the log once restic is done
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !strings.Contains(logs.String(), `msg="restic starts in its sandbox"`); time.Sleep(10 * time.Millisecond) {
	}
	stop()
	r := s.selfTestRestic(context.Background()) // as the start's run found
	if listening == 0 || listening >= slow {
		t.Fatalf("the agent listened after %s, with restic version taking %s:\n%s", listening, slow, logs.String())
	}
	if r == nil || r.err != nil || r.bin != bin || !strings.HasPrefix(r.version, "restic 0.0.0-test compiled with ") {
		t.Fatalf("the agent started without testing restic: %+v\n%s", r, logs.String())
	}
	l := logs.String()
	probes, tested := strings.Index(l, "restic runs in a Landlock sandbox, but"), strings.Index(l, `msg="restic starts in its sandbox"`)
	if probes < 0 || tested < probes || strings.Count(l, `msg="restic starts in its sandbox"`) != 1 || !strings.Contains(l, `msg="agent listening"`) {
		t.Fatalf("log: %s", l)
	}

	// an agent stopped while its restic hangs: it does not wait for restic,
	// nor says restic failed — it ended restic
	other := filepath.Join(dir, "restic.new")
	if err := os.Symlink(self, other); err != nil {
		t.Fatal(err)
	}
	toolBinaries["restic"] = []string{other}
	t.Setenv("MP_FAKE_RESTIC_SLEEP", "10s")
	logs.Reset()
	if listening, stop = serve(); listening == 0 {
		t.Fatalf("the agent does not listen:\n%s", logs.String())
	}
	time.Sleep(200 * time.Millisecond) // restic version is under way
	if took := stop(); took > 5*time.Second || strings.Contains(logs.String(), "restic does not start") {
		t.Fatalf("the agent stopped after %s:\n%s", took, logs.String())
	}
}

// A ping that finds restic installed or upgraded since the last self-test,
// or a failure old enough to be tested again, waits for the run no longer
// than resticPingWait: a status page gives up on the agent after 5 s, and a
// restic that hangs is found out only after resticSelfTestTimeout. It says
// what the last run found of the binary installed now — the failure being
// tested again, nothing for a binary not tested yet — and the run goes on:
// its outcome is a later ping's.
func TestPingDoesNotWaitForTheSelfTest(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	sw := fipsSwitchHere(t)
	t.Setenv(fakeResticEnv, "1")
	t.Setenv("MP_FAKE_RESTIC_GOFIPS", sw)
	old := resticPingWait
	resticPingWait = 50 * time.Millisecond
	t.Cleanup(func() { resticPingWait = old })
	ctx := context.Background()
	// a failure of this binary, old enough to be tested again
	restore := withSystemReads(t, func(p string) string {
		if p == sw {
			return ""
		}
		return p
	})
	failed := s.selfTestRestic(ctx)
	restore()
	if failed == nil || failed.err == nil {
		t.Fatalf("the failure: %+v", failed)
	}
	failed.at = time.Now().Add(-ResticRetestAfter - time.Minute)

	// restic version takes its time from here on
	const slow = time.Second
	t.Setenv("MP_FAKE_RESTIC_SLEEP", slow.String())
	ping := func() (*SandboxInfo, time.Duration) {
		t.Helper()
		start := time.Now()
		p, err := s.ping(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p.Sandbox, time.Since(start)
	}
	if sb, took := ping(); took >= slow || sb.Restic != bin || sb.ResticError != failed.err.Error() || sb.ResticVersion != "" {
		t.Fatalf("a ping while the failure is tested again took %s: %+v", took, sb)
	}
	if r := s.selfTestRestic(ctx); r == nil || r == failed || r.err != nil {
		t.Fatalf("the run the ping set off: %+v", r)
	}
	if sb, took := ping(); took >= slow || sb.Restic != bin || sb.ResticVersion == "" || sb.ResticError != "" {
		t.Fatalf("the next ping took %s: %+v", took, sb)
	}

	// a binary not tested yet: nothing to say until its run is done
	other := filepath.Join(filepath.Dir(bin), "restic.new")
	if err := os.Symlink(bin, other); err != nil {
		t.Fatal(err)
	}
	toolBinaries["restic"] = []string{other}
	if sb, took := ping(); took >= slow || sb.Restic != "" || sb.ResticVersion != "" || sb.ResticError != "" {
		t.Fatalf("a ping while a new binary is tested took %s: %+v", took, sb)
	}
	if r := s.selfTestRestic(ctx); r == nil || r.bin != other || r.err != nil {
		t.Fatalf("the new binary's run: %+v", r)
	}
	if sb, _ := ping(); sb.Restic != other || sb.ResticVersion == "" || sb.ResticError != "" {
		t.Fatalf("the ping after it: %+v", sb)
	}
}

// A run its caller gives up on — the agent stopping while it tests restic
// as it starts — ended restic; it did not find restic failing: nothing is
// kept or said, and the next run tests restic anew.
func TestResticSelfTestGivenUp(t *testing.T) {
	s, bin := sandboxServer(t, needLandlock(t))
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	t.Setenv(fakeResticEnv, "1")
	t.Setenv("MP_FAKE_RESTIC_SLEEP", "5s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if r := s.selfTestRestic(ctx); r != nil || logs.Len() != 0 || time.Since(start) > 4*time.Second {
		t.Fatalf("a run given up on, after %s: %+v\n%s", time.Since(start), r, logs.String())
	}
	t.Setenv("MP_FAKE_RESTIC_SLEEP", "")
	if r := s.selfTestRestic(context.Background()); r == nil || r.err != nil || r.bin != bin || !strings.Contains(logs.String(), `msg="restic starts in its sandbox"`) {
		t.Fatalf("the next run: %+v\n%s", r, logs.String())
	}
}

// The self-test's verdict as the ping carries it to the doctor, which fails
// on an error and warns of a timeout: a restic version that did not finish
// in time timed out (restic_timeout) — a warning in the agent's log, which
// says the host may only have been busy, when the test runs again and what
// to look at —; one that exited with an error, was killed by a signal or
// whose sandbox could not be started at all failed, logged as before and
// without the flag. A verdict is logged when it changes, not while it
// stands, and a run the agent gives up on is none. Scripts stand in for
// sandbox-exec here, doing what restic in its sandbox would: this runs on a
// kernel without Landlock too.
func TestResticSelfTestTimeoutOrError(t *testing.T) {
	s, bin := sandboxServer(t, 6)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	old := resticSelfTestTimeout
	t.Cleanup(func() { resticSelfTestTimeout = old })
	// the scripts are written before anything runs: a file still open for
	// writing in a child forked meanwhile cannot be executed (ETXTBSY)
	dir := t.TempDir()
	script := map[string]string{}
	for name, body := range map[string]string{
		"hangs":  "exec sleep 10",
		"fails":  "echo 'Fatal: unable to start: operation not permitted' >&2; exit 1",
		"killed": "kill -KILL $$",
		"starts": "echo 'restic 0.18.1 compiled with go1.25.5 on linux/amd64'",
	} {
		script[name] = filepath.Join(dir, name)
		if err := os.WriteFile(script[name], []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	// run has the sandbox executable be exe and the same binary tested
	// again, as a ping does once a failure is ResticRetestAfter old, with
	// the time a test has: short for the scripts that hang, ample for those
	// that do not, which a loaded machine must not see time out
	const hang, ample = 300 * time.Millisecond, 20 * time.Second
	run := func(exe string, timeout time.Duration) *resticSelfTest {
		t.Helper()
		s.sandboxExe, resticSelfTestTimeout = exe, timeout
		if last := s.resticTest.Load(); last != nil {
			last.at = time.Now().Add(-ResticRetestAfter - time.Minute)
		}
		logs.Reset()
		return s.selfTestRestic(ctx)
	}
	ping := func() (*SandboxInfo, string) {
		t.Helper()
		p, err := s.ping(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(p.Sandbox)
		return p.Sandbox, string(b)
	}

	// restic version hangs, and the deadline kills it
	start := time.Now()
	r := run(script["hangs"], hang)
	wantErr := "restic version did not finish within 300ms"
	if r == nil || !r.timedOut || r.err == nil || r.err.Error() != wantErr || r.version != "" || r.bin != bin || time.Since(start) > 4*time.Second {
		t.Fatalf("a restic that hangs, after %s: %+v", time.Since(start), r)
	}
	if l := logs.String(); !strings.Contains(l, "level=WARN") || !strings.Contains(l, "restic took too long to start in its sandbox") || !strings.Contains(l, "too busy") ||
		!strings.Contains(l, "first ping 10 minutes after this run") || !strings.Contains(l, "backups hang the same way") || !strings.Contains(l, wantErr) || strings.Contains(l, "does not start") {
		t.Fatalf("log of a timeout: %s", l)
	}
	if sb, b := ping(); sb.Restic != bin || sb.ResticError != wantErr || !sb.ResticTimeout || sb.ResticVersion != "" || !strings.Contains(b, `"restic_timeout":true`) {
		t.Fatalf("ping of a timeout: %s", b)
	}
	// timing out again is no news
	if r = run(script["hangs"], hang); r == nil || !r.timedOut || logs.Len() != 0 {
		t.Fatalf("timed out again: %+v\n%s", r, logs.String())
	}

	// restic exits with an error, and says why
	r = run(script["fails"], ample)
	wantErr = "Fatal: unable to start: operation not permitted (exit 1)"
	if r == nil || r.timedOut || r.err == nil || r.err.Error() != wantErr {
		t.Fatalf("a restic that exits with an error: %+v", r)
	}
	if l := logs.String(); !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "restic does not start in its sandbox") || !strings.Contains(l, wantErr) || strings.Contains(l, "too busy") {
		t.Fatalf("log of an error: %s", l)
	}
	if sb, b := ping(); sb.ResticError != wantErr || sb.ResticTimeout || strings.Contains(b, "restic_timeout") {
		t.Fatalf("ping of an error: %s", b)
	}

	// killed by a signal that is not the deadline's (the OOM killer)
	if r = run(script["killed"], ample); r == nil || r.timedOut || r.err == nil || r.err.Error() != "it said nothing (exit -1)" || !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a restic killed: %+v\n%s", r, logs.String())
	}

	// the sandbox cannot be started at all
	if r = run(filepath.Join(dir, "missing"), ample); r == nil || r.timedOut || !errors.Is(r.err, fs.ErrNotExist) || !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a sandbox that cannot be started: %+v\n%s", r, logs.String())
	}
	if sb, b := ping(); sb.ResticError != r.err.Error() || sb.ResticTimeout || strings.Contains(b, "restic_timeout") {
		t.Fatalf("ping of a sandbox that cannot be started: %s", b)
	}

	// a run the agent gives up on while restic hangs is no verdict: the last
	// one stands, and nothing is said
	last := s.resticTest.Load()
	last.at = time.Now().Add(-ResticRetestAfter - time.Minute)
	s.sandboxExe, resticSelfTestTimeout = script["hangs"], ample
	gctx, giveUp := context.WithCancel(ctx)
	defer giveUp()
	time.AfterFunc(100*time.Millisecond, giveUp)
	logs.Reset()
	if r = s.selfTestRestic(gctx); r != nil || s.resticTest.Load() != last || logs.Len() != 0 {
		t.Fatalf("a run given up on: %+v, kept %+v\n%s", r, s.resticTest.Load(), logs.String())
	}

	// and a restic that starts
	if r = run(script["starts"], ample); r == nil || r.timedOut || r.err != nil || r.version != "restic 0.18.1 compiled with go1.25.5 on linux/amd64" || !strings.Contains(logs.String(), "level=INFO") {
		t.Fatalf("a restic that starts: %+v\n%s", r, logs.String())
	}
	if sb, b := ping(); sb.ResticError != "" || sb.ResticTimeout || sb.ResticVersion != r.version || strings.Contains(b, "restic_timeout") {
		t.Fatalf("ping of a restic that starts: %s", b)
	}
}

// lockedBuffer is a buffer a goroutine writes while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}
