package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Apache mode: every account with a site in Apache mode has an Apache of
// its own, monopanel-apache@<login>.service (Debian/Ubuntu). Its parent runs
// as root and binds the account's address, 127.<100+uid>>16>.<uid>>8>.<uid>:81
// — a port below 1024 on an address no other account's Apache has, so no
// account can take it, also not while the Apache restarts; its children
// serve the sites as the account. Up to 0.8.19 one shared Apache, the
// distribution's apache2.service on 127.0.0.1:8080, served every account's
// sites with children running as www-data, a member of the web group: a
// site's .htaccess could point PHP at another site's pool socket and set its
// php_admin_values, follow a symlink into another home or show the server's
// status page, and while the shared Apache restarted any local account could
// bind 127.0.0.1:8080 and receive every site's requests. Inside one shared
// Apache none of that can be fenced off — .htaccess merges last.
//
// nginx proxies a site to its owner's Apache. The Apache gets a site before
// nginx sends it there (applyApacheSite, then the nginx reload), and drops it
// after nginx stopped (settleApacheInstance): a site moves without a moment
// of 502. An Apache with no site left stops, is disabled and its directory
// goes; an account goes only after its Apache did.

const (
	// settingApacheInstances is "yes" once no site is served by the shared
	// Apache of 0.8.19 and older: the sites run on their accounts' own, and
	// apache2.service is off (moveToApacheInstances).
	settingApacheInstances = "stack.apache.instances"
	// apacheInstanceLog is an Apache's own log (start, stop, errors no site
	// owns), next to its sites' logs in the account's directory of the site
	// log tree.
	apacheInstanceLog = "apache.error.log"
	// sharedApachePID is the pid file of Debian's apache2.service.
	sharedApachePID = "/run/apache2/apache2.pid"
	// apacheWaitDefault is how long a newly started Apache may take to
	// answer on its address before nginx is sent there.
	apacheWaitDefault = 10 * time.Second
)

// apacheInstance is an account's Apache: its unit, its files, its address.
type apacheInstance struct {
	login  string
	addr   string // empty when the account's uid has no address
	unit   string
	conf   string
	sites  string
	runDir string
	check  []string
}

// apacheInstanceOf is the Apache of login (paths and unit, no address).
func apacheInstanceOf(login string) *apacheInstance {
	return &apacheInstance{
		login: login, unit: confcheck.ApacheInstanceUnit(login), conf: confcheck.ApacheInstanceConf(login),
		sites: confcheck.ApacheInstanceSites(login), runDir: confcheck.ApacheInstanceRunDir(login), check: confcheck.ApacheInstanceCheckArgv(login),
	}
}

// vhost is the file of a site's virtual host.
func (a *apacheInstance) vhost(domain string) string { return path.Join(a.sites, domain+".conf") }

// customDir holds a site's custom directives (inside its virtual host).
func (a *apacheInstance) customDir(domain string) string { return path.Join(a.sites, domain+".d") }

// apacheInstanceFor is the Apache of a site's owner, with its address.
func (s *Server) apacheInstanceFor(u *store.User) (*apacheInstance, error) {
	if s.profile.Family() != osprofile.FamilyDebian {
		return nil, errors.New("sites in Apache mode run on Debian and Ubuntu only")
	}
	if u.UnixUID == nil {
		return nil, errors.New("owner has no unix account yet")
	}
	inst := apacheInstanceOf(u.Login)
	addr, err := confcheck.ApacheInstanceAddr(*u.UnixUID)
	if err != nil {
		return nil, fmt.Errorf("the Apache of %s: %w", u.Login, err)
	}
	inst.addr = addr
	return inst, nil
}

// apacheInstalled says whether the panel installed Apache (mp stack install
// apache), which sites in Apache mode need.
func (s *Server) apacheInstalled(ctx context.Context) bool {
	v, _ := s.db.GetSetting(ctx, settingApache)
	return v == "installed"
}

// apacheServes says whether a site is served by its account's Apache.
func apacheServes(site *store.Site) bool {
	return site.Mode == store.ModeApache && site.Status != store.SiteSuspended && site.Status != store.SiteDeleting
}

