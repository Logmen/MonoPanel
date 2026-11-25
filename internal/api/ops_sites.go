package api

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	// phpredis sessions (sites with sessions in Valkey): locking and its timing,
	// compression, TTL refresh on read.
	"redis.session.locking_enabled": true, "redis.session.lock_expire": true, "redis.session.lock_wait_time": true,
	"redis.session.lock_retries": true, "redis.session.lock_failure_readonly": true, "redis.session.early_refresh": true,
	"redis.session.compression": true, "redis.session.compression_level": true,
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
	// Domain is the site's name when a site.delete was queued: a row by
	// another name under SiteID is not the site asked for (siteGone).
	Domain string `json:"domain,omitempty"`
	Purge  bool   `json:"purge,omitempty"`
	// LogsMoved marks the site.apply that switches an upgraded site to the
	// root-owned log tree (moveSiteLogs); its log says so.
	LogsMoved bool `json:"logs_moved,omitempty"`
	// MailAfter marks the apply of the webmail's site the webmail job
	// queues: once the site is applied — its pool's socket is there —
	// mail.apply writes the webmail's server block on its port, which
	// passes PHP to that socket.
	MailAfter bool `json:"mail_after,omitempty"`
}

// siteGone tells whether the site a job was queued for is gone: no row by
// its id, or one by another name — SQLite gives the newest row's id to the
// next row once that row is gone, and a site made meanwhile, another
// account's maybe, may have it. A job queued without the name (an older
// panel's) goes by the id alone.
func siteGone(site *store.Site, err error, domain string) bool {
	return errors.Is(err, store.ErrNotFound) || (err == nil && domain != "" && site.Domain != domain)
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

// siteStays refuses a change of a site whose removal is under way — its own
// (site.delete) or its account's (user.delete, which removes the account's
// sites itself): the removal is queued, what the change wrote or queued
// would be taken away or skipped, and the answer would say it went in. A
// removal of either asked for between the request's read and its write is
// caught by the write (writeSiteChange).
func (s *Server) siteStays(ctx context.Context, site *store.Site) error {
	owner, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	return siteOwnerStays(site, owner)
}

// siteOwnerStays is siteStays for a caller that read the owner already.
func siteOwnerStays(site *store.Site, owner *store.User) error {
	if site.Status == store.SiteDeleting {
		return huma.Error409Conflict(fmt.Sprintf("site %s is being removed", site.Domain))
	}
	if owner.Status == store.UserDeleting {
		return huma.Error409Conflict(fmt.Sprintf("the account %s is being removed, and its site %s with it", owner.Login, site.Domain))
	}
	return nil
}

// writeSiteChange writes what a request changed of a site, the fields that
// differ from was (store.UpdateSiteChangesIfOwnerStays). A removal of the
// site or of its account asked for since the request read them — after
// siteStays let it through — is caught here, and nothing went in: the
// answer says whose removal it is (409), or that the site is gone (404),
// and the caller queues nothing.
func (s *Server) writeSiteChange(ctx context.Context, was, site *store.Site) error {
	if s.beforeChangeWrite != nil {
		s.beforeChangeWrite()
	}
	err := s.db.UpdateSiteChangesIfOwnerStays(ctx, was, site)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return huma.Error404NotFound("site not found")
	case errors.Is(err, store.ErrBeingRemoved):
		if cur, err := s.db.GetSite(ctx, site.ID); err == nil {
			if owner, err := s.db.GetUserByID(ctx, cur.UserID); err == nil {
				if refused := siteOwnerStays(cur, owner); refused != nil {
					return refused
				}
			}
		}
		return huma.Error409Conflict(fmt.Sprintf("site %s is being removed", site.Domain))
	}
	return err
}

