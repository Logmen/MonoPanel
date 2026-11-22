package tui

import (
	"context"
	"fmt"
	"strconv"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
)

// runsReplacedBinary tells that a component's service, or one of its
// accounts' Apaches, still runs the binary an upgrade replaced (the
// distribution's apache2, an upgrade whose check failed).
func runsReplacedBinary(c apitypes.StackComponent) bool {
	return (c.Service != nil && c.Service.BinaryReplaced) || (c.Instances != nil && c.Instances.Replaced > 0)
}

// upgradeComponentOf is how mp stack upgrade names a stack row's component,
// "" for a row it does not upgrade: as mp stack list says it.
func upgradeComponentOf(c apitypes.StackComponent) string { return c.UpgradeComponent() }

// stackUpgradeNote: what an upgrade installs and restarts, as the web UI's
// dialog says it.
func stackUpgradeNote(name string) string {
	switch name {
	case "db":
		return T("Пакеты сервера баз данных обновятся из репозитория вендора, конфигурация проверится новой версией, и сервер перезапустится: на это время сайты останутся без баз.",
			"The database server's packages are upgraded from the vendor's repository, the configuration is checked with the new version and the server restarts: the sites lose their databases meanwhile.")
	case "nginx":
		return T("nginx обновится с nginx.org; nginx -t проверит конфигурацию новой версией, и nginx перечитает её (работающий бинарник пакет подменяет сам). Если проверка не пройдёт, nginx продолжит работать как работал.",
			"nginx is upgraded from nginx.org; nginx -t checks the configuration with the new version and nginx reloads it (the package swaps the running binary itself). If the check fails, nginx runs on as it was.")
	case "php":
		return T("Пакеты установленных веток PHP обновятся; php-fpm каждой обновлённой ветки после php-fpm -t перезапустится, и запросы, которые выполняются в этот момент, оборвутся.",
			"The packages of the installed PHP branches are upgraded; the php-fpm of every upgraded branch restarts after php-fpm -t, which cuts off the requests running at that moment.")
	case "apache":
		return T("Пакеты Apache обновятся; Apache каждого аккаунта после apache2 -t мягко перезапустится, а тот, что работает на заменённом бинарнике, перезапустится полностью — его сайты на это время ответят 502.",
			"Apache's packages are upgraded; each account's Apache gets a graceful restart after apache2 -t, and one that runs the replaced binary a full restart, its sites answering 502 meanwhile.")
	}
	return T("Обновятся пакеты nginx, PHP и Apache, для которых в их репозиториях есть новые версии. Конфигурация каждой службы проверится новой версией, затем nginx перечитает её, а php-fpm и Apache аккаунтов перезапустятся. Если проверка не пройдёт, служба продолжит работать как работала. Сервер баз данных не затрагивается: mp stack upgrade db.",
		"The packages of nginx, PHP and Apache with newer versions in their repositories are upgraded. Each service's configuration is checked with the new version, then nginx reloads, and php-fpm and the accounts' Apaches restart. If a check fails, the service runs on as it was. The database server is not touched: mp stack upgrade db.")
}

// autoUpgradeLines describe the daily upgrade and the last upgrade.
func autoUpgradeLines(st *apitypes.StackAutoUpgrade) []string {
	var out []string
	if st.Enabled {
		line := fmt.Sprintf(T("Ежедневное обновление nginx, PHP и Apache: включено, в %02d:00 (%s)", "Daily upgrade of nginx, PHP and Apache: on, at %02d:00 (%s)"), st.Hour, st.Timezone)
		if st.NextRun != nil {
			line += " · " + fmt.Sprintf(T("следующее: %s", "next: %s"), st.NextRun.Format("2006-01-02 15:04"))
		}
		out = append(out, line)
	} else {
		out = append(out, styleWarn.Render(fmt.Sprintf(T("Ежедневное обновление nginx, PHP и Apache выключено (час %02d:00, %s): исправления ставятся только вручную.", "The daily upgrade of nginx, PHP and Apache is off (hour %02d:00, %s): fixes are installed by hand only."), st.Hour, st.Timezone)))
	}
	if l := st.Last; l != nil {
		line := fmt.Sprintf(T("Последнее обновление: %s, задача #%d", "Last upgrade: %s, job #%d"), whenPtr(&l.At), l.JobID)
		switch {
		case l.Error != "":
			out = append(out, styleErr.Render(line+": "+l.Error))
		case len(l.Upgraded) == 0:
			out = append(out, line+T(", обновлять было нечего", ", nothing to upgrade"))
		default:
			out = append(out, line)
			for _, u := range l.Upgraded {
				out = append(out, "  "+u)
			}
		}
	}
	return out
}

func autoUpgradeScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: T("Ежедневное обновление", "Daily upgrade")}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.StackAutoUpgrade(ctx)
		if err != nil {
			return nil, err
		}
		return &result{info: autoUpgradeLines(st), vals: map[string]string{"enabled": tf(st.Enabled), "hour": strconv.Itoa(st.Hour)}}, nil
	}
	on := func(v map[string]string) bool { return v["enabled"] == "true" }
	h.entries = []entry{
		{act: &act{path: "stack auto-upgrade", label: T("Выключить", "Switch off"), flags: map[string]string{"off": "=true"}, only: []string{"off"}}, key: "o", when: on},
		{act: &act{path: "stack auto-upgrade", label: T("Включить", "Switch on"), flags: map[string]string{"on": "=true"}, only: []string{"on"}, quick: true}, key: "n",
			when: func(v map[string]string) bool { return !on(v) }},
		{act: &act{path: "stack auto-upgrade", label: T("Сменить час", "Change the hour"), prefill: same("hour"), only: []string{"hour"}, change: true}, key: "h"},
		{act: &act{path: "stack upgrade", label: T("Обновить сейчас: nginx, PHP, Apache", "Upgrade now: nginx, PHP, Apache")}, key: "u"},
		{act: &act{path: "stack auto-upgrade", label: T("Показать (mp stack auto-upgrade)", "Show (mp stack auto-upgrade)"), quick: true, hide: []string{"on", "off", "hour"}}},
	}
	return h
}
