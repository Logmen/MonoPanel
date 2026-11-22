package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"monopanel/internal/apitypes"
	"monopanel/internal/buildinfo"
	"monopanel/internal/client"
)

// Settings: the caller's tokens and 2FA; for an administrator webhooks,
// updates, the panel's own configuration and its templates.

func openSettings(m *model, a *area) screen {
	h := &hubScreen{screenBase: screenBase{title: a.title}}
	panel := ""
	if m.status != nil {
		panel = m.status.Panel.Version
	}
	// root over the socket has no panel account, so no 2FA of its own.
	account := m.me == nil || m.me.UserID != 0
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		info := []string{fmt.Sprintf(T("Панель: %s · этот mp: %s", "Panel: %s · this mp: %s"), dash(panel), buildinfo.String())}
		if !account {
			info = append(info, styleMuted.Render(T("root на сокете — не аккаунт панели, своей 2FA у него нет; 2FA аккаунта сбрасывается на его странице (Пользователи).", "root over the socket is not a panel account and has no 2FA of its own; an account's 2FA is reset on its page (Users).")))
		} else if st, err := cl.TOTPStatus(ctx); err == nil {
			info = append(info, fmt.Sprintf(T("Двухфакторный вход: %s", "Two-factor sign-in: %s"), onOff(st.Enabled)))
		}
		return &result{info: info}, nil
	}
	h.entries = []entry{
		{label: T("API-токены", "API tokens"), key: "t", open: func(m *model) screen { return tokensScreen(m, "") }},
		{label: T("Двухфакторный вход (2FA)", "Two-factor sign-in (2FA)"), key: "2", open: totpScreen, when: func(map[string]string) bool { return account }},
		// Shown once this mp has the command (an older one has not).
		{act: &act{path: "whoami", quick: true}, key: "i"},
		{label: "Webhooks", key: "w", admin: true, open: webhooksScreen},
		{label: T("Обновления панели", "Panel updates"), key: "u", admin: true, open: updatesScreen},
		{label: T("Конфигурация панели", "Panel configuration"), key: "c", admin: true, open: panelConfigScreen},
		{label: T("Шаблоны конфигураций", "Configuration templates"), key: "p", admin: true, open: templatesScreen},
		allEntry(a),
	}
	return h
}

func onOff(b bool) string {
	if b {
		return styleKey.Render(T("включён", "on"))
	}
	return styleMuted.Render(T("выключен", "off"))
}

func totpScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: "2FA"}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.TOTPStatus(ctx)
		if err != nil {
			return nil, err
		}
		return &result{info: []string{
			fmt.Sprintf(T("Двухфакторный вход: %s", "Two-factor sign-in: %s"), onOff(st.Enabled)),
			styleMuted.Render(T("Включение: s — секрет и QR-код (в терминале), затем e — код из приложения.", "To switch it on: s — the secret and a QR code (in the terminal), then e — a code from the app.")),
		}, vals: map[string]string{"enabled": tf(st.Enabled)}}, nil
	}
	on := func(v map[string]string) bool { return v["enabled"] == "true" }
	h.entries = []entry{
		{act: &act{path: "totp setup", tty: true}, key: "s", when: func(v map[string]string) bool { return !on(v) }},
		{act: &act{path: "totp enable"}, key: "e", when: func(v map[string]string) bool { return !on(v) }},
		{act: &act{path: "totp disable"}, key: "d", when: on},
		{act: &act{path: "totp status", quick: true}},
	}
	return h
}

func webhooksScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: "Webhooks"},
		cols: []column{{title: "ID"}, {title: "URL"}, {title: T("События", "Events")}, {title: T("Создан", "Created")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Webhooks(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("Подпись HMAC-SHA256 в заголовке; секрет показывается при создании.", "An HMAC-SHA256 signature in a header; the secret is shown on creation."))}}
		for _, h := range list {
			res.rows = append(res.rows, row{cells: []string{h.ID, h.URL, strings.Join(h.Events, ","), h.CreatedAt}, tones: []tone{toneMuted}, vals: map[string]string{"id": h.ID}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "webhook add", key: "a"},
		{path: "webhook test", key: "t", args: []string{"id"}, quick: true},
		{path: "webhook rm", key: "d", args: []string{"id"}},
		{path: "webhook list", quick: true},
	}
	s.empty = T("подписчиков нет — a: добавить", "no subscribers — a: add one")
	return s
}

