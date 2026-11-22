package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/systemd"
)

func init() {
	// No test reaches the host's systemd: the tests of the runs put a fake
	// of their own in (withUnits), for the others systemd is unreachable.
	connectPkgUnits = func(context.Context) (pkgUnits, error) { return nil, errors.New("tests do not reach systemd") }
}

// fakeUnits stands for systemd in the tests of the runs: it records the
// units started, stopped and reset, and plays each run as the test says.
type fakeUnits struct {
	t  *testing.T
	mu sync.Mutex
	// started are the units started, with what the run found of its output
	// file when it started (outputs: the file's path, mode and owner).
	started []systemd.Oneshot
	outputs []string
	stopped []string
	reset   []string
	// play is what a run prints and how it ends: the job's result and how
	// its main process exited. nil prints "ok" and exits 0.
	play func(o systemd.Oneshot) (out, result string, exit systemd.Exit)
	// hang keeps every run going until it is stopped.
	hang bool
	// busy answers the polls of running: each poll the next entry, the
	// last one from then on (none: nothing runs).
	busy     [][]string
	polls    int
	patterns []string
	// startedAt is the poll count at each start.
	startedAt []int
	exits     map[string]systemd.Exit
	jobs      map[string]chan string
}

// withUnits gives s a directory of its own (the test's user stands for
// root) and the fake for systemd; a run's program is /usr/bin/<name>.
func withUnits(t *testing.T, s *Server) *fakeUnits {
	t.Helper()
	s.cfg.Agent.StateDir = t.TempDir()
	f := &fakeUnits{t: t, exits: map[string]systemd.Exit{}, jobs: map[string]chan string{}}
	oldConnect, oldBinary, oldPoll := connectPkgUnits, pkgBinary, pkgWaitPoll
	connectPkgUnits = func(context.Context) (pkgUnits, error) { return f, nil }
	pkgBinary = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	pkgWaitPoll = 5 * time.Millisecond
	t.Cleanup(func() { connectPkgUnits, pkgBinary, pkgWaitPoll = oldConnect, oldBinary, oldPoll })
	return f
}

func (f *fakeUnits) start(_ context.Context, o systemd.Oneshot) (<-chan string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var st unix.Stat_t
	path, _ := os.Readlink("/proc/self/fd/" + strconv.Itoa(o.Output))
	if err := unix.Fstat(o.Output, &st); err != nil {
		f.t.Errorf("the run's output: %v", err)
	}
	f.started = append(f.started, o)
	f.startedAt = append(f.startedAt, f.polls)
	f.outputs = append(f.outputs, path+" "+strconv.FormatUint(uint64(st.Mode), 8)+" "+strconv.Itoa(int(st.Uid)))
	out, result, exit := "ok\n", "done", systemd.Exit{Code: 1, Result: "success"}
	if f.play != nil {
		out, result, exit = f.play(o)
	}
	// The run writes into the descriptor the agent passed, as dpkg would
	// through systemd's copy of it.
	if _, err := unix.Write(o.Output, []byte(out)); err != nil {
		f.t.Errorf("write the run's output: %v", err)
	}
	ch := make(chan string, 1)
	f.exits[o.Unit] = exit
	if f.hang {
		f.jobs[o.Unit] = ch
		return ch, nil
	}
	ch <- result
	return ch, nil
}

func (f *fakeUnits) exit(_ context.Context, unit string) (systemd.Exit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exits[unit], nil
}

func (f *fakeUnits) resetFailed(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset = append(f.reset, unit)
	return nil
}

// stop ends a hanging run as systemd does: its start job is cancelled, its
// main process killed by SIGTERM.
func (f *fakeUnits) stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, unit)
	if ch, ok := f.jobs[unit]; ok {
		f.exits[unit] = systemd.Exit{Code: 2, Status: int32(unix.SIGTERM), Result: "signal"}
		ch <- "canceled"
		delete(f.jobs, unit)
	}
	return nil
}

func (f *fakeUnits) running(_ context.Context, patterns ...string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patterns = patterns
	var names []string
	switch {
	case len(f.busy) == 0:
	case f.polls < len(f.busy):
		names = f.busy[f.polls]
	default:
		names = f.busy[len(f.busy)-1]
	}
	f.polls++
	return names, nil
}

func (f *fakeUnits) close() {}

