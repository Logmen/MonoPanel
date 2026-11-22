package api

import (
	"context"
	"errors"
	"io"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/jobs"
)

// handOverTree gives an account what root unpacked into its tree before a
// failure. tar runs with --no-same-owner, so every member it makes is root's
// until the chown that follows a finished unpack; a broken one would leave
// files the account cannot remove or replace. The job's context may be what
// ended the unpack, so the chown runs on a context of its own.
func (s *Server) handOverTree(ctx context.Context, jc *jobs.Context, dir, owner, group string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	ch, err := s.agent.Chown(cctx, &agent.ChownRequest{Path: dir, Owner: owner, Group: group, Recursive: true})
	if jc == nil {
		return
	}
	switch {
	case err != nil:
		jc.Logf("warning: what was unpacked into %s before the failure stays root's: %v", dir, err)
	case ch.Changed > 0:
		jc.Logf("%d objects unpacked into %s before the failure handed to %s", ch.Changed, dir, owner)
	}
}

// readTrack keeps the first error of the reader under a stream: the agent's
// side of a stream that broke off says only that it did.
type readTrack struct {
	r   io.Reader
	err error
}

func (t *readTrack) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && t.err == nil {
		t.err = err
	}
	return n, err
}
