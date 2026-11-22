package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/config"
	"monopanel/internal/store"
	"monopanel/internal/updater"
)

// The panel's own settings in config.yaml — its name, its HTTPS address,
// logging and the number of job workers — for every client. The API process
// cannot write /etc (ProtectSystem=strict), and must not: the same file holds
// the release key. The agent changes these five keys and nothing else.

type panelConfigOutput struct {
	Body apitypes.PanelConfig
}

type panelConfigSetInput struct {
	Body apitypes.PanelConfigUpdate
}

type panelConfigSetOutput struct {
	Body apitypes.PanelConfigResult
}

type templateListOutput struct {
	Body apitypes.TemplateList
}

// lowPortStart is the first port an unprivileged process may listen on, and
// canBindLowPorts tells whether this one may listen below it all the same
// (CAP_NET_BIND_SERVICE). monopanel-api runs as the service user without any
// capabilities, and a web.listen it cannot bind leaves the panel down after
// the restart. Tests replace both.
var (
	lowPortStart = func() int {
		b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
		if err != nil {
			return 1024
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return 1024
		}
		return n
	}
	canBindLowPorts = func() bool {
		f, err := os.Open("/proc/self/status")
		if err != nil {
			return false
		}
		defer f.Close() //nolint:errcheck // read-only
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "CapEff:"); ok {
				caps, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
				return err == nil && caps&(1<<10) != 0 // CAP_NET_BIND_SERVICE
			}
		}
		return false
	}
)

// panelConfigFile reads config.yaml afresh: the page shows what the file
// says, which is what the panel runs with after its next restart.
func (s *Server) panelConfigFile() (config.Config, error) {
	if s.cfg.Path() == "" {
		// A configuration built in memory has no file behind it.
		return s.cfg, nil
	}
	cfg, err := config.Load(s.cfg.Path())
	if err != nil {
		return cfg, huma.Error500InternalServerError(err.Error())
	}
	return cfg, nil
}

// panelConfigView is the file's settings for the clients, with whether the
// running process still has others. key_pinned is the key in effect, as GET
// /system/update reports it: both daemons read it at start, so a key changed
// in the file only shows as restart_pending until then.
func (s *Server) panelConfigView(file config.Config) apitypes.PanelConfig {
	key, _ := updater.ReleaseKey(s.cfg.Update.PublicKey)
	return apitypes.PanelConfig{
		Path:           file.Path(),
		Hostname:       file.Web.Hostname,
		Listen:         file.Web.Listen,
		LogLevel:       logLevelName(file.Log.Level),
		LogFormat:      logFormatName(file.Log.Format),
		JobsWorkers:    file.Jobs.Workers,
		DataDir:        file.DataDir,
		TemplatesDir:   file.TemplatesDir,
		KeyPinned:      key != "",
		RestartPending: !reflect.DeepEqual(file, s.cfg),
		// While a move waits for the restart, the clients still reach the
		// panel here.
		RunningHostname: s.cfg.Web.Hostname,
		RunningListen:   s.cfg.Web.Listen,
	}
}

// logLevelName and logFormatName are the values as the daemons' logger reads
// them: a level it does not know means info, any format but json is text.
func logLevelName(v string) string {
	switch v = strings.ToLower(v); v {
	case "debug", "warn", "error":
		return v
	case "warning":
		return "warn"
	}
	return "info"
}

func logFormatName(v string) string {
	if v == "json" {
		return v
	}
	return "text"
}

