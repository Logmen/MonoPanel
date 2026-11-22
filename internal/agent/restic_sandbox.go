package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
	"monopanel/internal/sandbox"
)

// restic runs as root and walks what it backs up by path, and O_NOFOLLOW
// guards only the last component of a path: an account that swaps a
// directory of its own for a symlink to / while restic is inside it (inotify
// tells it when) has root read /etc/shadow, /root/.ssh or the agent's state
// through the link and store them as the account's own files, which an
// in-place restore of the account then hands it. So every restic the agent
// runs runs in a Landlock sandbox (internal/sandbox): a backup reads only
// below the paths it backs up, pinned by descriptor when the policy checked
// them, a restore writes only below its target, and both reach besides only
// restic's cache and temporary directory, a local repository and the
// world-readable system files restic and ssh read — those of /etc in a
// private /etc of restic's own, where the host can rename nothing away from
// under it (sandbox.Rules.Etc). Landlock decides on the file a path resolves
// to, so a link that leads out of those trees leads nowhere, and restic
// reports the file as one it could not read. It grants a tree however it is
// reached, though, and a server backup reads the panel's configuration,
// certificates and dumps and every home: so a backup also reads every tree it
// saves in a view that follows no symlink at all (sandbox.Rules.NoSymlinks),
// where the agent may mount.
//
// That view needs a mount namespace, not Landlock, and restic stores a
// symlink as one and follows none: on a kernel without Landlock — older than
// 5.13, or booted with an lsm= list that leaves it out, as Oracle Linux's UEK
// is — a backup still runs in it (sandbox-exec --no-landlock), and the race
// stays closed there too; what such a kernel loses is the rest of the
// confinement. The agent says so when it starts and the doctor shows it, with
// how to turn Landlock on.
//
// A restic the sandbox keeps from what it reads as it starts cannot back up
// at all — EPEL's, built with Red Hat's Go, ended on the kernel's FIPS switch
// (resticSystemReads) —, so the agent runs restic version in a backup's
// sandbox as it starts and once the binary changed (selfTestRestic), and the
// doctor says when restic does not start there.
//
// Every check of the policy stays as it is (resticBackupPath, resticEnv, the
// shapes): the sandbox is a second wall, not the first.

// resticSystemReads are the system's files restic and the programs it runs
// read: the account databases (restic records owners by name, ssh needs
// root's entry), name resolution (mdns.allow: Go's resolver looks for it when
// nsswitch.conf names mdns), the time zone, the CA certificates of the
// remote backends where Go's crypto/x509 looks for them, the configuration
// of OpenSSL and of EL's crypto policies (ssh's libcrypto reads it), and the
// dynamic loader's cache and preloads. All of them are world-readable; what
// is not — /etc/shadow, the host keys in /etc/ssh, /etc/ssl/private,
// /etc/pki/tls/private, root's home — stays out of reach, and so does
// whatever a backed-up home points at there.
//
// Of /proc one file alone, the kernel's FIPS switch (fipsSwitch): Red Hat's
// Go — EPEL builds restic with it — reads it as every program it built
// starts, and a refusal ends the program there (golang-fips: hostfips,
// crypto/internal/backend), before restic has parsed its arguments; EL's
// libcrypto, ssh's too, reads it to decide on FIPS mode and takes a refusal
// for "off". In FIPS mode that Go up to go1.26 does its crypto in OpenSSL,
// as ssh always does: libcrypto and the FIPS provider lie in /usr/lib64
// (resticSystemExecs), openssl.cnf and the crypto policies are here, and so
// is the provider's configuration, fips_local.cnf — a link to the crypto
// policies' back-end, which libcrypto loads in FIPS mode alone; without it
// the provider would run without what the policy sets. The third-party
// providers of /etc/pki/tls/openssl.d stay out: restic and ssh need none,
// libcrypto skips an include that is not there, and a provider loaded in the
// sandbox would find little of what it reads.
var resticSystemReads = []string{
	"/etc/passwd", "/etc/group", "/etc/nsswitch.conf",
	"/etc/hosts", "/etc/host.conf", "/etc/resolv.conf", "/etc/gai.conf", "/etc/services", "/etc/protocols", "/etc/mdns.allow",
	"/etc/localtime", "/usr/share/zoneinfo",
	"/etc/ssl/certs", "/etc/ssl/openssl.cnf", "/etc/pki/tls/certs", "/etc/pki/tls/openssl.cnf", "/etc/pki/tls/fips_local.cnf", "/etc/pki/ca-trust",
	"/etc/ca-certificates", "/usr/share/ca-certificates", "/usr/local/share/ca-certificates",
	"/etc/crypto-policies", "/usr/share/crypto-policies",
	"/etc/ld.so.cache", "/etc/ld.so.preload",
	"/dev/urandom",
	fipsSwitch,
}

// fipsSwitch is where the kernel says whether it runs in FIPS mode: 0444,
// "0" or "1", and missing from a kernel built without CONFIG_CRYPTO_FIPS,
// which Red Hat's Go and libcrypto take for "off" — and then no rule names
// it (systemPaths).
const fipsSwitch = "/proc/sys/crypto/fips_enabled"

// The names of resticSystemReads below /etc and the rest: the former go into
// the private /etc where the agent may mount (resticRules).
var resticEtcReads, resticOtherReads = splitEtc(resticSystemReads)