// apacheSitesOf are the sites of an account that its Apache serves, but the
// one with the ID except.
func (s *Server) apacheSitesOf(ctx context.Context, userID, except int64) ([]*store.Site, error) {
	sites, err := s.db.ListSites(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []*store.Site
	for _, site := range sites {
		if site.ID != except && apacheServes(site) {
			out = append(out, site)
		}
	}
	return out, nil
}

// apacheAccounts are the accounts whose Apache runs (a site of theirs is in
// Apache mode, neither suspended nor being removed), by login.
func (s *Server) apacheAccounts(ctx context.Context) ([]*store.User, error) {
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return nil, err
	}
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	with := map[int64]bool{}
	for _, site := range sites {
		if apacheServes(site) {
			with[site.UserID] = true
		}
	}
	var out []*store.User
	for _, u := range users {
		if with[u.ID] && u.UnixUID != nil && u.Status != store.UserDeleting {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b *store.User) int { return strings.Compare(a.Login, b.Login) })
	return out, nil
}

// apacheUnits are the Apaches of the accounts that have one.
func (s *Server) apacheUnits(ctx context.Context) []string {
	if !s.apacheInstalled(ctx) {
		return nil
	}
	users, err := s.apacheAccounts(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, confcheck.ApacheInstanceUnit(u.Login))
	}
	return out
}

// apacheInstancesStatus counts the accounts' Apaches and the running ones
// (the stack's "apache").
func (s *Server) apacheInstancesStatus(ctx context.Context) *apitypes.StackInstances {
	out := &apitypes.StackInstances{}
	for _, unit := range s.apacheUnits(ctx) {
		out.Total++
		if st, err := s.agent.Service(ctx, unit, "status"); err == nil && st.Status.ActiveState == "active" {
			out.Running++
		}
	}
	return out
}

// apacheMainFiles are httpd.conf of an account's Apache and the note next
// to its virtual hosts.
func (s *Server) apacheMainFiles(inst *apacheInstance, u *store.User) ([]agent.FileSpec, error) {
	main, err := s.render.Render("apache/account.conf.tmpl", render.ApacheAccount{
		Login: u.Login, Listen: inst.addr, RunDir: inst.runDir, Home: s.homeOf(u),
		ErrorLog: path.Join(s.siteLogDir(u.Login), apacheInstanceLog), SitesDir: inst.sites,
	})
	if err != nil {
		return nil, err
	}
	return []agent.FileSpec{
		{Path: inst.conf, Content: main, Mode: 0o644},
		{Path: path.Join(inst.sites, "README"), Content: "Generated virtual hosts of the sites of " + u.Login + " in Apache mode. Custom directives go to sites/<domain>.d/*.conf.\n", Mode: 0o644},
	}, nil
}

// lockApache takes the account's Apache for one site's change and returns
// the release. The jobs of two sites of one account run side by side (each
// holds only its site's lock), and without it the one whose site leaves as
// the account's last could stop the Apache and take its httpd.conf away
// while the other gives it a site, which nginx then sends there; or remove
// the runtime directory under the other's apache2 -t.
func (s *Server) lockApache(login string) func() {
	m, _ := s.apacheLocks.LoadOrStore(login, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ensureApacheRunDir makes the runtime directory of an account's Apache
// before its configuration is checked: apache2 -t refuses a DefaultRuntimeDir
// or a Mutex directory that is not there, and systemd makes it (and removes
// it again) only with the unit's start and stop.
func (s *Server) ensureApacheRunDir(ctx context.Context, inst *apacheInstance) error {
	_, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{
		{Path: confcheck.ApacheInstanceRunRoot, Mode: 0o755, Owner: "root", Group: "root"},
		{Path: inst.runDir, Mode: 0o755, Owner: "root", Group: "root"},
	}})
	return err
}

