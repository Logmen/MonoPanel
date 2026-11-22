package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
	"monopanel/internal/store"
)

// Users for an administrator; for an ordinary account its own page: cron,
// app services, Valkey, shared folders, tokens, files, password.

func userVals(u *store.User) map[string]string {
	uid := "—"
	if u.UnixUID != nil {
		uid = strconv.Itoa(*u.UnixUID)
	}
	return map[string]string{"login": u.Login, "user": u.Login, "role": u.Role, "status": u.Status, "email": u.Email, "shell": tf(u.Shell), "uid": uid, "home": u.Home}
}

func userActs() []*act {
	l := []string{"login"}
	return []*act{
		{path: "user set", key: "s", args: l, prefill: same("email", "status"), change: true, label: T("Изменить аккаунт", "Change the account")},
		{path: "user set", key: "p", args: l, only: []string{"password", "generate"}, change: true, label: T("Сменить пароль (панель и SFTP/SSH)", "Change the password (panel and SFTP/SSH)")},
		{path: "user set", args: l, flags: map[string]string{"shell": "=true"}, only: []string{"shell"}, admin: true, label: T("Разрешить SSH-shell", "Allow an SSH shell"),
			when: func(v map[string]string) bool { return v["shell"] != "true" }},
		{path: "user set", args: l, flags: map[string]string{"sftp-only": "=true"}, only: []string{"sftp-only"}, admin: true, label: T("Только SFTP", "SFTP only"),
			when: func(v map[string]string) bool { return v["shell"] == "true" }},
		{path: "user totp-reset", key: "T", args: l},
		{path: "user show", args: l, quick: true},
		// As in the web UI, one's own account has no delete (the API refuses it).
		{path: "user rm", key: "d", args: l, when: func(v map[string]string) bool { return v["self"] != "true" }},
	}
}

// markSelf tells the caller's own account apart in the values of a row or
// a page: some actions do not apply to it.
func markSelf(v map[string]string, me string) map[string]string {
	v["self"] = tf(me != "" && v["login"] == me)
	return v
}

func openUsers(m *model, a *area) screen {
	if !m.admin() && m.me != nil {
		return userPage(m, m.me.Login)
	}
	s := &listScreen{screenBase: screenBase{title: a.title},
		cols: []column{{title: T("Логин", "Login")}, {title: T("Роль", "Role")}, {title: T("Статус", "Status")}, {title: "UID", right: true}, {title: T("Доступ", "Access")}, {title: "E-mail"}, {title: T("Домашний каталог", "Home")}}}
	me := meLogin(m)
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, u := range list {
			v := markSelf(userVals(u), me)
			access := "SFTP"
			if u.Shell {
				access = "shell"
			}
			res.rows = append(res.rows, row{cells: []string{u.Login, u.Role, u.Status, v["uid"], access, u.Email, u.Home}, tones: []tone{toneNone, toneNone, stateTone(u.Status), toneMuted}, vals: v})
		}
		return res, nil
	}
	s.acts = append([]*act{{path: "user add", key: "a"}}, userActs()...)
	s.acts = append(s.acts, &act{path: "user list", quick: true})
	s.open = func(m *model, r row) screen { return userPage(m, r.vals["login"]) }
	s.links = []link{
		{key: "C", label: T("весь cron", "all cron"), open: func(m *model) screen { return cronScreen(m, "") }},
		{key: "A", label: T("все app", "all apps"), open: func(m *model) screen { return appsScreen(m, "") }},
		{key: "V", label: T("весь Valkey", "all Valkey"), open: func(m *model) screen { return valkeyScreen(m, "") }},
		allLink(a),
	}
	return s
}

