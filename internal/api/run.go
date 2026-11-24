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

// Listeners are what the API serves on: the HTTPS listener — the socket
// systemd passed (onSocket) or one of the process's own — and the local unix
// socket. Listen takes them, Serve serves them.
type Listeners struct {
	tcp      net.Listener
	onSocket bool
	unix     net.Listener
	sock     string
}

// Listen takes what the API serves on before anything else of a start
// happens: the sockets systemd passed, the certificate, the local unix
// socket and, last, the HTTPS listener, which may stop monopanel-api.socket
// on the way. A start that fails here — the port of web.listen held by
// another process, an unreadable TLS key, an api.sock the API cannot
// replace — leaves nothing open and changes nothing on the host: the job
// workers and the start-up refresh start only once Listen has succeeded
// (Serve, and the daemon between the two), so that the process can exit at
// once and systemd start it again, instead of running on without its port
// while the workers and the refresh change the host.
func (s *Server) Listen(ctx context.Context) (_ *Listeners, err error) {
	// The sockets systemd passed come first: activation unsets LISTEN_FDS
	// and marks them close-on-exec, so that no child of this process (the
	// console's mp) inherits the panel's port.
	passed, aerr := s.activation()
	if aerr != nil {
		s.log.Warn("the sockets systemd passed", "err", aerr)
	}
	defer func() {
		// The process's copies only: PID 1 keeps the socket bound with its
		// own, and starts the API again on the next connection.
		if err != nil {
			for _, p := range passed {
				if p != nil {
					_ = p.Close()
				}
			}
		}
	}()
	if err := s.tls.Reload(); err != nil {
		return nil, err
	}
	sock := s.cfg.APISocket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o771); err != nil {
		return nil, err
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
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
	tcp, onSocket, err := s.httpsListener(ctx, passed)
	if err != nil {
		// Closing it removes the socket file: mp gets "no such file" rather
		// than a socket nobody serves.
		_ = ln.Close()
		return nil, err
	}
	return &Listeners{tcp: tcp, onSocket: onSocket, unix: ln, sock: sock}, nil
}

// Serve serves HTTPS on what Listen took and plain HTTP on the local unix
// socket until ctx is cancelled, and runs the start-up refresh and the
// loops beside them (certificates, metrics, webhooks, updates, the daily
// upgrade, the hourly tick). It returns when ctx ends or a server fails;
// either way everything it started stops.
func (s *Server) Serve(ctx context.Context, l *Listeners) error {
	// Before the first request: a forged migration token must not get one.
	s.revokeForgedMigrationTokens(ctx)
	tlsSrv := &http.Server{
		Addr:              s.cfg.Web.Listen,
		Handler:           s.Handler(),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.tls.GetCertificate, NextProtos: []string{"h2", "http/1.1"}},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	unixSrv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext, ReadHeaderTimeout: 10 * time.Second}
	tcp, sock := l.tcp, l.sock
	s.listenAddr.Store(tcp.Addr())
	s.listenV6Only.Store(v6Only(tcp))
	from := "itself"
	if l.onSocket {
		s.listenVia.Store(viaSocket)
		from = s.cfg.APISocketUnit()
	} else {
		s.listenVia.Store(viaSelf)
	}
	defer s.holdPort(tcp)()
	g, gctx := errgroup.WithContext(ctx)
	// The listeners exist: the host may change now. The refresh ends with
	// the servers, a failed one as well.
	go s.startupRefresh(gctx)
	g.Go(func() error {
		s.log.Info("api listening", "https", tcp.Addr().String(), "from", from, "socket", sock)
		if err := tlsSrv.ServeTLS(tcp, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		if err := unixSrv.Serve(&peercred.Listener{Listener: l.unix}); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		s.upgradeLoop(gctx)
		return nil
	})
	g.Go(func() error {
		// phpMyAdmin's temporary accounts end with their sessions: a
		// sign-out is picked up within ten minutes.
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-t.C:
				if s.refreshed.Load() {
					s.pmaSweep(gctx)
				}
			}
		}
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
				// An expired certificate of the mail name has no event of
				// its own.
				s.webmailFollowsCertificate(gctx)
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
	// start: twice what the agent itself may take before it listens, about
	// three minutes — its probes of the backups' sandbox (nosymfollow, and a
	// private /etc where Landlock is), 30 s each at most; taking over the
	// mount units of shared folders an older panel left, a minute at most
	// (each mount checked with its own 10 s budget, then the units removed);
	// clearing up after the runs of the package manager an earlier agent
	// started, a minute at most. The rest of its start is quick work on its
	// own directories, and the restic self-test runs beside the socket, not
	// before it. A job claimed after that fails on the agent as it would at
	// any later time.
	jobsAgentWaitDefault = 6 * time.Minute
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
	// Once: the pattern grants of older databases name them literally.
	s.refreshLiteralGrants(ctx)
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
	// A host whose mail server an older panel set up gets the mail policy:
	// its unit, its file, main.cf with it before opendkim (mail.apply).
	s.moveMailPolicy(ctx)
	// Once on EL: smtpd reads the mail server's self-signed certificate.
	s.refreshSELinuxMailTLS(ctx)
	s.refreshRepoSchemes(ctx)
	// phpMyAdmin's expired temporary accounts, and the move to the pinned
	// release.
	s.refreshPHPMyAdmin(ctx)
	// Last: it may wait on packages.sury.org and two index updates.
	s.switchPHPRepoToSury(ctx)
	s.refreshed.Store(true)
}

// retryMoves queues again, once an hour after the start-up refresh ran, the
// moves of an upgrade that did not finish and whose next try is due: a cause
// fixed meanwhile — a PHP branch installed again, another site's broken file
// that failed nginx -t for the whole host, a semanage that failed at the
// start — needs no restart of the panel. The SELinux label rule of the log
// tree comes first, as at start: until it is in place no site's logs move.
func (s *Server) retryMoves(ctx context.Context) {
	if !s.refreshed.Load() {
		return
	}
	s.refreshSELinuxSiteLogs(ctx)
	s.moveSiteLogs(ctx)
	s.refreshApacheInstances(ctx)
	s.moveMailPolicy(ctx)
	// A write of the mail policy that failed is done again.
	s.refreshMailPolicy(ctx)
}
