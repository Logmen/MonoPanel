package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Every program the API process has the agent run as root goes through a
// policy in the toolpolicy*.go files. A tool runs with root's full power, and
// most of them can be told to run a program, load a library or write a file
// of the caller's choosing (nginx -c, tar --to-command, restic -o
// sftp.command, mysql's \!, crontab -u root …): whoever controls the API —
// an RCE in it, or anyone who can talk to the agent socket as the service
// user — would be root. So each policy accepts exactly the argv shapes the
// panel builds, named at the policy with their call sites, and refuses the
// rest: an allow-list of shapes, never a deny-list of options.

// toolMode is how the tool's input and output travel.
type toolMode int

const (
	// modeTool is /v1/tool: stdin from the request, output captured.
	modeTool toolMode = iota
	// modeStreamIn is /v1/stream/in: stdin is the request body.
	modeStreamIn
	// modeStreamOut is /v1/stream/out: stdout is the response body.
	modeStreamOut
)

func (m toolMode) String() string {
	switch m {
	case modeStreamIn:
		return "/v1/stream/in"
	case modeStreamOut:
		return "/v1/stream/out"
	}
	return "/v1/tool"
}

// toolBinaries maps a tool to the binaries it may resolve to (first found
// wins). Only the tools the API calls are here; the tools the agent runs for
// itself (chpasswd, useradd …) are not reachable through /v1/tool.
var toolBinaries = map[string][]string{
	"mysql":           {"/usr/bin/mysql"},
	"mysqldump":       {"/usr/bin/mysqldump"},
	"percona-release": {"/usr/bin/percona-release"},
	"restic":          {"/usr/local/bin/restic", "/usr/bin/restic"},
	"fail2ban-client": {"/usr/bin/fail2ban-client"},
	"nft":             {"/usr/sbin/nft"},
	"crontab":         {"/usr/bin/crontab"},
	"tar":             {"/usr/bin/tar", "/bin/tar"},
	"du":              {"/usr/bin/du", "/bin/du"},
	"phpenmod":        {"/usr/sbin/phpenmod"},
	"phpdismod":       {"/usr/sbin/phpdismod"},
	"sshd":            {"/usr/sbin/sshd"},
	"journalctl":      {"/usr/bin/journalctl"},
	"dnf":             {"/usr/bin/dnf"},
	"semanage":        {"/usr/sbin/semanage"},
	"setsebool":       {"/usr/sbin/setsebool"},
	"semodule":        {"/usr/sbin/semodule"},
	"restorecon":      {"/usr/sbin/restorecon"},
	"ausearch":        {"/usr/sbin/ausearch"},
	"setenforce":      {"/usr/sbin/setenforce"},
	"firewall-cmd":    {"/usr/bin/firewall-cmd"},
	"postmap":         {"/usr/sbin/postmap"},
	"postconf":        {"/usr/sbin/postconf"},
	"newaliases":      {"/usr/bin/newaliases", "/usr/sbin/newaliases"},
}

