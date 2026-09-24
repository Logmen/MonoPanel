package api

import (
	"context"
	"path"
	"path/filepath"
	"slices"

	"monopanel/internal/agent"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Site logs live in the accounts' homes (data/logs), where neither the
// distribution's nor the web servers' own logrotate configuration reaches.
// The panel keeps one stanza per account in siteLogrotatePath, rotating under
// that account's rights.

const (
	siteLogrotatePath = "/etc/logrotate.d/monopanel-sites"
	nginxPIDFile      = "/run/nginx.pid" // pid in templates/nginx/nginx.conf.tmpl
)

// applySiteLogrotate rewrites the rotation of the site logs for every account
// with a home and checks it with logrotate -d, so a broken file never replaces
// a working one. An account being deleted drops out before its unix user goes:
// logrotate must never meet a su user that no longer exists.
func (s *Server) applySiteLogrotate(ctx context.Context) error {
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return err
	}
	m := render.SiteLogrotate{NginxPID: nginxPIDFile, ApachePID: s.profile.Web().ApachePID}
	for _, u := range users {
		if u.UnixUID == nil || u.Status == store.UserDeleting {
			continue
		}
		home := u.Home
		if home == "" {
			home = filepath.Join(s.cfg.WWWRoot, u.Login)
		}
		m.Accounts = append(m.Accounts, render.LogrotateAccount{Login: u.Login, LogDir: filepath.Join(home, "data", "logs")})
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

// siteLogNames are the logs a site has: nginx's, Apache's in apache mode, and
// PHP's error log and php-fpm's slow log unless the site is a proxy.
func siteLogNames(site *store.Site) []string {
	names := []string{site.Domain + ".access.log", site.Domain + ".error.log"}
	if site.Mode == store.ModeApache {
		names = append(names, site.Domain+".apache.access.log", site.Domain+".apache.error.log")
	}
	if site.Mode != store.ModeProxy {
		names = append(names, site.Domain+".php.error.log", site.Domain+".php.slow.log")
	}
	return names
}

// ensureSiteLogs hands a site's logs to the account before the web servers
// open them: the files exist, belong to the account and have mode 0660.
// logrotate before 3.19 (EL9) opens a log to rotate it and read-write to
// compress it, which the account (su) could not do with a file nginx made as
// root or with the slow log php-fpm makes readable by root alone; nginx changes
// only the owner when it reopens a log and php-fpm keeps a slow log it finds,
// so the group and the mode stay. A file that is there already is handed over
// through the agent, never by name.
func (s *Server) ensureSiteLogs(ctx context.Context, login, logDir string, names ...string) error {
	for _, n := range names {
		p := path.Join(logDir, n)
		if _, err := s.agent.EnsureFile(ctx, &agent.EnsureFileRequest{Path: p, Owner: login, Group: login, Mode: 0o660, OnlyIfMissing: true}); err != nil {
			return err
		}
		if _, err := s.agent.Chown(ctx, &agent.ChownRequest{Path: p, Owner: login, Group: login, Mode: 0o660}); err != nil {
			return err
		}
	}
	return nil
}

// refreshSiteLogrotate writes the rotation at start, so an upgraded panel
// covers the accounts it already has, lets the web group through their
// data/logs as siteTree does now — without that, the nginx workers cannot
// reopen the site logs after USR1, theirs or the distribution's nightly one,
// and keep writing into the rotated files — and hands the logs nginx and
// Apache made as root over to the accounts.
func (s *Server) refreshSiteLogrotate(ctx context.Context) {
	if err := s.applySiteLogrotate(ctx); err != nil {
		s.log.Warn("site log rotation", "err", err)
	}
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return
	}
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return
	}
	for _, u := range users {
		if u.UnixUID == nil || !slices.ContainsFunc(sites, func(site *store.Site) bool { return site.UserID == u.ID }) {
			continue
		}
		dir := filepath.Join(s.cfg.WWWRoot, u.Login, "data", "logs")
		if u.Home != "" {
			dir = filepath.Join(u.Home, "data", "logs")
		}
		if err := s.agent.SetACL(ctx, &agent.SetACLRequest{Path: dir, Entries: []string{"g:" + s.cfg.WebGroup + ":x"}}); err != nil {
			s.log.Warn("site logs acl", "dir", dir, "err", err)
		}
		for _, site := range sites {
			if site.UserID != u.ID {
				continue
			}
			if err := s.ensureSiteLogs(ctx, u.Login, dir, siteLogNames(site)...); err != nil {
				s.log.Warn("site logs", "site", site.Domain, "err", err)
			}
		}
	}
}
