// Package api is the unprivileged HTTP process: REST API (OpenAPI 3.1 via
// huma), SSE, the embedded Web UI and the local CLI socket.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"monopanel/internal/acme"
	"monopanel/internal/agent"
	"monopanel/internal/buildinfo"
	"monopanel/internal/config"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/secrets"
	"monopanel/internal/store"
)

// Server wires the API together.
type Server struct {
	// listenAddr is the address the HTTPS listener is bound to once Serve
	// has it (a net.Addr): the console reaches the API there, an address no
	// other process can hold at the same time. listenV6Only says that it is
	// the IPv6 wildcard without IPv4, listenVia where the listener came from
	// (viaSocket, viaSelf).
	listenAddr   atomic.Value
	listenV6Only atomic.Bool
	listenVia    atomic.Int32
	// activation takes the listening sockets systemd passed the process
	// (monopanel-api.socket); tests pass their own. stopAPISocket stops
	// that socket through the agent when it keeps the port of web.listen
	// on another address (httpsListener).
	activation    func() ([]net.Listener, error)
	stopAPISocket func(ctx context.Context) error
	// keepPort makes Serve leave its copy of the HTTPS socket (keptPort) open
	// for the exit of the process to close (KeepPortUntilExit).
	keepPort atomic.Bool
	keptPort atomic.Pointer[os.File]

	cfg     config.Config
	db      *store.DB
	agent   *agent.Client
	jobs    *jobs.Runner
	profile osprofile.Profile
	render  *render.Renderer
	log     *slog.Logger
	api     huma.API
	router  chi.Router
	started time.Time
	limiter *loginLimiter
	tls     *certHolder
	acme    *acme.Manager
	secrets *secrets.Box
	// Readiness waits after a reload: systemd returns before php-fpm opens the
	// pool socket and nginx drains its old workers. Zero skips the wait, which
	// is what tests with a fake agent want.
	poolSocketWait time.Duration
	nginxWait      time.Duration
	// apacheWait is how long an account's Apache that has just started may
	// take to answer on its address before nginx is sent there; apacheDial
	// connects to it (tests answer elsewhere).
	apacheWait time.Duration
	apacheDial func(ctx context.Context, network, addr string) (net.Conn, error)
	// apacheLocks holds a *sync.Mutex per login (lockApache): the jobs of
	// two sites of one account run side by side.
	apacheLocks sync.Map
	// siteLocks holds a *sync.Mutex per site ID (lockSite): applySite runs
	// in the site's own jobs and, inline, in the move of the sites to their
	// accounts' Apaches.
	siteLocks sync.Map
	// movesMu orders the changes of the site moves (settingSiteMoves): the
	// start-up refresh queues them, every site.apply that succeeds takes
	// its site out.
	movesMu sync.Mutex
	// siteLogsRuleMu lets one call at a time add the SELinux label rule of
	// the site log tree (selinuxSiteLogsRule): the start-up refresh, the
	// hourly retry and every site.apply on a host where it is missing.
	siteLogsRuleMu sync.Mutex
	// strayShares holds why the start-up refresh could not unbind a shared
	// folder the agent keeps that no share names (an error by mount point,
	// unbindStrayShares), for the doctor.
	strayShares sync.Map
	// apacheLegacy is the directory of the shared Apache's configuration of
	// 0.8.19 and older (/etc/apache2/monopanel), whose files the upgrade
	// carries over and removes (the agent reads them); tests point it
	// elsewhere.
	apacheLegacy string
	// agentWaitStep is the first pause between two pings of an agent that
	// does not answer yet (waitForAgent), doubled up to agentWaitStepMax;
	// jobsAgentWait is how long the job workers wait for it at start.
	// Tests shorten them.
	agentWaitStep time.Duration
	jobsAgentWait time.Duration
	// refreshed is set once the start-up refresh ran: from then on the
	// hourly tick queues again the moves that did not finish.
	refreshed atomic.Bool
	// portStart answers net.ipv4.ip_unprivileged_port_start; tests and the
	// demo, whose Apache binds nothing, answer themselves.
	portStart func() int
	// probe answers whether a local port has a listener. Mail installs verify
	// their own listeners this way; tests with a fake agent replace it.
	probe func(port int, banner bool) (bool, string)
	// reach answers whether a port accepts connections on one of the host's
	// own outside addresses: a daemon bound to 127.0.0.1 answers the probe
	// above and still serves nobody. Tests replace it with the probe.
	reach func(ip string, port int) bool
	// hostIPs lists the host's IPv4 addresses, hostIPv6s its global IPv6
	// ones; tests move the host.
	hostIPs   func() []string
	hostIPv6s func() []string
	// lookup resolves a name in public DNS; tests answer themselves.
	lookup func(ctx context.Context, name string) ([]string, error)
	// restartDelay is how long a restart of monopanel-api that a request
	// asked for waits after the answer; tests need no wait.
	restartDelay time.Duration
	// kernel is where the doctor's kernel modules check looks (zero: this
	// host's; SetKernelSources).
	kernel kernelSources
}

