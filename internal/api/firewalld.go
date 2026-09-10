package api

import (
	"context"
	"fmt"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/jobs"
)

// firewalldOpen opens ports ("80/tcp") in firewalld when it is running. Some
// EL images (Oracle Linux) ship it enabled with only ssh allowed, and a panel
// nobody can reach is no panel. Without firewalld nothing happens; once the
// panel's own nftables firewall takes over, firewalld is gone (see
// firewallEnable). Failures are logged, not fatal: the service itself works.
func (s *Server) firewalldOpen(ctx context.Context, jc *jobs.Context, ports ...string) {
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
		jc.Logf("firewalld: cannot open %s: %v", strings.Join(ports, " "), err)
	case res.ExitCode != 0:
		jc.Logf("firewalld: cannot open %s: %s", strings.Join(ports, " "), strings.TrimSpace(res.Output))
	default:
		jc.Logf("firewalld: opened %s", strings.Join(ports, " "))
	}
}

// firewalldRunning reports whether firewalld is installed and active.
func (s *Server) firewalldRunning(ctx context.Context) bool {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "firewall-cmd", Args: []string{"--state"}})
	return err == nil && res.ExitCode == 0 && strings.Contains(res.Output, "running")
}

// firewalldRetire stops and disables firewalld before the panel's nftables
// table takes over: two rule sets on one host only confuse each other.
func (s *Server) firewalldRetire(ctx context.Context) error {
	if !s.firewalldRunning(ctx) {
		return nil
	}
	for _, action := range []string{"stop", "disable"} {
		if _, err := s.agent.Service(ctx, "firewalld.service", action); err != nil {
			return fmt.Errorf("firewalld %s: %w", action, err)
		}
	}
	return nil
}
