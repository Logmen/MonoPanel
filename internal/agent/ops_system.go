package agent

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"monopanel/internal/sysinfo"
	"monopanel/internal/systemd"
)

func (s *Server) systemInfo(_ context.Context, _ *struct{}) (*sysinfo.Info, error) {
	return sysinfo.Collect(s.profile, s.cfg.WWWRoot, "/var/lib/mysql"), nil
}

func (s *Server) ensureGroup(ctx context.Context, req *EnsureGroupRequest) (*EnsureGroupResponse, error) {
	return EnsureGroup(ctx, req)
}

func (s *Server) ensureUnixUser(ctx context.Context, req *EnsureUnixUserRequest) (*EnsureUnixUserResponse, error) {
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

func (s *Server) pkg(ctx context.Context, req *PkgRequest) (*PkgResponse, error) {
	downloads := filepath.Join(s.cfg.DataDir, "downloads") + "/"
	for _, p := range req.Packages {
		localPkg := req.Action == "install" && strings.HasPrefix(p, downloads) && (strings.HasSuffix(p, ".deb") || strings.HasSuffix(p, ".rpm")) && !strings.Contains(p, "..")
		if !pkgRe.MatchString(p) && !(req.Action == "install" && rpmURL.MatchString(p)) && !localPkg {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid package name: " + p}
		}
	}
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
		argv = pm.InstallArgv(req.Packages)
	case "remove":
		if len(req.Packages) == 0 {
			return nil, &Error{Status: http.StatusBadRequest, Message: "no packages"}
		}
		argv = pm.RemoveArgv(req.Packages)
	case "query":
		if len(req.Packages) == 0 {
			return &PkgResponse{Installed: map[string]string{}}, nil
		}
		out, _ := runArgv(ctx, pm.Env(), pm.QueryInstalledArgv(req.Packages)...)
		return &PkgResponse{Output: out, Installed: pm.ParseQuery(out)}, nil
	case "available":
		if len(req.Packages) == 0 {
			return &PkgResponse{Available: map[string]string{}}, nil
		}
		// apt-cache / dnf repoquery exit non-zero for unknown names while
		// still reporting the known ones, so the exit code is not an error.
		out, _ := runArgv(ctx, pm.Env(), pm.AvailableArgv(req.Packages)...)
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
		qout, _ := runArgv(ctx, pm.Env(), pm.QueryInstalledArgv(req.Packages)...)
		resp.Installed = pm.ParseQuery(qout)
	}
	return resp, nil
}
