package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

// Valkey instances of an account: one for the cache and one for PHP sessions,
// each a systemd unit running as the user and reachable only through its own
// unix socket (mode 600 in a directory of mode 700), so no account sees the
// keys of another. The server is a stack component, mp stack install valkey:
// Valkey where the distribution ships it, Redis where it does not; the
// distribution's own shared instance is kept off.
//
// Sockets live under /run/monopanel-valkey/<login>-<purpose>, snapshots under
// /var/lib/monopanel-valkey/<login>-<purpose>: systemd makes both for the
// unit, and on EL they carry the labels of the packaged server, which the
// stock policy already lets php-fpm (httpd_t) connect to. /var/lib/monopanel-valkey
// itself keeps var_lib_t: systemd (init_t) may not add entries to a
// redis_var_lib_t directory, so a labelled parent would fail every
// StateDirectory with EACCES — and without an AVC to tell why.

const (
	settingValkeyEngine = "valkey.engine" // valkey | redis: the package the install chose
	valkeyRunDir        = "/run/monopanel-valkey"
	valkeyStateDir      = "/var/lib/monopanel-valkey"
)

var valkeyDefaultMemory = map[string]int{store.ValkeyCache: 128, store.ValkeySessions: 64}

type valkeyPurposeInput struct {
	Login   string `path:"login" pattern:"^[a-z_][a-z0-9_-]{0,31}$"`
	Purpose string `path:"purpose" enum:"cache,sessions"`
}

type valkeyPutInput struct {
	Login   string `path:"login" pattern:"^[a-z_][a-z0-9_-]{0,31}$"`
	Purpose string `path:"purpose" enum:"cache,sessions"`
	Body    apitypes.ValkeyRequest
}

type valkeyListOutput struct {
	Body apitypes.ValkeyList
}

type valkeyOutput struct {
	Status int
	Body   *apitypes.ValkeyStatus
}

// valkeyLayout is the server the install chose, or nil when it has not run.
func (s *Server) valkeyLayout(ctx context.Context) *osprofile.ValkeyLayout {
	engine, _ := s.db.GetSetting(ctx, settingValkeyEngine)
	for _, l := range osprofile.ValkeyCandidates(s.profile) {
		if l.Engine == engine {
			return &l
		}
	}
	return nil
}

// valkeySocketFor is where the instance of an account for a purpose listens.
func valkeySocketFor(login, purpose string) string {
	return path.Join(valkeyRunDir, login+"-"+purpose, "valkey.sock")
}

func valkeySocket(v *store.ValkeyInstance) string { return valkeySocketFor(v.Login, v.Purpose) }

// checkSessionStore holds a site's PHP sessions to what can serve them: the
// Valkey store needs the account's sessions instance and the redis extension
// of the site's PHP branch, or every session_start() would fail.
func (s *Server) checkSessionStore(ctx context.Context, site *store.Site) error {
	if site.SessionStore != store.SessionStoreValkey {
		return nil
	}
	if site.Mode == store.ModeProxy {
		return errors.New("a proxy site has no PHP sessions")
	}
	if _, err := s.db.GetValkey(ctx, site.UserID, store.ValkeySessions); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s has no sessions instance: mp valkey add sessions --user %s", site.Login, site.Login)
	} else if err != nil {
		return err
	}
	if on, err := s.redisEnabled(ctx, site.PHPVersion); err != nil || on {
		return err
	}
	return fmt.Errorf("the redis extension of PHP %s is off: mp php ext enable %s redis", site.PHPVersion, site.PHPVersion)
}

