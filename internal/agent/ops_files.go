package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
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
	root := filepath.Clean(s.cfg.WWWRoot)
	p := filepath.Join(dir, filepath.Base(req.Path))
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
	// The link is made, replaced and handed over relative to its directory,
	// reached without following a symlink: safeHomePath only checked the
	// path, and the account could swap a directory on the way for a link to
	// /etc meanwhile.
	w, err := walkDir(root, belowRoot(root, dir), walkOpts{uid: -1, gid: -1})
	if err != nil {
		return nil, walkErr("symlink "+p, err)
	}
	defer w.close()
	name := filepath.Base(p)
	var st unix.Stat_t
	switch err := unix.Fstatat(w.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		if st.Mode&unix.S_IFMT != unix.S_IFLNK {
			return nil, &Error{Status: http.StatusForbidden, Message: "exists and is not a symlink: " + p}
		}
		buf := make([]byte, unix.PathMax)
		if n, err := unix.Readlinkat(w.fd, name, buf); err == nil && (string(buf[:n]) == req.Target || req.OnlyIfMissing) {
			return &struct{}{}, nil
		}
		if err := unix.Unlinkat(w.fd, name, 0); err != nil {
			return nil, &Error{Message: "replace " + p, Output: err.Error()}
		}
	case !errors.Is(err, unix.ENOENT):
		return nil, &Error{Message: "stat " + p, Output: err.Error()}
	}
	if err := unix.Symlinkat(req.Target, w.fd, name); err != nil {
		return nil, &Error{Message: "symlink " + p, Output: err.Error()}
	}
	_ = unix.Fchownat(w.fd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
	return &struct{}{}, nil
}

var (
	aclEntryRe  = regexp.MustCompile(`^(d:)?[ugmo]:[a-z0-9_-]*:[rwxX-]{1,4}$`)
	aclRemoveRe = regexp.MustCompile(`^(d:)?[ug]:[a-z0-9_-]+$`)
)

// setACL runs setfacl on the path, and with Recursive on everything below
// it, through file descriptors: every entry is opened relative to its
// directory without following a symlink, and setfacl gets /proc/self/fd/N.
// Given the path, setfacl would resolve it again, and the account owning the
// tree could have swapped a directory on the way for a symlink to /etc by
// then: u:<guest>:rwx with a default entry on /etc is root for the guest.
func (s *Server) setACL(ctx context.Context, req *SetACLRequest) (*struct{}, error) {
	p, err := s.safeHomePath(req.Path)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	if len(req.Entries) == 0 && len(req.Remove) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "no ACL entries"}
	}
	var ops, fileOps []string
	add := func(flag, e string) {
		ops = append(ops, flag, e)
		if !strings.HasPrefix(e, "d:") {
			// setfacl -R gives a file only the access entries.
			fileOps = append(fileOps, flag, e)
		}
	}
	for _, e := range req.Remove {
		if !aclRemoveRe.MatchString(e) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid ACL entry to remove: " + e}
		}
		add("-x", e)
		if req.Default && !strings.HasPrefix(e, "d:") {
			add("-x", "d:"+e)
		}
	}
	for _, e := range req.Entries {
		if !aclEntryRe.MatchString(e) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid ACL entry: " + e}
		}
		add("-m", e)
		if req.Default && !strings.HasPrefix(e, "d:") {
			add("-m", "d:"+e)
		}
	}
	top, err := s.openHomeEntry(p)
	if err != nil {
		return nil, err
	}
	if !req.Recursive {
		defer top.close()
		if err := runACL(ctx, ops, []*treeEntry{top}); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	}
	dirs, files := &aclBatch{ctx: ctx}, &aclBatch{ctx: ctx}
	err = walkTree(ctx, top, func(e *treeEntry) error {
		if e.dir {
			return dirs.add(ops, e)
		}
		return files.add(fileOps, e)
	})
	if err == nil {
		err = dirs.flush()
	}
	if err == nil {
		err = files.flush()
	}
	dirs.discard()
	files.discard()
	if err != nil {
		return nil, aclErr(p, err)
	}
	return &struct{}{}, nil
}

var aclGroupRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// siteACLBatch keeps one setfacl call well under the argv limit and the
// descriptors it gets to a few hundred.
const siteACLBatch = 256

