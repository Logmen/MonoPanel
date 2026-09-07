package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

const (
	settingFirewall = "firewall.enabled"
	settingFail2ban = "stack.fail2ban"
	nftRulesPath    = "/etc/nftables.d/monopanel.nft"
	nftUnitPath     = "/etc/systemd/system/monopanel-firewall.service"
	nftUnit         = "monopanel-firewall.service"
)

var portRe = regexp.MustCompile(`^[0-9]{1,5}(-[0-9]{1,5})?$`)

const firewallUnit = `[Unit]
Description=MonoPanel firewall (nftables table inet monopanel)
After=network-pre.target
Wants=network-pre.target
Before=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/monopanel.nft
ExecStop=-/usr/sbin/nft delete table inet monopanel

[Install]
WantedBy=multi-user.target
`

type firewallOutput struct {
	Body apitypes.FirewallStatus
}

type firewallRuleInput struct {
	Body apitypes.FirewallRuleRequest
}

type firewallRuleOutput struct {
	Status int
	Body   *store.FirewallRule
}

type ruleIDInput struct {
	ID int64 `path:"id" minimum:"1"`
}

type banInput struct {
	Body apitypes.BanRequest
}

func validateRule(r apitypes.FirewallRuleRequest) error {
	if r.Kind != "allow" && r.Kind != "deny" {
		return errors.New("kind must be allow or deny")
	}
	if r.Port != "" && !portRe.MatchString(r.Port) {
		return errors.New("port must be N or N-M")
	}
	if r.Source != "" {
		if _, _, err := net.ParseCIDR(r.Source); err != nil && net.ParseIP(r.Source) == nil {
			return errors.New("source must be an IP or CIDR")
		}
	}
	if r.Kind == "deny" && r.Source == "" && r.Port == "" {
		return errors.New("a deny rule needs a source or a port")
	}
	return nil
}

func ruleLine(r *store.FirewallRule) string {
	parts := []string{}
	if r.Source != "" {
		fam := "ip"
		if strings.Contains(r.Source, ":") {
			fam = "ip6"
		}
		parts = append(parts, fam+" saddr "+r.Source)
	}
	if r.Port != "" {
		switch r.Proto {
		case "udp":
			parts = append(parts, "udp dport "+r.Port)
		case "any":
			parts = append(parts, "meta l4proto { tcp, udp } th dport "+r.Port)
		default:
			parts = append(parts, "tcp dport "+r.Port)
		}
	}
	verb := "accept"
	if r.Kind == "deny" {
		verb = "drop"
	}
	return strings.Join(parts, " ") + " " + verb
}

// sshPorts asks sshd for its effective listening ports.
func (s *Server) sshPorts(ctx context.Context) []int {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "sshd", Args: []string{"-T"}, TimeoutSeconds: 20})
	ports := []int{}
	if err == nil && res.ExitCode == 0 {
		for _, line := range strings.Split(res.Output, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "port" {
				if n, err := strconv.Atoi(f[1]); err == nil {
					ports = append(ports, n)
				}
			}
		}
	}
	if len(ports) == 0 {
		ports = []int{22}
	}
	return ports
}

func (s *Server) panelPort() int {
	_, port, _ := net.SplitHostPort(s.cfg.Web.Listen)
	n, err := strconv.Atoi(port)
	if err != nil || n == 0 {
		return 8443
	}
	return n
}