// redisEnabled tells whether php-fpm of a branch loads the redis extension,
// which PHP needs to keep sessions in Valkey.
func (s *Server) redisEnabled(ctx context.Context, version string) (bool, error) {
	mods, err := s.phpModules(ctx, version)
	if err != nil {
		return false, err
	}
	for _, m := range mods {
		if m.Name == "redis" && m.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// valkeySessionSites names the sites of an account that keep PHP sessions in
// its sessions instance.
func (s *Server) valkeySessionSites(ctx context.Context, userID int64) ([]string, error) {
	sites, err := s.db.ListSites(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, site := range sites {
		if site.SessionStore == store.SessionStoreValkey {
			out = append(out, site.Domain)
		}
	}
	return out, nil
}

// applyValkey writes the unit, reloads systemd and (re)starts the instance.
func (s *Server) applyValkey(ctx context.Context, v *store.ValkeyInstance, l *osprofile.ValkeyLayout) (*systemd.Status, error) {
	unit := v.Unit()
	text, err := s.render.Render("systemd/valkey.service.tmpl", render.ValkeyUnit{
		Login: v.Login, Purpose: v.Purpose, Name: v.Name(), Engine: l.Engine, Binary: l.Binary,
		Socket: valkeySocket(v), DataDir: path.Join(valkeyStateDir, v.Name()),
		MemoryMB: v.MemoryMB, MemoryMax: 2*v.MemoryMB + 64, Sessions: v.Purpose == store.ValkeySessions,
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: path.Join(unitDir, unit), Content: text, Mode: 0o644}}, Origin: "valkey:" + unit}); err != nil {
		return nil, err
	}
	if _, err := s.agent.Service(ctx, "", "daemon-reload"); err != nil {
		return nil, err
	}
	var last *agent.ServiceResponse
	for _, action := range []string{"enable", "restart"} {
		res, err := s.agent.Service(ctx, unit, action)
		if err != nil {
			s.db.SetValkeyStatus(context.WithoutCancel(ctx), v.ID, "failed", err.Error())
			return nil, fmt.Errorf("%s %s: %w", action, unit, err)
		}
		last = res
	}
	st := last.Status
	v.Status, v.LastError = st.ActiveState, ""
	s.db.SetValkeyStatus(context.WithoutCancel(ctx), v.ID, v.Status, "")
	return &st, nil
}

// removeValkey stops the instance and deletes its unit and snapshots: the
// sessions instance keeps the data of logged-in visitors.
func (s *Server) removeValkey(ctx context.Context, v *store.ValkeyInstance) error {
	unit := v.Unit()
	s.agent.Service(ctx, unit, "stop")    //nolint:errcheck // may not be running
	s.agent.Service(ctx, unit, "disable") //nolint:errcheck // may not be enabled
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{path.Join(unitDir, unit), path.Join(valkeyStateDir, v.Name())}, Recursive: true}); err != nil {
		return err
	}
	_, err := s.agent.Service(ctx, "", "daemon-reload")
	return err
}

func (s *Server) valkeyStatus(ctx context.Context, v *store.ValkeyInstance) *apitypes.ValkeyStatus {
	out := &apitypes.ValkeyStatus{Instance: v, Socket: valkeySocket(v)}
	if res, err := s.agent.Service(ctx, v.Unit(), "status"); err == nil {
		st := res.Status
		out.Service = &st
	}
	return out
}

