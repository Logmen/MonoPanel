package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/client"
	"monopanel/internal/store"
)

// PHP, extensions, databases, SSL, firewall, backups and migration.

func openPHP(m *model, a *area) screen {
	s := &listScreen{screenBase: screenBase{title: a.title},
		cols: []column{{title: T("Ветка", "Version")}, {title: "Upstream"}, {title: T("Состояние", "State")}, {title: T("Пакет", "Package")}, {title: T("Расширений", "Extensions"), right: true}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		v, err := cl.PHPVersions(ctx)
		if err != nil {
			return nil, err
		}
		installed := map[string]*store.PHPVersion{}
		for _, p := range v.Installed {
			installed[p.Version] = p
		}
		res := &result{info: []string{styleMuted.Render(T("enter — расширения ветки · I — php.ini для всех сайтов сервера", "enter — the branch's extensions · I — php.ini for every site on the server"))}}
		for _, av := range v.Available {
			p := installed[av.Version]
			state, pkg, ext := T("не установлена", "not installed"), "—", "—"
			tn := toneMuted
			if p != nil {
				state, pkg, ext, tn = p.Status, p.PackageVersion, strconv.Itoa(len(p.Extensions)), stateTone(p.Status)
				if p.IsDefault {
					state += T(" · по умолчанию", " · default")
				}
				if p.LastError != "" {
					state += ": " + firstLine(p.LastError, "")
					tn = toneErr
				}
			} else if !av.Available {
				state = T("недоступна: ", "unavailable: ") + av.Note
			}
			res.rows = append(res.rows, row{cells: []string{av.Version, av.Support, state, pkg, ext}, tones: []tone{toneNone, toneMuted, tn},
				vals: map[string]string{"version": av.Version, "installed": tf(p != nil), "available": tf(av.Available)}})
		}
		return res, nil
	}
	ver := []string{"version"}
	s.acts = []*act{
		{path: "php install", key: "i", args: ver, when: func(v map[string]string) bool { return v["installed"] != "true" && v["available"] == "true" }},
		{path: "php remove", key: "d", args: ver, when: func(v map[string]string) bool { return v["installed"] == "true" }},
		{path: "php ext list", key: "e", args: ver, quick: true, when: func(v map[string]string) bool { return v["installed"] == "true" }},
		{path: "php list", quick: true, flags: map[string]string{"available": "=true"}},
	}
	s.open = func(m *model, r row) screen {
		if r.vals["installed"] != "true" {
			return rowPage(m, s, r)
		}
		return phpExtScreen(m, r.vals["version"])
	}
	s.links = []link{{key: "I", label: "php.ini", open: phpIniScreen}, allLink(a)}
	return s
}

func phpExtScreen(m *model, version string) screen {
	s := &listScreen{screenBase: screenBase{title: "PHP " + version}, ctx: map[string]string{"version": version},
		cols: []column{{title: T("Расширение", "Extension")}, {title: T("Состояние", "State")}, {title: ""}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		st, err := cl.PHPExtensions(ctx, version)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("Расширение включается для всей ветки: php-fpm у версии один, он перезапустится.", "An extension is switched for the whole branch: the version has one php-fpm, and it restarts."))}}
		for _, e := range st.Extensions {
			state, note := T("выключено", "disabled"), ""
			tn := toneMuted
			if e.Enabled {
				state, tn = T("включено", "enabled"), toneOK
			}
			if !e.Installed {
				state, note = T("не установлено", "not installed"), T("включение поставит ", "enabling installs ")+e.Package
			}
			if e.Critical {
				note = T("нужно типовому сайту", "needed by a typical site")
			}
			res.rows = append(res.rows, row{cells: []string{e.Name, state, note}, tones: []tone{toneNone, tn, toneMuted},
				vals: map[string]string{"name": e.Name, "enabled": tf(e.Enabled), "critical": tf(e.Critical)}})
		}
		return res, nil
	}
	vn := []string{"version", "name"}
	s.acts = []*act{
		{path: "php ext enable", key: "e", args: vn, quick: true, when: func(v map[string]string) bool { return v["enabled"] != "true" }},
		{path: "php ext disable", key: "e", args: vn, when: func(v map[string]string) bool { return v["enabled"] == "true" }},
		{path: "php ext list", args: []string{"version"}, quick: true},
	}
	return s
}

func phpIniScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("php.ini сервера", "Server php.ini")},
		cols: []column{{title: T("Ключ", "Key")}, {title: T("Значение", "Value")}, {title: T("Источник", "Source")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		g, err := cl.PHPSettings(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{
			styleMuted.Render(T("Глобальный слой: пресет и значения сайта перекрывают его. s — изменить, a — задать новый ключ, u — вернуть значение панели.", "The global layer: a preset and the site's own values override it. s — change, a — set a new key, u — back to the panel's value.")),
			styleMuted.Render(T("Допустимые ключи: ", "Allowed keys: ") + strings.Join(g.Allowed, ", ")),
		}}
		for _, v := range g.Values {
			src := T("панель", "panel")
			if v.Source == "global" {
				src = T("глобально", "global")
			}
			res.rows = append(res.rows, row{cells: []string{v.Key, v.Value, src}, tones: []tone{toneNone, toneNone, toneMuted},
				vals: map[string]string{"key": v.Key, "kv": v.Key + "=" + v.Value, "source": v.Source}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "php ini set", key: "s", args: []string{"kv"}, label: T("Изменить значение", "Change the value")},
		{path: "php ini set", key: "a", label: T("Задать ключ", "Set a key")},
		{path: "php ini unset", key: "u", args: []string{"key"}, when: func(v map[string]string) bool { return v["source"] == "global" }},
		{path: "php ini", quick: true},
	}
	return s
}

func openStack(m *model, a *area) screen {
	s := &listScreen{screenBase: screenBase{title: a.title},
		cols: []column{{title: T("Компонент", "Component")}, {title: T("Вид", "Kind")}, {title: T("Версия", "Version")}, {title: T("Состояние", "State")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Stack(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("PHP ставится в разделе PHP · m — memcached, p — реальный IP за прокси", "PHP is installed in the PHP area · m — memcached, p — the real IP behind proxies"))}}
		for _, c := range list {
			state, ver := T("не установлен", "not installed"), "—"
			tn := toneMuted
			if c.Installed {
				state, ver, tn = T("установлен", "installed"), c.Version, toneOK
				if c.Service != nil {
					state += " · " + c.Service.ActiveState
					if c.Service.ActiveState != "active" {
						tn = toneWarn
					}
				}
				// Apache: one per account with sites in apache mode.
				if in := c.Instances; in != nil && in.Total > 0 {
					state += " · " + fmt.Sprintf(T("запущено %d из %d", "%d of %d running"), in.Running, in.Total)
					if in.Running < in.Total {
						tn = toneWarn
					}
				}
			}
			res.rows = append(res.rows, row{cells: []string{c.Name, c.Kind, ver, state}, tones: []tone{toneNone, toneMuted, toneNone, tn},
				vals: map[string]string{"name": c.Name, "installed": tf(c.Installed), "removable": tf(c.Removable)}})
		}
		return res, nil
	}
	n := []string{"name"}
	s.acts = []*act{
		{path: "stack install", key: "i", args: n, when: func(v map[string]string) bool { return v["installed"] != "true" || v["name"] == "composer" }},
		// As the web UI's "Reinstall": the way to repair a component; the
		// confirmation says what running the installation again does.
		{path: "stack install", key: "i", args: n, label: T("Переустановить", "Reinstall"), when: func(v map[string]string) bool { return v["installed"] == "true" && v["name"] != "composer" }},
		{path: "stack remove", key: "d", args: n, when: func(v map[string]string) bool { return v["installed"] == "true" && v["removable"] == "true" }},
		{path: "stack list", quick: true},
	}
	s.links = []link{{key: "m", label: "memcached", open: memcachedScreen}, {key: "p", label: "real-ip", open: realIPScreen}, allLink(a)}
	return s
}

func memcachedScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: "memcached"}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.MemcachedSettings(ctx)
		if err != nil {
			return nil, err
		}
		state := T("не установлен", "not installed")
		if st.Installed {
			state = T("установлен, 127.0.0.1:11211", "installed, 127.0.0.1:11211")
		}
		return &result{info: []string{fmt.Sprintf(T("memcached: %s · память %d МБ · соединений %d", "memcached: %s · memory %d MB · connections %d"), state, st.MemoryMB, st.MaxConnections)},
			// name and installed: installing it again says it runs again.
			vals: map[string]string{"memory-mb": strconv.Itoa(st.MemoryMB), "max-conn": strconv.Itoa(st.MaxConnections), "name": "memcached", "installed": tf(st.Installed)}}, nil
	}
	h.entries = []entry{
		{act: &act{path: "stack memcached", label: T("Изменить и применить", "Change and apply"), prefill: same("memory-mb", "max-conn"), change: true}, key: "s"},
		{act: &act{path: "stack install", label: T("Установить memcached", "Install memcached"), args: []string{"=memcached"}}},
		{act: &act{path: "stack memcached", label: T("Показать (mp stack memcached)", "Show (mp stack memcached)"), quick: true, hide: []string{"memory-mb", "max-conn"}}},
	}
	return h
}

func realIPScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: "real-ip"}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.RealIP(ctx)
		if err != nil {
			return nil, err
		}
		return &result{info: []string{
			fmt.Sprintf(T("Cloudflare (CF-Connecting-IP): %s", "Cloudflare (CF-Connecting-IP): %s"), yesNo(st.Cloudflare)),
			fmt.Sprintf(T("Свои прокси (X-Forwarded-For): %s", "Own proxies (X-Forwarded-For): %s"), dash(strings.Join(st.From, ", "))),
		}, vals: map[string]string{"cloudflare": tf(st.Cloudflare), "from": joinList(st.From)}}, nil
	}
	h.entries = []entry{
		{act: &act{path: "stack real-ip", label: T("Изменить список своих прокси", "Change the list of own proxies"), prefill: same("from"), only: []string{"from"}, change: true}, key: "s"},
		{act: &act{path: "stack real-ip", label: T("Доверять Cloudflare", "Trust Cloudflare"), flags: map[string]string{"cloudflare": "=true"}, only: []string{"cloudflare"}, quick: true},
			when: func(v map[string]string) bool { return v["cloudflare"] != "true" }},
		{act: &act{path: "stack real-ip", label: T("Перестать доверять Cloudflare", "Stop trusting Cloudflare"), flags: map[string]string{"no-cloudflare": "=true"}, only: []string{"no-cloudflare"}},
			when: func(v map[string]string) bool { return v["cloudflare"] == "true" }},
		{act: &act{path: "stack real-ip", label: T("Очистить список своих прокси", "Clear the list of own proxies"), flags: map[string]string{"clear-from": "=true"}, only: []string{"clear-from"}},
			when: func(v map[string]string) bool { return v["from"] != "" }},
		{act: &act{path: "stack real-ip", label: T("Все параметры", "All the options")}},
	}
	return h
}

func openDatabases(m *model, a *area) screen {
	s := &listScreen{screenBase: screenBase{title: a.title},
		cols: []column{{title: T("База", "Database")}, {title: T("Владелец", "Owner")}, {title: T("Аккаунты", "Accounts")}, {title: T("Размер", "Size"), right: true}, {title: "Collation"}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Databases(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, d := range list {
			users := make([]string, 0, len(d.Users))
			for _, u := range d.Users {
				users = append(users, u.Name+"@"+u.Host+" ("+u.AuthPlugin+")")
			}
			res.rows = append(res.rows, row{cells: []string{d.Name, d.Login, strings.Join(users, ", "), humanBytes(uint64(d.SizeBytes)), d.Collation}, tones: []tone{toneNone, toneNone, toneMuted},
				vals: map[string]string{"name": d.Name, "login": d.Login}})
		}
		return res, nil
	}
	n := []string{"name"}
	s.acts = []*act{
		{path: "db create", key: "a"},
		{path: "db passwd", key: "p", args: n},
		{path: "db rm", key: "d", args: n},
		{path: "db list", quick: true},
		{path: "db engine", quick: true},
	}
	s.links = []link{{key: "e", label: T("сервер БД", "server"), open: dbEngineScreen}, {key: "c", label: T("параметры", "settings"), admin: true, open: dbConfigScreen}, allLink(a)}
	s.empty = T("баз нет — a: создать", "no databases — a: create one")
	return s
}

func dbEngineScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: T("Сервер БД", "Database server")}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.DBEngine(ctx)
		if err != nil {
			return nil, err
		}
		if !st.Installed {
			info := []string{styleWarn.Render(T("Сервер БД не установлен: Расширения → percona или mysql", "The database server is not installed: Extensions → percona or mysql"))}
			if st.Instance != nil && st.Instance.LastError != "" {
				info = append(info, styleErr.Render(T("последняя ошибка: ", "last error: ")+st.Instance.LastError))
			}
			return &result{info: info}, nil
		}
		state := "—"
		if st.Service != nil {
			state = st.Service.ActiveState
		}
		return &result{info: []string{
			fmt.Sprintf("%s %s · %s", st.Instance.Engine, st.Instance.Version, stateTone(state).render(state)),
			fmt.Sprintf(T("Сокет %s · mysql_native_password: %s · баз: %d", "Socket %s · mysql_native_password: %s · databases: %d"), st.Instance.Socket, yesNo(st.Instance.NativePassword), st.Databases),
		}}, nil
	}
	h.entries = []entry{
		{act: &act{path: "db engine tune", note: func(map[string]string) string {
			return T("Значения панели пересчитаются по памяти этого хоста, MySQL перезапустится.", "The panel's values are recalculated for this host's memory and MySQL restarts.")
		}}, key: "t"},
		{label: T("Параметры сервера", "Server settings"), key: "c", admin: true, open: dbConfigScreen},
		{act: &act{path: "db engine", quick: true}},
	}
	return h
}

func dbConfigScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Параметры MySQL", "MySQL settings")},
		cols: []column{{title: T("Ключ", "Key")}, {title: T("Значение", "Value")}, {title: T("Источник", "Source")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		cfg, err := cl.DBConfig(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{
			styleMuted.Render(fmt.Sprintf(T("Значения панели рассчитаны на %d МБ памяти. s — изменить, a — задать ключ, u — вернуть значение панели; MySQL перезапускается, при ошибке возвращаются прежние.", "The panel's values are sized for %d MB of memory. s — change, a — set a key, u — back to the panel's value; MySQL restarts, and the previous values come back on failure."), cfg.RAMMB)),
			styleMuted.Render(T("Допустимые ключи: ", "Allowed keys: ") + strings.Join(cfg.Allowed, ", ")),
		}}
		for _, v := range cfg.Values {
			src := T("панель", "panel")
			if v.Source == "custom" {
				src = T("задано", "custom")
			}
			res.rows = append(res.rows, row{cells: []string{v.Key, v.Value, src}, tones: []tone{toneNone, toneNone, toneMuted},
				vals: map[string]string{"key": v.Key, "kv": v.Key + "=" + v.Value, "source": v.Source}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "db config set", key: "s", args: []string{"kv"}, label: T("Изменить значение", "Change the value")},
		{path: "db config set", key: "a", label: T("Задать ключ", "Set a key")},
		{path: "db config unset", key: "u", args: []string{"key"}, when: func(v map[string]string) bool { return v["source"] == "custom" }},
		{path: "db config", quick: true},
	}
	return s
}

func openSSL(m *model, a *area) screen {
	h := &hubScreen{screenBase: screenBase{title: a.title}}
	h.entries = []entry{
		{label: T("Сертификаты", "Certificates"), key: "c", open: certsScreen},
		{label: T("Сертификат панели", "The panel's certificate"), key: "p", open: panelTLSScreen},
		{label: T("DNS-провайдеры (DNS-01)", "DNS providers (DNS-01)"), key: "d", open: dnsProvidersScreen},
		{act: &act{path: "ssl issue"}, key: "i"},
		{act: &act{path: "ssl import"}},
		{act: &act{path: "web tls", quick: true}},
		allEntry(a),
	}
	return h
}

func certsScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Сертификаты", "Certificates")},
		cols: []column{{title: "ID", right: true}, {title: T("Имя", "Name")}, {title: "SAN"}, {title: T("Статус", "Status")}, {title: T("Издатель", "Issuer")}, {title: T("Истекает", "Expires")}, {title: T("Авто", "Auto")}, {title: T("Ошибка", "Error")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Certificates(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, c := range list {
			exp := "—"
			tn := toneNone
			if c.NotAfter != nil {
				days := int(time.Until(*c.NotAfter).Hours() / 24)
				exp = fmt.Sprintf(T("%s (%dд)", "%s (%dd)"), c.NotAfter.Local().Format("2006-01-02"), days)
				switch {
				case days < 7:
					tn = toneErr
				case days < 21:
					tn = toneWarn
				}
			}
			res.rows = append(res.rows, row{cells: []string{strconv.FormatInt(c.ID, 10), c.Name, strings.Join(c.Names, ","), c.Status, c.Issuer, exp, yesNo(c.AutoRenew), firstLine(c.LastError, "")},
				tones: []tone{toneMuted, toneNone, toneMuted, stateTone(c.Status), toneMuted, tn, toneNone, toneErr},
				vals:  map[string]string{"id": strconv.FormatInt(c.ID, 10), "name": c.Name, "kind": c.Kind}})
		}
		return res, nil
	}
	id := []string{"id"}
	s.acts = []*act{
		{path: "ssl issue", key: "a"},
		{path: "ssl import", key: "i"},
		{path: "ssl show", key: "o", args: id, quick: true},
		{path: "ssl renew", key: "R", args: id, quick: true, when: func(v map[string]string) bool { return v["kind"] == store.CertKindACME }},
		{path: "ssl renew", label: T("Продлить все ACME-сертификаты", "Renew every ACME certificate"), flags: map[string]string{"all": "=true"}},
		{path: "ssl rm", key: "d", args: id},
		{path: "ssl list", quick: true},
	}
	s.empty = T("сертификатов нет", "no certificates")
	return s
}

func panelTLSScreen(m *model) screen {
	h := &hubScreen{screenBase: screenBase{title: T("Сертификат панели", "The panel's certificate")}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		p, err := cl.PanelTLS(ctx)
		if err != nil {
			return nil, err
		}
		src := T("самоподписанный", "self-signed")
		if p.Source == "acme" {
			src = T("выпущенный (Let's Encrypt / ACME)", "issued (Let's Encrypt / ACME)")
		}
		info := []string{
			fmt.Sprintf(T("Имя панели: %s · отдаётся: %s", "Panel hostname: %s · serving: %s"), dash(p.Hostname), src),
			fmt.Sprintf(T("Subject: %s · издатель: %s", "Subject: %s · issuer: %s"), p.Certificate.Subject, p.Certificate.Issuer),
			fmt.Sprintf(T("Действует: %s — %s", "Valid: %s — %s"), p.Certificate.NotBefore.Local().Format("2006-01-02"), p.Certificate.NotAfter.Local().Format("2006-01-02")),
		}
		vals := map[string]string{"record": "false"}
		if p.Record != nil {
			vals["record"] = "true"
			info = append(info, fmt.Sprintf(T("Запись: #%d %s, автопродление: %s", "Record: #%d %s, auto-renew: %s"), p.Record.ID, p.Record.Status, yesNo(p.Record.AutoRenew)))
			if p.Record.LastError != "" {
				info = append(info, styleErr.Render(T("Ошибка: ", "Error: ")+p.Record.LastError))
			}
		}
		if len(p.DNSProviders) > 0 {
			info = append(info, "DNS-01: "+strings.Join(p.DNSProviders, ", "))
		}
		return &result{info: info, vals: vals}, nil
	}
	h.entries = []entry{
		{act: &act{path: "ssl panel issue"}, key: "i"},
		{act: &act{path: "ssl panel import"}, key: "m"},
		{act: &act{path: "ssl panel self-signed"}, key: "s", when: func(v map[string]string) bool { return v["record"] == "true" }},
		{act: &act{path: "ssl panel", quick: true}},
		{act: &act{path: "web tls", quick: true}},
	}
	return h
}

func dnsProvidersScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("DNS-провайдеры", "DNS providers")},
		cols: []column{{title: T("Имя", "Name")}, {title: T("Тип", "Type")}, {title: T("Добавлен", "Added")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.DNSProviders(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("Для DNS-01: wildcard-сертификаты и хосты, где порт 80 закрыт снаружи. Ключи типов: t.", "For DNS-01: wildcard certificates and hosts whose port 80 is closed from outside. The keys of each type: t."))}}
		for _, p := range list {
			res.rows = append(res.rows, row{cells: []string{p.Name, p.Type, p.CreatedAt.Std().Local().Format("2006-01-02")}, vals: map[string]string{"name": p.Name}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "dns-provider add", key: "a"},
		{path: "dns-provider types", key: "t", quick: true},
		{path: "dns-provider rm", key: "d", args: []string{"name"}},
		{path: "dns-provider list", quick: true},
	}
	s.empty = T("провайдеров нет — a: добавить", "no providers — a: add one")
	return s
}

func openFirewall(m *model, a *area) screen {
	h := &hubScreen{screenBase: screenBase{title: a.title}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.Firewall(ctx)
		if err != nil {
			return nil, err
		}
		state := styleWarn.Render(T("выключен", "disabled"))
		if st.Enabled {
			state = styleKey.Render(T("включён", "enabled"))
			if !st.Active {
				state += styleErr.Render(T(" (таблица не загружена!)", " (table not loaded!)"))
			}
		}
		ports := make([]string, 0, len(st.SSHPorts))
		for _, p := range st.SSHPorts {
			ports = append(ports, strconv.Itoa(p))
		}
		info := []string{fmt.Sprintf(T("nftables: %s · policy drop · всегда открыты: ssh %s, 80, 443, панель %d · правил: %d", "nftables: %s · policy drop · always open: ssh %s, 80, 443, panel %d · rules: %d"), state, strings.Join(ports, ","), st.PanelPort, len(st.Rules))}
		for _, r := range st.Restricted {
			only := T("никого", "no one")
			if len(r.Sources) > 0 {
				only = strings.Join(r.Sources, ", ")
			}
			info = append(info, styleWarn.Render(fmt.Sprintf(T("порт %d закрыт для всех, кроме: %s", "port %d is closed to everyone except: %s"), r.Port, only)))
		}
		if f := st.Fail2ban; f != nil {
			if f.Installed {
				banned := 0
				for _, j := range f.Jails {
					banned += j.Banned
				}
				info = append(info, fmt.Sprintf(T("fail2ban: работает %s · jail: %d · забанено сейчас: %d", "fail2ban: running %s · jails: %d · banned now: %d"), yesNo(f.Running), len(f.Jails), banned))
			} else {
				info = append(info, styleMuted.Render(T("fail2ban не установлен: Расширения → fail2ban", "fail2ban is not installed: Extensions → fail2ban")))
			}
			if len(f.Trusted) > 0 {
				info = append(info, T("Доверенные (не банятся): ", "Trusted (never banned): ")+strings.Join(f.Trusted, ", "))
			}
		} else {
			info = append(info, styleMuted.Render(T("fail2ban не установлен: Расширения → fail2ban", "fail2ban is not installed: Extensions → fail2ban")))
		}
		return &result{info: info, vals: map[string]string{"enabled": tf(st.Enabled)}}, nil
	}
	on := func(v map[string]string) bool { return v["enabled"] == "true" }
	h.entries = []entry{
		{act: &act{path: "firewall enable", quick: true}, key: "e", when: func(v map[string]string) bool { return !on(v) }},
		{act: &act{path: "firewall apply"}, key: "p", when: on},
		{act: &act{path: "firewall disable", note: func(map[string]string) string {
			return T("Таблица nftables будет удалена: сервер останется без фильтрации.", "The nftables table is removed: the server is left without filtering.")
		}}, key: "x", when: on},
		{label: T("Правила", "Rules"), key: "l", open: firewallRulesScreen},
		{label: T("Забаненные адреса (fail2ban)", "Banned addresses (fail2ban)"), key: "b", open: bansScreen},
		{label: T("Доверенные адреса", "Trusted addresses"), key: "t", open: trustedScreen},
		{act: &act{path: "firewall status", quick: true}},
		allEntry(a),
	}
	return h
}

func firewallRulesScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Правила", "Rules")},
		cols: []column{{title: "ID", right: true}, {title: T("Тип", "Kind")}, {title: "Proto"}, {title: T("Порт", "Port")}, {title: T("Источник", "Source")}, {title: T("Комментарий", "Comment")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		st, err := cl.Firewall(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("Порядок: allow с источником, затем все deny, затем открытые порты и allow без источника.", "Order: allow rules with a source, then every deny, then the open ports and allow rules without a source."))}}
		for _, r := range st.Rules {
			tn := toneOK
			if r.Kind == "deny" {
				tn = toneErr
			}
			res.rows = append(res.rows, row{cells: []string{strconv.FormatInt(r.ID, 10), r.Kind, r.Proto, r.Port, r.Source, r.Comment}, tones: []tone{toneMuted, tn},
				vals: map[string]string{"id": strconv.FormatInt(r.ID, 10)}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "firewall allow", key: "a"},
		{path: "firewall deny", key: "x"},
		{path: "firewall rm", key: "d", args: []string{"id"}},
	}
	s.empty = T("правил нет", "no rules")
	return s
}

func bansScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Забаненные адреса", "Banned addresses")},
		cols: []column{{title: "IP"}, {title: "Jail"}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		st, err := cl.Firewall(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		if st.Fail2ban == nil || !st.Fail2ban.Installed {
			res.info = []string{styleMuted.Render(T("fail2ban не установлен: Расширения → fail2ban", "fail2ban is not installed: Extensions → fail2ban"))}
			return res, nil
		}
		for _, j := range st.Fail2ban.Jails {
			res.info = append(res.info, fmt.Sprintf(T("jail %s: забанено сейчас %d, всего %d", "jail %s: banned now %d, total %d"), j.Name, j.Banned, j.Total))
			for _, ip := range j.IPs {
				res.rows = append(res.rows, row{cells: []string{ip, j.Name}, vals: map[string]string{"ip": ip}})
			}
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "firewall ban", key: "a"},
		// Nothing to fill in: the confirmation shows what it does.
		{path: "firewall unban", key: "u", args: []string{"ip"}, quick: true},
	}
	s.empty = T("никто не забанен", "nobody is banned")
	return s
}

func trustedScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Доверенные адреса", "Trusted addresses")}, cols: []column{{title: T("Адрес или сеть", "Address or network")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		st, err := cl.Firewall(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("fail2ban не банит эти адреса (ignoreip), кроме них — loopback.", "fail2ban never bans these addresses (ignoreip), besides loopback."))}}
		if st.Fail2ban != nil {
			for _, ip := range st.Fail2ban.Trusted {
				res.rows = append(res.rows, row{cells: []string{ip}, vals: map[string]string{"ip": ip}})
			}
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "firewall trust", key: "a"},
		{path: "firewall untrust", key: "d", args: []string{"ip"}},
	}
	s.empty = T("доверенных адресов нет", "no trusted addresses")
	return s
}