// splitEtc tells the paths below /etc from the others.
func splitEtc(paths []string) (etc, other []string) {
	for _, p := range paths {
		if below(p, "/etc") {
			etc = append(etc, p)
		} else {
			other = append(other, p)
		}
	}
	return etc, other
}

// resticSystemExecs are the dynamic loader and the libraries: some
// distributions link restic against libc, and ssh always is.
var resticSystemExecs = []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64"}

// sshBinaries is the ssh an sftp repository is reached with.
var sshBinaries = []string{"/usr/bin/ssh"}

// logSandbox says once, when the agent starts, how restic runs.
func (s *Server) logSandbox() {
	if s.landlock < 1 {
		attrs := []any{"err", s.landlockErr}
		if hint := s.landlockHint(); hint != "" {
			attrs = append(attrs, "enable", hint)
		}
		if s.noSymfollow {
			s.log.Warn("Landlock is unavailable: restic backs up and restores as root without its sandbox; a backup still reads every tree it saves in a view that follows no symlink, so a directory an account swaps for a symlink leads nowhere, but nothing else confines restic", attrs...)
			return
		}
		attrs = append(attrs, "nosymfollow_err", s.noSymfollowErr)
		s.log.Warn("Landlock is unavailable: restic backs up and restores as root without its sandbox, and an account that swaps a directory of its own for a symlink while a backup reads its home can have root store files outside it (/etc/shadow, root's keys) as the account's own; boot a kernel with landlock in its lsm= list", attrs...)
		return
	}
	if s.noSymfollowErr != nil {
		s.log.Warn("restic runs in a Landlock sandbox, but a backup follows the symlinks in the trees it reads: during a server backup, which reads the panel's configuration, certificates and dumps as well, an account that swaps a directory of its own for a symlink can have those, or another account's files, stored as its own", "abi", s.landlock, "err", s.noSymfollowErr)
	}
	if s.privateEtcErr != nil {
		s.log.Warn("restic reads /etc/resolv.conf, /etc/hosts and /etc/passwd in its sandbox by inode: when the host replaces one of them during a run (NetworkManager, dhclient, useradd), restic loses name resolution, and with it a remote repository, or owner names; the agent cannot give it a private /etc", "abi", s.landlock, "err", s.privateEtcErr)
	}
	if s.noSymfollowErr == nil && s.privateEtcErr == nil {
		s.log.Info("restic runs in a Landlock sandbox", "abi", s.landlock, "nosymfollow", s.noSymfollow, "private_etc", s.privateEtc)
	}
}

// landlockHint says how to turn Landlock on where the kernel has it built in
// but the boot left it out of its LSM list (EOPNOTSUPP), "" where there is
// nothing to turn on (ENOSYS: the kernel lacks it). Oracle Linux's UEK is
// such a kernel; its Red Hat compatible kernel, like the one of RHEL and its
// rebuilds, has Landlock on.
func (s *Server) landlockHint() string {
	if !errors.Is(s.landlockErr, unix.EOPNOTSUPP) {
		return ""
	}
	var uts unix.Utsname
	kernel := ""
	if unix.Uname(&uts) == nil {
		kernel = unix.ByteSliceToString(uts.Release[:])
	}
	lsm, _ := os.ReadFile("/sys/kernel/security/lsm")
	return landlockHint(s.profile, kernel, strings.TrimSpace(string(lsm)))
}

// landlockHint is the advice for a kernel of that release whose active LSMs
// are lsm (/sys/kernel/security/lsm): the same list with landlock added, as
// the boot parameter that sets it. lsm= replaces the kernel's list, so the
// modules it has (SELinux, AppArmor) stay in it.
func landlockHint(profile osprofile.Profile, kernel, lsm string) string {
	list := "landlock"
	if lsm != "" {
		list = lsm + ",landlock"
	}
	h := "Landlock is built into this kernel but not enabled: add it to the kernel's lsm= list and reboot — "
	if profile != nil && profile.Family() == osprofile.FamilyDebian {
		h += "lsm=" + list + " in GRUB_CMDLINE_LINUX of /etc/default/grub, then update-grub"
	} else {
		h += "grubby --update-kernel=ALL --args=lsm=" + list
	}
	if strings.Contains(kernel, "uek") {
		h += "; or boot the Red Hat compatible kernel, which has it on (dnf install kernel, then grubby --set-default=/boot/vmlinuz-<its version>)"
	}
	return h
}

// probeNoSymfollow finds out once, as the agent starts, what restic's
// sandbox can be given besides Landlock: the view without symlinks of the
// trees a backup reads (sandbox.Rules.NoSymlinks, resticRules) — with
// Landlock or, where the kernel has none, without — and, with Landlock, the
// private /etc (sandbox.Rules.Etc). Both take a mount namespace, the view the
// new mount API (Linux 5.14), which a container's profile may refuse.
// Without them restic still runs as the kernel allows, and the agent and the
// doctor say what that leaves open.
func (s *Server) probeNoSymfollow(ctx context.Context) {
	s.noSymfollow, s.noSymfollowErr = false, nil
	s.privateEtc, s.privateEtcErr = false, nil
	if os.Geteuid() != 0 {
		s.noSymfollowErr = errors.New("the agent does not run as root")
		if s.landlock > 0 {
			s.privateEtcErr = s.noSymfollowErr
		}
		return
	}
	dir, err := filepath.EvalSymlinks(s.cfg.WWWRoot)
	if err != nil || !filepath.IsAbs(dir) {
		dir = os.TempDir() // no web root yet: any directory tells as much
	}
	probe := func(rules sandbox.Rules) error {
		argv, _ := rules.CheckCommand()
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(pctx, s.sandboxExe, argv...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, firstLines(string(out), "nothing said"))
		}
		return nil
	}
	if s.noSymfollowErr = probe(sandbox.Rules{NoSymlinks: []string{dir}, NoLandlock: s.landlock < 1}); s.noSymfollowErr == nil {
		s.noSymfollow = true
	}
	if s.landlock < 1 {
		return
	}
	if s.privateEtcErr = probe(sandbox.Rules{Etc: resticEtcReads}); s.privateEtcErr == nil {
		s.privateEtc = true
	}
}

