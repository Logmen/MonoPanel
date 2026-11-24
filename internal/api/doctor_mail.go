package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// mailUnits are the mail server's services, which the doctor's "service"
// lines cover while it is installed: postfix, dovecot and, with DKIM on,
// opendkim — the ones mp mail status shows.
func mailUnits(c mailConfig) []string {
	if !c.Installed {
		return nil
	}
	units := []string{postfixService, dovecotService}
	if c.DKIM {
		units = append(units, dkimService)
	}
	return units
}

// mailDoctorChecks are the doctor's "mail" and "webmail" lines while the mail
// server is installed. Before them the doctor said nothing of the mail server
// but its policy: a postfix that no longer listened on 587, a self-signed
// certificate or a webmail whose port did not answer showed in mp mail status
// alone.
func (s *Server) mailDoctorChecks(ctx context.Context) []apitypes.Check {
	c := s.loadMailConfig(ctx)
	if !c.Installed {
		return nil
	}
	ports := mailPortsFor(c)
	if c.Webmail != "" && c.WebmailPort > 0 {
		ports = append(ports, c.WebmailPort)
	}
	probes := s.probePorts(ports)
	out := []apitypes.Check{s.mailCheck(ctx, c, probes)}
	if c.Webmail != "" {
		out = append(out, s.webmailCheck(ctx, c, probes))
	}
	return out
}

// mailCheck: every port the settings publish listens, and the daemons
// present a certificate clients accept.
func (s *Server) mailCheck(ctx context.Context, c mailConfig, probes map[int]probeResult) apitypes.Check {
	const name = "mail"
	if missing := missingMailPorts(c, probes); len(missing) > 0 {
		return check(name, "fail", "ports not listening: "+strings.Join(missing, ", ")+"; journalctl -u postfix -u dovecot, then mp mail apply")
	}
	_, _, kind, _ := s.mailTLS(ctx, c)
	switch kind {
	case store.CertKindSelfSigned:
		return check(name, "warn", fmt.Sprintf("%s presents a self-signed certificate: mail clients complain about it; mp ssl issue %s", c.Hostname, c.Hostname))
	case "none":
		return check(name, "fail", fmt.Sprintf("no certificate for %s: submission (587/465) is off; mp ssl issue %s", c.Hostname, c.Hostname))
	}
	return check(name, "ok", fmt.Sprintf("%s, %s certificate, %d ports listening", c.Hostname, kind, len(mailPortsFor(c))))
}

// webmailCheck: the site the settings name is there, its Roundcube is the
// one this panel ships, and its port, when it has one, answers.
func (s *Server) webmailCheck(ctx context.Context, c mailConfig, probes map[int]probeResult) apitypes.Check {
	const name = "webmail"
	site, err := s.db.GetSiteByDomain(ctx, c.Webmail)
	if errors.Is(err, store.ErrNotFound) {
		return check(name, "warn", "the mail settings name the site "+c.Webmail+", which is gone; mp mail apply takes it out of them")
	}
	if err != nil {
		return check(name, "fail", "site "+c.Webmail+": "+err.Error())
	}
	where := "https://" + site.Domain + "/"
	if c.WebmailPort > 0 {
		where = fmt.Sprintf("https://%s:%d/", c.Hostname, c.WebmailPort)
		if !probes[c.WebmailPort].Open {
			return check(name, "fail", fmt.Sprintf("%s does not answer (site %s); mp mail apply writes its server block again", where, site.Domain))
		}
	}
	if c.WebmailVersion != roundcubeVersion {
		owner := "<owner>"
		if u, err := s.db.GetUserByID(ctx, site.UserID); err == nil {
			owner = u.Login
		}
		return check(name, "warn", fmt.Sprintf("Roundcube %s at %s, this panel ships %s; mp mail webmail %s --user %s updates it", c.WebmailVersion, where, roundcubeVersion, site.Domain, owner))
	}
	return check(name, "ok", fmt.Sprintf("Roundcube %s at %s", roundcubeVersion, where))
}
