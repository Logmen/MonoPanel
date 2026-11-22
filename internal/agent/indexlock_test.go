package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/systemd"
)

// lockHolderEnv makes the test binary another process that holds a lock of
// the index as apt does (holdLock), on the file it names.
const lockHolderEnv = "MP_TEST_LOCK_HOLDER"

func init() {
	if path := os.Getenv(lockHolderEnv); path != "" {
		os.Exit(holdLock(path))
	}
	// No test asks the host's own lock of apt's lists: it is free unless the
	// test says otherwise (withIndexLock).
	indexLockHeld = func(string) (bool, error) { return false, nil }
}

// withIndexLock has the agent's looks at the lock of the index answered by
// held.
func withIndexLock(t *testing.T, held func(path string) (bool, error)) {
	t.Helper()
	old := indexLockHeld
	indexLockHeld = held
	t.Cleanup(func() { indexLockHeld = old })
}

// holdLock is another process that holds the lock at path as apt's GetLock
// takes it, a POSIX write lock over the whole file, until its input ends:
// "locked" says it has it.
func holdLock(path string) int {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: unix.SEEK_SET}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLKW, &lk); err != nil {
		fmt.Println("lock:", err)
		return 1
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// listsLocked is apt-get update on the lock of the lists another apt-get
// update holds (apt 2.4 to 3.2 alike).
const listsLocked = "Reading package lists...\nE: Could not get lock /var/lib/apt/lists/lock. It is held by process 1139 (python3)\nE: Unable to lock directory /var/lib/apt/lists/\n"

// The wait for another index update has an end: a lock held longer than
// indexLockWait fails the update with apt's own words, as before the wait —
// the first run's, the lock held all along. When the agent cannot tell
// whether the lock is still held, the update runs again after every pause
// until then, and that is logged once.
func TestIndexUpdateLockWaitHasAnEnd(t *testing.T) {
	const locked = "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 9093 (apt-get)\nE: Unable to lock directory /var/lib/apt/lists/\n"
	oldRun, oldPoll, oldWait := runPackageManager, pkgWaitPoll, indexLockWait
	pkgWaitPoll, indexLockWait = time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { runPackageManager, pkgWaitPoll, indexLockWait = oldRun, oldPoll, oldWait })
	runs := 0
	runPackageManager = func(*Server, context.Context, string, []string, ...string) (string, error) {
		runs++
		return locked, errors.New("exit status 100 (monopanel-pkg-0123456789abcdef.service)")
	}
	deb := withTrees(t, policyServer(t, debian13))
	var logs bytes.Buffer
	deb.log = slog.New(slog.NewTextHandler(&logs, nil))
	const unsure = "cannot tell whether the lock of the index is still held"
	for _, c := range []struct {
		name string
		held func(string) (bool, error)
		runs func(int) bool
		logs int
	}{
		{"held all along", func(string) (bool, error) { return true, nil }, func(n int) bool { return n == 1 }, 0},
		{"cannot tell", func(string) (bool, error) {
			return false, errors.New("open /var/lib/apt/lists/lock: permission denied")
		}, func(n int) bool { return n >= 2 }, 1},
	} {
		withIndexLock(t, c.held)
		runs = 0
		logs.Reset()
		// The request ends long after the wait.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := time.Now()
		_, err := deb.pkg(ctx, &PkgRequest{Action: "update-index"})
		took := time.Since(start)
		cancel()
		var ae *Error
		if !errors.As(err, &ae) || ae.Output != locked || !c.runs(runs) {
			t.Fatalf("%s: a lock held past the wait: %d runs, %v", c.name, runs, err)
		}
		if took > 10*time.Second {
			t.Fatalf("%s: the wait took %s", c.name, took)
		}
		if n := strings.Count(logs.String(), unsure); n != c.logs {
			t.Errorf("%s: %q logged %d times:\n%s", c.name, unsure, n, logs.String())
		}
	}
}

// While another apt-get update holds the lock of the lists, the agent asks
// the lock, not apt: each run that fails on it is a start of the run's unit
// that systemd logs as failed ("Failed to start MonoPanel package manager:
// …", priority err), and the update runs again once the lock is free — the
// first run and the one after it, however long the other update takes.
func TestIndexUpdateRunsAgainOnceTheLockIsFree(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	f := withUnits(t, deb)
	// The other update holds the lock for the first five looks at it, the
	// agent's and apt's alike.
	looks := 0
	held := func() bool { looks++; return looks <= 5 }
	var asked []string
	withIndexLock(t, func(path string) (bool, error) {
		asked = append(asked, path)
		return held(), nil
	})
	f.play = func(systemd.Oneshot) (string, string, systemd.Exit) {
		if held() {
			return listsLocked, "failed", systemd.Exit{Code: 1, Status: 100, Result: "exit-code"}
		}
		return "Hit:1 http://deb.debian.org/debian trixie InRelease\nReading package lists...\n", "done", systemd.Exit{Code: 1, Result: "success"}
	}
	resp, err := deb.pkg(context.Background(), &PkgRequest{Action: "update-index", Run: testRun})
	if err != nil || !strings.Contains(resp.Output, "Hit:1") {
		t.Fatalf("an index update behind another one's lock: %v %+v", err, resp)
	}
	if len(f.started) != 2 || len(f.reset) != 1 {
		t.Fatalf("%s started %d times, %d of them failed: want the first run and the one once the lock was free", pkgUnit(testRun), len(f.started), len(f.reset))
	}
	if len(asked) != 5 || slices.ContainsFunc(asked, func(p string) bool { return p != "/var/lib/apt/lists/lock" }) {
		t.Fatalf("the looks at the lock: %v", asked)
	}
}

