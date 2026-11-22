package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
	"monopanel/internal/systemd"
)

// safeHomePath checks that p lies below WWWRoot and that no component below
// WWWRoot is a symlink: clients own those directories and could otherwise
// redirect a root write elsewhere. It returns the cleaned path.
func (s *Server) safeHomePath(p string) (string, error) {
	p = filepath.Clean(p)
	root := filepath.Clean(s.cfg.WWWRoot)
	if !filepath.IsAbs(p) || !strings.HasPrefix(p, root+"/") {
		return "", fmt.Errorf("path %s is outside %s", p, root)
	}
	if s.coversProtected(p) { // only a www_root set to / or /etc gets here
		return "", errors.New(protectedErr(p).Message)
	}
	rel := strings.TrimPrefix(p, root+"/")
	cur := root
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return p, nil // remaining components do not exist yet
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to follow symlink %s", cur)
		}
	}
	return p, nil
}

func (s *Server) ensureFile(_ context.Context, req *EnsureFileRequest) (*EnsureFileResponse, error) {
	if req.Owner == "" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "owner is required"}
	}
	p, err := s.safeHomePath(req.Path)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	if st, err := os.Lstat(p); err == nil {
		if !st.Mode().IsRegular() {
			return nil, &Error{Status: http.StatusForbidden, Message: "not a regular file: " + p}
		}
		if req.OnlyIfMissing {
			return &EnsureFileResponse{}, nil
		}
	}
	if req.OnlyIfDirEmpty {
		entries, err := os.ReadDir(filepath.Dir(p))
		if err == nil && len(entries) > 0 {
			return &EnsureFileResponse{}, nil
		}
	}
	content := []byte(req.Content)
	if req.ContentBase64 != "" {
		if content, err = base64.StdEncoding.DecodeString(req.ContentBase64); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "content_base64: " + err.Error()}
		}
	}
	uid, err := lookupUID(req.Owner)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "owner: " + err.Error()}
	}
	var gid int
	if req.Group != "" {
		if gid, err = lookupGID(req.Group); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "group: " + err.Error()}
		}
	} else {
		gid = primaryGID(req.Owner)
	}
	mode := os.FileMode(req.Mode) & os.ModePerm
	if mode == 0 {
		mode = 0o644
	}
	if err := writeHomeFile(filepath.Clean(s.cfg.WWWRoot), p, content, mode, uid, gid); err != nil {
		if errors.Is(err, errSymlinkInPath) {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
		return nil, &Error{Message: "write " + p, Output: err.Error()}
	}
	return &EnsureFileResponse{Written: true}, nil
}

// openBelow opens p (below root, a directory someone else may write to, such
// as the panel's log directory) for reading through directories opened one by
// one without following a symlink, the file itself with O_NOFOLLOW (and
// O_NONBLOCK, so a FIFO planted there cannot hang the agent): whoever swaps a
// component for a symlink makes the call fail instead of having root read the
// link's target.
func openBelow(root, p string) (*os.File, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("%s is not below %s", p, root)
	}
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() { unix.Close(dir) }() //nolint:errcheck // a directory opened for reading
	parts := strings.Split(rel, "/")
	cur := root
	for i, name := range parts {
		cur = filepath.Join(cur, name)
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		fd, err := unix.Openat(dir, name, flags, 0)
		if errors.Is(err, unix.ELOOP) || i < len(parts)-1 && errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("%w: refusing %s", errSymlinkInPath, cur)
		}
		if errors.Is(err, unix.ENOENT) {
			return nil, &os.PathError{Op: "open", Path: cur, Err: os.ErrNotExist}
		}
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: cur, Err: err}
		}
		if i == len(parts)-1 {
			return os.NewFile(uintptr(fd), p), nil
		}
		unix.Close(dir) //nolint:errcheck // a directory opened for reading
		dir = fd
	}
	return nil, fmt.Errorf("%s: nothing to open", p)
}

