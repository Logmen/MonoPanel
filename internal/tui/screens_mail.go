package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
)

// Mail: the server, its domains with DKIM and DNS, mailboxes, aliases.

func mailStatusLines(st *apitypes.MailStatus) []string {
	if !st.Installed {
		return []string{styleWarn.Render(T("Почтовый сервер не установлен: i — установить (mp mail install)", "The mail server is not installed: i — install it (mp mail install)"))}
	}
	out := []string{
		fmt.Sprintf(T("Сервер: %s · postfix %s · dovecot %s", "Server: %s · postfix %s · dovecot %s"), st.Hostname, st.Versions["postfix"], st.Versions["dovecot"]),
	}
	tls := st.TLS
	if st.CertUntil != nil {
		tls += T(" до ", " until ") + st.CertUntil.Format("2006-01-02")
	}
	out = append(out, fmt.Sprintf(T("TLS: %s · доменов %d · ящиков %d · алиасов %d · лимит письма %d МБ", "TLS: %s · domains %d · mailboxes %d · aliases %d · message limit %d MB"), tls, st.Domains, st.Mailboxes, st.Aliases, st.MaxSizeMB))
	out = append(out, fmt.Sprintf(T("POP3 %s · DKIM %s · порт 25 %s · RBL: %s", "POP3 %s · DKIM %s · port 25 %s · RBL: %s"), yesNo(st.POP3), yesNo(st.DKIM), yesNo(st.Port25), dash(strings.Join(st.RBL, ", "))))
	parts := make([]string, 0, len(st.Services))
	for _, s := range st.Services {
		parts = append(parts, stateTone(s.ActiveState).render(strings.TrimSuffix(s.Unit, ".service")))
	}
	if len(parts) > 0 {
		out = append(out, T("Сервисы: ", "Services: ")+strings.Join(parts, " "))
	}
	var open, closed []string
	for _, p := range st.Ports {
		label := fmt.Sprintf("%d/%s", p.Port, p.Name)
		switch {
		case p.Managed && p.Open:
			open = append(open, label)
		case p.Managed:
			closed = append(closed, label)
		case p.Open:
			closed = append(closed, label+" ("+p.Owner+")")
		}
	}
	if len(open) > 0 {
		out = append(out, T("Порты: ", "Ports: ")+strings.Join(open, " "))
	}
	if len(closed) > 0 {
		out = append(out, styleWarn.Render(T("Не отвечают или заняты: ", "Not answering or taken: ")+strings.Join(closed, " ")))
	}
	if st.Webmail != "" {
		line := fmt.Sprintf(T("Вебпочта: %s (Roundcube %s)", "Webmail: %s (Roundcube %s)"), st.WebmailURL, st.Versions["roundcube"])
		if st.WebmailPort > 0 {
			line += fmt.Sprintf(T(" · сайт %s, порт %d", " · site %s, port %d"), st.Webmail, st.WebmailPort)
		}
		out = append(out, line)
	}
	for _, w := range st.Warnings {
		out = append(out, styleWarn.Render("! "+w))
	}
	return out
}

func openMail(m *model, a *area) screen {
	h := &hubScreen{screenBase: screenBase{title: a.title}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.MailStatus(ctx)
		if err != nil {
			return nil, err
		}
		vals := map[string]string{"installed": tf(st.Installed), "hostname": st.Hostname, "max-size": strconv.Itoa(st.MaxSizeMB), "pop3": tf(st.POP3), "dkim": tf(st.DKIM),
			"port25": tf(st.Port25), "rbl": joinList(st.RBL), "webmail-port": strconv.Itoa(st.WebmailPort), "webmail": st.Webmail}
		return &result{info: mailStatusLines(st), vals: vals}, nil
	}
	h.entries = []entry{
		{label: T("Домены", "Domains"), key: "d", open: mailDomainsScreen},
		{label: T("Ящики", "Mailboxes"), key: "b", open: func(m *model) screen { return mailboxesScreen(m, "") }},
		{label: T("Алиасы", "Aliases"), key: "a", open: func(m *model) screen { return aliasesScreen(m, "") }},
		{act: &act{path: "mail settings", prefill: same("hostname", "max-size", "pop3", "dkim", "port25", "rbl", "webmail-port"), change: true}, key: "s"},
		{act: &act{path: "mail apply", quick: true}, key: "p"},
		{act: &act{path: "mail webmail"}, key: "w"},
		{act: &act{path: "mail install"}, key: "i"},
		{act: &act{path: "mail status", quick: true}},
		allEntry(a),
	}
	return h
}

func mailDomainActs() []*act {
	n := []string{"name"}
	return []*act{
		{path: "mail domain set", key: "s", args: n, prefill: same("active", "lenient", "send-only"), change: true},
		// K, not k: k moves the cursor on lists and menus.
		{path: "mail domain dkim", key: "K", args: n, note: func(map[string]string) string {
			return T("Письма будут подписываться новым ключом: обновите TXT-запись DKIM в DNS.", "Mail is signed with the new key from now on: update the DKIM TXT record in DNS.")
		}},
		{path: "mail domain dns", key: "n", args: n, quick: true},
		{path: "mail domain rm", key: "d", args: n},
	}
}

