package api

import (
	"context"
	"path"
	"slices"
	"strings"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Site logs live in <log_dir>/sites/<login>/, a tree only root writes, and not
// in the accounts' homes: nginx, Apache and the php-fpm master open them as
// root, and a log the account could swap for a symlink (data/logs was the
// account's, and so was data/ above it) would turn their writes into root's
// writes to /etc/ld.so.preload or /etc/cron.d. The account reads its logs
// through its group, the panel and mp site logs through the agent. Neither the
// distribution's nor the web servers' own logrotate configuration reaches the
// tree: the panel keeps two stanzas per account in siteLogrotatePath.

const (
	siteLogrotatePath = "/etc/logrotate.d/monopanel-sites"
	nginxPIDFile      = "/run/nginx.pid" // pid in templates/nginx/nginx.conf.tmpl
)

// webmailLogNames are the logs of the webmail published on a port, next to
// the logs of the site it lives in.
var webmailLogNames = []string{"webmail.access.log", "webmail.error.log"}

// siteLogsRoot is the tree of all site logs, <log_dir>/sites.
func (s *Server) siteLogsRoot() string { return path.Join(s.cfg.LogDir, "sites") }

// siteLogDir is an account's directory of site logs: root:<login> 0751, the
// agent makes it (agent.SiteLogsRequest).
func (s *Server) siteLogDir(login string) string { return path.Join(s.siteLogsRoot(), login) }

// applySiteLogrotate rewrites the rotation of the site logs for every account
// with a unix user and checks it with logrotate -d, so a broken file never
// replaces a working one. An account being deleted drops out before its unix
// user goes: logrotate must never meet a user or group (create) that no longer
// exists.
func (s *Server) applySiteLogrotate(ctx context.Context) error {
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return err
	}
	m := render.SiteLogrotate{NginxPID: nginxPIDFile}
	// Each account's Apache writes the Apache logs of its sites and reopens
	// them; the shared Apache of 0.8.19 does so for the sites not moved yet.
	apache := s.apacheInstalled(ctx) && s.profile.Family() == osprofile.FamilyDebian
	if v, _ := s.db.GetSetting(ctx, settingApacheInstances); apache && v != "yes" {
		m.SharedApachePID = sharedApachePID
	}
	for _, u := range users {
		if u.UnixUID == nil || u.Status == store.UserDeleting {
			continue
		}
		a := render.LogrotateAccount{Login: u.Login, LogDir: s.siteLogDir(u.Login)}
		if apache {
			a.ApachePID = path.Join(confcheck.ApacheInstanceRunDir(u.Login), "httpd.pid")
		}
		m.Accounts = append(m.Accounts, a)
	}
	text, err := s.render.Render("logrotate/sites.tmpl", m)
	if err != nil {
		return err
	}
	_, err = s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files:    []agent.FileSpec{{Path: siteLogrotatePath, Content: text, Mode: 0o644}},
		Validate: [][]string{{"/usr/sbin/logrotate", "--debug", siteLogrotatePath}},
		Origin:   "logrotate",
	})
	return err
}

// siteLogNames are the logs a site has: nginx's, Apache's in apache mode (and
// the log of the account's Apache itself), and PHP's error log and
// php-fpm's slow log unless the site is a proxy.
func siteLogNames(site *store.Site) []string {
	names := []string{site.Domain + ".access.log", site.Domain + ".error.log"}
	if site.Mode == store.ModeApache {
		names = append(names, site.Domain+".apache.access.log", site.Domain+".apache.error.log", apacheInstanceLog)
	}
	if site.Mode != store.ModeProxy {
		names = append(names, site.Domain+".php.error.log", site.Domain+".php.slow.log")
	}
	return names
}

// siteLogsOf are the logs of a site and, when it holds the webmail published
// on a port, the webmail's.
func (s *Server) siteLogsOf(ctx context.Context, site *store.Site) []string {
	names := siteLogNames(site)
	if c := s.loadMailConfig(ctx); c.Installed && c.Webmail == site.Domain && c.WebmailPort > 0 {
		names = append(names, webmailLogNames...)
	}
	return names
}

