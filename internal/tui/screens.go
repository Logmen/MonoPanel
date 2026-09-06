package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

// Extra screens: sites, PHP, databases, SSL, firewall. Each fetches through
// the same API as the CLI and renders a plain table.

type sitesMsg struct {
	sites []*store.Site
	err   error
}

type phpMsg struct {
	v   *apitypes.PHPVersions
	err error
}

type dbsMsg struct {
	dbs []*store.Database
	err error
}

type certsMsg struct {
	certs []*store.Certificate
	err   error
}

type firewallMsg struct {
	fw  *apitypes.FirewallStatus
	err error
}

func (m model) fetchSites() tea.Msg {
	s, err := m.cl.Sites(context.Background())
	return sitesMsg{s, err}
}

func (m model) fetchPHP() tea.Msg {
	v, err := m.cl.PHPVersions(context.Background())
	return phpMsg{v, err}
}

func (m model) fetchDBs() tea.Msg {
	d, err := m.cl.Databases(context.Background())
	return dbsMsg{d, err}
}

func (m model) fetchCerts() tea.Msg {
	c, err := m.cl.Certificates(context.Background())
	return certsMsg{c, err}
}

func (m model) fetchFirewall() tea.Msg {
	f, err := m.cl.Firewall(context.Background())
	return firewallMsg{f, err}
}

func renderSites(b *strings.Builder, sites []*store.Site, loading bool) {
	b.WriteString(styleTitle.Render("Сайты") + "\n")
	for _, s := range sites {
		ssl := s.SSL
		if s.CertificateID != nil {
			ssl = "https"
		}
		b.WriteString(fmt.Sprintf("  %-28s %-8s php %-4s %-7s %-6s %s\n", s.Domain, s.Login, s.PHPVersion, s.Mode, ssl, stateColor(s.Status, "active")))
	}
	if len(sites) == 0 && !loading {
		b.WriteString(styleMuted.Render("  сайтов нет") + "\n")
	}
	b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp site add <domain> --user <login> --www") + styleMuted.Render(" · esc назад") + "\n")
}

func renderPHP(b *strings.Builder, v *apitypes.PHPVersions) {
	b.WriteString(styleTitle.Render("PHP") + "\n")
	if v == nil {
		return
	}
	installed := map[string]*store.PHPVersion{}
	for _, p := range v.Installed {
		installed[p.Version] = p
	}
	for _, a := range v.Available {
		state := styleMuted.Render("не установлена")
		if p := installed[a.Version]; p != nil {
			state = stateColor(p.Status, "installed") + " " + styleMuted.Render(p.PackageVersion)
		} else if !a.Available {
			state = styleMuted.Render("недоступна")
		}
		b.WriteString(fmt.Sprintf("  %-5s %-9s %s\n", a.Version, a.Support, state))
	}
	b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp php install 8.4") + styleMuted.Render(" · esc назад") + "\n")
}

func renderDBs(b *strings.Builder, dbs []*store.Database, loading bool) {
	b.WriteString(styleTitle.Render("Базы данных") + "\n")
	for _, d := range dbs {
		users := make([]string, 0, len(d.Users))
		for _, u := range d.Users {
			users = append(users, u.Name+"@"+u.Host)
		}
		b.WriteString(fmt.Sprintf("  %-24s %-10s %-10s %s\n", d.Name, d.Login, humanMB(d.SizeBytes), strings.Join(users, ",")))
	}
	if len(dbs) == 0 && !loading {
		b.WriteString(styleMuted.Render("  баз нет") + "\n")
	}
	b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp db create <name> --user <login>") + styleMuted.Render(" · esc назад") + "\n")
}

func renderCerts(b *strings.Builder, certs []*store.Certificate, loading bool) {
	b.WriteString(styleTitle.Render("Сертификаты") + "\n")
	for _, c := range certs {
		exp := "-"
		if c.NotAfter != nil {
			exp = c.NotAfter.Format("2006-01-02")
		}
		b.WriteString(fmt.Sprintf("  %-28s %-8s %-12s до %s %s\n", c.Name, stateColor(c.Status, "valid"), c.Issuer, exp, styleMuted.Render(firstLine(c.LastError, ""))))
	}
	if len(certs) == 0 && !loading {
		b.WriteString(styleMuted.Render("  сертификатов нет") + "\n")
	}
	b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp ssl issue <hostname>") + styleMuted.Render(" · esc назад") + "\n")
}

func renderFirewall(b *strings.Builder, fw *apitypes.FirewallStatus) {
	b.WriteString(styleTitle.Render("Firewall") + "\n")
	if fw == nil {
		return
	}
	state := styleWarn.Render("выключен")
	if fw.Enabled {
		state = styleKey.Render("включён")
	}
	b.WriteString(fmt.Sprintf("  nftables: %s · панель %d · ssh %v\n", state, fw.PanelPort, fw.SSHPorts))
	for _, r := range fw.Rules {
		b.WriteString(fmt.Sprintf("  #%-3d %-5s %-4s %-10s %-18s %s\n", r.ID, r.Kind, r.Proto, r.Port, r.Source, r.Comment))
	}
	if fw.Fail2ban != nil {
		for _, j := range fw.Fail2ban.Jails {
			b.WriteString(fmt.Sprintf("  fail2ban %-14s banned %d (total %d)\n", j.Name, j.Banned, j.Total))
		}
	} else {
		b.WriteString(styleMuted.Render("  fail2ban не установлен: mp stack install fail2ban") + "\n")
	}
	b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp firewall enable | allow --port N | ban <ip>") + styleMuted.Render(" · esc назад") + "\n")
}

func stateColor(state, good string) string {
	if state == good {
		return styleKey.Render(state)
	}
	if state == "error" || state == "failed" {
		return styleWarn.Render(state)
	}
	return styleMuted.Render(state)
}

func humanMB(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.0f КБ", float64(b)/1024)
	}
}
