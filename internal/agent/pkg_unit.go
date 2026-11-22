package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
	"monopanel/internal/systemd"
)

// The package manager's changes — /v1/pkg update-index, install, remove and
// upgrade, and dnf install with CodeReady Builder (planDnf) — run as a
// transient service of their own, monopanel-pkg-<run>.service, that systemd
// starts, not as the agent's children. In monopanel-agent.service's cgroup
// (KillMode=control-group) they died with the agent — restarted by a
// self-update's postinstall, by an administrator, by the API's Requires=
// chain — and dpkg or rpm was left with half a transaction ("dpkg was
// interrupted", a package unpacked but not configured, a service a script
// stopped and never started again). A scope (systemd-run --scope) would not
// do: its processes stay the agent's children in the agent's mount namespace,
// and the PrivateTmp systemd removes when the agent stops is their /tmp and
// /var/tmp — where rpm writes its scriptlets and debconf its templates. The
// output goes into a file of the agent's rather than through a pipe whose
// reader dies with the agent (dpkg killed by SIGPIPE halfway), and the agent
// reads it back once the run is over.

// pkgUnitPrefix names the runs' units: monopanel-pkg-<run>.service.
const pkgUnitPrefix = "monopanel-pkg-"

// pkgUnitPattern matches the units of all runs.
const pkgUnitPattern = pkgUnitPrefix + "*.service"

// pkgOutputSuffix ends the name of a run's output file, <run>.out.
const pkgOutputSuffix = ".out"

// pkgOutputMax is what of a run's output its caller gets: the end, as
// runArgv keeps it.
const pkgOutputMax = 256 * 1024

// pkgStopMargin is how much longer than its limit systemd lets a run go
// (TimeoutStartSec): at the limit the agent stops the run itself, and
// systemd's own stop is for a run the agent no longer watches, having been
// restarted meanwhile.
const pkgStopMargin = time.Minute

// pkgStopTimeout is a run's TimeoutStopSec: systemd's default, given as it
// is so that an administrator's lower DefaultTimeoutStopSec does not apply —
// how long the package manager has after SIGTERM before SIGKILL, and how
// long the cleanup of its /tmp after it (pkgCleanup) may take, which gives up
// well before (PrivateTmpCleanWait): a cleanup past it would fail a run that
// did what it was to.
const pkgStopTimeout = 90 * time.Second

// pkgStopWait bounds the wait for the end of a run that is stopped — over its
// limit, or by someone else: SIGTERM to all of it, SIGKILL after
// pkgStopTimeout, then the cleanup of its /tmp (PrivateTmpCleanWait).
const pkgStopWait = 3 * time.Minute

// runIDRe is a run's name. The API's client picks it (Client.Pkg), and so it
// knows the unit to point at when the agent goes away in the middle of a call.
var runIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// newRunID names a run.
func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}

// pkgUnit is the unit of the run.
func pkgUnit(run string) string { return pkgUnitPrefix + run + ".service" }

// pkgChanges tells the actions of /v1/pkg that change the system and run in
// a unit of their own; query, available and candidates only read, as the
// agent's children.
func pkgChanges(action string) bool {
	switch action {
	case "update-index", "install", "remove", "upgrade":
		return true
	}
	return false
}

// pkgFences are the agent's own fences (packaging/systemd/
// monopanel-agent.service) for the runs that are no longer its children:
// apt-get, dnf and the packages' scripts keep what they had in the agent —
// the homes read-only; no kernel modules, tunables, logs, cgroups, clock or
// host name; a /tmp and /var/tmp of their own, out of reach of what the
// sites' accounts put in the host's, which the run empties (pkgCleanup) and
// systemd removes once it is over. None of them makes systemd set
// NoNewPrivileges for a root service, so dnf goes into rpm_t as it does from
// dnf-automatic's units, and the cleanup into unconfined_service_t. An OOM
// kill of one of the processes does not have systemd stop the rest of the
// transaction either (OOMPolicy=continue; the default stops the whole unit).
var pkgFences = []systemd.Setting{
	{Name: "ProtectHome", Value: "read-only"},
	{Name: "ProtectKernelModules", Value: true},
	{Name: "ProtectKernelTunables", Value: true},
	{Name: "ProtectKernelLogs", Value: true},
	{Name: "ProtectControlGroups", Value: true},
	{Name: "ProtectClock", Value: true},
	{Name: "ProtectHostname", Value: true},
	{Name: "PrivateTmp", Value: true},
	{Name: "RestrictRealtime", Value: true},
	{Name: "LockPersonality", Value: true},
	{Name: "SystemCallArchitectures", Value: []string{"native"}},
	{Name: "OOMPolicy", Value: "continue"},
}

