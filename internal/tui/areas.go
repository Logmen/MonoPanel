package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

// The menu mirrors the web UI's navigation. Each area owns command groups:
// besides the hand-placed actions of its screens, every command of those
// groups is in the area's "all commands" list, and groups no area owns land
// in "Other". So nothing the CLI can do is missing from the menus.

type groupRef struct {
	path  string
	admin bool // only administrators get this group here
}

type area struct {
	id     string
	title  string
	user   bool // ordinary accounts see the area too (as in the web UI)
	groups []groupRef
	open   func(m *model, a *area) screen
}

func g(path string) groupRef      { return groupRef{path: path} }
func gAdmin(path string) groupRef { return groupRef{path: path, admin: true} }

// areaDefs are the areas in the order of the web navigation.
func areaDefs(admin bool) []*area {
	users := T("Пользователи", "Users")
	if !admin {
		users = T("Аккаунт", "Account")
	}
	return []*area{
		{id: "dashboard", title: T("Дашборд", "Dashboard"), user: true, groups: []groupRef{g("status"), g("metrics"), gAdmin("doctor"), gAdmin("selinux")}, open: openDashboard},
		{id: "sites", title: T("Сайты", "Sites"), user: true, groups: []groupRef{g("site"), g("cms")}, open: openSites},
		{id: "users", title: users, user: true, groups: []groupRef{g("user"), g("cron"), g("app"), g("valkey"), g("token")}, open: openUsers},
		{id: "php", title: "PHP", groups: []groupRef{g("php")}, open: openPHP},
		{id: "stack", title: T("Расширения", "Extensions"), groups: []groupRef{g("stack")}, open: openStack},
		{id: "databases", title: T("Базы данных", "Databases"), user: true, groups: []groupRef{g("db")}, open: openDatabases},
		{id: "mail", title: T("Почта", "Mail"), user: true, groups: []groupRef{g("mail")}, open: openMail},
		{id: "files", title: T("Файлы", "Files"), user: true, groups: []groupRef{g("files")}, open: openFiles},
		{id: "ssl", title: "SSL", groups: []groupRef{g("ssl"), g("web"), g("dns-provider")}, open: openSSL},
		{id: "jobs", title: T("Задачи", "Jobs"), user: true, groups: []groupRef{g("job")}, open: openJobs},
		{id: "audit", title: T("Журнал", "Audit log"), user: true, groups: []groupRef{g("audit")}, open: openAudit},
		{id: "firewall", title: "Firewall", groups: []groupRef{g("firewall")}, open: openFirewall},
		{id: "services", title: T("Службы", "Services"), groups: []groupRef{g("service"), g("logs")}, open: openServices},
		{id: "backups", title: T("Бэкапы", "Backups"), groups: []groupRef{g("backup")}, open: openBackups},
		{id: "migrate", title: T("Перенос", "Migration"), groups: []groupRef{g("migrate")}, open: openMigrate},
		// whoami (GET /auth/me): who the caller is, next to its tokens and 2FA.
		{id: "settings", title: T("Настройки", "Settings"), user: true, groups: []groupRef{g("token"), g("totp"), g("whoami"), gAdmin("webhook"), gAdmin("update"), gAdmin("config")}, open: openSettings},
	}
}

func (a *area) visible(admin bool) bool { return admin || a.user }

// otherCommands are the top-level commands no area owns: a newer mp may
// bring groups the menus do not know yet.
func otherCommands(root *cobra.Command, opts Options) []*cobra.Command {
	owned := map[string]bool{}
	for _, a := range areaDefs(true) {
		for _, gr := range a.groups {
			top, _, _ := strings.Cut(gr.path, " ")
			owned[top] = true
		}
	}
	var out []*cobra.Command
	subs := append([]*cobra.Command{}, root.Commands()...)
	sort.SliceStable(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
	for _, c := range subs {
		if owned[c.Name()] || !offered(c, opts) {
			continue
		}
		out = append(out, runnables(c, opts)...)
	}
	return out
}

// areaByID finds an area for a screen opened from another one (the files of
// a site belong to Files).
func areaByID(m *model, id string) *area {
	for _, a := range areaDefs(m.admin()) {
		if a.id == id {
			return a
		}
	}
	return nil
}

func otherArea() *area {
	return &area{id: "other", title: T("Другое", "Other"), user: true}
}

// commandsOf lists the commands of an area for a role.
func commandsOf(root *cobra.Command, opts Options, a *area, admin bool) []*cobra.Command {
	if a.id == "other" {
		return filterAdmin(otherCommands(root, opts), admin)
	}
	seen := map[string]bool{}
	var out []*cobra.Command
	for _, gr := range a.groups {
		if gr.admin && !admin {
			continue
		}
		c := findCmd(root, gr.path)
		if c == nil {
			continue
		}
		for _, x := range runnables(c, opts) {
			p := cmdPath(x)
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, x)
		}
	}
	return filterAdmin(out, admin)
}

