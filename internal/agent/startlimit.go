package agent

import (
	"context"
	"errors"

	"monopanel/internal/systemd"
)

// systemd starts a unit no more often than its start limit lets it
// (StartLimitBurst= starts in StartLimitIntervalSec=, 5 in 10 s unless the
// unit says otherwise) and leaves one that runs into it failed. Restarts of
// a unit that runs fine get there: every run of the package manager with
// packages of a PHP branch restarts its php-fpm, every new site restarts
// fail2ban, and the panel's own reload right after is refused ("Start
// request repeated too quickly", Result=start-limit-hit); the unit then
// stays down, as nothing starts it again. So when systemd refuses a job
// that starts a unit for its start limit, the agent resets the unit and
// runs the job once more, once (startUnit).
//
// That does not lift a unit's protection against crashing in a loop:
// when the limit refuses the restart that follows a crash, systemd keeps the
// crash as the unit's Result (exit-code, signal, core-dump, timeout …), and
// Result says start-limit-hit only when the runs before it ended well. A
// unit that fails after the reset fails with its own error, nothing loops,
// and each unit a call of the API's names gets one retry at most.

// systemdConn is the agent's link to systemd for /v1/service and for the
// reloads and restarts of config/apply and paths/remove: *systemd.Conn.
type systemdConn interface {
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Reload(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	ReloadOrRestart(ctx context.Context, unit string) error
	Enable(ctx context.Context, unit string) error
	Disable(ctx context.Context, unit string) error
	DaemonReload(ctx context.Context) error
	Status(ctx context.Context, unit string) (systemd.Status, error)
	MainPID(ctx context.Context, unit string) (uint32, error)
	PIDFile(ctx context.Context, unit string) (string, error)
	Result(ctx context.Context, unit string) (string, error)
	ResetFailed(ctx context.Context, unit string) error
	Close()
}

// connectSystemd opens the link to systemd. Tests, which have no systemd to
// talk to, replace it.
var connectSystemd = func(ctx context.Context) (systemdConn, error) {
	c, err := systemd.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// startUnit runs a job that starts unit if it is not running — start,
// restart or reload-or-restart (action) — and, when systemd refused it for
// the unit's start limit, resets the unit and runs it once more. The checks
// the agent makes before it starts a unit run again first, exactly as they
// ran before the first try (vetStart): the retry is a start of its own, and
// what they look at may have changed since. Should they refuse, the unit
// stays as systemd left it.
func (s *Server) startUnit(ctx context.Context, conn systemdConn, action, unit string) error {
	run := func() error {
		switch action {
		case "start":
			return conn.Start(ctx, unit)
		case "restart":
			return conn.Restart(ctx, unit)
		}
		return conn.ReloadOrRestart(ctx, unit)
	}
	err := run()
	if err == nil || !startLimitHit(ctx, conn, unit, err) {
		return err
	}
	if verr := s.vetStart(ctx, unit, action); verr != nil {
		s.log.Warn("systemd refused the unit for its start limit; not retried, the checks before a start refuse it now", "unit", unit, "action", action, "err", verr)
		return verr
	}
	if rerr := conn.ResetFailed(ctx, unit); rerr != nil {
		s.log.Warn("systemd refused the unit for its start limit, and it could not be reset", "unit", unit, "action", action, "err", rerr)
		return err
	}
	s.log.Warn("systemd refused the unit for its start limit (started too often, not crashed): reset it and running the job once more", "unit", unit, "action", action)
	if err := run(); err != nil {
		s.log.Warn("the unit failed again after the reset", "unit", unit, "action", action, "err", err)
		return err
	}
	s.log.Info("the unit started after the reset of its start limit", "unit", unit, "action", action)
	return nil
}

// startLimitHit tells whether err is a job systemd refused for the unit's
// start limit: the job failed, and the unit's Result says start-limit-hit.
// A job that ended otherwise — a dependency that failed, a timeout, a
// cancelled one — is not, whatever the unit's Result was before.
func startLimitHit(ctx context.Context, conn systemdConn, unit string, err error) bool {
	var je *systemd.JobError
	if !errors.As(err, &je) || je.Result != "failed" {
		return false
	}
	res, rerr := conn.Result(ctx, unit)
	return rerr == nil && res == "start-limit-hit"
}

// vetStart runs the checks the agent makes before it starts a unit: the
// mount point of a shared folder's unit (vetMountStart) and the trees the
// root masters of php-fpm, nginx and Apache act on (vetRootDaemonTrees).
func (s *Server) vetStart(ctx context.Context, unit, action string) error {
	if err := s.vetMountStart(fullUnit(unit), action); err != nil {
		return err
	}
	return s.vetRootDaemonTrees(ctx, []string{unit}, nil)
}