// pkgOneshot is the unit of a run: argv, the program at path, with env and
// its output into the descriptor output, for at most limit (and systemd's
// margin), its /tmp emptied once it is over.
func pkgOneshot(unit, path string, argv, env []string, limit time.Duration, output int) systemd.Oneshot {
	desc := "MonoPanel package manager: " + strings.Join(argv, " ")
	if len(desc) > 200 {
		desc = desc[:197] + "..."
	}
	return systemd.Oneshot{Unit: unit, Description: desc, Path: path, Argv: argv, Env: env, Output: output,
		Limit: limit + pkgStopMargin, StopLimit: pkgStopTimeout, StopPost: pkgCleanup(unit), Settings: pkgFences}
}

// pkgCleanup empties the run's own /tmp and /var/tmp once it is over,
// however it ended, before systemd removes them: the agent's binary, `clean-
// private-tmp <unit>` (privatetmp.go). On EL, systemd may not remove what
// package scripts leave there — percona-server-server's %post leaves a
// directory after every install and upgrade —, and each run would add an
// SELinux denial and a directory kept until the next boot. Its failure is
// ignored (IgnoreFailure), and it gives up after PrivateTmpCleanWait, well
// within the run's TimeoutStopSec (pkgStopTimeout), past which systemd would
// fail the run whatever IgnoreFailure says: the run's result and exit
// status stay dnf's or apt-get's, and what it says goes into the run's
// output. A binary at a path the unit could not name as it is
// (panelBinaryRe: no "$" systemd would expand) gets no cleanup: systemd
// removes what it can, as before.
func pkgCleanup(unit string) []systemd.Command {
	bin := pkgSelf()
	if !panelBinaryRe.MatchString(bin) {
		return nil
	}
	return []systemd.Command{{Path: bin, Argv: []string{bin, "clean-private-tmp", unit}, IgnoreFailure: true}}
}

// pkgSelf is the agent's binary, which empties a run's /tmp; tests replace
// it.
var pkgSelf = thisBinary

// pkgUnits is what the agent asks of systemd for the runs; tests replace it
// (connectPkgUnits).
type pkgUnits interface {
	start(ctx context.Context, o systemd.Oneshot) (<-chan string, error)
	exit(ctx context.Context, unit string) (systemd.Exit, error)
	resetFailed(ctx context.Context, unit string) error
	stop(ctx context.Context, unit string) error
	running(ctx context.Context, patterns ...string) ([]string, error)
	close()
}

type systemdPkgUnits struct {
	c *systemd.Conn
	// cancel ends the connection's own context
	cancel context.CancelFunc
}

func (u systemdPkgUnits) start(ctx context.Context, o systemd.Oneshot) (<-chan string, error) {
	return u.c.StartOneshot(ctx, o)
}
func (u systemdPkgUnits) exit(ctx context.Context, unit string) (systemd.Exit, error) {
	return u.c.MainExit(ctx, unit)
}
func (u systemdPkgUnits) resetFailed(ctx context.Context, unit string) error {
	return u.c.ResetFailed(ctx, unit)
}
func (u systemdPkgUnits) stop(ctx context.Context, unit string) error { return u.c.Stop(ctx, unit) }
func (u systemdPkgUnits) running(ctx context.Context, patterns ...string) ([]string, error) {
	return u.c.Running(ctx, patterns...)
}
func (u systemdPkgUnits) close() {
	u.c.Close()
	u.cancel()
}

// pkgDialWait bounds connecting to systemd: a bus that does not answer must
// not hold pkgMu, and every run of the package manager with it, for good.
const pkgDialWait = time.Minute

