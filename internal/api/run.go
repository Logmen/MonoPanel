package api

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"monopanel/internal/osprofile"
	"monopanel/internal/peercred"
)

// Run serves HTTPS on cfg.Web.Listen — on the socket systemd passed when it
// is there (httpsListener) — and plain HTTP on the local unix socket until
// ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// The sockets systemd passed come first: activation unsets LISTEN_FDS
	// and marks them close-on-exec, so that no child of this process (the
	// console's mp) inherits the panel's port.
	passed, err := s.activation()
	if err != nil {
		s.log.Warn("the sockets systemd passed", "err", err)
	}
	if err := s.tls.Reload(); err != nil {
		return err
	}
	s.revokeForgedMigrationTokens(ctx)
	tlsSrv := &http.Server{
		Addr:              s.cfg.Web.Listen,
		Handler:           s.Handler(),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.tls.GetCertificate, NextProtos: []string{"h2", "http/1.1"}},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	sock := s.cfg.APISocket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o771); err != nil {
		return err
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	// Only root and the CLI group — the service user and accounts with shell
	// access — may connect; the peer uid still decides what they can do. Open
	// to everyone, the socket would let a hacked site drive its own account.
	if g, err := user.LookupGroup(s.cfg.CLIGroup); err != nil {
		s.log.Warn("api socket: CLI group is missing, only root can use mp locally", "group", s.cfg.CLIGroup, "err", err)
	} else if gid, err := strconv.Atoi(g.Gid); err == nil {
		if err := os.Chown(sock, -1, gid); err != nil {
			s.log.Warn("api socket: cannot hand it to the CLI group (is the service user a member?), only root can use mp locally", "group", s.cfg.CLIGroup, "err", err)
		}
	}
	_ = os.Chmod(sock, 0o660)
	unixSrv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext, ReadHeaderTimeout: 10 * time.Second}

	go s.startupRefresh(ctx)

	tcp, onSocket, err := s.httpsListener(ctx, passed)
	if err != nil {
		return err
	}
	s.listenAddr.Store(tcp.Addr())
	s.listenV6Only.Store(v6Only(tcp))
	from := "itself"
	if onSocket {
		s.listenVia.Store(viaSocket)
		from = s.cfg.APISocketUnit()
	} else {
		s.listenVia.Store(viaSelf)
	}
	defer s.holdPort(tcp)()
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		s.log.Info("api listening", "https", tcp.Addr().String(), "from", from, "socket", sock)
		if err := tlsSrv.ServeTLS(tcp, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		if err := unixSrv.Serve(&peercred.Listener{Listener: ln}); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = tlsSrv.Shutdown(shutdownCtx)
		_ = unixSrv.Shutdown(shutdownCtx)
		return nil
	})
	g.Go(func() error {
		s.renewLoop(gctx)
		return nil
	})
	g.Go(func() error {
		(&sampler{s: s}).run(gctx)
		return nil
	})
	g.Go(func() error {
		s.webhookLoop(gctx)
		return nil
	})
	g.Go(func() error {
		s.updateLoop(gctx)
		return nil
	})
	g.Go(func() error {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-t.C:
				s.db.DeleteExpiredSessions(context.Background()) //nolint:errcheck // best effort; the caller reports the real failure
				s.scheduledBackups(gctx)
				s.retryMoves(gctx)
			}
		}
	})
	return g.Wait()
}

const (
	// agentWaitStepFirst is the first pause between two pings of an agent
	// that does not answer yet, agentWaitStepMax the longest; agentWaitNote
	// is how often a long wait is logged again.
	agentWaitStepFirst = time.Second
	agentWaitStepMax   = 15 * time.Second
	agentWaitNote      = 5 * time.Minute
	// jobsAgentWaitDefault is how long the job workers wait for the agent at
	// start: twice what the agent itself may take before it listens (the
	// nosymfollow probe, 30 s at most, and retiring unsafe mount units of
	// shared folders, a minute at most, a daemon-reload each). A job claimed
	// after that fails on the agent as it would at any later time.
	jobsAgentWaitDefault = 3 * time.Minute
)