func (s *Server) registerPanelConfig() {
	huma.Register(s.api, huma.Operation{
		OperationID: "system-config-get", Method: http.MethodGet, Path: "/system/config", Summary: "The panel's own settings in config.yaml: name, HTTPS address, logging, job workers", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
		Description: "Read from the file on every call. restart_pending says that the file differs from what the running monopanel-api loaded. The release key (update.public_key) is not here: root changes it with mp update trust.",
	}, func(_ context.Context, _ *struct{}) (*panelConfigOutput, error) {
		file, err := s.panelConfigFile()
		if err != nil {
			return nil, err
		}
		return &panelConfigOutput{Body: s.panelConfigView(file)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "system-config-set", Method: http.MethodPut, Path: "/system/config", Summary: "Change the panel's own settings; with restart, monopanel-api restarts right after the answer", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
		Description: "Omitted fields keep their value. The agent writes config.yaml (these five keys only). A new web.listen must be an address this server can listen on, and its port is opened in the firewall (the panel's nftables table or a running firewalld) before anything is written. The change takes effect after monopanel-api restarts; with restart, the web.listen the restart applies is checked the same way and the agent must be able to restart the unit, else nothing changes. Reconnect a few seconds after a restart, at the new address when it changed.",
	}, s.setPanelConfig)

	huma.Register(s.api, huma.Operation{
		OperationID: "system-templates", Method: http.MethodGet, Path: "/system/templates", Summary: "Built-in configuration templates and which of them templates_dir overrides", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(_ context.Context, _ *struct{}) (*templateListOutput, error) {
		names, err := s.render.List()
		if err != nil {
			return nil, err
		}
		out := apitypes.TemplateList{Dir: s.cfg.TemplatesDir, Templates: make([]apitypes.TemplateInfo, 0, len(names))}
		for _, name := range names {
			t := apitypes.TemplateInfo{Name: name}
			// The renderer's own answer: an override it cannot read is not used.
			if _, override, _ := s.render.Source(name); override {
				t.Override, t.Path = true, filepath.Join(s.cfg.TemplatesDir, filepath.FromSlash(name))
			}
			out.Templates = append(out.Templates, t)
		}
		return &templateListOutput{Body: out}, nil
	})
}

func (s *Server) setPanelConfig(ctx context.Context, in *panelConfigSetInput) (*panelConfigSetOutput, error) {
	p := principalFrom(ctx)
	cur, err := s.panelConfigFile()
	if err != nil {
		return nil, err
	}
	b := in.Body
	asked := map[string]*string{config.KeyHostname: b.Hostname, config.KeyListen: b.Listen, config.KeyLogLevel: b.LogLevel, config.KeyLogFormat: b.LogFormat}
	if b.JobsWorkers != nil {
		n := strconv.Itoa(*b.JobsWorkers)
		asked[config.KeyJobsWorkers] = &n
	}
	// Only what differs from the file goes to the agent: a form sent back
	// unchanged writes nothing, and a value the file already has is not
	// judged again (mp setup may have written a host name these rules refuse).
	set := map[string]string{}
	for _, key := range config.PanelKeys() {
		v := asked[key]
		if v == nil {
			continue
		}
		was := cur.PanelValue(key)
		if strings.TrimSpace(*v) == was {
			continue
		}
		nv, err := config.NormalizePanelValue(key, *v)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if same, err := config.NormalizePanelValue(key, was); nv == was || err == nil && same == nv {
			continue
		}
		set[key] = nv
	}
	listen := cur.Web.Listen
	_, moved := set[config.KeyListen]
	if moved {
		listen = set[config.KeyListen]
		if err := s.checkListen(ctx, listen); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
	} else if b.Restart && listen != s.cfg.Web.Listen {
		// The restart applies a web.listen that is in the file already —
		// written earlier without a restart, or by root by hand: the panel
		// must be able to come back there as well.
		if err := s.checkListen(ctx, listen); err != nil {
			return nil, huma.Error422UnprocessableEntity("config.yaml, which the restart applies: " + err.Error())
		}
	}
	if b.Restart {
		if err := s.checkAPIRestart(ctx); err != nil {
			return nil, err
		}
	}
	// The firewall lets the port the panel runs on through. The one it moves
	// to must be open before it gets there: otherwise nobody can reach the
	// panel to undo the change. The old port stays open until the next
	// firewall change, so this session keeps working until the restart.
	if moved || b.Restart {
		if port := listenPort(listen); port != 0 && port != s.panelPort() {
			if err := s.panelMoveGuard(ctx, port); err != nil {
				return nil, huma.Error409Conflict(err.Error())
			}
			if err := s.openPanelPort(ctx, port); err != nil {
				return nil, huma.Error502BadGateway(fmt.Sprintf("the firewall did not open port %d for the panel, so nothing was changed: %v", port, err))
			}
		}
	}

	changes := map[string]any{}
	if len(set) > 0 {
		res, err := s.agent.SetPanelConfig(ctx, &agent.PanelConfigRequest{Path: s.cfg.Path(), Set: set})
		if err != nil {
			s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "config.set", Target: cur.Path(), IP: requestInfo(ctx).IP, Result: "error", Details: map[string]any{"keys": sortedKeys(set), "error": err.Error()}})
			return nil, agentRefusal(err)
		}
		for _, k := range res.Changed {
			changes[k] = map[string]string{"from": cur.PanelValue(k), "to": set[k]}
		}
	}
	// Recorded before the file is read back: the change is made either way.
	if len(changes) > 0 || b.Restart {
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "config.set", Target: cur.Path(), IP: requestInfo(ctx).IP, Details: map[string]any{"changes": changes, "restart": b.Restart}})
	}
	// A file this process cannot load any more would keep the restarted one
	// from starting at all: no restart then.
	file, err := s.panelConfigFile()
	if err != nil {
		return nil, err
	}
	if b.Restart {
		afterResponse(ctx, s.restartAPI)
	}
	return &panelConfigSetOutput{Body: apitypes.PanelConfigResult{Config: s.panelConfigView(file), Restarted: b.Restart}}, nil
}

