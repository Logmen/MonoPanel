package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/store"
)

// The web console runs the same mp binary an administrator would run over
// ssh, as a client of this API: a one-off token carries the caller's own
// rights and dies with the command, the output streams back as it comes.

// consoleBinary is the mp binary to run — the panel itself.
var consoleBinary = func() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "/usr/bin/monopanel"
}

// consoleForbidden are the commands that make no sense from a browser: the
// daemons, the privilege-dropping helper, the installer.
var consoleForbidden = map[string]bool{"api": true, "agent": true, "helper": true, "fsop": true, "setup": true, "update-run": true}

type consoleInput struct {
	Body apitypes.ConsoleRequest
}

// consoleServerURL is where the command reaches this API: the loopback side
// of the HTTPS listener.
func (s *Server) consoleServerURL() string {
	listen := s.cfg.Web.Listen
	if listen == "" {
		listen = ":8443"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		port = "8443"
	}
	return "https://127.0.0.1:" + port
}

func (s *Server) registerConsole() {
	huma.Register(s.api, huma.Operation{
		OperationID: "system-console", Method: http.MethodPost, Path: "/system/console", Summary: "Run an mp command with the caller's rights and stream its output", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *consoleInput) (*huma.StreamResponse, error) {
		p := principalFrom(ctx)
		if p.UserID == 0 {
			return nil, huma.Error422UnprocessableEntity("the console runs on behalf of a panel account")
		}
		args := in.Body.Args
		if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
			return nil, huma.Error422UnprocessableEntity("a command is required, e.g. doctor")
		}
		if consoleForbidden[args[0]] {
			return nil, huma.Error422UnprocessableEntity("mp " + args[0] + " is not for the console")
		}
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				return nil, huma.Error422UnprocessableEntity("invalid argument")
			}
			for _, own := range []string{"--server", "--token", "--config", "--insecure"} {
				if a == own || strings.HasPrefix(a, own+"=") {
					return nil, huma.Error422UnprocessableEntity(own + " is set by the console itself")
				}
			}
		}
		// a token for this one command, gone when it ends
		plain, err := auth.NewToken(32)
		if err != nil {
			return nil, err
		}
		exp := time.Now().Add(30 * time.Minute)
		rec := &store.APIToken{UserID: p.UserID, Name: "console " + time.Now().UTC().Format("15:04:05"), Hash: auth.HashToken(plain), ExpiresAt: &exp}
		if err := s.db.CreateAPIToken(ctx, rec); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "console.run", Target: strings.Join(args, " "), IP: requestInfo(ctx).IP})
		server := s.consoleServerURL()
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer s.db.DeleteAPIToken(context.Background(), rec.ID, p.UserID) //nolint:errcheck // it expires on its own anyway
			hctx.SetHeader("Content-Type", "text/plain; charset=utf-8")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetHeader("Cache-Control", "no-store")
			out := &flushWriter{w: hctx.BodyWriter()}
			cctx, cancel := context.WithTimeout(hctx.Context(), 30*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(cctx, consoleBinary(), append(append([]string{}, args...), "--server", server, "--token", plain, "--insecure")...)
			cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "NO_COLOR=1", "TERM=dumb", "HOME=" + s.cfg.DataDir}
			cmd.Stdout, cmd.Stderr = out, out
			cmd.WaitDelay = 5 * time.Second
			fmt.Fprintf(out, "$ mp %s\n", strings.Join(args, " "))
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				code = ee.ExitCode()
			case err != nil:
				fmt.Fprintf(out, "console: %v\n", err)
				code = 1
			}
			fmt.Fprintf(out, "\n[exit %d]\n", code)
		}}, nil
	})
}

// flushWriter pushes every write to the browser: the command's progress
// lines are the point of a console.
type flushWriter struct {
	w io.Writer
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
