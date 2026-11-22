package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

const nginxKeyURL = "https://nginx.org/keys/nginx_signing.key"

type stackOutput struct {
	Body []apitypes.StackComponent
}

type stackInstallInput struct {
	Body apitypes.StackInstallRequest
}

type jobRefOutput struct {
	Status int
	Body   apitypes.JobRef
}

func (s *Server) registerStack() {
	huma.Register(s.api, huma.Operation{
		OperationID: "stack-list", Method: http.MethodGet, Path: "/stack", Summary: "Installed stack components", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*stackOutput, error) {
		actx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out := &stackOutput{Body: []apitypes.StackComponent{}}
		web := s.profile.Web()
		q, err := s.agent.Pkg(actx, "query", "nginx")
		comp := apitypes.StackComponent{Name: "nginx", Kind: "web"}
		if err == nil {
			if v, ok := q.Installed["nginx"]; ok {
				comp.Installed, comp.Version = true, v
				if st, err := s.agent.Service(actx, web.NginxService, "status"); err == nil {
					comp.Service = &st.Status
				}
			}
		}
		out.Body = append(out.Body, comp)
		apachePkg := "apache2"
		if s.profile.Family() == osprofile.FamilyRHEL {
			apachePkg = "httpd"
		}
		// Apache is the accounts' own Apaches, one per account with sites in
		// Apache mode: how many run, not the distribution's apache2.service,
		// which the panel keeps off — but while the move of an upgrade has
		// not finished, when it serves the sites that wait.
		ac := apitypes.StackComponent{Name: "apache", Kind: "web"}
		if q, err := s.agent.Pkg(actx, "query", apachePkg); err == nil {
			if v, ok := q.Installed[apachePkg]; ok {
				ac.Installed, ac.Version = true, v
				s.apacheInstancesStatus(actx, &ac)
			}
		}
		out.Body = append(out.Body, ac)
		if inst, err := s.db.GetDBInstance(ctx); err == nil {
			dc := apitypes.StackComponent{Name: inst.Engine, Installed: inst.Status == store.DBReady, Version: inst.Version, Kind: "db"}
			if dc.Installed {
				if st, err := s.agent.Service(actx, inst.Service, "status"); err == nil {
					dc.Service = &st.Status
				}
			} else if inst.LastError != "" {
				dc.Version = inst.Status + ": " + inst.LastError
			}
			out.Body = append(out.Body, dc)
		}
		if versions, err := s.db.ListPHPVersions(ctx); err == nil {
			for _, v := range versions {
				pc := apitypes.StackComponent{Name: "php-" + v.Version, Installed: v.Status == store.PHPInstalled, Version: v.PackageVersion, Kind: "php"}
				if pc.Installed {
					if st, err := s.agent.Service(actx, v.FPMService, "status"); err == nil {
						pc.Service = &st.Status
					}
				}
				out.Body = append(out.Body, pc)
			}
		}
		out.Body = append(out.Body, s.fail2banComponent(actx))
		out.Body = append(out.Body, s.toolComponents(actx)...)
		s.annotateUpgrades(actx, out.Body)
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "stack-install", Method: http.MethodPost, Path: "/stack/install", Summary: "Install a stack component (async)", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *stackInstallInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		job, err := s.jobs.Enqueue(ctx, "stack.install", in.Body, jobs.WithLockKey("stack"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "stack.install", Target: in.Body.Component, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

func (s *Server) jobStackInstall(ctx context.Context, jc *jobs.Context) error {
	var p apitypes.StackInstallRequest
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	switch p.Component {
	case "nginx":
		return s.installNginx(ctx, jc)
	case "apache":
		return s.installApache(ctx, jc)
	case "percona", "mysql":
		return s.installDB(ctx, jc, p.Component)
	case "fail2ban":
		return s.installFail2ban(ctx, jc)
	case "memcached":
		return s.installMemcached(ctx, jc)
	case "jpegoptim", "git":
		return s.installToolPackages(ctx, jc, p.Component)
	case "composer":
		return s.installComposer(ctx, jc)
	case "sphinx":
		return s.installSphinx(ctx, jc)
	case "valkey":
		return s.installValkey(ctx, jc)
	}
	return fmt.Errorf("unknown component %q", p.Component)
}

func logTail(jc *jobs.Context, out string, lines int) {
	all := strings.Split(strings.TrimSpace(out), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	for _, l := range all {
		if strings.TrimSpace(l) != "" {
			jc.Logf("  %s", l)
		}
	}
}

func fetchText(ctx context.Context, url string) (string, error) {
	b, err := fetchBytes(ctx, url)
	return string(b), err
}

// fetchBytes downloads a small file (a signing key, a release package). A
// transient network failure or a 5xx is retried a few times: a TLS handshake
// timeout to a vendor repository must not fail a whole installation.
func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	return fetchBytesN(ctx, url, 1<<20)
}

// fetchRetryBase paces retries of network fetches: 5 s, then 20 s. A dropped
// connection to github.com or a mirror is common enough on a fresh server
// that one timeout must not fail a whole install.
var fetchRetryBase = 5 * time.Second

// withRetry runs op up to three times while it reports a retryable failure.
func withRetry[T any](ctx context.Context, op func() (T, bool, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * fetchRetryBase):
			}
		}
		v, retry, err := op()
		if err == nil {
			return v, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return zero, lastErr
}

// fetchBytesN is fetchBytes with its own size limit (composer.phar is a few MB).
func fetchBytesN(ctx context.Context, url string, limit int64) ([]byte, error) {
	return withRetry(ctx, func() ([]byte, bool, error) { return fetchOnce(ctx, url, limit) })
}

// fetchClient has no overall deadline: a 40 MB Sphinx tarball from a slow
// mirror takes minutes, and the job's context bounds the whole install.
// Connecting and waiting for headers are bounded here; the body is watched
// for stalls instead (fetchIdle).
var fetchClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}}

// fetchIdle is how long a download may deliver nothing before it is cut off.
var fetchIdle = time.Minute

func fetchOnce(ctx context.Context, url string, limit int64) (body []byte, retry bool, err error) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	res, err := fetchClient.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode >= 500, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	stall := time.AfterFunc(fetchIdle, cancel)
	defer stall.Stop()
	b, err := io.ReadAll(io.LimitReader(&stallReader{r: res.Body, timer: stall}, limit))
	if err != nil && rctx.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("GET %s: no data for %s: %w", url, fetchIdle, err)
	}
	return b, err != nil, err
}

