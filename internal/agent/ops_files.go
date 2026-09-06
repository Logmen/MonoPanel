package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

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
	gid := -1
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
	if err := atomicWrite(p, content, mode, uid, gid); err != nil {
		return nil, &Error{Message: "write " + p, Output: err.Error()}
	}
	return &EnsureFileResponse{Written: true}, nil
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
	p, err := s.safeHomePath(req.Path)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
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

var aclEntryRe = regexp.MustCompile(`^(d:)?[ugmo]:[a-z0-9_-]*:[rwxX-]{1,4}$`)

func (s *Server) setACL(ctx context.Context, req *SetACLRequest) (*struct{}, error) {
	p, err := s.safeHomePath(req.Path)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	if len(req.Entries) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "no ACL entries"}
	}
	argv := []string{"setfacl"}
	if req.Recursive {
		argv = append(argv, "-R")
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

func (s *Server) removePaths(ctx context.Context, req *RemovePathsRequest) (*RemovePathsResponse, error) {
	resp := &RemovePathsResponse{Removed: []string{}}
	root := filepath.Clean(s.cfg.WWWRoot)
	for _, raw := range req.Paths {
		p := filepath.Clean(raw)
		inHome := strings.HasPrefix(p, root+"/")
		switch {
		case inHome:
			if _, err := s.safeHomePath(p); err != nil {
				return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
			}
			// never remove the home or data directory itself
			if depth := strings.Count(strings.TrimPrefix(p, root+"/"), "/"); depth < 2 {
				return nil, &Error{Status: http.StatusForbidden, Message: "refusing to remove " + p}
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
		if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "validator must be an absolute argv"}
		}
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