// siteACL walks a site tree without following symlinks and sets the web
// group's entries: directories get r-x plus the same default entry, files
// r-- (r-x when the owner may execute them) with an explicit mask. The mask
// keeps the file's group read/write bits, gains r for the web group and
// never gains an x the owner did not give — recalculated by setfacl it would
// take the x of the group:: entry inherited from the directory's default ACL.
// setfacl works on the entries the walk opened (/proc/self/fd/N), never on a
// path the account could redirect after the walk.
func (s *Server) siteACL(ctx context.Context, req *SiteACLRequest) (*SiteACLResponse, error) {
	root, err := s.safeHomePath(req.Root)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	if !aclGroupRe.MatchString(req.Group) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group: " + req.Group}
	}
	top, err := s.openHomeEntry(root)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) && errors.Is(err, os.ErrNotExist) {
			ae.Status = http.StatusNotFound
		}
		return nil, err
	}
	if !top.dir {
		top.close()
		return nil, &Error{Status: http.StatusBadRequest, Message: root + " is not a directory"}
	}
	g := "g:" + req.Group + ":"
	dirOps := []string{"-m", g + "r-x", "-m", "d:" + g + "r-x"}
	// Files are grouped by their entries, one batch of calls per group.
	batches := map[[2]string]*aclBatch{}
	dirs := &aclBatch{ctx: ctx}
	resp := &SiteACLResponse{}
	err = walkTree(ctx, top, func(e *treeEntry) error {
		if e.dir {
			resp.Dirs++
			return dirs.add(dirOps, e)
		}
		key := fileACL(os.FileMode(e.mode))
		b := batches[key]
		if b == nil {
			b = &aclBatch{ctx: ctx}
			batches[key] = b
		}
		resp.Files++
		return b.add([]string{"-m", g + key[0], "-m", "m::" + key[1]}, e)
	})
	if err == nil {
		err = dirs.flush()
	}
	for _, b := range batches {
		if err == nil {
			err = b.flush()
		}
		b.discard()
	}
	dirs.discard()
	if err != nil {
		return nil, aclErr(root, err)
	}
	return resp, nil
}

// aclErr keeps a setfacl failure's own answer and names the tree otherwise.
func aclErr(p string, err error) error {
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	if errors.Is(err, errSymlinkInPath) {
		return &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	return &Error{Message: "walking " + p + ": " + err.Error()}
}

// belowRoot is dir relative to root, "." for root itself; dir is root or
// lies below it (safeHomePath checked).
func belowRoot(root, dir string) string {
	if rel, ok := strings.CutPrefix(dir, root+"/"); ok {
		return rel
	}
	return "."
}

// treeEntry is a directory or a plain file of a tree below the web root,
// opened without following a symlink. fd is an O_PATH descriptor: enough for
// setfacl through /proc/self/fd/N, and it never reads or writes the file.
type treeEntry struct {
	fd   int
	path string
	dir  bool
	mode uint32
}

func (e *treeEntry) close() {
	if e != nil && e.fd >= 0 {
		unix.Close(e.fd) //nolint:errcheck // an O_PATH descriptor
		e.fd = -1
	}
}

// openHomeEntry opens p below the web root (its directories without
// following a symlink, p itself as O_PATH without following one either).
func (s *Server) openHomeEntry(p string) (*treeEntry, error) {
	root := filepath.Clean(s.cfg.WWWRoot)
	w, err := walkDir(root, belowRoot(root, filepath.Dir(p)), walkOpts{uid: -1, gid: -1})
	if errors.Is(err, os.ErrNotExist) {
		return nil, &Error{Status: http.StatusNotFound, Message: err.Error()}
	}
	if err != nil {
		return nil, walkErr("open "+p, err)
	}
	defer w.close()
	e, err := openTreeEntry(w.fd, filepath.Base(p), p)
	if err != nil {
		return nil, walkErr("open "+p, err)
	}
	if e == nil {
		return nil, &Error{Status: http.StatusForbidden, Message: "refusing " + p + ": not a directory or a plain file (a symlink?)"}
	}
	return e, nil
}

// openTreeEntry opens the entry name of dir as O_PATH without following a
// symlink; nil (and no error) for anything but a directory or a plain file.
// A plain file with a second name (a hard link — where the kernel allows
// linking files one does not own, one to /etc/shadow) is skipped too unless
// its owner is the owner of dir: the account's own files it may link as it
// likes.
func openTreeEntry(dir int, name, p string) (*treeEntry, error) {
	fd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd) //nolint:errcheck // an O_PATH descriptor
		return nil, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return &treeEntry{fd: fd, path: p, dir: true, mode: st.Mode & 0o7777}, nil
	case unix.S_IFREG:
		if st.Nlink > 1 {
			var dst unix.Stat_t
			if err := unix.Fstat(dir, &dst); err != nil || dst.Uid != st.Uid {
				unix.Close(fd) //nolint:errcheck // an O_PATH descriptor
				return nil, nil
			}
		}
		return &treeEntry{fd: fd, path: p, mode: st.Mode & 0o7777}, nil
	}
	unix.Close(fd) //nolint:errcheck // an O_PATH descriptor
	return nil, nil
}