// SetReadinessWaits overrides the post-reload waits: for php-fpm's socket,
// and for nginx and an account's Apache to answer. Intended for tests.
func (s *Server) SetReadinessWaits(poolSocket, nginx time.Duration) {
	s.poolSocketWait, s.nginxWait, s.apacheWait = poolSocket, nginx, nginx
}

// SetUnprivilegedPortStart overrides the reading of
// net.ipv4.ip_unprivileged_port_start. Intended for tests and the demo,
// whose pretend Apache binds nothing.
func (s *Server) SetUnprivilegedPortStart(fn func() int) { s.portStart = fn }

// SetHostIPs overrides the host's addresses. Intended for tests.
func (s *Server) SetHostIPs(fn func() []string) { s.hostIPs = fn }

// KeepPortUntilExit makes Serve leave a copy of the HTTPS socket open for the
// end of the process to close, for monopanel api: after Serve it waits for the
// jobs in flight, and a port closed before that would be anybody's while the
// socket moves to a new address, or wherever there is no socket. In a
// process that runs the server again (the demo, tests) Serve closes it.
func (s *Server) KeepPortUntilExit() { s.keepPort.Store(true) }

// SetHostIPv6s replaces the host's IPv6 address list (tests, the demo).
func (s *Server) SetHostIPv6s(fn func() []string) { s.hostIPv6s = fn }

// SetLookup overrides the public DNS lookup. Intended for tests.
func (s *Server) SetLookup(fn func(ctx context.Context, name string) ([]string, error)) {
	s.lookup = fn
}

// SetPortProbe overrides how the panel checks a local listener. Intended for
// tests, where no daemon actually binds anything.
func (s *Server) SetPortProbe(fn func(port int, banner bool) (bool, string)) {
	s.probe = fn
	s.reach = func(_ string, port int) bool { open, _ := fn(port, false); return open }
}

// SetReach overrides the check on the host's outside address. Intended for
// tests.
func (s *Server) SetReach(fn func(ip string, port int) bool) { s.reach = fn }

// SetOutbound routes the panel's own downloads (repository keys, release
// packages, the probe of packages.sury.org) through rt. Intended for the
// demo, which has no internet access; it affects every Server in the process.
func SetOutbound(rt http.RoundTripper) {
	fetchClient = &http.Client{Transport: rt}
	repoProbeClient = &http.Client{Transport: rt, Timeout: 20 * time.Second}
}

var secured = []map[string][]string{{"bearer": {}}, {"session": {}}}

