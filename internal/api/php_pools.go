package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// The php-fpm package of every branch ships a pool of the distribution's,
// [www], which runs PHP as the web server's account: www-data on Debian and
// Ubuntu, who owns /run/php, where the root master writes its pid file and
// chowns the pool's socket by path; apache on EL, whose socket nginx may
// connect to (listen.acl_users) and who owns the directory of the root
// master's log. PHP as either account reaches root through those files. The
// sites never used that pool, and nothing else of the panel does (the
// webmail is a site with a pool of its own; there is no phpMyAdmin), so it
// stays idle (confcheck.IdlePool): a php-fpm master does not start without a
// pool. On EL the master's log goes to syslog as well. Both are written when
// a branch is installed and checked again at every start of the API:
// package upgrades keep a changed www.conf (dpkg's conffile, rpm's
// %config(noreplace)), but an administrator who takes the maintainer's
// version at dpkg's prompt brings the stock pool back.

// fpmConfMax is the most of php-fpm.conf the panel reads to edit it; the
// distributions' are some 5 KB.
const fpmConfMax = 256 << 10

// phpDistroPool is the pool file the branch's php-fpm package ships.
func phpDistroPool(l *osprofile.PHPLayout) string { return path.Join(l.PoolDir, "www.conf") }

// idlePHPPool is the distribution's pool file of the branch as the panel
// leaves it.
func (s *Server) idlePHPPool(l *osprofile.PHPLayout) (agent.FileSpec, error) {
	p := phpDistroPool(l)
	content, ok := confcheck.IdlePool(s.cfg.RunDir, p)
	if !ok {
		return agent.FileSpec{}, fmt.Errorf("no idle pool for %s with run_dir %q", p, s.cfg.RunDir)
	}
	return agent.FileSpec{Path: p, Content: content, Mode: 0o644}, nil
}

// fpmSyslogConf is the branch's php-fpm.conf with the master's log sent to
// syslog, on EL (Remi), where the log directory is apache's; nil where the
// file says so already, and on other families: Debian's master logs right in
// /var/log.
func (s *Server) fpmSyslogConf(ctx context.Context, l *osprofile.PHPLayout) (*agent.FileSpec, error) {
	if s.profile.Family() != osprofile.FamilyRHEL {
		return nil, nil
	}
	cur, err := s.agent.ReadFile(ctx, l.FPMConf, fpmConfMax)
	switch {
	case err != nil:
		return nil, err
	case cur.Truncated:
		return nil, fmt.Errorf("%s is larger than %d bytes", l.FPMConf, fpmConfMax)
	case cur.Content == "":
		return nil, fmt.Errorf("%s is missing or empty", l.FPMConf)
	}
	conf, changed := fpmSyslog(cur.Content)
	if !changed {
		return nil, nil
	}
	return &agent.FileSpec{Path: l.FPMConf, Content: conf, Mode: 0o644}, nil
}

// fpmSyslog is php-fpm.conf conf with the master's log sent to syslog: every
// error_log line outside the pools (before the first section and in
// [global]; php-fpm takes the last) becomes "error_log = syslog", and when
// there is none, one goes in after [global], or first in the file without
// that section. Everything else stays byte for byte. changed is false when
// the log goes to syslog already.
func fpmSyslog(conf string) (out string, changed bool) {
	const line = "error_log = syslog"
	lines := strings.Split(conf, "\n")
	section, global, found := "", -1, false
	for i, raw := range lines {
		s := strings.Trim(raw, " \t")
		if s == "" || s[0] == ';' || s[0] == '#' {
			continue
		}
		if s[0] == '[' {
			if end := strings.IndexByte(s, ']'); end > 0 {
				section = strings.ToLower(strings.Trim(s[1:end], " \t"))
				if section == "global" && global < 0 {
					global = i
				}
			}
			continue
		}
		if section != "" && section != "global" {
			continue
		}
		// php-fpm matches the names of its settings without regard to case.
		key, val, ok := strings.Cut(s, "=")
		if !ok || !strings.EqualFold(strings.Trim(key, " \t"), "error_log") {
			continue
		}
		found = true
		if fpmValue(val) != "syslog" {
			lines[i], changed = line, true
		}
	}
	switch {
	case found:
	case global >= 0:
		lines, changed = slices.Insert(lines, global+1, line), true
	default:
		lines, changed = slices.Insert(lines, 0, line), true
	}
	return strings.Join(lines, "\n"), changed
}

// fpmValue is an ini value without its comment, blanks and quotes.
func fpmValue(v string) string {
	v = strings.Trim(v, " \t")
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : 1+end]
		}
	}
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.Trim(v, " \t")
}

// refreshIdlePHPPools makes the distribution's pool of every installed
// branch idle again at start, and on EL sends its master's log to syslog:
// for a panel up to 0.8.19, which left both as the package shipped them, and
// after an upgrade that brought them back. Only the two files of a branch
// are written, and only when they differ; php-fpm -t checks them, and the
// branch's php-fpm is reloaded then.
func (s *Server) refreshIdlePHPPools(ctx context.Context) {
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return
	}
	for _, v := range versions {
		l := phpLayout(s, v.Version)
		if v.Status != store.PHPInstalled || l == nil {
			continue
		}
		if err := s.idlePHPDistro(ctx, l); err != nil {
			s.log.Warn("php: the distribution's pool", "version", v.Version, "err", err)
		}
	}
}

// idlePHPDistro brings one branch's pool and php-fpm.conf to what the panel
// leaves them (refreshIdlePHPPools).
func (s *Server) idlePHPDistro(ctx context.Context, l *osprofile.PHPLayout) error {
	// Packages removed by hand take the pool directory along, and
	// config/apply would make it again for a php-fpm that is not there.
	st, err := s.agent.Stat(ctx, l.PoolDir)
	if err != nil {
		return err
	}
	if len(st.Entries) != 1 || !st.Entries[0].Exists || !st.Entries[0].IsDir {
		return nil
	}
	pool, err := s.idlePHPPool(l)
	if err != nil {
		return err
	}
	var files []agent.FileSpec
	// A pool file that is missing (the agent answers with nothing; the
	// demo's with 404) is written: the master needs a pool.
	cur, err := s.agent.ReadFile(ctx, pool.Path, 64<<10)
	var ae *agent.Error
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		files = append(files, pool)
	case err != nil:
		return err
	case cur.Truncated || cur.Content != pool.Content:
		files = append(files, pool)
	}
	if conf, err := s.fpmSyslogConf(ctx, l); err != nil {
		s.log.Warn("php: the php-fpm master keeps its log file", "version", l.Version, "err", err)
	} else if conf != nil {
		files = append(files, *conf)
	}
	if len(files) == 0 {
		return nil
	}
	// The idle pool listens in the panel's socket directory, which php-fpm
	// needs to start: made as at an installation, should it be missing.
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: path.Join(s.cfg.RunDir, "php"), Mode: 0o755, Owner: "root", Group: "root"}}}); err != nil {
		return err
	}
	res, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files: files, Validate: [][]string{l.FPMCheckArgv}, Reload: []string{l.FPMService}, Origin: "php:" + l.Version,
	})
	if err != nil {
		return err
	}
	if len(res.Written) > 0 {
		s.log.Info("php: the distribution's pool is idle again", "version", l.Version, "written", res.Written)
	}
	return nil
}
