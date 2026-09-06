// Package tui is the interactive menu shown by `mp` without arguments. It is
// a thin client of the same API as the Web UI and the CLI.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
	"monopanel/internal/store"
)

type screen int

const (
	screenMenu screen = iota
	screenUsers
	screenJobs
	screenServices
	screenSites
	screenPHP
	screenDBs
	screenCerts
	screenFirewall
)

type menuItem struct {
	title string
	hint  string
	open  screen
}

var menu = []menuItem{
	{"Сайты", "", screenSites},
	{"Пользователи", "", screenUsers},
	{"PHP", "", screenPHP},
	{"Базы данных", "", screenDBs},
	{"SSL", "", screenCerts},
	{"Firewall", "", screenFirewall},
	{"Сервисы", "", screenServices},
	{"Задачи", "", screenJobs},
	{"Бэкапы", "mp backup", screenMenu},
	{"Настройки", "mp config", screenMenu},
}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#3FC9B8"))
	styleMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A98A4"))
	styleCursor = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#0E7C73"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F2A33A"))
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("#3FC9B8"))
)

type model struct {
	cl      *client.Client
	status  *apitypes.SystemStatus
	err     error
	cursor  int
	screen  screen
	users   []*store.User
	jobs    []*store.Job
	sites   []*store.Site
	php     *apitypes.PHPVersions
	dbs     []*store.Database
	certs   []*store.Certificate
	fw      *apitypes.FirewallStatus
	loading bool
	width   int
}

type statusMsg struct {
	s   *apitypes.SystemStatus
	err error
}

type usersMsg struct {
	users []*store.User
	err   error
}

type jobsMsg struct {
	jobs []*store.Job
	err  error
}

// Run starts the TUI.
func Run(ctx context.Context, cl *client.Client) error {
	m := model{cl: cl, loading: true}
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}

func (m model) fetchStatus() tea.Msg {
	s, err := m.cl.Status(context.Background())
	return statusMsg{s, err}
}

func (m model) fetchUsers() tea.Msg {
	u, err := m.cl.ListUsers(context.Background())
	return usersMsg{u, err}
}

func (m model) fetchJobs() tea.Msg {
	j, err := m.cl.ListJobs(context.Background(), 20, "")
	return jobsMsg{j, err}
}