// The runs after the first go its way (runPackageManager, runPackageUnit):
// in the request's own unit, which systemd starts again only once the failed
// run was reset (a failed transient unit keeps its name until then); with
// the package manager's environment — LANGUAGE= among it, or apt translated
// by the host's LANGUAGE would hide the lock's line —; and within the run's
// own limit, pkgRunLimit from the first run, not what is left of the
// request's.
func TestIndexUpdateRunsAgainInTheSameUnit(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	f := withUnits(t, deb)
	runs := 0
	f.play = func(o systemd.Oneshot) (string, string, systemd.Exit) {
		// f.mu is held: f.reset are the units systemd reset so far.
		if len(f.reset) < runs {
			t.Errorf("run %d of %s started before systemd reset the failed one (reset: %v): systemd refuses a unit name still loaded", runs+1, o.Unit, f.reset)
		}
		runs++
		if runs < 3 {
			// free at the agent's look, taken again before apt's
			return listsLocked, "failed", systemd.Exit{Code: 1, Status: 100, Result: "exit-code"}
		}
		return "Hit:1 http://deb.debian.org/debian trixie InRelease\nReading package lists...\n", "done", systemd.Exit{Code: 1, Result: "success"}
	}
	// A job's request: its end an hour away.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	pm := deb.profile.Packages()
	resp, err := deb.pkg(ctx, &PkgRequest{Action: "update-index", Run: testRun})
	if err != nil || !strings.Contains(resp.Output, "Hit:1") {
		t.Fatalf("an index update behind the lists' lock: %v %+v", err, resp)
	}
	if len(f.started) != 3 || len(f.reset) != 2 {
		t.Fatalf("runs: %d, reset: %v", len(f.started), f.reset)
	}
	for i, o := range f.started {
		if o.Unit != pkgUnit(testRun) {
			t.Errorf("run %d in %s, not in the request's unit %s", i+1, o.Unit, pkgUnit(testRun))
		}
		if !slices.Equal(o.Env, pm.Env()) || !slices.Contains(o.Env, "LANGUAGE=") {
			t.Errorf("run %d's environment %v, want the package manager's %v", i+1, o.Env, pm.Env())
		}
		if o.Limit <= time.Hour+pkgStopMargin || o.Limit > pkgRunLimit+pkgStopMargin {
			t.Errorf("run %d's limit %s: not its own (%s from the first run) but what is left of the request's hour", i+1, o.Limit, pkgRunLimit)
		}
	}
}

// lockHeld finds the holder of a lock file as apt's GetLock meets it:
// another process's POSIX write lock over the whole file — what apt-get
// update holds on /var/lib/apt/lists/lock while it runs —, or a lock of an
// open file description, holds it; a file nobody locks, or one that is not
// there (apt makes it), is free; a directory or a link is no lock file the
// agent asks.
func TestLockHeld(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "lock")
	if held, err := lockHeld(lock); held || err != nil {
		t.Fatalf("a lock file that is not there: %v %v", held, err)
	}
	if err := os.WriteFile(lock, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if held, err := lockHeld(lock); held || err != nil {
		t.Fatalf("a lock file nobody holds: %v %v", held, err)
	}

	// Another process locks it as apt does, and lets it go.
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+lock)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); line != "locked\n" {
		t.Fatalf("the other process: %q %v", line, err)
	}
	if held, err := lockHeld(lock); !held || err != nil {
		t.Fatalf("a lock file another process holds: %v %v", held, err)
	}
	_ = in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the other process: %v", err)
	}
	if held, err := lockHeld(lock); held || err != nil {
		t.Fatalf("a lock file its holder let go: %v %v", held, err)
	}

	// The lock of an open file description holds it as well.
	fd, err := unix.Open(lock, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd) //nolint:errcheck // the test's own descriptor
	ofd := unix.Flock_t{Type: unix.F_WRLCK, Whence: unix.SEEK_SET}
	if err := unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, &ofd); err != nil {
		t.Fatal(err)
	}
	if held, err := lockHeld(lock); !held || err != nil {
		t.Fatalf("a lock file an open file description locks: %v %v", held, err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(lock, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, link} {
		if held, err := lockHeld(p); err == nil {
			t.Errorf("%s taken for a lock file: held %v", p, held)
		}
	}
}