func openBackups(m *model, a *area) screen {
	h := &hubScreen{screenBase: screenBase{title: a.title}}
	h.entries = []entry{
		{label: T("Репозитории", "Repositories"), key: "t", open: backupTargetsScreen},
		{label: T("Выполненные бэкапы", "Backups that ran"), key: "l", open: func(m *model) screen { return backupRunsScreen(m, "") }},
		{act: &act{path: "backup run"}, key: "b"},
		{act: &act{path: "backup restore"}},
		{act: &act{path: "backup snapshots"}},
		{act: &act{path: "backup target add"}, key: "a"},
		allEntry(a),
	}
	return h
}

func backupTargetsScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Репозитории", "Repositories")},
		cols: []column{{title: "ID", right: true}, {title: T("Цель", "Target")}, {title: T("Тип", "Type")}, {title: T("Репозиторий", "Repository")}, {title: "D/W/M"}, {title: T("Расписание", "Schedule")}, {title: T("Последний запуск", "Last run")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.BackupTargets(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("enter — снимки репозитория · b — бэкап сейчас · l — выполненные", "enter — the repository's snapshots · b — back up now · l — the runs"))}}
		for _, t := range list {
			last := "—"
			if t.LastRunAt != nil {
				last = when(*t.LastRunAt) + " " + t.LastStatus
			}
			res.rows = append(res.rows, row{cells: []string{strconv.FormatInt(t.ID, 10), t.Name, t.Type, t.Repository, fmt.Sprintf("%d/%d/%d", t.KeepDaily, t.KeepWeekly, t.KeepMonthly), dash(t.Schedule), last},
				tones: []tone{toneMuted, toneNone, toneMuted, toneNone, toneMuted, toneNone, stateTone(t.LastStatus)}, vals: map[string]string{"name": t.Name}})
		}
		return res, nil
	}
	tg := map[string]string{"target": "name"}
	s.acts = []*act{
		{path: "backup target add", key: "a"},
		{path: "backup run", key: "b", flags: tg},
		{path: "backup list", key: "l", flags: tg, quick: true},
		{path: "backup snapshots", flags: tg, quick: true},
		{path: "backup target rm", key: "d", args: []string{"name"}, note: func(map[string]string) string {
			return T("Панель забудет репозиторий; снимки в нём останутся.", "The panel forgets the repository; its snapshots stay in it.")
		}},
		{path: "backup target list", quick: true},
	}
	s.open = func(m *model, r row) screen { return snapshotsScreen(m, r.vals["name"]) }
	s.empty = T("репозиториев нет — a: добавить", "no repositories — a: add one")
	return s
}

