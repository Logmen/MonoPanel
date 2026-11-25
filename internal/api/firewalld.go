package api

import (
	"context"
	"errors"
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
	running, err := s.firewalldAdd(ctx, ports...)
	switch {
	case !running:
	case err != nil:
		logf("firewalld: cannot open %s: %v", strings.Join(ports, " "), err)
	default:
		logf("firewalld: opened %s", strings.Join(ports, " "))
	}
}

// firewalldAdd opens ports in firewalld for good and at once: a permanent
// rule (what a reload or a reboot brings back) and the same rule at runtime
// (what works now). No --reload: it drops every runtime-only rule, somebody
// else's included. A port open already is only a warning to firewall-cmd,
// exit code 0. running is false when there is no firewalld to talk to.
func (s *Server) firewalldAdd(ctx context.Context, ports ...string) (running bool, err error) {
	if !s.firewalldRunning(ctx) {
		return false, nil
	}
	add := make([]string, 0, len(ports))
	for _, p := range ports {
		add = append(add, "--add-port="+p)
	}
	if err := s.firewallCmd(ctx, append([]string{"-q", "--permanent"}, add...)...); err != nil {
		return true, err
	}
	return true, s.firewallCmd(ctx, append([]string{"-q"}, add...)...)
}

// firewalldPort says whether a port ("443/udp") is open in firewalld now
// (runtime) and for good (permanent). The two differ until a reload: an
// admin's --permanent alone, or a port opened at runtime only, which the
// next reload or reboot takes away.
func (s *Server) firewalldPort(ctx context.Context, port string) (runtime, permanent bool) {
	query := func(args ...string) bool {
		res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "firewall-cmd", Args: args})
		return err == nil && res.ExitCode == 0 // 0 yes, 1 no
	}
	return query("-q", "--query-port="+port), query("-q", "--permanent", "--query-port="+port)
}

// firewallCmd runs firewall-cmd; a non-zero exit is an error with its output.
func (s *Server) firewallCmd(ctx context.Context, args ...string) error {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "firewall-cmd", Args: args})
	switch {
	case err != nil:
		return err
	case res.ExitCode != 0:
		if out := strings.TrimSpace(res.Output); out != "" {
			return errors.New(out)
		}
		return fmt.Errorf("firewall-cmd %s: exit code %d", strings.Join(args, " "), res.ExitCode)
	}
	return nil
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