// fetchStream opens a download for streaming (a CMS distribution goes
// straight into tar on the agent instead of through memory), with the same
// stall watch as fetchOnce: the body is cut off after fetchIdle without data.
// Opening the stream is retried like fetchBytes. Once bytes flow, a break or
// a stall picks the download up where it stopped, when the server takes byte
// ranges of the same file (Accept-Ranges, and its ETag or Last-Modified sent
// back as If-Range): the reader — tar on the agent — never sees the break,
// which servers behind DDoS filters (1c-bitrix.ru) deal out to long downloads.
// A server that takes no ranges, or a file that changed meanwhile, ends the
// stream with the error: the half-written tar cannot be resumed otherwise.
func fetchStream(ctx context.Context, url string) (io.ReadCloser, error) {
	b, err := withRetry(ctx, func() (*stallBody, bool, error) { return fetchStreamFrom(ctx, url, 0, "") })
	if err != nil {
		return nil, err
	}
	return &resumingBody{ctx: ctx, url: url, cur: b, validator: b.validator}, nil
}

// fetchStreamResumes is how often one download is picked up again.
var fetchStreamResumes = 8

// fetchStreamFrom asks for the file from byte off on (0: all of it); a resumed
// request sends validator as If-Range, so a changed file comes back whole and
// is refused rather than spliced.
func fetchStreamFrom(ctx context.Context, url string, off int64, validator string) (*stallBody, bool, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, false, err
	}
	want := http.StatusOK
	if off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
		req.Header.Set("If-Range", validator)
		want = http.StatusPartialContent
	}
	res, err := fetchClient.Do(req)
	if err != nil {
		cancel()
		return nil, ctx.Err() == nil, err
	}
	if res.StatusCode != want {
		res.Body.Close()
		cancel()
		if off > 0 && res.StatusCode == http.StatusOK {
			return nil, false, fmt.Errorf("GET %s: the server sent the whole file again instead of the rest from byte %d: it changed meanwhile, or takes no ranges", url, off)
		}
		return nil, res.StatusCode >= 500, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	if off > 0 {
		var start int64
		if _, err := fmt.Sscanf(res.Header.Get("Content-Range"), "bytes %d-", &start); err != nil || start != off {
			res.Body.Close()
			cancel()
			return nil, false, fmt.Errorf("GET %s: the server resumed at %q, not at byte %d", url, res.Header.Get("Content-Range"), off)
		}
	}
	b := &stallBody{body: res.Body, cancel: cancel, url: url}
	if off > 0 || res.Header.Get("Accept-Ranges") == "bytes" {
		// If-Range takes a strong ETag or a date; a weak ETag would make the
		// server send the whole file every time.
		if et := res.Header.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
			b.validator = et
		} else {
			b.validator = res.Header.Get("Last-Modified")
		}
		if off > 0 {
			b.validator = validator
		}
	}
	b.timer = time.AfterFunc(fetchIdle, func() { b.stalled.Store(true); cancel() })
	b.r = &stallReader{r: res.Body, timer: b.timer}
	return b, false, nil
}

