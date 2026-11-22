package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
	"monopanel/internal/store"
)

// Dashboard, jobs, audit log and services: the screens of the areas that
// mostly look at the server.

func tf(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func yesNo(b bool) string {
	if b {
		return T("да", "yes")
	}
	return T("нет", "no")
}

func humanBytes(b uint64) string {
	const unit = 1024.0
	f := float64(b)
	for _, s := range []string{T("Б", "B"), T("КБ", "KB"), T("МБ", "MB"), T("ГБ", "GB"), T("ТБ", "TB")} {
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= unit
	}
	return fmt.Sprintf(T("%.1f ПБ", "%.1f PB"), f)
}

func humanDuration(sec float64) string {
	d := time.Duration(sec) * time.Second
	if days := int(d.Hours()) / 24; days > 0 {
		return fmt.Sprintf(T("%dд %dч", "%dd %dh"), days, int(d.Hours())%24)
	}
	return fmt.Sprintf(T("%dч %dм", "%dh %dm"), int(d.Hours()), int(d.Minutes())%60)
}

func when(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func whenPtr(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return when(*t)
}

func allLink(a *area) link {
	return link{key: "*", label: T("все команды", "all commands"), open: func(m *model) screen {
		s := commandsScreen(m, T("Все команды", "All commands"), m.areaCommands(a))
		s.area = a
		return s
	}}
}

// statusLines is what mp status prints.
func statusLines(st *apitypes.SystemStatus) []string {
	var out []string
	out = append(out, fmt.Sprintf(T("MonoPanel %s, работает %s, схема БД v%d", "MonoPanel %s, up %s, database schema v%d"), st.Panel.Version, humanDuration(st.Panel.UptimeSeconds), st.Panel.SchemaVersion))
	if st.Panel.AgentOK {
		out = append(out, fmt.Sprintf(T("Агент: работает (%s)", "Agent: running (%s)"), st.Panel.AgentVersion))
	} else {
		out = append(out, styleWarn.Render(fmt.Sprintf(T("Агент: НЕДОСТУПЕН: %s", "Agent: UNREACHABLE: %s"), st.Panel.AgentError)))
	}
	if h := st.Host; h != nil {
		out = append(out, fmt.Sprintf(T("Хост: %s · %s · ядро %s · %d CPU · uptime %s", "Host: %s · %s · kernel %s · %d CPU · uptime %s"), h.Hostname, h.Release.PrettyName, h.Kernel, h.CPUs, humanDuration(h.UptimeSeconds)))
		out = append(out, fmt.Sprintf(T("Нагрузка: %.2f %.2f %.2f · память %s из %s", "Load: %.2f %.2f %.2f · memory %s of %s"), h.Load[0], h.Load[1], h.Load[2], humanBytes(h.MemTotalBytes-h.MemAvailableBytes), humanBytes(h.MemTotalBytes)))
		for _, d := range h.Disks {
			out = append(out, fmt.Sprintf(T("Диск %s: свободно %s из %s", "Disk %s: %s free of %s"), d.Mount, humanBytes(d.FreeBytes), humanBytes(d.TotalBytes)))
		}
	}
	if len(st.Services) > 0 {
		parts := make([]string, 0, len(st.Services))
		for _, s := range st.Services {
			parts = append(parts, stateTone(s.ActiveState).render(strings.TrimSuffix(s.Unit, ".service")))
		}
		out = append(out, T("Сервисы: ", "Services: ")+strings.Join(parts, " "))
	}
	out = append(out, fmt.Sprintf(T("Аккаунты: admin=%d user=%d · задачи: queued=%d running=%d done=%d failed=%d", "Accounts: admin=%d user=%d · jobs: queued=%d running=%d done=%d failed=%d"),
		st.Panel.Users["admin"], st.Panel.Users["user"], st.Panel.Jobs["queued"], st.Panel.Jobs["running"], st.Panel.Jobs["done"], st.Panel.Jobs["failed"]))
	return out
}

func openDashboard(m *model, a *area) screen {
	h := &hubScreen{screenBase: screenBase{title: a.title}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.Status(ctx)
		if err != nil {
			return nil, err
		}
		return &result{info: statusLines(st)}, nil
	}
	h.entries = []entry{
		{label: T("Диагностика", "Diagnostics"), key: "d", admin: true, open: openDoctor},
		{label: T("Метрики", "Metrics"), key: "m", open: openMetrics},
		{label: "SELinux", key: "s", admin: true, open: openSELinux},
		{act: &act{path: "status", quick: true}},
		{act: &act{path: "doctor", quick: true}},
		allEntry(a),
	}
	return h
}

func openDoctor(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Диагностика", "Diagnostics")}, cols: []column{{title: T("Итог", "Result")}, {title: T("Проверка", "Check"), max: 34}, {title: T("Подробности", "Details")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		d, err := cl.Doctor(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{d.Summary}}
		for _, c := range d.Checks {
			res.rows = append(res.rows, row{cells: []string{c.Status, c.Name, c.Detail}, tones: []tone{stateTone(c.Status)},
				vals: map[string]string{"action": c.Action, "target": c.Target}})
		}
		return res, nil
	}
	fixable := func(target string) func(v map[string]string) bool {
		return func(v map[string]string) bool {
			if v["action"] != "site.fix" {
				return false
			}
			return (target == "*") == (v["target"] == "*") && v["target"] != ""
		}
	}
	s.acts = []*act{
		{path: "site fix", key: "f", label: T("Починить сайт", "Fix the site"), args: []string{"target"}, quick: true, when: fixable("")},
		{path: "site fix", key: "f", label: T("Починить все сайты", "Fix every site"), args: []string{"target"}, when: fixable("*"), run: fixAllSites},
		{path: "selinux", key: "f", label: T("Включить enforcing", "Switch to enforcing"), args: []string{"=enforcing"}, quick: true, when: func(v map[string]string) bool { return v["action"] == "selinux.enforcing" }},
		{path: "doctor", quick: true},
	}
	return s
}

type runManyMsg struct {
	title string
	jobs  []runJob
	ask   *confirmSpec // asked before the commands run
	err   error
}

// fixAllSites runs mp site fix for every site, one after another, in one
// output view: what the doctor's "fix every site" means. It asks first, as
// mp site fix of one site does.
func fixAllSites(m *model, _ map[string]string) tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		sites, err := cl.Sites(ctx)
		if err != nil {
			return runManyMsg{err: err}
		}
		jobs := make([]runJob, 0, len(sites))
		var shown []string
		for _, s := range sites {
			job := runJob{argv: []string{"site", "fix", s.Domain}, shown: "mp site fix " + shellQuote(s.Domain)}
			jobs = append(jobs, job)
			if len(shown) < 3 {
				shown = append(shown, job.shown)
			}
		}
		line := strings.Join(shown, "; ")
		if more := len(jobs) - len(shown); more > 0 {
			line += fmt.Sprintf(T("; … и ещё %d", "; … and %d more"), more)
		}
		title := T("Починить все сайты", "Fix every site")
		return runManyMsg{title: title, jobs: jobs, ask: &confirmSpec{title: title, line: line, note: siteFixNote()}}
	}
}

