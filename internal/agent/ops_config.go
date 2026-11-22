package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/config"
)

const historyKeep = 20

// protectedFiles are config.yaml and the secret key. /etc/monopanel/ is on
// the write allow-list for the panel's templates and SELinux module, but these
// two are root's: config.yaml holds the release key that vouches for packages
// installed as root, and the secret key decrypts every stored secret. Only
// mp setup, mp update trust and mp config set --local (root, no agent) write
// them, and the panel's own settings in config.yaml change through
// /v1/panel/config alone. Each path is listed as configured and with its
// directory's symlinks resolved.
func (s *Server) protectedFiles() []string {
	def := config.Default()
	var out []string
	seen := map[string]bool{}
	for _, p := range []string{s.cfg.Path(), config.DefaultPath, filepath.Join(s.cfg.ConfigDir, "config.yaml"), s.cfg.SecretKeyFile, def.SecretKeyFile} {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		for _, q := range []string{filepath.Clean(p), resolveDir(filepath.Clean(p))} {
			if !seen[q] {
				seen[q] = true
				out = append(out, q)
			}
		}
	}
	return out
}

// resolveDir returns p with the symlinks of its directory resolved, or p
// itself when the directory does not exist.
func resolveDir(p string) string {
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// protected reports whether p is config.yaml or the secret key.
func (s *Server) protected(p string) bool {
	p = filepath.Clean(p)
	alias := resolveDir(p)
	for _, f := range s.protectedFiles() {
		if p == f || alias == f {
			return true
		}
	}
	return false
}

// coversProtected reports whether p is config.yaml, the secret key or a
// directory above them: removing such a directory, or creating, re-owning or
// re-moding it, reaches the files too. A symlink as the last component is
// not followed by any of those operations, so only the directories above it
// are resolved.
func (s *Server) coversProtected(p string) bool {
	p = filepath.Clean(p)
	cands := []string{p, resolveDir(p)}
	for _, f := range s.protectedFiles() {
		for _, c := range cands {
			if c == f || c == "/" || strings.HasPrefix(f, c+"/") {
				return true
			}
		}
	}
	return false
}

// protectedErr is the refusal for a write that would reach them.
func protectedErr(p string) *Error {
	return &Error{Status: http.StatusForbidden, Message: "path not allowed: " + p + " is, or holds, config.yaml or the secret key; root changes them (mp config set --local, mp update trust), the panel's own settings go through PUT /system/config"}
}

// pathAllowed reports whether p is on the write allow-list, is neither
// config.yaml nor the secret key, and lies in none of the trees only root
// manages (rootTrees: an extra prefix in config.yaml does not open them).
func (s *Server) pathAllowed(p string) bool {
	if s.protected(p) || s.inRootTree(p) {
		return false
	}
	for _, pre := range s.cfg.WritePrefixes() {
		if strings.HasSuffix(pre, "/") {
			if strings.HasPrefix(p, pre) {
				return true
			}
		} else if p == pre {
			return true
		}
	}
	return false
}

// trust is the rule set for root's walks below the write allow-list.
func (s *Server) trust() *dirTrust {
	return &dirTrust{rootUID: s.rootUID, serviceUser: s.cfg.ServiceUser, strict: true}
}

// fileOwners are who may own what config/apply writes besides root: the
// DKIM keys are opendkim's (0600, opendkim alone reads them), the dovecot
// users file is root:dovecot 0640. Handing a configuration of a root daemon
// to anyone else would let them change it without the agent's checks.
var fileOwners = []struct{ below, owner, group string }{
	{"/etc/opendkim/keys/", "opendkim", "opendkim"},
	{"/etc/dovecot/", "", "dovecot"},
}

// checkFileSpec refuses an owner, a group or a mode config/apply must not
// give a file: someone besides root (or the daemon the file is for) as its
// owner or group, a mode that lets group or others write it, or a set-id or
// sticky bit.
func checkFileSpec(f *FileSpec) error {
	if f.Mode&^0o777 != 0 || f.Mode&0o022 != 0 {
		return fmt.Errorf("mode %#o of %s is not allowed: a file the agent writes is never writable by group or others and carries no set-id bit", f.Mode, f.Path)
	}
	owner := f.Owner == "" || f.Owner == "root"
	group := f.Group == "" || f.Group == "root"
	for _, o := range fileOwners {
		if strings.HasPrefix(f.Path, o.below) {
			owner = owner || (o.owner != "" && f.Owner == o.owner)
			group = group || f.Group == o.group
		}
	}
	if !owner {
		return fmt.Errorf("owner %q of %s is not allowed: files the agent writes there are root's", f.Owner, f.Path)
	}
	if !group {
		return fmt.Errorf("group %q of %s is not allowed: files the agent writes there are root's", f.Group, f.Path)
	}
	return nil
}

// walkErr turns the refusals of a walk into a 403 and the rest into a 500.
func walkErr(what string, err error) *Error {
	if errors.Is(err, errUntrusted) || errors.Is(err, errSymlinkInPath) || errors.Is(err, errNotPlainFile) {
		return &Error{Status: http.StatusForbidden, Message: what + ": " + err.Error()}
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return &Error{Message: what, Output: err.Error()}
}

func (s *Server) applyConfigSet(ctx context.Context, req *ApplyConfigSetRequest) (*ApplyConfigSetResponse, error) {
	if len(req.Files) == 0 && len(req.Reload) == 0 && len(req.Restart) == 0 && len(req.Validate) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "empty config set"}
	}
	if err := checkUnits("restart", req.Reload, req.Restart); err != nil {
		return nil, err
	}
	if err := s.vetMountReloads(req.Reload, req.Restart); err != nil {
		return nil, err
	}
	contents := make([][]byte, len(req.Files))
	for i := range req.Files {
		f := &req.Files[i]
		f.Path = filepath.Clean(f.Path)
		if s.protected(f.Path) {
			return nil, protectedErr(f.Path)
		}
		if s.inRootTree(f.Path) {
			return nil, rootTreeErr(f.Path)
		}
		if !filepath.IsAbs(f.Path) || !s.pathAllowed(f.Path) {
			return nil, &Error{Status: http.StatusForbidden, Message: "path not allowed: " + f.Path}
		}
		if f.Mode == 0 {
			f.Mode = 0o644
		}
		if err := checkFileSpec(f); err != nil {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
		contents[i] = []byte(f.Content)
		if f.ContentBase64 != "" {
			b, err := base64.StdEncoding.DecodeString(f.ContentBase64)
			if err != nil {
				return nil, &Error{Status: http.StatusBadRequest, Message: "content_base64 of " + f.Path + ": " + err.Error()}
			}
			contents[i] = b
		}
		// Configurations of daemons that run as root must not carry what
		// would run code or write files as root for their author.
		if err := confcheck.Check(f.Path, contents[i]); err != nil {
			return nil, &Error{Status: http.StatusForbidden, Message: f.Path + ": " + err.Error()}
		}
		if err := confcheck.CheckOwnership(f.Path, f.Owner, f.Group, f.Mode); err != nil {
			return nil, &Error{Status: http.StatusForbidden, Message: f.Path + ": " + err.Error()}
		}
	}
	for _, argv := range req.Validate {
		if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "validator must be an absolute argv"}
		}
		if err := s.validatorAllowed(argv); err != nil {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
	}
	// php-fpm, nginx and Apache act as root, by name, on paths in the
	// php-fpm socket directory and the site log tree: not while those are
	// someone else's (roottrees.go). Vetted before anything is written.
	if err := s.vetRootDaemonTrees(ctx, append(append([]string{}, req.Reload...), req.Restart...), req.Validate); err != nil {
		return nil, err
	}
	tx := s.newFileTx()
	defer tx.close()
	resp := &ApplyConfigSetResponse{Written: []string{}, Unchanged: []string{}, Reloaded: []string{}}
	for i, f := range req.Files {
		changed, err := tx.write(f, contents[i], s.managedDepth(f.Path))
		if err != nil {
			tx.rollback()
			return nil, walkErr("write "+f.Path, err)
		}
		if changed {
			resp.Written = append(resp.Written, f.Path)
		} else {
			resp.Unchanged = append(resp.Unchanged, f.Path)
		}
	}
	for _, argv := range req.Validate {
		out, err := runArgv(ctx, nil, argv...)
		if err != nil {
			tx.rollback()
			s.log.Warn("config validation failed; rolled back", "origin", req.Origin, "validator", argv[0], "err", err)
			return nil, &Error{Status: http.StatusUnprocessableEntity, Message: "validation failed: " + strings.Join(argv, " "), Output: out}
		}
		resp.Validated++
	}
	tx.commitHistory()
	s.restorecon(ctx, append(append([]string{}, resp.Written...), req.Restore...)...)
	s.log.Info("config applied", "origin", req.Origin, "written", len(resp.Written), "unchanged", len(resp.Unchanged))
	if len(resp.Written) == 0 && !req.Force {
		return resp, nil
	}
	if len(req.Reload)+len(req.Restart) > 0 {
		conn, err := connectSystemd(ctx)
		if err != nil {
			return nil, &Error{Message: "files applied but systemd is unreachable", Output: err.Error()}
		}
		defer conn.Close()
		// Each once more after a refusal for the start limit (startlimit.go).
		for _, u := range req.Reload {
			if err := s.startUnit(ctx, conn, "reload-or-restart", u); err != nil {
				return nil, appliedButFailed("reload", err)
			}
			resp.Reloaded = append(resp.Reloaded, u)
		}
		for _, u := range req.Restart {
			if err := s.startUnit(ctx, conn, "restart", u); err != nil {
				return nil, appliedButFailed("restart", err)
			}
			resp.Reloaded = append(resp.Reloaded, u)
		}
	}
	return resp, nil
}

