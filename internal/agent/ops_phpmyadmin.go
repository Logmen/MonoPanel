package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/phpmyadmin"
)

// The panel's phpMyAdmin (internal/phpmyadmin). Its tree comes from the
// pinned release through the tar extraction (planTarExtract, into
// phpmyadmin.Root), its directories through dirs/ensure (dirOwners); what
// neither can write — a file of the tree readable by the pool user's group,
// a ticket only the pool user reads — this call writes at fixed names, with
// fixed owners and modes. The pool user owns its state and run directories:
// it is a system account of its own (checkEnsureSystemUser), whose PHP the
// panel runs, and nothing root does there follows what it may put there.

// pmaRoot, pmaRunDir and pmaAccount are phpmyadmin.Root, phpmyadmin.RunDir
// and the pool user's ids — others in tests.
var (
	pmaRoot    = phpmyadmin.Root
	pmaRunDir  = phpmyadmin.RunDir
	pmaAccount = func() (uid, gid int, err error) {
		if uid, err = lookupUID(phpmyadmin.User); err != nil {
			return -1, -1, err
		}
		if gid, err = lookupGID(phpmyadmin.User); err != nil {
			return -1, -1, err
		}
		return uid, gid, nil
	}
)

// maxPMAFile bounds config.inc.php and the sign-in script.
const maxPMAFile = 256 << 10

// CheckPhpMyAdminRequest refuses what /v1/phpmyadmin never writes: another
// action, a release name that is not one, a ticket name that is not a
// digest, content too large or with a NUL byte. The fake agent checks the
// same.
func CheckPhpMyAdminRequest(req *PhpMyAdminRequest) error {
	bad := func(what string) error {
		return &Error{Status: http.StatusBadRequest, Message: "phpmyadmin: " + what}
	}
	switch req.Action {
	case "files":
		if !phpmyadmin.VersionRe.MatchString(req.Version) {
			return bad(fmt.Sprintf("the release %q is no version", req.Version))
		}
		for _, c := range []string{req.Config, req.Signon} {
			if c == "" || len(c) > maxPMAFile || strings.IndexByte(c, 0) >= 0 {
				return bad("the files are text of at most 256 KiB, both given")
			}
		}
	case "ticket":
		if !phpmyadmin.TicketNameRe.MatchString(req.Name) {
			return bad("a ticket is named by the sha256 of its token, in hex")
		}
		if req.Content == "" || len(req.Content) > phpmyadmin.MaxTicket || strings.IndexByte(req.Content, 0) >= 0 {
			return bad(fmt.Sprintf("a ticket is text of at most %d bytes", phpmyadmin.MaxTicket))
		}
	case "sweep":
	case "prune":
		if !phpmyadmin.VersionRe.MatchString(req.Version) {
			return bad(fmt.Sprintf("the release %q is no version", req.Version))
		}
	default:
		return bad("unknown action " + req.Action)
	}
	return nil
}

func (s *Server) phpMyAdmin(_ context.Context, req *PhpMyAdminRequest) (*PhpMyAdminResponse, error) {
	if err := CheckPhpMyAdminRequest(req); err != nil {
		return nil, err
	}
	switch req.Action {
	case "files":
		return s.pmaFiles(req)
	case "ticket":
		return s.pmaTicket(req)
	case "prune":
		return s.pmaPrune(req.Version)
	}
	return s.pmaSweep()
}

// pmaDir opens dir without following a symlink on the way, as root's walks
// below the write allow-list go.
func (s *Server) pmaDir(dir string) (*walked, error) {
	return walkDir("/", dir, walkOpts{trust: s.trust(), follow: s.managedDepth(dir, pmaRoot, pmaRunDir), uid: -1, gid: -1})
}

// pmaFiles writes config.inc.php and the sign-in script into a release's
// tree. The tree must be root's alone: the pool user reads it and never
// writes it.
func (s *Server) pmaFiles(req *PhpMyAdminRequest) (*PhpMyAdminResponse, error) {
	_, gid, err := pmaAccount()
	if err != nil {
		return nil, &Error{Status: http.StatusConflict, Message: "phpmyadmin: the account " + phpmyadmin.User + " is missing: " + err.Error()}
	}
	dir := filepath.Join(pmaRoot, req.Version)
	w, err := s.pmaDir(dir)
	if err != nil {
		return nil, walkErr(dir, err)
	}
	defer w.close()
	var st unix.Stat_t
	if err := unix.Fstat(w.fd, &st); err != nil {
		return nil, &Error{Message: "stat " + dir, Output: err.Error()}
	}
	if !rootsAlone(&st, s.rootUID) {
		return nil, &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("phpmyadmin: %s is not root's alone (owner uid %d, mode %#o)", dir, st.Uid, st.Mode&0o7777)}
	}
	resp := &PhpMyAdminResponse{}
	for _, f := range []struct {
		name    string
		content string
		mode    uint32
		gid     int
	}{
		{phpmyadmin.ConfigFile, req.Config, 0o640, gid},
		{phpmyadmin.SignonScript, req.Signon, 0o644, s.rootGID()},
	} {
		old, ost, exists, err := readFileAt(w.fd, f.name)
		if err != nil && !errors.Is(err, errNotPlainFile) {
			return nil, &Error{Message: "read " + filepath.Join(dir, f.name), Output: err.Error()}
		}
		if exists && err == nil && bytes.Equal(old, []byte(f.content)) && ost.Mode&0o7777 == f.mode && int(ost.Uid) == s.rootUID && int(ost.Gid) == f.gid {
			continue
		}
		if err := writeFileAt(w.fd, dir, f.name, []byte(f.content), f.mode, s.rootUID, f.gid); err != nil {
			return nil, &Error{Message: "write " + filepath.Join(dir, f.name), Output: err.Error()}
		}
		resp.Written = append(resp.Written, filepath.Join(dir, f.name))
	}
	return resp, nil
}

