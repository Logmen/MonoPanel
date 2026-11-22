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
	"monopanel/internal/apitypes"
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
// pool. On EL the master's log goes to syslog as well.
//
// The panel makes both so once per branch (settingPHPDistroIdle): when it
// installs the branch, or at the first start of this version for a branch an
// older panel installed. Package upgrades keep them (dpkg's conffile, rpm's
// %config(noreplace)). Afterwards they are the administrator's: one who
// brings the stock pool back — an apt-installed phpMyAdmin or a monitoring
// page needs its socket, or the maintainer's version taken at dpkg's prompt
// — gets a warning in the doctor, not a revert at every start, which would
// break what runs there again each time and keep dpkg prompting about the
// conffile. That leaves the panel's boundaries as they are: nothing the
// panel writes can send a request to that pool — nginx's fastcgi_pass may
// name only the panel's own sockets in <run_dir>/php (confcheck's nginx
// rules), and no Apache's children run as www-data any more, every account's
// own Apache runs as the account (apache.go) — so PHP runs as the web
// server's account only for what the administrator sends there. What the
// panel itself writes at that path stays the idle pool, byte for byte
// (confcheck.IdlePool). A pool file that is missing is written again: the
// master needs a pool, and removing the file is how an administrator hands
// it back to the panel.

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

// settingPHPDistroIdle is "yes" once the panel made the distribution's pool
// of the branch idle and, on EL, sent its master's log to syslog: after that
// the files are left as the administrator keeps them.
func settingPHPDistroIdle(version string) string { return "php.distro_idle." + version }

// refreshIdlePHPPools makes the distribution's pool of every installed branch
// idle at start, and on EL sends its master's log to syslog, for a branch
// that has not been made so yet: one an older panel installed, or one whose
// first attempt failed. A branch made so before keeps what the administrator
// did with the files since; a change is logged here and named by the doctor
// (phpDistroChecks), and only a pool file that is missing is written again.
// Only the two files of a branch are written, and only when they differ;
// php-fpm -t checks them, and the branch's php-fpm is reloaded then.
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
		if done, _ := s.db.GetSetting(ctx, settingPHPDistroIdle(v.Version)); done == "yes" {
			s.recheckPHPDistro(ctx, l)
			continue
		}
		complete, err := s.idlePHPDistro(ctx, l, true)
		if err != nil {
			s.log.Warn("php: the distribution's pool; the next start tries again", "version", v.Version, "err", err)
			continue
		}
		if complete {
			s.markPHPDistroIdle(ctx, v.Version)
		}
	}
}

// markPHPDistroIdle remembers that the branch's distribution files are the
// panel's way now.
func (s *Server) markPHPDistroIdle(ctx context.Context, version string) {
	if err := s.db.SetSetting(context.WithoutCancel(ctx), settingPHPDistroIdle(version), "yes"); err != nil {
		s.log.Warn("php: the idle pool is not remembered; the next start checks it again", "version", version, "err", err)
	}
}

// phpDistro is what a branch's distribution files are now (phpDistroState).
type phpDistro struct {
	// pool is "idle", "changed" or "missing"; "" without the pool
	// directory (the packages removed by hand).
	pool string
	// syslog: the master logs to syslog (always so off EL); confErr says
	// why php-fpm.conf could not be read.
	syslog  bool
	confErr error
}

// phpDistroState reads what a branch's distribution files are now.
func (s *Server) phpDistroState(ctx context.Context, l *osprofile.PHPLayout) (phpDistro, error) {
	st, err := s.agent.Stat(ctx, l.PoolDir)
	if err != nil {
		return phpDistro{}, err
	}
	if len(st.Entries) != 1 || !st.Entries[0].Exists || !st.Entries[0].IsDir {
		return phpDistro{syslog: true}, nil
	}
	idle, err := s.idlePHPPool(l)
	if err != nil {
		return phpDistro{}, err
	}
	var d phpDistro
	cur, err := s.agent.ReadFile(ctx, idle.Path, 64<<10)
	var ae *agent.Error
	switch {
	// The agent answers a missing file with nothing, the demo's with 404.
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		d.pool = "missing"
	case err != nil:
		return phpDistro{}, err
	case cur.Content == "" && !cur.Truncated:
		d.pool = "missing"
	case cur.Truncated || cur.Content != idle.Content:
		d.pool = "changed"
	default:
		d.pool = "idle"
	}
	conf, err := s.fpmSyslogConf(ctx, l)
	d.syslog, d.confErr = err == nil && conf == nil, err
	return d, nil
}