func updateLines(st *apitypes.UpdateStatus) []string {
	out := []string{fmt.Sprintf(T("Версия: %s", "Version: %s"), st.Current)}
	if st.Settings.Repo == "" {
		out = append(out, styleWarn.Render(T("Репозиторий не настроен", "The repository is not configured")))
	} else {
		where := st.Settings.Repo
		if st.Settings.API != "" {
			where += " @ " + st.Settings.API
		}
		if st.Settings.RepoBuiltIn {
			where += T(", из сборки", ", built-in")
		}
		out = append(out, fmt.Sprintf(T("Репозиторий: %s (%s), токен: %s", "Repository: %s (%s), token: %s"), where, st.Settings.Channel, yesNo(st.Settings.HasToken)))
	}
	if st.Latest != "" {
		if st.Available {
			out = append(out, styleKey.Render(fmt.Sprintf(T("Доступна: %s (%s)", "Available: %s (%s)"), st.Latest, whenPtr(st.PublishedAt))))
		} else {
			out = append(out, fmt.Sprintf(T("Последняя: %s — установлена свежая версия", "Latest: %s — up to date"), st.Latest))
		}
	}
	every := fmt.Sprintf(T("проверка каждые %d ч", "checked every %d h"), st.Settings.CheckHours)
	if st.Settings.CheckHours == 0 {
		every = T("проверка выключена", "checks off")
	}
	auto := T("установка по запросу", "installed on request")
	if st.Settings.AutoApply {
		auto = T("установка автоматически", "installed automatically")
	}
	key := T("подпись не проверяется", "no signature check")
	switch st.KeySource {
	case "builtin":
		key = T("подпись: встроенный ключ", "signature: built-in key")
	case "config":
		key = T("подпись: ключ из config.yaml (mp update trust)", "signature: key from config.yaml (mp update trust)")
	}
	out = append(out, every+" · "+auto+" · "+key)
	if st.CheckedAt != nil {
		out = append(out, T("Проверено: ", "Checked: ")+whenPtr(st.CheckedAt))
	}
	if st.LastError != "" {
		out = append(out, styleErr.Render(T("Ошибка: ", "Error: ")+st.LastError))
	}
	if a := st.LastAttempt; a != nil {
		line := fmt.Sprintf(T("Последняя установка: %s → %s, %s", "Last install: %s → %s, %s"), a.From, a.To, a.Status)
		if a.Error != "" {
			line += ": " + a.Error
		}
		out = append(out, line)
		if a.Reinstall {
			out = append(out, styleWarn.Render(fmt.Sprintf(T("%s уже числится в базе пакетов, а работает прежний бинарник: a (mp update apply) установит её ещё раз, сама панель её не повторит",
				"The package database lists %s already while the previous binary runs: a (mp update apply) installs it once more, the panel does not retry it on its own"), a.To)))
		}
	}
	return out
}

func updatesScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: T("Обновления", "Updates")}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.UpdateStatus(ctx)
		if err != nil {
			return nil, err
		}
		return &result{info: updateLines(st), vals: map[string]string{"repo": st.Settings.Repo, "api": st.Settings.API, "channel": st.Settings.Channel,
			"check-hours": strconv.Itoa(st.Settings.CheckHours), "auto-apply": tf(st.Settings.AutoApply), "available": tf(st.Available), "latest": st.Latest}}, nil
	}
	h.entries = []entry{
		{act: &act{path: "update check", quick: true}, key: "c"},
		{act: &act{path: "update apply", note: func(map[string]string) string {
			return T("Панель перезапустится; задачи в работе дождутся её.", "The panel restarts; running jobs wait for it.")
		}}, key: "a"},
		{act: &act{path: "update settings", prefill: same("repo", "api", "channel", "check-hours", "auto-apply"), change: true}, key: "s"},
		{act: &act{path: "update trust"}, key: "t"},
		{act: &act{path: "update", quick: true, label: T("Состояние (mp update)", "Status (mp update)")}},
	}
	return h
}

func panelConfigScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: T("Конфигурация панели", "Panel configuration")}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		c, err := cl.PanelConfig(ctx)
		if err != nil {
			return nil, err
		}
		info := []string{
			T("Файл: ", "File: ") + c.Path,
			fmt.Sprintf("web.hostname: %s · web.listen: %s", dash(c.Hostname), dash(c.Listen)),
			fmt.Sprintf("log.level: %s · log.format: %s · jobs.workers: %d", c.LogLevel, c.LogFormat, c.JobsWorkers),
			fmt.Sprintf(T("Данные: %s · шаблоны: %s · ключ релизов задан: %s", "Data: %s · templates: %s · release key pinned: %s"), c.DataDir, c.TemplatesDir, yesNo(c.KeyPinned)),
		}
		if c.RestartPending {
			info = append(info, styleWarn.Render(T("Файл изменён: изменения вступят в силу после перезапуска monopanel-api.", "The file changed: the changes take effect once monopanel-api restarts.")))
		}
		return &result{info: info}, nil
	}
	h.entries = []entry{
		{act: &act{path: "config set", note: func(map[string]string) string {
			return T("Новый web.listen или web.hostname может перенести панель на другой адрес.", "A new web.listen or web.hostname may move the panel to another address.")
		}}, key: "s"},
		{act: &act{path: "config show", quick: true}},
		{label: T("Шаблоны", "Templates"), key: "p", open: templatesScreen},
		{act: &act{path: "config templates", quick: true}},
	}
	return h
}

func templatesScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Шаблоны", "Templates")},
		cols: []column{{title: T("Шаблон", "Template")}, {title: T("Переопределён", "Overridden")}, {title: T("Файл", "File")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Templates(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(fmt.Sprintf(T("Файл с тем же именем в %s заменяет встроенный шаблон.", "A file of the same name in %s replaces the built-in template."), list.Dir))}}
		for _, t := range list.Templates {
			ov := ""
			if t.Override {
				ov = T("да", "yes")
			}
			res.rows = append(res.rows, row{cells: []string{t.Name, ov, t.Path}, tones: []tone{toneNone, toneWarn, toneMuted}})
		}
		return res, nil
	}
	s.acts = []*act{{path: "config templates", quick: true}}
	return s
}