// applyApacheSite gives a site to its account's Apache, before nginx sends
// the site there: httpd.conf and the virtual host, checked with apache2 -t,
// the Apache reloaded (graceful) or started and enabled. A started one is
// waited for until it answers on its address, so that nginx's reload, which
// follows, never sends a request where nothing listens yet.
func (s *Server) applyApacheSite(ctx context.Context, jc *jobs.Context, inst *apacheInstance, site *store.Site, u *store.User, rs render.Site) error {
	defer s.lockApache(u.Login)()
	rs.IncludeDir = inst.customDir(site.Domain)
	vhost, err := s.render.Render("apache/site.conf.tmpl", rs)
	if err != nil {
		return err
	}
	files, err := s.apacheMainFiles(inst, u)
	if err != nil {
		return err
	}
	files = append(files,
		agent.FileSpec{Path: inst.vhost(site.Domain), Content: vhost, Mode: 0o644},
		agent.FileSpec{Path: path.Join(rs.IncludeDir, "README"), Content: "Custom Apache directives for " + site.Domain + " (inside <VirtualHost>).\n", Mode: 0o644})
	// The custom directives a site had with the shared Apache of 0.8.19 come
	// along in the same checked set: the site never runs without them on its
	// account's Apache, whichever job moves it.
	carried, err := s.legacyApacheCustom(ctx, inst, site)
	if err != nil {
		return err
	}
	files = append(files, carried...)
	st, err := s.agent.Service(ctx, inst.unit, "status")
	if err != nil {
		return err
	}
	if st.Status.LoadState != "loaded" {
		return fmt.Errorf("%s is not a unit of this host (%s): the template monopanel-apache@.service comes with the panel's package (or run mp setup)", inst.unit, st.Status.LoadState)
	}
	running := st.Status.ActiveState == "active" || st.Status.ActiveState == "reloading"
	if err := s.ensureApacheRunDir(ctx, inst); err != nil {
		return err
	}
	apply, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files: files, Validate: [][]string{inst.check}, Reload: []string{inst.unit}, Force: !running, Origin: "site:" + site.Domain,
	})
	if err != nil {
		if len(carried) > 0 {
			return fmt.Errorf("the Apache of %s with the custom directives of %s from %s: %w", u.Login, site.Domain, path.Join(s.apacheLegacy, "sites", site.Domain+".d"), err)
		}
		return err
	}
	for _, f := range carried {
		jc.Logf("%s: custom Apache directives %s carried over to %s", site.Domain, path.Base(f.Path), path.Dir(f.Path))
	}
	if st.Status.UnitFileState != "enabled" {
		if _, err := s.agent.Service(ctx, inst.unit, "enable"); err != nil {
			return err
		}
	}
	switch {
	case !running:
		if err := s.waitApache(ctx, inst); err != nil {
			return err
		}
		jc.Logf("Apache of %s: %s started on %s, its children run as %s", u.Login, inst.unit, inst.addr, u.Login)
	case len(apply.Reloaded) > 0:
		jc.Logf("Apache of %s: %s reloaded (%s)", u.Login, inst.unit, inst.addr)
	}
	return nil
}