func openMetrics(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Метрики", "Metrics")}, filterCmd: "metrics", query: map[string]string{"range": "1h"},
		cols: []column{{title: T("Время", "Time")}, {title: "CPU", right: true}, {title: "LOAD1", right: true}, {title: T("Память", "Memory")}, {title: T("Диск", "Disk")}, {title: "RX", right: true}, {title: "TX", right: true}}}
	s.fetch = func(ctx context.Context, cl *client.Client, q map[string]string) (*result, error) {
		mt, err := cl.Metrics(ctx, q["range"])
		if err != nil {
			return nil, err
		}
		res := &result{}
		var cpu, load, mem []float64
		for i := len(mt.Points) - 1; i >= 0; i-- {
			p := mt.Points[i]
			res.rows = append(res.rows, row{cells: []string{time.Unix(p.TS, 0).Local().Format("01-02 15:04"), fmt.Sprintf("%.1f%%", p.CPU), fmt.Sprintf("%.2f", p.Load1),
				humanBytes(uint64(p.MemUsed)) + "/" + humanBytes(uint64(p.MemTotal)), humanBytes(uint64(p.DiskUsed)) + "/" + humanBytes(uint64(p.DiskTotal)),
				humanBytes(uint64(p.NetRx)), humanBytes(uint64(p.NetTx))}})
		}
		for _, p := range mt.Points {
			cpu, load, mem = append(cpu, p.CPU), append(load, p.Load1), append(mem, float64(p.MemUsed))
		}
		if len(mt.Points) == 0 {
			res.info = []string{T("данных пока нет (сэмплер пишет точку раз в минуту)", "no data yet (the sampler records a point once a minute)")}
			return res, nil
		}
		res.info = []string{
			fmt.Sprintf("CPU   %s", styleKey.Render(sparkline(cpu, 60))),
			fmt.Sprintf("LOAD  %s", styleKey.Render(sparkline(load, 60))),
			fmt.Sprintf(T("ПАМ   %s", "MEM   %s"), styleKey.Render(sparkline(mem, 60))),
			styleMuted.Render(fmt.Sprintf(T("за %s, шаг %d с, %d точек; f — другой период", "over %s, step %d s, %d points; f — another range"), mt.Range, mt.StepSeconds, len(mt.Points))),
		}
		return res, nil
	}
	s.acts = []*act{{path: "metrics", quick: true, flags: map[string]string{"range": "range"}}}
	return s
}

