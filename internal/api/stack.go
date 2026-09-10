package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"errors"
	"monopanel/internal/agent"

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
		ac := apitypes.StackComponent{Name: "apache", Kind: "web"}
		if q, err := s.agent.Pkg(actx, "query", apachePkg); err == nil {
			if v, ok := q.Installed[apachePkg]; ok {
				ac.Installed, ac.Version = true, v
				if st, err := s.agent.Service(actx, web.ApacheService, "status"); err == nil {
					ac.Service = &st.Status
				}
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
		out.Body = append(out.Body, s.toolComponents(actx)...)
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

// fetchBytesN is fetchBytes with its own size limit (composer.phar is a few MB).
func fetchBytesN(ctx context.Context, url string, limit int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			}
		}
		b, retry, err := fetchOnce(ctx, url, limit)
		if err == nil {
			return b, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func fetchOnce(ctx context.Context, url string, limit int64) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode >= 500, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit))
	return b, err != nil, err
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
			{Path: "/etc/apt/sources.list.d/nginx.list", Content: fmt.Sprintf("deb [signed-by=/etc/apt/keyrings/nginx.asc] http://nginx.org/packages/%s %s nginx\n", distro, rel.Codename), Mode: 0o644},
			{Path: "/etc/apt/preferences.d/99nginx", Content: "Package: *\nPin: origin nginx.org\nPin: release o=nginx\nPin-Priority: 900\n", Mode: 0o644},
		}
		if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "stack:nginx"}); err != nil {
			return err
		}
		jc.Logf("repository: http://nginx.org/packages/%s %s nginx", distro, rel.Codename)
		jc.Progress(15, "apt-get update")
		res, err := s.agent.Pkg(ctx, "update-index")
		if err != nil {
			return err
		}
		logTail(jc, res.Output, 3)
	case osprofile.FamilyRHEL:
		repo := fmt.Sprintf("[nginx-stable]\nname=nginx stable repo\nbaseurl=http://nginx.org/packages/centos/%s/$basearch/\ngpgcheck=1\nenabled=1\ngpgkey=https://nginx.org/keys/nginx_signing.key\nmodule_hotfixes=true\n", rel.MajorVersion())
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
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{
		{Path: "/var/lib/monopanel/acme", Mode: 0o755, Owner: s.cfg.ServiceUser, Group: s.cfg.ServiceGroup},
		{Path: "/var/lib/monopanel/acme/webroot", Mode: 0o755, Owner: s.cfg.ServiceUser, Group: s.cfg.ServiceGroup},
		{Path: "/var/lib/monopanel/acme/webroot/.well-known/acme-challenge", Mode: 0o755, Owner: s.cfg.ServiceUser, Group: s.cfg.ServiceGroup},
	}}); err != nil {
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

	s.firewalldOpen(ctx, jc, "80/tcp", "443/tcp")
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

// installApache installs Apache 2.4 behind nginx on 127.0.0.1:8080 with
// mpm_event + mod_proxy_fcgi (Debian/Ubuntu; EL in a later iteration).
func (s *Server) installApache(ctx context.Context, jc *jobs.Context) error {
	if s.profile.Family() != osprofile.FamilyDebian {
		return errors.New("Apache installation is implemented for Debian/Ubuntu so far")
	}
	web := s.profile.Web()
	jc.Progress(10, "installing apache2")
	res, err := s.agent.Pkg(ctx, "install", "apache2")
	if err != nil {
		return err
	}
	logTail(jc, res.Output, 3)
	jc.Logf("apache2 package: %s", res.Installed["apache2"])

	jc.Progress(40, "modules")
	for _, m := range []string{"mpm_prefork", "mpm_worker", "php8.4", "php8.3", "php8.2", "php8.1"} {
		s.agent.ApacheCtl(ctx, "dismod", m) //nolint:errcheck // best effort: the module may not be enabled
	}
	for _, m := range []string{"mpm_event", "proxy", "proxy_fcgi", "rewrite", "remoteip", "headers", "expires", "setenvif", "deflate", "dir", "alias", "mime"} {
		if _, err := s.agent.ApacheCtl(ctx, "enmod", m); err != nil {
			return err
		}
	}
	s.agent.ApacheCtl(ctx, "dissite", "000-default") //nolint:errcheck // best effort: apachectl reports the result
	if _, err := s.agent.EnsureGroup(ctx, &agent.EnsureGroupRequest{Name: s.cfg.WebGroup, System: true}); err != nil {
		return err
	}
	if _, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: web.ApacheUser, System: true, Groups: []string{s.cfg.WebGroup}}); err != nil {
		return err
	}

	jc.Progress(70, "configuration")
	confDir := web.ApacheConfDir
	main, err := s.render.Render("apache/httpd.conf.tmpl", render.ApacheMain{Backend: "127.0.0.1:8080", SitesDir: path.Join(confDir, "monopanel", "sites")})
	if err != nil {
		return err
	}
	files := []agent.FileSpec{
		{Path: path.Join(confDir, "ports.conf"), Content: "# Managed by MonoPanel: Apache listens on loopback only; nginx terminates client connections.\nListen 127.0.0.1:8080\n", Mode: 0o644},
		{Path: path.Join(confDir, "monopanel", "httpd.conf"), Content: main, Mode: 0o644},
		{Path: path.Join(confDir, "monopanel", "sites", "README"), Content: "Generated per-site virtual hosts. Custom directives go to sites/<domain>.d/*.conf.\n", Mode: 0o644},
		{Path: path.Join(confDir, "conf-available", "monopanel.conf"), Content: "IncludeOptional " + path.Join(confDir, "monopanel", "httpd.conf") + "\n", Mode: 0o644},
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "stack:apache"}); err != nil {
		return err
	}
	if _, err := s.agent.ApacheCtl(ctx, "enconf", "monopanel"); err != nil {
		return err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: [][]string{web.ApacheCheckArgv}, Restart: []string{web.ApacheService}, Force: true, Origin: "stack:apache"}); err != nil {
		return err
	}
	if _, err := s.agent.Service(ctx, web.ApacheService, "enable"); err != nil {
		return err
	}
	st, err := s.agent.Service(ctx, web.ApacheService, "status")
	if err != nil {
		return err
	}
	jc.Logf("%s: %s (%s) on 127.0.0.1:8080", web.ApacheService, st.Status.ActiveState, st.Status.SubState)
	if st.Status.ActiveState != "active" {
		return fmt.Errorf("%s is %s after install", web.ApacheService, st.Status.ActiveState)
	}
	if err := s.db.SetSetting(ctx, settingApache, "installed"); err != nil {
		return err
	}
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
	for _, ip := range localIPv4s() {
		conf, err := s.render.Render("nginx/ip-default.conf.tmpl", render.IPDefault{IP: ip, PanelHost: s.cfg.Web.Hostname, PanelPort: s.panelPort()})
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
	files, err := s.nginxGlobalFiles()
	if err != nil {
		s.log.Warn("default servers", "err", err)
		return
	}
	web := s.profile.Web()
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: [][]string{web.NginxCheckArgv}, Reload: []string{web.NginxService}, Origin: "stack:nginx"}); err != nil {
		s.log.Warn("default servers", "err", err)
	}
}
