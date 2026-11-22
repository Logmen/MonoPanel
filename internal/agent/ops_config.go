package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"monopanel/internal/agent/confcheck"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"monopanel/internal/config"
	"monopanel/internal/systemd"
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

func (s *Server) pathAllowed(p string) bool {
	if s.protected(p) {
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

func (s *Server) applyConfigSet(ctx context.Context, req *ApplyConfigSetRequest) (*ApplyConfigSetResponse, error) {
	if len(req.Files) == 0 && len(req.Reload) == 0 && len(req.Restart) == 0 && len(req.Validate) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "empty config set"}
	}
	for i := range req.Files {
		f := &req.Files[i]
		f.Path = filepath.Clean(f.Path)
		if s.protected(f.Path) {
			return nil, protectedErr(f.Path)
		}
		if !filepath.IsAbs(f.Path) || !s.pathAllowed(f.Path) {
			return nil, &Error{Status: http.StatusForbidden, Message: "path not allowed: " + f.Path}
		}
		if f.Mode == 0 {
			f.Mode = 0o644
		}
		content := []byte(f.Content)
		if f.ContentBase64 != "" {
			b, err := base64.StdEncoding.DecodeString(f.ContentBase64)
			if err != nil {
				return nil, &Error{Status: http.StatusBadRequest, Message: "content_base64 of " + f.Path + ": " + err.Error()}
			}
			content = b
		}
		// Configurations of daemons that run as root must not carry what
		// would run code or write files as root for their author.
		if err := confcheck.Check(f.Path, content); err != nil {
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
	tx := &fileTx{history: s.cfg.ConfHistoryDir()}
	resp := &ApplyConfigSetResponse{Written: []string{}, Unchanged: []string{}, Reloaded: []string{}}
	for _, f := range req.Files {
		changed, err := tx.write(f)
		if err != nil {
			tx.rollback()
			return nil, &Error{Message: "write " + f.Path, Output: err.Error()}
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
		conn, err := systemd.Connect(ctx)
		if err != nil {
			return nil, &Error{Message: "files applied but systemd is unreachable", Output: err.Error()}
		}
		defer conn.Close()
		for _, u := range req.Reload {
			if err := conn.ReloadOrRestart(ctx, u); err != nil {
				return nil, &Error{Message: "files applied but reload failed", Output: err.Error()}
			}
			resp.Reloaded = append(resp.Reloaded, u)
		}
		for _, u := range req.Restart {
			if err := conn.Restart(ctx, u); err != nil {
				return nil, &Error{Message: "files applied but restart failed", Output: err.Error()}
			}
			resp.Reloaded = append(resp.Reloaded, u)
		}
	}
	return resp, nil
}

type fileEntry struct {
	path       string
	existed    bool
	oldContent []byte
	oldMode    os.FileMode
	oldUID     int
	oldGID     int
}

type fileTx struct {
	history string
	entries []fileEntry
}

func (t *fileTx) write(f FileSpec) (bool, error) {
	e := fileEntry{path: f.Path, oldUID: -1, oldGID: -1}
	st, err := os.Lstat(f.Path)
	switch {
	case err == nil:
		if !st.Mode().IsRegular() {
			return false, errors.New("refusing to replace a non-regular file (symlink or directory)")
		}
		e.existed = true
		e.oldMode = st.Mode().Perm()
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			e.oldUID, e.oldGID = int(sys.Uid), int(sys.Gid)
		}
		if e.oldContent, err = os.ReadFile(f.Path); err != nil {
			return false, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
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
	content := []byte(f.Content)
	if f.ContentBase64 != "" {
		if content, err = base64.StdEncoding.DecodeString(f.ContentBase64); err != nil {
			return false, fmt.Errorf("content_base64: %w", err)
		}
	}
	mode := os.FileMode(f.Mode) & os.ModePerm
	if e.existed && bytes.Equal(e.oldContent, content) && e.oldMode == mode &&
		(uid == -1 || uid == e.oldUID) && (gid == -1 || gid == e.oldGID) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return false, err
	}
	if err := atomicWrite(f.Path, content, mode, uid, gid); err != nil {
		return false, err
	}
	t.entries = append(t.entries, e)
	return true, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode, uid, gid int) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".mp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err = tmp.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (t *fileTx) rollback() {
	for i := len(t.entries) - 1; i >= 0; i-- {
		e := t.entries[i]
		if e.existed {
			_ = atomicWrite(e.path, e.oldContent, e.oldMode, e.oldUID, e.oldGID)
		} else {
			_ = os.Remove(e.path)
		}
	}
	t.entries = nil
}

// commitHistory stores the previous version of every replaced file.
func (t *fileTx) commitHistory() {
	if t.history == "" {
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000Z")
	for _, e := range t.entries {
		if !e.existed {
			continue
		}
		dir := filepath.Join(t.history, strings.TrimPrefix(e.path, "/"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, stamp), e.oldContent, 0o600)
		pruneHistory(dir)
	}
}

func pruneHistory(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= historyKeep {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-historyKeep] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}
