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
// world-readable system files restic and ssh read. Landlock decides on the
// file a path resolves to, so a link that leads out of those trees leads
// nowhere, and restic reports the file as one it could not read. It grants a
// tree however it is reached, though, and a server backup reads the panel's
// configuration, certificates and dumps and every home: so a backup also
// reads the web root in a view of it that follows no symlink at all
// (sandbox.Rules.NoSymlinks), where the agent may mount.
//
// Every check of the policy stays as it is (resticBackupPath, resticEnv, the
// shapes): the sandbox is a second wall, not the first. Without Landlock —
// a kernel before 5.13, or booted with an lsm= list that leaves it out —
// restic runs as before; the agent says so when it starts and the doctor
// shows it.

// resticSystemReads are the system's files restic and the programs it runs
// read: the account databases (restic records owners by name, ssh needs
// root's entry), name resolution, the time zone, the CA certificates of the
// remote backends where Go's crypto/x509 looks for them, the configuration
// of OpenSSL and of EL's crypto policies (ssh's libcrypto reads it), and the
// dynamic loader's cache and preloads. All of them are world-readable; what
// is not — /etc/shadow, the host keys in /etc/ssh, /etc/ssl/private,
// /etc/pki/tls/private, root's home — stays out of reach, and so does
// whatever a backed-up home points at there.
var resticSystemReads = []string{
	"/etc/passwd", "/etc/group", "/etc/nsswitch.conf",
	"/etc/hosts", "/etc/host.conf", "/etc/resolv.conf", "/etc/gai.conf", "/etc/services", "/etc/protocols",
	"/etc/localtime", "/usr/share/zoneinfo",
	"/etc/ssl/certs", "/etc/ssl/openssl.cnf", "/etc/pki/tls/certs", "/etc/pki/tls/openssl.cnf", "/etc/pki/ca-trust",
	"/etc/ca-certificates", "/usr/share/ca-certificates", "/usr/local/share/ca-certificates",
	"/etc/crypto-policies", "/usr/share/crypto-policies",
	"/etc/ld.so.cache", "/etc/ld.so.preload",
	"/dev/urandom",
}

// resticSystemExecs are the dynamic loader and the libraries: some
// distributions link restic against libc, and ssh always is.
var resticSystemExecs = []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64"}

// sshBinaries is the ssh an sftp repository is reached with.
var sshBinaries = []string{"/usr/bin/ssh"}

// logSandbox says once, when the agent starts, how restic runs.
func (s *Server) logSandbox() {
	if s.landlock > 0 && s.noSymfollowErr != nil {
		s.log.Warn("restic runs in a Landlock sandbox, but a backup follows the symlinks in the web root: during a server backup, which reads the panel's configuration, certificates and dumps as well, an account that swaps a directory of its own for a symlink can have those, or another account's files, stored as its own", "abi", s.landlock, "err", s.noSymfollowErr)
		return
	}
	if s.landlock > 0 {
		s.log.Info("restic runs in a Landlock sandbox", "abi", s.landlock, "nosymfollow", s.noSymfollow)
		return
	}
	s.log.Warn("Landlock is unavailable: restic backs up and restores as root without its sandbox, and an account that swaps a directory of its own for a symlink while a backup reads its home can have root store files outside it (/etc/shadow, root's keys) as the account's own; boot a kernel with landlock in its lsm= list", "err", s.landlockErr)
}

