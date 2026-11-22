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

	// The agent may still be starting; a few tries cover the unit ordering.
	go func() {
		for attempt := 0; attempt < 6; attempt++ {
			if _, err := s.agent.Pkg(ctx, "query", "nginx"); err == nil {
				s.refreshDefaultServers(ctx)
				s.refreshDBConfig(ctx)
				s.refreshPHPCLIInis(ctx)
				s.refreshIdlePHPPools(ctx)
				s.refreshSiteLogrotate(ctx)
				// The label rule for the site log tree before anything is
				// made in it, then the sites switch to it once, before the
				// jails list its files.
				s.refreshSELinuxSiteLogs(ctx)
				s.moveSiteLogs(ctx)
				// Sites of 0.8.19 in Apache mode move off the shared Apache
				// to their accounts' own, once; an account's Apache that ran
				// before its backend secret demands it, once (queued after
				// the move, the same lock).
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
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

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
			}
		}
	})
	return g.Wait()
}
