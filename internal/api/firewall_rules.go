package api

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// Порядок правил в цепочке — то, на чём администратор запирает сам себя:
// «deny 8443» и три «allow 8443 с VPN» выглядят как «панель только с VPN»,
// но если deny стоит раньше, allow не срабатывают никогда. Поэтому правила
// раскладываются по трём полкам: allow с источником (исключения) — первыми,
// затем все deny, затем открытые порты и allow без источника. Deny на порт
// без источника при таком порядке закрывает порт для всех, кроме явно
// разрешённых адресов, — именно это от него и ждут.

type firewallPlan struct {
	except []string // allow с источником
	deny   []string
	allow  []string // allow без источника
}

func planFirewall(rules []*store.FirewallRule) firewallPlan {
	var p firewallPlan
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		switch {
		case r.Kind == "deny":
			p.deny = append(p.deny, ruleLine(r))
		case r.Source != "":
			p.except = append(p.except, ruleLine(r))
		default:
			p.allow = append(p.allow, ruleLine(r))
		}
	}
	return p
}

// portCovers reports whether a rule's port spec ("", "N" or "N-M") includes p.
func portCovers(spec string, p int) bool {
	if spec == "" {
		return true
	}
	lo, hi, ok := strings.Cut(spec, "-")
	a, err := strconv.Atoi(lo)
	if err != nil {
		return false
	}
	if !ok {
		return a == p
	}
	b, err := strconv.Atoi(hi)
	if err != nil {
		return false
	}
	return a <= p && p <= b
}

func protoTCP(proto string) bool { return proto == "" || proto == "tcp" || proto == "any" }

// sourceHas reports whether an IP or CIDR source contains ip.
func sourceHas(source, ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	if _, cidr, err := net.ParseCIDR(source); err == nil {
		return cidr.Contains(addr)
	}
	return net.ParseIP(source) != nil && net.ParseIP(source).Equal(addr)
}

// firewallRestricted lists the protected ports a source-less deny has closed,
// each with the sources that still get in.
func firewallRestricted(rules []*store.FirewallRule, protected []int) []apitypes.RestrictedPort {
	var out []apitypes.RestrictedPort
	for _, port := range protected {
		closed := false
		var sources []string
		for _, r := range rules {
			if !r.Enabled || !protoTCP(r.Proto) || !portCovers(r.Port, port) {
				continue
			}
			switch {
			case r.Kind == "deny" && r.Source == "":
				closed = true
			case r.Kind == "allow" && r.Source != "":
				sources = append(sources, r.Source)
			}
		}
		if closed {
			if sources == nil {
				sources = []string{}
			}
			out = append(out, apitypes.RestrictedPort{Port: port, Sources: sources})
		}
	}
	return out
}

// firewallProtected are the ports the administrator reaches the server by:
// SSH and the panel. Closing them to everyone is the lockout the guard
// refuses; 80/443 are open by default too, but closing them costs sites, not
// access.
func (s *Server) firewallProtected(ctx context.Context) []int {
	ports := append([]int{}, s.sshPorts(ctx)...)
	ports = append(ports, s.panelPort())
	sort.Ints(ports)
	return ports
}

func (s *Server) portName(port int) string {
	if port == s.panelPort() {
		return fmt.Sprintf("панель (порт %d)", port)
	}
	return fmt.Sprintf("SSH (порт %d)", port)
}

// lockoutError says why a rule set would lock the administrator out: a
// source-less deny on a protected port with no per-source allow to get
// through it (Deny set), or a deny that covers the caller's own address (Self).
type lockoutError struct {
	Port     string              // "панель (порт 8443)"
	Deny     *store.FirewallRule // the source-less deny that closes the port
	Self     *store.FirewallRule // the deny covering the caller
	CallerIP string
}

func (e *lockoutError) Error() string {
	if e.Self != nil {
		return fmt.Sprintf("deny%s закрывает %s с вашего текущего адреса %s", ruleRef(e.Self), e.Port, e.CallerIP)
	}
	hint := "с источником"
	if net.ParseIP(e.CallerIP) != nil {
		hint = fmt.Sprintf("с источником (например, вашим адресом %s)", e.CallerIP)
	}
	return fmt.Sprintf("deny%s без источника закроет %s для всех, включая вас; сначала добавьте allow на этот порт %s", ruleRef(e.Deny), e.Port, hint)
}

// onDelete words the same lockout for removing the rule that prevented it.
func (e *lockoutError) onDelete(id int64) string {
	if e.Self != nil {
		return e.Error()
	}
	return fmt.Sprintf("правило #%d — последний allow с источником для %s, без него deny%s закроет её для всех, включая вас; сначала удалите deny%s", id, e.Port, ruleRef(e.Deny), ruleRef(e.Deny))
}

func (s *Server) firewallLockout(rules []*store.FirewallRule, protected []int, callerIP string) *lockoutError {
	for _, port := range protected {
		var closedBy *store.FirewallRule
		allows := 0
		for _, r := range rules {
			if !r.Enabled || !protoTCP(r.Proto) || !portCovers(r.Port, port) {
				continue
			}
			switch {
			case r.Kind == "deny" && r.Source == "":
				if closedBy == nil {
					closedBy = r
				}
			case r.Kind == "allow" && r.Source != "":
				allows++
			case r.Kind == "deny" && sourceHas(r.Source, callerIP):
				return &lockoutError{Port: s.portName(port), Self: r, CallerIP: callerIP}
			}
		}
		if closedBy != nil && allows == 0 {
			return &lockoutError{Port: s.portName(port), Deny: closedBy, CallerIP: callerIP}
		}
	}
	return nil
}

// ruleRef names a stored rule by number; a rule that is only being proposed
// has no number yet.
func ruleRef(r *store.FirewallRule) string {
	if r.ID == 0 {
		return ""
	}
	return fmt.Sprintf(" #%d", r.ID)
}

// firewallGuard refuses a change that turns a reachable server into an
// unreachable one; a set that was already locked (say, by root over the
// local socket) is left for the administrator to untangle.
func (s *Server) firewallGuard(ctx context.Context, before, after []*store.FirewallRule) *lockoutError {
	protected := s.firewallProtected(ctx)
	ip := requestInfo(ctx).IP
	if s.firewallLockout(before, protected, ip) != nil {
		return nil //nolint:nilerr // уже запертый набор (root через локальный сокет) — не вина этого изменения
	}
	return s.firewallLockout(after, protected, ip)
}
