package api

import (
	"context"
	"fmt"
	"github.com/danielgtaylor/huma/v2"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/jobs"
)

const settingSELinux = "selinux.hosting"

// selinuxFileContexts are the labels the hosting layout needs beyond what
// the targeted policy already knows (/var/www is httpd_sys_content_t by
// default): php-fpm sockets that nginx connects to, and per-site logs that
// nginx writes.
var selinuxFileContexts = [][2]string{
	// Everything under a site's docroot is web content, whatever the base
	// policy thinks of directories named logs or cgi-bin inside it: OpenCart
	// writes system/storage/logs/error.log, which the base rule
	// /var/www(/.*)?/logs(/.*)? turns into httpd_log_t that php-fpm may not
	// write, and restorecon would only put that label back.
	{"/var/www/[^/]+/data/www(/.*)?", "httpd_sys_content_t"},
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
	if v, _ := s.db.GetSetting(ctx, settingSELinux); v == selinuxPolicyVersion {
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
	return s.db.SetSetting(ctx, settingSELinux, selinuxPolicyVersion)
}

// selinuxPolicyVersion changes when the rules above do: a host that applied
// an older set applies the current one at the next site fix.
const selinuxPolicyVersion = "v2"

// settingSELinuxRelabeledAt remembers the last relabel: the doctor counts
// denials from then on, so a fixed label is not reported all day.
const settingSELinuxRelabeledAt = "selinux.relabeled_at"

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

// selinuxConfigPath sets the mode for the next boot; tests point it elsewhere.
var selinuxConfigPath = "/etc/selinux/config"

// selinuxWarning is what the administrator reads before switching to
// permissive, and sees while it is on.
const selinuxWarning = "SELinux в permissive ничего не блокирует, а только записывает отказы в журнал. " +
	"Взломанный сайт сможет прочитать /etc/shadow, /root и чужие домашние каталоги, данные почты и баз; " +
	"postfix, dovecot, mysqld и fail2ban лишатся своих доменов. Панель продолжит работать; вернуть: mp selinux enforcing."

// selinuxStatus reads the live mode and the configured one.
func (s *Server) selinuxStatus() apitypes.SELinuxStatus {
	st := apitypes.SELinuxStatus{Mode: "disabled"}
	raw, err := os.ReadFile(selinuxEnforcePath)
	if err != nil {
		return st
	}
	st.Supported = true
	st.Mode = "permissive"
	if strings.TrimSpace(string(raw)) == "1" {
		st.Mode = "enforcing"
	}
	if cfg, err := os.ReadFile(selinuxConfigPath); err == nil {
		for _, line := range strings.Split(string(cfg), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SELINUX="); ok {
				st.Configured = strings.TrimSpace(v)
			}
		}
	}
	// the price of permissive travels with the status: shown before switching and while it is on
	st.Warning = selinuxWarning
	return st
}

type selinuxOutput struct {
	Body apitypes.SELinuxStatus
}

type selinuxInput struct {
	Body apitypes.SELinuxRequest
}