func (s *Server) valkeyList(ctx context.Context, userID int64) (*apitypes.ValkeyList, error) {
	list, err := s.db.ListValkey(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := &apitypes.ValkeyList{Instances: make([]*apitypes.ValkeyStatus, 0, len(list))}
	if l := s.valkeyLayout(ctx); l != nil {
		out.Engine = l.Engine
	}
	for _, v := range list {
		out.Instances = append(out.Instances, s.valkeyStatus(ctx, v))
	}
	return out, nil
}

// valkeyComponent is the server's row in the stack listing, among the tools.
func (s *Server) valkeyComponent(ctx context.Context) apitypes.StackComponent {
	c := apitypes.StackComponent{Name: "valkey", Kind: "tool", Removable: true}
	l := s.valkeyLayout(ctx)
	if l == nil {
		return c
	}
	if q, err := s.agent.Pkg(ctx, "query", l.Package); err == nil {
		if v, ok := q.Installed[l.Package]; ok {
			c.Installed, c.Version = true, v
			if l.Engine != "valkey" {
				c.Version = l.Engine + " " + v
			}
		}
	}
	return c
}

// installValkey puts Valkey on the host — or Redis where the repositories
// have no Valkey — and keeps the distribution's shared instance off.
func (s *Server) installValkey(ctx context.Context, jc *jobs.Context) error {
	cands := osprofile.ValkeyCandidates(s.profile)
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Package)
	}
	jc.Progress(10, "looking for valkey in the repositories")
	avail, err := s.agent.Pkg(ctx, "available", names...)
	if err != nil {
		return err
	}
	var l *osprofile.ValkeyLayout
	for _, c := range cands {
		if _, ok := avail.Available[c.Package]; ok {
			l = &c
			break
		}
	}
	if l == nil {
		return errors.New("neither valkey nor redis is in the repositories of this system")
	}
	if l.Engine != "valkey" {
		jc.Logf("valkey is not in the repositories of this system: installing %s, the same protocol and configuration", l.Package)
	}
	jc.Progress(20, "installing "+l.Package)
	res, err := s.agent.Pkg(ctx, "install", l.Package)
	if err != nil {
		return err
	}
	logTail(jc, res.Output, 2)
	// The packaged instance listens on 127.0.0.1:6379 without a password for
	// every account on the host: the panel runs one per account instead.
	s.agent.Service(ctx, l.Service, "stop")    //nolint:errcheck // Debian starts it on install, EL does not
	s.agent.Service(ctx, l.Service, "disable") //nolint:errcheck // same
	if s.profile.MAC() == "selinux" {
		jc.Progress(60, "SELinux labels for the sockets and snapshots")
		if _, err := s.agent.Pkg(ctx, "install", "policycoreutils-python-utils"); err != nil {
			return fmt.Errorf("install semanage: %w", err)
		}
		for _, fc := range [][2]string{{valkeyRunDir + "(/.*)?", "redis_var_run_t"}, {valkeyStateDir + "/[^/]+(/.*)?", "redis_var_lib_t"}} {
			if err := s.selinuxFileContext(ctx, fc[0], fc[1]); err != nil {
				return err
			}
		}
	}
	if err := s.db.SetSetting(ctx, settingValkeyEngine, l.Engine); err != nil {
		return err
	}
	// A reinstall may have switched the binary: the instances follow it.
	list, err := s.db.ListValkey(ctx, 0)
	if err != nil {
		return err
	}
	for _, v := range list {
		if _, err := s.applyValkey(ctx, v, l); err != nil {
			jc.Logf("instance %s: %v", v.Name(), err)
		}
	}
	jc.Logf("%s %s: instances per account — mp valkey add cache|sessions --user <login>", l.Engine, res.Installed[l.Package])
	jc.Progress(100, l.Engine+" ready")
	return nil
}

// removeValkeyServer uninstalls the server once no account uses it.
func (s *Server) removeValkeyServer(ctx context.Context, jc *jobs.Context) error {
	list, err := s.db.ListValkey(ctx, 0)
	if err != nil {
		return err
	}
	if len(list) > 0 {
		return fmt.Errorf("%d instances still run on it: remove them first (mp valkey rm)", len(list))
	}
	if l := s.valkeyLayout(ctx); l != nil {
		jc.Progress(50, "removing "+l.Package)
		if _, err := s.agent.Pkg(ctx, "remove", l.Package); err != nil {
			return err
		}
	}
	return s.db.SetSetting(ctx, settingValkeyEngine, "")
}

// removeUserValkey drops every instance of an account (user deletion).
func (s *Server) removeUserValkey(ctx context.Context, userID int64) (int, error) {
	list, err := s.db.ListValkey(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, v := range list {
		if err := s.removeValkey(ctx, v); err != nil {
			return 0, fmt.Errorf("valkey %s: %w", v.Name(), err)
		}
		s.db.DeleteValkey(ctx, v.ID) //nolint:errcheck // the unit is gone, that is what matters
	}
	return len(list), nil
}

func (s *Server) loadValkeyFor(ctx context.Context, login, purpose string) (*store.User, *store.ValkeyInstance, error) {
	u, err := s.loadUserFor(ctx, login)
	if err != nil {
		return nil, nil, err
	}
	v, err := s.db.GetValkey(ctx, u.ID, purpose)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, huma.Error404NotFound("no " + purpose + " instance")
	}
	if err != nil {
		return nil, nil, err
	}
	return u, v, nil
}