// rootGID is root's group: 0, or the agent's own group in tests.
func (s *Server) rootGID() int {
	if s.rootUID == 0 {
		return 0
	}
	return os.Getegid()
}

// pmaUserDir opens a directory of the pool user's below the run directory
// and checks that it is the pool user's alone: a ticket's password is for
// that account's eyes.
func (s *Server) pmaUserDir(dir string, uid int) (*walked, error) {
	w, err := s.pmaDir(dir)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(w.fd, &st); err != nil {
		w.close()
		return nil, err
	}
	if int(st.Uid) != uid || st.Mode&0o077 != 0 {
		w.close()
		return nil, &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("phpmyadmin: %s is not %s's alone (owner uid %d, mode %#o)", dir, phpmyadmin.User, st.Uid, st.Mode&0o7777)}
	}
	return w, nil
}

// pmaTicket writes a ticket, the pool user's 0400.
func (s *Server) pmaTicket(req *PhpMyAdminRequest) (*PhpMyAdminResponse, error) {
	uid, gid, err := pmaAccount()
	if err != nil {
		return nil, &Error{Status: http.StatusConflict, Message: "phpmyadmin: the account " + phpmyadmin.User + " is missing: " + err.Error()}
	}
	dir := filepath.Join(pmaRunDir, "tickets")
	w, err := s.pmaUserDir(dir, uid)
	if err != nil {
		return nil, walkErr(dir, err)
	}
	defer w.close()
	if err := writeFileAt(w.fd, dir, req.Name, []byte(req.Content), 0o400, uid, gid); err != nil {
		return nil, &Error{Message: "write a ticket", Output: err.Error()}
	}
	return &PhpMyAdminResponse{Written: []string{filepath.Join(dir, req.Name)}}, nil
}

// pmaEntries lists a directory opened as fd.
func pmaEntries(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	return f.Readdirnames(-1)
}

// pmaSweep takes away the tickets nobody signed in with (past twice their
// life) and whatever else stands in the ticket directory, and answers and
// removes the sign-out marks of temporary accounts. A directory that is
// not there (no ticket since the boot) is nothing to sweep.
func (s *Server) pmaSweep() (*PhpMyAdminResponse, error) {
	resp := &PhpMyAdminResponse{}
	uid, _, err := pmaAccount()
	if err != nil {
		return resp, nil //nolint:nilerr // no pool user: phpMyAdmin was never installed, nothing of it to sweep
	}
	now := time.Now()
	for _, sub := range []string{"tickets", "logout"} {
		dir := filepath.Join(pmaRunDir, sub)
		w, err := s.pmaUserDir(dir, uid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, walkErr(dir, err)
		}
		names, err := pmaEntries(w.fd)
		if err != nil {
			w.close()
			return nil, &Error{Message: "list " + dir, Output: err.Error()}
		}
		for _, name := range names {
			var st unix.Stat_t
			if err := unix.Fstatat(w.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				continue
			}
			regular := st.Mode&unix.S_IFMT == unix.S_IFREG
			keep := false
			switch sub {
			case "tickets":
				age := now.Sub(time.Unix(st.Mtim.Unix()))
				keep = regular && phpmyadmin.TicketNameRe.MatchString(name) && age < 2*phpmyadmin.TicketTTL
			case "logout":
				if regular && phpmyadmin.TempUserRe.MatchString(name) {
					resp.Logouts = append(resp.Logouts, name)
				}
			}
			if keep {
				continue
			}
			if _, err := removeEntryAt(w.fd, name, filepath.Join(dir, name), true); err == nil {
				resp.Removed = append(resp.Removed, filepath.Join(dir, name))
			}
		}
		w.close()
	}
	return resp, nil
}

// pmaPrune removes the trees of the releases other than keep: an upgrade
// leaves the one it replaced until the new one serves.
func (s *Server) pmaPrune(keep string) (*PhpMyAdminResponse, error) {
	resp := &PhpMyAdminResponse{}
	w, err := s.pmaDir(pmaRoot)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return resp, nil
	}
	if err != nil {
		return nil, walkErr(pmaRoot, err)
	}
	defer w.close()
	names, err := pmaEntries(w.fd)
	if err != nil {
		return nil, &Error{Message: "list " + pmaRoot, Output: err.Error()}
	}
	for _, name := range names {
		if name == keep || !phpmyadmin.VersionRe.MatchString(name) {
			continue
		}
		p := filepath.Join(pmaRoot, name)
		if _, err := removeEntryAt(w.fd, name, p, true); err != nil {
			return nil, &Error{Message: "remove " + p, Output: err.Error()}
		}
		resp.Removed = append(resp.Removed, p)
	}
	return resp, nil
}