// appliedButFailed is the answer to a config/apply whose files are in place
// and whose reload or restart failed; a refusal of the agent's own — the
// checks before the retry after a start limit — keeps its status.
func appliedButFailed(what string, err error) *Error {
	var ae *Error
	if errors.As(err, &ae) {
		return &Error{Status: ae.Status, Message: "files applied but " + what + " refused: " + ae.Message, Output: ae.Output}
	}
	return &Error{Message: "files applied but " + what + " failed", Output: err.Error()}
}

// fileEntry is a file the transaction replaced, with what to put back.
type fileEntry struct {
	path       string
	dir        *walked // the file's directory, open until the transaction ends
	name       string
	existed    bool
	oldContent []byte
	oldMode    uint32
	oldUID     int
	oldGID     int
}

// fileTx writes a set of files and can put every one back. Each file is
// reached through its directory opened without following an untrusted
// symlink (walkDir), and the directory stays open for the rollback: what
// was checked once is what gets restored.
type fileTx struct {
	s       *Server
	entries []fileEntry
	dirs    []*walked
}

func (s *Server) newFileTx() *fileTx { return &fileTx{s: s} }

// write replaces f.Path with content unless it already holds exactly that
// with the same mode and owner; it reports whether it wrote. follow is how
// many leading components may be root's symlinks (managedDepth).
func (t *fileTx) write(f FileSpec, content []byte, follow int) (bool, error) {
	w, err := walkDir("/", filepath.Dir(f.Path), walkOpts{trust: t.s.trust(), follow: follow, create: true, mode: 0o755, uid: -1, gid: -1})
	if err != nil {
		return false, err
	}
	t.dirs = append(t.dirs, w)
	name := filepath.Base(f.Path)
	// The directory may lie behind a symlink root made (EL's /var/mail):
	// the checks hold for where the file really goes as well.
	if real := filepath.Join(w.resolved, name); real != f.Path {
		if t.s.protected(real) {
			return false, protectedErr(real)
		}
		if t.s.inRootTree(real) {
			return false, rootTreeErr(real)
		}
	}
	old, st, existed, err := readFileAt(w.fd, name)
	if errors.Is(err, errNotPlainFile) {
		return false, fmt.Errorf("%w: refusing to replace a non-regular file (symlink or directory) %s", errNotPlainFile, f.Path)
	}
	if err != nil {
		return false, err
	}
	// Permission bits only, as before: the agent makes no set-id file, not
	// even when it puts one back.
	e := fileEntry{path: f.Path, dir: w, name: name, existed: existed, oldContent: old, oldMode: st.Mode & 0o777, oldUID: int(st.Uid), oldGID: int(st.Gid)}
	uid, gid := -1, -1
	if f.Owner != "" {
		if uid, err = lookupUID(f.Owner); err != nil {
			return false, fmt.Errorf("owner %q: %w", f.Owner, err)
		}
	}
	if f.Group != "" {
		if gid, err = lookupGID(f.Group); err != nil {
			return false, fmt.Errorf("group %q: %w", f.Group, err)
		}
	}
	mode := f.Mode & 0o777
	if existed && bytes.Equal(old, content) && st.Mode&0o777 == mode &&
		(uid == -1 || uid == int(st.Uid)) && (gid == -1 || gid == int(st.Gid)) {
		return false, nil
	}
	if err := writeFileAt(w.fd, w.resolved, name, content, mode, uid, gid); err != nil {
		return false, err
	}
	t.entries = append(t.entries, e)
	return true, nil
}