// userPage is one account: its facts and everything that belongs to it.
func userPage(m *model, login string) screen {
	h := &hubScreen{screenBase: screenBase{title: login}, ctx: map[string]string{"login": login, "user": login}}
	me := meLogin(m)
	h.ctx = markSelf(h.ctx, me)
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		u, err := cl.GetUser(ctx, login)
		if err != nil {
			return nil, err
		}
		access := T("только SFTP (chroot в домашний каталог)", "SFTP only (chrooted to the home)")
		if u.Shell {
			access = T("SSH-shell", "SSH shell")
		}
		info := []string{
			fmt.Sprintf(T("Роль: %s · статус: %s · доступ: %s", "Role: %s · status: %s · access: %s"), u.Role, stateTone(u.Status).render(u.Status), access),
			fmt.Sprintf(T("E-mail: %s · UID %s · %s", "E-mail: %s · UID %s · %s"), dash(u.Email), userVals(u)["uid"], dash(u.Home)),
			fmt.Sprintf(T("Создан: %s", "Created: %s"), when(u.CreatedAt.Std())),
		}
		return &result{info: info, vals: markSelf(userVals(u), me)}, nil
	}
	acts := userActs()
	h.entries = []entry{{section: T("Аккаунт", "Account")}}
	for _, a := range acts {
		h.entries = append(h.entries, entry{act: a, key: a.key})
	}
	h.entries = append(h.entries,
		entry{section: T("Ресурсы аккаунта", "The account's resources")},
		entry{label: T("Задания cron", "Cron jobs"), key: "c", open: func(m *model) screen { return cronScreen(m, login) }},
		entry{label: T("App-сервисы", "App services"), key: "a", open: func(m *model) screen { return appsScreen(m, login) }},
		entry{label: "Valkey", key: "v", open: func(m *model) screen { return valkeyScreen(m, login) }},
		entry{label: T("Общие папки", "Shared folders"), key: "h", open: func(m *model) screen { return sharesScreen(m, login) }},
		entry{label: T("API-токены", "API tokens"), key: "t", open: func(m *model) screen {
			// An administrator looks at another account's tokens with --user.
			if m.admin() && (m.me == nil || m.me.Login != login) {
				return tokensScreen(m, login)
			}
			return tokensScreen(m, "")
		}},
		entry{label: T("Файлы", "Files"), key: "f", open: func(m *model) screen { return filesScreen(m, login, "/") }},
	)
	if m.me != nil && m.me.Login == login && m.me.UserID != 0 {
		// The caller's own account: its 2FA is here too, as in the web UI.
		h.entries = append(h.entries, entry{label: T("Двухфакторный вход (2FA)", "Two-factor sign-in (2FA)"), key: "2", open: totpScreen})
	}
	// For an ordinary account this page is the area itself.
	h.entries = append(h.entries, entry{label: T("Все команды раздела", "All commands of the area"), key: "*", open: areaAll})
	return h
}

// meLogin is the caller's login, "" while it is not known.
func meLogin(m *model) string {
	if m.me == nil {
		return ""
	}
	return m.me.Login
}

func cronScreen(m *model, login string) screen {
	title := T("Задания cron", "Cron jobs")
	cols := []column{{title: "ID", right: true}, {title: T("Вкл", "On")}, {title: T("Расписание", "Schedule")}, {title: T("Команда", "Command")}, {title: T("Комментарий", "Comment")}}
	if login == "" {
		title = T("Все задания cron", "All cron jobs")
		cols = append([]column{{title: T("Аккаунт", "Account")}}, cols...)
	}
	// login is in the context even when empty: "add" then needs no row and
	// --user stays to be filled in (an all-accounts view binds it per row).
	s := &listScreen{screenBase: screenBase{title: title}, cols: cols, ctx: map[string]string{"login": login}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		var list []*store.CronJob
		var err error
		if login == "" {
			list, err = cl.AllCronJobs(ctx)
		} else {
			list, err = cl.CronJobs(ctx, login)
		}
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, j := range list {
			state := "on"
			if !j.Enabled {
				state = "off"
			}
			cells := []string{strconv.FormatInt(j.ID, 10), state, j.Schedule, j.Command, j.Comment}
			tones := []tone{toneMuted, stateTone(state)}
			if login == "" {
				cells = append([]string{j.Login}, cells...)
				tones = append([]tone{toneNone}, tones...)
			}
			res.rows = append(res.rows, row{cells: cells, tones: tones, vals: map[string]string{"id": strconv.FormatInt(j.ID, 10), "login": j.Login, "enabled": tf(j.Enabled), "schedule": j.Schedule, "command": j.Command, "comment": j.Comment}})
		}
		return res, nil
	}
	u := map[string]string{"user": "login"}
	id := []string{"id"}
	on := func(v map[string]string) bool { return v["enabled"] == "true" }
	s.acts = []*act{
		{path: "cron add", key: "a", flags: u},
		{path: "cron set", key: "s", args: id, flags: u, prefill: same("schedule", "command", "comment"), change: true},
		{path: "cron disable", key: "e", args: id, flags: u, when: on},
		{path: "cron enable", key: "e", args: id, flags: u, quick: true, when: func(v map[string]string) bool { return !on(v) }},
		{path: "cron rm", key: "d", args: id, flags: u},
		{path: "cron list", quick: true, flags: u},
	}
	s.empty = T("заданий нет — a: добавить", "no cron jobs — a: add one")
	return s
}

