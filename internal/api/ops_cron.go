package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/robfig/cron/v3"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

type cronListOutput struct {
	Body []*store.CronJob
}

type cronCreateInput struct {
	Login string `path:"login"`
	Body  apitypes.CronRequest
}

type cronJobOutput struct {
	Status int
	Body   *store.CronJob
}

type cronIDInput struct {
	Login string `path:"login"`
	ID    int64  `path:"id" minimum:"1"`
}

type cronUpdateInput struct {
	Login string `path:"login"`
	ID    int64  `path:"id" minimum:"1"`
	Body  apitypes.CronUpdateRequest
}

func (s *Server) loadUserFor(ctx context.Context, login string) (*store.User, error) {
	p := principalFrom(ctx)
	if p.Role != store.RoleAdmin && p.Login != login {
		return nil, huma.Error404NotFound("user not found")
	}
	u, err := s.db.GetUserByLogin(ctx, login)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("user not found")
	}
	return u, err
}

func validateCron(schedule, command string) error {
	if _, err := cronParser.Parse(schedule); err != nil {
		return fmt.Errorf("schedule: %v", err)
	}
	if command == "" || len(command) > 1000 || strings.ContainsAny(command, "\n\r") {
		return errors.New("command must be one line up to 1000 characters")
	}
	return nil
}

// applyCrontab renders and installs the whole crontab of a user.
func (s *Server) applyCrontab(ctx context.Context, u *store.User) error {
	if err := s.ensurePackages(ctx, nil, s.profile.CronPackage()); err != nil {
		return err
	}
	jobs, err := s.db.ListCronJobs(ctx, u.ID)
	if err != nil {
		return err
	}
	home := u.Home
	if home == "" {
		home = path.Join(s.cfg.WWWRoot, u.Login)
	}
	data := render.Crontab{Login: u.Login, BinDir: path.Join(home, "data", "bin")}
	for _, j := range jobs {
		data.Jobs = append(data.Jobs, render.CronLine{Schedule: j.Schedule, Command: j.Command, Enabled: j.Enabled, Comment: j.Comment})
	}
	text, err := s.render.Render("cron/crontab.tmpl", data)
	if err != nil {
		return err
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "crontab", Args: []string{"-u", u.Login, "-"}, Stdin: text, TimeoutSeconds: 30})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("crontab: %s", strings.TrimSpace(res.Output))
	}
	return nil
}

func (s *Server) registerCron() {
	huma.Register(s.api, huma.Operation{
		OperationID: "cron-list", Method: http.MethodGet, Path: "/users/{login}/cron", Summary: "Cron jobs of a user", Tags: []string{"cron"}, Security: secured,
	}, func(ctx context.Context, in *loginInputPath) (*cronListOutput, error) {
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		list, err := s.db.ListCronJobs(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		return &cronListOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "cron-create", Method: http.MethodPost, Path: "/users/{login}/cron", Summary: "Add a cron job (crontab is rewritten immediately)", Tags: []string{"cron"}, Security: secured, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *cronCreateInput) (*cronJobOutput, error) {
		p := principalFrom(ctx)
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		if u.UnixUID == nil {
			return nil, huma.Error422UnprocessableEntity("user has no unix account")
		}
		if err := validateCron(in.Body.Schedule, in.Body.Command); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		job := &store.CronJob{UserID: u.ID, Schedule: in.Body.Schedule, Command: in.Body.Command, Comment: in.Body.Comment, Enabled: true}
		if in.Body.Enabled != nil {
			job.Enabled = *in.Body.Enabled
		}
		if err := s.db.CreateCronJob(ctx, job); err != nil {
			return nil, err
		}
		if err := s.applyCrontab(ctx, u); err != nil {
			s.db.DeleteCronJob(ctx, job.ID) //nolint:errcheck // rollback of a job that could not be installed
			return nil, huma.Error502BadGateway(err.Error())
		}
		job.Login = u.Login
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cron.create", Target: u.Login, IP: requestInfo(ctx).IP})
		return &cronJobOutput{Status: http.StatusCreated, Body: job}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "cron-update", Method: http.MethodPatch, Path: "/users/{login}/cron/{id}", Summary: "Edit, enable or disable a cron job", Tags: []string{"cron"}, Security: secured,
	}, func(ctx context.Context, in *cronUpdateInput) (*cronJobOutput, error) {
		p := principalFrom(ctx)
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		job, err := s.db.GetCronJob(ctx, in.ID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && job.UserID != u.ID) {
			return nil, huma.Error404NotFound("cron job not found")
		}
		if err != nil {
			return nil, err
		}
		if in.Body.Schedule != "" {
			job.Schedule = in.Body.Schedule
		}
		if in.Body.Command != "" {
			job.Command = in.Body.Command
		}
		if in.Body.Comment != nil {
			job.Comment = *in.Body.Comment
		}
		if in.Body.Enabled != nil {
			job.Enabled = *in.Body.Enabled
		}
		if err := validateCron(job.Schedule, job.Command); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := s.db.UpdateCronJob(ctx, job); err != nil {
			return nil, err
		}
		if err := s.applyCrontab(ctx, u); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cron.update", Target: u.Login, IP: requestInfo(ctx).IP})
		return &cronJobOutput{Status: http.StatusOK, Body: job}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "cron-delete", Method: http.MethodDelete, Path: "/users/{login}/cron/{id}", Summary: "Remove a cron job", Tags: []string{"cron"}, Security: secured, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *cronIDInput) (*struct{}, error) {
		p := principalFrom(ctx)
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		job, err := s.db.GetCronJob(ctx, in.ID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && job.UserID != u.ID) {
			return nil, huma.Error404NotFound("cron job not found")
		}
		if err != nil {
			return nil, err
		}
		if err := s.db.DeleteCronJob(ctx, job.ID); err != nil {
			return nil, err
		}
		if err := s.applyCrontab(ctx, u); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cron.delete", Target: u.Login, IP: requestInfo(ctx).IP})
		return nil, nil
	})
}
