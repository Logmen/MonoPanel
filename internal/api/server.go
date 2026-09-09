// Package api is the unprivileged HTTP process: REST API (OpenAPI 3.1 via
// huma), SSE, the embedded Web UI and the local CLI socket.
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

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
	// probe answers whether a local port has a listener. Mail installs verify
	// their own listeners this way; tests with a fake agent replace it.
	probe func(port int, banner bool) (bool, string)
}

// SetReadinessWaits overrides the post-reload waits. Intended for tests.
func (s *Server) SetReadinessWaits(poolSocket, nginx time.Duration) {
	s.poolSocketWait, s.nginxWait = poolSocket, nginx
}

// SetPortProbe overrides how the panel checks a local listener. Intended for
// tests, where no daemon actually binds anything.
func (s *Server) SetPortProbe(fn func(port int, banner bool) (bool, string)) { s.probe = fn }

var secured = []map[string][]string{{"bearer": {}}, {"session": {}}}

// New builds the server and registers every route and job handler.
func New(cfg config.Config, db *store.DB, ag *agent.Client, runner *jobs.Runner, profile osprofile.Profile, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, db: db, agent: ag, jobs: runner, profile: profile, render: render.New(cfg.TemplatesDir), log: log, started: time.Now(), limiter: newLoginLimiter(8, time.Minute)}
	s.poolSocketWait, s.nginxWait = 10*time.Second, 15*time.Second
	s.probe = probePort
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
	hcfg.Info.Description = "Управление веб-сервером: пользователи, сайты, PHP, базы данных, TLS, бэкапы. Web UI, CLI и TUI используют этот же API."
	hcfg.Servers = []*huma.Server{{URL: "/api/v1"}}
	hcfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer":  {Type: "http", Scheme: "bearer", Description: "API token: mp token create"},
		"session": {Type: "apiKey", In: "cookie", Name: sessionCookieName, Description: "Browser session"},
	}
	r.Route("/api/v1", func(sub chi.Router) {
		s.api = humachi.New(sub, hcfg)
		s.api.UseMiddleware(s.authMiddleware)
		s.registerSystem()
		s.registerAuth()
		s.registerUsers()
		s.registerJobs()
		s.registerTokens()
		s.registerServices()
		s.registerStack()
		s.registerCerts()
		s.registerPHP()
		s.registerSites()
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
		s.registerRealIP()
		s.registerCertImport()
		s.registerUserDelete()
		s.registerSiteNginx()
		s.registerPresets()
		s.registerPHPExtensions()
		s.registerUpdate()
		s.registerMail()
		s.registerMigrateSource()
		s.registerMigrateImport()
	})
	r.Handle("/*", s.uiHandler())
	s.router = r

	s.jobs.Register("user.provision", s.jobUserProvision)
	s.jobs.Register("user.delete", s.jobUserDelete)
	s.jobs.Register("stack.install", s.jobStackInstall)
	s.jobs.Register("cert.issue", s.jobCertIssue)
	s.jobs.Register("php.install", s.jobPHPInstall)
	s.jobs.Register("php.remove", s.jobPHPRemove)
	s.jobs.Register("site.apply", s.jobSiteApply)
	s.jobs.Register("site.delete", s.jobSiteDelete)
	s.jobs.Register("backup.run", s.jobBackupRun)
	s.jobs.Register("backup.restore", s.jobBackupRestore)
	s.jobs.Register("panel.update", s.jobPanelUpdate)
	s.jobs.Register("mail.install", s.jobMailInstall)
	s.jobs.Register("mail.apply", s.jobMailApply)
	s.jobs.Register("mail.webmail", s.jobWebmailInstall)
	s.jobs.Register("migrate.run", s.jobMigrateRun)
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
	})
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