func appVals(a *apitypes.AppStatus) map[string]string {
	state := a.App.Status
	if a.Service != nil {
		state = a.Service.ActiveState
	}
	return map[string]string{"login": a.App.Login, "name": a.App.Name, "command": a.App.Command, "workdir": a.App.WorkDir, "env-file": a.App.EnvFile,
		"env": joinList(a.App.Env), "description": a.App.Description, "restart": a.App.Restart, "enabled": tf(a.App.Enabled), "state": state}
}

func appActs() []*act {
	u := map[string]string{"user": "login"}
	n := []string{"name"}
	active := func(v map[string]string) bool { return v["state"] == "active" || v["state"] == "activating" }
	return []*act{
		{path: "app set", key: "s", args: n, flags: u, prefill: same("command", "workdir", "env-file", "env", "description", "restart"), hide: []string{"enable", "disable"}, change: true},
		{path: "app set", args: n, flags: merge(u, map[string]string{"enable": "=true"}), only: []string{"enable"}, quick: true, label: T("Включить автозапуск и запустить", "Enable autostart and start"),
			when: func(v map[string]string) bool { return v["enabled"] != "true" }},
		{path: "app set", args: n, flags: merge(u, map[string]string{"disable": "=true"}), only: []string{"disable"}, label: T("Остановить и выключить автозапуск", "Stop and disable autostart"),
			when: func(v map[string]string) bool { return v["enabled"] == "true" }},
		{path: "app start", key: "S", args: n, flags: u, quick: true, when: func(v map[string]string) bool { return !active(v) }},
		{path: "app stop", key: "S", args: n, flags: u, when: active},
		{path: "app restart", key: "R", args: n, flags: u, quick: true},
		{path: "app logs", key: "l", args: n, flags: u, quick: true},
		{path: "app show", args: n, flags: u, quick: true},
		{path: "app rm", key: "d", args: n, flags: u},
	}
}

func appsScreen(m *model, login string) screen {
	title := T("App-сервисы", "App services")
	if login == "" {
		title = T("Все app-сервисы", "All app services")
	}
	s := &listScreen{screenBase: screenBase{title: title},
		cols: []column{{title: T("Аккаунт", "Account")}, {title: T("Имя", "Name")}, {title: T("Состояние", "State")}, {title: T("Автозапуск", "Enabled")}, {title: T("Команда", "Command")}, {title: T("Ошибка", "Error")}},
		ctx:  map[string]string{"login": login}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		var list []*apitypes.AppStatus
		var err error
		if login == "" {
			list, err = cl.AllApps(ctx)
		} else {
			list, err = cl.Apps(ctx, login)
		}
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, a := range list {
			v := appVals(a)
			state := v["state"]
			if a.Service != nil {
				state += "/" + a.Service.SubState
			}
			on := "on"
			if !a.App.Enabled {
				on = "off"
			}
			res.rows = append(res.rows, row{cells: []string{a.App.Login, a.App.Name, state, on, a.App.Command, firstLine(a.App.LastError, "")},
				tones: []tone{toneNone, toneNone, stateTone(v["state"]), stateTone(on), toneNone, toneErr}, vals: v})
		}
		return res, nil
	}
	s.acts = append([]*act{{path: "app add", key: "a", flags: map[string]string{"user": "login"}}}, appActs()...)
	s.acts = append(s.acts, &act{path: "app list", quick: true, flags: map[string]string{"user": "login"}})
	s.open = func(m *model, r row) screen { return appPage(m, r.vals) }
	s.empty = T("app-сервисов нет — a: создать", "no app services — a: create one")
	return s
}