func (s *Server) registerSELinux() {
	huma.Register(s.api, huma.Operation{
		OperationID: "selinux-status", Method: http.MethodGet, Path: "/system/selinux", Summary: "SELinux mode, live and configured", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(_ context.Context, _ *struct{}) (*selinuxOutput, error) {
		return &selinuxOutput{Body: s.selinuxStatus()}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "selinux-set", Method: http.MethodPut, Path: "/system/selinux", Summary: "Switch SELinux between enforcing and permissive, now and for the next boot", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *selinuxInput) (*selinuxOutput, error) {
		p := principalFrom(ctx)
		st := s.selinuxStatus()
		if !st.Supported {
			return nil, huma.Error422UnprocessableEntity("SELinux отсутствует на этом сервере")
		}
		flag := map[string]string{"enforcing": "1", "permissive": "0"}[in.Body.Mode]
		if flag == "" {
			return nil, huma.Error422UnprocessableEntity("режим: enforcing или permissive")
		}
		if st.Mode != in.Body.Mode {
			res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "setenforce", Args: []string{flag}, TimeoutSeconds: 30})
			if err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
			if res.ExitCode != 0 {
				return nil, huma.Error502BadGateway("setenforce: " + strings.TrimSpace(res.Output))
			}
		}
		if st.Configured != in.Body.Mode {
			// Keep the file's comments; only the SELINUX= line changes.
			cfg, err := os.ReadFile(selinuxConfigPath)
			if err != nil {
				cfg = []byte("# Generated by MonoPanel\nSELINUX=enforcing\nSELINUXTYPE=targeted\n")
			}
			lines := strings.Split(strings.TrimRight(string(cfg), "\n"), "\n")
			found := false
			for i, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), "SELINUX=") {
					lines[i] = "SELINUX=" + in.Body.Mode
					found = true
				}
			}
			if !found {
				lines = append(lines, "SELINUX="+in.Body.Mode)
			}
			if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: selinuxConfigPath, Content: strings.Join(lines, "\n") + "\n", Mode: 0o644}}, Origin: "selinux"}); err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "selinux." + in.Body.Mode, Target: "system", IP: requestInfo(ctx).IP})
		s.log.Warn("SELinux mode switched", "mode", in.Body.Mode, "by", p.Login)
		return &selinuxOutput{Body: s.selinuxStatus()}, nil
	})
}

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
	default:
		s.db.SetSetting(ctx, settingSELinuxRelabeledAt, time.Now().UTC().Format(time.RFC3339))
		if jc != nil {
			jc.Logf("SELinux labels restored under %s", dir)
		}
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
	if mode != "enforcing" {
		// switched off on purpose (mp selinux permissive) or by hand: say so every time
		c := check("selinux", "warn", "permissive: denials are only logged, the web domain is not confined (mp selinux enforcing)")
		c.Action = "selinux.enforcing"
		return &c
	}
	// denials since the last relabel when that was today, else since midnight
	since, sinceText := []string{"today"}, "today"
	if v, _ := s.db.GetSetting(ctx, settingSELinuxRelabeledAt); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			if l := t.Local(); l.YearDay() == time.Now().YearDay() && l.Year() == time.Now().Year() {
				// ausearch reads dates in the C locale's %x form: month/day/two-digit year
				since, sinceText = []string{l.Format("01/02/06"), l.Format("15:04:05")}, "since the fix at "+l.Format("15:04")
			}
		}
	}
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "ausearch", Args: append(append([]string{"-m", "AVC", "-ts"}, since...), "--raw"), TimeoutSeconds: 30})
	if err == nil && res.ExitCode != 0 && strings.Contains(res.Output, "date") && len(since) > 1 {
		// a locale that spells dates differently: count from midnight instead
		sinceText = "today"
		res, err = s.agent.Tool(ctx, &agent.ToolRequest{Name: "ausearch", Args: []string{"-m", "AVC", "-ts", "today", "--raw"}, TimeoutSeconds: 30})
	}
	// ausearch exits 1 when nothing matched — silently in --raw mode
	nothing := res != nil && res.ExitCode == 1 && (strings.TrimSpace(res.Output) == "" || strings.Contains(res.Output, "no matches"))
	if err != nil || (res.ExitCode != 0 && !nothing) {
		c := check("selinux", "ok", mode+"; audit log not readable, denials unknown")
		return &c
	}
	denials, last, action, target := 0, "", "", ""
	for _, line := range strings.Split(res.Output, "\n") {
		if !strings.Contains(line, "avc:  denied") || !strings.Contains(line, ":httpd_t:") {
			continue
		}
		denials++
		comm, obj, class, label := "", "", "", ""
		for _, m := range avcRe.FindAllStringSubmatch(line, -1) {
			switch {
			case m[1] != "":
				comm = m[1]
			case m[2] != "":
				obj = m[2]
			case m[3] != "":
				class = m[3]
			case m[4] != "":
				label = m[4]
			}
		}
		last = fmt.Sprintf("%s → %s (%s, %s)", comm, obj, class, label)
		switch rel, ok := strings.CutPrefix(obj, strings.TrimSuffix(s.cfg.WWWRoot, "/")+"/"); {
		case ok:
			// /var/www/<login>/data/www/<domain>/...: the site to fix
			if parts := strings.SplitN(rel, "/", 5); len(parts) >= 4 && parts[1] == "data" && parts[2] == "www" {
				last += "; mp site fix " + parts[3]
				action, target = "site.fix", parts[3]
			}
		case strings.HasSuffix(label, "_home_t"):
			// copied from /root or a home directory with cp -a / rsync -X
			last += "; a file copied from a home directory — mp site fix <domain>"
			action, target = "site.fix", "*"
		}
	}
	if denials == 0 {
		c := check("selinux", "ok", mode+", no denials for the web server "+sinceText)
		return &c
	}
	c := check("selinux", "warn", fmt.Sprintf("%s; %d denial(s) for the web server %s, last: %s", mode, denials, sinceText, last))
	c.Action, c.Target = action, target
	return &c
}