// resticSelfTest is what came of running a restic binary in its sandbox
// (selfTestRestic).
type resticSelfTest struct {
	bin string
	id  binaryID
	at  time.Time
	// version is what restic said when it started, err why it did not
	version string
	err     error
	// timedOut: err is restic version not finishing within
	// resticSelfTestTimeout, not restic ending with an error. A host too
	// busy at that moment does that as well, and the next test may pass;
	// an error fails every backup the same way. The doctor warns of the
	// one and fails on the other, so the ping says which (SandboxInfo).
	timedOut bool
}

// binaryID tells one restic binary from another: an upgrade renames a new
// file over the old one, a reinstall writes it anew.
type binaryID struct {
	dev, ino     uint64
	size         int64
	mtime, ctime unix.Timespec
}

func statBinary(p string) (binaryID, error) {
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		return binaryID{}, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	return binaryID{dev: st.Dev, ino: st.Ino, size: st.Size, mtime: st.Mtim, ctime: st.Ctim}, nil
}

// resticSelfTestTimeout bounds the self-test's run: restic version prints a
// line and exits within a fraction of a second.
var resticSelfTestTimeout = 30 * time.Second

// ResticRetestAfter is how long a failed self-test stands before a ping runs
// it again on the same binary: a failure that came of a moment's trouble — a
// host too busy to start restic in time — does not stay in the doctor until
// restic or the agent change. Exported for the doctor, which tells when a
// self-test that timed out is run again.
const ResticRetestAfter = 10 * time.Minute

// selfTestRestic runs the installed restic's `restic version` in the sandbox
// a backup's restic gets — the rules of resticRules, the private /etc and the
// mount namespace, as confineRestic builds them and planRunner starts them —
// and keeps what came of it for the ping, and so for the doctor. A restic
// that cannot start there fails every backup the same way, at once, and
// every other run the sandbox confines: EPEL's, built with Red Hat's Go,
// exited on its first read of /proc/sys/crypto/fips_enabled, which the
// sandbox did not grant then. The next such read — a new toolchain, a new
// restic — this finds before a backup does; restic version needs no
// repository and reads nothing of anybody's.
//
// The agent runs it as it starts, in the background — it changes nothing
// the agent does, so the API need not wait for it —, and a ping again once
// the binary is another one — the panel installs restic with a host's first
// backup, the package manager upgrades it while the agent runs — or a
// failure is older than ResticRetestAfter (pingResticSelfTest). A failure
// changes nothing else: restic keeps running in its sandbox and never
// outside it, and a backup fails as it would — the doctor and the agent's
// log say why and what to do. A restic version that did not finish within
// resticSelfTestTimeout is kept apart from one that ended with an error
// (timedOut): a host too busy at that moment does that as well, and the
// doctor warns of it where it fails on an error, until the next test tells
// more.
//
// nil: nothing to test — restic is not installed, a backup's restic runs
// without a sandbox (no Landlock and no view), or the agent has no
// sandbox-exec (a test's bare Server) —, or nothing found: ctx ended while
// restic ran (the agent stopping), which ended restic rather than found it
// failing.
func (s *Server) selfTestRestic(ctx context.Context) *resticSelfTest {
	if s.sandboxExe == "" || s.landlock < 1 && !s.noSymfollow {
		return nil
	}
	s.resticTestMu.Lock()
	defer s.resticTestMu.Unlock()
	bin, id, ok := installedRestic()
	if !ok {
		// not installed, or gone again: nothing to say
		s.resticTest.Store(nil)
		return nil
	}
	last := s.resticTest.Load()
	if last != nil && last.bin == bin && last.id == id && (last.err == nil || time.Since(last.at) < ResticRetestAfter) {
		return last
	}
	t := &resticSelfTest{bin: bin, id: id, at: time.Now()}
	t.version, t.timedOut, t.err = s.runResticSelfTest(ctx, bin)
	if ctx.Err() != nil {
		return nil
	}
	s.resticTest.Store(t)
	if last == nil || last.bin != t.bin || last.version != t.version || fmt.Sprint(last.err) != fmt.Sprint(t.err) {
		s.logResticSelfTest(t)
	}
	return t
}

// installedRestic is the restic binary the agent runs (resolveTool) and
// what tells it from another one; ok is false when restic is not installed.
func installedRestic() (string, binaryID, bool) {
	bin, err := resolveTool("restic")
	if err != nil {
		return "", binaryID{}, false
	}
	id, err := statBinary(bin)
	if err != nil {
		return "", binaryID{}, false
	}
	return bin, id, true
}

// resticPingWait is how long a ping waits for a self-test it sets off or
// finds under way. restic version is done in a fraction of a second, but one
// that hangs is found out only after resticSelfTestTimeout, and a status
// page gives up on the agent after 5 s, the API's wait for the agent after
// 10 s: they would report an agent that does not answer.
var resticPingWait = 2 * time.Second