func filterAdmin(list []*cobra.Command, admin bool) []*cobra.Command {
	if admin {
		return list
	}
	out := list[:0:0]
	for _, c := range list {
		if !adminOnly(cmdPath(c)) {
			out = append(out, c)
		}
	}
	return out
}

func (m *model) areaCommands(a *area) []*cobra.Command {
	return commandsOf(m.root, m.opts, a, m.admin())
}

// mainMenu lists the areas the caller's role sees.
func (m *model) mainMenu() *hubScreen {
	admin := m.admin()
	h := &hubScreen{screenBase: screenBase{title: "MonoPanel"}}
	h.info = []string{styleMuted.Render(T("Разделы — как в веб-интерфейсе. ? — действия и все команды раздела, : — любая команда mp.", "The areas of the web UI. ? — the actions and all commands of an area, : — any mp command."))}
	if len(m.leftovers) > 0 {
		h.info = append(h.info, styleWarn.Render(fmt.Sprintf(T("Правка файла из прошлого запуска могла не сохраниться — копия лежит здесь: %s (удалится через сутки).",
			"An edit from an earlier run may not have been saved — the copy is here: %s (removed after a day)."), strings.Join(m.leftovers, ", "))))
	}
	for _, a := range areaDefs(admin) {
		if !a.visible(admin) {
			continue
		}
		ar := a
		h.entries = append(h.entries, entry{label: ar.title, open: func(m *model) screen {
			s := ar.open(m, ar)
			s.base().area = ar
			return s
		}})
	}
	if ar := otherArea(); len(m.areaCommands(ar)) > 0 {
		h.entries = append(h.entries, entry{label: ar.title, open: func(m *model) screen {
			s := commandsScreen(m, ar.title, m.areaCommands(ar))
			s.area = ar
			return s
		}})
	}
	return h
}

// commandsScreen lists commands to run: an area's "all commands", a group
// opened from the palette, the "Other" area.
func commandsScreen(m *model, title string, cmds []*cobra.Command) *listScreen {
	s := &listScreen{screenBase: screenBase{title: title}, cols: []column{{title: T("Команда", "Command"), min: 16, max: 46}, {title: T("Что делает", "What it does")}}}
	s.res = &result{}
	for _, c := range cmds {
		s.res.rows = append(s.res.rows, row{cells: []string{useLine(c), c.Short}, vals: map[string]string{"path": cmdPath(c)}})
	}
	s.empty = T("команд нет", "no commands")
	s.enter = func(m *model, s *listScreen, r row) tea.Cmd {
		p := r.vals["path"]
		return m.trigger(&act{path: p, quick: readOnly(p)}, nil)
	}
	return s
}

// allEntry is the hub entry that opens the area's command list.
func allEntry(a *area) entry {
	return entry{label: T("Все команды раздела", "All commands of the area"), key: "*", open: func(m *model) screen {
		s := commandsScreen(m, T("Все команды", "All commands"), m.areaCommands(a))
		s.area = a
		return s
	}}
}

// areaAll opens the command list of the area the current screen belongs to:
// for screens that are an area's first page for one role and a sub-page for
// the other (an account's page, the file browser).
func areaAll(m *model) screen {
	ar := m.top().base().area
	if ar == nil {
		return nil
	}
	s := commandsScreen(m, T("Все команды", "All commands"), m.areaCommands(ar))
	s.area = ar
	return s
}