// waitApache waits until a started Apache accepts connections on its
// address (systemd's start of Type=simple returns before apache2 binds).
func (s *Server) waitApache(ctx context.Context, inst *apacheInstance) error {
	if s.apacheWait <= 0 {
		return nil
	}
	deadline := time.Now().Add(s.apacheWait)
	d := &net.Dialer{Timeout: time.Second}
	for {
		c, err := d.DialContext(ctx, "tcp", inst.addr)
		if err == nil {
			_ = c.Close() // the connection only had to be accepted
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%s does not answer on %s after %s: %v (journalctl -u %s)", inst.unit, inst.addr, s.apacheWait, err, inst.unit)
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// settleApacheInstance takes a site off its account's Apache once nginx no
// longer sends it there — it left Apache mode, it is suspended (gone false)
// or removed (gone true, its custom directives go too): the Apache reloads
// without it, checked first, or, with no site of the account left in it,
// retires.
func (s *Server) settleApacheInstance(ctx context.Context, logf func(string, ...any), u *store.User, site *store.Site, gone bool) error {
	if !s.apacheInstalled(ctx) || s.profile.Family() != osprofile.FamilyDebian {
		return nil
	}
	// The other sites are counted under the lock: a site that comes to the
	// account's Apache meanwhile is either counted or comes after this.
	defer s.lockApache(u.Login)()
	inst := apacheInstanceOf(u.Login)
	others, err := s.apacheSitesOf(ctx, u.ID, site.ID)
	if err != nil {
		return err
	}
	if len(others) == 0 {
		except := int64(0)
		if gone {
			except = site.ID
		}
		return s.retireApacheInstanceLocked(ctx, logf, u, except)
	}
	paths := []string{inst.vhost(site.Domain)}
	if gone {
		paths = append(paths, inst.customDir(site.Domain))
	}
	if err := s.ensureApacheRunDir(ctx, inst); err != nil {
		return fmt.Errorf("the Apache of %s: %w", u.Login, err)
	}
	rr, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: paths, Recursive: true, Validate: [][]string{inst.check}, Reload: []string{inst.unit}})
	if err != nil {
		return fmt.Errorf("the Apache of %s without %s: %w", u.Login, site.Domain, err)
	}
	if len(rr.Removed) > 0 {
		logf("Apache of %s: %s no longer served there; %s reloaded", u.Login, site.Domain, inst.unit)
	}
	return nil
}

// retireApacheInstance stops and disables an account's Apache, which no site
// needs (any more), and removes its configuration: httpd.conf and the
// virtual hosts, without which it cannot start, and the custom directives of
// the sites that go; the whole directory once the account has no site left.
// except is a site being removed, which counts as gone already.
func (s *Server) retireApacheInstance(ctx context.Context, logf func(string, ...any), u *store.User, except int64) error {
	defer s.lockApache(u.Login)()
	return s.retireApacheInstanceLocked(ctx, logf, u, except)
}

// retireApacheInstanceLocked is retireApacheInstance under lockApache.
func (s *Server) retireApacheInstanceLocked(ctx context.Context, logf func(string, ...any), u *store.User, except int64) error {
	inst := apacheInstanceOf(u.Login)
	st, err := s.agent.Service(ctx, inst.unit, "status")
	if err != nil {
		return fmt.Errorf("the Apache of %s: %w", u.Login, err)
	}
	if st.Status.LoadState == "loaded" {
		switch st.Status.ActiveState {
		case "inactive", "failed":
		default:
			if _, err := s.agent.Service(ctx, inst.unit, "stop"); err != nil {
				return fmt.Errorf("the Apache of %s: %w", u.Login, err)
			}
			logf("Apache of %s: %s stopped and disabled, it serves no site of the account any more", u.Login, inst.unit)
		}
		if st.Status.UnitFileState == "enabled" {
			if _, err := s.agent.Service(ctx, inst.unit, "disable"); err != nil {
				return fmt.Errorf("the Apache of %s: %w", u.Login, err)
			}
		}
	}
	sites, err := s.db.ListSites(ctx, u.ID)
	if err != nil {
		return err
	}
	// A site's custom directives are the administrator's and go only with
	// the site: one that is suspended or left Apache mode finds them again
	// when it comes back, as with the shared Apache of 0.8.19. Nothing that
	// stays is included by anything: no virtual host stays behind for the
	// next httpd.conf to include.
	gone := func(site *store.Site) bool { return site.ID == except || site.Status == store.SiteDeleting }
	paths := []string{confcheck.ApacheInstanceDir(u.Login)}
	if slices.ContainsFunc(sites, func(site *store.Site) bool { return !gone(site) }) {
		paths = []string{inst.conf, path.Join(inst.sites, "README")}
		for _, site := range sites {
			paths = append(paths, inst.vhost(site.Domain))
			if gone(site) {
				paths = append(paths, inst.customDir(site.Domain))
			}
		}
	}
	rr, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: paths, Recursive: true})
	if err != nil {
		return fmt.Errorf("the configuration of the Apache of %s: %w", u.Login, err)
	}
	if len(rr.Removed) > 0 {
		logf("Apache of %s: removed %s", u.Login, strings.Join(rr.Removed, ", "))
	}
	return nil
}