// pingResticSelfTest is what a ping says of the self-test: selfTestRestic,
// which tests a binary installed or upgraded since the last run — waited for
// no longer than resticPingWait. The run then goes on, and a later ping has
// its outcome; this one has what the last run found of the binary installed
// now — a failure being tested again stays in the doctor meanwhile, a binary
// not tested yet has nothing.
func (s *Server) pingResticSelfTest(ctx context.Context) *resticSelfTest {
	done := make(chan *resticSelfTest, 1)
	go func() { done <- s.selfTestRestic(context.WithoutCancel(ctx)) }()
	wait := time.NewTimer(resticPingWait)
	defer wait.Stop()
	select {
	case t := <-done:
		return t
	case <-wait.C:
	}
	last := s.resticTest.Load()
	if bin, id, ok := installedRestic(); !ok || last == nil || last.bin != bin || last.id != id {
		return nil
	}
	return last
}

// runResticSelfTest runs bin's restic version in its sandbox (selfTestRestic)
// and returns what restic said, and whether it did not finish in time.
func (s *Server) runResticSelfTest(ctx context.Context, bin string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, resticSelfTestTimeout)
	defer cancel()
	plan := s.resticSelfTestPlan()
	defer plan.close()
	out := &tailBuffer{max: 1 << 20}
	code, err := planRunner(ctx, plan, bin, "", out)(plan.args, nil)
	return resticSelfTestOutcome(ctx, code, err, out.String())
}

// resticSelfTestPlan is the self-test's plan: restic version, with the
// environment and the sandbox a plan of planRestic's has, but for the
// repository, which version needs none of.
func (s *Server) resticSelfTestPlan() *toolPlan {
	return &toolPlan{args: []string{"version"}, env: []string{"RESTIC_CACHE_DIR=" + s.resticCacheDir()}, confine: s.confineRestic("", nil)}
}

// resticSelfTestOutcome is what the self-test's restic said: the first line
// of a restic that started (restic 0.19.1 compiled with go1.26.4 …), and of
// one that did not its own error line with the exit status — or why it did
// not run at all. timedOut: it did not finish within resticSelfTestTimeout
// (the deadline of ctx killed it), which is told apart from the rest here,
// where it is known, rather than by the error's words later.
func resticSelfTestOutcome(ctx context.Context, code int, err error, out string) (version string, timedOut bool, _ error) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", true, fmt.Errorf("restic version did not finish within %s", resticSelfTestTimeout)
	case ctx.Err() != nil:
		return "", false, ctx.Err()
	case err != nil:
		return "", false, err
	case code != 0:
		return "", false, fmt.Errorf("%s (exit %d)", firstLine(out, "it said nothing"), code)
	}
	return firstLine(out, ""), false, nil
}

// logResticSelfTest says in the agent's log what the self-test found, at the
// level the doctor gives it: a restic that does not start fails every
// backup; one that did not finish in time may only have met a busy host, and
// is tested again — which a warning says in words of its own.
func (s *Server) logResticSelfTest(t *resticSelfTest) {
	switch {
	case t.timedOut:
		s.log.Warn(fmt.Sprintf("restic took too long to start in its sandbox: the host may only have been too busy at that moment, and the agent tests it again on the first ping %d minutes after this run. If it keeps timing out, backups hang the same way: look at the host's load, memory and disk I/O, and at how long restic version takes outside the panel; if it answers there at once, report this to the panel's developers", int(ResticRetestAfter/time.Minute)), "restic", t.bin, "err", t.err)
	case t.err != nil:
		s.log.Error("restic does not start in its sandbox: every backup fails the same way, and so does every other restic run the sandbox confines; the agent does not run restic outside it. Report restic's error, which names what the sandbox kept from it, to the panel's developers and upgrade the panel once a release fixes it", "restic", t.bin, "err", t.err)
	default:
		s.log.Info("restic starts in its sandbox", "restic", t.bin, "version", t.version)
	}
}

// sandboxRestic has each run of the plan's restic go through the sandbox.
// sources are the backed-up paths pinned for it (pinBackupSources), closed
// with the plan.
//
// A restore stays out of it on Landlock ABI 1: that version knows no REFER
// right and refuses every link and rename into another directory, and
// restic links a snapshot's hard links wherever they are. The restore
// writes into a directory of the agent's alone anyway, and the agent moves
// what came out into place itself (restoreInPlace).
//
// Without Landlock a backup still goes through sandbox-exec, for its view
// without symlinks alone; the rest runs as it is.
func (s *Server) sandboxRestic(plan *toolPlan, sources []*os.File) {
	for _, f := range sources {
		plan.closers = append(plan.closers, f)
	}
	switch {
	case s.landlock >= 2, s.landlock == 1 && plan.args[0] != "restore":
		prepare := plan.prepare
		plan.prepare = func() error {
			if err := prepare(); err != nil {
				return err
			}
			fd, err := ensureRootDir(s.resticTmpDir(), 0o700)
			if err == nil {
				unix.Close(fd) //nolint:errcheck // a directory opened for reading
			}
			return err
		}
	case s.landlock < 1 && s.noSymfollow && plan.args[0] == "backup":
		// the view alone (confineRestic)
	default:
		return
	}
	repo := ""
	for _, e := range plan.env {
		if v, ok := strings.CutPrefix(e, "RESTIC_REPOSITORY="); ok {
			repo = v
		}
	}
	plan.confine = s.confineRestic(repo, sources)
}

