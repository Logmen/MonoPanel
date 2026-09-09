package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

var adminOnly = map[string]any{"admin": true}

type usersOutput struct {
	Body []*store.User
}

type userOutput struct {
	Body *store.User
}

type loginInputPath struct {
	Login string `path:"login" pattern:"^[a-z_][a-z0-9_-]{0,31}$"`
}

type createUserInput struct {
	Body apitypes.CreateUserRequest
}

type createUserOutput struct {
	Status int
	Body   apitypes.UserWithJob
}

func (s *Server) registerUsers() {
	huma.Register(s.api, huma.Operation{
		OperationID: "users-list", Method: http.MethodGet, Path: "/users", Summary: "List users", Tags: []string{"users"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*usersOutput, error) {
		users, err := s.db.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		if users == nil {
			users = []*store.User{}
		}
		return &usersOutput{Body: users}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "users-get", Method: http.MethodGet, Path: "/users/{login}", Summary: "Get a user", Tags: []string{"users"}, Security: secured,
	}, func(ctx context.Context, in *loginInputPath) (*userOutput, error) {
		p := principalFrom(ctx)
		if p.Role != store.RoleAdmin && p.Login != in.Login {
			return nil, huma.Error403Forbidden("not your account")
		}
		u, err := s.db.GetUserByLogin(ctx, in.Login)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("user not found")
		}
		if err != nil {
			return nil, err
		}
		return &userOutput{Body: u}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "users-create", Method: http.MethodPost, Path: "/users", Summary: "Create a user (async: unix user and home are provisioned by a job)", Tags: []string{"users"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *createUserInput) (*createUserOutput, error) {
		p := principalFrom(ctx)
		u := &store.User{Login: in.Body.Login, Role: in.Body.Role, Email: in.Body.Email, Shell: in.Body.Shell, QuotaMB: in.Body.QuotaMB, Status: store.UserPending}
		if u.Role == "" {
			u.Role = store.RoleUser
		}
		if in.Body.Password != "" {
			h, err := auth.HashPassword(in.Body.Password)
			if err != nil {
				return nil, err
			}
			u.PasswordHash = h
		}
		if err := s.db.CreateUser(ctx, u); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("user already exists")
			}
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "user.create", Target: u.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"role": u.Role}})
		payload := userProvisionPayload{UserID: u.ID, Login: u.Login, Shell: u.Shell}
		if in.Body.Password != "" && s.secrets != nil {
			payload.PasswordEnc, _ = s.secrets.Encrypt(in.Body.Password)
		}
		job, err := s.jobs.Enqueue(ctx, "user.provision", payload, jobs.WithLockKey("user:"+u.Login), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		return &createUserOutput{Status: http.StatusAccepted, Body: apitypes.UserWithJob{User: u, JobID: job.ID}}, nil
	})
}

type updateUserInput struct {
	Login string `path:"login" pattern:"^[a-z_][a-z0-9_-]{0,31}$"`
	Body  apitypes.UserUpdateRequest
}

func (s *Server) registerUserUpdate() {
	huma.Register(s.api, huma.Operation{
		OperationID: "users-update", Method: http.MethodPatch, Path: "/users/{login}", Summary: "Change email, password, shell/SFTP mode or status", Tags: []string{"users"}, Security: secured, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *updateUserInput) (*createUserOutput, error) {
		p := principalFrom(ctx)
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		b := in.Body
		if (b.Shell != nil || b.Status != "") && p.Role != store.RoleAdmin {
			return nil, huma.Error403Forbidden("administrator role required")
		}
		if b.Email != "" {
			u.Email = b.Email
		}
		if b.Password != "" {
			h, err := auth.HashPassword(b.Password)
			if err != nil {
				return nil, err
			}
			if err := s.db.SetUserPassword(ctx, u.ID, h); err != nil {
				return nil, err
			}
			if u.UnixUID != nil {
				if err := s.agent.SetUnixPassword(ctx, u.Login, b.Password); err != nil {
					return nil, huma.Error502BadGateway(err.Error())
				}
			}
		}
		if b.Status != "" {
			u.Status = b.Status
		}
		reprovision := false
		if b.Shell != nil && *b.Shell != u.Shell {
			u.Shell = *b.Shell
			reprovision = true
		}
		if err := s.db.UpdateUserProfile(ctx, u); err != nil {
			return nil, err
		}
		out := &createUserOutput{Status: http.StatusAccepted, Body: apitypes.UserWithJob{User: u}}
		if reprovision && u.Role == store.RoleUser {
			job, err := s.jobs.Enqueue(ctx, "user.provision", userProvisionPayload{UserID: u.ID, Login: u.Login, Shell: u.Shell}, jobs.WithLockKey("user:"+u.Login), jobs.WithRequestedBy(p.Login))
			if err != nil {
				return nil, err
			}
			out.Body.JobID = job.ID
		} else {
			out.Status = http.StatusOK
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "user.update", Target: u.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"shell": u.Shell, "status": u.Status, "password": b.Password != ""}})
		return out, nil
	})
}