func appPage(m *model, v map[string]string) screen {
	login, name := v["login"], v["name"]
	h := &hubScreen{screenBase: screenBase{title: name}, ctx: map[string]string{"login": login, "name": name}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		a, err := cl.GetApp(ctx, login, name)
		if err != nil {
			return nil, err
		}
		info := []string{
			fmt.Sprintf(T("Аккаунт: %s · юнит: %s", "Account: %s · unit: %s"), a.App.Login, a.App.Unit()),
			T("Команда: ", "Command: ") + a.App.Command,
			fmt.Sprintf(T("Каталог: %s · перезапуск: %s · автозапуск: %s", "Directory: %s · restart: %s · autostart: %s"), dash(a.App.WorkDir), dash(a.App.Restart), yesNo(a.App.Enabled)),
		}
		if a.App.Description != "" {
			info = append(info, T("Описание: ", "Description: ")+a.App.Description)
		}
		if a.App.EnvFile != "" || len(a.App.Env) > 0 {
			info = append(info, fmt.Sprintf(T("Окружение: %s · файл: %s", "Environment: %s · file: %s"), dash(strings.Join(a.App.Env, " ")), dash(a.App.EnvFile)))
		}
		if a.Service != nil {
			info = append(info, fmt.Sprintf(T("Состояние: %s (%s) · автозапуск юнита: %s · с %s", "State: %s (%s) · unit file: %s · since %s"),
				stateTone(a.Service.ActiveState).render(a.Service.ActiveState), a.Service.SubState, a.Service.UnitFileState, when(a.Service.ActiveSince)))
		}
		if a.App.LastError != "" {
			info = append(info, styleErr.Render(T("Ошибка: ", "Error: ")+firstLine(a.App.LastError, "")))
		}
		return &result{info: info, vals: appVals(a)}, nil
	}
	for _, a := range appActs() {
		h.entries = append(h.entries, entry{act: a, key: a.key})
	}
	return h
}

func valkeyScreen(m *model, login string) screen {
	s := &listScreen{screenBase: screenBase{title: "Valkey"},
		cols: []column{{title: T("Аккаунт", "Account")}, {title: T("Назначение", "Purpose")}, {title: T("Память", "Memory"), right: true}, {title: T("Состояние", "State")}, {title: T("Сокет", "Socket")}}}
	s.ctx = map[string]string{"login": login}
	if login == "" {
		s.title = T("Весь Valkey", "All Valkey")
	}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		var res *apitypes.ValkeyList
		var err error
		if login == "" {
			res, err = cl.ValkeyAll(ctx)
		} else {
			res, err = cl.Valkey(ctx, login)
		}
		if err != nil {
			return nil, err
		}
		out := &result{}
		if res.Engine == "" {
			out.info = []string{styleWarn.Render(T("Valkey не установлен: Расширения → valkey (mp stack install valkey)", "Valkey is not installed: Extensions → valkey (mp stack install valkey)"))}
		} else {
			out.info = []string{fmt.Sprintf(T("Сервер: %s · кеш — для приложений, sessions — для PHP-сессий сайтов (mp site set --sessions valkey)", "Server: %s · cache is for applications, sessions for the sites' PHP sessions (mp site set --sessions valkey)"), res.Engine)}
		}
		for _, v := range res.Instances {
			state := v.Instance.Status
			if v.Service != nil {
				state = v.Service.ActiveState
			}
			out.rows = append(out.rows, row{cells: []string{v.Instance.Login, v.Instance.Purpose, fmt.Sprintf(T("%d МБ", "%d MB"), v.Instance.MemoryMB), state, v.Socket},
				tones: []tone{toneNone, toneNone, toneNone, stateTone(state), toneMuted},
				vals:  map[string]string{"login": v.Instance.Login, "purpose": v.Instance.Purpose, "memory": strconv.Itoa(v.Instance.MemoryMB)}})
		}
		return out, nil
	}
	u := map[string]string{"user": "login"}
	p := []string{"purpose"}
	s.acts = []*act{
		{path: "valkey add", key: "a", flags: u, label: T("Создать экземпляр", "Create an instance")},
		{path: "valkey add", key: "s", args: p, flags: u, prefill: map[string]string{"memory": "memory"}, change: true, label: T("Изменить память", "Change the memory")},
		{path: "valkey restart", key: "R", args: p, flags: u, quick: true},
		{path: "valkey rm", key: "d", args: p, flags: u},
		{path: "valkey list", quick: true, flags: u},
	}
	s.empty = T("экземпляров нет — a: создать", "no instances — a: create one")
	return s
}

