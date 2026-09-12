package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

var (
	backendRe  = regexp.MustCompile(`^http://(127\.0\.0\.1|localhost|10\.[0-9.]+|192\.168\.[0-9.]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9.]+):[0-9]{2,5}$|^http://unix:/[^\s:]+:$`)
	iniKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_.]{1,63}$`)
	iniValueRe = regexp.MustCompile(`^[A-Za-z0-9_./:@+, -]{0,255}$`)
	docrootRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]{0,127}$`)
)

// php_value keys a site may override; php_admin_value keys stay panel-owned.
var allowedIniKeys = map[string]bool{
	"memory_limit": true, "upload_max_filesize": true, "post_max_size": true, "max_execution_time": true, "max_input_time": true,
	"max_input_vars": true, "date.timezone": true, "display_errors": true, "error_reporting": true, "short_open_tag": true,
	"session.gc_maxlifetime": true, "default_charset": true, "mbstring.internal_encoding": true, "opcache.enable": true,
	"opcache.revalidate_freq": true, "opcache.validate_timestamps": true, "zlib.output_compression": true, "allow_url_fopen": true,
	"precision": true, "serialize_precision": true, "output_buffering": true, "realpath_cache_size": true, "log_errors": true,
	"max_file_uploads": true, "expose_php": true, "html_errors": true, "default_socket_timeout": true, "max_input_nesting_level": true,
	"session.cookie_secure": true, "session.cookie_httponly": true, "session.cookie_samesite": true, "session.use_strict_mode": true,
	"opcache.jit": true, "opcache.jit_buffer_size": true, "opcache.memory_consumption": true, "opcache.interned_strings_buffer": true,
	"opcache.max_accelerated_files": true, "mbstring.language": true, "intl.default_locale": true, "pcre.backtrack_limit": true, "pcre.recursion_limit": true,
}

type sitesOutput struct {
	Body []*store.Site
}

type siteOutput struct {
	Body *store.Site
}

type siteDomainInput struct {
	Domain string `path:"domain"`
}

type siteDeleteInput struct {
	Domain string `path:"domain"`
	Purge  bool   `query:"purge" doc:"Also delete the site directory"`
}

type createSiteInput struct {
	Body apitypes.SiteRequest
}

type updateSiteInput struct {
	Domain string `path:"domain"`
	Body   apitypes.SiteUpdateRequest
}

type siteJobOutput struct {
	Status int
	Body   apitypes.SiteWithJob
}

type sitePayload struct {
	SiteID int64 `json:"site_id"`
	Purge  bool  `json:"purge,omitempty"`
}

func (s *Server) loadSiteFor(ctx context.Context, domain string) (*store.Site, error) {
	site, err := s.db.GetSiteByDomain(ctx, acme.NormalizeName(domain))
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("site not found")
	}
	if err != nil {
		return nil, err
	}
	p := principalFrom(ctx)
	if p.Role != store.RoleAdmin && p.UserID != site.UserID {
		return nil, huma.Error404NotFound("site not found")
	}
	return site, nil
}

func validateIni(ini map[string]string) error {
	for k, v := range ini {
		if !iniKeyRe.MatchString(k) || !allowedIniKeys[k] {
			return fmt.Errorf("php_ini: key %q is not allowed", k)
		}
		if !iniValueRe.MatchString(v) {
			return fmt.Errorf("php_ini: invalid value for %q", k)
		}
	}
	return nil
}