// connectPkgUnits reaches systemd. The connection lasts until close, not
// until ctx ends — systemd.Connect ties a connection to its context, and the
// stop of a run over its limit needs the connection after the run's context
// ended —, so it gets a context of its own: ctx, within pkgDialWait, bounds
// only the connecting. Tests replace it.
var connectPkgUnits = func(ctx context.Context) (pkgUnits, error) {
	dctx, dcancel := context.WithTimeout(ctx, pkgDialWait)
	defer dcancel()
	cctx, cancel := context.WithCancel(context.Background())
	unbind := context.AfterFunc(dctx, cancel)
	c, err := systemd.Connect(cctx)
	if !unbind() {
		// dctx ended while connecting: cancel closed what was made of the
		// connection
		if err == nil {
			c.Close()
		}
		return nil, fmt.Errorf("systemd did not answer: %w", context.Cause(dctx))
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return systemdPkgUnits{c: c, cancel: cancel}, nil
}

// pkgBinary is a run's program, found as the agent found it when it ran the
// program itself (its PATH); systemd gets it by its absolute path. Tests
// replace it.
var pkgBinary = exec.LookPath

// pkgRun is how a run ended.
type pkgRun struct {
	unit   string
	output string
	// code is the exit status, 128+n for a signal n, -1 when there is none
	// to tell; why says how the run failed ("exit status 100", "stopped at
	// its time limit of 2h0m0s"), "" when it succeeded.
	code int
	why  string
}

func (r *pkgRun) failed() bool { return r.why != "" }

// runPackageUnit runs argv (the package manager, by name) with env in the
// run's unit and waits until it is over or ctx ends — the run's time limit,
// as the caller set it: then the unit is stopped, the package manager with
// all it started. The error tells a run that did not start; how a run that
// did ended is in the result. The caller holds pkgMu.
func (s *Server) runPackageUnit(ctx context.Context, run string, env, argv []string) (*pkgRun, error) {
	if !runIDRe.MatchString(run) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid run id " + strconv.Quote(run)}
	}
	if len(argv) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "no command"}
	}
	for _, a := range argv {
		// systemd expands $NAME and ${NAME} in a unit's command line. No
		// argument the panel builds has a $ — package names, the vendors'
		// URLs, osprofile's options —, and no other one is passed on.
		if strings.Contains(a, "$") {
			return nil, notAllowed("the argument "+strconv.Quote(a), "systemd would expand the $ in it")
		}
	}
	path, err := pkgBinary(argv[0])
	if err != nil {
		return nil, &Error{Message: argv[0] + " not found", Output: err.Error()}
	}
	dfd, out, err := s.createPkgOutput(run)
	if err != nil {
		return nil, &Error{Message: "the output file of " + pkgUnit(run), Output: err.Error()}
	}
	defer func() {
		out.Close()
		_ = unix.Unlinkat(dfd, run+pkgOutputSuffix, 0)
		unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	}()
	units, err := connectPkgUnits(ctx)
	if err != nil {
		return nil, &Error{Message: "systemd unreachable", Output: err.Error()}
	}
	defer units.close()
	limit := pkgRunLimit
	if d, ok := ctx.Deadline(); ok {
		limit = time.Until(d)
	}
	unit := pkgUnit(run)
	done, err := units.start(ctx, pkgOneshot(unit, path, argv, env, limit, int(out.Fd())))
	if err != nil {
		return nil, &Error{Message: "start " + unit, Output: err.Error()}
	}
	res := &pkgRun{unit: unit}
	var result string
	stopped := false
	select {
	case result = <-done:
	case <-ctx.Done():
		stopped = true
		s.log.Warn("the package manager runs past its time limit: stopping it", "unit", unit, "limit", limit.Round(time.Second))
		sctx, cancel := context.WithTimeout(context.Background(), pkgStopWait)
		if err := units.stop(sctx, unit); err != nil {
			s.log.Error("stop the package manager", "unit", unit, "err", err)
		}
		select {
		case result = <-done:
		case <-sctx.Done():
		}
		cancel()
	}
	outside := result == "canceled" && !stopped
	if outside {
		// Stopped by someone else (systemctl stop, a shutdown): systemd
		// cancels the start job at once, while the unit is still going — the
		// package manager's end, the cleanup of its /tmp after it. A stop of
		// the agent's own joins that one and returns once the unit is over,
		// so that what is read below is how it ended, its output all of it,
		// and the failed unit there to reset.
		s.log.Warn("the package manager's run was stopped outside the panel: waiting for its end", "unit", unit)
		sctx, cancel := context.WithTimeout(context.Background(), pkgStopWait)
		if err := units.stop(sctx, unit); err != nil {
			s.log.Warn("the end of the package manager's run", "unit", unit, "err", err)
		}
		cancel()
	}
	if result != "done" {
		ectx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		e, err := units.exit(ectx, unit)
		// systemd keeps a failed unit until it is reset; what there is to
		// learn from it is read now.
		_ = units.resetFailed(ectx, unit)
		cancel()
		res.code, res.why = exitOf(e, err, result)
		if res.code == 0 {
			// the unit failed though its main process exited with 0 (a
			// result of systemd's own: oom-kill …): no success to report
			res.code = -1
		}
		switch {
		case stopped:
			res.why = fmt.Sprintf("stopped at its time limit of %s (%s)", limit.Round(time.Second), res.why)
		case outside:
			res.why = fmt.Sprintf("stopped outside the panel (%s)", res.why)
		}
	}
	res.output = readTail(out, pkgOutputMax)
	return res, nil
}