// pinBackupSources opens the backed-up paths for the sandbox, each reached
// the way resticBackupPath checked it — below its tree without following a
// symlink — and opened itself without following one: the rule then grants
// that very directory or file, whatever is renamed or swapped on the way to
// it afterwards. A path that is a symlink gets no rule: restic stores the
// link itself and reads nothing through it. A path that is not there gets
// none either; restic reports it.
func (s *Server) pinBackupSources(paths []string) ([]*os.File, error) {
	var out []*os.File
	for _, p := range paths {
		f, err := s.pinBackupSource(p)
		if err != nil {
			for _, o := range out {
				o.Close()
			}
			return nil, err
		}
		if f != nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Server) pinBackupSource(p string) (*os.File, error) {
	root := s.resticBackupRoot(p)
	if root == "" || !cleanAbs(p) {
		return nil, notAllowed("a backup of "+p, "not a path the panel backs up")
	}
	dir, base := filepath.Dir(p), filepath.Base(p)
	var dfd int
	var err error
	if atOrBelow(dir, root) {
		dfd, err = openDirBelow(root, dir, false, 0)
	} else {
		// the tree itself, reached as the system's directories are
		dfd, err = openDirTrusted(dir)
	}
	if err != nil {
		return nil, notAllowed("a backup of "+p, err.Error())
	}
	defer unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	fd, err := unix.Openat(dfd, base, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd) //nolint:errcheck // a descriptor of a path
		return nil, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if t := st.Mode & unix.S_IFMT; t != unix.S_IFDIR && t != unix.S_IFREG {
		unix.Close(fd) //nolint:errcheck // a descriptor of a path
		return nil, nil
	}
	return os.NewFile(uintptr(fd), p), nil
}

// resticAccess is what one run of restic may reach in its sandbox, as
// resticRules lays it out.
type resticAccess struct {
	// ro, rw and rx are paths: the system's files, restic's cache and
	// temporary directory, /dev/null, the restic and ssh binaries and the
	// libraries, with the system's symlinks resolved.
	ro, rw, rx []string
	// etc are the system's files of /etc restic reads, in a private /etc
	// where the agent may mount (sandbox.Rules.Etc) — then not in ro — and
	// etcBind what a backup saves below /etc (the panel's configuration),
	// bound into it as it is.
	etc, etcBind []string
	// sources: a backup reads the backed-up paths (sandboxRestic's).
	sources bool
	// target is a restore's target below RestoreDir or the restore area of
	// the web root (restoreInPlace), made when missing.
	target string
	// repo is a local repository, written to.
	repo string
	// sftp is the server of an sftp repository: ssh reaches it through a
	// connection the agent opens outside the sandbox (sshMaster).
	sftp *sftpRemote
	// noSymlinks are the trees a backup reads, resolved (viewRoots): restic
	// follows no symlink below them (sandbox.Rules.NoSymlinks). The Landlock
	// rules alone would let a home's symlink lead into every other tree the
	// run reads — a server backup reads the panel's configuration,
	// certificates and dumps and all the homes — and without Landlock into
	// anything at all; restic never follows one there itself: a symlink it
	// backs up it stores as one, and the policy accepts no path with a
	// symlink on the way below a tree (resticBackupPath).
	noSymlinks []string
}

// resticRules is the one place that says what restic may reach for a run
// with args (as the policy and its wrappers built them) against the
// repository repo, bin being the restic binary.
func (s *Server) resticRules(bin string, args []string, repo string) (*resticAccess, error) {
	if len(args) == 0 {
		return nil, errors.New("restic: no command")
	}
	paths := resticBackupArgs(args)
	acc := &resticAccess{
		rw: systemPaths([]string{s.resticCacheDir(), s.resticTmpDir(), "/dev/null"}),
	}
	if bind, ok := etcSources(paths); s.privateEtc && ok {
		acc.etc, acc.etcBind = existingPaths(resticEtcReads), bind
		acc.ro = systemPaths(resticOtherReads)
		// resolv.conf as a symlink into /run (systemd-resolved,
		// NetworkManager) stays one, and its directory is granted, as
		// without the private /etc: a copy would keep the name servers
		// of the start, the directory follows them
		if dir, ok := runLinkDir(resolvConf); ok {
			acc.etc = slices.DeleteFunc(acc.etc, func(p string) bool { return p == resolvConf })
			acc.etcBind = append(acc.etcBind, resolvConf)
			acc.ro = append(acc.ro, dir)
		}
	} else {
		// a rule per file, as the host has them now
		acc.ro = systemPaths(resticSystemReads)
	}
	execs := append([]string{bin}, resticSystemExecs...)
	switch {
	case strings.HasPrefix(repo, "/"):
		acc.repo = repo
	case strings.HasPrefix(repo, "sftp:"):
		r, err := parseSFTPRepository(repo)
		if err != nil {
			return nil, notAllowed("the repository "+strconv.Quote(repo), err.Error())
		}
		acc.sftp = &r
		execs = append(execs, sshBinaries...)
	}
	acc.rx = systemPaths(execs)
	switch args[0] {
	case "backup":
		acc.sources = true
		if s.noSymfollow {
			roots, err := s.viewRoots(paths)
			if err != nil {
				return nil, err
			}
			acc.noSymlinks = roots
		}
	case "restore":
		i := slices.Index(args, "--target")
		if i < 0 || i+1 >= len(args) || !cleanAbs(args[i+1]) || s.restoreBase(args[i+1]) == "" {
			return nil, notAllowed("a restore of "+strings.Join(args, " "), "restic restores into "+RestoreDir+" and the agent's staging of in-place restores alone")
		}
		acc.target = args[i+1]
	case "ls", "snapshots", "cat", "init", "forget":
	case "version":
		// the agent's own test of the sandbox (selfTestRestic), which the
		// policy never takes from the API
	default:
		return nil, notAllowed("restic "+args[0]+" in the sandbox", "not a command the panel runs")
	}
	return acc, nil
}

// resticBackupArgs are the paths a restic backup of args saves: what follows
// its tags, without the options the agent added (--exclude=).
func resticBackupArgs(args []string) []string {
	if len(args) < 7 || args[0] != "backup" {
		return nil
	}
	var out []string
	for _, a := range args[6:] {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// etcSources are the backed-up paths below /etc, which a private /etc has to
// hold as they are: the panel's configuration. ok is false when one cannot be
// put there next to the system's entries — it is /etc itself, or holds one
// of them or lies in one — and the run then reads the system's /etc by the
// rules of its files.
func etcSources(paths []string) ([]string, bool) {
	var out []string
	for _, p := range paths {
		if !atOrBelow(p, "/etc") {
			continue
		}
		if p == "/etc" || slices.ContainsFunc(resticEtcReads, func(e string) bool { return atOrBelow(p, e) || below(e, p) }) {
			return nil, false
		}
		out = append(out, p)
	}
	return outermost(out), true
}

// outermost are dirs without duplicates and without those below another one
// of them, in their order.
func outermost(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if !slices.ContainsFunc(dirs, func(o string) bool { return below(d, o) }) && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// existingPaths are the paths there are, followed through their symlinks.
func existingPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// viewRoots are the directories a backup of paths sees without symlinks:
// the tree each path lies in — the web root, the configuration directory,
// the data directory, which holds the certificates, the ACME account and the
// run's dumps — reached as the system's directories are and resolved
// (viewRoot), each once, none below another.
func (s *Server) viewRoots(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		tree := s.resticBackupRoot(p)
		if tree == "" {
			continue // not a path the policy lets through
		}
		d, err := s.viewRoot(tree)
		if errors.Is(err, os.ErrNotExist) {
			continue // nothing to read below it: restic says so
		}
		if err != nil {
			return nil, notAllowed("a backup of "+p, "the view of "+tree+" without symlinks: "+err.Error())
		}
		out = append(out, d)
	}
	return outermost(out), nil
}

// viewRoot is where the view without symlinks of the tree goes: the tree,
// resolved — or the directory above it whose name nobody but root may change.
// A view is mounted on a directory, not on its name: one in a directory the
// API may rearrange would leave with its directory when the API renames that,
// and whatever the API puts under the name is read as it is. The data
// directory is the API's, but it lies in root's /var/lib, so the view goes
// on the data directory, not on the certificates and dumps in it.
func (s *Server) viewRoot(tree string) (string, error) {
	fd, err := openDirTrusted(tree)
	if err != nil {
		return "", err
	}
	real, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	unix.Close(fd) //nolint:errcheck // a directory opened for its path
	if err != nil {
		return "", err
	}
	t := s.trust()
	for d := real; d != "/"; d = filepath.Dir(d) {
		var parent, self unix.Stat_t
		if err := unix.Lstat(filepath.Dir(d), &parent); err != nil {
			return "", &os.PathError{Op: "stat", Path: filepath.Dir(d), Err: err}
		}
		if err := unix.Lstat(d, &self); err != nil {
			return "", &os.PathError{Op: "stat", Path: d, Err: err}
		}
		// in a sticky directory of root's only an entry's owner renames it
		if t.rootOwned(parent.Uid) && (parent.Mode&0o022 == 0 || parent.Mode&unix.S_ISVTX != 0 && t.rootOwned(self.Uid)) {
			return d, nil
		}
	}
	return "", fmt.Errorf("no directory above %s has a name only root may change", real)
}

// resolvConf is where the resolvers look for their name servers.
const resolvConf = "/etc/resolv.conf"

// runLinkDir is the directory in /run that the symlink link leads to
// straight away — not through another name of /etc, which a private /etc
// lacks — and that holds the file it ends at.
func runLinkDir(link string) (string, bool) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", false
	}
	return linkDirIn(link, target, resolved, "/run")
}

// linkDirIn is runLinkDir of a link with that target that resolves to
// resolved, for the directory run.
func linkDirIn(link, target, resolved, run string) (string, bool) {
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if !below(filepath.Clean(target), run) || !below(resolved, run) {
		return "", false
	}
	return filepath.Dir(resolved), true
}

// systemPaths are paths with the system's own symlinks resolved — the
// sandbox takes no symlink, and Landlock looks at the file a path resolves
// to anyway — the missing ones left out, each once. resolv.conf kept below
// /run (systemd-resolved, NetworkManager) goes in by its directory: they
// replace the file by renaming another one over it, and a rule on the file
// would stay with the old one.
func systemPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if p == resolvConf && strings.HasPrefix(r, "/run/") {
			r = filepath.Dir(r)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// confineRestic turns a run of restic into one through sandbox-exec under
// resticRules — without Landlock, into one in the view without symlinks of
// what a backup reads, and nothing else.
func (s *Server) confineRestic(repo string, sources []*os.File) func(ctx context.Context, bin string, args []string, out io.Writer) (*confinedRun, error) {
	return func(ctx context.Context, bin string, args []string, out io.Writer) (*confinedRun, error) {
		acc, err := s.resticRules(bin, args, repo)
		if err != nil {
			return nil, err
		}
		if s.landlock < 1 {
			rules := sandbox.Rules{NoLandlock: true, NoSymlinks: acc.noSymlinks}
			argv, _ := rules.Command(bin, args)
			return &confinedRun{path: s.sandboxExe, args: argv, done: func() {}}, nil
		}
		rules := sandbox.Rules{RO: acc.ro, RW: acc.rw, RX: acc.rx, NoSymlinks: acc.noSymlinks, Etc: acc.etc, EtcBind: acc.etcBind}
		if acc.sources {
			rules.ROFiles = sources
		}
		var opened []*os.File
		var master *sshMaster
		done := func() {
			master.stop(out)
			for _, f := range opened {
				f.Close()
			}
		}
		if acc.target != "" {
			// RestoreDir and the restore area are root's and 0700: what is
			// made below them stays the agent's own until restic has written
			fd, err := openDirBelow(s.restoreBase(acc.target), acc.target, true, 0o700)
			if err != nil {
				return nil, err
			}
			f := os.NewFile(uintptr(fd), acc.target)
			opened = append(opened, f)
			rules.RWFiles = append(rules.RWFiles, f)
		}
		if acc.repo != "" {
			// restic init makes the repository's directory, which the
			// sandbox must name before restic starts: the agent makes it
			f, err := s.openLocalRepository(acc.repo, args[0] == "init")
			if err != nil {
				done()
				return nil, err
			}
			if f != nil {
				opened = append(opened, f)
				rules.RWFiles = append(rules.RWFiles, f)
			}
		}
		if acc.sftp != nil {
			if master, err = startSSHMaster(ctx, *acc.sftp); err != nil {
				done()
				return nil, err
			}
			args = append([]string{"-o", "sftp.command=" + master.mux}, args...)
		}
		argv, files := rules.Command(bin, args)
		return &confinedRun{path: s.sandboxExe, args: argv, env: []string{"TMPDIR=" + s.resticTmpDir()}, files: files, done: done}, nil
	}
}

// openLocalRepository opens a local repository for its rule the way the
// agent reaches the system's directories — a symlink root made in a
// directory of root's (/backup -> /mnt/disk/backup) is followed, any other
// is not — and checks where it ended up as checkResticRepository checks the
// path itself: a repository whose way leads into /etc or the web root is not
// one restic may write to. nil when it is not there: restic says so itself.
//
// For restic init (create) the directory is made first, as restic makes it —
// with the missing parents, 0700 — but as the agent walks: what is there is
// reached as above, and what is missing is made below the directory reached
// last, so that a symlink the API put in a directory of its own does not
// carry root's mkdir elsewhere.
//
// The rule grants restic all of the directory, to read as well: it must be
// restic's own (resticOwnDirectory).
func (s *Server) openLocalRepository(repo string, create bool) (*os.File, error) {
	open := openDirTrusted
	if create {
		open = func(p string) (int, error) { return makeDirTrusted(p, 0o700) }
	}
	fd, err := open(repo)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, notAllowed("the repository "+strconv.Quote(repo), err.Error())
	}
	f := os.NewFile(uintptr(fd), repo)
	real, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err == nil {
		err = s.checkResticRepository(real)
	}
	if err == nil {
		err = resticOwnDirectory(fd, repo)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// makeDirTrusted opens the directory p, made with mode when missing, its
// missing parents too: the part of p that is there is walked as
// openDirTrusted walks it, the rest is made with mkdirat below the
// directory reached last, following no symlink.
func makeDirTrusted(p string, mode uint32) (int, error) {
	var missing []string
	for dir := p; ; dir = filepath.Dir(dir) {
		fd, err := openDirTrusted(dir)
		if err == nil {
			defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
			slices.Reverse(missing)
			return openDirAt(fd, dir, strings.Join(missing, "/"), true, mode)
		}
		if !errors.Is(err, os.ErrNotExist) || dir == "/" {
			return -1, err
		}
		missing = append(missing, filepath.Base(dir))
	}
}

// resticRepoEntries are what restic keeps in a local repository's
// directory (lost+found: a repository a disk of its own holds).
var resticRepoEntries = map[string]bool{"config": true, "data": true, "index": true, "keys": true, "locks": true, "snapshots": true, "lost+found": true}

// resticOwnDirectory refuses a repository directory that holds anything
// besides restic's entries (and the temporary file of a config being
// written, config-tmp-…, which a restic init that broke off leaves). The
// sandbox lets restic read all of its repository, so in a repository named
// /var/backups it would read Debian's copies of /etc/shadow there, in /var
// the agent's state — and a symlink of an account or of the API that leads
// there in a tree being backed up would have restic store them.
func resticOwnDirectory(fd int, repo string) error {
	d, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: repo, Err: err}
	}
	f := os.NewFile(uintptr(d), repo)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !resticRepoEntries[n] && !strings.HasPrefix(n, "config-tmp-") {
			// the names stay out of the message: the API is not to learn
			// what a directory it cannot list holds
			return notAllowed("the repository "+strconv.Quote(repo), "it holds files restic did not make, and restic runs in a sandbox that lets it read all of its repository: a local repository is a directory of its own, such as "+filepath.Join(repo, "monopanel"))
		}
	}
	return nil
}