func (s *Server) validateNames(ctx context.Context, domain string, aliases []string, selfID int64) ([]string, error) {
	if err := acme.ValidateName(domain); err != nil {
		return nil, err
	}
	seen := map[string]bool{domain: true}
	out := []string{}
	for _, a := range aliases {
		a = acme.NormalizeName(a)
		if a == "" || seen[a] {
			continue
		}
		if err := acme.ValidateName(a); err != nil {
			return nil, fmt.Errorf("alias %s: %w", a, err)
		}
		seen[a] = true
		out = append(out, a)
	}
	for name := range seen {
		others, err := s.db.FindSitesByName(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, o := range others {
			if o.ID != selfID {
				return nil, fmt.Errorf("%s is already used by site %s", name, o.Domain)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Server) checkPHPInstalled(ctx context.Context, version string) error {
	v, err := s.db.GetPHPVersion(ctx, version)
	if errors.Is(err, store.ErrNotFound) || (err == nil && v.Status != store.PHPInstalled) {
		return fmt.Errorf("PHP %s is not installed (mp php install %s)", version, version)
	}
	return err
}

func (s *Server) checkMode(ctx context.Context, mode string) error {
	if mode == store.ModeProxy {
		return nil
	}
	if mode == store.ModeApache {
		if v, _ := s.db.GetSetting(ctx, settingApache); v != "installed" {
			return errors.New("Apache is not installed (mp stack install apache)")
		}
	}
	return nil
}

func (s *Server) enqueueSiteApply(ctx context.Context, site *store.Site, actor string) (int64, error) {
	job, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: site.ID}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(actor))
	if err != nil {
		return 0, err
	}
	return job.ID, nil
}

func (s *Server) registerSites() {
	huma.Register(s.api, huma.Operation{
		OperationID: "sites-list", Method: http.MethodGet, Path: "/sites", Summary: "List sites (admins: all, users: own)", Tags: []string{"sites"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*sitesOutput, error) {
		p := principalFrom(ctx)
		var uid int64
		if p.Role != store.RoleAdmin {
			uid = p.UserID
		}
		list, err := s.db.ListSites(ctx, uid)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []*store.Site{}
		}
		return &sitesOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-create", Method: http.MethodPost, Path: "/sites", Summary: "Create a site (async: pool, nginx config, welcome page, certificate)", Tags: []string{"sites"},
		Security: secured, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *createSiteInput) (*siteJobOutput, error) {
		p := principalFrom(ctx)
		b := in.Body
		var owner *store.User
		var err error
		switch {
		case p.Role == store.RoleAdmin && b.User != "":
			owner, err = s.db.GetUserByLogin(ctx, b.User)
		case p.UserID != 0:
			owner, err = s.db.GetUserByID(ctx, p.UserID)
		default:
			return nil, huma.Error422UnprocessableEntity("user is required (which account owns the site)")
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error422UnprocessableEntity("user not found")
		}
		if err != nil {
			return nil, err
		}
		if owner.Role != store.RoleUser || owner.UnixUID == nil || owner.Status != store.UserActive {
			return nil, huma.Error422UnprocessableEntity("owner must be an active user with a provisioned unix account")
		}
		domain := acme.NormalizeName(b.Domain)
		aliases := b.Aliases
		if b.WWW {
			aliases = append(aliases, "www."+domain)
		}
		names, err := s.validateNames(ctx, domain, aliases, 0)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		phpVersion := b.PHPVersion
		if b.Mode == store.ModeProxy {
			if !backendRe.MatchString(b.Backend) {
				return nil, huma.Error422UnprocessableEntity("proxy mode needs backend like http://127.0.0.1:3000 or http://unix:/run/app.sock:")
			}
			phpVersion = ""
		} else {
			if phpVersion == "" {
				versions, _ := s.db.ListPHPVersions(ctx)
				for _, v := range versions {
					if v.Status == store.PHPInstalled {
						phpVersion = v.Version
					}
				}
				if phpVersion == "" {
					return nil, huma.Error422UnprocessableEntity("no PHP branch installed (mp php install 8.4)")
				}
			}
			if err := s.checkPHPInstalled(ctx, phpVersion); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		if err := s.checkMode(ctx, b.Mode); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if b.Docroot != "" && (!docrootRe.MatchString(b.Docroot) || strings.Contains(b.Docroot, "..")) {
			return nil, huma.Error422UnprocessableEntity("invalid docroot")
		}
		if err := validateIni(b.PHPIni); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		site := &store.Site{UserID: owner.ID, Domain: domain, Aliases: names, Mode: b.Mode, Backend: b.Backend, PHPVersion: phpVersion, Docroot: strings.Trim(b.Docroot, "/"), IP: b.IP, SSL: b.SSL,
			HTTP2: true, RedirectHTTPS: true, RedirectWWW: b.RedirectWWW, StaticByNginx: true, FPMPM: b.FPMPM, FPMMaxChildren: b.FPMMaxChildren, PHPIni: b.PHPIni, ClientMaxBody: b.ClientMaxBody}
		if site.AllowFrom, err = normalizeAllowFrom(b.AllowFrom); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if site.Preset, err = normalizePreset(b.Preset); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if site.Preset == presetBitrix && b.ClientMaxBody == "" {
			site.ClientMaxBody = "256m"
		}
		if b.HTTP2 != nil {
			site.HTTP2 = *b.HTTP2
		}
		if b.HTTP3 != nil {
			site.HTTP3 = *b.HTTP3
		}
		if b.RedirectHTTPS != nil {
			site.RedirectHTTPS = *b.RedirectHTTPS
		}
		if b.StaticByNginx != nil {
			site.StaticByNginx = *b.StaticByNginx
		}
		if b.AllowExec != nil {
			site.AllowExec = *b.AllowExec
		}
		if site.IP == "" {
			if ips := localIPv4s(); len(ips) > 0 {
				site.IP = ips[0]
			}
		}
		if err := s.db.CreateSite(ctx, site); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("site already exists")
			}
			return nil, err
		}
		site.Login = owner.Login
		jobID, err := s.enqueueSiteApply(ctx, site, p.Login)
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.create", Target: domain, IP: requestInfo(ctx).IP, Details: map[string]any{"user": owner.Login, "php": phpVersion, "mode": site.Mode}})
		return &siteJobOutput{Status: http.StatusAccepted, Body: apitypes.SiteWithJob{Site: site, JobID: jobID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-get", Method: http.MethodGet, Path: "/sites/{domain}", Summary: "Get a site", Tags: []string{"sites"}, Security: secured,
	}, func(ctx context.Context, in *siteDomainInput) (*siteOutput, error) {
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		return &siteOutput{Body: site}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-update", Method: http.MethodPatch, Path: "/sites/{domain}", Summary: "Change site settings and re-apply (async)", Tags: []string{"sites"}, Security: secured, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *updateSiteInput) (*siteJobOutput, error) {
		p := principalFrom(ctx)
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		b := in.Body
		if b.Aliases != nil {
			names, err := s.validateNames(ctx, site.Domain, *b.Aliases, site.ID)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			site.Aliases = names
		}
		if b.Mode != "" {
			if err := s.checkMode(ctx, b.Mode); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			site.Mode = b.Mode
		}
		if b.Backend != "" {
			if !backendRe.MatchString(b.Backend) {
				return nil, huma.Error422UnprocessableEntity("backend must be http://127.0.0.1:PORT or http://unix:/path.sock:")
			}
			site.Backend = b.Backend
		}
		if site.Mode == store.ModeProxy && site.Backend == "" {
			return nil, huma.Error422UnprocessableEntity("proxy mode needs a backend")
		}
		if site.Mode != store.ModeProxy && site.PHPVersion == "" && b.PHPVersion == "" {
			versions, _ := s.db.ListPHPVersions(ctx)
			for _, v := range versions {
				if v.Status == store.PHPInstalled {
					site.PHPVersion = v.Version
				}
			}
		}
		if b.PHPVersion != "" {
			if err := s.checkPHPInstalled(ctx, b.PHPVersion); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			site.PHPVersion = b.PHPVersion
		}
		if b.Docroot != nil {
			d := strings.Trim(*b.Docroot, "/")
			if d != "" && (!docrootRe.MatchString(d) || strings.Contains(d, "..")) {
				return nil, huma.Error422UnprocessableEntity("invalid docroot")
			}
			site.Docroot = d
		}
		if b.IP != "" {
			site.IP = b.IP
		}
		if b.SSL != "" {
			site.SSL = b.SSL
		}
		if b.HTTP2 != nil {
			site.HTTP2 = *b.HTTP2
		}
		if b.HTTP3 != nil {
			site.HTTP3 = *b.HTTP3
		}
		if b.RedirectHTTPS != nil {
			site.RedirectHTTPS = *b.RedirectHTTPS
		}
		if b.RedirectWWW != "" {
			site.RedirectWWW = b.RedirectWWW
		}
		if b.StaticByNginx != nil {
			site.StaticByNginx = *b.StaticByNginx
		}
		if b.FPMPM != "" {
			site.FPMPM = b.FPMPM
		}
		if b.FPMMaxChildren > 0 {
			site.FPMMaxChildren = b.FPMMaxChildren
		}
		if b.PHPIni != nil {
			if err := validateIni(b.PHPIni); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			for k, v := range b.PHPIni {
				if v == "" {
					delete(site.PHPIni, k)
				} else {
					site.PHPIni[k] = v
				}
			}
		}
		if b.AllowExec != nil {
			site.AllowExec = *b.AllowExec
		}
		if b.ClientMaxBody != "" {
			site.ClientMaxBody = b.ClientMaxBody
		}
		if b.AllowFrom != nil {
			if site.AllowFrom, err = normalizeAllowFrom(*b.AllowFrom); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		if b.Preset != nil {
			if site.Preset, err = normalizePreset(*b.Preset); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		if site.Status == store.SiteError {
			site.Status = store.SitePending
		}
		if err := s.db.UpdateSite(ctx, site); err != nil {
			return nil, err
		}
		jobID, err := s.enqueueSiteApply(ctx, site, p.Login)
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.update", Target: site.Domain, IP: requestInfo(ctx).IP})
		return &siteJobOutput{Status: http.StatusAccepted, Body: apitypes.SiteWithJob{Site: site, JobID: jobID}}, nil
	})

	for _, action := range []string{"apply", "suspend", "unsuspend", "fix"} {
		action := action
		summary := strings.ToUpper(action[:1]) + action[1:] + " a site (async)"
		if action == "fix" {
			summary = "Fix the owner, modes, ACLs and SELinux labels of a site's files (async)"
		}
		huma.Register(s.api, huma.Operation{
			OperationID: "sites-" + action, Method: http.MethodPost, Path: "/sites/{domain}/" + action, Summary: summary, Tags: []string{"sites"}, Security: secured, DefaultStatus: http.StatusAccepted,
		}, func(ctx context.Context, in *siteDomainInput) (*siteJobOutput, error) {
			p := principalFrom(ctx)
			site, err := s.loadSiteFor(ctx, in.Domain)
			if err != nil {
				return nil, err
			}
			if action == "fix" {
				job, err := s.jobs.Enqueue(ctx, "site.fix", sitePayload{SiteID: site.ID}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(p.Login))
				if err != nil {
					return nil, err
				}
				s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.fix", Target: site.Domain, IP: requestInfo(ctx).IP})
				return &siteJobOutput{Status: http.StatusAccepted, Body: apitypes.SiteWithJob{Site: site, JobID: job.ID}}, nil
			}
			switch action {
			case "suspend":
				if p.Role != store.RoleAdmin {
					return nil, huma.Error403Forbidden("administrator role required")
				}
				site.Status = store.SiteSuspended
			case "unsuspend":
				if p.Role != store.RoleAdmin {
					return nil, huma.Error403Forbidden("administrator role required")
				}
				site.Status = store.SitePending
			default:
				if site.Status == store.SiteError {
					site.Status = store.SitePending
				}
			}
			if err := s.db.UpdateSite(ctx, site); err != nil {
				return nil, err
			}
			jobID, err := s.enqueueSiteApply(ctx, site, p.Login)
			if err != nil {
				return nil, err
			}
			s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site." + action, Target: site.Domain, IP: requestInfo(ctx).IP})
			return &siteJobOutput{Status: http.StatusAccepted, Body: apitypes.SiteWithJob{Site: site, JobID: jobID}}, nil
		})
	}

	huma.Register(s.api, huma.Operation{
		OperationID: "sites-delete", Method: http.MethodDelete, Path: "/sites/{domain}", Summary: "Delete a site (async; purge=true also removes files)", Tags: []string{"sites"}, Security: secured, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *siteDeleteInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		site, err := s.loadSiteFor(ctx, in.Domain)
		if err != nil {
			return nil, err
		}
		site.Status = store.SiteDeleting
		if err := s.db.UpdateSite(ctx, site); err != nil {
			return nil, err
		}
		job, err := s.jobs.Enqueue(ctx, "site.delete", sitePayload{SiteID: site.ID, Purge: in.Purge}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.delete", Target: site.Domain, IP: requestInfo(ctx).IP, Details: map[string]any{"purge": in.Purge}})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

// siteLayout is everything the templates need for one site.
type siteLayout struct {
	site       *store.Site
	user       *store.User
	php        *osprofile.PHPLayout
	home       string
	data       string
	siteRoot   string
	docroot    string
	socket     string
	nginxConf  string
	nginxDir   string
	poolConf   string
	apacheConf string
}

func (s *Server) layoutFor(site *store.Site, user *store.User) *siteLayout {
	web := s.profile.Web()
	home := user.Home
	if home == "" {
		home = path.Join(s.cfg.WWWRoot, user.Login)
	}
	l := &siteLayout{site: site, user: user, home: home, data: path.Join(home, "data")}
	l.siteRoot = path.Join(l.data, "www", site.Domain)
	l.docroot = l.siteRoot
	if site.Docroot != "" {
		l.docroot = path.Join(l.siteRoot, site.Docroot)
	}
	l.socket = path.Join(s.cfg.RunDir, "php", site.Domain+".sock")
	l.nginxConf = path.Join(web.NginxConfDir, "monopanel", "sites", site.Domain+".conf")
	l.nginxDir = path.Join(web.NginxConfDir, "monopanel", "sites", site.Domain+".d")
	l.apacheConf = path.Join(web.ApacheConfDir, "monopanel", "sites", site.Domain+".conf")
	if site.PHPVersion != "" {
		if l.php = osprofile.PHP(s.profile, site.PHPVersion); l.php != nil {
			l.poolConf = path.Join(l.php.PoolDir, site.Domain+".conf")
		}
	}
	return l
}

func (s *Server) siteFail(ctx context.Context, site *store.Site, err error) error {
	_ = s.db.SetSiteStatus(context.WithoutCancel(ctx), site.ID, store.SiteError, err.Error())
	return err
}

// certForSite returns a valid certificate covering the site, if any.
func (s *Server) certForSite(ctx context.Context, site *store.Site) *store.Certificate {
	var c *store.Certificate
	var err error
	if site.CertificateID != nil {
		c, err = s.db.GetCertificate(ctx, *site.CertificateID)
	}
	if c == nil || err != nil {
		c, err = s.db.GetCertificateByName(ctx, site.Domain)
		if err != nil {
			return nil
		}
	}
	// Files on disk decide: a failed renewal must not switch a site to HTTP
	// while the previous certificate is still valid.
	if c.CertPath == "" || c.KeyPath == "" || c.NotAfter == nil || time.Now().After(*c.NotAfter) || !containsName(c.Names, site.Domain) {
		return nil
	}
	return c
}

// jobSiteApply renders and applies the pool, nginx (and Apache) configuration.
// siteTree makes the client's directories with their modes and gives the
// web group its ACLs: on creation, and again by site.fix after someone
// reshaped the tree by hand.
func (s *Server) siteTree(ctx context.Context, jc *jobs.Context, l *siteLayout, login string) error {
	dirs := []agent.DirSpec{
		{Path: l.home, Mode: 0o710, Owner: login, Group: login},
		{Path: l.data, Mode: 0o750, Owner: login, Group: login},
		{Path: path.Join(l.data, "www"), Mode: 0o750, Owner: login, Group: login},
		{Path: l.siteRoot, Mode: 0o750, Owner: login, Group: login},
		{Path: path.Join(l.data, "logs"), Mode: 0o750, Owner: login, Group: login},
		{Path: path.Join(l.data, "tmp"), Mode: 0o700, Owner: login, Group: login},
		{Path: path.Join(l.data, "tmp", "sess"), Mode: 0o700, Owner: login, Group: login},
		{Path: path.Join(l.data, "bin"), Mode: 0o750, Owner: login, Group: login},
		{Path: path.Join(s.cfg.RunDir, "php"), Mode: 0o755, Owner: "root", Group: "root"},
	}
	if l.docroot != l.siteRoot {
		dirs = append(dirs, agent.DirSpec{Path: l.docroot, Mode: 0o750, Owner: login, Group: login})
	}
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: dirs}); err != nil {
		return err
	}
	if err := s.ensurePackages(ctx, jc, "acl"); err != nil {
		return err
	}
	webACL := "g:" + s.cfg.WebGroup
	for _, d := range []string{l.home, l.data, path.Join(l.data, "www")} {
		if err := s.agent.SetACL(ctx, &agent.SetACLRequest{Path: d, Entries: []string{webACL + ":x"}}); err != nil {
			return err
		}
	}
	return s.agent.SetACL(ctx, &agent.SetACLRequest{Path: l.siteRoot, Entries: []string{webACL + ":rX"}, Default: true, Recursive: true})
}

// jobSiteFix puts a site's files back in order after someone worked on them
// as root: directories and ACLs as on creation, the client as the owner of
// the whole site tree, SELinux labels the policy expects (files copied with
// cp -a from /root keep admin_home_t and nginx answers 403 for them).
func (s *Server) jobSiteFix(ctx context.Context, jc *jobs.Context) error {
	var p sitePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	site, err := s.db.GetSite(ctx, p.SiteID)
	if err != nil {
		return err
	}
	user, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	if user.UnixUID == nil {
		return errors.New("owner has no unix account yet")
	}
	l := s.layoutFor(site, user)
	jc.Progress(10, "directories and ACLs")
	if err := s.siteTree(ctx, jc, l, user.Login); err != nil {
		return err
	}
	jc.Progress(40, "owner")
	ch, err := s.agent.Chown(ctx, &agent.ChownRequest{Path: l.siteRoot, Owner: user.Login, Group: user.Login, Recursive: true})
	if err != nil {
		return err
	}
	jc.Logf("owner %s under %s: %d objects changed", user.Login, l.siteRoot, ch.Changed)
	jc.Progress(70, "SELinux labels")
	s.relabel(ctx, jc, l.data, true)
	jc.Progress(100, "fixed")
	return nil
}

func (s *Server) jobSiteApply(ctx context.Context, jc *jobs.Context) error {
	var p sitePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	site, err := s.db.GetSite(ctx, p.SiteID)
	if err != nil {
		return err
	}
	user, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	if user.UnixUID == nil {
		return s.siteFail(ctx, site, errors.New("owner has no unix account yet"))
	}
	proxy := site.Mode == store.ModeProxy
	if !proxy {
		if err := s.checkPHPInstalled(ctx, site.PHPVersion); err != nil {
			return s.siteFail(ctx, site, err)
		}
	}
	l := s.layoutFor(site, user)
	if l.php == nil && !proxy {
		return s.siteFail(ctx, site, errors.New("no PHP layout for this OS"))
	}
	web := s.profile.Web()
	if site.IP == "" {
		if ips := localIPv4s(); len(ips) > 0 {
			site.IP = ips[0]
		}
	}
	login := user.Login
	suspended := site.Status == store.SiteSuspended

	jc.Progress(10, "directories and permissions")
	if err := s.siteTree(ctx, jc, l, login); err != nil {
		return s.siteFail(ctx, site, err)
	}
	if !proxy {
		// Static placeholder for an empty docroot: only the domain, no server details.
		welcome, err := s.render.Render("site/index.html.tmpl", render.Welcome{Domain: site.Domain})
		if err != nil {
			return err
		}
		if res, err := s.agent.EnsureFile(ctx, &agent.EnsureFileRequest{Path: path.Join(l.docroot, "index.html"), Content: welcome, Mode: 0o644, Owner: login, OnlyIfDirEmpty: true}); err != nil {
			return s.siteFail(ctx, site, err)
		} else if res.Written {
			jc.Logf("placeholder page: %s/index.html", l.docroot)
		}
		if err := s.agent.EnsureSymlink(ctx, &agent.EnsureSymlinkRequest{Path: path.Join(l.data, "bin", "php"), Target: l.php.CLIBinary, Owner: login, OnlyIfMissing: true}); err != nil {
			jc.Logf("warning: php CLI symlink: %v", err)
		}
	}

	jc.Progress(35, "certificate")
	cert := (*store.Certificate)(nil)
	if site.SSL == "auto" {
		cert = s.certForSite(ctx, site)
	}
	tls := cert != nil
	if tls {
		id := cert.ID
		site.CertificateID = &id
		jc.Logf("TLS: %s (%s, until %s)", cert.Name, cert.Issuer, cert.NotAfter.Format("2006-01-02"))
	} else if site.SSL == "auto" {
		jc.Logf("TLS: no certificate yet; serving HTTP and ordering one")
	}

	jc.Progress(55, "rendering configuration")
	values := s.poolValues(ctx, site, tls)
	terminate := 150
	if v, ok := site.PHPIni["max_execution_time"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			terminate = n + 30
		}
	}
	maxChildren := site.FPMMaxChildren
	if maxChildren <= 0 {
		maxChildren = 8
	}
	disable := render.DefaultDisableFunctions
	if site.AllowExec {
		disable = ""
	}
	pool := render.Pool{
		Name: site.Domain, User: login, Group: login, Socket: l.socket, ListenGroup: s.cfg.WebGroup, PM: site.FPMPM, MaxChildren: maxChildren,
		StartServers: max(2, maxChildren/4), MinSpare: max(1, maxChildren/4), MaxSpare: max(2, maxChildren/2), TerminateTimeout: terminate,
		Home: l.home, DataDir: l.data, TmpDir: path.Join(l.data, "tmp"), LogDir: path.Join(l.data, "logs"), BinDir: path.Join(l.data, "bin"),
		SendmailFrom: "noreply@" + site.Domain, DisableFunctions: disable, Values: values,
		OpenBasedir: site.Preset != presetBitrix,
	}
	poolConf := ""
	if !proxy {
		if poolConf, err = s.render.Render("php-fpm/pool.conf.tmpl", pool); err != nil {
			return err
		}
	}
	rs := render.Site{
		Domain: site.Domain, Aliases: site.Aliases, Mode: site.Mode, IP: site.IP, TLS: tls, HTTP2: site.HTTP2, HTTP3: site.HTTP3 && tls,
		RedirectHTTPS: site.RedirectHTTPS, RedirectWWW: site.RedirectWWW, Docroot: l.docroot, LogDir: path.Join(l.data, "logs"),
		IncludeDir: "monopanel/sites/" + site.Domain + ".d", ClientMaxBodySize: strings.ToLower(site.ClientMaxBody), StaticByNginx: site.StaticByNginx,
		ApacheBackend: "127.0.0.1:8080", FPMSocket: l.socket, ProxyTimeout: terminate, Backend: site.Backend, AllowFrom: site.AllowFrom,
		HSTS: tls && site.RedirectHTTPS, Preset: site.Preset,
	}
	if len(site.AllowFrom) > 0 {
		jc.Logf("access limited to %s", strings.Join(site.AllowFrom, ", "))
	}
	if site.Preset != "" && !proxy {
		jc.Logf("preset: %s (nginx locations + PHP defaults)", presetTitle(site.Preset))
	}
	if tls {
		rs.CertPath, rs.KeyPath = cert.CertPath, cert.KeyPath
	}
	nginxTmpl := "nginx/site.conf.tmpl"
	if suspended {
		nginxTmpl = "nginx/site-suspended.conf.tmpl"
	}
	nginxConf, err := s.renderSiteNginx(nginxTmpl, rs)
	if err != nil {
		return err
	}
	files := []agent.FileSpec{
		{Path: l.nginxConf, Content: nginxConf, Mode: 0o644},
		{Path: path.Join(l.nginxDir, "README"), Content: "Custom nginx directives for " + site.Domain + " (inside server {}): *.conf files here are included by the panel and never overwritten.\n", Mode: 0o644},
	}
	validate := [][]string{web.NginxCheckArgv}
	reload := []string{web.NginxService}
	remove := []string{}
	if suspended || proxy {
		if l.poolConf != "" {
			remove = append(remove, l.poolConf)
		}
	} else {
		files = append(files, agent.FileSpec{Path: l.poolConf, Content: poolConf, Mode: 0o644})
		validate = append(validate, l.php.FPMCheckArgv)
		reload = append(reload, l.php.FPMService)
	}
	if site.Mode == store.ModeApache && !suspended {
		rs.IncludeDir = path.Join(web.ApacheConfDir, "monopanel", "sites", site.Domain+".d")
		apacheConf, err := s.render.Render("apache/site.conf.tmpl", rs)
		if err != nil {
			return err
		}
		files = append(files, agent.FileSpec{Path: l.apacheConf, Content: apacheConf, Mode: 0o644}, agent.FileSpec{Path: path.Join(rs.IncludeDir, "README"), Content: "Custom Apache directives for " + site.Domain + " (inside <VirtualHost>).\n", Mode: 0o644})
		validate = append(validate, web.ApacheCheckArgv)
		reload = append(reload, web.ApacheService)
	} else {
		remove = append(remove, l.apacheConf)
	}
	// stale pools of other PHP branches (after a version switch)
	for _, v := range osprofileVersions() {
		if v != site.PHPVersion {
			if other := osprofile.PHP(s.profile, v); other != nil {
				remove = append(remove, path.Join(other.PoolDir, site.Domain+".conf"))
			}
		}
	}
	if len(remove) > 0 {
		// Освобождение сокета старой ветки должно случиться до того, как новая
		// попытается его занять: пул слушает один и тот же путь, и мастер новой
		// версии не стартует, пока файл держит мастер прежней.
		staleReload := []string{}
		probe, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: remove})
		if err != nil {
			return s.siteFail(ctx, site, err)
		}
		if len(probe.Removed) > 0 {
			jc.Logf("removed stale: %s", strings.Join(probe.Removed, ", "))
			for _, v := range osprofileVersions() {
				if other := osprofile.PHP(s.profile, v); other != nil && v != site.PHPVersion {
					for _, r := range probe.Removed {
						if strings.HasPrefix(r, other.PoolDir+"/") {
							if _, err := s.db.GetPHPVersion(ctx, v); err == nil {
								staleReload = append(staleReload, other.FPMService)
							}
						}
					}
				}
			}
		}
		for _, unit := range staleReload {
			if _, err := s.agent.Service(ctx, unit, "reload-or-restart"); err != nil {
				return s.siteFail(ctx, site, err)
			}
			jc.Logf("%s released the pool socket", unit)
		}
	}
	jc.Progress(75, "applying")
	apply, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: validate, Reload: reload, Force: true, Origin: "site:" + site.Domain})
	if err != nil {
		return s.siteFail(ctx, site, err)
	}
	jc.Logf("configuration: %d written, %d unchanged; reloaded %s", len(apply.Written), len(apply.Unchanged), strings.Join(apply.Reloaded, ", "))
	if !suspended && !proxy && s.poolSocketWait > 0 {
		// systemd's reload returns once the signal is sent; give php-fpm a moment to open the pool socket.
		deadline := time.Now().Add(s.poolSocketWait)
		for {
			if _, err := os.Stat(l.socket); err == nil {
				break
			}
			if time.Now().After(deadline) {
				jc.Logf("warning: pool socket %s did not appear within %s", l.socket, s.poolSocketWait)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// nginx reload is graceful: old workers keep answering (with the old server
	// set) until they drain. Wait until a request for this host gets an HTTP
	// answer instead of the default server's connection close.
	if !suspended && site.IP != "" && s.nginxWait > 0 {
		if err := waitNginxHost(ctx, site.IP, site.Domain, s.nginxWait); err != nil {
			jc.Logf("warning: %v", err)
		}
	}

	jc.Progress(90, "saving")
	if suspended {
		site.Status = store.SiteSuspended
	} else {
		site.Status = store.SiteActive
	}
	site.LastError = ""
	if err := s.db.UpdateSite(context.WithoutCancel(ctx), site); err != nil {
		return err
	}
	if site.SSL == "auto" && !tls && !suspended {
		s.orderSiteCertificate(ctx, jc, site)
	}
	if suspended {
		jc.Progress(100, fmt.Sprintf("%s is suspended (503 stub, pool stopped)", site.Domain))
		return nil
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	if proxy {
		jc.Progress(100, fmt.Sprintf("%s://%s/ is live (%s %s)", scheme, site.Domain, modeLabel(site.Mode), site.Backend))
		return nil
	}
	jc.Progress(100, fmt.Sprintf("%s://%s/ is live (PHP %s, %s)", scheme, site.Domain, site.PHPVersion, modeLabel(site.Mode)))
	return nil
}

func osprofileVersions() []string {
	return []string{"5.6", "7.0", "7.1", "7.2", "7.3", "7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"}
}

func modeLabel(mode string) string {
	switch mode {
	case store.ModeApache:
		return "nginx + Apache"
	case store.ModeProxy:
		return "nginx → backend"
	}
	return "nginx + php-fpm"
}

// poolValues merges panel defaults with the site's php_ini into ordered php_value lines.
func (s *Server) poolValues(ctx context.Context, site *store.Site, tls bool) []render.KV {
	tz, _ := s.db.GetSetting(ctx, settingTZ)
	if tz == "" {
		tz = "UTC"
	}
	defaults := []render.KV{
		{Key: "memory_limit", Value: "256M"}, {Key: "upload_max_filesize", Value: "64M"}, {Key: "post_max_size", Value: "64M"},
		{Key: "max_execution_time", Value: "120"}, {Key: "date.timezone", Value: tz}, {Key: "display_errors", Value: "Off"},
	}
	preset := map[string]string{}
	for k, v := range presetIni[site.Preset] {
		preset[k] = v
	}
	// A secure-only session cookie needs HTTPS to exist: on a site that is
	// still on HTTP the login would not stick.
	if site.Preset == presetBitrix && tls {
		preset["session.cookie_secure"] = "On"
	}
	seen := map[string]bool{}
	out := make([]render.KV, 0, len(defaults)+len(preset)+len(site.PHPIni))
	for _, kv := range defaults {
		if v, ok := preset[kv.Key]; ok {
			kv.Value = v
		}
		if v, ok := site.PHPIni[kv.Key]; ok {
			kv.Value = v
		}
		seen[kv.Key] = true
		out = append(out, kv)
	}
	keys := make([]string, 0, len(site.PHPIni)+len(preset))
	for k := range preset {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range site.PHPIni {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok := site.PHPIni[k]
		if !ok {
			v = preset[k]
		}
		out = append(out, render.KV{Key: k, Value: v})
	}
	return out
}

// orderSiteCertificate creates or refreshes the certificate record for a site
// and enqueues the issue job for the names that publicly resolve to this host.
func (s *Server) orderSiteCertificate(ctx context.Context, jc *jobs.Context, site *store.Site) {
	existing, err := s.db.GetCertificateByName(ctx, site.Domain)
	if err == nil && existing.Status == store.CertPending {
		jc.Logf("certificate order for %s is already in progress", site.Domain)
		return
	}
	if err == nil && existing.CertPath == "" && renewalBackoff(existing) {
		jc.Logf("certificate: last attempt failed (%s); retry after %s", existing.LastError, existing.LastAttempt.Add(renewRetryBackoff).Format("15:04"))
		return
	}
	local := map[string]bool{}
	for _, ip := range localIPv4s() {
		local[ip] = true
	}
	names := []string{}
	for _, n := range append([]string{site.Domain}, site.Aliases...) {
		addrs, err := publicLookup(ctx, n)
		ok := false
		for _, a := range addrs {
			if local[a] {
				ok = true
			}
		}
		if err != nil || !ok {
			jc.Logf("certificate: %s does not point here yet, skipped", n)
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 || names[0] != site.Domain {
		jc.Logf("certificate: %s must resolve to this server first; run `mp site apply %s` afterwards", site.Domain, site.Domain)
		return
	}
	email, _ := s.db.GetSetting(ctx, settingACMEEmail)
	c := existing
	if c == nil {
		c = &store.Certificate{Name: site.Domain, AutoRenew: true}
	}
	c.Names, c.Kind, c.DirectoryURL, c.Email, c.KeyType = names, store.CertKindACME, acme.LetsEncrypt, email, "ec256"
	c.Status, c.LastError = store.CertPending, ""
	uid := site.UserID
	c.UserID = &uid
	if err := s.db.UpsertCertificate(ctx, c); err != nil {
		jc.Logf("certificate: %v", err)
		return
	}
	if _, err := s.jobs.Enqueue(ctx, "cert.issue", certIssuePayload{CertID: c.ID}, jobs.WithLockKey("cert:"+c.Name), jobs.WithRequestedBy(jc.RequestedBy)); err != nil {
		jc.Logf("certificate: %v", err)
		return
	}
	jc.Logf("certificate ordered for %s; the site switches to HTTPS automatically", strings.Join(names, ", "))
}

// reapplySitesForCert re-applies every auto-SSL site covered by a freshly
// issued certificate.
func (s *Server) reapplySitesForCert(ctx context.Context, logf func(string, ...any), c *store.Certificate) {
	seen := map[int64]bool{}
	for _, n := range c.Names {
		sites, err := s.db.FindSitesByName(ctx, n)
		if err != nil {
			continue
		}
		for _, site := range sites {
			if seen[site.ID] || site.SSL != "auto" || site.Status == store.SiteDeleting {
				continue
			}
			seen[site.ID] = true
			id := c.ID
			site.CertificateID = &id
			if err := s.db.UpdateSite(ctx, site); err != nil {
				continue
			}
			if _, err := s.enqueueSiteApply(ctx, site, "scheduler"); err == nil {
				logf("site %s will be re-applied with HTTPS", site.Domain)
			}
		}
	}
}

// jobSiteDelete removes configuration (and optionally files) of a site.
func (s *Server) jobSiteDelete(ctx context.Context, jc *jobs.Context) error {
	var p sitePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	site, err := s.db.GetSite(ctx, p.SiteID)
	if err != nil {
		return err
	}
	user, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	jc.Progress(20, "removing configuration")
	if err := s.removeSiteConfig(ctx, jc, site, user, p.Purge); err != nil {
		return s.siteFail(ctx, site, err)
	}
	if err := s.db.DeleteSite(context.WithoutCancel(ctx), site.ID); err != nil {
		return err
	}
	jc.Progress(100, "site removed")
	return nil
}

// removeSiteConfig deletes the nginx/Apache/php-fpm configuration of a site
// (validating and reloading) and, with purge, the site directory.
func (s *Server) removeSiteConfig(ctx context.Context, jc *jobs.Context, site *store.Site, user *store.User, purge bool) error {
	l := s.layoutFor(site, user)
	web := s.profile.Web()
	paths := []string{l.nginxConf, l.nginxDir, l.apacheConf, path.Join(web.ApacheConfDir, "monopanel", "sites", site.Domain+".d")}
	reload := []string{web.NginxService}
	for _, v := range osprofileVersions() {
		if other := osprofile.PHP(s.profile, v); other != nil {
			paths = append(paths, path.Join(other.PoolDir, site.Domain+".conf"))
			if pv, err := s.db.GetPHPVersion(ctx, v); err == nil && pv.Status == store.PHPInstalled {
				reload = append(reload, other.FPMService)
			}
		}
	}
	if v, _ := s.db.GetSetting(ctx, settingApache); v == "installed" {
		reload = append(reload, web.ApacheService)
	}
	rr, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: paths, Recursive: true, Validate: [][]string{web.NginxCheckArgv}, Reload: reload})
	if err != nil {
		return err
	}
	jc.Logf("%s: removed %s", site.Domain, strings.Join(rr.Removed, ", "))
	if purge {
		rr, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{l.siteRoot}, Recursive: true})
		if err != nil {
			return err
		}
		jc.Logf("%s: purged %s", site.Domain, strings.Join(rr.Removed, ", "))
	} else {
		jc.Logf("%s: files kept in %s", site.Domain, l.siteRoot)
	}
	return nil
}

// ensurePackages installs missing distro packages (idempotent, cheap when present).
func (s *Server) ensurePackages(ctx context.Context, jc *jobs.Context, pkgs ...string) error {
	q, err := s.agent.Pkg(ctx, "query", pkgs...)
	if err != nil {
		return err
	}
	missing := []string{}
	for _, p := range pkgs {
		if _, ok := q.Installed[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if jc != nil {
		jc.Logf("installing packages: %s", strings.Join(missing, " "))
	} else {
		s.log.Info("installing packages", "packages", missing)
	}
	_, err = s.agent.Pkg(ctx, "install", missing...)
	return err
}

// waitNginxHost polls http://ip/ with the Host header until nginx answers
// with any HTTP status (the site's server block is live).
func waitNginxHost(ctx context.Context, ip, host string, timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ip+"/", nil)
		if err != nil {
			return err
		}
		req.Host = host
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nginx did not start answering for %s within %s: %v", host, timeout, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
