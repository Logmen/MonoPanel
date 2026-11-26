package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// The site logs live in <log_dir>/sites/<login>/ and not in the account's
// home: nginx, Apache and the php-fpm master open them as root (the nginx
// master also chowns its files to the worker user, by name, when it reopens
// them), and a log the account could swap for a symlink would turn that into
// a root write to /etc/ld.so.preload or /etc/cron.d. Only root makes, renames
// or removes anything in the tree:
//
//	<log_dir>/sites            root:root        0711  accounts pass through, cannot list
//	<log_dir>/sites/<login>    root:<login>     0751  the account reads through its group, nginx workers pass
//	the servers' logs          root:<login>     0640  nginx (its worker user after a reopen), Apache, the php-fpm slow log
//	<domain>.php.error.log     <login>:<login>  0660  PHP itself opens it, as the account, for every write
//
// The agent goes down the tree through opened directories without following
// a symlink, refuses a directory anyone but root may write to, and changes
// owners and modes through the opened files.
//
// <log_dir> itself is root:<service group> 1771: the API writes its own
// files there (the audit log), and the sticky bit keeps it from renaming or
// removing root's — sites above all, which it could otherwise move away and
// replace with a tree of its own for nginx to open as root. Up to 0.8.19 the
// directory was the service user's; the agent takes it over (at its start
// and before it works in the tree), the package and mp setup make it so.
const (
	logDirMode         = 0o1771
	siteLogsMode       = 0o711
	siteLogsLoginMode  = 0o751
	siteLogServerMode  = 0o640
	siteLogAccountMode = 0o660
)

