package api

import (
	"context"
	"fmt"
	"strings"

	"monopanel/internal/agent"
)

// firewalldOpen opens ports ("80/tcp") in firewalld when it is running. Some
// EL images (Oracle Linux) ship it enabled with only ssh allowed, and a panel
// nobody can reach is no panel. Without firewalld nothing happens; once the
// panel's own nftables firewall takes over, firewalld is gone (see
// firewallEnable). Failures are logged, not fatal: the service itself works.
func (s *Server) firewalldOpen(ctx context.Context, logf func(string, ...any), ports ...string) {
	if !s.firewalldRunning(ctx) {
		return
	}
	args := []string{"-q", "--permanent"}
	for _, p := range ports {
		args = append(args, "--add-port="+p)
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "firewall-cmd", Args: args})
	if err == nil && res.ExitCode == 0 {
		res, err = s.agent.Tool(ctx, &agent.ToolRequest{Name: "firewall-cmd", Args: []string{"-q", "--reload"}})
	}
	switch {
	case err != nil:
		logf("firewalld: cannot open %s: %v", strings.Join(ports, " "), err)
	case res.ExitCode != 0:
		logf("firewalld: cannot open %s: %s", strings.Join(ports, " "), strings.TrimSpace(res.Output))
	default:
		logf("firewalld: opened %s", strings.Join(ports, " "))
	}
}

// firewalldRunning reports whether firewalld is installed and active.
func (s *Server) firewalldRunning(ctx context.Context) bool {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "firewall-cmd", Args: []string{"--state"}})
	return err == nil && res.ExitCode == 0 && strings.Contains(res.Output, "running")
}

// firewalldRetire stops and disables firewalld before the panel's nftables
// table takes over: two rule sets on one host only confuse each other.
//
// The EL fail2ban unit is PartOf=firewalld.service, so stopping firewalld
// stops fail2ban too — and fail2ban lifts every ban on the way out. It
// reports whether fail2ban was running then; the caller starts it again once
// the panel's table is loaded (ozarign.kz, 13.10.2026: fail2ban stayed dead
// after an unban that enabled the firewall).
func (s *Server) firewalldRetire(ctx context.Context) (fail2banStopped bool, err error) {
	if !s.firewalldRunning(ctx) {
		return false, nil
	}
	if st, err := s.agent.Service(ctx, fail2banService, "status"); err == nil && st.Status.ActiveState == "active" {
		fail2banStopped = true
	}
	for _, action := range []string{"stop", "disable"} {
		if _, err := s.agent.Service(ctx, "firewalld.service", action); err != nil {
			return fail2banStopped, fmt.Errorf("firewalld %s: %w", action, err)
		}
	}
	return fail2banStopped, nil
}