// ensureSiteLogs makes an account's logs before the web servers open them: in
// the account's directory of the root-owned tree, the servers' logs
// root:<login> 0640 and the PHP error log <login>:<login> 0660 (the agent
// decides which is which by name). nginx creates a missing log itself but not
// a missing directory, and php-fpm refuses to start without its slow log's,
// so this runs before their configuration is applied. Files that exist get
// owner and mode back; nginx hands its own to its worker user again when it
// reopens them, keeping the group and the mode.
func (s *Server) ensureSiteLogs(ctx context.Context, login string, names ...string) (*agent.SiteLogsResponse, error) {
	return s.agent.EnsureSiteLogs(ctx, &agent.SiteLogsRequest{Login: login, Files: names})
}

// refreshSiteLogrotate writes the rotation at start, so an upgraded panel
// covers the accounts it already has, in the tree it uses now.
func (s *Server) refreshSiteLogrotate(ctx context.Context) {
	if err := s.applySiteLogrotate(ctx); err != nil {
		s.log.Warn("site log rotation", "err", err)
	}
}

// moveSiteLogs switches the sites of an upgraded panel to the log tree: up
// to 0.8.19 nginx, Apache and php-fpm wrote the site logs into the account's
// data/logs, and they go on doing so — as root, in a directory the account
// owns — until a site.apply renders the site anew. At the first start of this
// version (and again should log_dir change) it lists every site with such a
// configuration in the moves (siteMoves), and the webmail published on a
// port; then, at every start and once an hour, it queues a site.apply (a
// mail.apply) for each one still there whose next try is due, after making
// the logs of their accounts. A site leaves the list with the first
// site.apply that succeeds, whoever queued it; one whose job failed waits
// longer each time (retryDelay), and the doctor says why it has not moved.
// On a host with SELinux nothing is queued before the tree's label rule is
// in place (siteLogsLabelled): the next start tries the rule again. The old
// files stay where they are: they are the account's own, and nothing writes
// them once the site has moved.
//
// Until then the rotation and fail2ban do not cover the site's logs in
// data/logs, and that is the right way round: the agent refuses logrotate
// stanzas outside the tree (and su, which rotating in a directory of the
// account's would need) and fail2ban log paths outside /var/log and the
// tree, as root rotating or reading files where the account may plant a
// symlink or a FIFO is what the move ends. The jails watch the site's files
// in the tree instead, empty until it moves, from when the move made them
// (fail2banSiteLogs); the cure is the move, which the doctor asks for.
func (s *Server) moveSiteLogs(ctx context.Context) {
	root := s.siteLogsRoot()
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		s.log.Warn("site logs: cannot list the sites to move their logs", "err", err)
		return
	}
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		s.log.Warn("site logs: cannot list the accounts to move their logs", "err", err)
		return
	}
	owners := map[int64]*store.User{}
	for _, u := range users {
		owners[u.ID] = u
	}
	byID := map[int64]*store.Site{}
	for _, site := range sites {
		byID[site.ID] = site
	}
	mail := s.loadMailConfig(ctx)
	webmailPort := mail.Installed && mail.Webmail != "" && mail.WebmailPort > 0
	labelled := s.siteLogsLabelled(ctx)
	now := time.Now()
	var queue []*store.Site
	listed, waiting, pending := 0, 0, 0
	queueWebmail := false
	err = s.changeSiteMoves(ctx, func(m *siteMoves) bool {
		if m.Logs != root {
			*m = siteMoves{Logs: root, Sites: map[int64]*siteMove{}}
			for _, site := range sites {
				if u := owners[site.UserID]; oldLayoutSite(site, u) {
					m.Sites[site.ID] = &siteMove{Domain: site.Domain}
				}
			}
			if webmailPort {
				m.Webmail = &retry{}
			}
			listed = len(m.Sites)
		}
		for id, mv := range m.Sites {
			site := byID[id]
			var u *store.User
			if site != nil {
				u = owners[site.UserID]
			}
			// Gone, or going: nothing renders it again.
			if site == nil || site.Status == store.SiteDeleting || u == nil || u.Status == store.UserDeleting {
				delete(m.Sites, id)
				continue
			}
			pending++
			switch {
			case mv.settle(ctx, s.db):
				// queued or running
			case !mv.due(now):
				waiting++
			case labelled && u.UnixUID != nil:
				queue = append(queue, site)
			}
		}
		if m.Webmail != nil {
			switch {
			case !webmailPort:
				m.Webmail = nil
			case !m.Webmail.settle(ctx, s.db) && labelled && m.Webmail.due(now):
				queueWebmail = true
			}
		}
		return true
	})
	if err != nil {
		s.log.Warn("site logs: the moves are not stored; the next start lists them again", "err", err)
		return
	}
	if listed > 0 {
		s.log.Info("site logs: the sites switch to the root-owned log tree; the old files stay in the accounts' data/logs", "dir", root, "sites", listed, "webmail", webmailPort)
	}
	if !labelled {
		if pending > 0 {
			s.log.Warn("site logs: the sites stay on their data/logs until the SELinux label rule of the log tree is in place (see \"selinux: site logs context\" above); the next start tries again", "dir", root, "sites", pending)
		}
		return
	}
	if waiting > 0 {
		s.log.Info("site logs: sites whose move failed wait for their next try (mp doctor)", "sites", waiting)
	}
	if len(queue) == 0 && !queueWebmail {
		return
	}
	// The accounts' directories and logs come first; each job makes its own
	// logs again before it applies anything.
	logs := map[string][]string{}
	for _, site := range queue {
		u := owners[site.UserID]
		logs[u.Login] = append(logs[u.Login], s.siteLogsOf(ctx, site)...)
	}
	logins := make([]string, 0, len(logs))
	for login := range logs {
		logins = append(logins, login)
	}
	slices.Sort(logins)
	made := map[string]bool{}
	for _, login := range logins {
		if _, err := s.ensureSiteLogs(ctx, login, logs[login]...); err != nil {
			// The account's site.apply jobs try again, and stop there.
			s.log.Warn("site logs: the new directory", "login", login, "dir", s.siteLogDir(login), "err", err)
			continue
		}
		made[login] = true
	}
	// Before any of the jobs can run: the jails list these logs from now on
	// (siteMove.Made), and no job's refresh of the jails sees them missing.
	if err := s.changeSiteMoves(ctx, func(m *siteMoves) bool {
		changed := false
		for _, site := range queue {
			if mv := m.Sites[site.ID]; mv != nil && !mv.Made && made[owners[site.UserID].Login] {
				mv.Made, changed = true, true
			}
		}
		return changed
	}); err != nil {
		s.log.Warn("site logs: the logs made in the tree are not stored", "err", err)
	}
	queued := map[int64]int64{}
	var domains []string
	for _, site := range queue {
		j, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: site.ID, LogsMoved: true}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy("system"))
		if err != nil {
			s.log.Warn("site logs: cannot queue the site to switch its logs; the next start tries again", "site", site.Domain, "err", err)
			break
		}
		queued[site.ID] = j.ID
		domains = append(domains, site.Domain)
	}
	var webmailJob int64
	if queueWebmail {
		if j, err := s.jobs.Enqueue(ctx, "mail.apply", struct{}{}, jobs.WithLockKey("mail"), jobs.WithRequestedBy("system")); err != nil {
			s.log.Warn("site logs: cannot queue the webmail to switch its logs; the next start tries again", "err", err)
		} else {
			webmailJob = j.ID
		}
	}
	// A job that ran already took its site out; one that failed counts at
	// the next look.
	if err := s.changeSiteMoves(ctx, func(m *siteMoves) bool {
		for id, job := range queued {
			if mv := m.Sites[id]; mv != nil && mv.Job == 0 {
				mv.Job = job
			}
		}
		if m.Webmail != nil && webmailJob != 0 && m.Webmail.Job == 0 {
			m.Webmail.Job = webmailJob
		}
		return len(queued) > 0 || webmailJob != 0
	}); err != nil {
		s.log.Warn("site logs: the queued moves are not stored", "err", err)
	}
	if len(domains) > 0 && listed == 0 {
		s.log.Info("site logs: queued again the sites whose move did not finish", "sites", strings.Join(domains, ", "))
	}
}

// oldLayoutSite tells whether a site's live configuration, as an older panel
// left it, may log into the account's data/logs: one applied with its logs
// (the suspended stub has none) of an account that has its unix user and
// stays. A pending site counts too: it is a new one whose first apply has
// not run — one apply more does no harm — or one in error an administrator
// changed or applied again, whose apply the upgrade interrupted, and that
// one still runs on what the older panel rendered; left out, nothing would
// move it should its apply fail, and the move of the sites to their
// accounts' Apaches would take it for moved and stop the shared Apache that
// still serves it.
func oldLayoutSite(site *store.Site, u *store.User) bool {
	if u == nil || u.UnixUID == nil || u.Status == store.UserDeleting {
		return false
	}
	return site.Status == store.SiteActive || site.Status == store.SiteError || site.Status == store.SitePending
}