// New builds the server and registers every route and job handler.
func New(cfg config.Config, db *store.DB, ag *agent.Client, runner *jobs.Runner, profile osprofile.Profile, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, db: db, agent: ag, jobs: runner, profile: profile, render: render.New(cfg.TemplatesDir), log: log, started: time.Now(), limiter: newLoginLimiter(8, time.Minute)}
	if cfg.Log.AuditFile != "" {
		db.SetAuditSink(s.auditFile(cfg.Log.AuditFile))
	}
	s.poolSocketWait, s.nginxWait, s.apacheWait = 10*time.Second, 15*time.Second, apacheWaitDefault
	s.apacheDial = (&net.Dialer{Timeout: time.Second}).DialContext
	s.apacheLegacy = path.Join(profile.Web().ApacheConfDir, "monopanel")
	s.agentWaitStep, s.jobsAgentWait = agentWaitStepFirst, jobsAgentWaitDefault
	// Every job acts through the agent: the workers claim none before it
	// answers, or before jobsAgentWait has passed (waitForAgent).
	runner.Hold(func(ctx context.Context) {
		if !s.waitForAgent(ctx, "jobs", s.jobsAgentWait) && ctx.Err() == nil {
			s.log.Warn("jobs: the workers start without the agent; jobs fail on it until it answers")
		}
	})
	s.portStart = unprivilegedPortStart
	s.restartDelay = 500 * time.Millisecond
	s.activation = activation.Listeners
	s.stopAPISocket = func(ctx context.Context) error {
		if s.agent == nil {
			return errors.New("no agent")
		}
		_, err := s.agent.Service(ctx, s.cfg.APISocketUnit(), "stop")
		return err
	}
	s.probe = probePort
	s.reach = reachPort
	s.hostIPs = localIPv4s
	s.hostIPv6s = localIPv6s
	s.lookup = publicLookup
	s.tls = newCertHolder(cfg, log)
	if box, err := secrets.Open(cfg.SecretKeyFile); err == nil {
		s.secrets = box
	} else {
		log.Warn("secret key unavailable; encrypted settings (backups, DNS tokens) are disabled", "err", err)
	}
	s.acme = acme.New(filepath.Join(cfg.DataDir, "acme"), filepath.Join(cfg.DataDir, "acme", "webroot"), filepath.Join(cfg.DataDir, "certs"), "monopanel/"+buildinfo.Version, log)
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, requestInfoMiddleware, securityHeaders(uiCSPFromEmbed()), s.accessLog)

	hcfg := huma.DefaultConfig("MonoPanel API", buildinfo.Version)
	hcfg.Info.Description = "Web server management: users, sites, PHP, databases, TLS, backups. The web UI, the CLI and the TUI use this same API."
	hcfg.Servers = []*huma.Server{{URL: "/api/v1"}}
	hcfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer":  {Type: "http", Scheme: "bearer", Description: "API token: mp token create"},
		"session": {Type: "apiKey", In: "cookie", Name: sessionCookieName, Description: "Browser session"},
	}
	// Страница документации своя: huma тянет её скрипт с unpkg.com, а она
	// живёт на origin панели, рядом с сессией администратора.
	hcfg.DocsPath = ""
	r.Route("/api/v1", func(sub chi.Router) {
		sub.Use(s.docsGate("/api/v1"))
		mountDocs(sub, "/api/v1")
		s.api = humachi.New(sub, hcfg)
		s.api.UseMiddleware(s.authMiddleware)
		s.registerSystem()
		s.registerSELinux()
		s.registerCMS()
		s.registerConsole()
		s.registerAuth()
		s.registerUsers()
		s.registerJobs()
		s.registerAudit()
		s.registerTokens()
		s.registerServices()
		s.registerStack()
		s.registerStackUpgrade()
		s.registerCerts()
		s.registerPHP()
		s.registerSites()
		s.registerSiteIP()
		s.registerDB()
		s.registerCron()
		s.registerFirewall()
		s.registerMetrics()
		s.registerBackups()
		s.registerTOTP()
		s.registerWebhooks()
		s.registerFiles()
		s.registerUserUpdate()
		s.registerDNS()
		s.registerApps()
		s.registerValkey()
		s.registerShares()
		s.registerRealIP()
		s.registerCertImport()
		s.registerPanelTLS()
		s.registerTools()
		s.registerUserDelete()
		s.registerSiteNginx()
		s.registerPresets()
		s.registerPHPExtensions()
		s.registerPHPGlobal()
		s.registerDBConfig()
		s.registerUpdate()
		s.registerPanelConfig()
		s.registerMail()
		s.registerMigrateSource()
		s.registerMigrateImport()
	})
	r.Handle("/*", s.uiHandler())
	s.router = r

	s.jobs.Register("user.provision", s.jobUserProvision)
	s.jobs.Register("user.delete", s.jobUserDelete)
	s.jobs.Register("stack.install", s.jobStackInstall)
	s.jobs.Register("apache.migrate", s.jobApacheMigrate)
	s.jobs.Register("apache.backend", s.jobApacheBackend)
	s.jobs.Register("stack.remove", s.jobStackRemove)
	s.jobs.Register("stack.upgrade", s.jobStackUpgrade)
	s.jobs.Register("cert.issue", s.jobCertIssue)
	s.jobs.Register("php.install", s.jobPHPInstall)
	s.jobs.Register("php.remove", s.jobPHPRemove)
	s.jobs.Register("site.apply", s.jobSiteApply)
	s.jobs.Register("site.fix", s.jobSiteFix)
	s.jobs.Register("site.cms", s.jobSiteCMS)
	s.jobs.Register("site.delete", s.jobSiteDelete)
	s.jobs.Register("sites.move-ip", s.jobSitesMoveIP)
	s.jobs.Register("backup.run", s.jobBackupRun)
	s.jobs.Register("backup.restore", s.jobBackupRestore)
	s.jobs.Register("panel.update", s.jobPanelUpdate)
	s.jobs.Register("mail.install", s.jobMailInstall)
	s.jobs.Register("mail.apply", s.jobMailApply)
	s.jobs.Register("mail.webmail", s.jobWebmailInstall)
	s.jobs.Register("migrate.run", s.jobMigrateRun)
	// The daily upgrade of nginx, PHP and Apache and the panel's own update
	// come on their own, by night, and both run the package manager — and
	// the update ends in restarting the agent and the API. Neither starts
	// while the other runs, in either order: it waits in the queue instead
	// of meeting the other's dpkg lock or restarts. Their lock keys stay as
	// they are: the upgrade still waits for the other stack jobs, nothing
	// else waits for the update. The update's install, which goes on in
	// monopanel-update.service after its job, and the package manager's runs
	// keep apart in the agent (pkg_unit.go).
	s.jobs.Exclusive("stack.upgrade", "panel.update")
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.router }