// exitOf is the exit status and the reason of a run whose start job ended
// with result (anything but done): how its main process ended (e).
func exitOf(e systemd.Exit, err error, result string) (int, string) {
	if err != nil {
		return -1, "its job ended " + strconv.Quote(result) + ", and systemd did not say how it exited: " + err.Error()
	}
	var why string
	code := -1
	switch e.Code {
	case 1: // CLD_EXITED
		code, why = int(e.Status), fmt.Sprintf("exit status %d", e.Status)
	case 2, 3: // CLD_KILLED, CLD_DUMPED
		code, why = 128+int(e.Status), "killed by "+unix.SignalName(syscall.Signal(e.Status))
	default:
		why = "its job ended " + strconv.Quote(result)
	}
	if e.Result != "" && e.Result != "success" && e.Result != "exit-code" && e.Result != "signal" {
		why += ", result " + e.Result
	}
	return code, why
}

// readTail is the last max bytes of f, marked as tailBuffer marks a cut.
func readTail(f *os.File, max int64) string {
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off, mark := int64(0), ""
	if st.Size() > max {
		off, mark = st.Size()-max, "[...truncated...]\n"
	}
	b := make([]byte, st.Size()-off)
	n, _ := f.ReadAt(b, off)
	return mark + string(b[:n])
}

// pkgDir is the agent's directory of the runs: their output files and the
// release packages downloaded for them. It is root's alone (0700), in the
// agent's own directory: a run, root, writes there what root's tools print,
// and the agent reads it back.
func (s *Server) pkgDir() string { return filepath.Join(s.cfg.AgentStateDir(), "pkg") }

// openPkgDir opens pkgDir, made when missing.
func (s *Server) openPkgDir() (int, error) { return ensureRootDirAs(s.pkgDir(), 0o700, s.rootUID) }

// createPkgOutput makes a run's output file, root's 0600, open for reading
// and writing: a new file or none (O_EXCL), and no link followed should one
// be in its place (O_NOFOLLOW). It returns pkgDir opened too, for the
// file's removal.
func (s *Server) createPkgOutput(run string) (int, *os.File, error) {
	dfd, err := s.openPkgDir()
	if err != nil {
		return -1, nil, err
	}
	name := run + pkgOutputSuffix
	p := filepath.Join(s.pkgDir(), name)
	fd, err := unix.Openat(dfd, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		unix.Close(dfd) //nolint:errcheck // a directory opened for reading
		return -1, nil, &os.PathError{Op: "create", Path: p, Err: err}
	}
	return dfd, os.NewFile(uintptr(fd), p), nil
}

// pkgWaitPoll paces the wait for runs the agent did not start; tests shorten
// it.
var pkgWaitPoll = 5 * time.Second

