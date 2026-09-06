package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/render"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

// App services: one systemd unit per app, running under the owner's unix
// account. The panel writes /etc/systemd/system/monopanel-app-<login>-<name>.service.

const unitDir = "/etc/systemd/system"

var (
	appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	envPairRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^"\r\n]*$`)
)

type appsOutput struct {
	Body []*apitypes.AppStatus
}

type appOutput struct {
	Status int
	Body   *apitypes.AppStatus
}

type appCreateInput struct {
	Login string `path:"login"`
	Body  apitypes.AppRequest
}

type appNameInput struct {
	Login string `path:"login"`
	Name  string `path:"name" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$"`
}

type appUpdateInput struct {
	Login string `path:"login"`
	Name  string `path:"name" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$"`
	Body  apitypes.AppUpdateRequest
}

type appActionInput struct {
	Login  string `path:"login"`
	Name   string `path:"name" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$"`
	Action string `path:"action" enum:"start,stop,restart"`
}

type appLogsInput struct {
	Login string `path:"login"`
	Name  string `path:"name" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$"`
	Lines int    `query:"lines" default:"100" minimum:"1" maximum:"2000"`
}

func (s *Server) homeOf(u *store.User) string {
	if u.Home != "" {
		return u.Home
	}
	return path.Join(s.cfg.WWWRoot, u.Login)
}

// insideHome reports whether p is home itself or below it (after cleaning).
func insideHome(home, p string) bool {
	p = path.Clean(p)
	return p == home || strings.HasPrefix(p, home+"/")
}

// validateApp checks paths (inside the home), the executable and env pairs.
func (s *Server) validateApp(ctx context.Context, u *store.User, a *store.App) error {
	home := s.homeOf(u)
	if !appNameRe.MatchString(a.Name) {
		return errors.New("name must match ^[a-z0-9][a-z0-9_-]{0,31}$")
	}
	a.Command = strings.TrimSpace(a.Command)
	if a.Command == "" || len(a.Command) > 1000 || strings.ContainsAny(a.Command, "\n\r") {
		return errors.New("command must be one line up to 1000 characters")
	}
	exe := strings.Fields(a.Command)[0]
	if !path.IsAbs(exe) {
		return errors.New("command must start with an absolute path (e.g. /var/www/" + u.Login + "/data/venv/bin/gunicorn or /usr/bin/node)")
	}
	if a.WorkDir == "" {
		a.WorkDir = path.Join(home, "data")
	}
	a.WorkDir = path.Clean(a.WorkDir)
	if !path.IsAbs(a.WorkDir) || !insideHome(home, a.WorkDir) {
		return errors.New("workdir must be an absolute path inside " + home)
	}
	if a.EnvFile != "" {
		a.EnvFile = path.Clean(a.EnvFile)
		if !path.IsAbs(a.EnvFile) || !insideHome(home, a.EnvFile) {
			return errors.New("env_file must be an absolute path inside " + home)
		}
	}
	if len(a.Env) > 64 {
		return errors.New("env: at most 64 entries")
	}
	for _, e := range a.Env {
		if !envPairRe.MatchString(e) {
			return fmt.Errorf("env: %q must look like KEY=value (no double quotes)", e)
		}
	}
	switch a.Restart {
	case "", "always":
		a.Restart = "always"
	case "on-failure", "no":
	default:
		return errors.New("restart must be always, on-failure or no")
	}
	paths := []string{exe, a.WorkDir}
	if a.EnvFile != "" {
		paths = append(paths, a.EnvFile)
	}
	st, err := s.agent.Stat(ctx, paths...)
	if err != nil {
		return err
	}
	for _, e := range st.Entries {
		switch {
		case !e.Exists:
			return fmt.Errorf("%s does not exist", e.Path)
		case e.Path == a.WorkDir && !e.IsDir:
			return fmt.Errorf("workdir %s is not a directory", e.Path)
		case e.Path != a.WorkDir && e.IsDir:
			return fmt.Errorf("%s is a directory", e.Path)
		}
	}
	return nil
}