func openSELinux(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: "SELinux"}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.SELinux(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{vals: map[string]string{"mode": st.Mode, "supported": tf(st.Supported), "warning": st.Warning}}
		if !st.Supported {
			res.info = []string{T("SELinux отсутствует на этом сервере", "SELinux is not present on this server")}
			return res, nil
		}
		line := fmt.Sprintf(T("Режим: %s", "Mode: %s"), stateTone(map[string]string{"enforcing": "ok", "permissive": "warn"}[st.Mode]).render(st.Mode))
		if st.Configured != "" && st.Configured != st.Mode {
			line += fmt.Sprintf(T(" (после перезагрузки: %s)", " (after reboot: %s)"), st.Configured)
		}
		res.info = []string{line}
		if st.Mode != "enforcing" || (st.Configured != "" && st.Configured != "enforcing") {
			res.info = append(res.info, styleWarn.Render(T("Внимание: ", "Warning: ")+st.Warning))
		}
		return res, nil
	}
	supported := func(v map[string]string) bool { return v["supported"] == "true" }
	h.entries = []entry{
		{act: &act{path: "selinux", label: T("Включить enforcing", "Switch to enforcing"), args: []string{"=enforcing"}, quick: true}, key: "e",
			when: func(v map[string]string) bool { return supported(v) && v["mode"] != "enforcing" }},
		{act: &act{path: "selinux", label: T("Перевести в permissive", "Switch to permissive"), args: []string{"=permissive"}, quick: true,
			note: func(v map[string]string) string { return v["warning"] }}, key: "p",
			when: func(v map[string]string) bool { return supported(v) && v["mode"] == "enforcing" }},
		{act: &act{path: "selinux", label: T("Показать режим (mp selinux)", "Show the mode (mp selinux)"), quick: true}},
	}
	return h
}

