package api

import (
	"context"
	"fmt"
	"monopanel/internal/apitypes"
	"os"
	"regexp"
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

// selinuxEnforcePath tells the SELinux mode: "1" enforcing, "0" permissive;
// absent when SELinux is off or the kernel has none. Tests point it elsewhere.
var selinuxEnforcePath = "/sys/fs/selinux/enforce"

// relabel gives a client's tree the labels the policy expects. Files copied
// with cp -a or rsync -X from /root keep admin_home_t, and a confined nginx
// answers 403 for them — the classic EL support ticket. Nothing happens
// without SELinux; a failure is logged, the operation itself succeeded.
func (s *Server) relabel(ctx context.Context, jc *jobs.Context, dir string, recursive bool) {
	if s.profile.MAC() != "selinux" {
		return
	}
	args := []string{"-F"}
	if recursive {
		args = append(args, "-R")
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "restorecon", Args: append(args, dir), TimeoutSeconds: 600})
	switch {
	case err != nil:
		s.log.Warn("restorecon failed", "path", dir, "err", err)
	case res.ExitCode != 0:
		s.log.Warn("restorecon failed", "path", dir, "output", strings.TrimSpace(res.Output))
	case jc != nil:
		jc.Logf("SELinux labels restored under %s", dir)
	}
}

// avcRe picks the pieces of a raw audit record the panel reports: the
// process, the target (nginx records often carry only name=, not path=),
// the object class and the label the target had.
var avcRe = regexp.MustCompile(`comm="([^"]*)"|(?:path|name)="([^"]*)"|tclass=(\S+)|tcontext=\w+:\w+:(\w+):`)

// selinuxCheck is the doctor's SELinux line: the mode, and today's denials
// for the web domain (nginx and php-fpm both run as httpd_t) with the site
// the last one points at, so a 403 caused by a label is told apart from one
// caused by permissions. Nil where SELinux is not in the picture.
func (s *Server) selinuxCheck(ctx context.Context) *apitypes.Check {
	raw, err := os.ReadFile(selinuxEnforcePath)
	if err != nil {
		return nil
	}
	mode := "permissive"
	if strings.TrimSpace(string(raw)) == "1" {
		mode = "enforcing"
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "ausearch", Args: []string{"-m", "AVC", "-ts", "today", "--raw"}, TimeoutSeconds: 30})
	if err != nil || (res.ExitCode != 0 && !strings.Contains(res.Output, "no matches")) {
		c := check("selinux", "ok", mode+"; audit log not readable, denials unknown")
		return &c
	}
	denials, last := 0, ""
	for _, line := range strings.Split(res.Output, "\n") {
		if !strings.Contains(line, "avc:  denied") || !strings.Contains(line, ":httpd_t:") {
			continue
		}
		denials++
		comm, target, class, label := "", "", "", ""
		for _, m := range avcRe.FindAllStringSubmatch(line, -1) {
			switch {
			case m[1] != "":
				comm = m[1]
			case m[2] != "":
				target = m[2]
			case m[3] != "":
				class = m[3]
			case m[4] != "":
				label = m[4]
			}
		}
		last = fmt.Sprintf("%s → %s (%s, %s)", comm, target, class, label)
		switch rel, ok := strings.CutPrefix(target, strings.TrimSuffix(s.cfg.WWWRoot, "/")+"/"); {
		case ok:
			// /var/www/<login>/data/www/<domain>/...: the site to fix
			if parts := strings.SplitN(rel, "/", 5); len(parts) >= 4 && parts[1] == "data" && parts[2] == "www" {
				last += "; mp site fix " + parts[3]
			}
		case strings.HasSuffix(label, "_home_t"):
			// copied from /root or a home directory with cp -a / rsync -X
			last += "; a file copied from a home directory — mp site fix <domain>"
		}
	}
	if denials == 0 {
		c := check("selinux", "ok", mode+", no denials for the web server today")
		return &c
	}
	c := check("selinux", "warn", fmt.Sprintf("%s; %d denial(s) for the web server today, last: %s", mode, denials, last))
	return &c
}