// apacheDoctor checks what keeps the accounts' Apaches apart: the shared
// Apache of 0.8.19 must not run (its children ran as www-data for every
// account), and no account may bind the Apaches' port.
func (s *Server) apacheDoctor(ctx context.Context) []apitypes.Check {
	var out []apitypes.Check
	web := s.profile.Web()
	if st, err := s.agent.Service(ctx, web.ApacheService, "status"); err == nil && st.Status.LoadState == "loaded" {
		if st.Status.ActiveState == "active" {
			out = append(out, check("service "+web.ApacheService, "warn", "the shared Apache of 0.8.19 and older still runs, its children as "+web.ApacheUser+" for every account: mp stack install apache moves the sites still on it to their accounts' own Apaches and stops it"))
		} else {
			out = append(out, check("service "+web.ApacheService, "ok", "off: the sites in Apache mode run on their accounts' own Apaches"))
		}
	}
	if err := s.apachePortHeld(); err != nil {
		out = append(out, check("apache port", "warn", err.Error()))
	}
	return out
}

// ---------------------------------------------------------- upgrade ----

// refreshApacheInstances moves the sites of a panel of 0.8.19 or older off
// the shared Apache, once: the apache.migrate job (moveToApacheInstances).
// A host without Apache has nothing to move.
func (s *Server) refreshApacheInstances(ctx context.Context) {
	if v, _ := s.db.GetSetting(ctx, settingApacheInstances); v == "yes" {
		return
	}
	if !s.apacheInstalled(ctx) || s.profile.Family() != osprofile.FamilyDebian {
		if err := s.db.SetSetting(ctx, settingApacheInstances, "yes"); err != nil {
			s.log.Warn("apache instances", "err", err)
		}
		return
	}
	if _, err := s.jobs.Enqueue(ctx, "apache.migrate", struct{}{}, jobs.WithLockKey("stack"), jobs.WithRequestedBy("system")); err != nil {
		s.log.Warn("apache: cannot queue the move of the sites to their accounts' Apaches; it runs at the next start", "err", err)
		return
	}
	s.log.Info("apache: the sites in Apache mode move from the shared apache2.service to their accounts' own Apaches")
}

// jobApacheMigrate is the move of an upgraded panel (refreshApacheInstances).
func (s *Server) jobApacheMigrate(ctx context.Context, jc *jobs.Context) error {
	if v, _ := s.db.GetSetting(ctx, settingApacheInstances); v == "yes" {
		jc.Progress(100, "the sites already run on their accounts' Apaches")
		return nil
	}
	if err := s.moveToApacheInstances(ctx, jc); err != nil {
		return err
	}
	jc.Progress(100, "sites in Apache mode run on their accounts' Apaches; apache2.service is off")
	return nil
}