// applyFirewall renders and loads the nftables table.
func (s *Server) applyFirewall(ctx context.Context) (*apitypes.FirewallStatus, error) {
	if err := s.ensurePackages(ctx, nil, "nftables"); err != nil {
		return nil, err
	}
	rules, err := s.db.ListFirewallRules(ctx)
	if err != nil {
		return nil, err
	}
	base := map[int]bool{80: true, 443: true, s.panelPort(): true}
	for _, p := range s.sshPorts(ctx) {
		base[p] = true
	}
	// Почтовые порты открываются вместе с почтовым сервером: закрытый 25-й
	// молча съедал бы входящую почту.
	for _, p := range firewallMailPorts(s.loadMailConfig(ctx)) {
		base[p] = true
	}
	ports := make([]string, 0, len(base))
	for p := range base {
		ports = append(ports, strconv.Itoa(p))
	}
	sort.Slice(ports, func(i, j int) bool { a, _ := strconv.Atoi(ports[i]); b, _ := strconv.Atoi(ports[j]); return a < b })
	fw := render.Firewall{Policy: "drop", Allow: []string{"tcp dport { " + strings.Join(ports, ", ") + " } accept"}}
	if s.anySiteHTTP3(ctx) {
		fw.Allow = append(fw.Allow, "udp dport 443 accept")
	}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if r.Kind == "deny" {
			fw.Deny = append(fw.Deny, ruleLine(r))
		} else {
			fw.Allow = append(fw.Allow, ruleLine(r))
		}
	}
	text, err := s.render.Render("nftables/monopanel.nft.tmpl", fw)
	if err != nil {
		return nil, err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{
		{Path: nftRulesPath, Content: text, Mode: 0o644},
		{Path: nftUnitPath, Content: firewallUnit, Mode: 0o644},
	}, Origin: "firewall"}); err != nil {
		return nil, err
	}
	check, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "nft", Args: []string{"-c", "-f", nftRulesPath}})
	if err != nil {
		return nil, err
	}
	if check.ExitCode != 0 {
		return nil, fmt.Errorf("nft check failed: %s", strings.TrimSpace(check.Output))
	}
	load, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "nft", Args: []string{"-f", nftRulesPath}})
	if err != nil {
		return nil, err
	}
	if load.ExitCode != 0 {
		return nil, fmt.Errorf("nft load failed: %s", strings.TrimSpace(load.Output))
	}
	s.agent.Service(ctx, "", "daemon-reload") //nolint:errcheck // best effort: the state is read back afterwards
	if _, err := s.agent.Service(ctx, nftUnit, "enable"); err != nil {
		return nil, err
	}
	s.agent.Service(ctx, nftUnit, "start") //nolint:errcheck // best effort: the state is read back afterwards
	s.db.SetSetting(ctx, settingFirewall, "yes")
	return s.firewallStatus(ctx)
}

func (s *Server) anySiteHTTP3(ctx context.Context) bool {
	sites, _ := s.db.ListSites(ctx, 0)
	for _, site := range sites {
		if site.HTTP3 {
			return true
		}
	}
	return false
}

func (s *Server) disableFirewall(ctx context.Context) error {
	s.agent.Tool(ctx, &agent.ToolRequest{Name: "nft", Args: []string{"delete", "table", "inet", "monopanel"}}) //nolint:errcheck // best effort: the state is read back afterwards
	s.agent.Service(ctx, nftUnit, "disable")                                                                   //nolint:errcheck // best effort: the state is read back afterwards
	return s.db.SetSetting(ctx, settingFirewall, "no")
}

func (s *Server) firewallStatus(ctx context.Context) (*apitypes.FirewallStatus, error) {
	st := &apitypes.FirewallStatus{PanelPort: s.panelPort()}
	v, _ := s.db.GetSetting(ctx, settingFirewall)
	st.Enabled = v == "yes"
	st.Rules, _ = s.db.ListFirewallRules(ctx)
	if st.Rules == nil {
		st.Rules = []*store.FirewallRule{}
	}
	actx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st.SSHPorts = s.sshPorts(actx)
	if res, err := s.agent.Tool(actx, &agent.ToolRequest{Name: "nft", Args: []string{"list", "table", "inet", "monopanel"}}); err == nil && res.ExitCode == 0 {
		st.Active = true
	}
	if f, _ := s.db.GetSetting(ctx, settingFail2ban); f == "installed" {
		st.Fail2ban = &apitypes.Fail2banStatus{Installed: true, Jails: []apitypes.JailStatus{}}
		if res, err := s.agent.Tool(actx, &agent.ToolRequest{Name: "fail2ban-client", Args: []string{"status"}}); err == nil && res.ExitCode == 0 {
			st.Fail2ban.Running = true
			for _, line := range strings.Split(res.Output, "\n") {
				if i := strings.Index(line, "Jail list:"); i >= 0 {
					for _, j := range strings.Split(strings.TrimSpace(line[i+10:]), ",") {
						j = strings.TrimSpace(j)
						if j == "" {
							continue
						}
						js := apitypes.JailStatus{Name: j}
						if r, err := s.agent.Tool(actx, &agent.ToolRequest{Name: "fail2ban-client", Args: []string{"status", j}}); err == nil {
							for _, l := range strings.Split(r.Output, "\n") {
								if i := strings.Index(l, "Currently banned:"); i >= 0 {
									js.Banned, _ = strconv.Atoi(strings.TrimSpace(l[i+17:]))
								}
								if i := strings.Index(l, "Total banned:"); i >= 0 {
									js.Total, _ = strconv.Atoi(strings.TrimSpace(l[i+13:]))
								}
								if i := strings.Index(l, "Banned IP list:"); i >= 0 {
									js.IPs = strings.Fields(l[i+15:])
								}
							}
						}
						st.Fail2ban.Jails = append(st.Fail2ban.Jails, js)
					}
				}
			}
		}
	}
	return st, nil
}

