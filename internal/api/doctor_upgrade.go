package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/apitypes"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
	"monopanel/internal/systemd"
)

// replacedBinaryCheck is the doctor's line on a running service whose binary
// an upgrade (the panel's or the distribution's) replaced: the old code, the
// vulnerable one when the upgrade was a fix, runs until a restart. mp stack
// upgrade restarts nginx, php-fpm and the accounts' Apaches after checking
// their configuration (so does its daily run), the database server only as
// db; it restarts nothing else — fail2ban on a Python the distribution
// upgraded is restarted by hand.
func (s *Server) replacedBinaryCheck(ctx context.Context, unit string, st systemd.Status) apitypes.Check {
	comp := s.upgradeComponentOfUnit(ctx, unit)
	how := "systemctl restart " + unit
	switch comp {
	case "":
	case osprofile.UpgradeDB:
		how = "mp stack upgrade db restarts it after mysqld --validate-config (the sites lose their databases meanwhile)"
	default:
		how = "mp stack upgrade " + comp + " restarts it after checking the configuration"
		if auto := s.loadAutoUpgrade(ctx); !auto.Off {
			how += fmt.Sprintf(", as does the daily upgrade at %02d:00", auto.hour())
		}
	}
	c := check("service "+unit, "warn", st.SubState+", but on the binary an upgrade replaced: the new version runs only after a restart; "+how)
	if comp != "" {
		c.Action, c.Target = "stack.upgrade", comp
	}
	return c
}

// upgradeComponentOfUnit is the component of mp stack upgrade that restarts
// a unit, "" for none.
func (s *Server) upgradeComponentOfUnit(ctx context.Context, unit string) string {
	if unit == s.profile.Web().NginxService {
		return osprofile.UpgradeNginx
	}
	if _, ok := confcheck.ApacheInstanceLoginOf(unit); ok {
		return osprofile.UpgradeApache
	}
	if versions, err := s.db.ListPHPVersions(ctx); err == nil {
		for _, v := range versions {
			if v.FPMService == unit {
				return osprofile.UpgradePHP
			}
		}
	}
	if inst, err := s.db.GetDBInstance(ctx); err == nil && inst.Service == unit {
		return osprofile.UpgradeDB
	}
	return ""
}

// upgradeChecks are the doctor's lines on the vendor packages: one for each
// component with an update waiting (a CVE fixed in its repository is open on
// this server until it is installed), one when none waits, and the last
// upgrade's failure, which stays until an upgrade succeeds.
func (s *Server) upgradeChecks(ctx context.Context) []apitypes.Check {
	var out []apitypes.Check
	targets := s.upgradeTargets(ctx, upgradeAll, osprofile.UpgradeDB)
	st, err := s.upgradeState(ctx, targets)
	if err != nil {
		out = append(out, check("updates", "warn", "cannot ask the package manager what the repositories offer: "+err.Error()))
	} else {
		auto := s.loadAutoUpgrade(ctx)
		var current []string
		for _, t := range targets {
			if !st.present(t) {
				continue
			}
			if up := st.held[t.name]; up != nil {
				names, list := heldNames(up)
				out = append(out, check("held "+t.name, "warn", fmt.Sprintf("newer versions wait, but the administrator holds %s (apt-mark hold): the panel leaves them, and what the update fixes stays open; apt-mark unhold %s, then mp stack upgrade %s",
					strings.Join(list, ", "), strings.Join(names, " "), t.component)))
			}
			up := st.byName[t.name]
			if up == nil {
				if st.held[t.name] == nil {
					current = append(current, t.name)
				}
				continue
			}
			detail := fmt.Sprintf("%s %s → %s%s", t.main, up.From, up.To, fromOrigin(up.Origin))
			if n := len(up.Packages); n > 1 {
				detail += fmt.Sprintf(" (%d packages)", n)
			}
			cmd := "mp stack upgrade " + t.component
			switch {
			case t.component == osprofile.UpgradeDB:
				detail += ": " + cmd + " (the database server restarts)"
			case auto.Off:
				detail += ": " + cmd + " (the daily upgrade is off: mp stack auto-upgrade --on)"
			default:
				detail += fmt.Sprintf(": %s, or the daily upgrade at %02d:00", cmd, auto.hour())
			}
			c := check("update "+t.name, "warn", detail)
			c.Action, c.Target = "stack.upgrade", t.component
			out = append(out, c)
		}
		if len(out) == 0 && len(current) > 0 {
			out = append(out, check("updates", "ok", strings.Join(current, ", ")+": nothing newer in their repositories (as the package index last saw them)"))
		}
	}
	if last := s.lastUpgrade(ctx); last != nil && last.Error != "" {
		out = append(out, check("stack upgrade", "warn", fmt.Sprintf("the upgrade of %s (job %d) failed: %s; nothing was rolled back (mp job show %d)", last.At.In(time.Local).Format("2006-01-02 15:04"), last.JobID, last.Error, last.JobID)))
	}
	return out
}

