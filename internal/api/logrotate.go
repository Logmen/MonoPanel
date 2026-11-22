package api

import (
	"context"
	"path"
	"slices"

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
	// settingSiteLogsMoved is the log tree the sites were last re-applied to
	// write into (siteLogsRoot): moveSiteLogs runs once for it.
	settingSiteLogsMoved = "sites.logs_root"
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

// moveSiteLogs switches the sites of an upgraded panel to the log tree, once
// (and again should log_dir change): up to 0.8.19 nginx, Apache and php-fpm
// wrote the site logs into the account's data/logs, and they keep doing so
// until their configuration is rendered anew. It makes the directories and
// the logs of every account with sites, then queues site.apply for every site
// and mail.apply for a webmail published on a port; each job makes its own
// logs again before it applies anything. The old files stay where they are:
// they are the account's own, and nothing writes or rotates them any more.
// Should queuing fail, the next start tries again — a site applied twice is
// no harm.
func (s *Server) moveSiteLogs(ctx context.Context) {
	root := s.siteLogsRoot()
	if v, _ := s.db.GetSetting(ctx, settingSiteLogsMoved); v == root {
		return
	}
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
	mail := s.loadMailConfig(ctx)
	webmailPort := mail.Installed && mail.Webmail != "" && mail.WebmailPort > 0
	logs := map[string][]string{}
	var apply []*store.Site
	for _, site := range sites {
		u := owners[site.UserID]
		if u == nil || u.UnixUID == nil || u.Status == store.UserDeleting || site.Status == store.SiteDeleting {
			continue
		}
		logs[u.Login] = append(logs[u.Login], s.siteLogsOf(ctx, site)...)
		apply = append(apply, site)
	}
	logins := make([]string, 0, len(logs))
	for login := range logs {
		logins = append(logins, login)
	}
	slices.Sort(logins)
	for _, login := range logins {
		if _, err := s.ensureSiteLogs(ctx, login, logs[login]...); err != nil {
			// The account's site.apply jobs try again, and stop there.
			s.log.Warn("site logs: the new directory", "login", login, "dir", s.siteLogDir(login), "err", err)
		}
	}
	for _, site := range apply {
		if _, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: site.ID, LogsMoved: true}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy("system")); err != nil {
			s.log.Warn("site logs: cannot queue the site to switch its logs", "site", site.Domain, "err", err)
			return
		}
	}
	if webmailPort {
		if _, err := s.jobs.Enqueue(ctx, "mail.apply", struct{}{}, jobs.WithLockKey("mail"), jobs.WithRequestedBy("system")); err != nil {
			s.log.Warn("site logs: cannot queue the webmail to switch its logs", "err", err)
			return
		}
	}
	if err := s.db.SetSetting(ctx, settingSiteLogsMoved, root); err != nil {
		s.log.Warn("site logs: the move is not remembered and runs again at the next start", "err", err)
		return
	}
	if len(apply) > 0 {
		s.log.Info("site logs: the sites switch to the root-owned log tree; the old files stay in the accounts' data/logs",
			"dir", root, "accounts", len(logins), "sites", len(apply), "webmail", webmailPort)
	}
}