func (s *Server) registerValkey() {
	huma.Register(s.api, huma.Operation{
		OperationID: "valkey-list-all", Method: http.MethodGet, Path: "/valkey", Summary: "All Valkey instances (admin)", Tags: []string{"valkey"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*valkeyListOutput, error) {
		out, err := s.valkeyList(ctx, 0)
		if err != nil {
			return nil, err
		}
		return &valkeyListOutput{Body: *out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "valkey-list", Method: http.MethodGet, Path: "/users/{login}/valkey", Summary: "Valkey instances of a user: cache and sessions", Tags: []string{"valkey"}, Security: secured,
	}, func(ctx context.Context, in *loginInputPath) (*valkeyListOutput, error) {
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		out, err := s.valkeyList(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		return &valkeyListOutput{Body: *out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "valkey-put", Method: http.MethodPut, Path: "/users/{login}/valkey/{purpose}", Summary: "Create the cache or sessions instance of a user, or change its memory, and (re)start it", Tags: []string{"valkey"}, Security: secured,
	}, func(ctx context.Context, in *valkeyPutInput) (*valkeyOutput, error) {
		p := principalFrom(ctx)
		l := s.valkeyLayout(ctx)
		if l == nil {
			return nil, huma.Error422UnprocessableEntity("valkey is not installed: mp stack install valkey")
		}
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		if u.UnixUID == nil || u.Status != store.UserActive {
			return nil, huma.Error422UnprocessableEntity("owner must be an active user with a provisioned unix account")
		}
		v, err := s.db.GetValkey(ctx, u.ID, in.Purpose)
		created := errors.Is(err, store.ErrNotFound)
		switch {
		case created:
			v = &store.ValkeyInstance{UserID: u.ID, Login: u.Login, Purpose: in.Purpose, MemoryMB: valkeyDefaultMemory[in.Purpose]}
		case err != nil:
			return nil, err
		}
		if in.Body.MemoryMB > 0 {
			v.MemoryMB = in.Body.MemoryMB
		}
		if created {
			err = s.db.CreateValkey(ctx, v)
		} else {
			err = s.db.UpdateValkey(ctx, v)
		}
		if err != nil {
			return nil, err
		}
		st, err := s.applyValkey(ctx, v, l)
		if err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		action, status := "valkey.update", http.StatusOK
		if created {
			action, status = "valkey.create", http.StatusCreated
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: action, Target: v.Name(), IP: requestInfo(ctx).IP, Details: map[string]any{"memory_mb": v.MemoryMB}})
		return &valkeyOutput{Status: status, Body: &apitypes.ValkeyStatus{Instance: v, Service: st, Socket: valkeySocket(v)}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "valkey-restart", Method: http.MethodPost, Path: "/users/{login}/valkey/{purpose}/restart", Summary: "Restart an instance; the cache comes back empty", Tags: []string{"valkey"}, Security: secured,
	}, func(ctx context.Context, in *valkeyPurposeInput) (*valkeyOutput, error) {
		p := principalFrom(ctx)
		_, v, err := s.loadValkeyFor(ctx, in.Login, in.Purpose)
		if err != nil {
			return nil, err
		}
		res, err := s.agent.Service(ctx, v.Unit(), "restart")
		if err != nil {
			s.db.SetValkeyStatus(ctx, v.ID, "failed", err.Error())
			return nil, huma.Error502BadGateway(err.Error())
		}
		st := res.Status
		v.Status, v.LastError = st.ActiveState, ""
		s.db.SetValkeyStatus(ctx, v.ID, v.Status, "")
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "valkey.restart", Target: v.Name(), IP: requestInfo(ctx).IP})
		return &valkeyOutput{Status: http.StatusOK, Body: &apitypes.ValkeyStatus{Instance: v, Service: &st, Socket: valkeySocket(v)}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "valkey-delete", Method: http.MethodDelete, Path: "/users/{login}/valkey/{purpose}", Summary: "Stop and remove an instance with its data", Tags: []string{"valkey"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *valkeyPurposeInput) (*struct{}, error) {
		p := principalFrom(ctx)
		_, v, err := s.loadValkeyFor(ctx, in.Login, in.Purpose)
		if err != nil {
			return nil, err
		}
		if v.Purpose == store.ValkeySessions {
			sites, err := s.valkeySessionSites(ctx, v.UserID)
			if err != nil {
				return nil, err
			}
			if len(sites) > 0 {
				return nil, huma.Error409Conflict(fmt.Sprintf("PHP sessions of %s live here: switch them to files first (mp site set <domain> --sessions files)", strings.Join(sites, ", ")))
			}
		}
		if err := s.removeValkey(ctx, v); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		if err := s.db.DeleteValkey(ctx, v.ID); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "valkey.delete", Target: v.Name(), IP: requestInfo(ctx).IP})
		return nil, nil
	})
}