func (m model) Init() tea.Cmd { return m.fetchStatus }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case statusMsg:
		m.status, m.err, m.loading = msg.s, msg.err, false
	case usersMsg:
		m.users, m.err, m.loading = msg.users, msg.err, false
	case jobsMsg:
		m.jobs, m.err, m.loading = msg.jobs, msg.err, false
	case sitesMsg:
		m.sites, m.err, m.loading = msg.sites, msg.err, false
	case phpMsg:
		m.php, m.err, m.loading = msg.v, msg.err, false
	case dbsMsg:
		m.dbs, m.err, m.loading = msg.dbs, msg.err, false
	case certsMsg:
		m.certs, m.err, m.loading = msg.certs, msg.err, false
	case firewallMsg:
		m.fw, m.err, m.loading = msg.fw, msg.err, false
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q", "esc":
			if m.screen != screenMenu {
				m.screen = screenMenu
				return m, nil
			}
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(menu)-1 {
				m.cursor++
			}
		case "r":
			m.loading = true
			return m, m.fetchStatus
		case "enter":
			item := menu[m.cursor]
			switch item.open {
			case screenUsers:
				m.screen, m.loading = screenUsers, true
				return m, m.fetchUsers
			case screenJobs:
				m.screen, m.loading = screenJobs, true
				return m, m.fetchJobs
			case screenServices:
				m.screen, m.loading = screenServices, true
				return m, m.fetchStatus
			case screenSites:
				m.screen, m.loading = screenSites, true
				return m, m.fetchSites
			case screenPHP:
				m.screen, m.loading = screenPHP, true
				return m, m.fetchPHP
			case screenDBs:
				m.screen, m.loading = screenDBs, true
				return m, m.fetchDBs
			case screenCerts:
				m.screen, m.loading = screenCerts, true
				return m, m.fetchCerts
			case screenFirewall:
				m.screen, m.loading = screenFirewall, true
				return m, m.fetchFirewall
			}
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	var b strings.Builder
	b.WriteString(styleTitle.Render("MonoPanel"))
	if m.status != nil {
		b.WriteString(styleMuted.Render("  v" + m.status.Panel.Version))
	}
	b.WriteString("\n")
	if m.status != nil && m.status.Host != nil {
		h := m.status.Host
		used := float64(h.MemTotalBytes-h.MemAvailableBytes) / 1073741824
		total := float64(h.MemTotalBytes) / 1073741824
		b.WriteString(fmt.Sprintf("%s · %s · load %.2f %.2f %.2f · RAM %.1f/%.1f ГБ", h.Hostname, h.Release.PrettyName, h.Load[0], h.Load[1], h.Load[2], used, total))
		if !m.status.Panel.AgentOK {
			b.WriteString("  " + styleWarn.Render("агент недоступен"))
		}
		b.WriteString("\n")
	}
	if m.err != nil {
		b.WriteString(styleWarn.Render("ошибка: "+m.err.Error()) + "\n")
	}
	b.WriteString("\n")
	switch m.screen {
	case screenMenu:
		for i, it := range menu {
			line := " " + it.title
			if it.hint != "" {
				line += styleMuted.Render("  " + it.hint)
			}
			if i == m.cursor {
				b.WriteString(styleCursor.Render("▶") + line + "\n")
			} else {
				b.WriteString(" " + line + "\n")
			}
		}
		b.WriteString("\n" + styleMuted.Render("↑/↓ выбор · enter открыть · r обновить · q выход") + "\n")
	case screenUsers:
		b.WriteString(styleTitle.Render("Пользователи") + "\n")
		if m.loading {
			b.WriteString("загрузка...\n")
		}
		for _, u := range m.users {
			uid := "-"
			if u.UnixUID != nil {
				uid = strconv.Itoa(*u.UnixUID)
			}
			b.WriteString(fmt.Sprintf("  %-16s %-6s %-9s uid=%-6s %s\n", u.Login, u.Role, u.Status, uid, u.Home))
		}
		if len(m.users) == 0 && !m.loading {
			b.WriteString(styleMuted.Render("  нет пользователей") + "\n")
		}
		b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp user add <login> --generate") + styleMuted.Render(" · esc назад") + "\n")
	case screenJobs:
		b.WriteString(styleTitle.Render("Задачи") + "\n")
		for _, j := range m.jobs {
			b.WriteString(fmt.Sprintf("  #%-4d %-16s %-8s %3d%%  %s\n", j.ID, j.Type, j.Status, j.Progress, firstLine(j.Error, j.Message)))
		}
		if len(m.jobs) == 0 && !m.loading {
			b.WriteString(styleMuted.Render("  задач ещё не было") + "\n")
		}
		b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp job show <id>") + styleMuted.Render(" · esc назад") + "\n")
	case screenSites:
		renderSites(&b, m.sites, m.loading)
	case screenPHP:
		renderPHP(&b, m.php)
	case screenDBs:
		renderDBs(&b, m.dbs, m.loading)
	case screenCerts:
		renderCerts(&b, m.certs, m.loading)
	case screenFirewall:
		renderFirewall(&b, m.fw)
	case screenServices:
		b.WriteString(styleTitle.Render("Сервисы") + "\n")
		if m.status != nil {
			for _, s := range m.status.Services {
				state := s.ActiveState
				if state == "active" {
					state = styleKey.Render(state)
				} else {
					state = styleWarn.Render(state)
				}
				b.WriteString(fmt.Sprintf("  %-28s %s (%s)\n", s.Unit, state, s.SubState))
			}
		}
		b.WriteString("\n" + styleMuted.Render("CLI: ") + styleKey.Render("mp service restart <unit>") + styleMuted.Render(" · esc назад") + "\n")
	}
	return tea.NewView(b.String())
}

func firstLine(a, b string) string {
	s := a
	if s == "" {
		s = b
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
