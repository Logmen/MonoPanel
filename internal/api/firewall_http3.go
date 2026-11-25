package api

import (
	"context"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
)

// settingHTTP3Port records where the panel opened UDP 443 for HTTP/3: "nft"
// (its own table, applyFirewall), "firewalld", or "" — what it may close
// again once no site serves HTTP/3. A UDP 443 somebody else opened stays.
const settingHTTP3Port = "firewall.http3_port"

// firewallHTTP3 keeps the host's firewall in step with HTTP/3. A site's
// http3 puts QUIC sockets and an Alt-Svc header in nginx, and browsers try
// UDP 443 — closed, it is HTTP/3 on paper only. Once a site serves it the
// port opens in the panel's nftables table (applyFirewall opens it while
// anySiteHTTP3), else in a running firewalld; once none does, the panel
// closes what it opened. Without HTTP/3 and nothing opened it costs no call
// to the agent. Failures are warnings: browsers fall back to HTTP/2 over TCP.
func (s *Server) firewallHTTP3(ctx context.Context, logf func(string, ...any)) {
	want := s.anySiteHTTP3(ctx)
	opened, _ := s.db.GetSetting(ctx, settingHTTP3Port)
	if !want && opened == "" {
		return
	}
	if v, _ := s.db.GetSetting(ctx, settingFirewall); v == "yes" {
		if want == s.nftHasHTTP3(ctx) {
			return
		}
		if _, err := s.applyFirewall(ctx); err != nil {
			logf("warning: firewall: udp/443 for HTTP/3: %v", err)
		} else if want {
			logf("firewall: udp/443 opened for HTTP/3")
		} else {
			logf("firewall: udp/443 closed, no site serves HTTP/3")
		}
		return
	}
	switch {
	case want:
		if !s.firewalldRunning(ctx) || s.firewalldHasPort(ctx, "443/udp") {
			return
		}
		if _, err := s.firewalldAdd(ctx, "443/udp"); err != nil {
			logf("warning: HTTP/3 is on, but udp/443 is closed in firewalld (%v): browsers fall back to HTTP/2", err)
			return
		}
		s.db.SetSetting(ctx, settingHTTP3Port, "firewalld") //nolint:errcheck // a port left open is harmless
		logf("firewalld: udp/443 opened for HTTP/3")
	case opened == "firewalld":
		if s.firewalldRunning(ctx) && s.firewalldHasPort(ctx, "443/udp") {
			if err := s.firewalldCloseHTTP3(ctx); err != nil {
				logf("warning: firewalld: udp/443 stays open: %v", err)
				return
			}
			logf("firewalld: udp/443 closed, no site serves HTTP/3")
		}
		s.db.SetSetting(ctx, settingHTTP3Port, "") //nolint:errcheck // a port left open is harmless
	default:
		// "nft" with the panel's table switched off since: nothing to close.
		s.db.SetSetting(ctx, settingHTTP3Port, "") //nolint:errcheck // a port left open is harmless
	}
}

// nftHasHTTP3 says whether the panel's loaded table lets UDP 443 in.
func (s *Server) nftHasHTTP3(ctx context.Context) bool {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "nft", Args: []string{"list", "table", "inet", "monopanel"}})
	return err == nil && res.ExitCode == 0 && strings.Contains(res.Output, "udp dport 443 accept")
}

// http3Check is the doctor's "http3" line while a site serves HTTP/3: the
// firewall the panel can see lets UDP 443 in. A provider's or another
// network's firewall is beyond it — the note says so.
func (s *Server) http3Check(ctx context.Context) *apitypes.Check {
	if !s.anySiteHTTP3(ctx) {
		return nil
	}
	const name = "http3"
	const fix = "mp site apply <a site with HTTP/3> opens it"
	if v, _ := s.db.GetSetting(ctx, settingFirewall); v == "yes" {
		if !s.nftHasHTTP3(ctx) {
			c := check(name, "warn", "HTTP/3 is on, but the panel's firewall does not let udp/443 in: browsers fall back to HTTP/2; "+fix)
			return &c
		}
		c := check(name, "ok", "udp/443 open in the panel's firewall; it must be open in front of the server too")
		return &c
	}
	if s.firewalldRunning(ctx) {
		if !s.firewalldHasPort(ctx, "443/udp") {
			c := check(name, "warn", "HTTP/3 is on, but udp/443 is closed in firewalld: browsers fall back to HTTP/2; "+fix)
			return &c
		}
		c := check(name, "ok", "udp/443 open in firewalld; it must be open in front of the server too")
		return &c
	}
	c := check(name, "ok", "no host firewall in the way; udp/443 must be open in front of the server too")
	return &c
}
