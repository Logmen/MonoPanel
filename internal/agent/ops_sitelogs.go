package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
// owners and modes through the opened files. <log_dir> itself belongs to the
// API's service user, who could put a directory of its own in the place of
// sites: the owner check is what stops that.
const (
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
	resp, err := ensureSiteLogTree(filepath.Clean(s.cfg.LogDir), req.Login, siteLogIDs{rootUID: 0, rootGID: 0, uid: u.uid, gid: u.gid}, req.Files)
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
	top, err := unix.Open(logDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return resp, &os.PathError{Op: "open", Path: logDir, Err: err}
	}
	defer unix.Close(top) //nolint:errcheck // a directory opened for reading
	// The accounts and the nginx workers pass through the panel's own log
	// directory (0750 up to 0.8.19) on their way to their logs.
	var st unix.Stat_t
	if err := unix.Fstat(top, &st); err != nil {
		return resp, &os.PathError{Op: "stat", Path: logDir, Err: err}
	}
	if st.Mode&0o001 == 0 {
		if err := unix.Fchmod(top, st.Mode&0o7777|0o001); err != nil {
			return resp, &os.PathError{Op: "chmod", Path: logDir, Err: err}
		}
		resp.Changed++
	}
	sites, err := siteLogDir(top, sitesPath, ids.rootUID, ids.rootGID, siteLogsMode, resp)
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
		return -1, fmt.Errorf("%w: %s (owner uid %d, mode %#o); give it to root and take the write bits of group and others away by hand", errNotRootsAlone, p, st.Uid, st.Mode&0o7777)
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
	top, err := unix.Open(logDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, &os.PathError{Op: "open", Path: logDir, Err: err}
	}
	defer unix.Close(top) //nolint:errcheck // a directory opened for reading
	sitesPath := filepath.Join(logDir, "sites")
	sites, err := unix.Openat(top, "sites", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return false, nil
	case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
		return false, fmt.Errorf("%w: refusing %s", errSymlinkInPath, sitesPath)
	case err != nil:
		return false, &os.PathError{Op: "open", Path: sitesPath, Err: err}
	}
	defer unix.Close(sites) //nolint:errcheck // a directory opened for reading
	var st unix.Stat_t
	if err := unix.Fstat(sites, &st); err != nil {
		return false, &os.PathError{Op: "stat", Path: sitesPath, Err: err}
	}
	if int(st.Uid) != rootUID || st.Mode&0o022 != 0 {
		return false, fmt.Errorf("%w: %s (owner uid %d, mode %#o)", errNotRootsAlone, sitesPath, st.Uid, st.Mode&0o7777)
	}
	return removeAt(sites, login, filepath.Join(sitesPath, login))
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