// writeHomeFile replaces p (below root, the web root) atomically like
// atomicWrite, but reaches its directory through directories opened one by
// one without following a symlink, and makes, re-owns and renames the file
// relative to that directory: safeHomePath only checks the path, and a client
// could swap a component for a symlink right after, turning a root write into
// one of /etc/cron.d or /root/.ssh. Every directory on the way must exist.
func writeHomeFile(root, p string, data []byte, mode os.FileMode, uid, gid int) (err error) {
	rel, err := filepath.Rel(root, filepath.Dir(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is not below %s", p, root)
	}
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() { unix.Close(dir) }() //nolint:errcheck // a directory opened for reading
	if rel != "." {
		cur := root
		for _, name := range strings.Split(rel, "/") {
			cur = filepath.Join(cur, name)
			next, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return fmt.Errorf("%w: refusing %s", errSymlinkInPath, cur)
			}
			if err != nil {
				return &os.PathError{Op: "open", Path: cur, Err: err}
			}
			unix.Close(dir) //nolint:errcheck // a directory opened for reading
			dir = next
		}
	}
	base := filepath.Base(p)
	tmp := fmt.Sprintf(".%s.mp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &os.PathError{Op: "create", Path: filepath.Join(filepath.Dir(p), tmp), Err: err}
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() {
		if err != nil {
			f.Close()
			unix.Unlinkat(dir, tmp, 0) //nolint:errcheck // best effort: the temporary file of a failed write
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err = f.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// renameat replaces whatever entry has that name (a symlink planted there
	// is replaced itself, its target stays untouched).
	if err = unix.Renameat(dir, tmp, dir, base); err != nil {
		return &os.PathError{Op: "rename", Path: p, Err: err}
	}
	return nil
}

func primaryGID(login string) int {
	if u, err := lookupUser(login); err == nil {
		return u.gid
	}
	return -1
}

func (s *Server) ensureSymlink(_ context.Context, req *EnsureSymlinkRequest) (*struct{}, error) {
	if req.Owner == "" || req.Target == "" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "owner and target are required"}
	}
	// The link itself is expected to exist as a symlink on every re-apply;
	// only the directories above it must not be ones.
	dir, err := s.safeHomePath(filepath.Dir(filepath.Clean(req.Path)))
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	p := filepath.Join(dir, filepath.Base(req.Path))
	if st, err := os.Lstat(p); err == nil {
		if st.Mode()&os.ModeSymlink == 0 {
			return nil, &Error{Status: http.StatusForbidden, Message: "exists and is not a symlink: " + p}
		}
		if cur, _ := os.Readlink(p); cur == req.Target || req.OnlyIfMissing {
			return &struct{}{}, nil
		}
		if err := os.Remove(p); err != nil {
			return nil, err
		}
	}
	if err := os.Symlink(req.Target, p); err != nil {
		return nil, &Error{Message: "symlink " + p, Output: err.Error()}
	}
	uid, err := lookupUID(req.Owner)
	if err != nil {
		return nil, err
	}
	gid := primaryGID(req.Owner)
	if req.Group != "" {
		if gid, err = lookupGID(req.Group); err != nil {
			return nil, err
		}
	}
	_ = os.Lchown(p, uid, gid)
	return &struct{}{}, nil
}

var (
	aclEntryRe  = regexp.MustCompile(`^(d:)?[ugmo]:[a-z0-9_-]*:[rwxX-]{1,4}$`)
	aclRemoveRe = regexp.MustCompile(`^(d:)?[ug]:[a-z0-9_-]+$`)
)

func (s *Server) setACL(ctx context.Context, req *SetACLRequest) (*struct{}, error) {
	p, err := s.safeHomePath(req.Path)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	if len(req.Entries) == 0 && len(req.Remove) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "no ACL entries"}
	}
	argv := []string{"setfacl"}
	if req.Recursive {
		argv = append(argv, "-R")
	}
	for _, e := range req.Remove {
		if !aclRemoveRe.MatchString(e) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid ACL entry to remove: " + e}
		}
		argv = append(argv, "-x", e)
		if req.Default && !strings.HasPrefix(e, "d:") {
			argv = append(argv, "-x", "d:"+e)
		}
	}
	for _, e := range req.Entries {
		if !aclEntryRe.MatchString(e) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid ACL entry: " + e}
		}
		argv = append(argv, "-m", e)
		if req.Default && !strings.HasPrefix(e, "d:") {
			argv = append(argv, "-m", "d:"+e)
		}
	}
	argv = append(argv, p)
	if out, err := runArgv(ctx, nil, argv...); err != nil {
		return nil, &Error{Message: "setfacl failed", Output: out}
	}
	return &struct{}{}, nil
}

var aclGroupRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// siteACLBatch keeps one setfacl call well under the argv limit.
const siteACLBatch = 256

// siteACL walks a site tree without following symlinks and sets the web
// group's entries: directories get r-x plus the same default entry, files
// r-- (r-x when the owner may execute them) with an explicit mask. The mask
// keeps the file's group read/write bits, gains r for the web group and
// never gains an x the owner did not give — recalculated by setfacl it would
// take the x of the group:: entry inherited from the directory's default ACL.
// setfacl -P skips a path that became a symlink after the walk.
func (s *Server) siteACL(ctx context.Context, req *SiteACLRequest) (*SiteACLResponse, error) {
	root, err := s.safeHomePath(req.Root)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	if !aclGroupRe.MatchString(req.Group) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group: " + req.Group}
	}
	st, err := os.Lstat(root)
	if err != nil {
		return nil, &Error{Status: http.StatusNotFound, Message: err.Error()}
	}
	if !st.IsDir() {
		return nil, &Error{Status: http.StatusBadRequest, Message: root + " is not a directory"}
	}
	g := "g:" + req.Group + ":"
	var dirs []string
	files := map[[2]string][]string{} // {named entry, mask} -> paths
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a vanished or unreadable entry is skipped, not the whole tree
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
		case d.IsDir():
			dirs = append(dirs, p)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return nil //nolint:nilerr // the file vanished between the listing and the stat
			}
			key := fileACL(info.Mode().Perm())
			files[key] = append(files[key], p)
		}
		return nil
	})
	if err != nil {
		return nil, &Error{Message: "walking " + root + ": " + err.Error()}
	}
	run := func(entries []string, paths []string) error {
		for len(paths) > 0 {
			n := min(len(paths), siteACLBatch)
			argv := []string{"setfacl", "-P"}
			for _, e := range entries {
				argv = append(argv, "-m", e)
			}
			argv = append(argv, "--")
			argv = append(argv, paths[:n]...)
			if out, err := runArgv(ctx, nil, argv...); err != nil {
				return &Error{Message: "setfacl failed", Output: out}
			}
			paths = paths[n:]
		}
		return nil
	}
	if err := run([]string{g + "r-x", "d:" + g + "r-x"}, dirs); err != nil {
		return nil, err
	}
	resp := &SiteACLResponse{Dirs: len(dirs)}
	for key, paths := range files {
		if err := run([]string{g + key[0], "m::" + key[1]}, paths); err != nil {
			return nil, err
		}
		resp.Files += len(paths)
	}
	return resp, nil
}

// fileACL is the web group's entry and the mask for a file with this mode:
// read for the web group, x only where the owner has it, the group's own
// write bit kept as it was.
func fileACL(mode fs.FileMode) [2]string {
	named, mask := "r--", []byte("r--")
	if mode&0o020 != 0 {
		mask[1] = 'w'
	}
	if mode&0o100 != 0 {
		named, mask[2] = "r-x", 'x'
	}
	return [2]string{named, string(mask)}
}