func sharesScreen(m *model, login string) screen {
	s := &listScreen{screenBase: screenBase{title: T("Общие папки", "Shared folders")}, ctx: map[string]string{"login": login},
		cols: []column{{title: T("Имя", "Name")}, {title: T("В доме", "In the home")}, {title: T("Папка сайта", "Site folder")}, {title: T("Владелец", "Owner")}, {title: T("Смонтирована", "Mounted")}, {title: "PHP"}, {title: T("Вход", "Entry")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.ListShares(ctx, login)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("Папка сайта другого аккаунта, доступная этому аккаунту по SFTP; выдаёт администратор.", "A folder of another account's site that this account reaches over SFTP; given by an administrator."))}}
		for _, st := range list {
			php := T("разрешён", "allowed")
			if st.Share.NoPHP {
				php = T("запрещён", "denied")
			}
			entryCell := ""
			if st.Share.Entry {
				entryCell = T("да", "yes")
			}
			res.rows = append(res.rows, row{cells: []string{st.Share.Name, st.Where, st.Share.Domain + "/" + st.Share.Path, st.Share.OwnerLogin, yesNo(st.Mounted), php, entryCell},
				vals: map[string]string{"login": login, "name": st.Share.Name, "entry": tf(st.Share.Entry)}})
		}
		return res, nil
	}
	ln := []string{"login", "name"}
	isEntry := func(v map[string]string) bool { return v["entry"] == "true" }
	s.acts = []*act{
		{path: "user share add", key: "a", args: []string{"login"}},
		{path: "user share entry", key: "e", args: ln, quick: true, label: T("Сделать точкой входа SFTP", "Make it the SFTP entry point"), when: func(v map[string]string) bool { return !isEntry(v) }},
		{path: "user share entry", key: "e", args: ln, flags: map[string]string{"off": "=true"}, quick: true, label: T("Входить снова в дом", "Enter the home again"), when: isEntry},
		{path: "user share rm", key: "d", args: ln},
		{path: "user share list", args: []string{"login"}, quick: true},
	}
	s.empty = T("общих папок нет", "no shared folders")
	return s
}

// tokensScreen: the caller's own tokens (login "") or an account's.
func tokensScreen(m *model, login string) screen {
	s := &listScreen{screenBase: screenBase{title: T("API-токены", "API tokens")},
		cols: []column{{title: "ID", right: true}, {title: T("Имя", "Name")}, {title: "Scopes"}, {title: T("Истекает", "Expires")}, {title: T("Использован", "Last used")}},
		ctx:  map[string]string{"login": login}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.ListTokens(ctx, login)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("Токен показывается один раз при создании; scope migrate:user:<логин> ограничивает его переездом, остальные — пометки.", "A token is shown once, when it is created; the scope migrate:user:<login> limits it to a migration, any other is a label."))}}
		for _, t := range list {
			res.rows = append(res.rows, row{cells: []string{strconv.FormatInt(t.ID, 10), t.Name, strings.Join(t.Scopes, ","), whenPtr(t.ExpiresAt), whenPtr(t.LastUsedAt)},
				tones: []tone{toneMuted}, vals: map[string]string{"id": strconv.FormatInt(t.ID, 10)}})
		}
		return res, nil
	}
	u := map[string]string{"user": "login"}
	s.acts = []*act{
		{path: "token create", key: "a", flags: u},
		{path: "token revoke", key: "d", args: []string{"id"}},
		{path: "token list", quick: true, flags: u},
	}
	s.empty = T("токенов нет — a: создать", "no tokens — a: create one")
	return s
}

// usersPicker lists the accounts for an action that needs one (the files of
// an account, a migration grant).
func usersPicker(m *model, title string, pick func(m *model, login string) tea.Cmd) screen {
	s := &listScreen{screenBase: screenBase{title: title}, cols: []column{{title: T("Логин", "Login")}, {title: T("Роль", "Role")}, {title: T("Домашний каталог", "Home")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, u := range list {
			res.rows = append(res.rows, row{cells: []string{u.Login, u.Role, u.Home}, vals: userVals(u)})
		}
		return res, nil
	}
	s.enter = func(m *model, s *listScreen, r row) tea.Cmd { return pick(m, r.vals["login"]) }
	return s
}