// waitPkgRuns waits until no unit of the patterns runs: runs of the package
// manager an earlier agent started, which went on when it stopped (and its
// pkgMu went with it), and — for the agent's own runs — a panel install
// under way (monopanel-update.service), whose package manager run of its own
// the next one would meet. apt-get would wait ten minutes for the dpkg lock
// and then fail (DPkg::Lock::Timeout), dnf waits for its lock as long as it
// takes: neither joins the run that holds it, but one that waits here
// starts once the other is over instead of failing on the lock. It returns
// when ctx ends, which the caller checks. Once no run is going, the output
// files runs an earlier agent left are taken care of (reapPkgOutputs). The
// caller holds pkgMu, so none of the agent's own runs is going.
func (s *Server) waitPkgRuns(ctx context.Context, patterns ...string) {
	units, err := connectPkgUnits(ctx)
	if err != nil {
		s.log.Warn("systemd unreachable: runs of the package manager the agent did not start are not waited for, only its lock keeps them apart", "err", err)
		return
	}
	defer units.close()
	var waited []string
	for {
		names, err := units.running(ctx, patterns...)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("runs of the package manager the agent did not start are not waited for, only its lock keeps them apart", "err", err)
			}
			return
		}
		if len(names) == 0 {
			if waited != nil {
				s.log.Info("the package manager's turn: what it waited for is over", "units", waited)
			}
			s.reapPkgOutputs(nil)
			return
		}
		if waited == nil {
			waited = names
			s.log.Info("the package manager waits for a run the agent did not start", "units", names)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pkgWaitPoll):
		}
	}
}

// indexLockWait bounds the wait for another index update: as long as apt
// waits for dpkg's lock (DPkg::Lock::Timeout). Tests shorten it.
var indexLockWait = 10 * time.Minute

// waitIndexLock runs an index update again, in the same unit, while it
// failed only on the lock another one holds (PackageManager.IndexLocked):
// apt-get update gives up at once on the lock of the lists — apt-daily's
// apt-get update has it, which its timer starts at boot when it missed its
// time, and the start of the API moving the host's PHP repository then would
// fail on it, to try again at the next start only. The update runs again once
// the lock is free (awaitIndexLock): each run that fails on it is a start of
// the unit that systemd logs as failed, at priority err. It gives up after
// indexLockWait, and when the request has ended: nobody waits for the run
// then. out and err are the run's; the last run's are returned. The caller
// holds pkgMu.
func (s *Server) waitIndexLock(ctx, rctx context.Context, pm osprofile.PackageManager, run string, argv []string, out string, err error) (string, error) {
	until := time.Now().Add(indexLockWait)
	lock, warned := pm.IndexLockFile(), false
	for waited := false; err != nil && pm.IndexLocked(out) && time.Now().Before(until); waited = true {
		if !waited {
			s.log.Info("the index update waits for another one, which holds the lock of the index", "unit", pkgUnit(run), "lock", lock, "limit", indexLockWait)
		}
		if !s.awaitIndexLock(ctx, lock, until, &warned) {
			return out, err
		}
		out, err = runPackageManager(s, rctx, run, pm.Env(), argv...)
	}
	return out, err
}

// awaitIndexLock waits, a pause (pkgWaitPoll) at a time, until no other
// process holds the lock of the index at path, which the agent asks the lock
// itself (indexLockHeld) rather than apt; or until it cannot tell — logged
// once (warned), the pause alone paces the runs then. It is false when the
// request ended first, or the wait's limit came while the lock was held.
func (s *Server) awaitIndexLock(ctx context.Context, path string, until time.Time, warned *bool) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pkgWaitPoll):
		}
		held, err := indexLockHeld(path)
		if err != nil {
			if !*warned {
				*warned = true
				s.log.Warn("the index update cannot tell whether the lock of the index is still held: it runs again after every pause", "lock", path, "err", err)
			}
			return true
		}
		if !held {
			return true
		}
		if !time.Now().Before(until) {
			return false
		}
	}
}

// reapPkgOutputs removes the output files of the runs that are over, all but
// those of running. A run's file is gone when the run is over and the agent
// saw it end; one still there is of a run an earlier agent did not see end,
// and its end is logged first: the administrator learns from it how the run
// went, which the job that asked for it could not.
func (s *Server) reapPkgOutputs(running map[string]bool) {
	dfd, err := s.openPkgDir()
	if err != nil {
		return
	}
	defer unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	for _, name := range dirNames(dfd) {
		run, ok := strings.CutSuffix(name, pkgOutputSuffix)
		if !ok || !runIDRe.MatchString(run) || running[run] {
			continue
		}
		s.log.Warn("a run of the package manager went on after the agent that started it stopped; it is over, the end of its output follows (its result: journalctl -u "+pkgUnit(run)+")",
			"unit", pkgUnit(run), "output", tailAt(dfd, name, 4096))
		_ = unix.Unlinkat(dfd, name, 0)
	}
}