func resolveTool(name string) (string, error) {
	cands, ok := toolBinaries[name]
	if !ok {
		return "", errors.New("tool not allowed: " + name)
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("tool not installed: " + name)
}

// toolPlan is what a policy approved: the exact command the agent runs, or
// the work it does itself instead.
type toolPlan struct {
	args []string
	env  []string
	// files are inherited by the program as fd 3, 4, …: directories the
	// agent opened without following a symlink, which args name as
	// /proc/self/fd/N, so that nothing can swap them between the check and
	// the use.
	files []*os.File
	// stdout, when set, receives the program's output (the dump file of a
	// backup) instead of the captured output.
	stdout *os.File
	// native, when set, runs instead of a program: tar -x is done by the
	// agent itself (see toolpolicy_untar.go).
	native func(ctx context.Context, stdin io.Reader, out io.Writer) int
	// wrap, when set, runs the program through it (restic restore lists the
	// snapshot first, and in place stages in a directory of the agent's and
	// moves the result into place).
	wrap func(ctx context.Context, run toolRun, out io.Writer) (int, error)
	// prepare runs after the plan is approved and before the program: it
	// makes the agent's own directories (restic's cache, RestoreDir).
	prepare func() error
	// confine, when set, turns each run of the program into one in a
	// Landlock sandbox (restic: restic_sandbox.go), with what to undo once
	// that run is over; out takes what the run's helpers have to say.
	confine func(ctx context.Context, bin string, args []string, out io.Writer) (*confinedRun, error)
	closers []io.Closer
}

// confinedRun is a run of a plan's program through sandbox-exec.
type confinedRun struct {
	// path and args are the command: the agent's binary, sandbox-exec with
	// its rules, "--" and the program with its arguments.
	path string
	args []string
	// env is added to the plan's environment.
	env []string
	// files are the command's descriptors 3, 4, … (the rules' trees), in
	// place of the plan's files.
	files []*os.File
	// done runs after the program exited.
	done func()
}

// toolRun runs the plan's program with args and returns its exit status;
// stdout, when not nil, takes the program's standard output instead of where
// the plan sends it.
type toolRun func(args []string, stdout io.Writer) (int, error)

func (p *toolPlan) close() {
	for _, f := range p.files {
		f.Close()
	}
	if p.stdout != nil {
		p.stdout.Close()
	}
	for _, c := range p.closers {
		c.Close()
	}
}

// toolCall is a request as the policies see it.
type toolCall struct {
	name       string
	args       []string
	env        []string
	mode       toolMode
	outputFile string
}

type toolPolicy func(s *Server, c *toolCall) (*toolPlan, error)

var toolPolicies map[string]toolPolicy

func init() {
	toolPolicies = map[string]toolPolicy{
		"mysql":           (*Server).planMySQL,
		"mysqldump":       (*Server).planMySQLDump,
		"restic":          (*Server).planRestic,
		"tar":             (*Server).planTar,
		"crontab":         (*Server).planCrontab,
		"du":              (*Server).planDu,
		"phpenmod":        (*Server).planPHPMod,
		"phpdismod":       (*Server).planPHPMod,
		"sshd":            (*Server).planSSHD,
		"journalctl":      (*Server).planJournalctl,
		"dnf":             (*Server).planDnf,
		"percona-release": (*Server).planPerconaRelease,
		"fail2ban-client": (*Server).planFail2banClient,
		"nft":             (*Server).planNft,
		"firewall-cmd":    (*Server).planFirewallCmd,
		"semanage":        (*Server).planSemanage,
		"setsebool":       (*Server).planSetsebool,
		"semodule":        (*Server).planSemodule,
		"restorecon":      (*Server).planRestorecon,
		"ausearch":        (*Server).planAusearch,
		"setenforce":      (*Server).planSetenforce,
		"postmap":         (*Server).planPostmap,
		"postconf":        (*Server).planPostconf,
		"newaliases":      (*Server).planNewaliases,
	}
}

// planTool vets a request against the tool's policy.
func (s *Server) planTool(c *toolCall) (*toolPlan, error) {
	pol, ok := toolPolicies[c.name]
	if !ok {
		return nil, &Error{Status: http.StatusBadRequest, Message: "tool not allowed: " + c.name}
	}
	for _, a := range c.args {
		if strings.ContainsRune(a, 0) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid argument"}
		}
	}
	if c.outputFile != "" && c.name != "mysqldump" {
		return nil, notAllowed("an output file for "+c.name, "only mysqldump writes one (the dumps of a backup)")
	}
	plan, err := pol(s, c)
	if err != nil {
		return nil, err
	}
	if len(c.env) > 0 && plan.env == nil {
		plan.close()
		return nil, notAllowed("environment for "+c.name, "the panel passes none")
	}
	return plan, nil
}

// notAllowed is the refusal of a shape the panel never sends.
func notAllowed(what, why string) *Error {
	return &Error{Status: http.StatusForbidden, Message: what + " is not allowed: " + why}
}

// refuseArgs is the generic refusal of a whole argv.
func refuseArgs(name string, args []string, why string) *Error {
	return notAllowed(fmt.Sprintf("%s %s", name, strings.Join(quoteArgs(args), " ")), why)
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\") {
			out[i] = strconv.Quote(a)
		} else {
			out[i] = a
		}
	}
	return out
}

// onlyTool refuses the streaming endpoints for a tool that only runs through
// /v1/tool.
func onlyTool(c *toolCall) error {
	if c.mode != modeTool {
		return notAllowed(c.name+" through "+c.mode.String(), "the panel runs it through /v1/tool only")
	}
	return nil
}