// catchWorkersCheck looks at every pool of the installed branches for
// catch_workers_output: with it a pool's workers write into the php-fpm
// master, which runs as root, and CVE-2026-129291 turns that into root for a
// site's code (PHP 8.2.36, 8.3.37, 8.4.28 and 8.5.13 fix it). The pool
// template up to 0.8.19 had it on; a site's pool loses it when the site is
// applied again. The upgrade's move of the logs does that for every site an
// older panel set up — not all at the first start: one whose apply fails
// waits for its next try, and the doctor's "site logs" line names those
// still waiting — and mp site apply <domain> does it for one at once.
func (s *Server) catchWorkersCheck(ctx context.Context) *apitypes.Check {
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return nil
	}
	// unread are the directories and files the check could not read: a pool
	// among them may have it on, so they make no "ok".
	var hits, unread []string
	branches := 0
	for _, v := range versions {
		l := phpLayout(s, v.Version)
		if l == nil || v.Status != store.PHPInstalled {
			continue
		}
		branches++
		dir, err := s.agent.ListDir(ctx, l.PoolDir)
		if err != nil {
			unread = append(unread, l.PoolDir+" ("+err.Error()+")")
			continue
		}
		for _, e := range dir.Entries {
			if e.IsDir || !strings.HasSuffix(e.Name, ".conf") {
				continue
			}
			p := path.Join(l.PoolDir, e.Name)
			res, err := s.agent.ReadFile(ctx, p, 1<<20)
			switch {
			case err != nil:
				unread = append(unread, p+" ("+err.Error()+")")
			case res.Truncated:
				// only its tail came back
				unread = append(unread, p+" (larger than 1 MB)")
			case len(catchingPools(res.Content)) > 0:
				hits = append(hits, p)
			}
		}
	}
	switch {
	case branches == 0:
		return nil
	case len(hits) == 0 && len(unread) == 0:
		c := check("php-fpm pools", "ok", "no pool has catch_workers_output on")
		return &c
	case len(hits) == 0:
		c := check("php-fpm pools", "warn", fmt.Sprintf("cannot read %s: a pool there with catch_workers_output on would let its workers get root through the php-fpm master (CVE-2026-129291)", strings.Join(unread, ", ")))
		return &c
	}
	shown := hits
	more := ""
	if len(shown) > 5 {
		shown, more = shown[:5], fmt.Sprintf(" and %d more", len(hits)-5)
	}
	if len(unread) > 0 {
		more += "; cannot read " + strings.Join(unread, ", ")
	}
	c := check("php-fpm pools", "warn", fmt.Sprintf("catch_workers_output = yes in %d pool file(s): %s%s. A worker of such a pool can get root through the php-fpm master (CVE-2026-129291; fixed in PHP 8.2.36, 8.3.37, 8.4.28 and 8.5.13: mp stack upgrade php). A site's pool loses it when the site is applied again: the upgrade's move of the logs does that for every site an older panel set up, and the doctor's site logs line names those still waiting; for one that still has it: mp site apply <domain>; a pool of your own: remove the line",
		len(hits), strings.Join(shown, ", "), more))
	return &c
}

