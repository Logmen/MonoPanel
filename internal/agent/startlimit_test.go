package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/systemd"
)

// fakeJob is how a job of the fake systemd ends: the job's own result
// ("done", "failed", "dependency" …) and the unit's Result after it.
type fakeJob struct{ job, result string }

// fakeSystemd stands in for systemd: it runs the jobs it is asked for as the
// test scripted them, keeps each unit's Result as systemd does (a reset
// makes it "success" again) and records the jobs and resets it was asked
// for, "<action> <unit>", in order.
type fakeSystemd struct {
	mu     sync.Mutex
	calls  []string
	jobs   map[string][]fakeJob
	result map[string]string
	// afterJob runs after each job: a test changes the host there.
	afterJob func(action, unit string)
}

// withFakeSystemd has the agent talk to a fake systemd for one test.
func withFakeSystemd(t *testing.T) *fakeSystemd {
	t.Helper()
	f := &fakeSystemd{jobs: map[string][]fakeJob{}, result: map[string]string{}}
	prev := connectSystemd
	connectSystemd = func(context.Context) (systemdConn, error) { return f, nil }
	t.Cleanup(func() { connectSystemd = prev })
	return f
}

// script sets how the next jobs of unit end; once they are used up, a job
// ends "done".
func (f *fakeSystemd) script(unit string, jobs ...fakeJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[unit] = jobs
}

func (f *fakeSystemd) asked() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ", ")
}

func (f *fakeSystemd) job(action, unit string) error {
	f.mu.Lock()
	f.calls = append(f.calls, action+" "+unit)
	j := fakeJob{"done", "success"}
	if next := f.jobs[unit]; len(next) > 0 {
		j, f.jobs[unit] = next[0], next[1:]
	}
	f.result[unit] = j.result
	after := f.afterJob
	f.mu.Unlock()
	if after != nil {
		after(action, unit)
	}
	if j.job != "done" {
		return &systemd.JobError{Op: action, Unit: unit, Result: j.job}
	}
	return nil
}

func (f *fakeSystemd) Start(_ context.Context, unit string) error   { return f.job("start", unit) }
func (f *fakeSystemd) Stop(_ context.Context, unit string) error    { return f.job("stop", unit) }
func (f *fakeSystemd) Reload(_ context.Context, unit string) error  { return f.job("reload", unit) }
func (f *fakeSystemd) Restart(_ context.Context, unit string) error { return f.job("restart", unit) }
func (f *fakeSystemd) ReloadOrRestart(_ context.Context, unit string) error {
	return f.job("reload-or-restart", unit)
}
func (f *fakeSystemd) Enable(_ context.Context, unit string) error  { return f.job("enable", unit) }
func (f *fakeSystemd) Disable(_ context.Context, unit string) error { return f.job("disable", unit) }
func (f *fakeSystemd) DaemonReload(context.Context) error           { return nil }
func (f *fakeSystemd) MainPID(context.Context, string) (uint32, error) {
	return 0, nil
}
func (f *fakeSystemd) PIDFile(context.Context, string) (string, error) { return "", nil }
func (f *fakeSystemd) Close()                                          {}

func (f *fakeSystemd) Status(_ context.Context, unit string) (systemd.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := systemd.Status{Unit: unit, LoadState: "loaded", ActiveState: "active", SubState: "running", UnitFileState: "enabled"}
	if r := f.result[unit]; r != "" && r != "success" {
		st.ActiveState, st.SubState = "failed", "failed"
	}
	return st, nil
}

func (f *fakeSystemd) Result(_ context.Context, unit string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.result[unit]; r != "" {
		return r, nil
	}
	return "success", nil
}

func (f *fakeSystemd) ResetFailed(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "reset-failed "+unit)
	f.result[unit] = "success"
	return nil
}

// startLimitHost is a host with the trees root's daemons act on, a
// directory config/apply may write to, and the agent's log at hand.
func startLimitHost(t *testing.T) (s *Server, run, allowed string, logs *bytes.Buffer) {
	t.Helper()
	s, run, _, _ = treeHost(t)
	allowed = filepath.Join(filepath.Dir(run), "conf")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.Agent.ExtraWritePrefixes = []string{allowed + "/"}
	logs = &bytes.Buffer{}
	s.log = slog.New(slog.NewTextHandler(logs, nil))
	return s, run, allowed, logs
}