// waitForAgent pings the agent until it answers, ctx ends or limit has
// passed (0: no limit), and tells whether it answers. systemd starts the API
// as soon as the agent's process runs, and the agent listens only after its
// own work at start, which may take a minute and more: what needs the agent
// waits for it here rather than failing on it. what names the waiter in the
// log, which says when the wait begins, every few minutes while it lasts,
// when the agent answers and when the waiter gives up.
func (s *Server) waitForAgent(ctx context.Context, what string, limit time.Duration) bool {
	start := time.Now()
	step := s.agentWaitStep
	if step <= 0 {
		step = agentWaitStepFirst
	}
	var note time.Duration
	for {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := s.agent.Ping(pctx)
		cancel()
		waited := time.Since(start)
		if err == nil {
			if note > 0 {
				s.log.Info(what+": the agent answers", "waited", waited.Round(time.Second))
			}
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		switch {
		case note == 0:
			s.log.Info(what+": waiting for the agent, which may still be at its own start", "socket", s.agent.Socket(), "err", err)
			note = agentWaitNote
		case waited >= note:
			s.log.Warn(what+": the agent still does not answer", "waited", waited.Round(time.Second), "err", err)
			note += agentWaitNote
		}
		pause := step
		if limit > 0 {
			if waited >= limit {
				s.log.Warn(what+": gave up waiting for the agent", "waited", waited.Round(time.Second), "err", err)
				return false
			}
			pause = min(pause, limit-waited)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pause):
		}
		step = min(step*2, agentWaitStepMax)
	}
}

// startupRefresh brings the host up to date at every start of the API: what
// an upgrade moves once (the site logs, the sites in Apache mode, the shared
// folders, the apps' environment) and what a package or an administrator
// may have changed since (default servers, rotation, jails, SELinux rules,
// repositories). Every step acts through the agent, so the refresh waits for
// it for as long as the API runs, and says so in the log. A refresh that did
// not run — the API stopped first — runs at the next start: each step is
// idempotent, and each move remembers what it finished, not what it queued.
// On an unsupported distribution (the generic profile) the panel changes
// nothing of the host by itself.
func (s *Server) startupRefresh(ctx context.Context) {
	if s.profile.Family() == osprofile.FamilyUnknown {
		s.log.Info("start-up refresh skipped: an unsupported distribution (generic profile), the panel changes nothing of the host by itself")
		return
	}
	if !s.waitForAgent(ctx, "start-up refresh", 0) {
		s.log.Warn("start-up refresh did not run: the API stopped before the agent answered; it runs at the next start")
		return
	}
	s.refreshDefaultServers(ctx)
	s.refreshDBConfig(ctx)
	s.refreshPHPCLIInis(ctx)
	s.refreshIdlePHPPools(ctx)
	s.refreshSiteLogrotate(ctx)
	// The label rule for the site log tree before anything is made in it,
	// then the sites switch to it, before the jails list its files.
	s.refreshSELinuxSiteLogs(ctx)
	s.moveSiteLogs(ctx)
	// Sites of 0.8.19 in Apache mode move off the shared Apache to their
	// accounts' own; an account's Apache that ran before its backend secret
	// demands it, once (queued after the move, the same lock).
	s.refreshApacheInstances(ctx)
	s.refreshApacheBackends(ctx)
	s.refreshFail2ban(ctx)
	s.refreshSELinuxModule(ctx)
	s.refreshSELinuxSSH(ctx)
	s.refreshSFTPHomes(ctx)
	// After the SFTP homes are root's again: folders move in.
	s.refreshShareMounts(ctx)
	s.refreshCLIGroup(ctx)
	s.moveAppEnvOutOfUnits(ctx)
	s.refreshComposerWrapper(ctx)
	s.refreshRepoSchemes(ctx)
	// Last: it may wait on keyserver.ubuntu.com.
	s.refreshOndrejKey(ctx)
	s.refreshed.Store(true)
}

// retryMoves queues again, once an hour after the start-up refresh ran, the
// moves of an upgrade that did not finish and whose next try is due: a cause
// fixed meanwhile — a PHP branch installed again, another site's broken file
// that failed nginx -t for the whole host — needs no restart of the panel.
func (s *Server) retryMoves(ctx context.Context) {
	if !s.refreshed.Load() {
		return
	}
	s.moveSiteLogs(ctx)
	s.refreshApacheInstances(ctx)
}