// sshMaster is the ssh connection an sftp repository is reached through:
// opened by the agent outside the sandbox, as restic's own ssh was, with
// root's keys, configuration and known hosts, and shared with the ssh that
// restic runs in the sandbox (ControlMaster). That ssh reads no key and no
// configuration — the sandbox grants it none: what restic may read, a
// backed-up home could have it read — and only asks the master for a
// session with the sftp subsystem.
type sshMaster struct {
	cmd *exec.Cmd
	dir string
	// mux is restic's sftp.command: the ssh that goes through the master.
	mux    string
	out    *tailBuffer
	exited chan struct{}
}

// sshMasterTimeout bounds how long the master may take to log in.
var sshMasterTimeout = 3 * time.Minute

// sshMasterOptions are added to the master's command line: tests point it
// at a server, key and known hosts of their own.
var sshMasterOptions []string

// muxPathRe: restic splits sftp.command at blanks and quotes.
var muxPathRe = regexp.MustCompile(`^[A-Za-z0-9/._+-]+$`)

// sshMasterBase is where the master's directory goes: the temporary
// directory, which for the agent is its own /tmp (PrivateTmp=) — or /tmp
// itself when TMPDIR names one too long for a socket (108 bytes) or with
// characters restic would split sftp.command at.
func sshMasterBase() string {
	if base := os.TempDir(); muxPathRe.MatchString(base) && len(base) <= 64 {
		return base
	}
	return "/tmp"
}