// A unit systemd refused for its start limit — restarted too often while it
// ran fine: every package of a PHP branch restarts its php-fpm, and the
// panel's reload right after was refused — is reset and started once more,
// whichever way the agent starts it: /v1/service, the reloads and restarts
// of config/apply, the reload after paths/remove. The agent says so.
func TestStartLimitHitIsRetriedOnce(t *testing.T) {
	const unit = "php8.4-fpm.service"
	hit := fakeJob{"failed", "start-limit-hit"}
	for _, c := range []struct {
		name, job string
		call      func(ctx context.Context, s *Server, allowed string) error
	}{
		{"service start", "start", func(ctx context.Context, s *Server, _ string) error {
			res, err := s.service(ctx, &ServiceRequest{Unit: unit, Action: "start"})
			if err == nil && res.Status.ActiveState != "active" {
				t.Errorf("status after the retry: %+v", res.Status)
			}
			return err
		}},
		{"service restart", "restart", func(ctx context.Context, s *Server, _ string) error {
			_, err := s.service(ctx, &ServiceRequest{Unit: unit, Action: "restart"})
			return err
		}},
		{"service reload-or-restart", "reload-or-restart", func(ctx context.Context, s *Server, _ string) error {
			_, err := s.service(ctx, &ServiceRequest{Unit: unit, Action: "reload-or-restart"})
			return err
		}},
		{"config/apply reload", "reload-or-restart", func(ctx context.Context, s *Server, allowed string) error {
			res, err := s.applyConfigSet(ctx, &ApplyConfigSetRequest{Files: []FileSpec{{Path: filepath.Join(allowed, "pool.conf"), Content: "x"}}, Reload: []string{unit}})
			if err == nil && strings.Join(res.Reloaded, " ") != unit {
				t.Errorf("reloaded: %v", res.Reloaded)
			}
			return err
		}},
		{"config/apply restart", "restart", func(ctx context.Context, s *Server, _ string) error {
			_, err := s.applyConfigSet(ctx, &ApplyConfigSetRequest{Restart: []string{unit}, Force: true})
			return err
		}},
		{"paths/remove reload", "reload-or-restart", func(ctx context.Context, s *Server, allowed string) error {
			old := filepath.Join(allowed, "old.conf")
			if err := os.WriteFile(old, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := s.removePaths(ctx, &RemovePathsRequest{Paths: []string{old}, Reload: []string{unit}})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, _, allowed, logs := startLimitHost(t)
			sd := withFakeSystemd(t)
			sd.script(unit, hit)
			if err := c.call(ctx, s, allowed); err != nil {
				t.Fatal(err)
			}
			want := c.job + " " + unit + ", reset-failed " + unit + ", " + c.job + " " + unit
			if got := sd.asked(); got != want {
				t.Fatalf("asked systemd: %s\nwant: %s", got, want)
			}
			if !strings.Contains(logs.String(), "start limit") || !strings.Contains(logs.String(), "unit="+unit) {
				t.Errorf("the retry is not in the log:\n%s", logs)
			}
		})
	}
}

// Only a refusal for the start limit is retried: a job that failed for any
// other reason — the unit's own Result says why (exit-code: it crashed,
// which is also what systemd keeps when the limit stops a crash loop) — or
// ended otherwise than "failed" (a stale start-limit-hit of the unit's is no
// reason then), and a job that starts nothing (reload, stop, enable) fail as
// they did, with nothing reset.
func TestStartLimitOnlyForStartLimitHit(t *testing.T) {
	const unit = "php8.4-fpm.service"
	for _, c := range []struct {
		name, action string
		job          fakeJob
	}{
		{"crashed", "restart", fakeJob{"failed", "exit-code"}},
		{"killed", "start", fakeJob{"failed", "signal"}},
		{"a dependency failed", "restart", fakeJob{"dependency", "start-limit-hit"}},
		{"timed out", "reload-or-restart", fakeJob{"timeout", "start-limit-hit"}},
		{"reload", "reload", fakeJob{"failed", "start-limit-hit"}},
		{"stop", "stop", fakeJob{"failed", "start-limit-hit"}},
		{"enable", "enable", fakeJob{"failed", "start-limit-hit"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, _, _, logs := startLimitHost(t)
			sd := withFakeSystemd(t)
			sd.script(unit, c.job)
			_, err := s.service(ctx, &ServiceRequest{Unit: unit, Action: c.action})
			if err == nil || !strings.Contains(err.Error(), `job result "`+c.job.job+`"`) {
				t.Fatalf("got %v, want the job's own failure", err)
			}
			if got := sd.asked(); got != c.action+" "+unit {
				t.Fatalf("asked systemd: %s", got)
			}
			if strings.Contains(logs.String(), "start limit") {
				t.Errorf("logged a retry:\n%s", logs)
			}
		})
	}
	// config/apply's reload as well.
	ctx := context.Background()
	s, _, _, _ := startLimitHost(t)
	sd := withFakeSystemd(t)
	sd.script(unit, fakeJob{"failed", "exit-code"})
	if _, err := s.applyConfigSet(ctx, &ApplyConfigSetRequest{Reload: []string{unit}, Force: true}); err == nil || !strings.Contains(err.Error(), "files applied but reload failed") {
		t.Fatalf("config/apply: %v", err)
	}
	if got := sd.asked(); got != "reload-or-restart "+unit {
		t.Fatalf("config/apply asked systemd: %s", got)
	}
}

// The retry is one, never a loop: a unit that fails again after the reset
// fails with its own error — another start limit included — and one call
// resets it once.
func TestStartLimitRetryFailsWithItsOwnError(t *testing.T) {
	const unit = "fail2ban.service"
	hit := fakeJob{"failed", "start-limit-hit"}
	ctx := context.Background()
	for _, again := range []fakeJob{{"failed", "exit-code"}, hit} {
		s, _, _, logs := startLimitHost(t)
		sd := withFakeSystemd(t)
		sd.script(unit, hit, again, hit, hit)
		_, err := s.applyConfigSet(ctx, &ApplyConfigSetRequest{Restart: []string{unit}, Force: true})
		if err == nil || !strings.Contains(err.Error(), "files applied but restart failed") || !strings.Contains(err.Error(), `restart fail2ban.service: job result "failed"`) {
			t.Fatalf("after %s: %v", again.result, err)
		}
		if got, want := sd.asked(), "restart "+unit+", reset-failed "+unit+", restart "+unit; got != want {
			t.Fatalf("after %s, asked systemd: %s", again.result, got)
		}
		if !strings.Contains(logs.String(), "failed again after the reset") {
			t.Errorf("after %s: the second failure is not in the log:\n%s", again.result, logs)
		}
		// The next call of the API is a call of its own, with its own retry.
		_, err = s.service(ctx, &ServiceRequest{Unit: unit, Action: "restart"})
		if err == nil {
			t.Fatalf("after %s: the next call", again.result)
		}
		if got := strings.Count(sd.asked(), "reset-failed"); got != 2 {
			t.Fatalf("after %s: %d resets in two calls: %s", again.result, got, sd.asked())
		}
	}
}

// The retry is a start like the first try: the agent's checks before it
// starts php-fpm, nginx or Apache run again, and a tree that is someone
// else's by then — php/ others may write to — stops it before the unit is
// reset or started. The unit stays as systemd left it.
func TestStartLimitRetryPassesTheChecksAgain(t *testing.T) {
	const unit = "php8.4-fpm.service"
	hit := fakeJob{"failed", "start-limit-hit"}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		call func(s *Server) error
	}{
		{"service", func(s *Server) error {
			_, err := s.service(ctx, &ServiceRequest{Unit: unit, Action: "restart"})
			return err
		}},
		{"config/apply", func(s *Server) error {
			_, err := s.applyConfigSet(ctx, &ApplyConfigSetRequest{Reload: []string{unit}, Force: true})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, run, _, _ := startLimitHost(t)
			php := filepath.Join(run, "php")
			sd := withFakeSystemd(t)
			sd.script(unit, hit)
			sd.afterJob = func(string, string) {
				if err := os.Chmod(php, 0o777); err != nil {
					t.Error(err)
				}
			}
			defer os.Chmod(php, 0o755) //nolint:errcheck // the test's own directory
			err := c.call(s)
			if agentStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), php) || !strings.Contains(err.Error(), "refusing to start, restart or reload "+unit) {
				t.Fatalf("got %v, want the refusal of the trees", err)
			}
			if got := sd.asked(); strings.Contains(got, "reset-failed") || strings.Count(got, unit) != 1 {
				t.Fatalf("asked systemd: %s", got)
			}
		})
	}
}

