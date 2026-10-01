package api

import (
	"context"
	"fmt"
	"strings"

	"monopanel/internal/apitypes"
	"monopanel/internal/osprofile"
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
