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
	"golang.org/x/sys/unix"

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
	// panelMu serialises edits of config.yaml: each one reads the file,
	// changes some keys and writes it back.
	panelMu sync.Mutex
	// rootUID is the user whose directories root's walks trust as root's
	// own: 0 for the agent as it runs, the test's user in tests.
	rootUID int
	// unitDir is where /v1/units/* write (/etc/systemd/system).
	unitDir string
}

// NewServer builds an agent.
func NewServer(cfg config.Config, profile osprofile.Profile, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, profile: profile, log: log, rootUID: os.Geteuid(), unitDir: systemUnitDir}
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
	r.Post("/v1/sitelogs/ensure", handle(s.ensureSiteLogs))
	r.Post("/v1/sitelogs/remove", handle(s.removeSiteLogs))
	r.Post("/v1/symlink/ensure", handle(s.ensureSymlink))
	r.Post("/v1/acl/set", handle(s.setACL))
	r.Post("/v1/acl/site", handle(s.siteACL))
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
	r.Post("/v1/panel/config", handle(s.setPanelConfig))
	r.Post("/v1/chown", handle(s.chown))
	r.Post("/v1/user/shadow", handle(s.unixShadow))
	r.Post("/v1/units/apply", handle(s.applyUnit))
	r.Post("/v1/units/remove", handle(s.removeUnit))
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
	s.prepareDirs()
	sock := s.cfg.AgentSocket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o750); err != nil {
		return err
	}
	_ = os.Remove(sock)
	// The socket is made with its mode (the umask of the bind) and handed to
	// the group without following a symlink: the run directory is writable
	// by the service group, and a chmod or chown by name after the bind
	// could be pointed at /etc/shadow by swapping the socket for a link.
	old := unix.Umask(0o117)
	ln, err := net.Listen("unix", sock)
	unix.Umask(old)
	if err != nil {
		return err
	}
	if gid, err := lookupGID(s.cfg.ServiceGroup); err == nil {
		_ = os.Lchown(sock, 0, gid)
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

// prepareDirs gives the directories the agent shares with the API the owners
// and modes that keep root's entries in them root's, and makes its own. mp
// setup and the package do the same; an installation from before 0.8.20 is
// fixed up here, at the agent's start, before the API (which starts after
// the agent) can use the old modes:
//
//   - the run directory, root:<service group> 1771: the API makes its socket
//     there, and the sticky bit keeps it from renaming root's entries — the
//     agent's socket, php/, where the php-fpm masters make the pools'
//     sockets as root;
//   - the log directory, root:<service group> 1771, and sites/ in it,
//     root:root 0711 (ops_sitelogs.go);
//   - the agent's own directory and the configuration history, root 0700.
//
// Failures are logged, not fatal: the panel needs the agent up, and the calls
// that rely on these directories check them again.
func (s *Server) prepareDirs() {
	if os.Geteuid() != 0 {
		return
	}
	group := s.cfg.ServiceGroup
	if s.serviceGID() < 0 {
		group = ""
	}
	for _, d := range []DirSpec{
		{Path: s.cfg.RunDir, Mode: 0o1771, Owner: "root", Group: group},
		{Path: s.cfg.AgentStateDir(), Mode: 0o700, Owner: "root", Group: "root"},
		{Path: s.cfg.ConfHistoryDir(), Mode: 0o700, Owner: "root", Group: "root"},
	} {
		if _, _, err := ensureDirAt(d, s.trust(), followAll); err != nil {
			s.log.Warn("agent directories", "dir", d.Path, "err", err)
		}
	}
	if err := PrepareLogDir(s.cfg.LogDir, s.cfg.ServiceGroup); err != nil {
		s.log.Warn("site log tree", "dir", s.cfg.LogDir, "err", err)
	}
	s.removeForeignAPISocket()
}

// removeForeignAPISocket removes the API's socket when somebody else than the
// service user made it (monopanel api started by hand as root): in the
// sticky run directory the service user could not replace it, and the API
// would not come up again. The agent starts before the API.
func (s *Server) removeForeignAPISocket() {
	sock := s.cfg.APISocket()
	svc, err := lookupUID(s.cfg.ServiceUser)
	if err != nil {
		return
	}
	run, err := walkDir("/", filepath.Dir(sock), walkOpts{trust: s.trust(), follow: followAll, uid: -1, gid: -1})
	if err != nil {
		return
	}
	defer run.close()
	var st unix.Stat_t
	if err := unix.Fstatat(run.fd, filepath.Base(sock), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || int(st.Uid) == svc {
		return
	}
	if err := unix.Unlinkat(run.fd, filepath.Base(sock), 0); err == nil {
		s.log.Info("removed an API socket the service user did not make", "socket", sock, "uid", st.Uid)
	}
}