// applyApp writes the unit, reloads systemd and (re)starts or stops the app.
func (s *Server) applyApp(ctx context.Context, a *store.App) (*systemd.Status, error) {
	unit := a.Unit()
	text, err := s.render.Render("systemd/app.service.tmpl", render.AppUnit{
		Login: a.Login, Name: a.Name, Description: a.Description, Command: a.Command, WorkDir: a.WorkDir, EnvFile: a.EnvFile, Env: a.Env, Restart: a.Restart,
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: path.Join(unitDir, unit), Content: text, Mode: 0o644}}, Origin: "app:" + unit}); err != nil {
		return nil, err
	}
	if _, err := s.agent.Service(ctx, "", "daemon-reload"); err != nil {
		return nil, err
	}
	actions := []string{"enable", "restart"}
	if !a.Enabled {
		actions = []string{"disable", "stop"}
	}
	var last *agent.ServiceResponse
	for _, action := range actions {
		res, err := s.agent.Service(ctx, unit, action)
		if err != nil {
			s.db.SetAppStatus(context.WithoutCancel(ctx), a.ID, "failed", err.Error())
			return nil, fmt.Errorf("%s %s: %w", action, unit, err)
		}
		last = res
	}
	st := last.Status
	a.Status, a.LastError = st.ActiveState, ""
	s.db.SetAppStatus(context.WithoutCancel(ctx), a.ID, a.Status, "")
	return &st, nil
}

// removeApp stops, disables and deletes the unit.
func (s *Server) removeApp(ctx context.Context, a *store.App) error {
	unit := a.Unit()
	s.agent.Service(ctx, unit, "stop")    //nolint:errcheck // may not be running
	s.agent.Service(ctx, unit, "disable") //nolint:errcheck // may not be enabled
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{path.Join(unitDir, unit)}}); err != nil {
		return err
	}
	_, err := s.agent.Service(ctx, "", "daemon-reload")
	return err
}

func (s *Server) appStatus(ctx context.Context, a *store.App) *apitypes.AppStatus {
	out := &apitypes.AppStatus{App: a}
	if res, err := s.agent.Service(ctx, a.Unit(), "status"); err == nil {
		st := res.Status
		out.Service = &st
	}
	return out
}

func (s *Server) loadAppFor(ctx context.Context, login, name string) (*store.User, *store.App, error) {
	u, err := s.loadUserFor(ctx, login)
	if err != nil {
		return nil, nil, err
	}
	a, err := s.db.GetAppByName(ctx, u.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, huma.Error404NotFound("app not found")
	}
	if err != nil {
		return nil, nil, err
	}
	return u, a, nil
}