// probeNoSymfollow finds out once, as the agent starts, whether a backup's
// restic can be given its view of the web root without symlinks
// (sandbox.Rules.NoSymlinks, resticRules): that takes a mount namespace and
// the new mount API (Linux 5.14), which a container's profile may refuse.
// Without it restic still runs in the Landlock sandbox, and the agent and the
// doctor say what that leaves open.
func (s *Server) probeNoSymfollow(ctx context.Context) {
	s.noSymfollow, s.noSymfollowErr = false, nil
	if s.landlock < 1 {
		return
	}
	if os.Geteuid() != 0 {
		s.noSymfollowErr = errors.New("the agent does not run as root")
		return
	}
	dir, err := filepath.EvalSymlinks(s.cfg.WWWRoot)
	if err != nil || !filepath.IsAbs(dir) {
		dir = os.TempDir() // no web root yet: any directory tells as much
	}
	rules := sandbox.Rules{NoSymlinks: []string{dir}}
	argv, _ := rules.CheckCommand()
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(pctx, s.sandboxExe, argv...).CombinedOutput(); err != nil {
		s.noSymfollowErr = fmt.Errorf("%v: %s", err, firstLines(string(out), "nothing said"))
		return
	}
	s.noSymfollow = true
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
func (s *Server) sandboxRestic(plan *toolPlan, sources []*os.File) {
	for _, f := range sources {
		plan.closers = append(plan.closers, f)
	}
	if s.landlock < 1 || plan.args[0] == "restore" && s.landlock < 2 {
		return
	}
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
	// sources: a backup reads the backed-up paths (sandboxRestic's).
	sources bool
	// target is a restore's target below RestoreDir, made when missing.
	target string
	// repo is a local repository, written to.
	repo string
	// sftp is the server of an sftp repository: ssh reaches it through a
	// connection the agent opens outside the sandbox (sshMaster).
	sftp *sftpRemote
	// noSymlinks is the web root, resolved, for a backup that reads in it:
	// restic follows no symlink below it (sandbox.Rules.NoSymlinks). The
	// Landlock rules alone would let a home's symlink lead into every other
	// tree the run reads — a server backup reads the panel's
	// configuration, certificates and dumps and all the homes — and restic
	// never follows one there itself: a symlink it backs up it stores as
	// one, and the policy accepts no path with a symlink on the way below
	// the web root (resticBackupPath).
	noSymlinks []string
}

// resticRules is the one place that says what restic may reach for a run
// with args (as the policy and its wrappers built them) against the
// repository repo, bin being the restic binary.
func (s *Server) resticRules(bin string, args []string, repo string) (*resticAccess, error) {
	if len(args) == 0 {
		return nil, errors.New("restic: no command")
	}
	acc := &resticAccess{
		ro: systemPaths(resticSystemReads),
		rw: systemPaths([]string{s.resticCacheDir(), s.resticTmpDir(), "/dev/null"}),
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
		if s.noSymfollow && len(args) > 6 && slices.ContainsFunc(args[6:], func(p string) bool { return atOrBelow(p, s.cfg.WWWRoot) }) {
			www, err := filepath.EvalSymlinks(s.cfg.WWWRoot)
			if err != nil {
				return nil, fmt.Errorf("the web root: %w", err)
			}
			acc.noSymlinks = []string{www}
		}
	case "restore":
		i := slices.Index(args, "--target")
		if i < 0 || i+1 >= len(args) || !cleanAbs(args[i+1]) || !below(args[i+1], RestoreDir) {
			return nil, notAllowed("a restore of "+strings.Join(args, " "), "restic restores into "+RestoreDir+" alone")
		}
		acc.target = args[i+1]
	case "ls", "snapshots", "cat", "init", "forget":
	default:
		return nil, notAllowed("restic "+args[0]+" in the sandbox", "not a command the panel runs")
	}
	return acc, nil
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
		if p == "/etc/resolv.conf" && strings.HasPrefix(r, "/run/") {
			r = filepath.Dir(r)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// confineRestic turns a run of restic into one through sandbox-exec under
// resticRules.
func (s *Server) confineRestic(repo string, sources []*os.File) func(ctx context.Context, bin string, args []string, out io.Writer) (*confinedRun, error) {
	return func(ctx context.Context, bin string, args []string, out io.Writer) (*confinedRun, error) {
		acc, err := s.resticRules(bin, args, repo)
		if err != nil {
			return nil, err
		}
		rules := sandbox.Rules{RO: acc.ro, RW: acc.rw, RX: acc.rx, NoSymlinks: acc.noSymlinks}
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
			// RestoreDir is root's and 0700: what is made below it stays the
			// agent's own until restic has written
			fd, err := openDirBelow(RestoreDir, acc.target, true, 0o700)
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
