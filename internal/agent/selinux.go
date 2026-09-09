package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// restorecon gives paths the SELinux label the policy expects. Files the
// agent creates inherit the parent's label or the agent's own, which is not
// what a confined nginx or php-fpm may open; on hosts without SELinux (or
// without the tool) nothing happens. Failures are logged, never fatal: the
// operation itself succeeded.
func (s *Server) restorecon(ctx context.Context, paths ...string) {
	if len(paths) == 0 || s.profile.MAC() != "selinux" {
		return
	}
	bin := "/usr/sbin/restorecon"
	if _, err := os.Stat(bin); err != nil {
		return
	}
	// A missing path is not an error here: Restore may name a pid file the
	// validator did not create this time.
	existing := paths[:0:0]
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-F"}, existing...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		s.log.Warn("restorecon failed", "paths", strings.Join(existing, " "), "err", err, "output", strings.TrimSpace(string(out)))
	}
}
