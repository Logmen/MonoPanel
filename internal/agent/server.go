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
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sys/unix"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/osprofile"
	"monopanel/internal/peercred"
	"monopanel/internal/sandbox"
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
	// siteLogTreeErr is why the agent's start did not finish taking the site
	// log tree over and clearing it, nil once it did (roottrees.go);
	// siteLogTreeMu guards it.
	siteLogTreeMu  sync.Mutex
	siteLogTreeErr error
	// rootUID is the user whose directories root's walks trust as root's
	// own: 0 for the agent as it runs, the test's user in tests.
	rootUID int
	// unitDir is where /v1/units/* write (/etc/systemd/system), and the
	// drop-ins of the panel's socket go.
	unitDir string
	// shareMu orders the binds of the shared folders and the removal of
	// their units (write lock, taken with TryLock: lockShareUnits) against
	// the calls that change directories below the web root under a
	// shareGuard (read lock): a folder is bound only after the way to its
	// mount point was checked, never while such a call that read the mount
	// points before it is still at work (sharemount.go, sharebind.go).
	shareMu sync.RWMutex
	// lookupIP resolves a host name of web.listen for the panel's socket,
	// daemonReload reloads systemd after its drop-in changed. Tests replace
	// both.
	lookupIP     LookupIP
	daemonReload func(ctx context.Context) error
	// landlock is the Landlock ABI version restic runs its sandbox with
	// (restic_sandbox.go), 0 when the kernel has none — landlockErr says
	// why, and restic then runs as it is.
	landlock    int
	landlockErr error
	// noSymfollow: a backup's restic gets a view of the trees it reads in
	// which no symlink is followed (restic_sandbox.go), Landlock or not,
	// which ListenAndServe finds out once; noSymfollowErr says why not.
	noSymfollow    bool
	noSymfollowErr error
	// privateEtc: restic in its Landlock sandbox reads the system's files of
	// /etc in a private /etc of its own; privateEtcErr says why not.
	privateEtc    bool
	privateEtcErr error
	// resticTest is what came of the installed restic's restic version in
	// the sandbox a backup gets, as the agent started or since
	// (selfTestRestic); resticTestMu orders the runs, and a ping that does
	// not wait for one reads what the last one found without it
	// (pingResticSelfTest).
	resticTestMu sync.Mutex
	resticTest   atomic.Pointer[resticSelfTest]
	// sandboxExe is the binary sandbox-exec runs from: the agent's own,
	// /proc/self/exe — the one running, even when an update replaced the
	// file meanwhile.
	sandboxExe string
	// resticCache and resticTmp are restic's cache and temporary directory,
	// root's alone: resticCacheDir and resticTmpDir. Tests move them.
	resticCache, resticTmp string
	// restoreRoot is RestoreDir, where an in-place restore of the trees
	// outside the web root is staged; tests move it.
	restoreRoot string
	// apacheLegacySites is the shared Apache's directory of 0.8.19 that
	// /v1/apache/legacy reads (confcheck.ApacheLegacySites). Tests move it.
	apacheLegacySites string
}

// NewServer builds an agent.
func NewServer(cfg config.Config, profile osprofile.Profile, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	abi, err := sandbox.ABI()
	return &Server{cfg: cfg, profile: profile, log: log, rootUID: os.Geteuid(), unitDir: systemUnitDir,
		lookupIP: net.DefaultResolver.LookupIPAddr, daemonReload: systemdReload,
		landlock: abi, landlockErr: err, sandboxExe: "/proc/self/exe", resticCache: resticCacheDir, resticTmp: resticTmpDir, restoreRoot: RestoreDir,
		apacheLegacySites: confcheck.ApacheLegacySites}
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
	r.Post("/v1/apache/legacy", handle(s.apacheLegacy))
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
	r.Post("/v1/shares/bind", handle(s.shareBind))
	r.Post("/v1/shares/unbind", handle(s.shareUnbind))
	r.Post("/v1/shares/status", handle(s.shareStatus))
	r.Post("/v1/shares/records", handle(s.listShareRecords))
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
	resp := &PingResponse{OK: true, Version: buildinfo.Version, Hostname: host, PeerUID: -1,
		Sandbox: &SandboxInfo{Landlock: s.landlock, NoSymfollow: s.noSymfollow, PrivateEtc: s.privateEtc}}
	if s.landlockErr != nil {
		resp.Sandbox.Error = s.landlockErr.Error()
		resp.Sandbox.Hint = s.landlockHint()
	}
	if s.noSymfollowErr != nil {
		resp.Sandbox.NoSymfollowError = s.noSymfollowErr.Error()
	}
	if s.privateEtcErr != nil {
		resp.Sandbox.PrivateEtcError = s.privateEtcErr.Error()
	}
	// a restic installed or upgraded since the last test is tested now — but
	// not waited for longer than a status page waits for the agent
	if t := s.pingResticSelfTest(ctx); t != nil {
		resp.Sandbox.Restic, resp.Sandbox.ResticVersion = t.bin, t.version
		if t.err != nil {
			resp.Sandbox.ResticError = t.err.Error()
		}
	}
	if c, ok := peercred.FromContext(ctx); ok {
		resp.PeerUID = int(c.UID)
	}
	return resp, nil
}