func (s *Server) removePaths(ctx context.Context, req *RemovePathsRequest) (*RemovePathsResponse, error) {
	// The checks to run afterwards are vetted before anything is removed.
	for _, argv := range req.Validate {
		if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "validator must be an absolute argv"}
		}
		if err := s.validatorAllowed(argv); err != nil {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
	}
	resp := &RemovePathsResponse{Removed: []string{}}
	root := filepath.Clean(s.cfg.WWWRoot)
	for _, raw := range req.Paths {
		p := filepath.Clean(raw)
		// /etc/monopanel/ is on the allow-list, and a recursive removal of it
		// would take config.yaml and the secret key along.
		if s.coversProtected(p) {
			return nil, protectedErr(p)
		}
		inHome := strings.HasPrefix(p, root+"/")
		switch {
		case inHome:
			if _, err := s.safeHomePath(p); err != nil {
				return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
			}
			// never remove the home or data directory itself; an empty directory
			// right under the home (the mount point of a shared folder after
			// unmounting) may go, and only with rmdir semantics
			if depth := strings.Count(strings.TrimPrefix(p, root+"/"), "/"); depth < 2 {
				st, err := os.Lstat(p)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				entries, _ := os.ReadDir(p)
				if err != nil || depth < 1 || req.Recursive || !st.IsDir() || len(entries) > 0 {
					return nil, &Error{Status: http.StatusForbidden, Message: "refusing to remove " + p}
				}
				if err := os.Remove(p); err != nil {
					return nil, &Error{Message: "remove " + p, Output: err.Error()}
				}
				resp.Removed = append(resp.Removed, p)
				continue
			}
		case s.pathAllowed(p) || s.pathAllowed(p+"/"):
		default:
			return nil, &Error{Status: http.StatusForbidden, Message: "path not allowed: " + p}
		}
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			if !req.Recursive {
				return nil, &Error{Status: http.StatusBadRequest, Message: "is a directory (use recursive): " + p}
			}
			if err := os.RemoveAll(p); err != nil {
				return nil, &Error{Message: "remove " + p, Output: err.Error()}
			}
		} else if err := os.Remove(p); err != nil {
			return nil, &Error{Message: "remove " + p, Output: err.Error()}
		}
		resp.Removed = append(resp.Removed, p)
	}
	for _, argv := range req.Validate {
		if out, err := runArgv(ctx, nil, argv...); err != nil {
			return nil, &Error{Status: http.StatusUnprocessableEntity, Message: "validation failed after removal: " + strings.Join(argv, " "), Output: out}
		}
	}
	if len(req.Reload) > 0 && len(resp.Removed) > 0 {
		conn, err := systemd.Connect(ctx)
		if err != nil {
			return nil, &Error{Message: "systemd unreachable", Output: err.Error()}
		}
		defer conn.Close()
		for _, u := range req.Reload {
			if err := conn.ReloadOrRestart(ctx, u); err != nil {
				return nil, &Error{Message: "reload " + u, Output: err.Error()}
			}
		}
	}
	return resp, nil
}

var apacheNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (s *Server) apacheCtl(ctx context.Context, req *ApacheCtlRequest) (*ApacheCtlResponse, error) {
	if s.profile.Family() != osprofile.FamilyDebian {
		return nil, &Error{Status: http.StatusNotImplemented, Message: "a2* tools exist on Debian/Ubuntu only"}
	}
	tools := map[string]string{"enmod": "a2enmod", "dismod": "a2dismod", "enconf": "a2enconf", "disconf": "a2disconf", "ensite": "a2ensite", "dissite": "a2dissite"}
	tool, ok := tools[req.Action]
	if !ok || !apacheNameRe.MatchString(req.Name) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid apache action or name"}
	}
	out, err := runArgv(ctx, nil, "/usr/sbin/"+tool, "-q", req.Name)
	if err != nil {
		return nil, &Error{Message: tool + " " + req.Name + " failed", Output: out}
	}
	return &ApacheCtlResponse{Output: out}, nil
}

// ensure the syscall import stays used on all platforms
var _ = syscall.Stat_t{}