func openJobs(m *model, a *area) screen {
	s := &listScreen{screenBase: screenBase{title: a.title}, filterCmd: "job list", query: map[string]string{"limit": "50", "status": ""},
		cols: []column{{title: "ID", right: true}, {title: T("Тип", "Type")}, {title: T("Статус", "Status")}, {title: "%", right: true}, {title: T("Кто", "By")}, {title: T("Создана", "Created")}, {title: T("Сообщение", "Message")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, q map[string]string) (*result, error) {
		limit, _ := strconv.Atoi(q["limit"])
		list, err := cl.ListJobs(ctx, limit, q["status"])
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, j := range list {
			res.rows = append(res.rows, row{cells: []string{strconv.FormatInt(j.ID, 10), j.Type, j.Status, strconv.Itoa(j.Progress), j.RequestedBy, when(j.CreatedAt.Std()), firstLine(j.Error, j.Message)},
				tones: []tone{toneMuted, toneNone, stateTone(j.Status)}, vals: map[string]string{"id": strconv.FormatInt(j.ID, 10), "status": j.Status}})
		}
		return res, nil
	}
	running := func(v map[string]string) bool {
		return v["status"] == store.JobQueued || v["status"] == store.JobRunning
	}
	s.acts = []*act{
		{path: "job show", key: "o", args: []string{"id"}, quick: true},
		{path: "job wait", key: "w", args: []string{"id"}, quick: true, when: running},
		{path: "job list", quick: true, flags: map[string]string{"limit": "limit", "status": "status"}},
	}
	// Enter follows a running job live and shows a finished one with its log.
	s.enter = func(m *model, s *listScreen, r row) tea.Cmd {
		if running(r.vals) {
			return m.trigger(s.acts[1], s.values(true))
		}
		return m.trigger(s.acts[0], s.values(true))
	}
	s.links = []link{allLink(a)}
	s.empty = T("задач ещё не было", "no jobs yet")
	return s
}

func openAudit(m *model, a *area) screen {
	s := &listScreen{screenBase: screenBase{title: a.title}, filterCmd: "audit", query: map[string]string{"limit": "100", "actor": "", "action": ""},
		cols: []column{{title: T("Время", "Time")}, {title: T("Кто", "Who")}, {title: T("Действие", "Action")}, {title: T("Объект", "Target"), max: 30}, {title: "IP"}, {title: T("Результат", "Result")}, {title: T("Подробности", "Details")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, q map[string]string) (*result, error) {
		limit, _ := strconv.Atoi(q["limit"])
		list, err := cl.ListAudit(ctx, limit, q["actor"], q["action"])
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, e := range list {
			parts := make([]string, 0, len(e.Details))
			for k, v := range e.Details {
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
			sort.Strings(parts)
			res.rows = append(res.rows, row{cells: []string{e.Time.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, e.Target, e.IP, e.Result, strings.Join(parts, " ")},
				tones: []tone{toneMuted, toneNone, toneNone, toneNone, toneMuted, stateTone(e.Result)}, info: parts})
		}
		return res, nil
	}
	s.acts = []*act{{path: "audit", quick: true, flags: map[string]string{"limit": "limit", "actor": "actor", "action": "action"}}}
	s.links = []link{allLink(a)}
	s.empty = T("записей нет", "no entries")
	return s
}

func openServices(m *model, a *area) screen {
	s := &listScreen{screenBase: screenBase{title: a.title},
		cols: []column{{title: T("Юнит", "Unit"), flex: true}, {title: T("Состояние", "Active")}, {title: "SUB"}, {title: T("Автозапуск", "Enabled")}, {title: T("Запущен", "Since")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Services(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, u := range list {
			since := "—"
			if !u.ActiveSince.IsZero() {
				since = when(u.ActiveSince)
			}
			res.rows = append(res.rows, row{cells: []string{u.Unit, u.ActiveState, u.SubState, u.UnitFileState, since},
				tones: []tone{toneNone, stateTone(u.ActiveState), toneMuted, toneMuted}, vals: map[string]string{"unit": u.Unit, "active": u.ActiveState, "enabled": u.UnitFileState},
				info: []string{"LoadState: " + u.LoadState}})
		}
		return res, nil
	}
	active := func(v map[string]string) bool { return v["active"] == "active" || v["active"] == "reloading" }
	enabled := func(v map[string]string) bool { return v["enabled"] == "enabled" }
	u := []string{"unit"}
	s.acts = []*act{
		{path: "service start", key: "s", args: u, quick: true, when: func(v map[string]string) bool { return !active(v) }},
		{path: "service stop", key: "s", args: u, quick: true, when: active},
		{path: "service restart", key: "R", args: u, quick: true},
		{path: "service reload", key: "L", args: u, quick: true, when: active},
		// Without a key of its own: on the unit's page (enter) and in ?.
		{path: "service reload-or-restart", args: u, quick: true},
		{path: "service enable", key: "e", args: u, quick: true, when: func(v map[string]string) bool { return !enabled(v) }},
		{path: "service disable", key: "e", args: u, quick: true, when: enabled},
		{path: "logs", key: "l", args: u},
		{path: "service status", key: "i", args: u, quick: true},
		{path: "service status", key: "c", label: T("Проверить другой юнит", "Check another unit")},
	}
	s.links = []link{allLink(a)}
	return s
}
