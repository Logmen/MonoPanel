package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"monopanel/internal/osprofile"
)

// EnsureGroup creates a group when missing. Exported so `mp setup` (which
// runs as root before the agent exists) can reuse the exact same logic.
func EnsureGroup(ctx context.Context, req *EnsureGroupRequest) (*EnsureGroupResponse, error) {
	if !nameRe.MatchString(req.Name) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group name"}
	}
	if gid, err := lookupGID(req.Name); err == nil {
		return &EnsureGroupResponse{GID: gid}, nil
	}
	argv := []string{"groupadd"}
	if req.System {
		argv = append(argv, "-r")
	}
	argv = append(argv, req.Name)
	if out, err := runArgv(ctx, nil, argv...); err != nil {
		return nil, &Error{Message: "groupadd failed", Output: out}
	}
	gid, err := lookupGID(req.Name)
	if err != nil {
		return nil, err
	}
	return &EnsureGroupResponse{GID: gid, Created: true}, nil
}

// EnsureUnixUser creates a unix user when missing and adds supplementary groups.
func EnsureUnixUser(ctx context.Context, profile osprofile.Profile, req *EnsureUnixUserRequest) (*EnsureUnixUserResponse, error) {
	if !nameRe.MatchString(req.Login) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	for _, g := range req.Groups {
		if !nameRe.MatchString(g) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group name: " + g}
		}
	}
	created := false
	u, err := user.Lookup(req.Login)
	if err != nil {
		shell := req.Shell
		if shell == "" {
			shell = profile.NologinShell()
		}
		argv := []string{"useradd"}
		if req.System {
			argv = append(argv, "-r")
		}
		if req.Home != "" {
			argv = append(argv, "-d", req.Home)
		}
		if req.CreateHome {
			argv = append(argv, "-m")
		} else {
			argv = append(argv, "-M")
		}
		if req.Comment != "" {
			argv = append(argv, "-c", req.Comment)
		}
		if req.PrimaryGroup != "" {
			if !nameRe.MatchString(req.PrimaryGroup) {
				return nil, &Error{Status: http.StatusBadRequest, Message: "invalid primary group"}
			}
			argv = append(argv, "-g", req.PrimaryGroup)
		} else {
			argv = append(argv, "-U")
		}
		argv = append(argv, "-s", shell, req.Login)
		if out, err := runArgv(ctx, nil, argv...); err != nil {
			return nil, &Error{Message: "useradd failed", Output: out}
		}
		created = true
		if u, err = user.Lookup(req.Login); err != nil {
			return nil, err
		}
	}
	if !created && req.UpdateShell && req.Shell != "" {
		if out, err := runArgv(ctx, nil, "usermod", "-s", req.Shell, req.Login); err != nil {
			return nil, &Error{Message: "usermod -s failed", Output: out}
		}
	}
	for _, g := range req.Groups {
		if out, err := runArgv(ctx, nil, "usermod", "-aG", g, req.Login); err != nil {
			return nil, &Error{Message: "usermod failed for group " + g, Output: out}
		}
	}
	for _, g := range req.RemoveGroups {
		if !nameRe.MatchString(g) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group name: " + g}
		}
		runArgv(ctx, nil, "gpasswd", "-d", req.Login, g) // not a member: ignore
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &EnsureUnixUserResponse{UID: uid, GID: gid, Home: u.HomeDir, Created: created}, nil
}

// EnsureDir creates one directory with mode and ownership (idempotent).
func EnsureDir(d DirSpec) (created bool, err error) {
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
		if err := os.Chown(p, uid, gid); err != nil {
			return created, err
		}
	}
	return created, nil
}

func (s *Server) dirAllowed(p string) bool {
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

func (s *Server) ensureDirs(_ context.Context, req *EnsureDirsRequest) (*EnsureDirsResponse, error) {
	resp := &EnsureDirsResponse{Created: []string{}}
	for _, d := range req.Dirs {
		p := filepath.Clean(d.Path)
		if !filepath.IsAbs(p) || !s.dirAllowed(p) {
			return nil, &Error{Status: http.StatusForbidden, Message: "directory not allowed: " + p}
		}
		d.Path = p
		created, err := EnsureDir(d)
		if err != nil {
			return nil, &Error{Message: "ensure dir " + p, Output: err.Error()}
		}
		if created {
			resp.Created = append(resp.Created, p)
		}
	}
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
