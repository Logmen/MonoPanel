package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A stream whose request names no limit — a migration's copy of a home, a
// database, a mail domain's Maildirs — runs for StreamLimit, which the API's
// limit of a migration counts in; one that names a limit runs for that.
func TestStreamPlanLimit(t *testing.T) {
	s := policyServer(t, debian13)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	for _, c := range []struct {
		seconds int
		want    time.Duration
	}{{0, StreamLimit}, {90, 90 * time.Second}} {
		req := &StreamRequest{Name: "tar", Args: []string{"--extract", "--file", "-", "--directory", home, "--no-same-owner", "--warning=no-timestamp"}, TimeoutSeconds: c.seconds}
		before := time.Now()
		plan, _, rctx, cancel, err := s.streamPlan(context.Background(), req, modeStreamIn)
		if err != nil {
			t.Fatal(err)
		}
		deadline, ok := rctx.Deadline()
		cancel()
		plan.close()
		if got := deadline.Sub(before); !ok || got < c.want || got > c.want+time.Minute {
			t.Errorf("TimeoutSeconds %d: the stream runs for %s (%v), want %s", c.seconds, got, ok, c.want)
		}
	}
	if StreamLimit != 6*time.Hour {
		t.Errorf("StreamLimit %s", StreamLimit)
	}
}