type stallBody struct {
	r      io.Reader
	body   io.Closer
	timer  *time.Timer
	cancel context.CancelFunc
	url    string
	// stalled is set when the watch cut the download off: the read then
	// fails with the context's error, which says nothing about why.
	stalled atomic.Bool
	// validator is what a resumed request sends as If-Range; empty when the
	// server takes no ranges.
	validator string
}

func (b *stallBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.stalled.Load() {
		err = fmt.Errorf("GET %s: no data for %s: %w", b.url, fetchIdle, err)
	}
	return n, err
}

func (b *stallBody) Close() error {
	b.timer.Stop()
	b.cancel()
	return b.body.Close()
}

// resumingBody is a download that picks itself up after a break.
type resumingBody struct {
	ctx       context.Context
	url       string
	cur       *stallBody
	off       int64
	validator string
	resumes   int
	// broken holds a break that came with data: the data goes out first.
	broken error
}

func (r *resumingBody) Read(p []byte) (int, error) {
	for {
		if r.broken == nil {
			n, err := r.cur.Read(p)
			r.off += int64(n)
			switch {
			case err == nil || errors.Is(err, io.EOF):
				return n, err
			case n > 0:
				r.broken = err
				return n, nil
			}
			r.broken = err
		}
		if r.validator == "" || r.resumes >= fetchStreamResumes || r.ctx.Err() != nil {
			return 0, r.broken
		}
		r.resumes++
		r.cur.Close() //nolint:errcheck // the stream that broke, replaced by the resumed one
		next, err := withRetry(r.ctx, func() (*stallBody, bool, error) { return fetchStreamFrom(r.ctx, r.url, r.off, r.validator) })
		if err != nil {
			return 0, fmt.Errorf("%w; resuming at byte %d: %v", r.broken, r.off, err)
		}
		r.cur, r.broken = next, nil
	}
}

func (r *resumingBody) Close() error { return r.cur.Close() }

// githubLatestTag names a public repository's latest release without the
// API and its anonymous quota: the site redirects /releases/latest to the tag.
func githubLatestTag(ctx context.Context, repo string) (string, error) {
	return githubLatestTagAt(ctx, "https://github.com", repo)
}

func githubLatestTagAt(ctx context.Context, site, repo string) (string, error) {
	return withRetry(ctx, func() (string, bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, site+"/"+repo+"/releases/latest", nil)
		if err != nil {
			return "", false, err
		}
		hc := *fetchClient
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		res, err := hc.Do(req)
		if err != nil {
			return "", ctx.Err() == nil, err
		}
		res.Body.Close()
		loc := res.Header.Get("Location")
		tag := loc[strings.LastIndex(loc, "/")+1:]
		if res.StatusCode/100 != 3 || !strings.Contains(loc, "/releases/tag/") || tag == "" {
			return "", res.StatusCode >= 500, fmt.Errorf("github.com/%s: no release found (%s)", repo, res.Status)
		}
		return tag, false, nil
	})
}

// stallReader pushes the stall timer back whenever data arrives.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (r *stallReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(fetchIdle)
	}
	return n, err
}

