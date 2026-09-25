package demo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/api"
	"monopanel/internal/auth"
	"monopanel/internal/client"
	"monopanel/internal/config"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// The demo's own administrator. The browser never signs in: the container's
// front door adds the demo token to every request, and the password is only
// for someone who signed out and wants back in.
const (
	AdminLogin    = "demo"
	AdminPassword = "monopanel-demo"
	PanelHostname = "panel.example.com"
	// SiteIP is the address the pretend sites listen on (documentation range).
	SiteIP = "203.0.113.10"
)

// Options says where the demo lives.
type Options struct {
	// Dir holds the prepared demo: config, database, keys, the pretend
	// server's state and the token.
	Dir string
	// WWW is the root of the account homes; its files are real.
	WWW string
	// Listen is the plain HTTP address the Worker talks to (Serve only).
	Listen string
	// PanelAddr is where the API itself listens with HTTPS (the web console
	// reaches it there); 127.0.0.1:8443 by default.
	PanelAddr string
	// Run holds the sockets; <Dir>/run by default. A unix socket path is
	// limited to about a hundred bytes, so tests keep it short.
	Run string
	// Self is the monopanel binary; the file manager runs its fsop.
	Self string
	Log  *slog.Logger
}

func (o Options) configPath() string { return filepath.Join(o.Dir, "etc", "config.yaml") }
func (o Options) tokenPath() string  { return filepath.Join(o.Dir, "etc", "demo.token") }
func (o Options) statePath() string  { return filepath.Join(o.Dir, "agent.json") }

func (o Options) logger() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// Profile is Ubuntu 24.04 wherever the demo runs, so the pages and the
// package names match the screenshots.
func Profile() osprofile.Profile {
	p, err := osprofile.FromRelease(Release)
	if err != nil {
		panic(err) // a supported release by construction
	}
	return p
}

// place puts the paths that belong to this run, not to the prepared demo.
func (o Options) place(cfg *config.Config) {
	cfg.RunDir = filepath.Join(o.Dir, "run")
	if o.Run != "" {
		cfg.RunDir = o.Run
	}
	cfg.Web.Listen = "127.0.0.1:8443"
	if o.PanelAddr != "" {
		cfg.Web.Listen = o.PanelAddr
	}
}

func (o Options) config() config.Config {
	cfg := config.Default()
	cfg.ConfigDir = filepath.Join(o.Dir, "etc")
	cfg.DataDir = filepath.Join(o.Dir, "data")
	cfg.LogDir = filepath.Join(o.Dir, "log")
	cfg.TemplatesDir = filepath.Join(o.Dir, "templates")
	cfg.SecretKeyFile = filepath.Join(o.Dir, "etc", "secret.key")
	cfg.WWWRoot = o.WWW
	cfg.Web.Hostname = PanelHostname
	cfg.Jobs.Workers = 2
	o.place(&cfg)
	return cfg
}

// Prepare builds the demo once, when the image is made: a panel with the
// example accounts, sites, databases, mail and backups, and a day of
// metrics. A container then starts from it in a moment.
func Prepare(ctx context.Context, o Options) error {
	log := o.logger()
	for _, d := range []string{"etc", "data", "log"} {
		if err := os.MkdirAll(filepath.Join(o.Dir, d), 0o755); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(o.WWW, 0o755); err != nil {
		return err
	}
	cfg := o.config()
	if err := cfg.Save(o.configPath()); err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.SecretKeyFile, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := api.EnsureSelfSigned(cfg.TLSCertPath(), cfg.TLSKeyPath(), api.LocalNames(cfg.Web.Hostname)); err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(AdminPassword)
	if err != nil {
		return err
	}
	admin := &store.User{Login: AdminLogin, Role: store.RoleAdmin, PasswordHash: hash, Email: "demo@example.com", Status: store.UserActive}
	if err := db.CreateUser(ctx, admin); err != nil {
		return err
	}
	token, err := auth.NewToken(32)
	if err != nil {
		return err
	}
	if err := db.CreateAPIToken(ctx, &store.APIToken{UserID: admin.ID, Name: "demo", Hash: auth.HashToken(token)}); err != nil {
		return err
	}
	if err := os.WriteFile(o.tokenPath(), []byte(token), 0o600); err != nil {
		return err
	}
	// Without the internet a check could only fail; the settings page says
	// "checks off" instead of an error.
	if err := db.SetSetting(ctx, "update", `{"check_hours":0}`); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}

	ag := NewAgent(cfg, o.Self, log)
	ag.BaseSystem()
	run, err := start(ctx, o, cfg, ag)
	if err != nil {
		return err
	}
	cl := client.NewRemote("https://"+cfg.Web.Listen, token, true)
	if err := waitReady(ctx, cl); err != nil {
		run.stop()
		return err
	}
	err = seed(ctx, cl, run.db, o, log)
	if err == nil {
		err = waitJobs(ctx, cl)
	}
	if err == nil {
		err = metricsHistory(ctx, run.db)
	}
	run.stop()
	if err != nil {
		return err
	}
	return ag.Save(o.statePath())
}