const testRun = "0123456789abcdef"

// A change of the package manager runs as a unit of its own that systemd
// starts — not as the agent's child, which dies with the agent: the argv
// osprofile builds, no shell, the package manager's environment, the
// agent's own fences, the time limit and systemd's margin past it. Its
// output goes into a file of the agent's (root's 0600, in its own
// directory), not through a pipe, and comes back in the answer; the file
// goes with the run.
func TestPkgRunsInAUnitOfItsOwn(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	f := withUnits(t, deb)
	f.play = func(systemd.Oneshot) (string, string, systemd.Exit) {
		return "Setting up nginx (1.30.7-1~trixie) ...\n", "done", systemd.Exit{Code: 1, Result: "success"}
	}
	pm := deb.profile.Packages()
	resp, err := deb.pkg(context.Background(), &PkgRequest{Action: "install", Packages: []string{"nginx"}, Run: testRun})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 {
		t.Fatalf("units started: %+v", f.started)
	}
	o := f.started[0]
	if o.Unit != "monopanel-pkg-"+testRun+".service" || o.Path != "/usr/bin/apt-get" || !slices.Equal(o.Argv, pm.InstallArgv([]string{"nginx"})) || !slices.Equal(o.Env, pm.Env()) {
		t.Fatalf("the run: %+v", o)
	}
	for _, a := range o.Argv {
		if a == "sh" || a == "-c" || strings.HasSuffix(a, "/sh") || strings.Contains(a, "$") {
			t.Fatalf("a shell or an expansion in the run: %q", o.Argv)
		}
	}
	if !strings.Contains(o.Description, "apt-get") || !strings.Contains(o.Description, "nginx") {
		t.Errorf("description %q", o.Description)
	}
	if o.Limit <= pkgRunLimit || o.Limit > pkgRunLimit+pkgStopMargin {
		t.Errorf("limit %s: systemd stops the run a little after the agent would", o.Limit)
	}
	fences := map[string]any{}
	for _, s := range o.Settings {
		fences[s.Name] = s.Value
	}
	for name, want := range map[string]any{"ProtectHome": "read-only", "PrivateTmp": true, "ProtectKernelModules": true, "ProtectKernelTunables": true, "ProtectControlGroups": true, "OOMPolicy": "continue"} {
		if fences[name] != want {
			t.Errorf("%s = %v, want %v (the agent's own fences)", name, fences[name], want)
		}
	}
	if archs, _ := fences["SystemCallArchitectures"].([]string); !slices.Equal(archs, []string{"native"}) {
		t.Errorf("SystemCallArchitectures %v", fences["SystemCallArchitectures"])
	}
	output := filepath.Join(deb.pkgDir(), testRun+".out")
	if want := output + " " + strconv.FormatUint(unix.S_IFREG|0o600, 8) + " " + strconv.Itoa(os.Geteuid()); f.outputs[0] != want {
		t.Errorf("the run's output: %s, want %s", f.outputs[0], want)
	}
	if st, err := os.Stat(deb.pkgDir()); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("the runs' directory: %v %v", st, err)
	}
	if !strings.Contains(resp.Output, "Setting up nginx") {
		t.Fatalf("the output: %q", resp.Output)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the output file stays: %v", err)
	}
	if len(f.reset) != 0 || len(f.stopped) != 0 {
		t.Errorf("a successful run reset %v, stopped %v", f.reset, f.stopped)
	}

	// The reads stay the agent's children: nothing to outlive the agent.
	if _, err := deb.pkg(context.Background(), &PkgRequest{Action: "query", Packages: []string{"nginx"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 {
		t.Fatalf("a query ran in a unit: %+v", f.started[1:])
	}
	// The run's name is the caller's, or the agent's own when it gives none.
	if _, err := deb.pkg(context.Background(), &PkgRequest{Action: "update-index"}); err != nil {
		t.Fatal(err)
	}
	if u := f.started[1].Unit; u == f.started[0].Unit || !strings.HasPrefix(u, "monopanel-pkg-") || !slices.Equal(f.started[1].Argv, pm.UpdateIndexArgv()) {
		t.Fatalf("a run the caller did not name: %s %q", u, f.started[1].Argv)
	}
	for _, bad := range []string{"../../etc", "0123456789ABCDEF", "abc", "0123456789abcdef0"} {
		if _, err := deb.pkg(context.Background(), &PkgRequest{Action: "install", Packages: []string{"nginx"}, Run: bad}); agentStatus(err) != http.StatusBadRequest {
			t.Errorf("run %q: %v", bad, err)
		}
	}
}

// A run that fails says how — the exit status and its unit — with its
// output; the failed unit systemd keeps is reset, the file goes.
func TestPkgRunFailure(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	f := withUnits(t, deb)
	f.play = func(systemd.Oneshot) (string, string, systemd.Exit) {
		return "E: Unable to locate package nginx\n", "failed", systemd.Exit{Code: 1, Status: 100, Result: "exit-code"}
	}
	_, err := deb.pkg(context.Background(), &PkgRequest{Action: "install", Packages: []string{"nginx"}, Run: testRun})
	var ae *Error
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "apt install failed: exit status 100 (monopanel-pkg-"+testRun+".service)") || !strings.Contains(ae.Output, "Unable to locate package") {
		t.Fatalf("a failed run: %#v", err)
	}
	if !slices.Equal(f.reset, []string{pkgUnit(testRun)}) {
		t.Errorf("reset %v", f.reset)
	}
	if _, err := os.Lstat(filepath.Join(deb.pkgDir(), testRun+".out")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the output file stays: %v", err)
	}
	for _, c := range []struct {
		e    systemd.Exit
		code int
		why  string
	}{
		{systemd.Exit{Code: 2, Status: 9, Result: "signal"}, 137, "killed by SIGKILL"},
		{systemd.Exit{Code: 2, Status: 15, Result: "timeout"}, 143, "killed by SIGTERM, result timeout"},
		{systemd.Exit{Code: 1, Status: 1, Result: "oom-kill"}, 1, "exit status 1, result oom-kill"},
		{systemd.Exit{}, -1, `its job ended "failed"`},
	} {
		if code, why := exitOf(c.e, nil, "failed"); code != c.code || why != c.why {
			t.Errorf("%+v: %d %q", c.e, code, why)
		}
	}
}

// A run past its time limit is stopped as a whole: its unit, the package
// manager with what it started. The answer says so and has what the run
// printed; the file goes.
func TestPkgRunTimeLimitStopsTheUnit(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	f := withUnits(t, deb)
	f.hang = true
	f.play = func(systemd.Oneshot) (string, string, systemd.Exit) {
		return "Unpacking php8.4-fpm (8.4.28-1) ...\n", "", systemd.Exit{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := deb.runPackageUnit(ctx, testRun, nil, []string{"apt-get", "-q", "-y", "install", "php8.4-fpm"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.stopped, []string{pkgUnit(testRun)}) {
		t.Fatalf("stopped %v", f.stopped)
	}
	if !res.failed() || !strings.Contains(res.why, "stopped at its time limit") || res.code != 128+int(unix.SIGTERM) || !strings.Contains(res.output, "Unpacking php8.4-fpm") {
		t.Fatalf("the run: %+v", res)
	}
	if l := f.started[0].Limit; l > 100*time.Millisecond+pkgStopMargin {
		t.Errorf("systemd's limit %s is not the run's", l)
	}
	if _, err := os.Lstat(filepath.Join(deb.pkgDir(), testRun+".out")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the output file stays: %v", err)
	}
}

// The output file is a new file of the agent's: one in its place — a file,
// a link to somewhere else — and the run does not start, and what the link
// points to stays as it was. No argument systemd would expand reaches it.
func TestPkgOutputFileIsNew(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	f := withUnits(t, deb)
	dfd, err := deb.openPkgDir()
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(dfd)
	output := filepath.Join(deb.pkgDir(), testRun+".out")
	target := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(target, []byte("root:x:0:0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func() error{
		"a file": func() error { return os.WriteFile(output, nil, 0o600) },
		"a link": func() error { return os.Symlink(target, output) },
	} {
		if err := plant(); err != nil {
			t.Fatal(err)
		}
		if _, err := deb.runPackageUnit(context.Background(), testRun, nil, []string{"apt-get", "update"}); err == nil || !strings.Contains(err.Error(), "exists") {
			t.Errorf("%s in its place: %v", name, err)
		}
		os.Remove(output)
	}
	if b, _ := os.ReadFile(target); string(b) != "root:x:0:0\n" {
		t.Fatalf("the link's target: %q", b)
	}
	if _, err := deb.runPackageUnit(context.Background(), testRun, nil, []string{"apt-get", "install", "${PATH}"}); agentStatus(err) != http.StatusForbidden {
		t.Errorf("an argument with a $: %v", err)
	}
	if len(f.started) != 0 {
		t.Fatalf("started: %+v", f.started)
	}
}

// pkgMu goes with the agent, a run does not: after a restart the agent's
// next run waits until the runs of the earlier agent and a panel install
// are over, rather than meeting them on the dpkg lock (apt-get gives up
// after ten minutes) — and the output an earlier run left is logged and
// removed. A request that ends meanwhile starts nothing.
func TestPkgWaitsForRunsTheAgentDidNotStart(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	var logs bytes.Buffer
	deb.log = slog.New(slog.NewTextHandler(&logs, nil))
	f := withUnits(t, deb)
	earlier := "fedcba9876543210"
	f.busy = [][]string{{pkgUnit(earlier)}, {pkgUnit(earlier), updateUnit}, {updateUnit}, {}}
	dfd, err := deb.openPkgDir()
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(dfd)
	if err := os.WriteFile(filepath.Join(deb.pkgDir(), earlier+".out"), []byte("Setting up php8.4-fpm ...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := deb.pkg(context.Background(), &PkgRequest{Action: "update-index", Run: testRun}); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 || f.startedAt[0] != 4 {
		t.Fatalf("started %d after %v polls, want once after 4", len(f.started), f.startedAt)
	}
	if !slices.Equal(f.patterns, []string{pkgUnitPattern, updateUnit}) {
		t.Errorf("waited for %v", f.patterns)
	}
	if _, err := os.Lstat(filepath.Join(deb.pkgDir(), earlier+".out")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the earlier run's output stays: %v", err)
	}
	if !strings.Contains(logs.String(), pkgUnit(earlier)) || !strings.Contains(logs.String(), "Setting up php8.4-fpm") {
		t.Errorf("the earlier run's end is not logged:\n%s", logs.String())
	}

	f.busy, f.polls = [][]string{{pkgUnit(earlier)}}, 0
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := deb.pkg(ctx, &PkgRequest{Action: "update-index"}); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("a request that ended while it waited: %v", err)
	}
	if len(f.started) != 1 {
		t.Fatalf("it started: %+v", f.started[1:])
	}
}

// The agent's start takes care of what runs of an earlier agent left: the
// output of a run that is over is logged and goes, that of a run still going
// stays for its end, nothing else is touched. The release packages
// downloaded go once no run is going: a run still going installs its
// release package from there, and may not have handed it to dpkg yet.
func TestPkgRunsLeftOverAtStart(t *testing.T) {
	deb := policyServer(t, debian13)
	var logs bytes.Buffer
	deb.log = slog.New(slog.NewTextHandler(&logs, nil))
	f := withUnits(t, deb)
	over, going := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	f.busy = [][]string{{pkgUnit(going)}}
	dfd, err := deb.openPkgDir()
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(dfd)
	dir := deb.pkgDir()
	for name, body := range map[string]string{over + ".out": "Setting up nginx ...\nErrors were encountered while processing: nginx\n", going + ".out": "Unpacking ...\n", "notes": "the administrator's"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release := filepath.Join(dir, releaseDirPrefix+"x1")
	if err := os.Mkdir(release, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "percona-release_latest.generic_all.deb"), []byte("deb"), 0o600); err != nil {
		t.Fatal(err)
	}
	deb.reapPkgRuns(context.Background())
	for name, stays := range map[string]bool{over + ".out": false, going + ".out": true, "notes": true, releaseDirPrefix + "x1": true} {
		if _, err := os.Lstat(filepath.Join(dir, name)); (err == nil) != stays {
			t.Errorf("%s: stays %v, want %v", name, err == nil, stays)
		}
	}
	if !strings.Contains(logs.String(), pkgUnit(over)) || !strings.Contains(logs.String(), "Errors were encountered") || !strings.Contains(logs.String(), pkgUnit(going)) {
		t.Errorf("the log:\n%s", logs.String())
	}
	if !slices.Equal(f.patterns, []string{pkgUnitPattern}) {
		t.Errorf("asked for %v", f.patterns)
	}

	// A later start, with nothing going: the release package goes too, and
	// so does the output of the run that has ended meanwhile.
	f.busy, f.polls = nil, 0
	deb.reapPkgRuns(context.Background())
	for name, stays := range map[string]bool{going + ".out": false, "notes": true, releaseDirPrefix + "x1": false} {
		if _, err := os.Lstat(filepath.Join(dir, name)); (err == nil) != stays {
			t.Errorf("a later start: %s stays %v, want %v", name, err == nil, stays)
		}
	}
}

// The client names a change's run, and when the agent goes away in the
// middle of the call (restarted, stopped) the error says where the run went
// on and how to see its end — not a bare connection error. A call that
// never reached the agent, or one that runs nothing in a unit, stays an
// unreachable agent.
func TestClientPointsAtTheRunWhenTheAgentGoesAway(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	var mu sync.Mutex
	var runs []string
	mux := http.NewServeMux()
	gone := func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Run string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		runs = append(runs, req.Run)
		mu.Unlock()
		// the agent dies before it answers
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}
	mux.HandleFunc("/v1/pkg", gone)
	mux.HandleFunc("/v1/tool", gone)
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck // closed at the end
	t.Cleanup(func() { srv.Close() })
	c := NewClient(sock)
	ctx := context.Background()

	_, err = c.Pkg(ctx, "upgrade", "nginx")
	mu.Lock()
	run := runs[len(runs)-1]
	mu.Unlock()
	if !runIDRe.MatchString(run) {
		t.Fatalf("the run's name: %q", run)
	}
	if err == nil || !strings.Contains(err.Error(), "goes on to its end in "+pkgUnit(run)) || !strings.Contains(err.Error(), "journalctl -u "+pkgUnit(run)) || !strings.Contains(err.Error(), "upgrade nginx") {
		t.Fatalf("the agent gone during an upgrade: %v", err)
	}
	if _, err := c.Tool(ctx, &ToolRequest{Name: "dnf", Args: []string{"-y", "install", "--enablerepo=crb", "opendkim"}}); err == nil || !strings.Contains(err.Error(), "journalctl -u monopanel-pkg-") {
		t.Fatalf("the agent gone during dnf install: %v", err)
	}
	for what, call := range map[string]func() error{
		"a query": func() error { _, err := c.Pkg(ctx, "query", "nginx"); return err },
		"dnf repolist": func() error {
			_, err := c.Tool(ctx, &ToolRequest{Name: "dnf", Args: []string{"-q", "repolist", "--all"}})
			return err
		},
		"no agent": func() error {
			_, err := NewClient(filepath.Join(t.TempDir(), "none.sock")).Pkg(ctx, "install", "nginx")
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "agent unreachable") || strings.Contains(err.Error(), "monopanel-pkg-") {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// dnf install with CodeReady Builder (planDnf) waits for the package
// manager's turn as /v1/pkg's changes do — pkgMu, a run an earlier agent
// left going, a panel install —, but that wait comes out of the request,
// not out of the run: the run gets the whole of the tool's time limit from
// its own start. Were the wait to eat it, the agent would stop dnf at what
// was left, in the middle of its transaction.
func TestPkgToolRunKeepsItsTimeLimit(t *testing.T) {
	s := withTrees(t, policyServer(t, alma9))
	f := withUnits(t, s)
	earlier := "fedcba9876543210"
	for range 40 {
		f.busy = append(f.busy, []string{pkgUnit(earlier)})
	}
	f.busy = append(f.busy, []string{})
	const budget = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	began := time.Now()
	if _, err := s.runPkgTool(ctx, testRun, []string{"dnf", "-y", "install", "--enablerepo=crb", "opendkim"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	waited := time.Since(began)
	if len(f.started) != 1 || f.startedAt[0] != len(f.busy) {
		t.Fatalf("started %d after %v polls, want once after %d", len(f.started), f.startedAt, len(f.busy))
	}
	if l := f.started[0].Limit - pkgStopMargin; l < budget-50*time.Millisecond {
		t.Fatalf("the run may go on for %s of the tool's %s: the %s it waited came out of the run", l.Round(time.Millisecond), budget, waited.Round(time.Millisecond))
	}
	if l := f.started[0].Limit - pkgStopMargin; l > budget {
		t.Fatalf("the run's limit %s is past the tool's %s", l, budget)
	}
}