// rollback puts back every file written so far, newest first.
func (t *fileTx) rollback() {
	for i := len(t.entries) - 1; i >= 0; i-- {
		e := t.entries[i]
		if e.existed {
			_ = writeFileAt(e.dir.fd, e.dir.resolved, e.name, e.oldContent, e.oldMode, e.oldUID, e.oldGID)
		} else {
			_ = unix.Unlinkat(e.dir.fd, e.name, 0)
		}
	}
	t.entries = nil
}

// close releases the directories the transaction opened.
func (t *fileTx) close() {
	for _, w := range t.dirs {
		w.close()
	}
	t.dirs = nil
}

// commitHistory stores the previous version of every replaced file.
func (t *fileTx) commitHistory() {
	h := t.s.history()
	for _, e := range t.entries {
		if e.existed {
			if err := h.store(e.path, e.oldContent); err != nil {
				t.s.log.Warn("configuration history", "path", e.path, "err", err)
			}
		}
	}
}

// confHistory keeps the previous versions of the files the agent replaces,
// <dir>/<path>/<stamp>, the newest historyKeep of each. The directory is
// root's (config.ConfHistoryDir, in the agent's own directory, 0700), made
// and written through directories opened without following a symlink.
type confHistory struct {
	dir   string
	trust *dirTrust
	now   func() time.Time
}