// recheckPHPDistro looks at the distribution's files of a branch the panel
// made idle before: a missing pool is written again, a changed one is the
// administrator's and only logged.
func (s *Server) recheckPHPDistro(ctx context.Context, l *osprofile.PHPLayout) {
	d, err := s.phpDistroState(ctx, l)
	if err != nil {
		s.log.Warn("php: the distribution's pool", "version", l.Version, "err", err)
		return
	}
	switch d.pool {
	case "missing":
		// The pool alone: php-fpm.conf stays as the administrator keeps it.
		if _, err := s.idlePHPDistro(ctx, l, false); err != nil {
			s.log.Warn("php: the distribution's pool is missing and cannot be written", "version", l.Version, "err", err)
		}
	case "changed":
		s.log.Warn("php: the distribution's pool is not the panel's idle pool any more; left as it is (mp doctor)", "version", l.Version, "pool", phpDistroPool(l))
	}
	if !d.syslog && d.confErr == nil {
		s.log.Warn("php: the php-fpm master logs into a file again; left as it is (mp doctor)", "version", l.Version, "conf", l.FPMConf)
	}
}

// idlePHPDistro brings one branch's pool — and with conf its php-fpm.conf —
// to what the panel leaves them, and tells whether both are so now.
func (s *Server) idlePHPDistro(ctx context.Context, l *osprofile.PHPLayout, conf bool) (bool, error) {
	// Packages removed by hand take the pool directory along, and
	// config/apply would make it again for a php-fpm that is not there.
	st, err := s.agent.Stat(ctx, l.PoolDir)
	if err != nil {
		return false, err
	}
	if len(st.Entries) != 1 || !st.Entries[0].Exists || !st.Entries[0].IsDir {
		return false, nil
	}
	pool, err := s.idlePHPPool(l)
	if err != nil {
		return false, err
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
		return false, err
	case cur.Truncated || cur.Content != pool.Content:
		files = append(files, pool)
	}
	complete := true
	if !conf {
		complete = false
	} else if c, err := s.fpmSyslogConf(ctx, l); err != nil {
		s.log.Warn("php: the php-fpm master keeps its log file", "version", l.Version, "err", err)
		complete = false
	} else if c != nil {
		files = append(files, *c)
	}
	if len(files) == 0 {
		return complete, nil
	}
	// The idle pool listens in the panel's socket directory, which php-fpm
	// needs to start: made as at an installation, should it be missing.
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: path.Join(s.cfg.RunDir, "php"), Mode: 0o755, Owner: "root", Group: "root"}}}); err != nil {
		return false, err
	}
	res, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{
		Files: files, Validate: [][]string{l.FPMCheckArgv}, Reload: []string{l.FPMService}, Origin: "php:" + l.Version,
	})
	if err != nil {
		return false, err
	}
	if len(res.Written) > 0 {
		s.log.Info("php: the distribution's pool is idle", "version", l.Version, "written", res.Written)
	}
	return complete, nil
}

// phpDistroChecks are the doctor's word on the distribution's pools: one the
// panel has not made idle yet, and one an administrator changed since.
func (s *Server) phpDistroChecks(ctx context.Context) []apitypes.Check {
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return nil
	}
	var out []apitypes.Check
	for _, v := range versions {
		l := phpLayout(s, v.Version)
		if v.Status != store.PHPInstalled || l == nil {
			continue
		}
		name := "php " + v.Version + " pool www"
		d, err := s.phpDistroState(ctx, l)
		switch {
		case err != nil:
			out = append(out, check(name, "warn", err.Error()))
			continue
		case d.pool == "":
			continue
		}
		if done, _ := s.db.GetSetting(ctx, settingPHPDistroIdle(v.Version)); done != "yes" {
			out = append(out, check(name, "warn", "the panel has not made the distribution's pool "+phpDistroPool(l)+" idle yet; it tries at every start of the API (journalctl -u monopanel-api)"))
			continue
		}
		var why []string
		if d.pool == "changed" {
			why = append(why, phpDistroPool(l)+" is not the panel's idle pool any more. The distribution's pool there runs PHP as "+s.profile.Web().ApacheUser+", the web server's account, which can reach root through what the root php-fpm master keeps in that account's directories; nothing of the panel sends requests to it, what you send there runs that way. The panel leaves the file as it is; to have the idle pool back, remove the file and restart monopanel-api")
		}
		switch {
		case d.confErr != nil:
			why = append(why, "cannot read "+l.FPMConf+": "+d.confErr.Error())
		case !d.syslog:
			why = append(why, l.FPMConf+" has the php-fpm master log into a file again, in a directory the apache account may write: set error_log = syslog there")
		}
		if len(why) > 0 {
			out = append(out, check(name, "warn", strings.Join(why, "; ")))
		}
	}
	return out
}
