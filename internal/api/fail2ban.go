package api

import (
	"context"
	"encoding/json"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
)

const (
	// settingFail2banTrusted is the JSON list of addresses fail2ban never bans.
	settingFail2banTrusted = "fail2ban.trusted"
	fail2banJailPath       = "/etc/fail2ban/jail.d/monopanel.local"
	fail2banFilterPath     = "/etc/fail2ban/filter.d/monopanel.conf"
	fail2banService        = "fail2ban.service"
)

// fail2banPackages is what to install and the package whose version says
// fail2ban is there. On EL the fail2ban meta package also brings
// fail2ban-sendmail, which needs some /usr/sbin/sendmail — dnf picks exim on a
// host without postfix — and fail2ban-firewalld with firewalld itself. The
// panel bans through nftables and sends no mail, so the server alone is
// installed there.
func (s *Server) fail2banPackages() (install []string, version string) {
	if s.profile.Family() == osprofile.FamilyDebian {
		return []string{"fail2ban", "python3-systemd"}, "fail2ban"
	}
	return []string{"fail2ban-server", "python3-systemd"}, "fail2ban-server"
}

// fail2banTrusted lists the addresses in ignoreip besides loopback.
func (s *Server) fail2banTrusted(ctx context.Context) []string {
	out := []string{}
	if raw, err := s.db.GetSetting(ctx, settingFail2banTrusted); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (s *Server) setFail2banTrusted(ctx context.Context, list []string) error {
	sort.Strings(list)
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.db.SetSetting(ctx, settingFail2banTrusted, string(raw))
}

// normalizeSource writes an address or a network the canonical way, so that
// the same one is not kept twice.
func normalizeSource(src string) (string, bool) {
	src = strings.TrimSpace(src)
	if ip := net.ParseIP(src); ip != nil {
		return ip.String(), true
	}
	if _, n, err := net.ParseCIDR(src); err == nil {
		return n.String(), true
	}
	return "", false
}

// fail2banSiteLogs lists the nginx logs of every site, and of the webmail
// published on a port. The files exist: a site's logs are made before its
// configuration is applied.
func (s *Server) fail2banSiteLogs(ctx context.Context) (access, errs []string) {
	sites, err := s.db.ListSites(ctx, 0)
	if err != nil {
		return nil, nil
	}
	for _, site := range sites {
		owner, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			continue
		}
		logs := path.Join(s.layoutFor(site, owner).data, "logs")
		access = append(access, path.Join(logs, site.Domain+".access.log"))
		errs = append(errs, path.Join(logs, site.Domain+".error.log"))
		if c := s.loadMailConfig(ctx); c.Webmail == site.Domain && c.WebmailPort > 0 {
			access = append(access, path.Join(logs, "webmail.access.log"))
			errs = append(errs, path.Join(logs, "webmail.error.log"))
		}
	}
	sort.Strings(access)
	sort.Strings(errs)
	return access, errs
}

// applyFail2ban writes the jails from the panel's state: the sites' logs and
// the trusted addresses. fail2ban is restarted only when the file changed
// (restart forces it: the package has just been installed).
func (s *Server) applyFail2ban(ctx context.Context, restart bool) error {
	nginx := false
	if q, err := s.agent.Pkg(ctx, "query", "nginx"); err == nil {
		_, nginx = q.Installed["nginx"]
	}
	access, errs := s.fail2banSiteLogs(ctx)
	jail, err := s.render.Render("fail2ban/jail.local.tmpl", render.Fail2ban{
		BanTime: "1h", FindTime: "10m", MaxRetry: 5, PanelPort: strconv.Itoa(s.panelPort()), Nginx: nginx,
		IgnoreIPs: s.fail2banTrusted(ctx), AccessLogs: access, ErrorLogs: errs,
	})
	if err != nil {
		return err
	}
	filter, _, err := s.render.Source("fail2ban/filter-monopanel.conf")
	if err != nil {
		return err
	}
	req := &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{
		{Path: fail2banJailPath, Content: jail, Mode: 0o644},
		{Path: fail2banFilterPath, Content: string(filter), Mode: 0o644},
		// A restart, not a reload: the unit reloads through fail2ban-client,
		// which SELinux confines to its own domain (fail2ban_client_t) — it
		// cannot look into the accounts' directories, finds no site logs and
		// drops them from the jails without a word. The server reads the
		// configuration itself at start and may. Bans survive the restart:
		// fail2ban keeps them in its database.
	}, Validate: [][]string{{"/usr/bin/fail2ban-client", "-t"}}, Restart: []string{fail2banService}, Force: restart, Origin: "fail2ban"}
	_, err = s.agent.ApplyConfigSet(ctx, req)
	return err
}

// refreshFail2ban brings the jails up to date after the sites changed, and at
// startup on a host that got its jails from an older panel. Without fail2ban
// it does nothing.
func (s *Server) refreshFail2ban(ctx context.Context) {
	if v, _ := s.db.GetSetting(ctx, settingFail2ban); v != "installed" {
		return
	}
	if err := s.applyFail2ban(ctx, false); err != nil {
		s.log.Warn("fail2ban jails", "err", err)
	}
}