func (s *Server) registerFirewall() {
	huma.Register(s.api, huma.Operation{
		OperationID: "firewall-status", Method: http.MethodGet, Path: "/firewall", Summary: "Firewall and fail2ban status", Tags: []string{"firewall"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*firewallOutput, error) {
		st, err := s.firewallStatus(ctx)
		if err != nil {
			return nil, err
		}
		return &firewallOutput{Body: *st}, nil
	})

	for _, action := range []string{"apply", "enable", "disable"} {
		action := action
		huma.Register(s.api, huma.Operation{
			OperationID: "firewall-" + action, Method: http.MethodPost, Path: "/firewall/" + action, Summary: strings.ToUpper(action[:1]) + action[1:] + " the firewall", Tags: []string{"firewall"}, Security: secured, Metadata: adminOnly,
		}, func(ctx context.Context, _ *struct{}) (*firewallOutput, error) {
			p := principalFrom(ctx)
			actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			var st *apitypes.FirewallStatus
			var err error
			if action == "disable" {
				if err = s.disableFirewall(actx); err == nil {
					st, err = s.firewallStatus(actx)
				}
			} else {
				st, err = s.applyFirewall(actx)
			}
			s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "firewall." + action, IP: requestInfo(ctx).IP, Result: resultOf(err)})
			if err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
			return &firewallOutput{Body: *st}, nil
		})
	}

	huma.Register(s.api, huma.Operation{
		OperationID: "firewall-rule-add", Method: http.MethodPost, Path: "/firewall/rules", Summary: "Add an allow/deny rule and apply", Tags: []string{"firewall"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *firewallRuleInput) (*firewallRuleOutput, error) {
		p := principalFrom(ctx)
		if err := validateRule(in.Body); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		proto := in.Body.Proto
		if proto == "" {
			proto = "tcp"
		}
		r := &store.FirewallRule{Kind: in.Body.Kind, Proto: proto, Port: in.Body.Port, Source: in.Body.Source, Comment: in.Body.Comment, Enabled: true}
		if err := s.db.CreateFirewallRule(ctx, r); err != nil {
			return nil, err
		}
		if v, _ := s.db.GetSetting(ctx, settingFirewall); v == "yes" {
			if _, err := s.applyFirewall(ctx); err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "firewall.rule", Target: ruleLine(r), IP: requestInfo(ctx).IP})
		return &firewallRuleOutput{Status: http.StatusCreated, Body: r}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "firewall-rule-delete", Method: http.MethodDelete, Path: "/firewall/rules/{id}", Summary: "Delete a rule and apply", Tags: []string{"firewall"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *ruleIDInput) (*struct{}, error) {
		p := principalFrom(ctx)
		if err := s.db.DeleteFirewallRule(ctx, in.ID); errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("rule not found")
		} else if err != nil {
			return nil, err
		}
		if v, _ := s.db.GetSetting(ctx, settingFirewall); v == "yes" {
			if _, err := s.applyFirewall(ctx); err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "firewall.rule.delete", Target: strconv.FormatInt(in.ID, 10), IP: requestInfo(ctx).IP})
		return nil, nil
	})

	for _, action := range []string{"ban", "unban"} {
		action := action
		huma.Register(s.api, huma.Operation{
			OperationID: "firewall-" + action, Method: http.MethodPost, Path: "/firewall/" + action, Summary: strings.ToUpper(action[:1]) + action[1:] + " an address (deny rule)", Tags: []string{"firewall"}, Security: secured, Metadata: adminOnly,
		}, func(ctx context.Context, in *banInput) (*firewallOutput, error) {
			p := principalFrom(ctx)
			ip := strings.TrimSpace(in.Body.IP)
			if _, _, err := net.ParseCIDR(ip); err != nil && net.ParseIP(ip) == nil {
				return nil, huma.Error422UnprocessableEntity("ip must be an address or CIDR")
			}
			rules, _ := s.db.ListFirewallRules(ctx)
			if action == "ban" {
				exists := false
				for _, r := range rules {
					if r.Kind == "deny" && r.Source == ip && r.Port == "" {
						exists = true
					}
				}
				if !exists {
					s.db.CreateFirewallRule(ctx, &store.FirewallRule{Kind: "deny", Proto: "any", Source: ip, Comment: "banned by " + p.Login, Enabled: true}) //nolint:errcheck // best effort; the caller reports the real failure
				}
			} else {
				for _, r := range rules {
					if r.Kind == "deny" && r.Source == ip && r.Port == "" {
						s.db.DeleteFirewallRule(ctx, r.ID) //nolint:errcheck // best effort; the caller reports the real failure
					}
				}
				s.agent.Tool(ctx, &agent.ToolRequest{Name: "fail2ban-client", Args: []string{"unban", ip}}) //nolint:errcheck // best effort: the state is read back afterwards
			}
			st, err := s.applyFirewall(ctx)
			s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "firewall." + action, Target: ip, IP: requestInfo(ctx).IP, Result: resultOf(err)})
			if err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
			return &firewallOutput{Body: *st}, nil
		})
	}
}