// catchingPools are the pools of a pool file that switch
// catch_workers_output on, as php-fpm reads its ini: ; and # start a
// comment, yes, on, true and 1 are on.
func catchingPools(content string) []string {
	pool := ""
	var out []string
	for _, line := range strings.Split(content, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || l[0] == ';' || l[0] == '#' {
			continue
		}
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			pool = strings.TrimSpace(l[1 : len(l)-1])
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.TrimSpace(k) != "catch_workers_output" {
			continue
		}
		if i := strings.IndexByte(v, ';'); i >= 0 {
			v = v[:i]
		}
		switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`)) {
		case "yes", "on", "true", "1":
			out = append(out, pool)
		}
	}
	return out
}

// kernelSources are the files the kernel modules check reads.
type kernelSources struct {
	// rules are the places of the panel's rule, the one that wins first: a
	// file of the same name in /etc replaces the package's.
	rules []string
	// modules lists the loaded modules (/proc/modules), builtin the ones
	// built into the running kernel (its modules.builtin).
	modules string
	builtin func() string
}

// kernelSourcesOf is the host's unless SetKernelSources said otherwise.
func (s *Server) kernelSourcesOf() kernelSources {
	if s.kernel.modules != "" {
		return s.kernel
	}
	return kernelSources{
		rules:   []string{osprofile.ModprobeLocalFile, osprofile.ModprobePackageFile},
		modules: "/proc/modules",
		builtin: func() string {
			var u unix.Utsname
			if err := unix.Uname(&u); err != nil {
				return ""
			}
			return filepath.Join("/lib/modules", unix.ByteSliceToString(u.Release[:]), "modules.builtin")
		},
	}
}

// SetKernelSources points the kernel modules check at other files: the
// rules in the order they win, the loaded modules, the built-in ones.
// Intended for tests and the demo, whose server is a pretend one.
func (s *Server) SetKernelSources(rules []string, modules, builtin string) {
	s.kernel = kernelSources{rules: rules, modules: modules, builtin: func() string { return builtin }}
}

// kernelModulesCheck: the modules hosting does not need (sctp, dccp, rds,
// tipc) must not be loadable by a local account — SCTP's local root of
// November 2026 has no kernel fix. The package ships the modprobe rule, mp
// setup writes it on an install without the package; the rule keeps a
// module from loading but does not unload one that is loaded already, and
// cannot stop one built into the kernel. A module the administrator's
// /etc/modprobe.d/monopanel.conf lets load again is their choice. In a
// container (the agent says which) none of this is the panel's: an
// account's socket(AF_INET, SOCK_STREAM, IPPROTO_SCTP) has the host's
// kernel run the host's modprobe with the host's configuration, the
// container's file changes nothing, and its /proc/modules lists the host's
// modules, which nobody in the container can unload.
func (s *Server) kernelModulesCheck(container string) apitypes.Check {
	if container != "" {
		return check("kernel modules", "warn", fmt.Sprintf("the panel runs in a container (%s): the host's kernel loads the modules with the host's modprobe configuration, which %s in the container does not change. Any account here can have the host load %s unless the host has install <module> /bin/false for them in its /etc/modprobe.d (SCTP had a local root with no kernel fix in November 2026)",
			container, osprofile.ModprobePackageFile, strings.Join(osprofile.BlockedModules, ", ")))
	}
	src := s.kernelSourcesOf()
	rule, blocked, err := modprobeRule(src.rules)
	if err != nil {
		return check("kernel modules", "warn", err.Error())
	}
	if rule == "" {
		return check("kernel modules", "warn", fmt.Sprintf("neither %s nor %s is there: any local account can have the kernel load %s (SCTP had a local root with no kernel fix in November 2026); mp setup writes the rule",
			osprofile.ModprobePackageFile, osprofile.ModprobeLocalFile, strings.Join(osprofile.BlockedModules, ", ")))
	}
	loaded := firstFields(src.modules)
	builtin := map[string]bool{}
	if f := src.builtin(); f != "" {
		for p := range firstFields(f) {
			builtin[strings.ReplaceAll(strings.TrimSuffix(path.Base(p), ".ko"), "-", "_")] = true
		}
	}
	var problems, kept, allowed []string
	for _, m := range osprofile.BlockedModules {
		switch {
		case !blocked[m]:
			allowed = append(allowed, m)
		case builtin[m]:
			problems = append(problems, m+" is built into the kernel, which no modprobe rule stops")
		case loaded[m]:
			problems = append(problems, fmt.Sprintf("%s is loaded: the rule keeps it from loading again but does not unload it (rmmod %s, or reboot)", m, m))
		default:
			kept = append(kept, m)
		}
	}
	if len(problems) > 0 {
		return check("kernel modules", "warn", strings.Join(problems, "; "))
	}
	detail := strings.Join(kept, ", ") + " cannot load (" + rule + ")"
	if len(allowed) > 0 {
		detail += "; " + strings.Join(allowed, ", ") + " may: " + rule + " allows them"
	}
	return check("kernel modules", "ok", detail)
}

// modprobeRule is the panel's rule in effect, and the modules it keeps from
// loading ("install <module> /bin/false" or /bin/true: a command other than
// the module's load).
func modprobeRule(rules []string) (string, map[string]bool, error) {
	for _, f := range rules {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return f, nil, fmt.Errorf("cannot read %s: %v", f, err)
		}
		blocked := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			fs := strings.Fields(line)
			if len(fs) >= 3 && fs[0] == "install" {
				switch fs[2] {
				case "/bin/false", "/bin/true", "/usr/bin/false", "/usr/bin/true", "false", "true":
					blocked[strings.ReplaceAll(fs[1], "-", "_")] = true
				}
			}
		}
		return f, blocked, nil
	}
	return "", nil, nil
}

// firstFields is the set of the first words of a file's lines.
func firstFields(file string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(file)
	if err != nil {
		return out
	}
	defer f.Close() //nolint:errcheck // read-only
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fs := strings.Fields(sc.Text()); len(fs) > 0 {
			out[fs[0]] = true
		}
	}
	return out
}