// API exposes the huma API (tests, OpenAPI export).
func (s *Server) API() huma.API { return s.api }

type reqInfo struct {
	IP     string
	UA     string
	Host   string
	Origin string
	TLS    bool

	mu    sync.Mutex
	after []func()
}

type reqInfoKey struct{}

func requestInfoMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || ip == "" {
			ip = "local"
		}
		info := &reqInfo{IP: ip, UA: r.UserAgent(), Host: r.Host, Origin: r.Header.Get("Origin"), TLS: r.TLS != nil}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info)))
		info.mu.Lock()
		after := info.after
		info.after = nil
		info.mu.Unlock()
		if len(after) > 0 {
			go func() {
				for _, fn := range after {
					fn()
				}
			}()
		}
	})
}

// afterResponse runs fn once the handler of the current request has written
// its answer. Restarting monopanel-api from inside the request that asked for
// it would kill the process before the client hears back; started from here,
// the restart finds the request finished, and the graceful shutdown on
// SIGTERM lets the answer leave. Outside an HTTP request fn just runs in the
// background.
func afterResponse(ctx context.Context, fn func()) {
	if info, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		info.mu.Lock()
		info.after = append(info.after, fn)
		info.mu.Unlock()
		return
	}
	go fn()
}

func requestInfo(ctx context.Context) *reqInfo {
	if i, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return i
	}
	return &reqInfo{IP: "unknown"}
}

func securityHeaders(csp string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "same-origin")
			if r.TLS != nil {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			if !strings.HasPrefix(r.URL.Path, "/api/") {
				h.Set("Content-Security-Policy", csp)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(), "ms", time.Since(start).Milliseconds(), "ip", requestInfo(r.Context()).IP)
		}
	})
}