// running is a started demo: the pretend agent, the API and its database.
type running struct {
	db      *store.DB
	handler http.Handler
	cancel  context.CancelFunc
	done    chan struct{}
}

func (r *running) stop() {
	r.cancel()
	<-r.done
	_ = r.db.Close()
}

// goOffline cuts the process off the internet once, before anything else
// makes a request: downloads get canned answers or a plain "no internet
// access", the same when the image is built and in a container.
var goOffline sync.Once

// start runs the pretend agent and the API (HTTPS on cfg.Web.Listen and the
// local socket) the way the systemd units would.
func start(ctx context.Context, o Options, cfg config.Config, ag *Agent) (*running, error) {
	log := o.logger()
	goOffline.Do(func() {
		api.SetOutbound(offline{})
		http.DefaultTransport = offline{}
	})
	cctx, cancel := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		if err := ag.ListenAndServe(cctx); err != nil {
			log.Error("demo agent", "err", err)
		}
	}()
	db, err := store.Open(cctx, cfg.DBPath())
	if err != nil {
		cancel()
		return nil, err
	}
	runner := jobs.NewRunner(db, cfg.Jobs.Workers, log)
	srv := api.New(cfg, db, agent.NewClient(cfg.AgentSocket()), runner, Profile(), log)
	srv.SetReadinessWaits(0, 0)
	srv.SetHostIPs(func() []string { return []string{SiteIP} })
	srv.SetLookup(lookup)
	srv.SetPortProbe(ag.probe)
	runner.Start(cctx)
	r := &running{db: db, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		if err := srv.Run(cctx); err != nil {
			log.Error("demo api", "err", err)
		}
		runner.Wait()
		<-agentDone
	}()
	r.handler = srv.Handler()
	return r, nil
}