func (s *Server) history() *confHistory {
	return &confHistory{dir: s.cfg.ConfHistoryDir(), trust: s.trust(), now: time.Now}
}

// store saves old as the newest version of p.
func (h *confHistory) store(p string, old []byte) error {
	if h.dir == "" {
		return nil
	}
	// Root's symlinks only on the way to the history itself: inside it the
	// agent makes directories and files, never a link.
	w, err := walkDir("/", filepath.Join(h.dir, strings.TrimPrefix(filepath.Clean(p), "/")), walkOpts{trust: h.trust, follow: len(pathParts(filepath.Clean(h.dir))), create: true, mode: 0o700, uid: -1, gid: -1})
	if err != nil {
		return err
	}
	defer w.close()
	stamp := h.now().UTC().Format("20060102T150405.000Z")
	var fd int
	for i := 0; ; i++ {
		name := stamp
		if i > 0 {
			name = fmt.Sprintf("%s-%d", stamp, i)
		}
		fd, err = unix.Openat(w.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if !errors.Is(err, unix.EEXIST) || i >= 100 {
			break
		}
	}
	if err != nil {
		return &os.PathError{Op: "create", Path: w.resolved, Err: err}
	}
	f := os.NewFile(uintptr(fd), w.resolved)
	if _, err := f.Write(old); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	pruneHistory(w.fd)
	return nil
}

// pruneHistory removes all but the newest historyKeep versions in the
// directory dir (the stamps sort by time). The listing comes from the
// directory opened anew: btrfs lists through an open directory only what it
// held when it was opened, and the newest version came after.
func pruneHistory(dir int) {
	fd, err := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), "history")
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return
	}
	var versions []string
	for _, n := range names {
		var st unix.Stat_t
		if unix.Fstatat(dir, n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG {
			versions = append(versions, n)
		}
	}
	if len(versions) <= historyKeep {
		return
	}
	sort.Strings(versions)
	for _, n := range versions[:len(versions)-historyKeep] {
		_ = unix.Unlinkat(dir, n, 0)
	}
}