// siteAsStored is the site as its row is after a change, for the answer:
// what went in together with what changed beside it — the address
// sites.move-ip gave the site, a suspension that kept the way back from a
// failure out — not the copy the request changed. The copy when the row
// cannot be read.
func (s *Server) siteAsStored(ctx context.Context, site *store.Site) *store.Site {
	if cur, err := s.db.GetSite(ctx, site.ID); err == nil {
		return cur
	}
	return site
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

// validateNames checks a site's names (selfID: the site itself when it
// changes; ownerID: the account the site belongs to, 0 for one not created
// yet) and returns the aliases in canonical form.
func (s *Server) validateNames(ctx context.Context, domain string, aliases []string, selfID, ownerID int64) ([]string, error) {
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
	// The panel's own names (its hostname, its mail server's) are not an
	// ordinary account's to serve: cookies do not keep to a port, so a site
	// on the panel's name would receive the administrators' panel session
	// with every visit, and the certificate store is keyed by name.
	if p := principalFrom(ctx); p == nil || p.Role != store.RoleAdmin {
		own := s.panelNames(ctx)
		for name := range seen {
			if own[name] {
				return nil, fmt.Errorf("%s is a hostname of this panel itself (the panel or its mail server)", name)
			}
			if other, err := s.foreignDomain(ctx, ownerID, name); err != nil {
				return nil, err
			} else if other != "" && other != name {
				return nil, fmt.Errorf("%s is under %s, which belongs to another account", name, other)
			} else if other == name {
				// The name itself, when another account's site has it, is
				// reported below as taken; that check knows sites only, and
				// another account's mail domain is its owner's: a site of
				// the name would hand the name's web to this account, and
				// the mail policy too.
				if md, err := s.db.GetMailDomain(ctx, name); err == nil && md.UserID != ownerID {
					return nil, fmt.Errorf("%s is a mail domain of another account", name)
				} else if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
			}
		}
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
		names, err := s.validateNames(ctx, domain, aliases, 0, owner.ID)
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
		// IPv6 comes along by default when the host has a global address;
		// "none" keeps the site on IPv4 only.
		ipv6 := b.IPv6
		switch ipv6 {
		case "":
			if v6 := s.hostIPv6s(); len(v6) > 0 {
				ipv6 = v6[0]
			}
		case "none":
			ipv6 = ""
		default:
			if err := s.checkHostIPv6(ipv6); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		if b.IP != "" {
			if err := s.checkHostIP(b.IP); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		site := &store.Site{UserID: owner.ID, Domain: domain, Aliases: names, Mode: b.Mode, Backend: b.Backend, PHPVersion: phpVersion, Docroot: strings.Trim(b.Docroot, "/"), IP: b.IP, IPv6: ipv6, SSL: b.SSL,
			HTTP2: true, RedirectHTTPS: true, RedirectWWW: b.RedirectWWW, StaticByNginx: true, FPMPM: b.FPMPM, FPMMaxChildren: b.FPMMaxChildren, PHPIni: b.PHPIni, ClientMaxBody: b.ClientMaxBody}
		if b.SecurityHeaders != nil {
			site.SecurityHeaders = *b.SecurityHeaders
		}
		if b.RateLimit != nil {
			site.RateLimit = *b.RateLimit
		}
		if site.AllowFrom, err = normalizeAllowFrom(b.AllowFrom); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if site.Preset, err = normalizePreset(b.Preset); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if site.Preset == presetBitrix && b.FPMMaxChildren == 0 {
			site.FPMMaxChildren = s.bitrixPoolSize(ctx)
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
			if ips := s.hostIPs(); len(ips) > 0 {
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
		if err := s.siteStays(ctx, site); err != nil {
			return nil, err
		}
		// Only what the request changes goes in (UpdateSiteChanges):
		// sites.move-ip or another request may change the site between this
		// read and the write, and the whole row written back would undo it.
		was := site.Clone()
		b := in.Body
		if b.Aliases != nil {
			names, err := s.validateNames(ctx, site.Domain, *b.Aliases, site.ID, site.UserID)
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
			if err := s.checkHostIP(b.IP); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			site.IP = b.IP
		}
		if b.IPv6 != nil {
			switch v := strings.TrimSpace(*b.IPv6); v {
			case "", "none":
				site.IPv6 = ""
			default:
				if err := s.checkHostIPv6(v); err != nil {
					return nil, huma.Error422UnprocessableEntity(err.Error())
				}
				site.IPv6 = v
			}
		}
		if b.SecurityHeaders != nil {
			site.SecurityHeaders = *b.SecurityHeaders
		}
		if b.RateLimit != nil {
			site.RateLimit = *b.RateLimit
		}
		if b.SSL != "" {
			site.SSL = b.SSL
		}
		if b.Certificate != "" {
			if p.Role != store.RoleAdmin {
				return nil, huma.Error403Forbidden("only an administrator gives a site a certificate by hand")
			}
			if err := s.pinSiteCert(ctx, site, b.Certificate); err != nil {
				return nil, err
			}
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
			was := site.Preset
			if site.Preset, err = normalizePreset(*b.Preset); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			// A site turning into 1C-Bitrix with the stock pool size gets
			// the preset's; a size someone set stays.
			if site.Preset == presetBitrix && was != presetBitrix && b.FPMMaxChildren == 0 && site.FPMMaxChildren == 8 {
				site.FPMMaxChildren = s.bitrixPoolSize(ctx)
			}
		}
		switch b.SessionStore {
		case "files":
			site.SessionStore = ""
		case store.SessionStoreValkey:
			site.SessionStore = store.SessionStoreValkey
		}
		// A site turning into a proxy has no PHP pool and so no sessions to keep.
		if site.Mode == store.ModeProxy && b.SessionStore != store.SessionStoreValkey {
			site.SessionStore = ""
		}
		// Checked after every other change: a new PHP branch needs its own redis extension.
		if err := s.checkSessionStore(ctx, site); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if site.Status == store.SiteError {
			site.Status = store.SitePending
		}
		if err := s.writeSiteChange(ctx, was, site); err != nil {
			return nil, err
		}
		jobID, err := s.enqueueSiteApply(ctx, site, p.Login)
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site.update", Target: site.Domain, IP: requestInfo(ctx).IP})
		return &siteJobOutput{Status: http.StatusAccepted, Body: apitypes.SiteWithJob{Site: s.siteAsStored(ctx, site), JobID: jobID}}, nil
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
			if err := s.siteStays(ctx, site); err != nil {
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
			// The status alone (UpdateSiteChanges says why).
			was := site.Clone()
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
			if err := s.writeSiteChange(ctx, was, site); err != nil {
				return nil, err
			}
			jobID, err := s.enqueueSiteApply(ctx, site, p.Login)
			if err != nil {
				return nil, err
			}
			s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "site." + action, Target: site.Domain, IP: requestInfo(ctx).IP})
			return &siteJobOutput{Status: http.StatusAccepted, Body: apitypes.SiteWithJob{Site: s.siteAsStored(ctx, site), JobID: jobID}}, nil
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
		// Not beside an in-place restore into the site's tree (restoreRefused
		// says why): refused while one is queued or runs, marked otherwise,
		// and a restore asked for or starting from then on is refused
		// (markRemoval).
		owner, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			return nil, err
		}
		root := s.layoutFor(site, owner).siteRoot
		// The status alone: a change made since the read stays
		// (MarkSiteDeleting).
		if err := s.markRemoval(ctx, "the site", root, func() error {
			return s.db.MarkSiteDeleting(ctx, site.ID)
		}); err != nil {
			return nil, err
		}
		job, err := s.jobs.Enqueue(ctx, "site.delete", sitePayload{SiteID: site.ID, Domain: site.Domain, Purge: in.Purge}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(p.Login))
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
	apacheConf string // the shared Apache's virtual host of 0.8.19; the account's own Apache has apacheInstance.vhost
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

// siteFail marks the site failed with err — while its status is still the
// one the job read (site.Status): a suspension or a removal asked for while
// the job ran is not overwritten (store.SaveAppliedSite).
func (s *Server) siteFail(ctx context.Context, site *store.Site, err error) error {
	// A shutdown is not the site's fault: the job runs again after the restart.
	if errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	_ = s.db.SetSiteStatusFrom(context.WithoutCancel(ctx), site.ID, site.Status, store.SiteError, err.Error())
	return err
}

// certForSite returns a valid certificate covering the site, if any: the
// one on the site's record, else the best of the others (certRank) — a
// wildcard like *.example.com serves www.example.com too.
func (s *Server) certForSite(ctx context.Context, site *store.Site) *store.Certificate {
	if site.CertificateID != nil {
		if c, err := s.db.GetCertificate(ctx, *site.CertificateID); err == nil && certUsable(c) && acme.CertCovers(c.Names, site.Domain) {
			return c
		}
	}
	certs, err := s.db.ListCertificates(ctx)
	if err != nil {
		return nil
	}
	var best *store.Certificate
	bestRank := 0
	for _, c := range certs {
		r := certRank(c, site.Domain)
		if r == 0 || !certUsable(c) || (best != nil && (r > bestRank || (r == bestRank && !c.NotAfter.After(*best.NotAfter)))) {
			continue
		}
		if r > 1 && !s.certOwnerFits(ctx, c, site) {
			continue
		}
		best, bestRank = c, r
	}
	return best
}

// pinSiteCert gives a site the certificate ref names (an id or a name) for
// good — reapplySitesForCert leaves it there — or, with "auto", lets the
// panel choose again (certForSite). A certificate given by hand turns HTTPS on.
func (s *Server) pinSiteCert(ctx context.Context, site *store.Site, ref string) error {
	if ref == "auto" {
		site.CertificateID, site.CertPinned = nil, false
		return nil
	}
	c, err := s.certByRef(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound("certificate " + ref + " not found")
	}
	if err != nil {
		return err
	}
	if err := s.certFitsSite(ctx, c, site); err != nil {
		return err
	}
	id := c.ID
	site.CertificateID, site.CertPinned, site.SSL = &id, true, "auto"
	return nil
}

// certFitsSite says why a certificate cannot serve a site by hand, or nil.
func (s *Server) certFitsSite(ctx context.Context, c *store.Certificate, site *store.Site) error {
	switch {
	case !certCovers(c.Names, site.Domain):
		return huma.Error422UnprocessableEntity(fmt.Sprintf("certificate %s does not cover %s (names: %s)", c.Name, site.Domain, strings.Join(c.Names, ", ")))
	case !certUsable(c):
		return huma.Error422UnprocessableEntity(fmt.Sprintf("certificate %s is not valid now (%s); issue or import it first", c.Name, c.Status))
	case !s.certOwnerFits(ctx, c, site):
		return huma.Error422UnprocessableEntity(fmt.Sprintf("certificate %s belongs to another account and does not serve %s", c.Name, site.Domain))
	}
	return nil
}

// certByRef finds a certificate by its id or its name.
func (s *Server) certByRef(ctx context.Context, ref string) (*store.Certificate, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return s.db.GetCertificate(ctx, id)
	}
	return s.db.GetCertificateByName(ctx, acme.NormalizeName(ref))
}

// certUsable says whether a certificate can be served now. Files on disk
// decide: a failed renewal must not switch a site to HTTP while the previous
// certificate is still valid.
func certUsable(c *store.Certificate) bool {
	return c.CertPath != "" && c.KeyPath != "" && c.NotAfter != nil && time.Now().Before(*c.NotAfter)
}

// certRank says how a certificate names a domain, best first: 1 the record
// is the domain's own, 2 the domain is among its names, 3 a wildcard covers
// it; 0 not at all.
func certRank(c *store.Certificate, domain string) int {
	switch {
	case c.Name == domain && acme.CertCovers(c.Names, domain):
		return 1
	case containsName(c.Names, domain):
		return 2
	case acme.CertCovers(c.Names, domain):
		return 3
	}
	return 0
}

// certOwnerFits says whether a certificate may serve a site it was not made
// for: one of the panel's own or an administrator's serves any site, one an
// account brought along (a migration) only that account's sites — its key is
// in that account's hands.
func (s *Server) certOwnerFits(ctx context.Context, c *store.Certificate, site *store.Site) bool {
	return (c.UserID != nil && *c.UserID == site.UserID) || s.adminCert(ctx, c)
}

// adminCert says whether a certificate is the panel's or an administrator's.
func (s *Server) adminCert(ctx context.Context, c *store.Certificate) bool {
	if c.UserID == nil {
		return true
	}
	u, err := s.db.GetUserByID(ctx, *c.UserID)
	return err == nil && u.Role == store.RoleAdmin
}

// systemCert is the certificate a service of the panel itself (its HTTPS
// port, phpMyAdmin, the mail server) serves for host: the record of host's
// own name, else the best valid one that covers it (certRank) — the panel's
// or an administrator's: an account's key never serves the panel's
// services. Its files must be there.
func (s *Server) systemCert(ctx context.Context, host string) *store.Certificate {
	host = acme.NormalizeName(host)
	if host == "" || net.ParseIP(host) != nil {
		return nil
	}
	certs, err := s.db.ListCertificates(ctx)
	if err != nil {
		return nil
	}
	var best *store.Certificate
	bestRank := 0
	for _, c := range certs {
		r := certRank(c, host)
		if r == 0 || !certUsable(c) || (best != nil && (r > bestRank || (r == bestRank && !c.NotAfter.After(*best.NotAfter)))) {
			continue
		}
		if r > 1 && !s.adminCert(ctx, c) {
			continue
		}
		if _, err := os.Stat(c.CertPath); err != nil {
			continue
		}
		best, bestRank = c, r
	}
	return best
}

// panelCoverFiles are the files of the certificate that covers the panel's
// hostname (certHolder.find).
func (s *Server) panelCoverFiles() (certPath, keyPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c := s.systemCert(ctx, s.panelCertName()); c != nil {
		return c.CertPath, c.KeyPath
	}
	return "", ""
}

// jobSiteApply renders and applies the pool, nginx (and Apache) configuration.
// siteTree makes the client's directories with their modes and gives the
// web group its ACLs: on creation, and again by site.fix after someone
// reshaped the tree by hand. The site logs are not in the home any more
// (ensureSiteLogs); an old data/logs stays as the account left it.
func (s *Server) siteTree(ctx context.Context, jc *jobs.Context, l *siteLayout, user *store.User) error {
	login := user.Login
	dirs := []agent.DirSpec{
		homeDirSpec(l.home, login, user.Shell),
		{Path: l.data, Mode: 0o750, Owner: login, Group: login},
		{Path: path.Join(l.data, "www"), Mode: 0o750, Owner: login, Group: login},
		{Path: l.siteRoot, Mode: 0o750, Owner: login, Group: login},
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
		entries := []string{webACL + ":x"}
		if d == l.home && !user.Shell {
			entries = append(entries, sftpHomeGroupACL)
		}
		if err := s.agent.SetACL(ctx, &agent.SetACLRequest{Path: d, Entries: entries}); err != nil {
			return err
		}
	}
	res, err := s.agent.SiteACL(ctx, &agent.SiteACLRequest{Root: l.siteRoot, Group: s.cfg.WebGroup})
	if err != nil {
		return err
	}
	if jc != nil {
		jc.Logf("ACL for %s: %d directories, %d files", s.cfg.WebGroup, res.Dirs, res.Files)
	}
	return nil
}

// jobSiteFix puts a site's files back in order after someone worked on them
// as root: directories and ACLs as on creation, the client as the owner of
// the whole site tree, the site's logs with their owners and modes, SELinux
// labels the policy expects (files copied with cp -a from /root keep
// admin_home_t and nginx answers 403 for them).
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
	if err := s.siteTree(ctx, jc, l, user); err != nil {
		return err
	}
	jc.Progress(40, "owner")
	ch, err := s.agent.Chown(ctx, &agent.ChownRequest{Path: l.siteRoot, Owner: user.Login, Group: user.Login, Recursive: true})
	if err != nil {
		return err
	}
	jc.Logf("owner %s under %s: %d objects changed", user.Login, l.siteRoot, ch.Changed)
	logs, err := s.ensureSiteLogs(ctx, user.Login, s.siteLogsOf(ctx, site)...)
	if err != nil {
		return fmt.Errorf("site logs: %w", err)
	}
	jc.Logf("logs in %s: %d made, %d set back", s.siteLogDir(user.Login), len(logs.Created), logs.Changed)
	jc.Progress(70, "SELinux labels")
	// the rules first: a host that applied an older policy gets the current one
	if err := s.selinuxHostingPolicy(ctx, jc); err != nil {
		jc.Logf("warning: SELinux policy: %v", err)
	}
	s.relabel(ctx, jc, l.data, true)
	s.relabel(ctx, jc, s.siteLogDir(user.Login), true)
	// The tree-wide ACL pass above knows nothing about guests' entries.
	s.reapplyShareACLs(ctx, jc, site.ID)
	jc.Progress(100, "fixed")
	return nil
}

func (s *Server) jobSiteApply(ctx context.Context, jc *jobs.Context) error {
	var p sitePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	err := s.applySite(ctx, jc, p)
	if p.MailAfter && !errors.Is(err, errSiteGone) {
		// Also after a failed apply: the block follows the mail settings,
		// and the next apply of the site brings the socket.
		if j, qerr := s.jobs.Enqueue(context.WithoutCancel(ctx), "mail.apply", struct{}{}, jobs.WithLockKey("mail"), jobs.WithRequestedBy(jc.RequestedBy)); qerr != nil {
			jc.Logf("warning: the webmail's port is not set up: %v; mp mail apply does it", qerr)
		} else {
			jc.Logf("the webmail's port is set up by job #%d", j.ID)
		}
	}
	if errors.Is(err, errSiteGone) {
		// Queued before the site went — by the webmail, the shared folders,
		// sites.move-ip or a change of the site — and run after its
		// site.delete or user.delete: that took the configuration away, and
		// nothing is left to fix. A failure here would count in the doctor's
		// jobs line for a site that does not exist.
		jc.Progress(100, err.Error())
		return nil
	}
	return err
}

// errSiteGone is what applySiteLocked answers for a site removed before it
// got the site's lock; each caller decides what that means for it.
var errSiteGone = errors.New("the site is gone: nothing to apply")

// applySite is site.apply's work, under the site's lock (lockSite): the move
// of the sites in Apache mode to their accounts' Apaches does it inline, site
// by site (apache.go), beside the sites' own jobs.
func (s *Server) applySite(ctx context.Context, jc *jobs.Context, p sitePayload) error {
	defer s.lockSite(p.SiteID)()
	// The log move's own apply of a site that another apply moved while it
	// waited — the move of the sites to their accounts' Apaches, which on a
	// busy host gets to many of them first, or an administrator's — has
	// nothing left to do: that apply rendered this version's configuration,
	// and a second one would only reload the site's servers again.
	if p.LogsMoved && !s.siteUnmoved(ctx, p.SiteID) {
		jc.Progress(100, "moved already: the servers write the logs into "+s.siteLogsRoot())
		return nil
	}
	return s.applySiteLocked(ctx, jc, p)
}

// applySiteLocked is applySite under the site's lock: the site is read
// under it, so that one apply after another renders what the one before
// left, not what both read at once.
func (s *Server) applySiteLocked(ctx context.Context, jc *jobs.Context, p sitePayload) error {
	site, err := s.db.GetSite(ctx, p.SiteID)
	if errors.Is(err, store.ErrNotFound) {
		return errSiteGone
	}
	if err != nil {
		return err
	}
	// The status and the certificate as read: the apply saves its own only
	// while they are still these (store.SaveAppliedSite).
	from := site.Status
	var fromCert *int64
	if site.CertificateID != nil {
		id := *site.CertificateID
		fromCert = &id
	}
	user, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	// A site being removed, or one of an account being removed, is not
	// rendered again: the removal takes its configuration away under the
	// site's lock (removeSiteConfig), and an apply after that — the move of
	// the sites to their accounts' Apaches applies them inside a job of its
	// own, beside the removal's — would leave a server block, a pool and a
	// virtual host behind for a site the panel no longer knows.
	if site.Status == store.SiteDeleting || user.Status == store.UserDeleting {
		jc.Logf("%s is being removed: nothing to apply", site.Domain)
		return nil
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
		if ips := s.hostIPs(); len(ips) > 0 {
			site.IP = ips[0]
		}
	}
	login := user.Login
	suspended := site.Status == store.SiteSuspended
	// A site in Apache mode runs on its account's own Apache, behind nginx.
	apacheMode := site.Mode == store.ModeApache && !suspended
	var apache *apacheInstance
	if apacheMode {
		if apache, err = s.apacheInstanceFor(user); err != nil {
			return s.siteFail(ctx, site, err)
		}
	}
	// The servers of a live site open its logs in the tree, which SELinux
	// lets them only once the tree's label rule is in place; the suspended
	// stub logs nothing, and its pool goes.
	if !suspended {
		if err := s.siteLogsRuleReady(ctx, jc.Logf); err != nil {
			return s.siteFail(ctx, site, err)
		}
	}

	jc.Progress(10, "directories and permissions")
	if err := s.siteTree(ctx, jc, l, user); err != nil {
		return s.siteFail(ctx, site, err)
	}
	if !proxy {
		welcome, err := s.placeholderPage(site.Domain)
		if err != nil {
			return err
		}
		if res, err := s.agent.EnsureFile(ctx, &agent.EnsureFileRequest{Path: path.Join(l.docroot, placeholderName), Content: welcome, Mode: 0o644, Owner: login, OnlyIfDirEmpty: true}); err != nil {
			return s.siteFail(ctx, site, err)
		} else if res.Written {
			jc.Logf("placeholder page: %s/%s", l.docroot, placeholderName)
		}
		if err := s.agent.EnsureSymlink(ctx, &agent.EnsureSymlinkRequest{Path: path.Join(l.data, "bin", "php"), Target: l.php.CLIBinary, Owner: login, OnlyIfMissing: true}); err != nil {
			jc.Logf("warning: php CLI symlink: %v", err)
		}
		if err := s.refreshPHPCLIIni(ctx, l.php); err != nil {
			jc.Logf("warning: PHP CLI settings: %v", err)
		}
	}

	jc.Progress(35, "certificate")
	cert := (*store.Certificate)(nil)
	if site.SSL == "auto" {
		cert = s.certForSite(ctx, site)
	}
	tls := cert != nil
	if tls && site.CertPinned && site.CertificateID != nil && *site.CertificateID != cert.ID {
		// The one given by hand is not valid now: another serves meanwhile,
		// and the site keeps the one it was given.
		jc.Logf("warning: the certificate given to the site is not valid now; %s serves meanwhile", cert.Name)
		jc.Logf("TLS: %s (%s, until %s)", cert.Name, cert.Issuer, cert.NotAfter.Format("2006-01-02"))
	} else if tls {
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
		Home: l.home, DataDir: l.data, TmpDir: path.Join(l.data, "tmp"), LogDir: s.siteLogDir(login), BinDir: path.Join(l.data, "bin"),
		SendmailFrom: "noreply@" + site.Domain, DisableFunctions: disable, Values: values,
		OpenBasedir: site.Preset != presetBitrix,
	}
	if site.SessionStore == store.SessionStoreValkey {
		pool.SessionSocket = valkeySocketFor(login, store.ValkeySessions)
	}
	poolConf := ""
	if !proxy {
		if poolConf, err = s.render.Render("php-fpm/pool.conf.tmpl", pool); err != nil {
			return err
		}
	}
	rs := render.Site{
		Domain: site.Domain, Aliases: site.Aliases, Mode: site.Mode, IP: site.IP, TLS: tls, HTTP2: site.HTTP2, HTTP3: site.HTTP3 && tls,
		RedirectHTTPS: site.RedirectHTTPS, RedirectWWW: site.RedirectWWW, Docroot: l.docroot, LogDir: s.siteLogDir(login),
		IncludeDir: "monopanel/sites/" + site.Domain + ".d", ClientMaxBodySize: strings.ToLower(site.ClientMaxBody), StaticByNginx: site.StaticByNginx,
		FPMSocket: l.socket, ProxyTimeout: terminate, Backend: site.Backend, AllowFrom: site.AllowFrom,
		HSTS: tls && site.RedirectHTTPS, Preset: site.Preset, IPv6: site.IPv6, SecurityHeaders: site.SecurityHeaders, RateLimit: site.RateLimit,
		NoPHPPaths: s.siteNoPHPPaths(ctx, site.ID),
	}
	if apache != nil {
		rs.ApacheBackend, rs.ApacheBackendConf = apache.addr, s.nginxBackendInclude(login)
	}
	for _, p := range rs.NoPHPPaths {
		jc.Logf("shared folder %s: PHP execution denied", p)
	}
	if site.RateLimit > 0 {
		jc.Logf("rate limit: %d requests/s per address on dynamic pages", site.RateLimit)
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
	if !apacheMode {
		// The site's virtual host of the shared Apache of 0.8.19: nginx sends
		// the site elsewhere (the account's own Apache keeps a site in Apache
		// mode, settleApacheInstance below takes it off there otherwise).
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
	// The logs exist, in the root-owned tree, before nginx, Apache or php-fpm
	// opens them: nginx -t fails on a missing directory, php-fpm on a slow log
	// it cannot make, and a tree the agent refuses is not one to apply over.
	logs, err := s.ensureSiteLogs(ctx, login, siteLogNames(site)...)
	if err != nil {
		return s.siteFail(ctx, site, fmt.Errorf("site logs: %w", err))
	}
	if len(logs.Created) > 0 || p.LogsMoved {
		jc.Logf("logs: %s", s.siteLogDir(login))
	}
	// A default server of an address the host lost fails the check below.
	if gone := s.pruneDefaultServers(ctx); len(gone) > 0 {
		jc.Logf("removed default servers of gone addresses: %s", strings.Join(gone, ", "))
	}
	jc.Progress(75, "applying")
	if apacheMode {
		// The account's Apache gets the site, and answers, before nginx is
		// sent there: until nginx's reload below the site is served where it
		// was. nginx sends the account's backend secret, which the Apache
		// demands, from the same checked set as the site's server block.
		if err := s.applyApacheSite(ctx, jc, apache, site, user, rs); err != nil {
			return s.siteFail(ctx, site, err)
		}
		files = append(files, nginxBackendFile(login, apache.secret))
	}
	apply, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Validate: validate, Reload: reload, Force: true, Origin: "site:" + site.Domain})
	if err != nil {
		return s.siteFail(ctx, site, err)
	}
	jc.Logf("configuration: %d written, %d unchanged; reloaded %s", len(apply.Written), len(apply.Unchanged), strings.Join(apply.Reloaded, ", "))
	if !apacheMode {
		// nginx no longer sends the site to its account's Apache (it left
		// Apache mode or is suspended): its virtual host goes there, and the
		// Apache reloads without it, or stops with no site left.
		if err := s.settleApacheInstance(ctx, jc.Logf, user, site, false); err != nil {
			jc.Logf("warning: %v", err)
		}
	} else if v, _ := s.db.GetSetting(ctx, settingApacheInstances); v != "yes" {
		// nginx sends the site to its account's Apache now: its virtual host
		// of the shared Apache of 0.8.19 goes too. That Apache keeps running
		// until every site has moved (apache.migrate), and the rotation has
		// it reopen its logs every day — as root, in the account's data/logs,
		// which the move ends. Nothing sends the site there any more, so it is
		// not reloaded for this; its next start does without the site.
		rr, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{l.apacheConf}})
		if err != nil {
			return s.siteFail(ctx, site, fmt.Errorf("the shared Apache's virtual host %s: %w", l.apacheConf, err))
		}
		if len(rr.Removed) > 0 {
			jc.Logf("removed %s: the shared Apache of 0.8.19 does not serve the site any more", l.apacheConf)
		}
	}
	// The configuration in place is this version's: logs in the tree and, in
	// Apache mode, the account's Apache. A site an older panel set up has
	// moved (siteMoves), and fail2ban's refresh below lists its logs now.
	if s.siteMoved(ctx, site.ID, s.siteLogsRoot()) {
		jc.Logf("the servers write the logs into %s from now on, not into %s: root opens them, and that directory is the account's; the old files stay there as the account's own, nothing writes or rotates them any more", s.siteLogDir(login), path.Join(l.data, "logs"))
	}
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
	// Only what the apply decided goes back: a change made while it ran (the
	// mode, the PHP branch, a suspension) stays for the site.apply that change
	// queued, and is not overwritten with what this one read at its start.
	if err := s.db.SaveAppliedSite(context.WithoutCancel(ctx), site, from, tls, fromCert); err != nil {
		return err
	}
	if site.SSL == "auto" && !tls && !suspended {
		s.orderSiteCertificate(ctx, jc, site, false)
	}
	// An alias added to an HTTPS site: the certificate it has still names
	// the domain, so it is served, but the new name needs a new one.
	if site.SSL == "auto" && tls && !suspended {
		if missing := uncoveredNames(cert, site); len(missing) > 0 {
			if cert.Kind == store.CertKindACME && cert.Name == site.Domain {
				jc.Logf("certificate %s does not cover %s", cert.Name, strings.Join(missing, ", "))
				s.orderSiteCertificate(ctx, jc, site, true)
			} else {
				// Not the site's own ACME certificate (a wildcard, an imported
				// one): nothing to re-order, but those names answer with a warning.
				jc.Logf("warning: certificate %s does not cover %s: browsers warn on those names", cert.Name, strings.Join(missing, ", "))
			}
		}
	}
	// fail2ban reads the sites' own logs: a new site joins its nginx jails,
	// and a log they listed before this apply made it is read once fail2ban
	// restarts (refreshFail2banAfter).
	s.refreshFail2banAfter(ctx, logs.Created)
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

// bitrixPoolSize is pm.max_children of a 1C-Bitrix site nobody sized by
// hand: its templates call the site over HTTP from inside a request, and the
// stock eight workers all waiting for each other stall the site. The site
// keeps the number (the store turns an unset size into 8, so it is chosen
// when the preset arrives: creation, a preset change, a migration).
func (s *Server) bitrixPoolSize(ctx context.Context) int {
	info, err := s.agent.SystemInfo(ctx)
	if err != nil {
		return 8
	}
	return bitrixMaxChildren(int(info.MemTotalBytes >> 20))
}

// bitrixMaxChildren gives a worker per 256 MB of RAM, 8 to 48: a Bitrix
// worker holds about 100 MB, the rest is for MySQL and the page cache.
func bitrixMaxChildren(ramMB int) int {
	return min(max(ramMB/256, 8), 48)
}

// poolValues merges panel defaults with the site's php_ini into ordered php_value lines.
func (s *Server) poolValues(ctx context.Context, site *store.Site, tls bool) []render.KV {
	vals := s.sitePHPValues(ctx, site, tls)
	out := make([]render.KV, 0, len(vals))
	for _, v := range vals {
		out = append(out, render.KV{Key: v.Key, Value: v.Value})
	}
	return out
}

// pointsHere says whether a name resolves in public DNS to this host.
func (s *Server) pointsHere(ctx context.Context, name string) bool {
	local := map[string]bool{}
	for _, ip := range s.hostIPs() {
		local[ip] = true
	}
	addrs, err := s.lookup(ctx, name)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if local[a] {
			return true
		}
	}
	return false
}

// uncoveredNames lists the site's names a certificate does not cover.
func uncoveredNames(c *store.Certificate, site *store.Site) []string {
	var out []string
	for _, n := range append([]string{site.Domain}, site.Aliases...) {
		if !acme.CertCovers(c.Names, n) {
			out = append(out, n)
		}
	}
	return out
}

// orderSiteCertificate creates or refreshes the certificate record for a site
// and enqueues the issue job for the names that publicly resolve to this host.
// With extend the site already has a certificate and a new one is ordered only
// when it would carry more names (an alias was added and points here).
func (s *Server) orderSiteCertificate(ctx context.Context, jc *jobs.Context, site *store.Site, extend bool) {
	existing, err := s.db.GetCertificateByName(ctx, site.Domain)
	if err == nil && existing.Status == store.CertPending {
		jc.Logf("certificate order for %s is already in progress", site.Domain)
		return
	}
	if err == nil && (existing.CertPath == "" || extend) && renewalBackoff(existing) {
		jc.Logf("certificate: last attempt failed (%s); retry after %s", existing.LastError, existing.LastAttempt.Add(renewRetryBackoff).Format("15:04"))
		return
	}
	names := []string{}
	for _, n := range append([]string{site.Domain}, site.Aliases...) {
		if !s.pointsHere(ctx, n) {
			jc.Logf("certificate: %s does not point here yet, skipped", n)
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 || names[0] != site.Domain {
		jc.Logf("certificate: %s must resolve to this server first; run `mp site apply %s` afterwards", site.Domain, site.Domain)
		return
	}
	if extend && existing != nil {
		more := false
		for _, n := range names {
			if !acme.CertCovers(existing.Names, n) {
				more = true
			}
		}
		if !more {
			return
		}
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
	if extend {
		jc.Logf("certificate re-ordered for %s (the current one is served until then)", strings.Join(names, ", "))
		return
	}
	jc.Logf("certificate ordered for %s; the site switches to HTTPS automatically", strings.Join(names, ", "))
}

// reapplySitesForCert re-applies every auto-SSL site covered by a freshly
// issued or imported certificate. A site it names (by its domain or an
// alias) takes it, as before; a site only its wildcard covers takes it when
// the site has no valid certificate of its own — one for its very name stays
// — and the certificate may serve it (certOwnerFits).
func (s *Server) reapplySitesForCert(ctx context.Context, logf func(string, ...any), c *store.Certificate) {
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return
	}
	for _, site := range sites {
		if site.SSL != "auto" || site.Status == store.SiteDeleting {
			continue
		}
		bound := site.CertificateID != nil && *site.CertificateID == c.ID
		if site.CertPinned && site.CertificateID != nil && !bound {
			continue // given another by hand (pinSiteCert)
		}
		takes := s.certTakes(ctx, c, site)
		if !takes && !bound {
			continue
		}
		// The certificate alone (UpdateSiteChanges says why); a site
		// whose removal was asked for meanwhile gets neither it nor an
		// apply.
		was := site.Clone()
		id := c.ID
		site.CertificateID = &id
		if err := s.db.UpdateSiteChanges(ctx, was, site); err != nil {
			continue
		}
		if _, err := s.enqueueSiteApply(ctx, site, "scheduler"); err != nil {
			continue
		}
		if takes {
			logf("site %s will be re-applied with HTTPS", site.Domain)
		} else {
			// The new files under the same path no longer cover it (the
			// names changed): its apply finds another certificate or HTTP.
			logf("site %s will be re-applied: certificate %s no longer covers it", site.Domain, c.Name)
		}
	}
}

// certTakes says whether a site moves to certificate c (reapplySitesForCert).
func (s *Server) certTakes(ctx context.Context, c *store.Certificate, site *store.Site) bool {
	for _, n := range append([]string{site.Domain}, site.Aliases...) {
		if containsName(c.Names, n) {
			return true
		}
	}
	if !acme.CertCovers(c.Names, site.Domain) {
		return false
	}
	if site.CertificateID != nil && *site.CertificateID == c.ID {
		return true
	}
	if !s.certOwnerFits(ctx, c, site) {
		return false
	}
	cur := s.certForSite(ctx, site)
	// None yet, this one already, or another wildcard that runs out sooner.
	return cur == nil || cur.ID == c.ID || (certRank(cur, site.Domain) == 3 && c.NotAfter != nil && c.NotAfter.After(*cur.NotAfter))
}

// jobSiteDelete removes configuration (and optionally files) of a site.
func (s *Server) jobSiteDelete(ctx context.Context, jc *jobs.Context) error {
	var p sitePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	// The site may be gone already: removed with its account by a
	// user.delete, which runs beside this job under a key of its own, or by
	// an earlier site.delete of it (one asked for twice, which runs after
	// the first under the same key) — done then, with the files as that
	// removal left them. A user.delete may also take the row between this
	// job's removal of the configuration, which lets the site's lock go, and
	// its DeleteSite below, which finds nothing left to delete then. A site
	// made since, under the id of the one gone, is not this one (siteGone).
	site, err := s.db.GetSite(ctx, p.SiteID)
	if siteGone(site, err, p.Domain) {
		jc.Progress(100, "the site is gone already: its files are as its removal left them")
		return nil
	}
	if err != nil {
		return err
	}
	user, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return err
	}
	jc.Progress(20, "removing configuration")
	if err := s.removeSiteConfig(ctx, jc, site, user, purgeTree(p.Purge)); err != nil {
		return s.siteFail(ctx, site, err)
	}
	if err := s.db.DeleteSite(context.WithoutCancel(ctx), site.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.refreshFail2ban(ctx)
	jc.Progress(100, "site removed")
	return nil
}

// siteTree is what the removal of a site does with the site's tree, its
// files (removeSiteConfig).
type siteTree int

const (
	// treeKept: the tree stays where it is.
	treeKept siteTree = iota
	// treePurged: the removal takes the tree along (site.delete --purge).
	treePurged
	// treeWithHome: the tree goes with the account's home, which the
	// account's purge removes after the sites (user.delete --purge).
	treeWithHome
)

// purgeTree is the site.delete's choice: the tree purged or kept.
func purgeTree(purge bool) siteTree {
	if purge {
		return treePurged
	}
	return treeKept
}

// removeSiteConfig deletes the nginx/Apache/php-fpm configuration of a site
// (validating and reloading) and, for treePurged, the site directory. It
// holds the site's lock (lockSite): an apply under way — the move of the
// sites to their accounts' Apaches applies them inside a job of its own,
// beside the removal's — writes its configuration before this removes it,
// and one after this renders nothing for a site being removed
// (applySiteLocked).
func (s *Server) removeSiteConfig(ctx context.Context, jc *jobs.Context, site *store.Site, user *store.User, tree siteTree) error {
	defer s.lockSite(site.ID)()
	if err := s.removeSiteShares(ctx, jc, site.ID); err != nil {
		return err
	}
	l := s.layoutFor(site, user)
	web := s.profile.Web()
	// The shared Apache's virtual host of 0.8.19 too, should the site still
	// have one: nothing reloads that Apache any more.
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
	rr, err := s.removeSitePaths(ctx, jc, site, &agent.RemovePathsRequest{Paths: paths, Recursive: true, Validate: [][]string{web.NginxCheckArgv}, Reload: reload})
	if err != nil {
		return err
	}
	// The agent names what it found to remove. Nothing is left when another
	// removal of the site came first — its own site.delete before a
	// user.delete that listed it — or when nothing was ever written: the
	// site a webmail job made and then gave up on.
	if len(rr.Removed) > 0 {
		jc.Logf("%s: removed %s", site.Domain, strings.Join(rr.Removed, ", "))
	} else {
		jc.Logf("%s: no configuration left to remove", site.Domain)
	}
	// nginx no longer sends the site anywhere: off its account's Apache with
	// its custom directives; the Apache reloads, or stops with no site left
	// (an account's removal comes here for every site before the account goes).
	if err := s.settleApacheInstance(ctx, jc.Logf, user, site, true); err != nil {
		jc.Logf("warning: %v", err)
	}
	switch {
	case tree == treePurged:
		rr, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{l.siteRoot}, Recursive: true})
		if err != nil {
			return err
		}
		if len(rr.Removed) > 0 {
			jc.Logf("%s: purged %s", site.Domain, strings.Join(rr.Removed, ", "))
		} else {
			jc.Logf("%s: nothing to purge, %s does not exist", site.Domain, l.siteRoot)
		}
	case s.pathGone(ctx, l.siteRoot):
		jc.Logf("%s: no files to keep, %s does not exist", site.Domain, l.siteRoot)
	case tree == treeWithHome:
		// Not "kept": the account's purge removes the home right after.
		jc.Logf("%s: files go with the home %s", site.Domain, l.home)
	default:
		jc.Logf("%s: files kept in %s", site.Domain, l.siteRoot)
	}
	return nil
}

// pathGone tells that the agent finds nothing at p. Not knowing — the agent
// did not answer — is not gone.
func (s *Server) pathGone(ctx context.Context, p string) bool {
	st, err := s.agent.Stat(ctx, p)
	return err == nil && len(st.Entries) == 1 && !st.Entries[0].Exists
}

// removeSitePaths takes a site's configuration away for removeSiteConfig,
// and the webmail with it when the site holds it. The mail settings name
// that site, and only the webmail job, under the same site's lock, makes a
// site the webmail: the server block that publishes it on its port goes in
// the same checked reload, and the settings stop naming the site
// (forgetWebmail). Left behind, the block would serve a docroot and a pool
// socket that are gone and name logs in a directory the removal of the
// account takes along — nginx's check then fails for every later change on
// the host — and every mail change would fail on a webmail site that is
// not there (applyWebmailPort). webmailMu keeps a mail change that read
// the settings before from writing the block back after.
func (s *Server) removeSitePaths(ctx context.Context, jc *jobs.Context, site *store.Site, req *agent.RemovePathsRequest) (*agent.RemovePathsResponse, error) {
	if s.loadMailConfig(ctx).Webmail != site.Domain {
		return s.agent.RemovePaths(ctx, req)
	}
	s.webmailMu.Lock()
	defer s.webmailMu.Unlock()
	// Asked again under the lock: the webmail may have moved to another site
	// meanwhile, whose block on the port is not this site's to take.
	if s.loadMailConfig(ctx).Webmail != site.Domain {
		return s.agent.RemovePaths(ctx, req)
	}
	req.Paths = append(req.Paths, s.webmailConfPath())
	rr, err := s.agent.RemovePaths(ctx, req)
	if err != nil {
		return nil, err
	}
	return rr, s.forgetWebmail(ctx, jc.Logf, site.Domain)
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

// foreignDomain names the site or mail domain of another account that name
// is, or lies under ("" when there is none). Ordinary accounts keep to their
// own names: mail for a domain the server handles is delivered here, whatever
// DNS says, so a mailbox under another account's domain would receive what
// this server's sites send there, and a site under it would share its
// cookies' parent domain.
func (s *Server) foreignDomain(ctx context.Context, ownerID int64, name string) (string, error) {
	for n := name; strings.Contains(n, "."); n = n[strings.Index(n, ".")+1:] {
		sites, err := s.db.FindSitesByName(ctx, n)
		if err != nil {
			return "", err
		}
		for _, o := range sites {
			if o.UserID != ownerID {
				return n, nil
			}
		}
		md, err := s.db.GetMailDomain(ctx, n)
		if err == nil && md.UserID != ownerID {
			return n, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	return "", nil
}
