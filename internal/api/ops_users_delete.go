package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

type userDeleteInput struct {
	Login string `path:"login" pattern:"^[a-z_][a-z0-9_-]{0,31}$"`
	Purge bool   `query:"purge" doc:"Also delete the home directory with every site and file"`
}

type userDeletePayload struct {
	UserID int64  `json:"user_id"`
	Login  string `json:"login"`
	Purge  bool   `json:"purge"`
}

func (s *Server) registerUserDelete() {
	huma.Register(s.api, huma.Operation{
		OperationID: "users-delete", Method: http.MethodDelete, Path: "/users/{login}", Summary: "Delete a user (async): sites, databases, cron, app services, certificates and the unix account; purge=true removes the home too",
		Tags: []string{"users"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *userDeleteInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		u, err := s.db.GetUserByLogin(ctx, in.Login)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("user not found")
		}
		if err != nil {
			return nil, err
		}
		if u.Login == p.Login || (p.UserID != 0 && u.ID == p.UserID) {
			return nil, huma.Error422UnprocessableEntity("you cannot delete your own account")
		}
		if u.Role == store.RoleAdmin {
			counts, _ := s.db.CountUsers(ctx)
			if counts[store.RoleAdmin] <= 1 {
				return nil, huma.Error422UnprocessableEntity("the last administrator cannot be deleted")
			}
		}
		if err := s.db.SetUserStatus(ctx, u.ID, store.UserDeleting); err != nil {
			return nil, err
		}
		job, err := s.jobs.Enqueue(ctx, "user.delete", userDeletePayload{UserID: u.ID, Login: u.Login, Purge: in.Purge}, jobs.WithLockKey("user:"+u.Login), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "user.delete", Target: u.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"purge": in.Purge}})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

// jobUserDelete tears down everything a user owns, then the account itself.
func (s *Server) jobUserDelete(ctx context.Context, jc *jobs.Context) error {
	var p userDeletePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	u, err := s.db.GetUserByID(ctx, p.UserID)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = s.db.SetUserStatus(context.WithoutCancel(ctx), u.ID, store.UserSuspended)
		return err
	}

	jc.Progress(10, "sites")
	sites, err := s.db.ListSites(ctx, u.ID)
	if err != nil {
		return fail(err)
	}
	for _, site := range sites {
		if err := s.removeSiteConfig(ctx, jc, site, u, false); err != nil {
			return fail(fmt.Errorf("site %s: %w", site.Domain, err))
		}
		for _, name := range append([]string{site.Domain}, site.Aliases...) {
			c, err := s.db.GetCertificateByName(ctx, name)
			if err != nil || c.Name == s.cfg.Web.Hostname {
				continue
			}
			if err := s.acme.Remove(c.Name); err != nil {
				jc.Logf("certificate %s: files not removed: %v", c.Name, err)
			}
			if err := s.db.DeleteCertificate(ctx, c.ID); err == nil {
				jc.Logf("certificate %s removed", c.Name)
			}
		}
		if err := s.db.DeleteSite(ctx, site.ID); err != nil {
			return fail(err)
		}
	}

	jc.Progress(35, "databases")
	dbs, err := s.db.ListDatabases(ctx, u.ID)
	if err != nil {
		return fail(err)
	}
	for _, db := range dbs {
		sql := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;\n", db.Name)
		for _, a := range db.Users {
			sql += fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s';\n", a.Name, a.Host)
		}
		sql += "FLUSH PRIVILEGES;\n"
		if _, err := s.mysqlExec(ctx, sql); err != nil {
			return fail(fmt.Errorf("database %s: %w", db.Name, err))
		}
		if err := s.db.DeleteDatabase(ctx, db.ID); err != nil {
			return fail(err)
		}
		jc.Logf("database %s dropped", db.Name)
	}

	jc.Progress(50, "mail")
	// Домены каскадно уйдут вместе с аккаунтом, но письма на диске и карты
	// postfix сами не исчезнут — их надо убрать до удаления строк.
	mailDomains, err := s.db.ListMailDomains(ctx, u.ID)
	if err != nil {
		return fail(err)
	}
	for _, d := range mailDomains {
		if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{mailBase + "/" + d.Name, dkimDir + "/keys/" + d.Name}, Recursive: true}); err != nil {
			jc.Logf("почтовый домен %s: файлы не удалены: %v", d.Name, err)
		}
		if err := s.db.DeleteMailDomain(ctx, d.ID); err != nil {
			return fail(err)
		}
		jc.Logf("почтовый домен %s удалён (%d ящиков)", d.Name, d.Mailboxes)
	}
	if len(mailDomains) > 0 {
		if err := s.applyMail(ctx, jc.Logf); err != nil {
			jc.Logf("предупреждение: конфигурация почты не перегенерирована: %v", err)
		}
	}

	jc.Progress(55, "app services and cron")
	apps, err := s.db.ListApps(ctx, u.ID)
	if err != nil {
		return fail(err)
	}
	for _, a := range apps {
		if err := s.removeApp(ctx, a); err != nil {
			return fail(fmt.Errorf("app %s: %w", a.Name, err))
		}
		s.db.DeleteApp(ctx, a.ID) //nolint:errcheck
		jc.Logf("app service %s removed", a.Unit())
	}
	if u.UnixUID != nil {
		if res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "crontab", Args: []string{"-r", "-u", u.Login}, TimeoutSeconds: 30}); err == nil && res.ExitCode == 0 {
			jc.Logf("crontab removed")
		}
	}

	jc.Progress(75, "unix account")
	if u.UnixUID != nil {
		home := u.Home
		if home == "" {
			home = s.cfg.WWWRoot + "/" + u.Login
		}
		res, err := s.agent.RemoveUnixUser(ctx, &agent.RemoveUnixUserRequest{Login: u.Login, RemoveHome: p.Purge, HomeUnder: s.cfg.WWWRoot, Home: home})
		if err != nil {
			return fail(err)
		}
		switch {
		case !res.Removed:
			jc.Logf("unix user %s was already absent", u.Login)
		case res.HomeRemoved:
			jc.Logf("unix user %s removed with %s (%d processes stopped)", u.Login, home, res.Killed)
		default:
			jc.Logf("unix user %s removed; files kept in %s (%d processes stopped)", u.Login, home, res.Killed)
		}
	}

	jc.Progress(95, "panel account")
	if err := s.db.DeleteUser(context.WithoutCancel(ctx), u.ID); err != nil {
		return fail(err)
	}
	jc.Progress(100, fmt.Sprintf("user %s deleted (%d sites, %d databases, %d apps)", u.Login, len(sites), len(dbs), len(apps)))
	return nil
}
