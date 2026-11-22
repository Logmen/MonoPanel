package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// RestoreDir is where a snapshot restored aside lands, as
// <RestoreDir>/<snapshot>: root's alone. Under the API's data directory the
// service user could swap the directory for a symlink while restic writes,
// and restic would write the snapshot — whose content the API chooses, it
// holds the repository's password — wherever the link points.
const RestoreDir = "/var/lib/monopanel-agent/restore"

// resticCacheDir is restic's cache, root's alone for the same reason: the
// API names one in its own data directory, and the agent ignores that.
const resticCacheDir = "/var/cache/monopanel-agent/restic"

var (
	resticSnapshotRe = regexp.MustCompile(`^([0-9a-f]{8,64}|latest)$`)
	resticScopeRe    = regexp.MustCompile(`^scope=(server|user:[a-z_][a-z0-9_-]{0,31}|site:[A-Za-z0-9][A-Za-z0-9.-]{0,252}|db:[A-Za-z0-9_]{1,64})$`)
	resticKeepRe     = regexp.MustCompile(`^[0-9]{1,4}$`)
	envKeyRe         = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	sftpHostRe       = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*|\[[0-9A-Fa-f:.]+\])$`)
	sftpUserRe       = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)
)

// resticEnvPrefixes are the variables a repository's credentials come in.
var resticEnvPrefixes = []string{"RESTIC_", "AWS_", "B2_", "AZURE_", "GOOGLE_", "OS_", "SWIFT_"}

// planRestic: the repository's snapshots, its config (does it exist), init,
// a backup of what the panel backs up, retention, and a restore aside or in
// place (internal/api/ops_backup.go). -o/--option (sftp.command and
// rclone.program run a program), --password-command, --repository-file,
// --cache-dir, --files-from, --stdin-from-command and the rest are not
// among them.
func (s *Server) planRestic(c *toolCall) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	env, err := s.resticEnv(c.env)
	if err != nil {
		return nil, err
	}
	plan := &toolPlan{args: c.args, env: env, prepare: ensureResticCache}
	a := c.args
	switch {
	case equalArgs(a, []string{"snapshots", "--json", "--tag", "monopanel"}),
		equalArgs(a, []string{"cat", "config"}),
		equalArgs(a, []string{"init"}):
	case len(a) == 10 && a[0] == "forget" && a[1] == "--prune" && a[2] == "--tag" && a[3] == "monopanel" &&
		a[4] == "--keep-daily" && resticKeepRe.MatchString(a[5]) && a[6] == "--keep-weekly" && resticKeepRe.MatchString(a[7]) &&
		a[8] == "--keep-monthly" && resticKeepRe.MatchString(a[9]):
	case len(a) >= 7 && a[0] == "backup" && a[1] == "--json" && a[2] == "--tag" && a[3] == "monopanel" && a[4] == "--tag" && resticScopeRe.MatchString(a[5]):
		for _, p := range a[6:] {
			if err := s.resticBackupPath(p); err != nil {
				return nil, err
			}
		}
	case len(a) >= 4 && a[0] == "restore":
		return s.planResticRestore(plan)
	default:
		return nil, refuseArgs("restic", a, "not a form the panel uses")
	}
	return plan, nil
}

// resticBackupPath accepts what internal/api/ops_backup.go backupPaths
// backs up: the web root (or a home, or a site in it), the panel's
// configuration, its certificates and ACME account directories, and the
// dumps of the run. Anything else would hand the API a copy of /etc/shadow
// or /root.
//
// restic resolves the directories on the way to a path itself, and a symlink
// among them — one an account left in its home, or the API in its data
// directory — would have it back up the link's target instead: those
// directories are walked first without following one (restic keeps a path
// that is itself a symlink as the symlink). The walk checks, it does not pin:
// an account that swaps a directory of its own for a symlink between the
// walk and restic's open can still win that race.
func (s *Server) resticBackupPath(p string) error {
	refuse := func(why string) error { return notAllowed("a backup of "+p, why) }
	if !cleanAbs(p) {
		return refuse("a clean absolute path")
	}
	var root string
	dir, base := filepath.Dir(p), filepath.Base(p)
	switch {
	case atOrBelow(p, s.cfg.WWWRoot):
		root = s.cfg.WWWRoot
	case atOrBelow(p, s.cfg.ConfigDir):
		root = s.cfg.ConfigDir
	case p == filepath.Join(s.cfg.DataDir, "certs"), p == filepath.Join(s.cfg.DataDir, "acme"):
		root = s.cfg.DataDir
	case filepath.Dir(dir) == s.backupTmpDir() && backupRunRe.MatchString(filepath.Base(dir)) &&
		(base == "panel.db" || strings.HasSuffix(base, ".sql") && dbNameRe.MatchString(strings.TrimSuffix(base, ".sql"))):
		root = s.cfg.DataDir
	default:
		return refuse("the panel backs up the web root, its configuration, certificates and the run's dumps")
	}
	if !atOrBelow(dir, root) {
		return nil // the tree itself: nothing of anybody else's on the way
	}
	fd, err := openDirBelow(root, dir, false, 0)
	if err != nil {
		return refuse(err.Error())
	}
	unix.Close(fd) //nolint:errcheck // a directory opened for reading
	return nil
}

// resticEnv vets the repository's environment: the repository, its password
// and the credentials of its backend. A *_COMMAND variable runs a program,
// a *_FILE one has root read a file of the caller's choosing (restic prints
// a repository read from one), and the cache is the agent's own.
func (s *Server) resticEnv(env []string) ([]string, error) {
	out := make([]string, 0, len(env)+1)
	repo := ""
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || !envKeyRe.MatchString(k) || strings.ContainsAny(v, "\n\x00") {
			return nil, notAllowed("the environment entry "+strconv.Quote(k), "a NAME=value pair without newlines")
		}
		prefixed := false
		for _, pre := range resticEnvPrefixes {
			prefixed = prefixed || strings.HasPrefix(k, pre)
		}
		switch {
		case !prefixed:
			return nil, notAllowed("the environment variable "+k+" for restic", "only RESTIC_* and the credentials of the backends")
		case strings.HasSuffix(k, "_COMMAND"):
			return nil, notAllowed("the environment variable "+k, "it runs a program as root")
		case strings.HasSuffix(k, "_FILE"), strings.HasPrefix(k, "RESTIC_FROM_"), k == "AWS_PROFILE", k == "GOOGLE_APPLICATION_CREDENTIALS":
			return nil, notAllowed("the environment variable "+k, "it has root read a file or a profile of its own")
		case k == "RESTIC_CACHE_DIR":
			continue // the agent's own cache below
		case k == "RESTIC_REPOSITORY":
			repo = v
		}
		out = append(out, e)
	}
	if err := s.checkResticRepository(repo); err != nil {
		return nil, err
	}
	return append(out, "RESTIC_CACHE_DIR="+resticCacheDir), nil
}

// checkResticRepository accepts the backends the panel offers: a local
// directory, sftp, s3, b2 and rest (apitypes.BackupTargetRequest). rclone,
// whose program and flags the repository string reaches, is not one. An sftp
// host that starts with "-" would be an option to the ssh restic runs as
// root (-oProxyCommand=…).
func (s *Server) checkResticRepository(repo string) error {
	refuse := func(why string) error { return notAllowed("the repository "+strconv.Quote(repo), why) }
	if repo == "" {
		return refuse("RESTIC_REPOSITORY is required")
	}
	if strings.ContainsAny(repo, " \t\r\n\x00") {
		return refuse("no whitespace")
	}
	switch {
	case strings.HasPrefix(repo, "/"):
		if !cleanAbs(repo) || repo == "/" {
			return refuse("a clean absolute directory")
		}
		for _, d := range []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/boot", "/dev", "/proc", "/sys",
			"/run", "/root", "/var/spool", s.cfg.WWWRoot, s.cfg.ConfigDir, "/var/lib/monopanel-agent", "/var/cache/monopanel-agent"} {
			if atOrBelow(repo, d) {
				return refuse("restic writes into the repository as root, and " + d + " is the system's")
			}
		}
	case strings.HasPrefix(repo, "sftp:"):
		var user, host string
		if strings.HasPrefix(repo, "sftp://") {
			u, err := url.Parse(repo)
			if err != nil {
				return refuse(err.Error())
			}
			user, host = u.User.Username(), u.Hostname()
			if strings.Contains(u.Host, ":") && u.Port() == "" && !strings.HasPrefix(u.Host, "[") {
				return refuse("a port is a number")
			}
		} else {
			hostPart, _, ok := strings.Cut(strings.TrimPrefix(repo, "sftp:"), ":")
			if !ok {
				return refuse("sftp:[user@]host:/path")
			}
			host = hostPart
			if i := strings.LastIndex(hostPart, "@"); i >= 0 {
				user, host = hostPart[:i], hostPart[i+1:]
			}
		}
		if !sftpHostRe.MatchString(host) || (user != "" && !sftpUserRe.MatchString(user)) {
			return refuse("the host and user are names (ssh would take one starting with - for an option)")
		}
	case strings.HasPrefix(repo, "s3:"), strings.HasPrefix(repo, "b2:"), strings.HasPrefix(repo, "rest:"):
	default:
		return refuse("the panel's backends are a local directory, sftp, s3, b2 and rest")
	}
	return nil
}

// ensureResticCache makes restic's cache directory, root's and 0700.
func ensureResticCache() error {
	fd, err := ensureRootDir(resticCacheDir, 0o700)
	if err == nil {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
	}
	return err
}

// relabelTrees gives restored trees the labels the policy expects: what was
// staged in the agent's directory carries that directory's type unless
// restic put the snapshot's label back. Without SELinux nothing happens.
func (s *Server) relabelTrees(ctx context.Context, paths []string, out io.Writer) {
	if s.profile.MAC() != "selinux" {
		return
	}
	bin, err := resolveTool("restorecon")
	if err != nil {
		return
	}
	res, err := exec.CommandContext(ctx, bin, append([]string{"-RF", "-i"}, paths...)...).CombinedOutput()
	if err != nil {
		fmt.Fprintf(out, "in place: restorecon: %v %s\n", err, strings.TrimSpace(string(res)))
	}
}

// planResticRestore: restic restore <snapshot> --target <dir> [--include
// <path>]…, aside into RestoreDir/<snapshot> or in place (target /).
func (s *Server) planResticRestore(plan *toolPlan) (*toolPlan, error) {
	a := plan.args
	if len(a) < 4 || a[2] != "--target" {
		return nil, refuseArgs("restic", a, "restic restore <snapshot> --target <dir> [--include <path>]…")
	}
	snap, target := a[1], a[3]
	if !resticSnapshotRe.MatchString(snap) {
		// <snapshot>:<subfolder> would restore any part of it anywhere
		return nil, notAllowed("the snapshot "+strconv.Quote(snap), "a snapshot id or latest")
	}
	var includes []string
	for i := 4; i < len(a); i += 2 {
		if a[i] != "--include" || i+1 >= len(a) || !filepath.IsAbs(a[i+1]) {
			return nil, refuseArgs("restic", a, "only --include <absolute path> may follow the target")
		}
		includes = append(includes, a[i+1])
	}
	if target != "/" {
		if target != filepath.Join(RestoreDir, snap) {
			return nil, notAllowed("restoring into "+target, "a snapshot is restored into "+filepath.Join(RestoreDir, "<snapshot>")+" or in place")
		}
		plan.prepare = func() error {
			if err := ensureResticCache(); err != nil {
				return err
			}
			fd, err := ensureRootDir(RestoreDir, 0o700)
			if err == nil {
				unix.Close(fd) //nolint:errcheck // a directory opened for reading
			}
			return err
		}
		plan.wrap = func(ctx context.Context, run toolRun, out io.Writer) (int, error) {
			return s.restoreAside(ctx, run, snap, target, includes, out)
		}
		return plan, nil
	}
	if len(includes) == 0 {
		return nil, notAllowed("an in-place restore without --include", "it would write the whole snapshot over the system")
	}
	for _, inc := range includes {
		if !cleanAbs(inc) || strings.ContainsAny(inc, "*?[\\") {
			return nil, notAllowed("restoring "+inc+" in place", "in place the paths are plain absolute paths")
		}
		if s.protected(inc) {
			return nil, notAllowed("restoring "+inc+" in place", "config.yaml and the secret key are root's")
		}
		if _, ok := s.inPlaceRoot(inc); !ok {
			return nil, notAllowed("restoring "+inc+" in place", "in place only the web root, the panel's configuration, certificates, ACME account and dumps; restore it aside into "+filepath.Join(RestoreDir, snap)+" and copy what is needed")
		}
	}
	plan.wrap = func(ctx context.Context, run toolRun, out io.Writer) (int, error) {
		return s.restoreInPlace(ctx, run, snap, includes, out)
	}
	return plan, nil
}

// inPlaceKind tells how what a snapshot brings is owned in a tree restored
// in place.
type inPlaceKind int

const (
	// the accounts' homes: owners and modes as the snapshot has them
	inPlaceHomes inPlaceKind = iota
	// the panel's configuration: root's, no symlinks, and every file passes
	// the content checks of config/apply (confcheck) — daemons that run as
	// root read files of this directory, and a restore must not be the way
	// around those checks
	inPlaceConfig
	// the panel's certificates, ACME account and the dumps of a backup run:
	// the service user's, as the API keeps them
	inPlaceData
)

// inPlaceRoot is a tree an in-place restore writes into: dir is where walks
// into it start (openDirBelow), the web root, the configuration directory or
// the data directory.
type inPlaceRoot struct {
	dir  string
	kind inPlaceKind
}

// inPlaceRoot names the tree p is restored into in place: the web root, the
// panel's configuration, and in the data directory the certificates, the
// ACME account and the dumps (internal/api/ops_backup.go backupPaths).
func (s *Server) inPlaceRoot(p string) (inPlaceRoot, bool) {
	switch {
	case !cleanAbs(p):
	case atOrBelow(p, s.cfg.WWWRoot):
		return inPlaceRoot{dir: filepath.Clean(s.cfg.WWWRoot), kind: inPlaceHomes}, true
	case atOrBelow(p, s.cfg.ConfigDir):
		return inPlaceRoot{dir: filepath.Clean(s.cfg.ConfigDir), kind: inPlaceConfig}, true
	case atOrBelow(p, filepath.Join(s.cfg.DataDir, "certs")), atOrBelow(p, filepath.Join(s.cfg.DataDir, "acme")), atOrBelow(p, s.backupTmpDir()):
		return inPlaceRoot{dir: filepath.Clean(s.cfg.DataDir), kind: inPlaceData}, true
	}
	return inPlaceRoot{}, false
}

// restoreAside restores the snapshot into target, RestoreDir/<snapshot>,
// after vetSnapshot. The target is made afresh: what an earlier restore left
// there — under "latest" or a short id that snapshot may have been another
// one — would hold symlinks of the API's choosing where this snapshot has
// directories, and restic before 0.17 makes a snapshot's directories with
// MkdirAll, which follows them.
func (s *Server) restoreAside(ctx context.Context, run toolRun, snap, target string, includes []string, out io.Writer) (int, error) {
	id, code, err := s.vetSnapshot(run, snap)
	if err != nil || code != 0 {
		return code, err
	}
	if _, err := os.Lstat(target); err == nil {
		// RestoreDir is root's and 0700: nobody swaps anything in it
		// meanwhile, and os.RemoveAll follows no symlink.
		fmt.Fprintf(out, "replacing the earlier restore in %s\n", target)
		if err := os.RemoveAll(target); err != nil {
			return 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	args := []string{"restore", id, "--target", target}
	for _, inc := range includes {
		args = append(args, "--include", inc)
	}
	code, err = run(args, nil)
	if err != nil {
		return code, err
	}
	// What is taken from there "by hand" must not be a set-user-ID root
	// binary or a device node of the API's making: the same as in place.
	m := &restoreMerge{s: s, ctx: ctx, out: out, aside: true}
	if err := m.sanitize(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return code, err
	}
	if m.skipped > 0 {
		fmt.Fprintf(out, "aside: %d left out or made safe (%s)\n", m.skipped, m.skippedWhy())
	}
	return code, nil
}

// vetSnapshot lists the snapshot (restic ls --json) and returns its full id,
// which the restore then names: the id pins the snapshot and every tree in it
// (restic checks each blob against its hash), so what was vetted is what is
// restored, whatever the repository — the API's — holds by then.
//
// restic restores as root a snapshot the API composed (it holds the
// repository's password, and a local repository may be a directory of its
// own). A tree with two entries of one name — a directory and a symlink to
// /etc — has restic make the symlink and then write the directory's content,
// owners and modes through it: restic 0.14's restorer, for one, makes
// symlinks in a second pass, replacing an empty directory of that name, and
// restores the twin directory's empty files, hard links and modes by path
// in that same pass. In a tree without two entries of one name and without
// names such as ".." or "a/b", every path has one entry, symlinks are
// leaves, and nothing restic makes is ever walked through.
func (s *Server) vetSnapshot(run toolRun, snap string) (string, int, error) {
	pr, pw := io.Pipe()
	chk := &snapshotCheck{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		chk.read(pr)
		io.Copy(io.Discard, pr) //nolint:errcheck // what follows a refusal is not needed
	}()
	code, err := run([]string{"ls", "--json", snap}, pw)
	pw.Close() //nolint:errcheck // a pipe's writer
	<-done
	if err != nil || code != 0 {
		return "", code, err
	}
	if chk.err != nil {
		return "", 0, notAllowed("restoring the snapshot "+snap, chk.err.Error())
	}
	if chk.id == "" {
		return "", 0, fmt.Errorf("restic ls %s named no snapshot", snap)
	}
	return chk.id, 0, nil
}

var resticFullIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// snapshotCheck reads the JSON lines of restic ls --json: the snapshot, then
// its nodes depth first, each directory's entries right after it.
type snapshotCheck struct {
	id    string
	stack []snapshotDir
	err   error
}

type snapshotDir struct {
	path  string
	names map[string]struct{}
}

func (c *snapshotCheck) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if c.line(sc.Bytes()); c.err != nil {
			return
		}
	}
	if err := sc.Err(); err != nil {
		c.err = fmt.Errorf("restic ls: %v", err)
	}
}

func (c *snapshotCheck) line(b []byte) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		// a notice of restic's own (repository opened …): every node comes
		// as one JSON object per line, its name escaped inside
		return
	}
	var n struct {
		StructType  string `json:"struct_type"`
		MessageType string `json:"message_type"`
		ID          string `json:"id"`
		Name        string `json:"name"`
		Type        string `json:"type"`
		Path        string `json:"path"`
	}
	if err := json.Unmarshal(b, &n); err != nil {
		c.err = fmt.Errorf("restic ls printed something else than JSON: %v", err)
		return
	}
	kind := n.MessageType
	if kind == "" {
		kind = n.StructType
	}
	switch kind {
	case "snapshot":
		if c.id != "" || !resticFullIDRe.MatchString(n.ID) {
			c.err = fmt.Errorf("restic ls named snapshot %q after %q", n.ID, c.id)
			return
		}
		c.id = n.ID
		c.stack = []snapshotDir{{path: "/", names: map[string]struct{}{}}}
	case "node":
		c.node(n.Name, n.Type, n.Path)
	}
}

func (c *snapshotCheck) node(name, typ, p string) {
	if c.id == "" {
		c.err = errors.New("restic ls listed a node before the snapshot")
		return
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || !strings.HasPrefix(p, "/") || path.Base(p) != name {
		c.err = fmt.Errorf("it holds the entry %q (%s): not a name restic restores safely", name, p)
		return
	}
	parent := path.Dir(p)
	// The directories the walk has left are done with.
	for len(c.stack) > 0 && c.stack[len(c.stack)-1].path != parent {
		c.stack = c.stack[:len(c.stack)-1]
	}
	if len(c.stack) == 0 {
		c.err = fmt.Errorf("restic ls listed %s outside the directory it was in", p)
		return
	}
	dir := c.stack[len(c.stack)-1]
	if _, twin := dir.names[name]; twin {
		c.err = fmt.Errorf("it holds two entries named %s: restic would write the one through the other", p)
		return
	}
	dir.names[name] = struct{}{}
	if typ == "dir" {
		c.stack = append(c.stack, snapshotDir{path: p, names: map[string]struct{}{}})
	}
}

// restoreInPlace has restic restore into a directory of the agent's, which
// nobody else can enter, and then moves what came out into place itself,
// without following a symlink (see restoreMerge). restic restores a
// snapshot's owners, modes, device nodes and file capabilities, and the API
// chooses the snapshot: written straight over /, a setuid root binary or
// /dev/sda would land in a home, and a symlink an account left in its home
// would carry the write anywhere.
func (s *Server) restoreInPlace(ctx context.Context, run toolRun, snap string, includes []string, out io.Writer) (int, error) {
	id, code, err := s.vetSnapshot(run, snap)
	if err != nil || code != 0 {
		return code, err
	}
	rfd, err := ensureRootDir(RestoreDir, 0o700)
	if err != nil {
		return 0, err
	}
	defer unix.Close(rfd) //nolint:errcheck // a directory opened for reading
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return 0, err
	}
	name := ".in-place-" + hex.EncodeToString(b)
	if err := unix.Mkdirat(rfd, name, 0o700); err != nil {
		return 0, &os.PathError{Op: "mkdir", Path: filepath.Join(RestoreDir, name), Err: err}
	}
	staging := filepath.Join(RestoreDir, name)
	defer os.RemoveAll(staging) //nolint:errcheck // what was not moved goes with it
	return s.stageAndMerge(ctx, run, staging, id, includes, out)
}

// stageAndMerge has restic restore the vetted snapshot id into staging (a
// directory of root's alone), makes what came out safe and moves it into
// place.
func (s *Server) stageAndMerge(ctx context.Context, run toolRun, staging, id string, includes []string, out io.Writer) (int, error) {
	args := []string{"restore", id, "--target", staging}
	for _, inc := range includes {
		args = append(args, "--include", inc)
	}
	code, err := run(args, nil)
	if err != nil || code != 0 {
		return code, err
	}
	sfd, err := unix.Open(staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: staging, Err: err}
	}
	defer unix.Close(sfd) //nolint:errcheck // a directory opened for reading
	m := &restoreMerge{s: s, ctx: ctx, out: out}
	if err := m.sanitize(staging); err != nil {
		return 0, err
	}
	m.merge(sfd, staging, includes)
	fmt.Fprintf(out, "in place: %d moved into place, %d left out (%s), %d failed\n", m.moved, m.skipped, m.skippedWhy(), m.failed)
	s.relabelTrees(ctx, includes, out)
	if m.failed > 0 {
		return 1, nil
	}
	return 0, nil
}
