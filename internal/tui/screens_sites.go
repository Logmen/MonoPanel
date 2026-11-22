package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"monopanel/internal/client"
	"monopanel/internal/store"
)

// siteVals are the values site actions bind: named after the flags of
// mp site set, so its form starts from the site's current settings.
func siteVals(s *store.Site) map[string]string {
	sessions := s.SessionStore
	if sessions == "" {
		sessions = "files"
	}
	start := "/data/www/" + s.Domain
	if s.Docroot != "" {
		start += "/" + s.Docroot
	}
	ssl := s.SSL
	if s.CertificateID != nil {
		ssl = "https"
	}
	return map[string]string{
		"domain": s.Domain, "login": s.Login, "user": s.Login, "php": s.PHPVersion, "mode": s.Mode, "backend": s.Backend,
		"docroot": s.Docroot, "ip": s.IP, "ipv6": s.IPv6, "ssl": s.SSL, "ssl-state": ssl, "redirect-www": s.RedirectWWW, "pm": s.FPMPM,
		"max-children": strconv.Itoa(s.FPMMaxChildren), "max-body": s.ClientMaxBody, "http2": tf(s.HTTP2), "http3": tf(s.HTTP3),
		"https-redirect": tf(s.RedirectHTTPS), "static-by-nginx": tf(s.StaticByNginx), "allow-exec": tf(s.AllowExec),
		"security-headers": tf(s.SecurityHeaders), "rate-limit": strconv.Itoa(s.RateLimit), "preset": s.Preset, "sessions": sessions,
		"allow": joinList(s.AllowFrom), "status": s.Status, "path": start, "dir": start,
	}
}

// sitePrefill: mp site set starts from what the site has now; only what
// is changed goes on the command line.
func sitePrefill() map[string]string {
	return same("php", "mode", "backend", "docroot", "ip", "ipv6", "ssl", "redirect-www", "pm", "max-children", "max-body", "http2", "http3",
		"https-redirect", "static-by-nginx", "allow-exec", "security-headers", "rate-limit", "preset", "sessions", "allow")
}

// siteActions are the actions on one site, for the list and the site page.
type siteActions struct {
	set, ini, apply, fix, suspend, unsuspend, tls, cms                *act
	logAccess, logError, logPHP, logOther, php, nginx, nginxGen, show *act
	nginxEdit, nginxClear, rm                                         *act
}

func siteActs() siteActions {
	d := []string{"domain"}
	suspended := func(v map[string]string) bool { return v["status"] == store.SiteSuspended }
	return siteActions{
		set:        &act{path: "site set", key: "s", args: d, prefill: sitePrefill(), change: true, label: T("Настройки сайта", "Site settings")},
		ini:        &act{path: "site set", key: "i", args: d, only: []string{"ini"}, change: true, label: T("PHP-параметры сайта (php_value)", "The site's PHP values (php_value)")},
		apply:      &act{path: "site apply", key: "p", args: d, quick: true},
		fix:        &act{path: "site fix", key: "x", args: d, quick: true},
		suspend:    &act{path: "site suspend", key: "S", args: d, quick: true, when: func(v map[string]string) bool { return !suspended(v) }},
		unsuspend:  &act{path: "site unsuspend", key: "S", args: d, quick: true, when: suspended},
		tls:        &act{path: "site tls", key: "t", args: d},
		cms:        &act{path: "cms install", key: "c", args: d},
		logAccess:  &act{path: "site logs", key: "l", args: d, quick: true, label: T("Лог доступа", "Access log")},
		logError:   &act{path: "site logs", args: d, flags: map[string]string{"type": "=error"}, quick: true, label: T("Лог ошибок", "Error log")},
		logPHP:     &act{path: "site logs", args: d, flags: map[string]string{"type": "=php"}, quick: true, label: T("Лог PHP", "PHP log")},
		logOther:   &act{path: "site logs", args: d, label: T("Другой лог…", "Another log…")},
		php:        &act{path: "site php", args: d, quick: true},
		nginx:      &act{path: "site nginx", key: "n", args: d, quick: true, label: T("Свои nginx-директивы", "Custom nginx directives")},
		nginxGen:   &act{path: "site nginx", args: d, flags: map[string]string{"generated": "=true"}, quick: true, label: T("Сгенерированный server-блок", "The generated server block")},
		nginxEdit:  &act{path: "site nginx", key: "E", args: d, edit: "nginx", admin: true, label: T("Редактировать nginx-директивы в $EDITOR", "Edit the nginx directives in $EDITOR")},
		nginxClear: &act{path: "site nginx", args: d, flags: map[string]string{"clear": "=true"}, admin: true, label: T("Удалить свои nginx-директивы", "Delete the custom nginx directives")},
		show:       &act{path: "site show", args: d, quick: true},
		rm:         &act{path: "site rm", key: "d", args: d},
	}
}

func (a siteActions) list() []*act {
	return []*act{a.set, a.ini, a.apply, a.fix, a.suspend, a.unsuspend, a.tls, a.cms, a.logAccess, a.logError, a.logPHP, a.logOther,
		a.php, a.nginx, a.nginxGen, a.nginxEdit, a.nginxClear, a.show, a.rm}
}