type userProvisionPayload struct {
	UserID      int64  `json:"user_id"`
	Login       string `json:"login"`
	Shell       bool   `json:"shell"`
	PasswordEnc string `json:"password_enc,omitempty"`
}

const sftpGroup = "monopanel-sftp"

const sshdDropIn = `# Generated by MonoPanel: SFTP-only clients are chrooted into their home.
Match Group monopanel-sftp
    ChrootDirectory %h
    ForceCommand internal-sftp -u 0027
    PasswordAuthentication yes
    AllowTcpForwarding no
    X11Forwarding no
    PermitTunnel no
`

// ensureSSHConfig installs the sshd drop-in once (validated with sshd -t).
func (s *Server) ensureSSHConfig(ctx context.Context) error {
	_, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:    []agent.FileSpec{{Path: "/etc/ssh/sshd_config.d/monopanel.conf", Content: sshdDropIn, Mode: 0o644}},
		Validate: [][]string{{"/usr/sbin/sshd", "-t"}},
		Reload:   []string{s.profile.SSHService()},
		Origin:   "ssh",
	})
	return err
}

// jobUserProvision creates the unix user and the FASTPANEL-style home layout.
func (s *Server) jobUserProvision(ctx context.Context, jc *jobs.Context) error {
	var p userProvisionPayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	return s.provisionUser(ctx, p, jc.Logf, jc.Progress)
}

// provisionUser creates the unix side of an account: groups, user, home
// layout, SFTP chroot. Переезд между панелями делает ровно то же самое, только
// без своей задачи, поэтому тело живёт отдельно от обработчика.
func (s *Server) provisionUser(ctx context.Context, p userProvisionPayload, logf func(string, ...any), progress func(int, string)) error {
	progress(10, "groups")
	for _, g := range []string{s.cfg.WebGroup, sftpGroup} {
		if _, err := s.agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: g, System: true}); err != nil {
			return err
		}
	}
	home := filepath.Join(s.cfg.WWWRoot, p.Login)
	shell := s.profile.NologinShell()
	groups, remove := []string{sftpGroup}, []string{}
	if p.Shell {
		shell = "/bin/bash"
		groups, remove = nil, []string{sftpGroup}
	}
	progress(30, "unix user")
	u, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: p.Login, Home: home, Shell: shell, UpdateShell: true, CreateHome: true, Groups: groups, RemoveGroups: remove, Comment: "MonoPanel user"})
	if err != nil {
		return err
	}
	logf("unix user %s: uid=%d gid=%d created=%v shell=%s home=%s", p.Login, u.UID, u.GID, u.Created, shell, u.Home)
	if p.PasswordEnc != "" && s.secrets != nil {
		if pw, err := s.secrets.Decrypt(p.PasswordEnc); err == nil {
			if err := s.agent.SetUnixPassword(ctx, p.Login, pw); err != nil {
				return err
			}
			logf("SFTP/SSH password set")
		}
	}
	if err := s.ensureSSHConfig(ctx); err != nil {
		return fmt.Errorf("sshd configuration: %w", err)
	}
	progress(60, "home layout")
	data := filepath.Join(home, "data")
	homeOwner, homeMode := p.Login, uint32(0o710)
	if !p.Shell {
		// ChrootDirectory must be root-owned and not group-writable; 0750 lets
		// the client list the chroot root, data/ below stays theirs.
		homeOwner, homeMode = "root", 0o750
	}
	dirs := []agent.DirSpec{
		{Path: s.cfg.WWWRoot, Mode: 0o711, Owner: "root", Group: "root"},
		{Path: home, Mode: homeMode, Owner: homeOwner, Group: p.Login},
		{Path: data, Mode: 0o750, Owner: p.Login, Group: p.Login},
		{Path: filepath.Join(data, "www"), Mode: 0o750, Owner: p.Login, Group: p.Login},
		{Path: filepath.Join(data, "logs"), Mode: 0o750, Owner: p.Login, Group: p.Login},
		{Path: filepath.Join(data, "tmp"), Mode: 0o700, Owner: p.Login, Group: p.Login},
		{Path: filepath.Join(data, "tmp", "sess"), Mode: 0o700, Owner: p.Login, Group: p.Login},
		{Path: filepath.Join(data, "bin"), Mode: 0o750, Owner: p.Login, Group: p.Login},
	}
	res, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: dirs})
	if err != nil {
		return err
	}
	logf("directories created: %d; mode: %s", len(res.Created), map[bool]string{true: "SSH shell", false: "SFTP-only (chroot " + home + ")"}[p.Shell])
	progress(90, "saving")
	if err := s.db.SetUserUnix(ctx, p.UserID, u.UID, u.GID, u.Home); err != nil {
		return err
	}
	if err := s.db.SetUserStatus(ctx, p.UserID, store.UserActive); err != nil {
		return err
	}
	progress(100, "user ready")
	return nil
}
