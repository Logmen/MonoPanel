package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

func phpCmd() *cobra.Command {
	c := &cobra.Command{Use: "php", Short: T("версии PHP (Sury / Remi), php-fpm на каждую", "PHP versions (Sury / Remi), each with its own php-fpm")}
	var available bool
	list := &cobra.Command{Use: "list", Short: T("установленные версии (с --available — вся матрица)", "installed versions (with --available, the whole matrix)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		v, err := cl.PHPVersions(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(v)
		}
		installed := map[string]*store.PHPVersion{}
		for _, p := range v.Installed {
			installed[p.Version] = p
		}
		rows := [][]string{}
		for _, a := range v.Available {
			p := installed[a.Version]
			if p == nil && !available {
				continue
			}
			state, pkg, ext := "-", "-", "-"
			if p != nil {
				state, pkg, ext = p.Status, p.PackageVersion, strconv.Itoa(len(p.Extensions))
				if p.LastError != "" {
					state += ": " + firstLine(p.LastError, "")
				}
			} else if !a.Available {
				state = T("недоступна: ", "unavailable: ") + a.Note
			}
			rows = append(rows, []string{a.Version, a.Support, state, pkg, ext})
		}
		if len(rows) == 0 {
			fmt.Println(T("ни одной версии не установлено: mp php install 8.4  (матрица: mp php list --available)", "no versions installed: mp php install 8.4  (the matrix: mp php list --available)"))
			return nil
		}
		table([]string{"VERSION", "UPSTREAM", "STATE", "PACKAGE", "EXT"}, rows)
		return nil
	}}
	list.Flags().BoolVar(&available, "available", false, T("показать все доступные ветки", "show every available branch"))
	install := &cobra.Command{Use: "install <version>", Short: T("установить ветку PHP (например 8.4) с FPM и расширениями", "install a PHP branch (for example 8.4) with FPM and extensions"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.PHPInstall(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	remove := &cobra.Command{Use: "remove <version>", Short: T("удалить ветку PHP (если её не используют сайты)", "remove a PHP branch (if no site uses it)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.PHPRemove(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	c.AddCommand(list, install, remove, phpExtCmd(), phpIniCmd())
	return c
}

func parseIni(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, &exitError{code: 2, msg: T("--ini ожидает key=value: ", "--ini expects key=value: ") + p}
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func siteCmd() *cobra.Command {
	c := &cobra.Command{Use: "site", Short: T("сайты: nginx + php-fpm или nginx + Apache", "sites: nginx + php-fpm or nginx + Apache")}

	var req apitypes.SiteRequest
	var ini []string
	var http3, allowExec, noRedirect bool
	add := &cobra.Command{Use: "add <domain>", Short: T("создать сайт (каталоги, пул php-fpm, nginx, welcome-страница, сертификат)", "create a site (directories, php-fpm pool, nginx, placeholder page, certificate)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Domain = args[0]
		if req.PHPIni, err = parseIni(ini); err != nil {
			return err
		}
		if http3 {
			t := true
			req.HTTP3 = &t
		}
		if allowExec {
			t := true
			req.AllowExec = &t
		}
		if noRedirect {
			f := false
			req.RedirectHTTPS = &f
		}
		res, err := cl.CreateSite(cmd.Context(), req)
		if err != nil {
			return err
		}
		if !g.json {
			fmt.Printf(T("сайт %s (пользователь %s, PHP %s, %s) создан\n", "site %s (user %s, PHP %s, %s) created\n"), res.Site.Domain, res.Site.Login, res.Site.PHPVersion, res.Site.Mode)
		} else if g.noWait {
			return printJSON(res)
		}
		return followJob(cmd, cl, res.JobID)
	}}
	add.Flags().StringVar(&req.User, "user", "", T("владелец (логин); обязателен для администратора", "owner (login); required for an administrator"))
	add.Flags().StringVar(&req.PHPVersion, "php", "", T("версия PHP (по умолчанию новейшая установленная)", "PHP version (the newest installed by default)"))
	add.Flags().StringVar(&req.Mode, "mode", "fpm", T("fpm (nginx → php-fpm), apache (nginx → Apache → php-fpm) или proxy (nginx → backend)", "fpm (nginx → php-fpm), apache (nginx → Apache → php-fpm) or proxy (nginx → backend)"))
	add.Flags().StringVar(&req.Backend, "backend", "", T("для proxy: http://127.0.0.1:3000 или http://unix:/run/app.sock:", "for proxy: http://127.0.0.1:3000 or http://unix:/run/app.sock:"))
	add.Flags().BoolVar(&req.WWW, "www", false, T("добавить алиас www.<domain>", "add the alias www.<domain>"))
	add.Flags().StringSliceVar(&req.Aliases, "alias", nil, T("дополнительные имена (можно несколько раз)", "additional names (repeatable)"))
	add.Flags().StringVar(&req.Docroot, "docroot", "", T("подкаталог docroot, например public", "docroot subdirectory, for example public"))
	add.Flags().StringVar(&req.SSL, "ssl", "auto", T("auto (Let's Encrypt автоматически) или none", "auto (Let's Encrypt, issued automatically) or none"))
	add.Flags().StringVar(&req.IP, "ip", "", T("IP-адрес (по умолчанию первый адрес сервера)", "IP address (the server's first address by default)"))
	add.Flags().StringVar(&req.RedirectWWW, "redirect-www", "", T("none, to_www или to_root", "none, to_www or to_root"))
	add.Flags().StringVar(&req.FPMPM, "pm", "", T("ondemand, dynamic или static", "ondemand, dynamic or static"))
	add.Flags().IntVar(&req.FPMMaxChildren, "max-children", 0, "pm.max_children")
	add.Flags().StringSliceVar(&ini, "ini", nil, T("php_value key=value (можно несколько раз)", "php_value key=value (repeatable)"))
	add.Flags().BoolVar(&http3, "http3", false, T("включить HTTP/3 (после выпуска сертификата)", "enable HTTP/3 (once the certificate is issued)"))
	add.Flags().BoolVar(&allowExec, "allow-exec", false, T("не отключать exec/system/proc_open", "do not disable exec/system/proc_open"))
	add.Flags().BoolVar(&noRedirect, "no-https-redirect", false, T("не перенаправлять HTTP на HTTPS", "do not redirect HTTP to HTTPS"))
	add.Flags().StringSliceVar(&req.AllowFrom, "allow", nil, T("пускать только с этих IP/CIDR (можно несколько раз); ACME-проверка остаётся доступной", "allow access only from these IPs/CIDRs (repeatable); ACME validation stays open"))
	add.Flags().StringVar(&req.Preset, "preset", "", T("пресет CMS: wordpress, joomla, bitrix, opencart (mp site presets)", "CMS preset: wordpress, joomla, bitrix, opencart (mp site presets)"))

	list := &cobra.Command{Use: "list", Short: T("список сайтов", "list sites"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		sites, err := cl.Sites(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(sites)
		}
		rows := make([][]string, 0, len(sites))
		for _, s := range sites {
			ssl := s.SSL
			if s.CertificateID != nil {
				ssl = "issued"
			}
			cms := "-"
			if s.CMS != "" {
				cms = strings.TrimSpace(s.CMS + " " + s.CMSVersion)
			}
			rows = append(rows, []string{s.Domain, s.Login, s.PHPVersion, s.Mode, s.Preset, cms, ssl, s.IP, s.Status, firstLine(s.LastError, "")})
		}
		table([]string{"DOMAIN", "USER", "PHP", "MODE", "PRESET", "CMS", "SSL", "IP", "STATUS", "ERROR"}, rows)
		return nil
	}}

	show := &cobra.Command{Use: "show <domain>", Short: T("показать сайт", "show a site"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		s, err := cl.GetSite(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return printJSON(s)
	}}

	var upd apitypes.SiteUpdateRequest
	var updIni, addAlias, rmAlias []string
	var setWWW, unsetWWW bool
	var docroot string
	var allowFrom []string
	var allowAll bool
	var preset string
	set := &cobra.Command{Use: "set <domain>", Short: T("изменить настройки сайта и применить", "change a site's settings and apply them"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if len(addAlias)+len(rmAlias) > 0 || setWWW || unsetWWW {
			cur, err := cl.GetSite(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			aliases := map[string]bool{}
			for _, a := range cur.Aliases {
				aliases[a] = true
			}
			for _, a := range addAlias {
				aliases[strings.ToLower(a)] = true
			}
			if setWWW {
				aliases["www."+cur.Domain] = true
			}
			for _, a := range rmAlias {
				delete(aliases, strings.ToLower(a))
			}
			if unsetWWW {
				delete(aliases, "www."+cur.Domain)
			}
			list := make([]string, 0, len(aliases))
			for a := range aliases {
				list = append(list, a)
			}
			upd.Aliases = &list
		}
		if cmd.Flags().Changed("docroot") {
			upd.Docroot = &docroot
		}
		if allowAll {
			empty := []string{}
			upd.AllowFrom = &empty
		} else if cmd.Flags().Changed("allow") {
			upd.AllowFrom = &allowFrom
		}
		if cmd.Flags().Changed("preset") {
			upd.Preset = &preset
		}
		if len(updIni) > 0 {
			if upd.PHPIni, err = parseIni(updIni); err != nil {
				return err
			}
		}
		for _, f := range []struct {
			name string
			dst  **bool
		}{{"http2", &upd.HTTP2}, {"http3", &upd.HTTP3}, {"https-redirect", &upd.RedirectHTTPS}, {"static-by-nginx", &upd.StaticByNginx}, {"allow-exec", &upd.AllowExec}} {
			if cmd.Flags().Changed(f.name) {
				v, _ := cmd.Flags().GetBool(f.name)
				*f.dst = &v
			}
		}
		res, err := cl.UpdateSite(cmd.Context(), args[0], upd)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, res.JobID)
	}}
	set.Flags().StringVar(&upd.PHPVersion, "php", "", T("версия PHP", "PHP version"))
	set.Flags().StringVar(&upd.Mode, "mode", "", T("fpm, apache или proxy", "fpm, apache or proxy"))
	set.Flags().StringVar(&upd.Backend, "backend", "", T("backend для proxy", "backend for proxy"))
	set.Flags().StringSliceVar(&addAlias, "alias", nil, T("добавить алиас", "add an alias"))
	set.Flags().StringSliceVar(&rmAlias, "rm-alias", nil, T("убрать алиас", "remove an alias"))
	set.Flags().BoolVar(&setWWW, "www", false, T("добавить www.<domain>", "add www.<domain>"))
	set.Flags().BoolVar(&unsetWWW, "no-www", false, T("убрать www.<domain>", "remove www.<domain>"))
	set.Flags().StringVar(&docroot, "docroot", "", T("подкаталог docroot (пустая строка — корень сайта)", "docroot subdirectory (an empty string means the site root)"))
	set.Flags().StringVar(&upd.IP, "ip", "", T("IP-адрес", "IP address"))
	set.Flags().StringVar(&upd.SSL, "ssl", "", T("auto или none", "auto or none"))
	set.Flags().StringVar(&upd.RedirectWWW, "redirect-www", "", T("none, to_www или to_root", "none, to_www or to_root"))
	set.Flags().StringVar(&upd.FPMPM, "pm", "", T("ondemand, dynamic или static", "ondemand, dynamic or static"))
	set.Flags().IntVar(&upd.FPMMaxChildren, "max-children", 0, "pm.max_children")
	set.Flags().StringVar(&upd.ClientMaxBody, "max-body", "", T("client_max_body_size, например 128m", "client_max_body_size, for example 128m"))
	set.Flags().StringSliceVar(&updIni, "ini", nil, T("php_value key=value (пустое значение удаляет)", "php_value key=value (an empty value removes the key)"))
	set.Flags().Bool("http2", true, "HTTP/2")
	set.Flags().Bool("http3", false, "HTTP/3")
	set.Flags().Bool("https-redirect", true, T("перенаправлять HTTP на HTTPS", "redirect HTTP to HTTPS"))
	set.Flags().Bool("static-by-nginx", true, T("в режиме apache отдавать статику через nginx", "in apache mode, serve static files through nginx"))
	set.Flags().Bool("allow-exec", false, T("разрешить exec/system/proc_open", "allow exec/system/proc_open"))
	set.Flags().StringSliceVar(&allowFrom, "allow", nil, T("заменить список разрешённых IP/CIDR", "replace the list of allowed IPs/CIDRs"))
	set.Flags().BoolVar(&allowAll, "allow-all", false, T("снять ограничение по IP", "lift the IP restriction"))
	set.Flags().StringVar(&preset, "preset", "", T("пресет CMS: wordpress, joomla, bitrix, opencart; пустая строка — универсальный", "CMS preset: wordpress, joomla, bitrix, opencart; an empty string selects the universal one"))
	set.Flags().StringVar(&upd.SessionStore, "sessions", "", T("где PHP хранит сессии: files (tmp аккаунта) или valkey (экземпляр сессий аккаунта, mp valkey add sessions)", "where PHP keeps sessions: files (the account's tmp) or valkey (the account's sessions instance, mp valkey add sessions)"))

	for _, action := range []string{"apply", "suspend", "unsuspend", "fix"} {
		action := action
		short := map[string]string{"apply": T("перегенерировать и применить конфигурацию", "regenerate and apply the configuration"), "suspend": T("приостановить (503-заглушка, пул остановлен)", "suspend a site (503 placeholder page, pool stopped)"), "unsuspend": T("снова включить", "enable a site again"),
			"fix": T("починить владельца, права, ACL и метки SELinux файлов сайта", "repair the owner, permissions, ACLs and SELinux labels of the site's files")}[action]
		c.AddCommand(&cobra.Command{Use: action + " <domain>", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			res, err := cl.SiteAction(cmd.Context(), args[0], action)
			if err != nil {
				return err
			}
			return followJob(cmd, cl, res.JobID)
		}})
	}

	var moveFrom string
	moveIP := &cobra.Command{Use: T("move-ip <новый-адрес>", "move-ip <new-address>"), Short: T("перенести сайты на другой адрес этого сервера (после смены IP хоста)", "move sites to another address of this server (after the host's IP changes)"), Long: T("Переносит разом все сайты прежнего адреса (--from) или всех адресов, которых на сервере больше нет: по одному через site set --ip они не проходят проверку nginx, пока остальные слушают старый адрес.", "Moves every site on the previous address (--from), or on all the addresses the server no longer has, in one go: moved one at a time with site set --ip, they fail the nginx check while the rest still listen on the old address."), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.MoveSitesIP(cmd.Context(), apitypes.SiteMoveIPRequest{From: moveFrom, To: args[0]})
		if err != nil {
			return err
		}
		if !g.json {
			fmt.Printf(T("сайты на %s: %s\n", "sites moving to %s: %s\n"), args[0], strings.Join(res.Sites, ", "))
		}
		return followJob(cmd, cl, res.JobID)
	}}
	moveIP.Flags().StringVar(&moveFrom, "from", "", T("прежний адрес (по умолчанию — все адреса, которых на сервере нет)", "the previous address (by default, all the addresses the server no longer has)"))

	var purge bool
	rm := &cobra.Command{Use: "rm <domain>", Short: T("удалить сайт (--purge удаляет и файлы)", "delete a site (--purge deletes its files too)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.DeleteSite(cmd.Context(), args[0], purge)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	rm.Flags().BoolVar(&purge, "purge", false, T("удалить каталог сайта", "delete the site directory"))

	presets := &cobra.Command{Use: "presets", Short: T("пресеты CMS для новых сайтов", "CMS presets for new sites"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.Presets(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, p := range list {
			id := p.ID
			if id == "" {
				id = "-"
			}
			rows = append(rows, []string{id, p.Name, p.Description})
		}
		table([]string{"PRESET", "NAME", "DESCRIPTION"}, rows)
		return nil
	}}
	c.AddCommand(add, list, show, set, moveIP, rm, siteLogsCmd(), siteNginxCmd(), sitePHPCmd(), siteTLSCmd(), presets)
	return c
}

// phpExtCmd управляет расширениями ветки целиком: php-fpm — один мастер на
// версию, поэтому отключить расширение только для одного сайта нельзя.
func phpExtCmd() *cobra.Command {
	c := &cobra.Command{Use: "ext", Short: T("расширения ветки: посмотреть, включить, выключить", "a branch's extensions: list, enable, disable")}
	show := func(st *apitypes.PHPExtensions) error {
		if g.json {
			return printJSON(st)
		}
		rows := make([][]string, 0, len(st.Extensions))
		for _, e := range st.Extensions {
			state, note := T("выключено", "disabled"), ""
			if e.Enabled {
				state = T("включено", "enabled")
			}
			if !e.Installed {
				state, note = T("не установлено", "not installed"), T("включение поставит ", "enabling installs ")+e.Package
			}
			if e.Critical {
				note = T("нужен типовому сайту", "needed by a typical site")
			}
			rows = append(rows, []string{e.Name, state, note})
		}
		table([]string{T("РАСШИРЕНИЕ", "EXTENSION"), T("СОСТОЯНИЕ", "STATE"), ""}, rows)
		return nil
	}
	list := &cobra.Command{Use: T("list <версия>", "list <version>"), Short: T("расширения ветки", "list the extensions of a branch"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.PHPExtensions(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return show(st)
	}}
	c.AddCommand(list)
	for _, action := range []string{"enable", "disable"} {
		on := action == "enable"
		short := T("выключить расширение (php-fpm перезапустится)", "disable an extension (php-fpm restarts)")
		if on {
			short = T("включить расширение (php-fpm перезапустится)", "enable an extension (php-fpm restarts)")
		}
		c.AddCommand(&cobra.Command{Use: action + T(" <версия> <расширение>", " <version> <extension>"), Short: short, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			st, err := cl.SetPHPExtension(cmd.Context(), args[0], args[1], on)
			if err != nil {
				return err
			}
			if !g.json {
				fmt.Fprintf(os.Stderr, T("%s для PHP %s: %s\n", "%s for PHP %s: %s\n"), args[1], args[0], map[bool]string{true: T("включено", "enabled"), false: T("выключено", "disabled")}[on])
			}
			return show(st)
		}})
	}
	return c
}

// siteTLSCmd orders a certificate for a site's names and switches it to HTTPS.
func siteTLSCmd() *cobra.Command {
	var req apitypes.SiteTLSIssueRequest
	c := &cobra.Command{Use: "tls <domain>", Short: T("выпустить сертификат для сайта (домен и алиасы) и включить HTTPS; --dns для DNS-01", "issue a certificate for a site (domain and aliases) and switch it to HTTPS; --dns for DNS-01"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.SiteTLSIssue(cmd.Context(), args[0], req)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, res.JobID)
	}}
	c.Flags().StringVar(&req.DNS, "dns", "", T("DNS-провайдер для DNS-01 (когда порт 80 недоступен снаружи)", "DNS provider for DNS-01 (when port 80 cannot be reached from outside)"))
	c.Flags().BoolVar(&req.Staging, "staging", false, T("staging-директория Let's Encrypt (тестовый сертификат)", "use the Let's Encrypt staging directory (a test certificate)"))
	c.Flags().StringVar(&req.Email, "email", "", T("e-mail аккаунта ACME (запоминается)", "ACME account e-mail (remembered)"))
	return c
}

// phpIniCmd shows and changes the server-wide php.ini layer: what every site
// inherits unless its preset or the site itself sets the key.
func phpIniCmd() *cobra.Command {
	c := &cobra.Command{Use: "ini", Short: T("PHP-параметры для всех сайтов сервера (слой «глобально»)", "PHP settings for all sites on the server (the “global” layer)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.PHPSettings(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		rows := make([][]string, 0, len(res.Values))
		for _, v := range res.Values {
			src := T("панель", "panel")
			if v.Source == "global" {
				src = T("глобально", "global")
			}
			rows = append(rows, []string{v.Key, v.Value, src})
		}
		table([]string{"KEY", "VALUE", "SOURCE"}, rows)
		fmt.Println(T("пресет и значения сайта перекрывают эти; менять: mp php ini set key=value, вернуть панельное: mp php ini unset key", "a preset and the site's own values override these; to change: mp php ini set key=value, to restore the panel's value: mp php ini unset key"))
		return nil
	}}
	change := func(ini map[string]string, cmd *cobra.Command) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.PHPSettingsSet(cmd.Context(), ini)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf(T("сохранено; пулы %d сайтов пересобираются\n", "saved; the pools of %d sites are being rebuilt\n"), len(res.Jobs))
		for _, id := range res.Jobs {
			if err := followJob(cmd, cl, id); err != nil {
				return err
			}
		}
		return nil
	}
	set := &cobra.Command{Use: "set key=value ...", Short: T("задать значения для всех сайтов", "set values for all sites"), Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ini, err := parseIni(args)
		if err != nil {
			return err
		}
		for k, v := range ini {
			if v == "" {
				return &exitError{code: 2, msg: T("пустое значение для ", "empty value for ") + k + T(": чтобы вернуть панельное, используйте mp php ini unset ", ": to restore the panel's value, use mp php ini unset ") + k}
			}
		}
		return change(ini, cmd)
	}}
	unset := &cobra.Command{Use: "unset key ...", Short: T("убрать глобальные значения (снова действует значение панели)", "remove global values (the panel's value applies again)"), Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ini := map[string]string{}
		for _, k := range args {
			ini[strings.TrimSpace(k)] = ""
		}
		return change(ini, cmd)
	}}
	c.AddCommand(set, unset)
	return c
}