func openSites(m *model, a *area) screen {
	s := &listScreen{screenBase: screenBase{title: a.title},
		cols: []column{{title: T("Домен", "Domain"), min: 14}, {title: T("Владелец", "Owner")}, {title: "PHP"}, {title: T("Режим", "Mode")}, {title: "SSL"}, {title: T("Статус", "Status")}, {title: T("Ошибка", "Error")}}}
	s.fetch = func(ctx context.Context, cl *client.Client, _ map[string]string) (*result, error) {
		list, err := cl.Sites(ctx)
		if err != nil {
			return nil, err
		}
		res := &result{}
		for _, st := range list {
			v := siteVals(st)
			mode := st.Mode
			if st.Preset != "" {
				mode += " · " + st.Preset
			}
			name := st.Domain
			if len(st.Aliases) > 0 {
				name += " +" + strconv.Itoa(len(st.Aliases))
			}
			res.rows = append(res.rows, row{cells: []string{name, st.Login, st.PHPVersion, mode, v["ssl-state"], st.Status, firstLine(st.LastError, "")},
				tones: []tone{toneNone, toneNone, toneNone, toneMuted, stateTone(v["ssl-state"]), stateTone(st.Status), toneErr}, vals: v})
		}
		return res, nil
	}
	s.acts = append([]*act{{path: "site add", key: "a"}, {path: "site move-ip", key: "m"}}, siteActs().list()...)
	s.acts = append(s.acts, &act{path: "site presets", quick: true}, &act{path: "cms list", quick: true}, &act{path: "site list", quick: true})
	s.open = func(m *model, r row) screen { return sitePage(m, r.vals["domain"]) }
	s.links = []link{allLink(a)}
	s.empty = T("сайтов нет — a: создать", "no sites — a: create one")
	return s
}

// sitePage is the site with its settings and every action on it.
func sitePage(m *model, domain string) screen {
	h := &hubScreen{screenBase: screenBase{title: domain}, ctx: map[string]string{"domain": domain}}
	h.fetch = func(ctx context.Context, cl *client.Client) (*result, error) {
		st, err := cl.GetSite(ctx, domain)
		if err != nil {
			return nil, err
		}
		return &result{info: siteInfo(st), vals: siteVals(st)}, nil
	}
	a := siteActs()
	e := func(x *act) entry { return entry{act: x, key: x.key} }
	h.entries = []entry{
		{section: T("Настройки", "Settings")}, e(a.set), e(a.ini), e(a.tls), e(a.cms),
		{section: T("Просмотр", "View")}, e(a.show), e(a.php), e(a.logAccess), e(a.logError), e(a.logPHP), e(a.logOther), e(a.nginx), e(a.nginxGen),
		{section: T("Управление", "Manage")}, e(a.apply), e(a.fix), e(a.suspend), e(a.unsuspend), e(a.nginxEdit), e(a.nginxClear), e(a.rm),
		{section: T("Файлы", "Files")},
		{label: T("Файлы сайта", "The site's files"), key: "f", open: func(m *model) screen {
			v := h.values()
			return filesScreen(m, v["login"], v["path"])
		}},
	}
	return h
}

func siteInfo(s *store.Site) []string {
	on := func(b bool) string {
		if b {
			return styleKey.Render(T("вкл", "on"))
		}
		return styleMuted.Render(T("выкл", "off"))
	}
	ssl := s.SSL
	if s.CertificateID != nil {
		ssl = fmt.Sprintf(T("https (сертификат #%d)", "https (certificate #%d)"), *s.CertificateID)
	}
	out := []string{
		fmt.Sprintf(T("Владелец: %s · статус: %s", "Owner: %s · status: %s"), s.Login, stateTone(s.Status).render(s.Status)),
	}
	if s.LastError != "" {
		out = append(out, styleErr.Render(T("Ошибка: ", "Error: ")+firstLine(s.LastError, "")))
	}
	if len(s.Aliases) > 0 {
		out = append(out, T("Алиасы: ", "Aliases: ")+strings.Join(s.Aliases, ", "))
	}
	mode := s.Mode
	if s.Backend != "" {
		mode += " → " + s.Backend
	}
	sessions := s.SessionStore
	if sessions == "" {
		sessions = "files"
	}
	out = append(out,
		fmt.Sprintf(T("Режим: %s · PHP %s · пресет: %s · сессии: %s", "Mode: %s · PHP %s · preset: %s · sessions: %s"), mode, dash(s.PHPVersion), dash(s.Preset), sessions),
		fmt.Sprintf(T("Docroot: %s · IP: %s · IPv6: %s", "Docroot: %s · IP: %s · IPv6: %s"), dash(s.Docroot), dash(s.IP), dash(s.IPv6)),
		fmt.Sprintf(T("SSL: %s · HTTP/2 %s · HTTP/3 %s · на HTTPS %s · www: %s", "SSL: %s · HTTP/2 %s · HTTP/3 %s · to HTTPS %s · www: %s"), ssl, on(s.HTTP2), on(s.HTTP3), on(s.RedirectHTTPS), dash(s.RedirectWWW)),
		fmt.Sprintf(T("FPM: %s, max_children %d · client_max_body %s · exec %s · статика nginx %s", "FPM: %s, max_children %d · client_max_body %s · exec %s · nginx statics %s"), dash(s.FPMPM), s.FPMMaxChildren, dash(s.ClientMaxBody), on(s.AllowExec), on(s.StaticByNginx)),
		fmt.Sprintf(T("Заголовки безопасности %s · лимит запросов: %d/с", "Security headers %s · rate limit: %d/s"), on(s.SecurityHeaders), s.RateLimit),
	)
	if len(s.AllowFrom) > 0 {
		out = append(out, T("Доступ только с: ", "Access only from: ")+strings.Join(s.AllowFrom, ", "))
	}
	if len(s.PHPIni) > 0 {
		parts := make([]string, 0, len(s.PHPIni))
		for k, v := range s.PHPIni {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		out = append(out, "php_value: "+strings.Join(parts, " "))
	}
	if s.CMS != "" {
		cms := strings.TrimSpace("CMS: " + s.CMS + " " + s.CMSVersion)
		if s.CMSAt != "" {
			cms += " (" + s.CMSAt + ")"
		}
		out = append(out, cms)
	}
	return out
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