// startSSHMaster logs in to the repository's server and waits until the
// master listens on its control socket, in a fresh directory of the agent's
// own /tmp (PrivateTmp=, 0700).
func startSSHMaster(ctx context.Context, r sftpRemote) (*sshMaster, error) {
	ssh := ""
	for _, c := range sshBinaries {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			ssh = c
			break
		}
	}
	if ssh == "" {
		return nil, errors.New("ssh is not installed: an sftp repository needs it")
	}
	dir, err := os.MkdirTemp(sshMasterBase(), "mp-ssh-")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "s")
	if !muxPathRe.MatchString(sock) || len(sock) > 100 {
		os.RemoveAll(dir) //nolint:errcheck // the directory just made
		return nil, fmt.Errorf("the ssh control socket %q: not a path restic can be given", sock)
	}
	host := strings.TrimSuffix(strings.TrimPrefix(r.host, "["), "]")
	var to []string
	if r.port != "" {
		to = append(to, "-p", r.port)
	}
	if r.user != "" {
		to = append(to, "-l", r.user)
	}
	args := append([]string{"-N", "-T", "-o", "BatchMode=yes", "-o", "ControlMaster=yes", "-o", "ControlPath=" + sock, "-o", "ControlPersist=no"}, sshMasterOptions...)
	args = append(append(args, to...), "--", host)
	m := &sshMaster{dir: dir, out: &tailBuffer{max: 64 << 10}, exited: make(chan struct{}),
		mux: ssh + " -F none -T -o BatchMode=yes -o ControlMaster=no -o ControlPath=" + sock + " -s -- " + host + " sftp"}
	m.cmd = exec.CommandContext(ctx, ssh, args...)
	m.cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
	m.cmd.Stdout, m.cmd.Stderr = m.out, m.out
	m.cmd.WaitDelay = 5 * time.Second
	if err := m.cmd.Start(); err != nil {
		os.RemoveAll(dir) //nolint:errcheck // the directory just made
		return nil, fmt.Errorf("ssh: %w", err)
	}
	go func() {
		m.cmd.Wait() //nolint:errcheck // the master's status is its output
		close(m.exited)
	}()
	deadline := time.NewTimer(sshMasterTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if st, err := os.Lstat(sock); err == nil && st.Mode()&os.ModeSocket != 0 {
			return m, nil
		}
		select {
		case <-m.exited:
			os.RemoveAll(dir) //nolint:errcheck // the master is gone
			return nil, fmt.Errorf("ssh to %s: %s", host, firstLines(m.out.String(), "it exited without a word"))
		case <-deadline.C:
			m.stop(nil)
			return nil, fmt.Errorf("ssh to %s: not logged in after %s: %s", host, sshMasterTimeout, firstLines(m.out.String(), "nothing said"))
		case <-ctx.Done():
			m.stop(nil)
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// stop ends the master once restic is done with it and passes on what it
// said, if anything, to out.
func (m *sshMaster) stop(out io.Writer) {
	if m == nil {
		return
	}
	m.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // gone already, or going
	select {
	case <-m.exited:
	case <-time.After(5 * time.Second):
		m.cmd.Process.Kill() //nolint:errcheck // gone already, or going
		<-m.exited
	}
	os.RemoveAll(m.dir) //nolint:errcheck // the master's own directory
	if said := strings.TrimSpace(m.out.String()); said != "" && out != nil {
		fmt.Fprintf(out, "ssh: %s\n", said)
	}
}

// firstLine is the first line a program said, cut at 400 bytes — the line of
// an error, which a Go panic's goroutines follow —, or none.
func firstLine(s, none string) string {
	for l := range strings.Lines(s) {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 400 {
				l = strings.ToValidUTF8(l[:400], "") + "…"
			}
			return l
		}
	}
	return none
}

// firstLines is the first few lines of what a program said, or none.
func firstLines(s, none string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return none
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 5 {
		lines = lines[:5]
	}
	return strings.Join(lines, " | ")
}