// checkAPIRestart makes sure the agent can restart monopanel-api before the
// answer says it restarts: a client told so waits for the panel to come back.
func (s *Server) checkAPIRestart(ctx context.Context) error {
	unit := s.cfg.Update.APIUnit
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := s.agent.Service(actx, unit, "status")
	if err != nil {
		return huma.Error502BadGateway("cannot restart " + unit + ", so nothing was changed: agent: " + err.Error())
	}
	if res.Status.LoadState != "loaded" {
		return huma.Error409Conflict(fmt.Sprintf("%s is not a loaded systemd unit on this server (load state %q), so the panel cannot restart itself and nothing was changed; save without restart and restart the API by hand", unit, res.Status.LoadState))
	}
	return nil
}

// agentRefusal turns the agent's answer into the API's: what it refused is
// the request's fault (or the file's) and says why, anything else is the
// agent failing.
func agentRefusal(err error) error {
	var ae *agent.Error
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusConflict:
			return huma.Error409Conflict(ae.Message)
		case http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity:
			return huma.Error422UnprocessableEntity(ae.Message)
		case http.StatusNotFound:
			// An agent older than this API: the two restart together after an
			// update, so one of them did not.
			return huma.Error502BadGateway("the agent does not know how to change config.yaml yet: restart monopanel-agent so that it runs the same version as the API")
		}
	}
	return huma.Error502BadGateway("agent: " + err.Error())
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// listenPort is the port of a web.listen value (0 when there is none).
func listenPort(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

// checkListen refuses an address the panel could not listen on after its
// restart: it would stay down, and only ssh and mp config set --local could
// bring it back.
func (s *Server) checkListen(ctx context.Context, listen string) error {
	host, _, err := net.SplitHostPort(listen)
	port := listenPort(listen)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("web.listen %q is not host:port or :port with a port from 1 to 65535", listen)
	}
	if start := lowPortStart(); port < start && !canBindLowPorts() {
		return fmt.Errorf("web.listen: the panel runs without privileges and cannot listen on port %d; choose a port from %d to 65535", port, start)
	}
	var addrs []string // where the new listener will be
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		addrs = append([]string{"127.0.0.1"}, append(s.hostIPs(), s.hostIPv6s()...)...)
	} else {
		if ip == nil {
			lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			found, err := net.DefaultResolver.LookupIPAddr(lctx, host)
			cancel()
			if err != nil || len(found) == 0 {
				return fmt.Errorf("web.listen: %s does not resolve on this server; give an IP address of the server instead", host)
			}
			// A listener on a name takes one of its addresses, the first
			// IPv4 one when there is one — as net.Listen does.
			ip = found[0].IP
			for _, a := range found {
				if a.IP.To4() != nil {
					ip = a.IP
					break
				}
			}
		}
		if !localIP(ip) {
			return fmt.Errorf("web.listen: %s is not an address of this server, the panel could not listen there", ip)
		}
		addrs = append(addrs, ip.String())
	}
	if port == s.panelPort() {
		return nil // the panel's own listener holds it now
	}
	for _, a := range addrs {
		if s.reach(a, port) {
			return fmt.Errorf("web.listen: port %d is already in use on %s; choose another one", port, a)
		}
	}
	return nil
}