// exactShapes accepts argv equal to one of the shapes.
func exactShapes(c *toolCall, shapes ...[]string) (*toolPlan, error) {
	if err := onlyTool(c); err != nil {
		return nil, err
	}
	for _, sh := range shapes {
		if equalArgs(c.args, sh) {
			return &toolPlan{args: c.args}, nil
		}
	}
	return nil, refuseArgs(c.name, c.args, "not a form the panel uses")
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// cleanAbs reports whether p is an absolute path in its clean form: a path
// the policies compare by prefix must not hide a "..".
func cleanAbs(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p
}

// below reports whether p lies strictly below dir.
func below(p, dir string) bool {
	dir = filepath.Clean(dir)
	if dir == "/" {
		return p != "/" && strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, dir+"/")
}

// atOrBelow reports whether p is dir or lies below it.
func atOrBelow(p, dir string) bool {
	return p == filepath.Clean(dir) || below(p, dir)
}

var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// errSymlinkComponent marks a path the agent refused to walk: a symlink, or
// something else than a directory, where a directory was expected.
var errSymlinkComponent = errors.New("a symlink or a non-directory on the way")

// openDirNoFollow opens the absolute directory p one component at a time
// from /: a directory opened this way is the one that was checked, whatever
// is renamed or replaced afterwards. A symlink on the way is followed only
// where nobody but root can have put it — a symlink of root's in a directory
// of root's that no one else may write: /var/mail -> spool/mail on EL, a
// /var/www an administrator moved elsewhere. One in a directory an account
// or the service user owns (a home, the data directory) is refused.
//
// That rule holds for the system's own directories only. Below the panel's
// trees the agent itself makes files on the API's word — an archive's
// directories and symlinks (untar), a snapshot's (an in-place restore) — and
// what the agent made is root's like what an administrator made: a root-owned
// symlink to /etc in a root-owned directory of a home would pass it. Every
// walk into those trees goes through openDirBelow.
func openDirNoFollow(p string) (int, error) {
	return openDirTrusted(p)
}

// openDirBelow opens the directory p, which is root or lies below it: root
// itself is reached as openDirNoFollow reaches a directory, and from root on
// no symlink is followed at all, whoever owns it. root is one of the panel's
// trees: the web root, the mail store, the data or configuration directory,
// Sphinx's install directory. With create, missing directories below root are
// made with mode (root-owned).
func openDirBelow(root, p string, create bool, mode uint32) (int, error) {
	return openDirBelowOwned(root, p, create, mode, -1, -1)
}

// openDirBelowOwned is openDirBelow that gives the directories it makes the
// owner uid:gid (-1 keeps root's).
func openDirBelowOwned(root, p string, create bool, mode uint32, uid, gid int) (int, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || !cleanAbs(p) || !atOrBelow(p, root) {
		return -1, fmt.Errorf("%s is not below %s", p, root)
	}
	rfd, err := openDirTrusted(root)
	if err != nil {
		return -1, err
	}
	defer unix.Close(rfd) //nolint:errcheck // a directory opened for reading
	rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
	return openDirAtOwned(rfd, root, rel, create, mode, uid, gid)
}

// openDirTrusted is the walk of openDirNoFollow.
func openDirTrusted(p string) (int, error) {
	if !filepath.IsAbs(p) {
		return -1, fmt.Errorf("%s is not an absolute path", p)
	}
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: "/", Err: err}
	}
	// stack holds the directories walked so far, for ".." in a symlink
	stack := []int{root}
	names := []string{"/"}
	defer func() {
		for _, fd := range stack {
			unix.Close(fd) //nolint:errcheck // a directory opened for reading
		}
	}()
	comps := splitPath(p)
	for hops := 0; len(comps) > 0; {
		name := comps[0]
		comps = comps[1:]
		cur := stack[len(stack)-1]
		where := filepath.Join(names[len(names)-1], name)
		switch name {
		case ".":
			continue
		case "..":
			if len(stack) > 1 {
				unix.Close(cur) //nolint:errcheck // a directory opened for reading
				stack, names = stack[:len(stack)-1], names[:len(names)-1]
			}
			continue
		}
		next, err := unix.Openat(cur, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			target, terr := trustedSymlink(cur, name)
			if terr != nil {
				return -1, fmt.Errorf("%w: refusing %s (%v)", errSymlinkComponent, where, terr)
			}
			if hops++; hops > 40 {
				return -1, &os.PathError{Op: "open", Path: p, Err: unix.ELOOP}
			}
			if filepath.IsAbs(target) {
				for _, fd := range stack[1:] {
					unix.Close(fd) //nolint:errcheck // a directory opened for reading
				}
				stack, names = stack[:1], names[:1]
			}
			comps = append(splitPath(target), comps...)
			continue
		}
		if err != nil {
			return -1, &os.PathError{Op: "open", Path: where, Err: err}
		}
		stack, names = append(stack, next), append(names, where)
	}
	return unix.Dup(stack[len(stack)-1])
}

// symlinkTrustUID is root's uid for trustedSymlink. Tests, which cannot make
// files of root's, put their own uid here to stand for root.
var symlinkTrustUID uint32