func (s *Server) registerApps() {
	huma.Register(s.api, huma.Operation{
		OperationID: "apps-list-all", Method: http.MethodGet, Path: "/apps", Summary: "All app services (admin)", Tags: []string{"apps"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*appsOutput, error) {
		list, err := s.db.ListApps(ctx, 0)
		if err != nil {
			return nil, err
		}
		out := make([]*apitypes.AppStatus, 0, len(list))
		for _, a := range list {
			out = append(out, s.appStatus(ctx, a))
		}
		return &appsOutput{Body: out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apps-list", Method: http.MethodGet, Path: "/users/{login}/apps", Summary: "App services of a user", Tags: []string{"apps"}, Security: secured,
	}, func(ctx context.Context, in *loginInputPath) (*appsOutput, error) {
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		list, err := s.db.ListApps(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		out := make([]*apitypes.AppStatus, 0, len(list))
		for _, a := range list {
			out = append(out, s.appStatus(ctx, a))
		}
		return &appsOutput{Body: out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apps-create", Method: http.MethodPost, Path: "/users/{login}/apps", Summary: "Create an app service (systemd unit running as the user) and start it", Tags: []string{"apps"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *appCreateInput) (*appOutput, error) {
		p := principalFrom(ctx)
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		if u.UnixUID == nil || u.Status != store.UserActive {
			return nil, huma.Error422UnprocessableEntity("owner must be an active user with a provisioned unix account")
		}
		b := in.Body
		a := &store.App{UserID: u.ID, Login: u.Login, Name: b.Name, Description: b.Description, Command: b.Command, WorkDir: b.WorkDir, EnvFile: b.EnvFile, Env: b.Env, Restart: b.Restart, Enabled: true}
		if b.Enabled != nil {
			a.Enabled = *b.Enabled
		}
		if err := s.validateApp(ctx, u, a); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := s.db.CreateApp(ctx, a); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("app already exists")
			}
			return nil, err
		}
		st, err := s.applyApp(ctx, a)
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "app.create", Target: u.Login + "/" + a.Name, IP: requestInfo(ctx).IP, Details: map[string]any{"command": a.Command}})
		return &appOutput{Status: http.StatusCreated, Body: &apitypes.AppStatus{App: a, Service: st}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apps-get", Method: http.MethodGet, Path: "/users/{login}/apps/{name}", Summary: "Get an app service with its live state", Tags: []string{"apps"}, Security: secured,
	}, func(ctx context.Context, in *appNameInput) (*appOutput, error) {
		_, a, err := s.loadAppFor(ctx, in.Login, in.Name)
		if err != nil {
			return nil, err
		}
		return &appOutput{Status: http.StatusOK, Body: s.appStatus(ctx, a)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apps-update", Method: http.MethodPatch, Path: "/users/{login}/apps/{name}", Summary: "Edit an app service and re-apply it", Tags: []string{"apps"}, Security: secured,
	}, func(ctx context.Context, in *appUpdateInput) (*appOutput, error) {
		p := principalFrom(ctx)
		u, a, err := s.loadAppFor(ctx, in.Login, in.Name)
		if err != nil {
			return nil, err
		}
		b := in.Body
		if b.Description != nil {
			a.Description = *b.Description
		}
		if b.Command != "" {
			a.Command = b.Command
		}
		if b.WorkDir != nil {
			a.WorkDir = *b.WorkDir
		}
		if b.EnvFile != nil {
			a.EnvFile = *b.EnvFile
		}
		if b.Env != nil {
			a.Env = *b.Env
		}
		if b.Restart != "" {
			a.Restart = b.Restart
		}
		if b.Enabled != nil {
			a.Enabled = *b.Enabled
		}
		if err := s.validateApp(ctx, u, a); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := s.db.UpdateApp(ctx, a); err != nil {
			return nil, err
		}
		st, err := s.applyApp(ctx, a)
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "app.update", Target: u.Login + "/" + a.Name, IP: requestInfo(ctx).IP})
		return &appOutput{Status: http.StatusOK, Body: &apitypes.AppStatus{App: a, Service: st}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apps-action", Method: http.MethodPost, Path: "/users/{login}/apps/{name}/{action}", Summary: "Start, stop or restart an app service", Tags: []string{"apps"}, Security: secured,
	}, func(ctx context.Context, in *appActionInput) (*appOutput, error) {
		p := principalFrom(ctx)
		u, a, err := s.loadAppFor(ctx, in.Login, in.Name)
		if err != nil {
			return nil, err
		}
		res, err := s.agent.Service(ctx, a.Unit(), in.Action)
		if err != nil {
			s.db.SetAppStatus(ctx, a.ID, "failed", err.Error())
			return nil, huma.Error502BadGateway(err.Error())
		}
		st := res.Status
		a.Status, a.LastError = st.ActiveState, ""
		s.db.SetAppStatus(ctx, a.ID, a.Status, "")
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "app." + in.Action, Target: u.Login + "/" + a.Name, IP: requestInfo(ctx).IP})
		return &appOutput{Status: http.StatusOK, Body: &apitypes.AppStatus{App: a, Service: &st}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apps-logs", Method: http.MethodGet, Path: "/users/{login}/apps/{name}/logs", Summary: "Journal of an app service", Tags: []string{"apps"}, Security: secured,
	}, func(ctx context.Context, in *appLogsInput) (*logsOutput, error) {
		_, a, err := s.loadAppFor(ctx, in.Login, in.Name)
		if err != nil {
			return nil, err
		}
		res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "journalctl", Args: []string{"-u", a.Unit(), "-n", strconv.Itoa(in.Lines), "--no-pager", "-o", "short-iso"}, TimeoutSeconds: 30})
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		return &logsOutput{Body: apitypes.LogTail{Path: "journal:" + a.Unit(), Lines: lastLines(res.Output, in.Lines)}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apps-delete", Method: http.MethodDelete, Path: "/users/{login}/apps/{name}", Summary: "Stop and remove an app service (files of the app stay)", Tags: []string{"apps"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *appNameInput) (*struct{}, error) {
		p := principalFrom(ctx)
		u, a, err := s.loadAppFor(ctx, in.Login, in.Name)
		if err != nil {
			return nil, err
		}
		if err := s.removeApp(ctx, a); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		if err := s.db.DeleteApp(ctx, a.ID); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "app.delete", Target: u.Login + "/" + a.Name, IP: requestInfo(ctx).IP})
		return nil, nil
	})
}
