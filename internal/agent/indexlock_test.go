package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The wait for another index update has an end: a lock held longer than
// indexLockWait fails the update with apt's own words, as before the wait.
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
	start := time.Now()
	_, err := deb.pkg(context.Background(), &PkgRequest{Action: "update-index"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Output != locked || runs < 2 {
		t.Fatalf("a lock held past the wait: %d runs, %v", runs, err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the wait took %s", took)
	}
}