// tailAt is the last max bytes of the file name in the directory dfd, ""
// when it is no plain file.
func tailAt(dfd int, name string, max int64) string {
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return readTail(f, max)
}

// reapPkgRuns takes care, at the agent's start, of what runs of an earlier
// agent left in pkgDir: the output files of the runs that are over
// (reapPkgOutputs), and the release packages downloaded for them once no
// run is going — a run still going installs its release package from there
// and may not have handed it to dpkg yet. The file of a run still going
// stays until that run is over: the agent's next run of the package
// manager, which waits for it, or its next start removes it; the release
// packages stay until a start finds no run going (the agent's own runs
// remove theirs, pkgArgs).
func (s *Server) reapPkgRuns(ctx context.Context) {
	dfd, err := s.openPkgDir()
	if err != nil {
		s.log.Warn("the package manager's directory", "dir", s.pkgDir(), "err", err)
		return
	}
	names := dirNames(dfd)
	unix.Close(dfd) //nolint:errcheck // a directory opened for reading
	var releases []string
	outputs := false
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, releaseDirPrefix):
			releases = append(releases, name)
		case strings.HasSuffix(name, pkgOutputSuffix):
			outputs = true
		}
	}
	if !outputs && len(releases) == 0 {
		return
	}
	units, err := connectPkgUnits(ctx)
	if err != nil {
		s.log.Warn("systemd unreachable: what earlier runs of the package manager left stays until a later start", "err", err)
		return
	}
	defer units.close()
	names, err = units.running(ctx, pkgUnitPattern)
	if err != nil {
		s.log.Warn("what earlier runs of the package manager left stays until a later start", "err", err)
		return
	}
	running := map[string]bool{}
	for _, n := range names {
		if run, ok := strings.CutSuffix(strings.TrimPrefix(n, pkgUnitPrefix), ".service"); ok {
			running[run] = true
			s.log.Info("a run of the package manager an earlier agent started goes on; the agent's runs wait for it", "unit", n)
		}
	}
	if len(names) == 0 {
		for _, name := range releases {
			// root's directory in root's 0700 one: os.RemoveAll follows no
			// link it meets
			if err := os.RemoveAll(filepath.Join(s.pkgDir(), name)); err != nil {
				s.log.Warn("a release package an earlier agent downloaded", "dir", filepath.Join(s.pkgDir(), name), "err", err)
			}
		}
	}
	s.reapPkgOutputs(running)
}

// runPkgTool runs a tool that is the package manager (planDnf's install)
// the way /v1/pkg runs its changes: under pkgMu, after the runs the agent
// did not start, in the run's unit, and on to the end when the request goes
// away — within the tool's time limit, ctx's deadline as the call began,
// counted from the run's own start. The wait for the package manager's turn
// (pkgMu, a run an earlier agent left going, a panel install) comes out of
// the request, not out of the run: what that wait left of the limit would
// have the agent stop dnf in the middle of its transaction. The output goes
// to out; the exit status is returned.
func (s *Server) runPkgTool(ctx context.Context, run string, argv []string, out io.Writer) (int, error) {
	if run == "" {
		run = newRunID() // an API older than the agent names none
	}
	limit := pkgRunLimit
	if d, ok := ctx.Deadline(); ok {
		limit = min(time.Until(d), pkgRunLimit)
	}
	s.pkgMu.Lock()
	defer s.pkgMu.Unlock()
	s.waitPkgRuns(ctx, pkgUnitPattern, updateUnit)
	if err := ctx.Err(); err != nil {
		return 0, &Error{Message: argv[0] + " not started: the request ended while it waited for the package manager", Output: err.Error()}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
	defer cancel()
	s.log.Info("package manager", "command", argv, "unit", pkgUnit(run))
	res, err := s.runPackageUnit(rctx, run, s.profile.Packages().Env(), argv)
	if err != nil {
		return 0, err
	}
	_, _ = io.WriteString(out, res.output)
	if res.failed() {
		fmt.Fprintf(out, "\n[%s: %s]\n", res.unit, res.why)
	}
	return res.code, nil
}