// installNginx adds the nginx.org repository, installs nginx, writes the
// panel's nginx.conf and snippets, validates and starts the service.
func (s *Server) installNginx(ctx context.Context, jc *jobs.Context) error {
	rel := s.profile.Release()
	web := s.profile.Web()
	if err := s.selinuxHostingPolicy(ctx, jc); err != nil {
		return err
	}
	jc.Progress(5, "nginx.org repository")
	switch s.profile.Family() {
	case osprofile.FamilyDebian:
		key, err := fetchText(ctx, nginxKeyURL)
		if err != nil {
			return fmt.Errorf("download nginx signing key: %w", err)
		}
		if !strings.HasPrefix(key, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
			return fmt.Errorf("unexpected content at %s", nginxKeyURL)
		}
		distro := strings.ToLower(rel.ID)
		if distro != "debian" && distro != "ubuntu" {
			distro = "debian"
		}
		files := []agent.FileSpec{
			{Path: "/etc/apt/keyrings/nginx.asc", Content: key, Mode: 0o644},
			{Path: "/etc/apt/sources.list.d/nginx.list", Content: fmt.Sprintf("deb [signed-by=/etc/apt/keyrings/nginx.asc] https://nginx.org/packages/%s %s nginx\n", distro, rel.Codename), Mode: 0o644},
			{Path: "/etc/apt/preferences.d/99nginx", Content: "Package: *\nPin: origin nginx.org\nPin: release o=nginx\nPin-Priority: 900\n", Mode: 0o644},
		}
		if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "stack:nginx"}); err != nil {
			return err
		}
		jc.Logf("repository: https://nginx.org/packages/%s %s nginx", distro, rel.Codename)
		jc.Progress(15, "apt-get update")
		res, err := s.agent.Pkg(ctx, "update-index")
		if err != nil {
			return err
		}
		logTail(jc, res.Output, 3)
	case osprofile.FamilyRHEL:
		repo := fmt.Sprintf("[nginx-stable]\nname=nginx stable repo\nbaseurl=https://nginx.org/packages/centos/%s/$basearch/\ngpgcheck=1\nenabled=1\ngpgkey=https://nginx.org/keys/nginx_signing.key\nmodule_hotfixes=true\n", rel.MajorVersion())
		if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: "/etc/yum.repos.d/nginx.repo", Content: repo, Mode: 0o644}}, Origin: "stack:nginx"}); err != nil {
			return err
		}
		jc.Logf("repository: nginx.org/packages/centos/%s", rel.MajorVersion())
	}

	jc.Progress(30, "installing nginx")
	res, err := s.agent.Pkg(ctx, "install", "nginx")
	if err != nil {
		return err
	}
	logTail(jc, res.Output, 5)
	jc.Logf("nginx package: %s", res.Installed["nginx"])

	jc.Progress(60, "web group and users")
	if _, err := s.agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: s.cfg.WebGroup, System: true}); err != nil {
		return err
	}
	if _, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: web.NginxUser, System: true, Groups: []string{s.cfg.WebGroup}}); err != nil {
		return err
	}
	if err := s.ensureACMEWebroot(); err != nil {
		return err
	}

	jc.Progress(70, "writing nginx configuration")
	mainConf, err := s.render.Render("nginx/nginx.conf.tmpl", render.NginxMain{User: web.NginxUser})
	if err != nil {
		return err
	}
	confDir := web.NginxConfDir
	files := []agent.FileSpec{
		{Path: path.Join(confDir, "nginx.conf"), Content: mainConf, Mode: 0o644},
		{Path: path.Join(confDir, "conf.d", "default.conf"), Content: "# Disabled by MonoPanel: default servers are generated per IP in monopanel/http.d/\n", Mode: 0o644},
		{Path: path.Join(confDir, "monopanel", "http.d", "README"), Content: "Generated http-level configuration (one default server per IP). Do not edit; custom http directives go to /etc/nginx/conf.d/.\n", Mode: 0o644},
		{Path: path.Join(confDir, "monopanel", "http.d", "00-monopanel.conf"), Content: "# Generated by MonoPanel\nmap $http_upgrade $connection_upgrade { default upgrade; '' close; }\n", Mode: 0o644},
		{Path: path.Join(confDir, "monopanel", "sites", "README"), Content: "Generated per-site server blocks. Custom directives go to sites/<domain>.d/*.conf.\n", Mode: 0o644},
	}
	defaults, err := s.nginxGlobalFiles()
	if err != nil {
		return err
	}
	files = append(files, defaults...)
	for _, f := range defaults {
		if strings.Contains(f.Path, "/http.d/") {
			jc.Logf("default server: %s", path.Base(f.Path))
		}
	}
	apply, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		// nginx -t (the validator) creates the pid file with the agent's
		// SELinux label; nginx's confined domain could not open it.
		Restore: []string{"/run/nginx.pid"},
		Files:   files, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Force: true, Origin: "stack:nginx",
	})
	if err != nil {
		return err
	}
	jc.Logf("configuration: %d written, %d unchanged, validated with %s", len(apply.Written), len(apply.Unchanged), web.NginxCheckArgv[0])

	s.firewalldOpen(ctx, jc.Logf, "80/tcp", "443/tcp")
	if err := s.ensureNginxBoot(ctx); err != nil {
		return err
	}
	jc.Logf("boot: net.ipv6.ip_nonlocal_bind=1 and a restart-on-failure drop-in, so an IPv6 address that arrives late does not leave nginx down")
	jc.Progress(90, "enabling service")
	if _, err := s.agent.Service(ctx, web.NginxService, "enable"); err != nil {
		return err
	}
	st, err := s.agent.Service(ctx, web.NginxService, "status")
	if err != nil {
		return err
	}
	jc.Logf("%s: %s (%s)", web.NginxService, st.Status.ActiveState, st.Status.SubState)
	if st.Status.ActiveState != "active" {
		return fmt.Errorf("%s is %s after install", web.NginxService, st.Status.ActiveState)
	}
	jc.Progress(100, "nginx ready")
	return nil
}

