package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

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

// A job is one line of the account's crontab, which the panel writes in full
// (crontab -u <login> -): "<schedule> <command>  # <comment>". What the API
// stores has to be what cron accepts — the install fails after the job is
// saved otherwise — and no field may start a line of its own.
const (
	maxCronSchedule = 64
	maxCronCommand  = 1000
	maxCronComment  = 120
)

// cronNicknames are the @-schedules of both crons the panel installs
// (Debian's cron and cronie). @every comes from Go's cron libraries: crontab
// refuses it.
var cronNicknames = map[string]bool{
	"@reboot": true, "@yearly": true, "@annually": true, "@monthly": true,
	"@weekly": true, "@daily": true, "@midnight": true, "@hourly": true,
}

// cronField is one field of a five-field schedule: its range and the
// three-letter names cron takes for its values, counted from min.
type cronField struct {
	name     string
	min, max int
	names    []string
}

var cronFields = [...]cronField{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	// 0 and 7 are both Sunday.
	{name: "day of week", min: 0, max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// validateCronJob checks a job the way the panel writes it: the schedule as
// cron reads it, the command and the comment as one line each. The API's
// create and update paths use it, and so does the migration import for the
// jobs another server hands over.
func validateCronJob(schedule, command, comment string) error {
	if err := validateCron(schedule, command); err != nil {
		return err
	}
	if utf8.RuneCountInString(comment) > maxCronComment {
		return fmt.Errorf("comment: at most %d characters", maxCronComment)
	}
	if !cronOneLine(comment) {
		return errors.New("comment must be one line, without control characters")
	}
	return nil
}

// validateCron checks the schedule and the command of a job.
func validateCron(schedule, command string) error {
	if err := validateCronSchedule(schedule); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	if strings.TrimSpace(command) == "" || utf8.RuneCountInString(command) > maxCronCommand || !cronOneLine(command) {
		return fmt.Errorf("command must be one line up to %d characters, without control characters", maxCronCommand)
	}
	return nil
}

// cronOneLine refuses what would end the crontab line early or garble it: a
// line break starts a line of the account's choosing (with #DISABLED# in
// front of a job, the part after it would even run), and the other control
// characters have no business in a crontab — nor in the terminal of whoever
// lists the jobs. Tabs are blanks to cron and stay.
func cronOneLine(v string) bool {
	for _, r := range v {
		if r != '\t' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validateCronSchedule accepts five fields — numbers, the names of months
// and days, *, ranges, lists and steps after * or a range — or one of the
// nicknames, as both crons the panel installs read them. Go's cron libraries
// take more (@every, ?, a step after a single number, TZ= in front), which
// crontab refuses or does not read the same everywhere — cronie refuses
// 5/10, Debian's cron does not take it for 5-59/10. Such a schedule is
// refused with what to write instead.
func validateCronSchedule(schedule string) error {
	if utf8.RuneCountInString(schedule) > maxCronSchedule {
		return fmt.Errorf("at most %d characters", maxCronSchedule)
	}
	fields := strings.FieldsFunc(schedule, func(r rune) bool { return r == ' ' || r == '\t' })
	for _, f := range fields {
		for _, r := range f {
			if r <= ' ' || r > '~' {
				return fmt.Errorf("%q: cron reads Latin letters, digits and * , - / only", f)
			}
		}
	}
	const how = "five fields (minute hour day-of-month month day-of-week, e.g. */5 * * * *) or @reboot, @hourly, @daily, @weekly, @monthly, @yearly"
	switch {
	case len(fields) == 0:
		return errors.New("empty; give " + how)
	case fields[0] == "@every":
		return errors.New("@every is not cron syntax; write five fields instead, e.g. */10 * * * * for every 10 minutes")
	case strings.HasPrefix(fields[0], "@"):
		if len(fields) == 1 && cronNicknames[fields[0]] {
			return nil
		}
		return fmt.Errorf("%q is not a schedule cron knows; give %s", schedule, how)
	case len(fields) != len(cronFields):
		return fmt.Errorf("%d fields; give %s", len(fields), how)
	}
	for i, f := range fields {
		if err := cronFields[i].check(f); err != nil {
			return err
		}
	}
	return nil
}

func (c cronField) check(expr string) error {
	for _, item := range strings.Split(expr, ",") {
		rng, step, stepped := strings.Cut(item, "/")
		if stepped {
			if n, ok := cronNumber(step); !ok || n < 1 || n > c.max-c.min+1 {
				return fmt.Errorf("%s: the step in %q must be a number from 1 to %d", c.name, item, c.max-c.min+1)
			}
		}
		if rng == "*" {
			continue
		}
		lo, hi, ranged := strings.Cut(rng, "-")
		from, err := c.value(lo)
		if err != nil {
			return err
		}
		if !ranged {
			if stepped {
				return fmt.Errorf("%s: %q — cron takes a step only after * or a range: */%s, or %s-%d/%s from %s on", c.name, item, step, lo, c.max, step, lo)
			}
			continue
		}
		to, err := c.value(hi)
		if err != nil {
			return err
		}
		if from > to {
			return fmt.Errorf("%s: the range %q runs backwards", c.name, rng)
		}
	}
	return nil
}

// value reads one number or name of the field.
func (c cronField) value(s string) (int, error) {
	if n, ok := cronNumber(s); ok && n >= c.min && n <= c.max {
		return n, nil
	}
	for i, name := range c.names {
		if strings.EqualFold(s, name) {
			return c.min + i, nil
		}
	}
	if c.names != nil {
		return 0, fmt.Errorf("%s: %q is not a number from %d to %d or a name such as %s", c.name, s, c.min, c.max, c.names[1])
	}
	return 0, fmt.Errorf("%s: %q is not a number from %d to %d", c.name, s, c.min, c.max)
}

// cronNumber reads the digits cron takes for a number; four are plenty for
// any field and keep the value from overflowing.
func cronNumber(s string) (int, bool) {
	if s == "" || len(s) > 4 {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// crontabComment is the comment as it goes into the crontab, after the
// command: cron cuts the command at an unescaped % and hands the rest to it
// as standard input, so a % in the comment ("runs at 50% load") would change
// what the job reads. Escaped as \%, it stays in the shell comment, where it
// belongs; what is escaped already stays as it is.
func crontabComment(comment string) string {
	if !strings.Contains(comment, "%") {
		return comment
	}
	var b strings.Builder
	escaped := false
	for _, r := range comment {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '%':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// applyCrontab renders and installs the whole crontab of a user.
func (s *Server) applyCrontab(ctx context.Context, u *store.User) error {
	jobs, err := s.db.ListCronJobs(ctx, u.ID)
	if err != nil {
		return err
	}
	return s.installCrontab(ctx, u, jobs)
}

// installCrontab installs these jobs as the user's whole crontab.
func (s *Server) installCrontab(ctx context.Context, u *store.User, jobs []*store.CronJob) error {
	if err := s.ensurePackages(ctx, nil, s.profile.CronPackage()); err != nil {
		return err
	}
	home := u.Home
	if home == "" {
		home = path.Join(s.cfg.WWWRoot, u.Login)
	}
	data := render.Crontab{Login: u.Login, BinDir: path.Join(home, "data", "bin")}
	// The account's login and home are in the crontab's header lines.
	if !cronOneLine(data.Login) || !cronOneLine(data.BinDir) {
		return fmt.Errorf("account %q: the login or the home has a line break or another control character, so its crontab is not installed", u.Login)
	}
	for _, j := range jobs {
		// The API checks what it stores, but a job may have come another way
		// (an import, an older version that let a line break through): it
		// must still not add a line to the crontab. The whole install stops,
		// naming the job, until somebody fixes or deletes it.
		for _, f := range [][2]string{{"schedule", j.Schedule}, {"command", j.Command}, {"comment", j.Comment}} {
			if !cronOneLine(f[1]) {
				return fmt.Errorf("cron job %d of %s: the %s has a line break or another control character; fix or delete the job (nothing was installed)", j.ID, u.Login, f[0])
			}
		}
		data.Jobs = append(data.Jobs, render.CronLine{Schedule: j.Schedule, Command: j.Command, Enabled: j.Enabled, Comment: crontabComment(j.Comment)})
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
		OperationID: "cron-list-all", Method: http.MethodGet, Path: "/cron", Summary: "Cron jobs of every user (admin)", Tags: []string{"cron"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*cronListOutput, error) {
		list, err := s.db.ListCronJobs(ctx, 0)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []*store.CronJob{}
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
		job := &store.CronJob{UserID: u.ID, Schedule: strings.TrimSpace(in.Body.Schedule), Command: strings.TrimSpace(in.Body.Command), Comment: strings.TrimSpace(in.Body.Comment), Enabled: true}
		if in.Body.Enabled != nil {
			job.Enabled = *in.Body.Enabled
		}
		if err := validateCronJob(job.Schedule, job.Command, job.Comment); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := s.db.CreateCronJob(ctx, job); err != nil {
			return nil, err
		}
		if err := s.applyCrontab(ctx, u); err != nil {
			s.db.DeleteCronJob(context.WithoutCancel(ctx), job.ID) //nolint:errcheck // rollback of a job that could not be installed
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
		was := *job
		if in.Body.Schedule != "" {
			job.Schedule = strings.TrimSpace(in.Body.Schedule)
		}
		if in.Body.Command != "" {
			job.Command = strings.TrimSpace(in.Body.Command)
		}
		if in.Body.Comment != nil {
			job.Comment = strings.TrimSpace(*in.Body.Comment)
		}
		if in.Body.Enabled != nil {
			job.Enabled = *in.Body.Enabled
		}
		if err := validateCronJob(job.Schedule, job.Command, job.Comment); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := s.db.UpdateCronJob(ctx, job); err != nil {
			return nil, err
		}
		if err := s.applyCrontab(ctx, u); err != nil {
			// The account's crontab still has the job as it was, and so must
			// the panel: a value crontab refused, left in the store, would
			// fail every later rewrite of this account's crontab.
			if rerr := s.db.UpdateCronJob(context.WithoutCancel(ctx), &was); rerr != nil {
				s.log.Error("cron: restoring a job the crontab refused", "id", was.ID, "err", rerr)
			}
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
		// The crontab goes first: a job deleted from the store while the
		// install fails would keep running from the old crontab, out of the
		// panel's sight and beyond its reach.
		jobs, err := s.db.ListCronJobs(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		rest := make([]*store.CronJob, 0, len(jobs))
		for _, j := range jobs {
			if j.ID != job.ID {
				rest = append(rest, j)
			}
		}
		if err := s.installCrontab(ctx, u, rest); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		if err := s.db.DeleteCronJob(ctx, job.ID); err != nil {
			// The job is still stored: put it back into the crontab.
			if aerr := s.applyCrontab(context.WithoutCancel(ctx), u); aerr != nil {
				s.log.Error("cron: restoring the crontab after a failed delete", "login", u.Login, "err", aerr)
			}
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "cron.delete", Target: u.Login, IP: requestInfo(ctx).IP})
		return nil, nil
	})
}