// ListenAndServe serves on the agent socket until ctx is cancelled. The socket
// is root:<service group> 0660 and only root or the service user may connect.
func (s *Server) ListenAndServe(ctx context.Context) error {
	// The content checks of config/apply know the trees of this host.
	confcheck.Configure(confcheck.Layout{
		LogDir: s.cfg.LogDir, RunDir: s.cfg.RunDir, WWWRoot: s.cfg.WWWRoot,
		ServiceUser: s.cfg.ServiceUser, ServiceGroup: s.cfg.ServiceGroup,
		WebGroup: s.cfg.WebGroup, CLIGroup: s.cfg.CLIGroup,
	})
	s.prepareDirs(ctx)
	s.probeNoSymfollow(ctx)
	s.logSandbox()
	// restic version in a backup's sandbox tells the doctor and the log
	// whether restic starts there, and changes nothing the agent does: the
	// API does not wait for it — a restic that hangs would hold the socket
	// back for resticSelfTestTimeout. It ends with the agent.
	tctx, stopTest := context.WithCancel(ctx)
	tested := make(chan struct{})
	go func() {
		defer close(tested)
		s.selfTestRestic(tctx)
	}()
	defer func() {
		stopTest()
		<-tested
	}()
	// What in-place restores an earlier agent was killed in the middle of
	// left behind: listed now, before any restore can start, removed in
	// the background — a staged home may take a while.
	go s.removeStaging(s.staleStaging())
	if os.Geteuid() == 0 {
		// before the API, which starts after the agent, can start anything
		// that pulls such a unit in (sharemount.go)
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		s.retireUnsafeShareUnits(rctx)
		cancel()
		// before a run of the package manager can start: what the runs of
		// an earlier agent left in its directory (pkg_unit.go)
		rctx, cancel = context.WithTimeout(ctx, time.Minute)
		s.reapPkgRuns(rctx)
		cancel()
	}
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
//     agent's socket, php/;
//   - php/ in it, root:root 0755, where the php-fpm masters make the pools'
//     sockets as root: taken over and cleared when someone else made it
//     (roottrees.go);
//   - the log directory, root:<service group> 1771, and sites/ in it,
//     root:root 0711 (ops_sitelogs.go), taken over and cleared the same way;
//   - the agent's own directory and the configuration history, root 0700.
//
// Failures are logged, not fatal: the panel needs the agent up, and the calls
// that rely on these directories check them again — php-fpm, nginx and
// Apache are not started, restarted or reloaded, nor packages installed,
// while php/ or sites/ is not root's alone or sites/ not cleared
// (vetRootDaemonTrees, vetPackageScripts).
func (s *Server) prepareDirs(ctx context.Context) {
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
	if err := s.preparePHPSocketDir(ctx); err != nil {
		s.log.Error("php-fpm socket directory: php-fpm is not started, restarted or reloaded while it is not root's alone", "dir", filepath.Join(s.cfg.RunDir, "php"), "err", err)
	}
	if err := s.prepareSiteLogTree(ctx); err != nil {
		s.log.Error("site log tree: nginx, Apache and php-fpm are not started, restarted or reloaded until it is root's alone and cleared; the agent tries again before each", "dir", s.cfg.LogDir, "err", err)
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