func mailDomainsScreen(m *model) screen {
	s := &listScreen{screenBase: screenBase{title: T("Почтовые домены", "Mail domains")},
		cols: []column{{title: T("Домен", "Domain")}, {title: T("Владелец", "Owner")}, {title: T("Состояние", "State")}, {title: T("Ящиков", "Mailboxes"), right: true}, {title: T("Алиасов", "Aliases"), right: true}, {title: "DKIM"}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.MailDomains(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, d := range list {
			state := T("активен", "active")
			tn := toneOK
			if !d.Active {
				state, tn = T("выключен", "disabled"), toneWarn
			}
			if d.Lenient {
				state += T(", приёмник", ", lenient")
			}
			if d.SendOnly {
				state += T(", только отправка", ", send only")
			}
			res.rows = append(res.rows, row{cells: []string{d.Name, d.Login, state, strconv.Itoa(d.Mailboxes), strconv.Itoa(d.Aliases), d.DKIMSelector}, tones: []tone{toneNone, toneNone, tn},
				vals: map[string]string{"name": d.Name, "domain": d.Name, "active": tf(d.Active), "lenient": tf(d.Lenient), "send-only": tf(d.SendOnly)}})
		}
		return res, nil
	}
	s.acts = append([]*act{{path: "mail domain add", key: "a"}}, mailDomainActs()...)
	s.acts = append(s.acts, &act{path: "mail domain list", quick: true})
	s.open = func(m *model, r row) screen { return mailDomainPage(m, r.vals) }
	s.empty = T("доменов нет — a: добавить", "no domains — a: add one")
	return s
}

func mailDomainPage(m *model, v map[string]string) screen {
	name := v["name"]
	h := &hubScreen{screenBase: screenBase{title: name}, ctx: v}
	h.info = []string{fmt.Sprintf(T("Активен: %s · приёмник: %s · только отправка: %s", "Active: %s · lenient: %s · send only: %s"), yesNo(v["active"] == "true"), yesNo(v["lenient"] == "true"), yesNo(v["send-only"] == "true"))}
	h.entries = []entry{
		{label: T("Ящики домена", "The domain's mailboxes"), key: "b", open: func(m *model) screen { return mailboxesScreen(m, name) }},
		{label: T("Алиасы домена", "The domain's aliases"), key: "a", open: func(m *model) screen { return aliasesScreen(m, name) }},
	}
	for _, a := range mailDomainActs() {
		h.entries = append(h.entries, entry{act: a, key: a.key})
	}
	return h
}

func mailboxesScreen(m *model, domain string) screen {
	title := T("Ящики", "Mailboxes")
	if domain != "" {
		title += " · " + domain
	}
	s := &listScreen{screenBase: screenBase{title: title}, ctx: map[string]string{"domain": domain},
		cols: []column{{title: T("Адрес", "Address")}, {title: T("Имя", "Name")}, {title: T("Квота", "Quota"), right: true}, {title: T("Занято", "Used"), right: true}, {title: T("Состояние", "State")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Mailboxes(ctx, domain)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, b := range list {
			quota := T("без лимита", "unlimited")
			if b.QuotaMB > 0 {
				quota = strconv.Itoa(b.QuotaMB) + T(" МБ", " MB")
			}
			state := T("активен", "active")
			tn := toneOK
			if !b.Active {
				state, tn = T("выключен", "disabled"), toneWarn
			}
			res.rows = append(res.rows, row{cells: []string{b.Address, b.Name, quota, humanBytes(uint64(b.SizeBytes)), state}, tones: []tone{toneNone, toneNone, toneNone, toneMuted, tn},
				vals: map[string]string{"address": b.Address, "name": b.Name, "quota": strconv.Itoa(b.QuotaMB), "active": tf(b.Active)}})
		}
		return res, nil
	}
	ad := []string{"address"}
	s.acts = []*act{
		{path: "mail box add", key: "a", note: func(v map[string]string) string {
			if v["domain"] == "" {
				return ""
			}
			return fmt.Sprintf(T("Адрес вида имя@%s", "An address like name@%s"), v["domain"])
		}},
		{path: "mail box set", key: "s", args: ad, prefill: same("name", "quota", "active"), hide: []string{"password"}, change: true, label: T("Изменить ящик", "Change the mailbox")},
		{path: "mail box set", key: "p", args: ad, only: []string{"password"}, label: T("Сменить пароль (пустой — сгенерировать)", "Change the password (empty: generate one)")},
		{path: "mail box rm", key: "d", args: ad},
		{path: "mail box list", quick: true, flags: map[string]string{"domain": "domain"}},
	}
	s.empty = T("ящиков нет — a: создать", "no mailboxes — a: create one")
	return s
}

func aliasesScreen(m *model, domain string) screen {
	title := T("Алиасы", "Aliases")
	if domain != "" {
		title += " · " + domain
	}
	s := &listScreen{screenBase: screenBase{title: title}, ctx: map[string]string{"domain": domain},
		cols: []column{{title: T("Адрес", "Address")}, {title: T("Куда", "Forward to")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.MailAliases(ctx, domain)
		if err != nil {
			return nil, err
		}
		res := &result{info: []string{styleMuted.Render(T("@домен — catch-all: вся почта на несуществующие адреса домена.", "@domain is a catch-all: all mail for the domain's addresses that do not exist."))}}
		for _, a := range list {
			res.rows = append(res.rows, row{cells: []string{a.Address, strings.Join(a.Destinations(), ", ")}, vals: map[string]string{"address": a.Address}})
		}
		return res, nil
	}
	s.acts = []*act{
		{path: "mail alias add", key: "a"},
		{path: "mail alias rm", key: "d", args: []string{"address"}},
		{path: "mail alias list", quick: true, flags: map[string]string{"domain": "domain"}},
	}
	s.empty = T("алиасов нет — a: создать", "no aliases — a: create one")
	return s
}