// lookup is the public DNS of the demo's world: every example name points at
// the pretend server.
func lookup(_ context.Context, name string) ([]string, error) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, zone := range []string{"example.com", "example.org", "example.net"} {
		if name == zone || strings.HasSuffix(name, "."+zone) {
			return []string{SiteIP}, nil
		}
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// probe answers for the mail ports once the pretend mail server is there,
// with the greetings postfix and dovecot would give.
func (a *Agent) probe(port int, _ bool) (bool, string) {
	a.mu.Lock()
	_, mail := a.st.Packages["postfix"]
	a.mu.Unlock()
	if !mail {
		return false, ""
	}
	switch port {
	case 25, 587:
		return true, "220 mail.example.com ESMTP Postfix (Ubuntu)"
	case 143:
		return true, "* OK [CAPABILITY IMAP4rev1 SASL-IR LOGIN-REFERRALS ID ENABLE IDLE LITERAL+ STARTTLS AUTH=PLAIN AUTH=LOGIN] Dovecot (Ubuntu) ready."
	case 110:
		return true, "+OK Dovecot (Ubuntu) ready."
	case 4190:
		return true, `"IMPLEMENTATION" "Dovecot (Ubuntu) Pigeonhole"`
	case 465, 993, 995:
		return true, ""
	}
	return false, ""
}

func waitReady(ctx context.Context, cl *client.Client) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := cl.Health(ctx); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("the panel did not start: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitJobs waits until the queue is empty and fails on a failed job: the
// demo must not ship with a broken example.
func waitJobs(ctx context.Context, cl *client.Client) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		list, err := cl.ListJobs(ctx, 500, "")
		if err != nil {
			return err
		}
		busy := false
		for _, j := range list {
			switch j.Status {
			case store.JobQueued, store.JobRunning:
				busy = true
			case store.JobFailed:
				return fmt.Errorf("job #%d %s failed: %s", j.ID, j.Type, j.Error)
			}
		}
		if !busy {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("jobs did not finish in time")
}

// metricsHistory draws the last day of the dashboard's charts: the panel
// only starts sampling when the container starts.
func metricsHistory(ctx context.Context, db *store.DB) error {
	const gb = 1 << 30
	now := time.Now().Truncate(time.Minute)
	for t := now.Add(-24 * time.Hour); t.Before(now); t = t.Add(time.Minute) {
		load, used := HostLoad(t)
		rx := int64(180_000 + 900_000*load)
		if err := db.InsertMetric(ctx, &store.MetricPoint{TS: t.Unix(), CPU: 6 + 18*load, Load1: load, MemUsed: int64(used), MemTotal: 8 * gb, DiskUsed: 29*gb - 300<<20, DiskTotal: 80 * gb, NetRx: rx, NetTx: rx * 3}); err != nil {
			return err
		}
	}
	return nil
}

// Serve runs a prepared demo inside a container: the pretend agent, the API
// and a plain HTTP front door for the Worker on o.Listen.
func Serve(ctx context.Context, o Options) error {
	cfg, err := config.Load(o.configPath())
	if err != nil {
		return err
	}
	o.place(&cfg)
	token, err := os.ReadFile(o.tokenPath())
	if err != nil {
		return err
	}
	ag := NewAgent(cfg, o.Self, o.logger())
	if err := ag.Load(o.statePath()); err != nil {
		return err
	}
	run, err := start(ctx, o, cfg, ag)
	if err != nil {
		return err
	}
	defer run.stop()
	front := &http.Server{Addr: o.Listen, Handler: Front(run.handler, strings.TrimSpace(string(token)), demoTokenID(ctx, run.db)), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = front.Close()
	}()
	o.logger().Info("demo listening", "http", o.Listen)
	if err := front.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func demoTokenID(ctx context.Context, db *store.DB) int64 {
	u, err := db.GetUserByLogin(ctx, AdminLogin)
	if err != nil {
		return 0
	}
	list, err := db.ListAPITokens(ctx, u.ID)
	if err != nil {
		return 0
	}
	for _, t := range list {
		if t.Name == "demo" {
			return t.ID
		}
	}
	return 0
}

// refusal is one thing the demo does not do, and why.
type refusal struct {
	method string
	path   *regexp.Regexp
	why    string
}

var refusals = []refusal{
	{http.MethodPost, regexp.MustCompile(`^/api/v1/system/update/(apply|check)$`), "updating the panel"},
	{http.MethodPut, regexp.MustCompile(`^/api/v1/system/update$`), "update settings"},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/sites/[^/]+/cms$`), "installing a CMS downloads it from its vendor, and the demo has no internet access"},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/(sites/[^/]+/tls/issue|certificates(/[0-9]+/renew)?|ssl/panel/issue)$`), "Let's Encrypt needs a real domain and internet access"},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/migrate/(plan|run)$`), "a move needs a source server"},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/mail/webmail$`), "Roundcube is downloaded from its vendor, and the demo has no internet access"},
	{http.MethodDelete, regexp.MustCompile(`^/api/v1/users/` + AdminLogin + `$`), "the demo's own administrator stays"},
}

// Front is the container's door for the Worker: every request acts as the
// demo administrator (the demo token is added here, the browser never holds
// it), and what cannot work in a sandbox answers why instead of failing
// halfway through a job.
func Front(next http.Handler, token string, tokenID int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		for _, f := range refusals {
			if r.Method == f.method && f.path.MatchString(r.URL.Path) {
				refuse(w, f.why)
				return
			}
		}
		if r.Method == http.MethodDelete && tokenID != 0 && r.URL.Path == "/api/v1/tokens/"+strconv.FormatInt(tokenID, 10) {
			refuse(w, "the demo signs you in with this token")
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/users/"+AdminLogin || r.Method == http.MethodPost && (r.URL.Path == "/api/v1/stack/install" || r.URL.Path == "/api/v1/system/console") {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req struct {
				Status    string   `json:"status"`
				Role      string   `json:"role"`
				Component string   `json:"component"`
				Args      []string `json:"args"`
			}
			_ = json.Unmarshal(body, &req)
			switch {
			case req.Status != "" && req.Status != store.UserActive, req.Role != "" && req.Role != store.RoleAdmin:
				refuse(w, "the demo's own administrator stays")
				return
			case req.Component == "composer" || req.Component == "sphinx":
				refuse(w, req.Component+" is downloaded from its vendor, and the demo has no internet access")
				return
			case len(req.Args) > 0 && req.Args[0] == "demo":
				refuse(w, "the demo itself is not run from its console")
				return
			}
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Del("Cookie")
		next.ServeHTTP(w, r)
	})
}

func refuse(w http.ResponseWriter, why string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": "Forbidden", "status": http.StatusForbidden, "detail": "Not available in the demo: " + why + "."})
}