// The retry of a shared folder's mount checks its mount point again, as the
// first try did: one that is no longer root's alone by then — the home of
// the SFTP-only guest made writable for its group — is refused before the
// unit is reset or mounted, from /v1/service and from config/apply alike.
func TestStartLimitRetryChecksTheMountPointAgain(t *testing.T) {
	ctx := context.Background()
	h := newShareHost(t)
	where := filepath.Join(h.home, "kaspy")
	mkdirs(t, where)
	unit := h.placeUnit(t, where)
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"service", func() error {
			_, err := h.s.service(ctx, &ServiceRequest{Unit: unit, Action: "restart"})
			return err
		}},
		{"config/apply", func() error {
			_, err := h.s.applyConfigSet(ctx, &ApplyConfigSetRequest{Restart: []string{unit}, Force: true})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			chmods(t, map[string]uint32{h.home: 0o750})
			sd := withFakeSystemd(t)
			sd.script(unit, fakeJob{"failed", "start-limit-hit"})
			sd.afterJob = func(string, string) { chmods(t, map[string]uint32{h.home: 0o770}) }
			err := c.call()
			if agentStatus(err) != http.StatusForbidden || !strings.Contains(err.Error(), "mount point") || !strings.Contains(err.Error(), "writable by the group") {
				t.Fatalf("got %v, want the refusal of the mount point", err)
			}
			if got := sd.asked(); strings.Contains(got, "reset-failed") || strings.Count(got, unit) != 1 {
				t.Fatalf("asked systemd: %s", got)
			}
		})
	}
}