// walkTree hands top and every directory and plain file below it to visit,
// directories before what they hold, and never follows a symlink: each
// directory is read through a descriptor of its own, opened relative to its
// parent with O_NOFOLLOW. visit owns the entries it gets (it closes them).
// An entry that vanished meanwhile is skipped, not the whole tree.
func walkTree(ctx context.Context, top *treeEntry, visit func(*treeEntry) error) error {
	if !top.dir {
		return visit(top)
	}
	fd, err := unix.Openat(top.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		top.close()
		return &os.PathError{Op: "open", Path: top.path, Err: err}
	}
	path := top.path
	if err := visit(top); err != nil {
		unix.Close(fd) //nolint:errcheck // a directory opened for reading
		return err
	}
	return walkTreeDir(ctx, fd, path, visit)
}

// walkTreeDir visits what the directory fd (which it closes) holds.
func walkTreeDir(ctx context.Context, fd int, path string, visit func(*treeEntry) error) error {
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil //nolint:nilerr // an unreadable directory is skipped, not the whole tree
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.Join(path, name)
		e, err := openTreeEntry(int(f.Fd()), name, p)
		if err != nil || e == nil {
			continue // vanished, or a symlink, a FIFO, a socket
		}
		sub := -1
		if e.dir {
			if sub, err = unix.Openat(int(f.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err != nil {
				sub = -1
			}
		}
		if err := visit(e); err != nil {
			if sub >= 0 {
				unix.Close(sub) //nolint:errcheck // a directory opened for reading
			}
			return err
		}
		if sub >= 0 {
			if err := walkTreeDir(ctx, sub, p, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// aclBatch collects entries that get the same setfacl operations and runs
// setfacl for each siteACLBatch of them.
type aclBatch struct {
	ctx     context.Context
	ops     []string
	entries []*treeEntry
}

// add queues e for ops; a batch holds one set of operations, so a change of
// them runs what was queued first.
func (b *aclBatch) add(ops []string, e *treeEntry) error {
	if len(b.entries) > 0 && strings.Join(b.ops, "\x00") != strings.Join(ops, "\x00") {
		if err := b.flush(); err != nil {
			e.close()
			return err
		}
	}
	b.ops = ops
	b.entries = append(b.entries, e)
	if len(b.entries) >= siteACLBatch {
		return b.flush()
	}
	return nil
}

func (b *aclBatch) flush() error {
	if len(b.entries) == 0 {
		return nil
	}
	err := runACL(b.ctx, b.ops, b.entries)
	b.discard()
	return err
}

func (b *aclBatch) discard() {
	for _, e := range b.entries {
		e.close()
	}
	b.entries = nil
}

// runACL runs setfacl with ops on the entries, handed over as descriptors:
// the child gets them as 3, 4, … and setfacl reaches each through
// /proc/self/fd/N, the very file the walk opened.
func runACL(ctx context.Context, ops []string, entries []*treeEntry) error {
	if len(ops) == 0 || len(entries) == 0 {
		return nil
	}
	argv := append([]string{"setfacl"}, ops...)
	argv = append(argv, "--")
	files := make([]*os.File, 0, len(entries))
	for i, e := range entries {
		dup, err := unix.Dup(e.fd)
		if err != nil {
			for _, f := range files {
				f.Close()
			}
			return &os.PathError{Op: "dup", Path: e.path, Err: err}
		}
		files = append(files, os.NewFile(uintptr(dup), e.path))
		argv = append(argv, fmt.Sprintf("/proc/self/fd/%d", 3+i))
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.ExtraFiles = files
	cmd.WaitDelay = 5 * time.Second
	out := &tailBuffer{max: 64 * 1024}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		msg := out.String()
		for i, e := range entries {
			msg = strings.ReplaceAll(msg, fmt.Sprintf("/proc/self/fd/%d", 3+i), e.path)
		}
		if msg == "" {
			msg = err.Error()
		}
		return &Error{Message: "setfacl failed", Output: msg}
	}
	return nil
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
		if !filepath.IsAbs(p) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "absolute path required: " + raw}
		}
		// /etc/monopanel/ is on the allow-list, and a recursive removal of it
		// would take config.yaml and the secret key along.
		if s.coversProtected(p) {
			return nil, protectedErr(p)
		}
		var removed bool
		var err error
		switch {
		case strings.HasPrefix(p, root+"/"):
			if _, err := s.safeHomePath(p); err != nil {
				return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
			}
			removed, err = s.removeHomePath(root, p, req.Recursive)
		case s.pathAllowed(p) || s.pathAllowed(p+"/"):
			removed, err = s.removeRootPath(p, req.Recursive, s.managedDepth(p))
		default:
			return nil, &Error{Status: http.StatusForbidden, Message: "path not allowed: " + p}
		}
		if err != nil {
			return nil, err
		}
		if removed {
			resp.Removed = append(resp.Removed, p)
		}
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

// removeHomePath removes p below the web root relative to its directory,
// reached without following a symlink: the account owns every directory on
// the way and could swap one for a link to /etc between a check of the path
// and a removal by name. The home and the data directory themselves never
// go; an empty directory right under the home (the mount point of a shared
// folder after unmounting) may, and only with rmdir semantics.
func (s *Server) removeHomePath(root, p string, recursive bool) (bool, error) {
	rel := strings.TrimPrefix(p, root+"/")
	w, err := walkDir(root, filepath.Dir(rel), walkOpts{uid: -1, gid: -1})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, walkErr("remove "+p, err)
	}
	defer w.close()
	name := filepath.Base(p)
	if depth := strings.Count(rel, "/"); depth < 2 {
		var st unix.Stat_t
		if err := unix.Fstatat(w.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			return false, nil
		} else if err != nil {
			return false, &Error{Message: "remove " + p, Output: err.Error()}
		}
		if depth < 1 || recursive || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return false, &Error{Status: http.StatusForbidden, Message: "refusing to remove " + p}
		}
		if err := unix.Unlinkat(w.fd, name, unix.AT_REMOVEDIR); err != nil {
			if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
				return false, &Error{Status: http.StatusForbidden, Message: "refusing to remove " + p}
			}
			return false, &Error{Message: "remove " + p, Output: err.Error()}
		}
		return true, nil
	}
	return removeIn(w, name, p, recursive)
}

// removeRootPath removes p on the write allow-list relative to its
// directory, reached through trusted directories only (walkDir with
// dirTrust), root's symlinks resolved in the first follow components alone.
func (s *Server) removeRootPath(p string, recursive bool, follow int) (bool, error) {
	w, err := walkDir("/", filepath.Dir(p), walkOpts{trust: s.trust(), follow: follow, uid: -1, gid: -1})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, walkErr("remove "+p, err)
	}
	defer w.close()
	name := filepath.Base(p)
	if real := filepath.Join(w.resolved, name); real != p && (s.coversProtected(real) || s.inRootTree(real)) {
		return false, &Error{Status: http.StatusForbidden, Message: "path not allowed: " + p + " leads to " + real}
	}
	return removeIn(w, name, p, recursive)
}

func removeIn(w *walked, name, p string, recursive bool) (bool, error) {
	removed, err := removeEntryAt(w.fd, name, p, recursive)
	switch {
	case errors.Is(err, errIsDir):
		return false, &Error{Status: http.StatusBadRequest, Message: "is a directory (use recursive): " + p}
	case err != nil:
		return false, &Error{Message: "remove " + p, Output: err.Error()}
	}
	return removed, nil
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
