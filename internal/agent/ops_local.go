package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// EnsureDir creates one directory with mode and ownership (idempotent).
func EnsureDir(d DirSpec) (created bool, err error) {
	var made []string
	p := filepath.Clean(d.Path)
	if !filepath.IsAbs(p) {
		return false, errors.New("path must be absolute")
	}
	mode := os.FileMode(d.Mode) & os.ModePerm
	if mode == 0 {
		mode = 0o755
	}
	st, err := os.Lstat(p)
	switch {
	case err == nil:
		if !st.IsDir() {
			return false, errors.New("exists and is not a directory: " + p)
		}
	case errors.Is(err, os.ErrNotExist):
		// The levels MkdirAll makes get the owner too: a root-owned parent
		// keeps the service user from managing what lives under it (the ACME
		// webroot's .well-known was made that way and broke certificates).
		made = missingLevels(p)
		if err := os.MkdirAll(p, mode); err != nil {
			return false, err
		}
		created = true
	default:
		return false, err
	}
	if err := os.Chmod(p, mode); err != nil {
		return created, err
	}
	uid, gid := -1, -1
	if d.Owner != "" {
		if uid, err = lookupUID(d.Owner); err != nil {
			return created, err
		}
	}
	if d.Group != "" {
		if gid, err = lookupGID(d.Group); err != nil {
			return created, err
		}
	}
	if uid >= 0 || gid >= 0 {
		for _, dir := range append(made, p) {
			if err := os.Chown(dir, uid, gid); err != nil {
				return created, err
			}
		}
	}
	return created, nil
}

// errSymlinkInPath marks a path below the web root that leads through a
// symlink or a file where a directory should be.
var errSymlinkInPath = errors.New("a symlink or a file in the path")

// dirIDs resolves the mode and the numeric owner and group of a DirSpec (-1:
// leave as is).
func dirIDs(d DirSpec) (mode os.FileMode, uid, gid int, err error) {
	mode = os.FileMode(d.Mode) & os.ModePerm
	if mode == 0 {
		mode = 0o755
	}
	uid, gid = -1, -1
	if d.Owner != "" {
		if uid, err = lookupUID(d.Owner); err != nil {
			return 0, -1, -1, err
		}
	}
	if d.Group != "" {
		if gid, err = lookupGID(d.Group); err != nil {
			return 0, -1, -1, err
		}
	}
	return mode, uid, gid, nil
}

// ensureHomeDir is EnsureDir for a directory below root (the web root, whose
// subdirectories the clients own). It walks every component below root
// through the directory opened before it, never following a symlink
// (O_NOFOLLOW), makes the missing levels there and changes mode and owner
// through the opened directory: a client who swaps a component for a symlink
// at any moment can only make the call fail, never redirect it.
func ensureHomeDir(root string, d DirSpec) (created bool, err error) {
	mode, uid, gid, err := dirIDs(d)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, d.Path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false, fmt.Errorf("%s is not below %s", d.Path, root)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() { unix.Close(fd) }() //nolint:errcheck // a directory opened for reading
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	parts := strings.Split(rel, "/")
	cur := root
	for i, name := range parts {
		cur = filepath.Join(cur, name)
		made := false
		next, err := unix.Openat(fd, name, flags, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(fd, name, uint32(mode)); err != nil && !errors.Is(err, unix.EEXIST) {
				return created, &os.PathError{Op: "mkdir", Path: cur, Err: err}
			}
			made = true
			next, err = unix.Openat(fd, name, flags, 0)
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return created, fmt.Errorf("%w: refusing %s", errSymlinkInPath, cur)
		}
		if err != nil {
			return created, &os.PathError{Op: "open", Path: cur, Err: err}
		}
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		fd = next
		last := i == len(parts)-1
		if made && last {
			created = true
		}
		// The levels made on the way get the owner too, as EnsureDir gives
		// them (a root-owned parent keeps the client out of its own tree).
		if made && !last && (uid >= 0 || gid >= 0) {
			if err := unix.Fchown(fd, uid, gid); err != nil {
				return created, &os.PathError{Op: "chown", Path: cur, Err: err}
			}
		}
	}
	if err := unix.Fchmod(fd, uint32(mode)); err != nil {
		return created, &os.PathError{Op: "chmod", Path: d.Path, Err: err}
	}
	if uid >= 0 || gid >= 0 {
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return created, &os.PathError{Op: "chown", Path: d.Path, Err: err}
		}
	}
	return created, nil
}

// missingLevels lists the ancestors of p that do not exist yet, topmost
// first; p itself is not included.
func missingLevels(p string) []string {
	var out []string
	for q := filepath.Dir(p); q != filepath.Dir(q); q = filepath.Dir(q) {
		if _, err := os.Lstat(q); err == nil {
			break
		}
		out = append([]string{q}, out...)
	}
	return out
}

func (s *Server) dirAllowed(p string) bool {
	// A directory made in place of config.yaml or the secret key, or a
	// change of mode or owner of the directory that holds them, is a write
	// to them as well.
	if s.coversProtected(p) {
		return false
	}
	if s.pathAllowed(strings.TrimSuffix(p, "/") + "/") {
		return true
	}
	for _, root := range []string{filepath.Clean(s.cfg.WWWRoot), filepath.Clean(s.cfg.RunDir), filepath.Clean(s.cfg.LogDir)} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func (s *Server) ensureDirs(ctx context.Context, req *EnsureDirsRequest) (*EnsureDirsResponse, error) {
	resp := &EnsureDirsResponse{Created: []string{}}
	root := filepath.Clean(s.cfg.WWWRoot)
	for _, d := range req.Dirs {
		p := filepath.Clean(d.Path)
		if !filepath.IsAbs(p) || !s.dirAllowed(p) {
			return nil, &Error{Status: http.StatusForbidden, Message: "directory not allowed: " + p}
		}
		d.Path = p
		var created bool
		var err error
		if strings.HasPrefix(p, root+"/") {
			// Clients own the directories below the web root: one of them
			// swapped for a symlink would turn this root mkdir, chmod and
			// chown into one of /etc/systemd/system or /root.
			created, err = ensureHomeDir(root, d)
			if errors.Is(err, errSymlinkInPath) {
				return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
			}
		} else {
			created, err = EnsureDir(d)
		}
		if err != nil {
			return nil, &Error{Message: "ensure dir " + p, Output: err.Error()}
		}
		if created {
			resp.Created = append(resp.Created, p)
		}
	}
	s.restorecon(ctx, resp.Created...)
	return resp, nil
}

// EnsureDirOwner chowns any path by names (used by setup for files too).
func EnsureDirOwner(path, owner, group string) (bool, error) {
	uid, gid := -1, -1
	var err error
	if owner != "" {
		if uid, err = lookupUID(owner); err != nil {
			return false, err
		}
	}
	if group != "" {
		if gid, err = lookupGID(group); err != nil {
			return false, err
		}
	}
	if uid < 0 && gid < 0 {
		return false, nil
	}
	return true, os.Lchown(path, uid, gid)
}
