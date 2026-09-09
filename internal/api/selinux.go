package api

import (
	"context"
	"fmt"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/jobs"
)

const settingSELinux = "selinux.hosting"

// selinuxFileContexts are the labels the hosting layout needs beyond what
// the targeted policy already knows (/var/www is httpd_sys_content_t by
// default): php-fpm sockets that nginx connects to, and per-site logs that
// nginx writes.
var selinuxFileContexts = [][2]string{
	{"/run/monopanel(/.*)?", "httpd_var_run_t"},
	{"/var/www/[^/]+/data/logs(/.*)?", "httpd_log_t"},
}

// selinuxBooleans is the usual shared-hosting set: PHP writes into the
// document root, connects to MySQL and to the network, sends mail.
var selinuxBooleans = []string{
	"httpd_unified=1", "httpd_can_network_connect=1", "httpd_can_network_connect_db=1",
	"httpd_can_sendmail=1", "httpd_execmem=1", "httpd_setrlimit=1",
}

// selinuxHostingPolicy prepares an EL host once: the semanage tools, the
// file contexts above, the booleans, and a relabel of what exists already.
// Without SELinux it returns at once. Later directories get their label
// from the agent (restorecon after ensureDirs) and from these rules.
func (s *Server) selinuxHostingPolicy(ctx context.Context, jc *jobs.Context) error {
	if s.profile.MAC() != "selinux" {
		return nil
	}
	if v, _ := s.db.GetSetting(ctx, settingSELinux); v == "ready" {
		return nil
	}
	jc.Progress(3, "SELinux policy for hosting")
	if _, err := s.agent.Pkg(ctx, "install", "policycoreutils-python-utils"); err != nil {
		return fmt.Errorf("install semanage: %w", err)
	}
	for _, fc := range selinuxFileContexts {
		if err := s.selinuxFileContext(ctx, fc[0], fc[1]); err != nil {
			return err
		}
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "setsebool", Args: append([]string{"-P"}, selinuxBooleans...)})
	if err != nil {
		return fmt.Errorf("setsebool: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("setsebool: %s", strings.TrimSpace(res.Output))
	}
	// Relabel what is there already; a missing path is not an error.
	if _, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "restorecon", Args: []string{"-RF", s.cfg.WWWRoot, s.cfg.RunDir, "/etc/nginx", "/var/log/nginx"}}); err != nil {
		return fmt.Errorf("restorecon: %w", err)
	}
	jc.Logf("selinux: file contexts for %s and per-site logs, booleans %s", s.cfg.RunDir, strings.Join(selinuxBooleans, " "))
	return s.db.SetSetting(ctx, settingSELinux, "ready")
}

// selinuxFileContext adds one fcontext rule, or updates it when it exists.
// Paths under an equivalence (EL9 maps /run to /var/run, EL10 the other way
// round) are refused with a hint naming the spelling semanage wants; the
// hint is followed once.
func (s *Server) selinuxFileContext(ctx context.Context, spec, label string) error {
	for attempt := 0; attempt < 2; attempt++ {
		res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "semanage", Args: []string{"fcontext", "-a", "-t", label, spec}})
		if err != nil {
			return fmt.Errorf("semanage fcontext %s: %w", spec, err)
		}
		if res.ExitCode != 0 && strings.Contains(res.Output, "already defined") {
			if res, err = s.agent.Tool(ctx, &agent.ToolRequest{Name: "semanage", Args: []string{"fcontext", "-m", "-t", label, spec}}); err != nil {
				return fmt.Errorf("semanage fcontext %s: %w", spec, err)
			}
		}
		if res.ExitCode == 0 {
			return nil
		}
		_, hint, ok := strings.Cut(res.Output, "Try adding '")
		if alt, _, found := strings.Cut(hint, "'"); ok && found && attempt == 0 && alt != spec {
			spec = alt
			continue
		}
		return fmt.Errorf("semanage fcontext %s: %s", spec, strings.TrimSpace(res.Output))
	}
	return nil
}
