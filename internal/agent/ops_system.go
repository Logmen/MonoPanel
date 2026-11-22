package agent

import (
	"context"
	"net/http"
	"strings"

	"monopanel/internal/sysinfo"
	"monopanel/internal/systemd"
)

func (s *Server) systemInfo(_ context.Context, _ *struct{}) (*sysinfo.Info, error) {
	return sysinfo.Collect(s.profile, s.cfg.WWWRoot, "/var/lib/mysql"), nil
}

// ensureGroup makes the groups the panel uses (internal/api/ops_users.go,
// stack.go, ops_mail_install.go): the web group, the CLI and SFTP groups and
// vmail's. A group of another name could be one sudoers or polkit already
// grants something to.
func (s *Server) ensureGroup(ctx context.Context, req *EnsureGroupRequest) (*EnsureGroupResponse, error) {
	if !nameRe.MatchString(req.Name) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group name"}
	}
	switch req.Name {
	case s.cfg.WebGroup, s.cfg.CLIGroup, sftpGroup, mailUser:
	default:
		return nil, notAllowed("the group "+req.Name, "the panel makes only "+s.cfg.WebGroup+", "+s.cfg.CLIGroup+", "+sftpGroup+" and "+mailUser)
	}
	return EnsureGroup(ctx, req)
}

// ensureUnixUser makes or updates a unix account in the shapes the panel
// uses (checkEnsureUser); EnsureUnixUser itself stays unchecked for mp setup,
// which runs as root before the agent exists.
func (s *Server) ensureUnixUser(ctx context.Context, req *EnsureUnixUserRequest) (*EnsureUnixUserResponse, error) {
	if err := s.checkEnsureUser(req); err != nil {
		return nil, err
	}
	return EnsureUnixUser(ctx, s.profile, req)
}

func (s *Server) service(ctx context.Context, req *ServiceRequest) (*ServiceResponse, error) {
	unit := req.Unit
	if unit != "" && !strings.Contains(unit, ".") {
		unit += ".service"
	}
	if unit != "" && !unitRe.MatchString(unit) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid unit name"}
	}
	conn, err := systemd.Connect(ctx)
	if err != nil {
		return nil, &Error{Message: "systemd unreachable", Output: err.Error()}
	}
	defer conn.Close()
	switch req.Action {
	case "start":
		err = conn.Start(ctx, unit)
	case "stop":
		err = conn.Stop(ctx, unit)
	case "reload":
		err = conn.Reload(ctx, unit)
	case "restart":
		err = conn.Restart(ctx, unit)
	case "reload-or-restart":
		err = conn.ReloadOrRestart(ctx, unit)
	case "enable":
		err = conn.Enable(ctx, unit)
	case "disable":
		err = conn.Disable(ctx, unit)
	case "daemon-reload":
		err = conn.DaemonReload(ctx)
	case "status", "":
	default:
		return nil, &Error{Status: http.StatusBadRequest, Message: "unknown action: " + req.Action}
	}
	if err != nil {
		return nil, &Error{Message: req.Action + " " + unit + " failed", Output: err.Error()}
	}
	resp := &ServiceResponse{}
	if unit != "" {
		st, err := conn.Status(ctx, unit)
		if err != nil {
			return nil, &Error{Message: "status " + unit, Output: err.Error()}
		}
		resp.Status = st
	}
	return resp, nil
}

// pkg drives the package manager with package names only (pkgArgs): a local
// or remote package is code of anybody's making run as root by its
// maintainer scripts. The vendors' release packages the panel installs are
// the one exception, by their exact URLs.
func (s *Server) pkg(ctx context.Context, req *PkgRequest) (*PkgResponse, error) {
	switch req.Action {
	case "update-index", "install", "remove", "query", "available":
	default:
		return nil, &Error{Status: http.StatusBadRequest, Message: "unknown action: " + req.Action}
	}
	pkgs, cleanup, err := s.pkgArgs(ctx, req.Action, req.Packages)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	pm := s.profile.Packages()
	if pm.Name() == "none" {
		return nil, &Error{Status: http.StatusNotImplemented, Message: "package management is unavailable on " + s.profile.Release().PrettyName}
	}
	var argv []string
	switch req.Action {
	case "update-index":
		argv = pm.UpdateIndexArgv()
	case "install":
		if len(req.Packages) == 0 {
			return nil, &Error{Status: http.StatusBadRequest, Message: "no packages"}
		}
		argv = pm.InstallArgv(pkgs)
	case "remove":
		if len(req.Packages) == 0 {
			return nil, &Error{Status: http.StatusBadRequest, Message: "no packages"}
		}
		argv = pm.RemoveArgv(pkgs)
	case "query":
		if len(req.Packages) == 0 {
			return &PkgResponse{Installed: map[string]string{}}, nil
		}
		out, _ := runArgv(ctx, pm.Env(), pm.QueryInstalledArgv(pkgs)...)
		return &PkgResponse{Output: out, Installed: pm.ParseQuery(out)}, nil
	case "available":
		if len(req.Packages) == 0 {
			return &PkgResponse{Available: map[string]string{}}, nil
		}
		// apt-cache / dnf repoquery exit non-zero for unknown names while
		// still reporting the known ones, so the exit code is not an error.
		out, _ := runArgv(ctx, pm.Env(), pm.AvailableArgv(pkgs)...)
		return &PkgResponse{Output: out, Available: pm.ParseAvailable(out)}, nil
	default:
		return nil, &Error{Status: http.StatusBadRequest, Message: "unknown action: " + req.Action}
	}
	s.pkgMu.Lock()
	defer s.pkgMu.Unlock()
	s.log.Info("package manager", "action", req.Action, "packages", req.Packages)
	out, err := runArgv(ctx, pm.Env(), argv...)
	if err != nil {
		return nil, &Error{Message: pm.Name() + " " + req.Action + " failed", Output: out}
	}
	resp := &PkgResponse{Output: out, Installed: map[string]string{}}
	if len(req.Packages) > 0 {
		// a release package is asked for by its name, not its URL
		names := make([]string, 0, len(req.Packages))
		release := s.releasePackages()
		for _, p := range req.Packages {
			if n, ok := release[p]; ok {
				p = n
			}
			names = append(names, p)
		}
		qout, _ := runArgv(ctx, pm.Env(), pm.QueryInstalledArgv(names)...)
		resp.Installed = pm.ParseQuery(qout)
	}
	return resp, nil
}
