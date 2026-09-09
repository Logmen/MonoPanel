package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/osprofile"
	"monopanel/internal/peercred"
)

// Server executes privileged operations for the API process.
type Server struct {
	cfg     config.Config
	profile osprofile.Profile
	log     *slog.Logger
	pkgMu   sync.Mutex
}

// NewServer builds an agent.
func NewServer(cfg config.Config, profile osprofile.Profile, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, profile: profile, log: log}
}

// Handler returns the RPC router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/ping", handle(s.ping))
	r.Post("/v1/system/info", handle(s.systemInfo))
	r.Post("/v1/config/apply", handle(s.applyConfigSet))
	r.Post("/v1/group/ensure", handle(s.ensureGroup))
	r.Post("/v1/user/ensure", handle(s.ensureUnixUser))
	r.Post("/v1/dirs/ensure", handle(s.ensureDirs))
	r.Post("/v1/file/ensure", handle(s.ensureFile))
	r.Post("/v1/symlink/ensure", handle(s.ensureSymlink))
	r.Post("/v1/acl/set", handle(s.setACL))
	r.Post("/v1/paths/remove", handle(s.removePaths))
	r.Post("/v1/apache/ctl", handle(s.apacheCtl))
	r.Post("/v1/tool", handle(s.tool))
	r.Post("/v1/stat", handle(s.stat))
	r.Post("/v1/dir/list", handle(s.listDir))
	r.Post("/v1/file/read", handle(s.readFile))
	r.Post("/v1/user/password", handle(s.setUnixPassword))
	r.Post("/v1/user/remove", handle(s.removeUnixUser))
	r.Post("/v1/runas", handle(s.runAsUser))
	r.Post("/v1/service", handle(s.service))
	r.Post("/v1/pkg", handle(s.pkg))
	r.Post("/v1/panel/install", handle(s.installPanel))
	r.Post("/v1/chown", handle(s.chown))
	r.Post("/v1/user/shadow", handle(s.unixShadow))
	// Потоки идут мимо handle(): у них тело — это данные, а не JSON.
	r.Post("/v1/stream/out", s.streamOut)
	r.Post("/v1/stream/in", s.streamIn)
	return r
}

func handle[Req, Resp any](fn func(context.Context, *Req) (*Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, &Error{Status: http.StatusBadRequest, Message: "bad request: " + err.Error()})
				return
			}
		}
		resp, err := fn(r.Context(), &req)
		if err != nil {
			var ae *Error
			if !errors.As(err, &ae) {
				ae = &Error{Message: err.Error()}
			}
			if ae.Status == 0 {
				ae.Status = http.StatusInternalServerError
			}
			writeErr(w, ae)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func writeErr(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

func (s *Server) ping(ctx context.Context, _ *struct{}) (*PingResponse, error) {
	host, _ := os.Hostname()
	resp := &PingResponse{OK: true, Version: buildinfo.Version, Hostname: host, PeerUID: -1}
	if c, ok := peercred.FromContext(ctx); ok {
		resp.PeerUID = int(c.UID)
	}
	return resp, nil
}

// ListenAndServe serves on the agent socket until ctx is cancelled. The socket
// is root:<service group> 0660 and only root or the service user may connect.
func (s *Server) ListenAndServe(ctx context.Context) error {
	sock := s.cfg.AgentSocket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o750); err != nil {
		return err
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o660); err != nil {
		return err
	}
	if gid, err := lookupGID(s.cfg.ServiceGroup); err == nil {
		_ = os.Chown(sock, 0, gid)
	} else {
		s.log.Warn("service group missing; only root may connect", "group", s.cfg.ServiceGroup)
	}
	serviceUID := -1
	if uid, err := lookupUID(s.cfg.ServiceUser); err == nil {
		serviceUID = uid
	}
	allow := func(c *peercred.Cred) bool {
		return c.UID == 0 || (serviceUID >= 0 && int(c.UID) == serviceUID)
	}
	srv := &http.Server{Handler: s.Handler(), ConnContext: peercred.ConnContext, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("agent listening", "socket", sock, "family", s.profile.Family(), "os", s.profile.Release().PrettyName)
	err = srv.Serve(&peercred.Listener{Listener: ln, Allow: allow})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
