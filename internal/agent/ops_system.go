package agent

import (
	"context"
	"errors"
	"net/http"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"

	"monopanel/internal/sysinfo"
)

func (s *Server) systemInfo(_ context.Context, _ *struct{}) (*sysinfo.Info, error) {
	info := sysinfo.Collect(s.profile, s.cfg.WWWRoot, "/var/lib/mysql")
	info.Container = hostContainer()
	return info, nil
}

// hostContainer is the container the agent runs in, asked once: it does not
// change while the process runs.
var hostContainer = sync.OnceValue(sysinfo.Container)

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
	// useradd -m makes a missing home the new account's: never one on the way
	// to a shared folder's mount point (sharemount.go). An existing account's
	// home is left as it is.
	if req.CreateHome && req.Home != "" && !req.System {
		var unknown user.UnknownUserError
		if _, err := lookupAccount(req.Login); errors.As(err, &unknown) {
			g, release, err := s.holdShareGuard()
			if err != nil {
				return nil, err
			}
			defer release()
			if m, ok := g.holding(req.Home); ok {
				return nil, g.refuse("making "+filepath.Clean(req.Home)+" the home of the new account "+req.Login, m)
			}
		}
	}
	return EnsureUnixUser(ctx, s.profile, req)
}

func (s *Server) service(ctx context.Context, req *ServiceRequest) (*ServiceResponse, error) {
	unit := fullUnit(req.Unit)
	if unit != "" && !unitRe.MatchString(unit) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid unit name"}
	}
	if unit == "" && req.Action != "daemon-reload" && req.Action != "status" && req.Action != "" {
		return nil, &Error{Status: http.StatusBadRequest, Message: req.Action + " needs a unit"}
	}
	if err := serviceAllowed(unit, req.Action); err != nil {
		return nil, err
	}
	if err := s.vetMountStart(unit, req.Action); err != nil {
		return nil, err
	}
	switch req.Action {
	case "start", "restart", "reload", "reload-or-restart":
		if err := s.vetRootDaemonTrees(ctx, []string{unit}, nil); err != nil {
			return nil, err
		}
	}
	conn, err := connectSystemd(ctx)
	if err != nil {
		return nil, &Error{Message: "systemd unreachable", Output: err.Error()}
	}
	defer conn.Close()
	switch req.Action {
	case "start", "restart", "reload-or-restart":
		// Once more after a refusal for the start limit (startlimit.go).
		err = s.startUnit(ctx, conn, req.Action, unit)
	case "stop":
		err = conn.Stop(ctx, unit)
	case "reload":
		err = conn.Reload(ctx, unit)
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
		var ae *Error
		if errors.As(err, &ae) {
			return nil, ae // the checks before the retry refused it
		}
		return nil, &Error{Message: req.Action + " " + unit + " failed", Output: err.Error()}
	}
	resp := &ServiceResponse{}
	if unit != "" {
		st, err := conn.Status(ctx, unit)
		if err != nil {
			return nil, &Error{Message: "status " + unit, Output: err.Error()}
		}
		st.BinaryReplaced = binaryReplaced(ctx, conn, unit, st)
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
	case "update-index", "install", "remove", "query", "available", "candidates", "upgrade":
	default:
		return nil, &Error{Status: http.StatusBadRequest, Message: "unknown action: " + req.Action}
	}
	run := req.Run
	switch {
	case !pkgChanges(req.Action):
	case run == "":
		run = newRunID() // an API older than the agent names none
	case !runIDRe.MatchString(run):
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid run id " + strconv.Quote(run)}
	}
	pkgs, cleanup, err := s.pkgArgs(ctx, req.Action, req.Packages)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// Only reading the index and the database runs no package's scripts:
	// what is installed, what the repositories offer, what an upgrade would
	// bring (candidates, asked for by every look at the Stack page and the
	// doctor).
	switch req.Action {
	case "update-index", "query", "available", "candidates":
	default:
		if err := s.vetPackageScripts(ctx, req.Action); err != nil {
			return nil, err
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
	case "candidates":
		return s.pkgCandidates(ctx, pm, pkgs), nil
	case "upgrade":
		if len(req.Packages) == 0 {
			return nil, &Error{Status: http.StatusBadRequest, Message: "no packages"}
		}
	default:
		return nil, &Error{Status: http.StatusBadRequest, Message: "unknown action: " + req.Action}
	}
	s.pkgMu.Lock()
	defer s.pkgMu.Unlock()
	// pkgMu orders the agent's own runs; a run an earlier agent left going
	// and a panel install are waited for as well (pkg_unit.go).
	s.waitPkgRuns(ctx, pkgUnitPattern, updateUnit)
	if err := ctx.Err(); err != nil {
		// The API went away while this waited for its turn: a change nobody
		// would settle afterwards (reload, restart, record) does not start.
		return nil, &Error{Message: pm.Name() + " " + req.Action + " not started: the request ended while it waited for the package manager", Output: err.Error()}
	}
	if req.Action == "upgrade" {
		// Under the lock: no removal comes between the check and the upgrade.
		if argv, err = s.upgradeArgv(ctx, pm, pkgs); err != nil {
			return nil, err
		}
	}
	s.log.Info("package manager", "action", req.Action, "packages", req.Packages, "unit", pkgUnit(run))
	// The run outlives the request. The request ends whenever the API does
	// — monopanel-api restarted during the nightly upgrade, a job's time
	// limit — and a package manager killed halfway leaves dpkg or rpm with
	// a half-done transaction ("dpkg was interrupted", a package unpacked
	// but not configured, a service a maintainer script stopped and never
	// started) that only an administrator repairs. It outlives the agent as
	// well, in a unit of its own (pkg_unit.go). The lock keeps the next run
	// waiting until this one is through; pkgRunLimit bounds one that hangs.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pkgRunLimit)
	defer cancel()
	out, err := runPackageManager(s, rctx, run, pm.Env(), argv...)
	if req.Action == "update-index" {
		out, err = s.waitIndexLock(ctx, rctx, pm, run, argv, out, err)
	}
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			return nil, &Error{Status: ae.Status, Message: pm.Name() + " " + req.Action + " not started: " + ae.Message, Output: ae.Output}
		}
		return nil, &Error{Message: pm.Name() + " " + req.Action + " failed: " + err.Error(), Output: out}
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