// siteLogNameRe is a file of an account's log directory: a domain (or
// webmail) and a suffix, ending in .log.
var siteLogNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,250}\.log$`)

var (
	// errNotRootsAlone marks a directory of the log tree someone besides
	// root could have written to: what it holds is not to be trusted.
	errNotRootsAlone = errors.New("not root's alone")
	// errNotPlainLog marks a log name that is something else than a file.
	errNotPlainLog = errors.New("not a regular file")
)

// siteLogIDs are the owners in an account's log tree: root (the test's own
// user in tests) and the account.
type siteLogIDs struct {
	rootUID, rootGID int
	uid, gid         int
	// serviceGID is the service group, which shares the log directory with
	// root (-1: keep the group it has).
	serviceGID int
	// takeOver gives a log directory that is not root's to root: the agent
	// does, as root; an unprivileged run (the tests) cannot.
	takeOver bool
}

// logDirFix is what the log directory itself needs, given its stat: a
// directory someone else owns goes to root and the service group, 1771 (when
// ids.takeOver); root's keeps its mode, gains x for others (the accounts and
// the nginx workers pass through on their way to their logs) and, when
// group or others may write to it, the sticky bit, so that nobody but root
// renames root's entries.
func logDirFix(st *unix.Stat_t, ids siteLogIDs) (uid, gid int, mode uint32, change bool) {
	if int(st.Uid) != ids.rootUID && ids.takeOver {
		gid = ids.serviceGID
		if gid < 0 {
			gid = int(st.Gid)
		}
		return ids.rootUID, gid, logDirMode, true
	}
	mode = st.Mode&0o7777 | 0o001
	if mode&0o022 != 0 {
		mode |= unix.S_ISVTX
	}
	return -1, -1, mode, mode != st.Mode&0o7777
}

// accountWritesLog tells the logs the account's own PHP workers write: the
// PHP error log. Root processes write all the others.
func accountWritesLog(name string) bool { return strings.HasSuffix(name, ".php.error.log") }

func (s *Server) ensureSiteLogs(ctx context.Context, req *SiteLogsRequest) (*SiteLogsResponse, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	for _, n := range req.Files {
		if !siteLogNameRe.MatchString(n) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid log file name: " + n}
		}
	}
	u, err := lookupUser(req.Login)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "account " + req.Login + ": " + err.Error()}
	}
	// A panel account, never one of the system's own: their group would
	// read the logs.
	if u.uid < uidMin() {
		return nil, &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("refusing the system account %s (uid %d)", req.Login, u.uid)}
	}
	ids := siteLogIDs{rootUID: 0, rootGID: 0, uid: u.uid, gid: u.gid, serviceGID: s.serviceGID(), takeOver: os.Geteuid() == 0}
	resp, err := ensureSiteLogTree(filepath.Clean(s.cfg.LogDir), req.Login, ids, req.Files)
	// What was made gets the label the policy expects (httpd_log_t), not the
	// var_log_t it inherits under /var/log; even when a later step failed.
	s.restorecon(ctx, resp.Created...)
	if err != nil {
		if errors.Is(err, errSymlinkInPath) || errors.Is(err, errNotRootsAlone) || errors.Is(err, errNotPlainLog) {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
		return nil, &Error{Message: "site logs of " + req.Login, Output: err.Error()}
	}
	if len(resp.Created) > 0 || resp.Changed > 0 {
		s.log.Info("site logs", "dir", resp.Dir, "created", len(resp.Created), "changed", resp.Changed)
	}
	return resp, nil
}

// ensureSiteLogTree makes <logDir>/sites/<login> and the files in it, or
// sets owners and modes back on what is there. The response lists what was
// made even when it fails halfway.
func ensureSiteLogTree(logDir, login string, ids siteLogIDs, files []string) (*SiteLogsResponse, error) {
	sitesPath := filepath.Join(logDir, "sites")
	resp := &SiteLogsResponse{Dir: filepath.Join(sitesPath, login), Created: []string{}}
	sites, err := openSiteLogRoot(logDir, ids, resp)
	if err != nil {
		return resp, err
	}
	defer unix.Close(sites) //nolint:errcheck // a directory opened for reading
	dir, err := siteLogDir(sites, resp.Dir, ids.rootUID, ids.gid, siteLogsLoginMode, resp)
	if err != nil {
		return resp, err
	}
	defer unix.Close(dir) //nolint:errcheck // a directory opened for reading
	for _, name := range files {
		uid, mode := ids.rootUID, uint32(siteLogServerMode)
		if accountWritesLog(name) {
			uid, mode = ids.uid, siteLogAccountMode
		}
		if err := siteLogFile(dir, filepath.Join(resp.Dir, name), uid, ids.gid, mode, resp); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// openSiteLogRoot gives <logDir> root as its owner and the mode that keeps
// root's entries root's, then opens <logDir>/sites (making it when it is
// missing) and checks it is root's alone.
func openSiteLogRoot(logDir string, ids siteLogIDs, resp *SiteLogsResponse) (int, error) {
	top, err := openLogDir(logDir, ids, resp)
	if err != nil {
		return -1, err
	}
	defer unix.Close(top) //nolint:errcheck // a directory opened for reading
	return siteLogDir(top, filepath.Join(logDir, "sites"), ids.rootUID, ids.rootGID, siteLogsMode, resp)
}

// openLogDir opens <logDir> and gives it root as its owner and the mode
// that keeps root's entries root's (logDirFix).
func openLogDir(logDir string, ids siteLogIDs, resp *SiteLogsResponse) (int, error) {
	top, err := unix.Open(logDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: logDir, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(top, &st); err != nil {
		unix.Close(top) //nolint:errcheck // a directory opened for reading
		return -1, &os.PathError{Op: "stat", Path: logDir, Err: err}
	}
	// The service user's, as mp setup made it up to 0.8.19: root takes it
	// over before it trusts anything below it — sites/ is checked right
	// after, so what the old owner left in its place is refused (or, at the
	// agent's start, taken over and cleared: prepareSiteLogTree).
	if uid, gid, mode, change := logDirFix(&st, ids); change {
		if uid >= 0 || gid >= 0 {
			if err := unix.Fchown(top, uid, gid); err != nil {
				unix.Close(top) //nolint:errcheck // a directory opened for reading
				return -1, &os.PathError{Op: "chown", Path: logDir, Err: err}
			}
			// Root's now, the old owner can set no ACL on it any more; the
			// ones it set go: a default ACL would be inherited by sites/,
			// made below right after, and by every log made there, readable
			// then by whoever it names (postinstall.sh leaves such a
			// directory to the agent for this).
			if _, err := dropACLs(top, logDir); err != nil {
				unix.Close(top) //nolint:errcheck // a directory opened for reading
				return -1, err
			}
		}
		if err := unix.Fchmod(top, mode); err != nil {
			unix.Close(top) //nolint:errcheck // a directory opened for reading
			return -1, &os.PathError{Op: "chmod", Path: logDir, Err: err}
		}
		resp.Changed++
	}
	return top, nil
}

// makeLogDir makes the panel's log directory when it is missing,
// root:<service group> 1771 (root's group when gid is -1: there is no
// service group).
func makeLogDir(logDir, serviceGroup string, gid int) error {
	if _, err := os.Lstat(logDir); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	d := DirSpec{Path: logDir, Mode: logDirMode, Owner: "root"}
	if gid >= 0 {
		d.Group = serviceGroup
	}
	_, _, err := ensureDirAt(d, &dirTrust{rootUID: os.Geteuid(), strict: true}, followAll)
	return err
}

// PrepareLogDir makes the panel's log directory root:<service group> 1771 —
// taking it over from the service user, whose it was up to 0.8.19 — and
// sites/ in it root:root 0711, refusing a sites/ that is not root's alone
// (what the old owner may have left there). mp setup's; the agent does the
// same at its start and goes further: it takes such a sites/ over and
// clears the tree of what root does not put there (prepareSiteLogTree).
// The directory is made when it is missing.
func PrepareLogDir(logDir, serviceGroup string) error {
	logDir = filepath.Clean(logDir)
	gid, err := lookupGID(serviceGroup)
	if err != nil {
		gid = -1
	}
	if err := makeLogDir(logDir, serviceGroup, gid); err != nil {
		return err
	}
	sites, err := openSiteLogRoot(logDir, siteLogIDs{rootUID: os.Geteuid(), rootGID: os.Getegid(), serviceGID: gid, takeOver: os.Geteuid() == 0}, &SiteLogsResponse{})
	if err != nil {
		return err
	}
	return unix.Close(sites)
}

// serviceGID is the gid of the service group, -1 when it is missing.
func (s *Server) serviceGID() int {
	gid, err := lookupGID(s.cfg.ServiceGroup)
	if err != nil {
		return -1
	}
	return gid
}

// siteLogDir opens the directory p, an entry of the directory parent, never
// through a symlink, making it when it is missing. A directory someone besides
// root could write to is refused: it could hold a symlink, or a second name
// of /etc/shadow, for root to chown. The one it accepts gets gid and mode.
func siteLogDir(parent int, p string, rootUID, gid int, mode uint32, resp *SiteLogsResponse) (int, error) {
	name := filepath.Base(p)
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	made := false
	fd, err := unix.Openat(parent, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(parent, name, mode); err == nil {
			made = true
			resp.Created = append(resp.Created, p)
		} else if !errors.Is(err, unix.EEXIST) {
			return -1, &os.PathError{Op: "mkdir", Path: p, Err: err}
		}
		fd, err = unix.Openat(parent, name, flags, 0)
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return -1, fmt.Errorf("%w: refusing %s", errSymlinkInPath, p)
	}
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: p, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return -1, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if int(st.Uid) != rootUID || st.Mode&0o022 != 0 {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		// Handed to root by hand, it would keep what its owner left in it.
		return -1, fmt.Errorf("%w: %s (owner uid %d, mode %#o); restart monopanel-agent: at its start it takes the site log tree over and clears it of what root did not put there", errNotRootsAlone, p, st.Uid, st.Mode&0o7777)
	}
	changed := false
	if int(st.Gid) != gid {
		if err := unix.Fchown(fd, -1, gid); err != nil {
			unix.Close(fd) //nolint:errcheck // a directory opened for reading
			return -1, &os.PathError{Op: "chown", Path: p, Err: err}
		}
		changed = true
	}
	if st.Mode&0o7777 != mode {
		if err := unix.Fchmod(fd, mode); err != nil {
			unix.Close(fd) //nolint:errcheck // a directory opened for reading
			return -1, &os.PathError{Op: "chmod", Path: p, Err: err}
		}
		changed = true
	}
	if changed && !made {
		resp.Changed++
	}
	return fd, nil
}

// siteLogFile makes the log p in the directory dir when it is missing, or
// opens it without following a symlink and without blocking on a FIFO, and
// gives the regular file it finds the owner and the mode. A file nginx has
// handed to its worker user goes back to root: the master hands it over
// again before its workers reopen it.
func siteLogFile(dir int, p string, uid, gid int, mode uint32, resp *SiteLogsResponse) error {
	name := filepath.Base(p)
	const flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	made := false
	fd, err := unix.Openat(dir, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		fd, err = unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
		switch {
		case err == nil:
			made = true
			resp.Created = append(resp.Created, p)
		case errors.Is(err, unix.EEXIST): // a server opened it meanwhile
			fd, err = unix.Openat(dir, name, flags, 0)
		}
	}
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%w: refusing %s", errSymlinkInPath, p)
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: p, Err: err}
	}
	defer unix.Close(fd) //nolint:errcheck // nothing was written through it
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: refusing %s", errNotPlainLog, p)
	}
	changed := false
	if int(st.Uid) != uid || int(st.Gid) != gid {
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return &os.PathError{Op: "chown", Path: p, Err: err}
		}
		changed = true
	}
	if st.Mode&0o7777 != mode {
		if err := unix.Fchmod(fd, mode); err != nil {
			return &os.PathError{Op: "chmod", Path: p, Err: err}
		}
		changed = true
	}
	if changed && !made {
		resp.Changed++
	}
	return nil
}

func (s *Server) removeSiteLogs(_ context.Context, req *RemoveSiteLogsRequest) (*RemoveSiteLogsResponse, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	if req.Domain != "" {
		if !apacheLegacyDomainRe.MatchString(req.Domain) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid domain"}
		}
		files, err := removeSiteLogFiles(filepath.Clean(s.cfg.LogDir), req.Login, req.Domain, 0)
		if err != nil {
			if errors.Is(err, errSymlinkInPath) || errors.Is(err, errNotRootsAlone) {
				return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
			}
			return nil, &Error{Message: "site logs of " + req.Domain, Output: err.Error()}
		}
		return &RemoveSiteLogsResponse{Removed: len(files) > 0, Files: files}, nil
	}
	removed, err := removeSiteLogTree(filepath.Clean(s.cfg.LogDir), req.Login, 0)
	if err != nil {
		if errors.Is(err, errSymlinkInPath) || errors.Is(err, errNotRootsAlone) {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
		return nil, &Error{Message: "site logs of " + req.Login, Output: err.Error()}
	}
	return &RemoveSiteLogsResponse{Removed: removed}, nil
}

// removeSiteLogTree removes <logDir>/sites/<login> with everything in it,
// through <logDir>/sites opened without following a symlink and found to be
// root's alone.
func removeSiteLogTree(logDir, login string, rootUID int) (bool, error) {
	sites, sitesPath, err := openSiteLogsDir(logDir, rootUID)
	if err != nil || sites < 0 {
		return false, err
	}
	defer unix.Close(sites) //nolint:errcheck // a directory opened for reading
	return removeAt(sites, login, filepath.Join(sitesPath, login))
}

// siteLogFileRe is what follows "<domain>." in the name of a site's log:
// nginx's, Apache's, PHP's error log and php-fpm's slow log
// (internal/api/logrotate.go siteLogNames), rotated (.1, .2.gz) or not.
var siteLogFileRe = regexp.MustCompile(`^(access|error|apache\.access|apache\.error|php\.error|php\.slow)\.log(\.[0-9]{1,4})?(\.gz)?$`)

// removeSiteLogFiles removes the logs of one site from <logDir>/sites/<login>
// — the names alone, each unlinked in the directory opened without following
// a symlink: a link the account put there goes itself, never what it points
// to. The account's other sites' logs and its Apache's log stay.
func removeSiteLogFiles(logDir, login, domain string, rootUID int) ([]string, error) {
	sites, sitesPath, err := openSiteLogsDir(logDir, rootUID)
	if err != nil || sites < 0 {
		return nil, err
	}
	defer unix.Close(sites) //nolint:errcheck // a directory opened for reading
	p := filepath.Join(sitesPath, login)
	fd, err := unix.Openat(sites, login, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, nil
	case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
		return nil, fmt.Errorf("%w: refusing %s", errSymlinkInPath, p)
	case err != nil:
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var gone []string
	for _, n := range names {
		if rest, ok := strings.CutPrefix(n, domain+"."); !ok || !siteLogFileRe.MatchString(rest) {
			continue
		}
		switch err := unix.Unlinkat(int(f.Fd()), n, 0); {
		case err == nil:
			gone = append(gone, n)
		case errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EISDIR):
		default:
			return gone, &os.PathError{Op: "unlink", Path: filepath.Join(p, n), Err: err}
		}
	}
	return gone, nil
}

// openSiteLogsDir opens <logDir>/sites without following a symlink and
// checks that it is root's alone; -1 when there is none.
func openSiteLogsDir(logDir string, rootUID int) (int, string, error) {
	top, err := unix.Open(logDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return -1, "", nil
	}
	if err != nil {
		return -1, "", &os.PathError{Op: "open", Path: logDir, Err: err}
	}
	defer unix.Close(top) //nolint:errcheck // a directory opened for reading
	sitesPath := filepath.Join(logDir, "sites")
	sites, err := unix.Openat(top, "sites", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, sitesPath, nil
	case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
		return -1, sitesPath, fmt.Errorf("%w: refusing %s", errSymlinkInPath, sitesPath)
	case err != nil:
		return -1, sitesPath, &os.PathError{Op: "open", Path: sitesPath, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(sites, &st); err != nil {
		unix.Close(sites) //nolint:errcheck // a directory opened for reading
		return -1, sitesPath, &os.PathError{Op: "stat", Path: sitesPath, Err: err}
	}
	if int(st.Uid) != rootUID || st.Mode&0o022 != 0 {
		unix.Close(sites) //nolint:errcheck // a directory opened for reading
		return -1, sitesPath, fmt.Errorf("%w: %s (owner uid %d, mode %#o)", errNotRootsAlone, sitesPath, st.Uid, st.Mode&0o7777)
	}
	return sites, sitesPath, nil
}

// removeAt removes the entry name of the directory dir, a directory with
// everything below it; a symlink goes itself, never what it points to.
func removeAt(dir int, name, p string) (bool, error) {
	err := unix.Unlinkat(dir, name, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return false, nil
	case !errors.Is(err, unix.EISDIR):
		return false, &os.PathError{Op: "unlink", Path: p, Err: err}
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, &os.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	names, err := f.Readdirnames(-1)
	if err == nil {
		for _, n := range names {
			if _, err = removeAt(int(f.Fd()), n, filepath.Join(p, n)); err != nil {
				break
			}
		}
	}
	f.Close()
	if err != nil {
		return false, err
	}
	if err := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return false, &os.PathError{Op: "rmdir", Path: p, Err: err}
	}
	return true, nil
}
