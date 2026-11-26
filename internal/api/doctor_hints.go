package api

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"monopanel/internal/apitypes"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// What an earlier version of the panel left on a host and a newer one does
// not undo by itself: the panel neither switches repositories off nor removes
// packages on its own, so the doctor names the leftovers and the commands.

// crbEnabledMark is the line panel 0.8.15 wrote into the mail.install log
// when it switched CodeReady Builder on for good.
const crbEnabledMark = "enabled: opendkim needs libmilter and libmemcached from it"

// crbLeftoverCheck warns when CodeReady Builder is still enabled after the
// mail install of 0.8.15 switched it on. An administrator who enabled it on
// purpose sees nothing: only the panel's own step is recognised, by its log.
func (s *Server) crbLeftoverCheck(ctx context.Context) *apitypes.Check {
	if s.profile.Family() == osprofile.FamilyDebian || !s.db.JobLogContains(ctx, crbEnabledMark) {
		return nil
	}
	id, enabled := s.crbRepo(ctx, true)
	if id == "" || !enabled {
		return nil
	}
	c := check("repositories", "warn", fmt.Sprintf("%s was switched on by the mail install of panel 0.8.15 and is still enabled: nothing needs it enabled, and a plain dnf upgrade pulls other packages from it. To switch it off: dnf config-manager --set-disabled %s", id, id))
	return &c
}

// fail2banLeftoverCheck warns about the fail2ban meta package on EL: it keeps
// fail2ban-sendmail (with whatever mail server dnf picked for it) and
// fail2ban-firewalld (with firewalld) installed, none of which the panel
// uses. fail2ban-server is their dependency, so it is marked first — a plain
// remove would take it along.
func (s *Server) fail2banLeftoverCheck(ctx context.Context) *apitypes.Check {
	if s.profile.Family() == osprofile.FamilyDebian {
		return nil
	}
	if v, _ := s.db.GetSetting(ctx, settingFail2ban); v != "installed" {
		return nil
	}
	q, err := s.agent.Pkg(ctx, "query", "fail2ban", "fail2ban-sendmail", "fail2ban-firewalld", "exim")
	if err != nil {
		return nil
	}
	var extra []string
	for _, p := range []string{"fail2ban", "fail2ban-sendmail", "fail2ban-firewalld"} {
		if q.Installed[p] != "" {
			extra = append(extra, p)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	with := ""
	if q.Installed["exim"] != "" {
		with = " (exim came with fail2ban-sendmail and goes with it if nothing else needs it)"
	}
	c := check("fail2ban packages", "warn", fmt.Sprintf("the fail2ban meta package is installed: %s%s. The panel needs fail2ban-server alone. To drop the rest: dnf mark install fail2ban-server fail2ban-selinux && dnf remove %s", strings.Join(extra, ", "), with, strings.Join(extra, " ")))
	return &c
}

// phpCLICheck compares what PHP from the command line reads — and with it
// every cron job — with what the panel writes: a mismatch means cron runs
// with other values than the sites, the way a timezone set with
// mp php ini set once reached the pools and left cron in UTC.
func (s *Server) phpCLICheck(ctx context.Context) *apitypes.Check {
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return nil
	}
	var stale, waiting []string
	checked := 0
	for _, v := range versions {
		l := phpLayout(s, v.Version)
		if v.Status != store.PHPInstalled || l == nil {
			continue
		}
		files, err := s.phpIniFiles(ctx, l, true)
		if err != nil || len(files) == 0 {
			continue
		}
		cur, err := s.agent.ReadFile(ctx, files[0].Path, 64<<10)
		if err != nil {
			continue
		}
		checked++
		want, have := iniValues(files[0].Content), iniValues(cur.Content)
		// Remi's php.d is FPM's too: there a change of short_open_tag waits
		// for a site apply while the branch has a site (refreshPHPInis), and
		// neither the panel's start nor mp php ini set makes it.
		if len(l.IniDirs) == 1 && have["short_open_tag"] != "" && iniOn(have["short_open_tag"]) != iniOn(want["short_open_tag"]) && s.branchRunsPools(ctx, v.Version) {
			waiting = append(waiting, v.Version)
		}
		var diff []string
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if have[k] != want[k] {
				was := have[k]
				if was == "" {
					was = "—"
				}
				diff = append(diff, fmt.Sprintf("%s %s instead of %s", k, was, want[k]))
			}
		}
		if len(diff) > 3 {
			diff = append(diff[:3], fmt.Sprintf("%d more", len(diff)-3))
		}
		if len(diff) > 0 {
			stale = append(stale, fmt.Sprintf("PHP %s (%s): %s", v.Version, files[0].Path, strings.Join(diff, ", ")))
		}
	}
	if checked == 0 {
		return nil
	}
	if len(stale) > 0 {
		hint := ". The panel rewrites the file at its start and on mp php ini set; mp site apply <domain> of a site on that branch does it too"
		if len(waiting) > 0 {
			hint += fmt.Sprintf(". PHP %s: FPM reads the same file, so short_open_tag there changes only with mp site apply <domain> of a site on the branch", strings.Join(waiting, ", "))
		}
		c := check("php cli", "warn", "the command line and cron read other values than the sites: "+strings.Join(stale, "; ")+hint)
		return &c
	}
	c := check("php cli", "ok", "the command line and cron read the sites' server-wide values")
	return &c
}

// iniValues reads the key = value lines of an ini file; the last one wins,
// as in PHP.
func iniValues(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}