// installFail2ban installs fail2ban with jails for sshd, nginx and the panel.
func (s *Server) installFail2ban(ctx context.Context, jc *jobs.Context) error {
	jc.Progress(10, "installing fail2ban")
	pkgs := []string{"fail2ban", "python3-systemd"}
	if s.profile.Family() == "rhel" {
		pkgs = []string{"epel-release", "fail2ban", "python3-systemd"}
	}
	res, err := s.agent.Pkg(ctx, "install", pkgs...)
	if err != nil {
		return err
	}
	logTail(jc, res.Output, 2)
	jc.Progress(50, "jails")
	nginx := false
	if q, err := s.agent.Pkg(ctx, "query", "nginx"); err == nil {
		_, nginx = q.Installed["nginx"]
	}
	jail, err := s.render.Render("fail2ban/jail.local.tmpl", render.Fail2ban{BanTime: "1h", FindTime: "10m", MaxRetry: 5, PanelPort: strconv.Itoa(s.panelPort()), Nginx: nginx})
	if err != nil {
		return err
	}
	filter, _, err := s.render.Source("fail2ban/filter-monopanel.conf")
	if err != nil {
		return err
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{
		{Path: "/etc/fail2ban/jail.d/monopanel.local", Content: jail, Mode: 0o644},
		{Path: "/etc/fail2ban/filter.d/monopanel.conf", Content: string(filter), Mode: 0o644},
	}, Restart: []string{"fail2ban.service"}, Force: true, Origin: "fail2ban"}); err != nil {
		return err
	}
	if _, err := s.agent.Service(ctx, "fail2ban.service", "enable"); err != nil {
		return err
	}
	st, err := s.agent.Service(ctx, "fail2ban.service", "status")
	if err != nil {
		return err
	}
	jc.Logf("fail2ban.service: %s (%s); jails: sshd, monopanel, nginx=%v", st.Status.ActiveState, st.Status.SubState, nginx)
	if st.Status.ActiveState != "active" {
		return fmt.Errorf("fail2ban is %s after install", st.Status.ActiveState)
	}
	s.db.SetSetting(ctx, settingFail2ban, "installed")
	jc.Progress(100, "fail2ban ready")
	return nil
}