// moveToApacheInstances puts every site in Apache mode on its account's own
// Apache and then retires the shared one: site by site, each Apache gets
// the site, with the administrator's custom directives of it, and answers
// before nginx is sent there (a site.apply each), the shared Apache serving
// the site until that reload. Only when every site moved does
// apache2.service stop: a site that did not keeps it running, and the job
// fails naming the site — mp stack install apache tries again.
func (s *Server) moveToApacheInstances(ctx context.Context, jc *jobs.Context) error {
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return err
	}
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return err
	}
	owners := map[int64]*store.User{}
	for _, u := range users {
		owners[u.ID] = u
	}
	loginOf := func(id int64) string {
		if u := owners[id]; u != nil {
			return u.Login
		}
		return ""
	}
	slices.SortFunc(sites, func(a, b *store.Site) int {
		if c := strings.Compare(loginOf(a.UserID), loginOf(b.UserID)); c != 0 {
			return c
		}
		return strings.Compare(a.Domain, b.Domain)
	})
	var failed []string
	// keep are custom directives of the shared Apache that stay where they
	// are: those of sites in another mode now that could not move.
	keep := map[string]bool{}
	moved := 0
	for _, site := range sites {
		u := owners[site.UserID]
		// A site of an account that has no unix user (or is going) cannot
		// be served by anything, the shared Apache included.
		if site.Status == store.SiteDeleting || u == nil || u.UnixUID == nil || u.Status == store.UserDeleting {
			continue
		}
		if !apacheServes(site) {
			// Nothing serves it on Apache now — it is suspended, or it left
			// Apache mode, which kept its custom directives in 0.8.19: they
			// wait in its directory of the account's Apache for its return.
			// Those of a site in another mode hold up nothing; they stay
			// where they are, and a return to Apache mode brings them along
			// (legacyApacheCustom) or names them.
			if err := s.carryApacheCustom(ctx, jc.Logf, site, u); err != nil {
				if site.Mode == store.ModeApache {
					failed = append(failed, site.Domain+": "+err.Error())
					continue
				}
				legacy := path.Join(s.apacheLegacy, "sites", site.Domain+".d")
				keep[legacy] = true
				jc.Logf("warning: %s (%s mode): %v; they stay in %s", site.Domain, site.Mode, err, legacy)
			}
			continue
		}
		jc.Logf("%s: to the Apache of %s", site.Domain, u.Login)
		if err := s.applySite(ctx, jc, sitePayload{SiteID: site.ID}); err != nil {
			failed = append(failed, site.Domain+": "+err.Error())
			continue
		}
		moved++
	}
	if len(failed) > 0 {
		return fmt.Errorf("sites that did not move to their account's Apache, which the shared apache2.service keeps serving: %s; fix them, then run mp stack install apache", strings.Join(failed, "; "))
	}
	if moved > 0 {
		jc.Logf("%d sites run on their accounts' Apaches", moved)
	}
	if err := s.retireSharedApache(ctx, jc.Logf, keep); err != nil {
		return err
	}
	if err := s.db.SetSetting(context.WithoutCancel(ctx), settingApacheInstances, "yes"); err != nil {
		return err
	}
	// The rotation reopens the accounts' Apaches now, not the shared one.
	if err := s.applySiteLogrotate(ctx); err != nil {
		jc.Logf("warning: site log rotation: %v", err)
	}
	return nil
}

// legacyApacheCustom reads the custom directives an administrator left for
// a site next to its virtual host of the shared Apache (sites/<domain>.d/*.conf
// of 0.8.19), to go to the site's directory of its account's Apache. A file
// that is there already stays as it is: it was carried over before, or
// written there anew. The agent checks them as directives of that account's
// Apache; one it refuses (an Include, a log in another directory, an
// APACHE_* variable, which nothing defines there) keeps the site where it is
// until it is fixed.
func (s *Server) legacyApacheCustom(ctx context.Context, inst *apacheInstance, site *store.Site) ([]agent.FileSpec, error) {
	old := path.Join(s.apacheLegacy, "sites", site.Domain+".d")
	entries, err := os.ReadDir(old)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("custom Apache directives in %s: %w", old, err)
	}
	var files []agent.FileSpec
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".conf") || name == ".conf" {
			continue
		}
		to := path.Join(inst.customDir(site.Domain), name)
		if st, err := s.agent.Stat(ctx, to); err != nil {
			return nil, err
		} else if len(st.Entries) == 1 && st.Entries[0].Exists {
			continue
		}
		b, err := os.ReadFile(path.Join(old, name))
		if err != nil {
			return nil, fmt.Errorf("custom Apache directives: %w", err)
		}
		files = append(files, agent.FileSpec{Path: to, Content: string(b), Mode: 0o644})
	}
	return files, nil
}

// carryApacheCustom carries the custom directives of a site no Apache
// serves now (suspended, or in another mode) over to its account's Apache
// directory, before the shared configuration goes.
func (s *Server) carryApacheCustom(ctx context.Context, logf func(string, ...any), site *store.Site, u *store.User) error {
	files, err := s.legacyApacheCustom(ctx, apacheInstanceOf(u.Login), site)
	if err != nil || len(files) == 0 {
		return err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Origin: "site:" + site.Domain}); err != nil {
		return fmt.Errorf("custom Apache directives in %s do not go to the Apache of %s: %w", path.Join(s.apacheLegacy, "sites", site.Domain+".d"), u.Login, err)
	}
	for _, f := range files {
		logf("%s: custom Apache directives %s carried over to %s", site.Domain, path.Base(f.Path), path.Dir(f.Path))
	}
	return nil
}