func snapshotsScreen(m *model, target string) screen {
	s := &listScreen{screenBase: screenBase{title: T("Снимки ", "Snapshots of ") + target}, ctx: map[string]string{"target": target},
		cols: []column{{title: "ID"}, {title: T("Время", "Time")}, {title: T("Теги", "Tags")}, {title: T("Пути", "Paths")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.BackupSnapshots(ctx, target)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for i := len(list) - 1; i >= 0; i-- {
			sn := list[i]
			tm := sn.Time
			if len(tm) > 19 {
				tm = tm[:19]
			}
			res.rows = append(res.rows, row{cells: []string{sn.ShortID, strings.Replace(tm, "T", " ", 1), strings.Join(sn.Tags, ","), strings.Join(sn.Paths, " ")}, vals: map[string]string{"snapshot": sn.ShortID}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "backup restore", key: "R", args: []string{"snapshot"}, flags: map[string]string{"target": "target"}},
		{path: "backup run", key: "b", flags: map[string]string{"target": "target"}},
	}
	s.empty = T("снимков нет", "no snapshots")
	return s
}

func backupRunsScreen(m *model, target string) screen {
	s := &listScreen{screenBase: screenBase{title: T("Выполненные бэкапы", "Backups that ran")},
		cols: []column{{title: "ID", right: true}, {title: T("Цель", "Target")}, {title: "Scope"}, {title: T("Статус", "Status")}, {title: T("Снимок", "Snapshot")}, {title: T("Размер", "Size"), right: true}, {title: T("Файлов", "Files"), right: true}, {title: T("Начат", "Started")}, {title: T("Ошибка", "Error")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		targets, err := cl.BackupTargets(ctx)
		if err != nil {
			return nil, err
		}
		names := map[int64]string{}
		for _, t := range targets {
			names[t.ID] = t.Name
		}
		list, err := cl.Backups(ctx, target, 50)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, b := range list {
			snap := b.SnapshotID
			if len(snap) > 8 {
				snap = snap[:8]
			}
			tname := names[b.TargetID]
			if tname == "" {
				tname = strconv.FormatInt(b.TargetID, 10)
			}
			res.rows = append(res.rows, row{cells: []string{strconv.FormatInt(b.ID, 10), tname, b.Scope, b.Status, snap, humanBytes(uint64(b.SizeBytes)), strconv.FormatInt(b.Files, 10), when(b.StartedAt.Std()), firstLine(b.Error, "")},
				tones: []tone{toneMuted, toneNone, toneNone, stateTone(b.Status), toneMuted, toneNone, toneNone, toneMuted, toneErr},
				vals:  map[string]string{"snapshot": snap, "target": names[b.TargetID]}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "backup restore", key: "R", args: []string{"snapshot"}, flags: map[string]string{"target": "target"}, when: func(v map[string]string) bool { return v["snapshot"] != "" }},
		{path: "backup list", quick: true},
	}
	s.empty = T("бэкапов ещё не было", "no backups yet")
	return s
}

func openMigrate(m *model, a *area) screen {
	h := &hubScreen{screenBase: screenBase{title: a.title}}
	h.info = []string{
		T("Передать: выдать другой панели одноразовый токен на чтение аккаунта (он читает и секреты аккаунта).", "Hand over: issue a one-off token for another panel to read an account (it reads the account's secrets too)."),
		T("Принять: разобрать источник (MonoPanel, FASTPANEL или BitrixVM) — план ничего не меняет — и перенести аккаунт сюда.", "Take: read the source (MonoPanel, FASTPANEL or BitrixVM) — the plan changes nothing — and move the account here."),
	}
	grant := &act{path: "migrate grant", args: []string{"scope"}}
	h.entries = []entry{
		{label: T("Передать аккаунт: выбрать из списка", "Hand over an account: pick from the list"), key: "g", open: func(m *model) screen {
			return usersPicker(m, T("Передать аккаунт", "Hand over an account"), func(m *model, login string) tea.Cmd {
				return m.trigger(grant, map[string]string{"scope": "user:" + login})
			})
		}},
		{act: &act{path: "migrate grant", args: []string{"=user:"}}},
		{act: &act{path: "migrate plan", next: "migrate run"}, key: "p"},
		{act: &act{path: "migrate run"}, key: "m"},
		allEntry(a),
	}
	return h
}