// trustedSymlink returns the target of name in dir when it is a symlink only
// root can have made: owned by root, in a directory owned by root that its
// group and others may not write.
func trustedSymlink(dir int, name string) (string, error) {
	var st, dst unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return "", errors.New("not a directory")
	}
	if err := unix.Fstat(dir, &dst); err != nil {
		return "", err
	}
	if st.Uid != symlinkTrustUID || dst.Uid != symlinkTrustUID || dst.Mode&0o022 != 0 {
		return "", errors.New("a symlink someone other than root could have made")
	}
	return readlinkat(dir, name)
}

func splitPath(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// openDirAt opens rel (relative, slash-separated) below the directory dirfd,
// whose path base names it in errors, without following a symlink. With
// create, missing directories are made with mode (root-owned, as the agent
// is root).
func openDirAt(dirfd int, base, rel string, create bool, mode uint32) (int, error) {
	return openDirAtOwned(dirfd, base, rel, create, mode, -1, -1)
}

// openDirAtOwned is openDirAt that gives the directories it makes the owner
// uid:gid (-1 keeps root's).
func openDirAtOwned(dirfd int, base, rel string, create bool, mode uint32, uid, gid int) (int, error) {
	cur, err := unix.Dup(dirfd)
	if err != nil {
		return -1, err
	}
	if rel == "" || rel == "." {
		return cur, nil
	}
	p := base
	for _, name := range strings.Split(rel, "/") {
		if name == "" || name == "." || name == ".." {
			unix.Close(cur) //nolint:errcheck // a directory opened for reading
			return -1, fmt.Errorf("invalid path component %q in %s", name, rel)
		}
		p = filepath.Join(p, name)
		next, err := unix.Openat(cur, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		made := false
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(cur, name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(cur) //nolint:errcheck // a directory opened for reading
				return -1, &os.PathError{Op: "mkdir", Path: p, Err: err}
			}
			made = err == nil
			next, err = unix.Openat(cur, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(cur) //nolint:errcheck // a directory opened for reading
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, fmt.Errorf("%w: refusing %s", errSymlinkComponent, p)
		}
		if err != nil {
			return -1, &os.PathError{Op: "open", Path: p, Err: err}
		}
		if made && (uid >= 0 || gid >= 0) {
			if err := unix.Fchown(next, uid, gid); err != nil {
				unix.Close(next) //nolint:errcheck // a directory opened for reading
				return -1, &os.PathError{Op: "chown", Path: p, Err: err}
			}
		}
		cur = next
	}
	return cur, nil
}

// fdFile wraps a directory descriptor for exec.Cmd.ExtraFiles.
func fdFile(fd int, name string) *os.File {
	return os.NewFile(uintptr(fd), name)
}

// ensureRootDir makes the absolute directory p (and missing parents, with the
// same mode) owned by root, refusing a symlink on the way or a directory
// someone else owns or may write: the agent keeps its own state there
// (restic's cache, restored snapshots), out of the service user's reach. It
// returns the directory opened.
func ensureRootDir(p string, mode uint32) (int, error) {
	return ensureRootDirAs(p, mode, 0)
}

// ensureRootDirAs is ensureRootDir for a root of uid rootUID: 0, or the
// test's own uid standing for root (Server.rootUID).
func ensureRootDirAs(p string, mode uint32, rootUID int) (int, error) {
	if !cleanAbs(p) || p == "/" {
		return -1, fmt.Errorf("%s is not a directory the agent makes", p)
	}
	parent, err := openDirNoFollow(filepath.Dir(p))
	if errors.Is(err, os.ErrNotExist) {
		var pfd int
		if pfd, err = ensureRootDirAs(filepath.Dir(p), mode, rootUID); err == nil {
			unix.Close(pfd) //nolint:errcheck // a directory opened for reading
			parent, err = openDirNoFollow(filepath.Dir(p))
		}
	}
	if err != nil {
		return -1, err
	}
	defer unix.Close(parent) //nolint:errcheck // a directory opened for reading
	var st unix.Stat_t
	if err := unix.Fstat(parent, &st); err != nil {
		return -1, err
	}
	if int(st.Uid) != rootUID || (st.Mode&0o022 != 0 && st.Mode&unix.S_ISVTX == 0) {
		return -1, fmt.Errorf("%s must belong to root and be writable by root alone", filepath.Dir(p))
	}
	fd, err := openDirAt(parent, filepath.Dir(p), filepath.Base(p), true, mode)
	if err != nil {
		return -1, err
	}
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return -1, err
	}
	if int(st.Uid) != rootUID {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return -1, fmt.Errorf("%s belongs to uid %d, not root", p, st.Uid)
	}
	if st.Mode&0o7777 != mode {
		if err := unix.Fchmod(fd, mode); err != nil {
			unix.Close(fd) //nolint:errcheck // a directory opened for reading
			return -1, err
		}
	}
	return fd, nil
}