// retireSharedApache switches the distribution's Apache off for good: no
// site of the panel runs on it. apache2.service stops and is disabled. It
// is not masked: the agent has no such action, and Debian's maintainer
// scripts neither enable nor start a unit the administrator disabled
// (deb-systemd-helper was-enabled, deb-systemd-invoke), so an upgrade of
// apache2 leaves it off. The panel's include of 0.8.19 is switched off and
// the panel's files of it removed — but the custom directives in keep, which
// could not move (moveToApacheInstances) — and the distribution's Apache
// account leaves the web group, through which its children read every site.
func (s *Server) retireSharedApache(ctx context.Context, logf func(string, ...any), keep map[string]bool) error {
	web := s.profile.Web()
	st, err := s.agent.Service(ctx, web.ApacheService, "status")
	if err != nil {
		return err
	}
	if st.Status.LoadState == "loaded" {
		if st.Status.ActiveState != "inactive" && st.Status.ActiveState != "failed" {
			if _, err := s.agent.Service(ctx, web.ApacheService, "stop"); err != nil {
				return err
			}
			logf("%s stopped: the sites in Apache mode run on their accounts' own Apaches", web.ApacheService)
		}
		if st.Status.UnitFileState == "enabled" {
			if _, err := s.agent.Service(ctx, web.ApacheService, "disable"); err != nil {
				return err
			}
			logf("%s disabled: it does not start at boot", web.ApacheService)
		}
	}
	enabled := path.Join(web.ApacheConfDir, "conf-enabled", "monopanel.conf")
	if res, err := s.agent.Stat(ctx, enabled); err != nil {
		return err
	} else if len(res.Entries) == 1 && res.Entries[0].Exists {
		if _, err := s.agent.ApacheCtl(ctx, "disconf", "monopanel"); err != nil {
			return err
		}
		logf("a2disconf monopanel: the shared configuration of 0.8.19 is not included any more")
	}
	legacySites := path.Join(s.apacheLegacy, "sites")
	paths := []string{path.Join(s.apacheLegacy, "httpd.conf"), path.Join(web.ApacheConfDir, "conf-available", "monopanel.conf")}
	if len(keep) == 0 {
		paths = append(paths, legacySites)
	} else {
		entries, err := os.ReadDir(legacySites)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("the shared configuration of 0.8.19: %w", err)
		}
		for _, e := range entries {
			if p := path.Join(legacySites, e.Name()); !keep[p] {
				paths = append(paths, p)
			}
		}
	}
	rr, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Recursive: true, Paths: paths})
	if err != nil {
		return err
	}
	if len(rr.Removed) > 0 {
		logf("removed the shared configuration of 0.8.19: %s", strings.Join(rr.Removed, ", "))
	}
	if _, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: web.ApacheUser, System: true, RemoveGroups: []string{s.cfg.WebGroup}}); err != nil {
		return fmt.Errorf("%s out of %s: %w", web.ApacheUser, s.cfg.WebGroup, err)
	}
	return nil
}

// unprivilegedPortStart is net.ipv4.ip_unprivileged_port_start, the first
// port an account may bind: 1024 unless lowered (and on kernels before
// 4.11, which have no such setting).
func unprivilegedPortStart() int {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		return 1024
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 1024
	}
	return n
}

// apachePortHeld refuses a host where accounts may bind the port of the
// accounts' Apaches: one of them could take an Apache's address while it
// restarts and receive its sites' requests, passwords and cookies included.
func (s *Server) apachePortHeld() error {
	if start := s.portStart(); start <= confcheck.ApacheInstancePort {
		return fmt.Errorf("net.ipv4.ip_unprivileged_port_start is %d on this host: any account may bind port %d, where the accounts' Apaches listen, and receive their sites' requests while one restarts; set it back to 1024 (sysctl -w net.ipv4.ip_unprivileged_port_start=1024, and in /etc/sysctl.d to stay so)", start, confcheck.ApacheInstancePort)
	}
	return nil
}