// panelAddress is host:port of the panel for a command handed to another
// machine (the hint of mp migrate grant): the configured name, else the
// address the request came to. Over the local socket (Host "monopanel") or
// from the console (127.0.0.1) that address means nothing elsewhere, and the
// one address the panel listens on stands in, else the host's own address —
// a public one when it has one.
func (s *Server) panelAddress(ctx context.Context) string {
	port := strconv.Itoa(s.panelPort())
	if h := s.cfg.Web.Hostname; h != "" {
		return net.JoinHostPort(h, port)
	}
	host := requestInfo(ctx).Host
	name, p, err := net.SplitHostPort(host)
	if err != nil {
		name, p = strings.Trim(host, "[]"), ""
	}
	if ip := net.ParseIP(name); name == "" || name == "monopanel" || name == "localhost" || ip != nil && ip.IsLoopback() {
		if h, _, err := net.SplitHostPort(s.cfg.Web.Listen); err == nil && h != "" && h != "localhost" {
			if ip := net.ParseIP(h); ip == nil || !ip.IsUnspecified() && !ip.IsLoopback() {
				return net.JoinHostPort(h, port)
			}
		}
		own := append(s.hostIPs(), s.hostIPv6s()...)
		for _, a := range own {
			if ip := net.ParseIP(a); ip != nil && !ip.IsPrivate() {
				return net.JoinHostPort(a, port)
			}
		}
		if len(own) > 0 {
			return net.JoinHostPort(own[0], port)
		}
	}
	if p == "" {
		p = port
	}
	return net.JoinHostPort(name, p)
}

// localIP reports whether the host has the address (loopback always counts).
func localIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // cannot tell: do not refuse on a guess
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// panelMoveGuard refuses to move the panel to a port the firewall rules treat
// differently from the current one: a port a deny closes (to everyone, or to
// the caller's address), or an open port when the rules keep the current one
// for some sources only — "allow 8443 from the VPN, deny 8443" would
// otherwise stop protecting the panel without a word. The rules count also
// while the firewall is off: they are what it applies once it is on.
func (s *Server) panelMoveGuard(ctx context.Context, to int) error {
	rules, err := s.db.ListFirewallRules(ctx)
	if err != nil {
		return err
	}
	if lock := s.firewallLockout(rules, []int{to}, requestInfo(ctx).IP); lock != nil {
		lock.Port = fmt.Sprintf("the panel's new port %d", to)
		return lock
	}
	from := s.panelPort()
	kept := firewallRestricted(rules, []int{from})
	if len(kept) == 0 || len(firewallRestricted(rules, []int{to})) > 0 {
		return nil
	}
	if len(kept[0].Sources) == 0 {
		return fmt.Errorf("the firewall rules close port %d to everyone, and on port %d the panel would be open to everyone; close port %d the same way first (mp firewall deny --port %d), then move the panel", from, to, to, to)
	}
	steps := make([]string, 0, len(kept[0].Sources)+1)
	for _, src := range kept[0].Sources {
		steps = append(steps, fmt.Sprintf("mp firewall allow --port %d --source %s", to, src))
	}
	steps = append(steps, fmt.Sprintf("mp firewall deny --port %d", to))
	return fmt.Errorf("the firewall rules keep port %d for %s only, and on port %d the panel would be open to everyone; give port %d the same rules first (%s), then move the panel",
		from, strings.Join(kept[0].Sources, ", "), to, to, strings.Join(steps, "; "))
}

// openPanelPort lets a new panel port through the host's firewall: the
// panel's own nftables table when it is on, else a running firewalld (some EL
// images ship it with only ssh allowed, which is why mp setup opens the panel
// port there too).
func (s *Server) openPanelPort(ctx context.Context, port int) error {
	actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if v, _ := s.db.GetSetting(actx, settingFirewall); v == "yes" {
		_, err := s.applyFirewall(actx, port)
		return err
	}
	_, err := s.firewalldAdd(actx, strconv.Itoa(port)+"/tcp")
	return err
}

// restartAPI restarts monopanel-api through the agent, as POST
// /services/{unit} does. It runs once the answer to the request that asked
// for it has been written: the process it stops is this one, so a success
// is never heard of here.
func (s *Server) restartAPI() {
	time.Sleep(s.restartDelay)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	unit := s.cfg.Update.APIUnit
	s.log.Info("restarting to apply config.yaml", "unit", unit)
	if _, err := s.agent.Service(ctx, unit, "restart"); err != nil {
		s.log.Error("restart after a change of config.yaml", "unit", unit, "err", err)
	}
}
