package api

import (
	"context"
	"path"
	"sort"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// cliPHPLink is an account's data/bin/php: the account's cron jobs
// (crontab.tmpl) and its sites' exec("php") (pool.conf.tmpl) find php there
// first in PATH.
func (s *Server) cliPHPLink(u *store.User) string {
	return path.Join(s.homeOf(u), "data", "bin", "php")
}

// cliPHPBranch is the branch data/bin/php of an account points at: the one
// its sites run, the newest of them when they run several, and without a
// site on PHP the newest branch installed. It depends on the sites alone, so
// the link moves only when the branches of the sites do. "" when there is no
// branch at all.
func (s *Server) cliPHPBranch(ctx context.Context, u *store.User) string {
	newest := ""
	sites, _ := s.db.ListSites(ctx, u.ID)
	for _, site := range sites {
		if site.Status == store.SiteDeleting || site.Mode == store.ModeProxy || site.PHPVersion == "" {
			continue
		}
		if newest == "" || versionLess(newest, site.PHPVersion) {
			newest = site.PHPVersion
		}
	}
	if newest != "" {
		return newest
	}
	versions, _ := s.db.ListPHPVersions(ctx)
	for _, v := range versions {
		if v.Status == store.PHPInstalled && phpLayout(s, v.Version) != nil && (newest == "" || versionLess(newest, v.Version)) {
			newest = v.Version
		}
	}
	return newest
}

// syncCLIPHP points data/bin/php of an account at cliPHPBranch, after a site
// is applied or removed and after a branch is removed. It used to be made
// once and never moved: the sites went to 8.2 and cron stayed on 7.4, until a
// CMS update in PHP 8 syntax broke every cron run (new.qaj.kz, 26.11.2026).
// A move is logged — the account's cron jobs run another PHP from then on,
// one that may run a job the old branch failed on — and the CLI settings of
// the new branch are refreshed.
func (s *Server) syncCLIPHP(ctx context.Context, logf func(string, ...any), u *store.User) {
	v := s.cliPHPBranch(ctx, u)
	l := phpLayout(s, v)
	if l == nil || l.CLIBinary == "" {
		return
	}
	res, err := s.agent.EnsureSymlink(ctx, &agent.EnsureSymlinkRequest{Path: s.cliPHPLink(u), Target: l.CLIBinary, Owner: u.Login})
	if err != nil {
		logf("warning: php CLI symlink: %v", err)
		return
	}
	if res.Was != "" {
		logf("php CLI of %s: data/bin/php is PHP %s now (was %s): its cron jobs and its sites' exec(\"php\") run it from now on", u.Login, v, s.cliPHPName(ctx, res.Was))
	}
	if res.Changed {
		if err := s.refreshPHPCLIIni(ctx, l); err != nil {
			logf("warning: PHP CLI settings: %v", err)
		}
	}
}

// cliPHPName is the branch whose CLI binary a link points at, as "PHP 7.4",
// else the path itself.
func (s *Server) cliPHPName(ctx context.Context, target string) string {
	if v := s.cliPHPOf(ctx, target); v != "" {
		return "PHP " + v
	}
	return target
}

// cliPHPOf is the branch the panel knows whose CLI binary is target, or "".
func (s *Server) cliPHPOf(ctx context.Context, target string) string {
	versions, _ := s.db.ListPHPVersions(ctx)
	for _, v := range versions {
		if l := phpLayout(s, v.Version); l != nil && l.CLIBinary == target {
			return v.Version
		}
	}
	return ""
}

// relinkCLIPHP moves data/bin/php of every account that points at a removed
// branch's CLI binary (no site runs it: php-remove refuses then) to the one
// cliPHPBranch names now — the branch is "removing" by then, so not that
// one — else it would point at nothing and every cron job with php would
// fail quietly.
func (s *Server) relinkCLIPHP(ctx context.Context, logf func(string, ...any), removed *osprofile.PHPLayout) {
	users, err := s.db.ListUsers(ctx)
	if err != nil || len(users) == 0 || removed.CLIBinary == "" {
		return
	}
	links := make([]string, len(users))
	for i, u := range users {
		links[i] = s.cliPHPLink(u)
	}
	st, err := s.agent.Stat(ctx, links...)
	if err != nil {
		logf("warning: php CLI symlinks of the accounts: %v", err)
		return
	}
	for i, e := range st.Entries {
		if i < len(users) && e.Link == removed.CLIBinary {
			s.syncCLIPHP(ctx, logf, users[i])
		}
	}
}

// cliPHPCheck is the doctor's "cli php" line: data/bin/php of every account
// with a site on PHP points at the branch cliPHPBranch names. One that points
// elsewhere is a warning (the next site apply moves it), one that points at
// nothing a failure: every cron job with php fails, quietly.
func (s *Server) cliPHPCheck(ctx context.Context) *apitypes.Check {
	const name = "cli php"
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return nil
	}
	type account struct {
		u    *store.User
		want *osprofile.PHPLayout
	}
	var accounts []account
	var links []string
	for _, u := range users {
		sites, _ := s.db.ListSites(ctx, u.ID)
		onPHP := false
		for _, site := range sites {
			onPHP = onPHP || (site.Status != store.SiteDeleting && site.Mode != store.ModeProxy && site.PHPVersion != "")
		}
		if !onPHP {
			continue
		}
		if l := phpLayout(s, s.cliPHPBranch(ctx, u)); l != nil && l.CLIBinary != "" {
			accounts = append(accounts, account{u, l})
			links = append(links, s.cliPHPLink(u))
		}
	}
	if len(accounts) == 0 {
		return nil
	}
	st, err := s.agent.Stat(ctx, links...)
	if err != nil || len(st.Entries) != len(accounts) {
		return nil
	}
	var warn, fail, targets []string
	off := map[int]string{}
	for i, e := range st.Entries {
		a := accounts[i]
		switch {
		case !e.Exists:
			warn = append(warn, a.u.Login+": no data/bin/php, mp site apply <a site of it> makes it")
		case e.Link == a.want.CLIBinary:
		case e.Link == "":
			warn = append(warn, a.u.Login+": data/bin/php is not a symlink")
		default:
			off[i] = e.Link
			targets = append(targets, e.Link)
		}
	}
	if len(targets) > 0 {
		there := map[string]bool{}
		if ts, err := s.agent.Stat(ctx, targets...); err == nil {
			for _, e := range ts.Entries {
				there[e.Path] = e.Exists
			}
		}
		for i := range accounts {
			target, ok := off[i]
			if !ok {
				continue
			}
			a := accounts[i]
			if !there[path.Clean(target)] {
				fail = append(fail, a.u.Login+": data/bin/php points at "+target+", which is gone: its cron jobs with php fail; mp site apply <a site of it> relinks it")
				continue
			}
			warn = append(warn, a.u.Login+": data/bin/php is "+s.cliPHPName(ctx, target)+", its sites run PHP "+a.want.Version+": cron and exec(\"php\") use the other one; mp site apply <a site of it> relinks it")
		}
	}
	sort.Strings(warn)
	sort.Strings(fail)
	switch {
	case len(fail) > 0:
		c := check(name, "fail", strings.Join(append(fail, warn...), "; "))
		return &c
	case len(warn) > 0:
		c := check(name, "warn", strings.Join(warn, "; "))
		return &c
	}
	c := check(name, "ok", "data/bin/php of every account runs the PHP of its sites")
	return &c
}