// localIPv6s lists the host's global IPv6 addresses: sites listen on the
// first one unless told otherwise, so link-local and unique-local ones are
// left out — nobody reaches a site through those.
func localIPv6s() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip := ipn.IP; ip.To4() == nil && ip.To16() != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
				out = append(out, ip.String())
			}
		}
	}
	return out
}

// bracket writes an IPv6 address the way nginx wants it in listen: [addr].
func bracket(ip string) string {
	if strings.Contains(ip, ":") {
		return "[" + ip + "]"
	}
	return ip
}

// localIPv4s lists the host's non-loopback IPv4 addresses (the API runs on
// the managed host itself).
func localIPv4s() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip := ipn.IP.To4(); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				out = append(out, ip.String())
			}
		}
	}
	return out
}

const settingApache = "stack.apache"

// installApache installs Apache 2.4 for the sites in Apache mode (Debian and
// Ubuntu): the package's binaries and modules, and nothing of its shared
// server. Each account with sites in Apache mode gets an Apache of its own
// (apache.go), which site.apply writes and starts; apache2.service stops and
// is disabled, and the distribution's Apache account leaves the web group.
// On a panel of 0.8.19 or older, or to repair, the sites in Apache mode are
// applied again first, each moving to (or refreshing) its account's Apache
// before the shared one stops.
func (s *Server) installApache(ctx context.Context, jc *jobs.Context) error {
	if s.profile.Family() != osprofile.FamilyDebian {
		return errors.New("Apache installation is implemented for Debian/Ubuntu so far")
	}
	if err := s.apachePortHeld(); err != nil {
		return err
	}
	jc.Progress(10, "installing apache2")
	res, err := s.agent.Pkg(ctx, "install", "apache2")
	if err != nil {
		return err
	}
	logTail(jc, res.Output, 3)
	jc.Logf("apache2 package: %s", res.Installed["apache2"])
	if err := s.db.SetSetting(ctx, settingApache, "installed"); err != nil {
		return err
	}
	jc.Progress(40, "sites in Apache mode")
	// Every site in Apache mode, those an upgrade moved already too: this is
	// the repair as well.
	if err := s.moveToApacheInstances(ctx, jc, true); err != nil {
		return err
	}
	jc.Logf("each account with sites in Apache mode gets its own Apache (monopanel-apache@<login>), its children running as the account, on 127.100.x.y:%d", confcheck.ApacheInstancePort)
	jc.Progress(100, "apache ready")
	return nil
}

