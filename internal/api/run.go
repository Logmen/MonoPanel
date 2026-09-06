package api

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"

	"monopanel/internal/peercred"
)

// Run serves HTTPS on cfg.Web.Listen and plain HTTP on the local unix socket
// until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if err := s.tls.Reload(); err != nil {
		return err
	}
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
	// Anyone local may connect; the peer uid decides what they can do.
	_ = os.Chmod(sock, 0o666)
	unixSrv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext, ReadHeaderTimeout: 10 * time.Second}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		s.log.Info("api listening", "https", s.cfg.Web.Listen, "socket", sock)
		if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