// nginxGlobalFiles is what the panel owns in nginx besides the sites: the
// snippets every site includes and one default server per local address
// (ACME challenges, a redirect of the panel's own name to the panel port,
// 444 for everything else).
func (s *Server) nginxGlobalFiles() ([]agent.FileSpec, error) {
	confDir := s.profile.Web().NginxConfDir
	files := []agent.FileSpec{}
	snippets, err := s.render.Snippets("nginx/snippets")
	if err != nil {
		return nil, err
	}
	for name, content := range snippets {
		files = append(files, agent.FileSpec{Path: path.Join(confDir, "monopanel", "snippets", name), Content: content, Mode: 0o644})
	}
	for _, ip := range append(s.hostIPs(), s.hostIPv6s()...) {
		conf, err := s.render.Render("nginx/ip-default.conf.tmpl", render.IPDefault{IP: bracket(ip), PanelHost: s.cfg.Web.Hostname, PanelPort: s.panelPort()})
		if err != nil {
			return nil, err
		}
		files = append(files, agent.FileSpec{Path: path.Join(confDir, "monopanel", "http.d", "ip-"+ip+".conf"), Content: conf, Mode: 0o644})
	}
	return files, nil
}

// refreshDefaultServers re-renders the snippets and default servers at
// startup when nginx is installed: a panel update that changes a snippet, a
// changed panel hostname (mp config set web.hostname --restart) or a new
// address reaches nginx that way; unchanged files reload nothing.
func (s *Server) refreshDefaultServers(ctx context.Context) {
	q, err := s.agent.Pkg(ctx, "query", "nginx")
	if err != nil || q.Installed["nginx"] == "" {
		return
	}
	if err := s.ensureNginxBoot(ctx); err != nil {
		s.log.Warn("nginx boot settings", "err", err)
	}
	if gone := s.pruneDefaultServers(ctx); len(gone) > 0 {
		s.log.Warn("removed default servers of addresses this host no longer has", "files", gone)
	}
	if off := s.sitesOffHost(ctx); len(off) > 0 {
		s.log.Warn("sites listen on addresses this host does not have", "hint", s.offHostHint(off))
	}
	files, err := s.nginxGlobalFiles()
	if err != nil {
		s.log.Warn("default servers", "err", err)
		return
	}
	web := s.profile.Web()
	// Restore: nginx -t creates the pid file with the agent's SELinux label
	// when nginx is down, and nginx's own domain could not open it then.
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Restore: []string{"/run/nginx.pid"}, Origin: "stack:nginx"}); err != nil {
		s.log.Warn("default servers", "err", err)
	}
}

// ensureACMEWebroot makes the ACME directory and the webroot nginx serves
// the HTTP-01 challenges from, every level 0755: the nginx workers, another
// user, pass through to read the tokens. They lie below the panel's data
// directory, which is the API's own, so the API makes them itself — the
// agent writes nothing there (a root write below a directory the API owns
// could be steered by it).
func (s *Server) ensureACMEWebroot() error {
	web := s.acme.Webroot
	for _, d := range []string{s.acme.Dir, web, filepath.Join(web, ".well-known"), filepath.Join(web, ".well-known", "acme-challenge")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("ACME webroot: %w", err)
		}
		st, err := os.Stat(d)
		if err != nil {
			return fmt.Errorf("ACME webroot: %w", err)
		}
		if st.Mode().Perm() != 0o755 {
			if err := os.Chmod(d, 0o755); err != nil {
				return fmt.Errorf("ACME webroot %s: %w (mp setup gives the data directory back to %s)", d, err, s.cfg.ServiceUser)
			}
		}
	}
	return nil
}

// repoSchemes are the vendor sources the panel wrote over plain http up to
// 0.8.19, with what turns them to https: over http the keyring alone vouched
// for the packages, and the agent's checks now take https sources only.
var repoSchemes = []struct{ path, from, to string }{
	{"/etc/apt/sources.list.d/nginx.list", " http://nginx.org/", " https://nginx.org/"},
	{"/etc/apt/sources.list.d/mysql.list", " http://repo.mysql.com/", " https://repo.mysql.com/"},
	{"/etc/yum.repos.d/nginx.repo", "baseurl=http://nginx.org/", "baseurl=https://nginx.org/"},
}

// refreshRepoSchemes moves the panel's vendor sources to https.
func (s *Server) refreshRepoSchemes(ctx context.Context) {
	for _, r := range repoSchemes {
		b, err := os.ReadFile(r.path)
		if err != nil || !strings.Contains(string(b), r.from) {
			continue
		}
		content := strings.ReplaceAll(string(b), r.from, r.to)
		if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: r.path, Content: content, Mode: 0o644}}, Origin: "repositories"}); err != nil {
			s.log.Warn("repository over https", "file", r.path, "err", err)
		}
	}
}
