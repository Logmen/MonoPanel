package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

func phpCmd() *cobra.Command {
	c := &cobra.Command{Use: "php", Short: "версии PHP (Sury / Remi), php-fpm на каждую"}
	var available bool
	list := &cobra.Command{Use: "list", Short: "установленные версии (с --available — вся матрица)", RunE: func(cmd *cobra.Command, _ []string) error {
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
				state = "недоступна: " + a.Note
			}
			rows = append(rows, []string{a.Version, a.Support, state, pkg, ext})
		}
		if len(rows) == 0 {
			fmt.Println("ни одной версии не установлено: mp php install 8.4  (матрица: mp php list --available)")
			return nil
		}
		table([]string{"VERSION", "UPSTREAM", "STATE", "PACKAGE", "EXT"}, rows)
		return nil
	}}
	list.Flags().BoolVar(&available, "available", false, "показать все доступные ветки")
	install := &cobra.Command{Use: "install <version>", Short: "установить ветку PHP (например 8.4) с FPM и расширениями", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	remove := &cobra.Command{Use: "remove <version>", Short: "удалить ветку PHP (если её не используют сайты)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	c.AddCommand(list, install, remove)
	return c
}

func parseIni(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, &exitError{code: 2, msg: "--ini ожидает key=value: " + p}
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func siteCmd() *cobra.Command {
	c := &cobra.Command{Use: "site", Short: "сайты: nginx + php-fpm или nginx + Apache"}

	var req apitypes.SiteRequest
	var ini []string
	var http3, allowExec, noRedirect bool
	add := &cobra.Command{Use: "add <domain>", Short: "создать сайт (каталоги, пул php-fpm, nginx, welcome-страница, сертификат)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			fmt.Printf("сайт %s (пользователь %s, PHP %s, %s) создан\n", res.Site.Domain, res.Site.Login, res.Site.PHPVersion, res.Site.Mode)
		} else if g.noWait {
			return printJSON(res)
		}
		return followJob(cmd, cl, res.JobID)
	}}
	add.Flags().StringVar(&req.User, "user", "", "владелец (логин); обязателен для администратора")
	add.Flags().StringVar(&req.PHPVersion, "php", "", "версия PHP (по умолчанию новейшая установленная)")
	add.Flags().StringVar(&req.Mode, "mode", "fpm", "fpm (nginx → php-fpm), apache (nginx → Apache → php-fpm) или proxy (nginx → backend)")
	add.Flags().StringVar(&req.Backend, "backend", "", "для proxy: http://127.0.0.1:3000 или http://unix:/run/app.sock:")
	add.Flags().BoolVar(&req.WWW, "www", false, "добавить алиас www.<domain>")
	add.Flags().StringSliceVar(&req.Aliases, "alias", nil, "дополнительные имена (можно несколько раз)")
	add.Flags().StringVar(&req.Docroot, "docroot", "", "подкаталог docroot, например public")
	add.Flags().StringVar(&req.SSL, "ssl", "auto", "auto (Let's Encrypt автоматически) или none")
	add.Flags().StringVar(&req.IP, "ip", "", "IP-адрес (по умолчанию первый адрес сервера)")
	add.Flags().StringVar(&req.RedirectWWW, "redirect-www", "", "none, to_www или to_root")
	add.Flags().StringVar(&req.FPMPM, "pm", "", "ondemand, dynamic или static")
	add.Flags().IntVar(&req.FPMMaxChildren, "max-children", 0, "pm.max_children")
	add.Flags().StringSliceVar(&ini, "ini", nil, "php_value key=value (можно несколько раз)")
	add.Flags().BoolVar(&http3, "http3", false, "включить HTTP/3 (после выпуска сертификата)")
	add.Flags().BoolVar(&allowExec, "allow-exec", false, "не отключать exec/system/proc_open")
	add.Flags().BoolVar(&noRedirect, "no-https-redirect", false, "не перенаправлять HTTP на HTTPS")
	add.Flags().StringSliceVar(&req.AllowFrom, "allow", nil, "пускать только с этих IP/CIDR (можно несколько раз); ACME-проверка остаётся доступной")
	add.Flags().StringVar(&req.Preset, "preset", "", "пресет CMS: wordpress, joomla, bitrix, opencart (mp site presets)")

	list := &cobra.Command{Use: "list", Short: "список сайтов", RunE: func(cmd *cobra.Command, _ []string) error {
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
			rows = append(rows, []string{s.Domain, s.Login, s.PHPVersion, s.Mode, s.Preset, ssl, s.IP, s.Status, firstLine(s.LastError, "")})
		}
		table([]string{"DOMAIN", "USER", "PHP", "MODE", "PRESET", "SSL", "IP", "STATUS", "ERROR"}, rows)
		return nil
	}}

	show := &cobra.Command{Use: "show <domain>", Short: "показать сайт", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	set := &cobra.Command{Use: "set <domain>", Short: "изменить настройки сайта и применить", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	set.Flags().StringVar(&upd.PHPVersion, "php", "", "версия PHP")
	set.Flags().StringVar(&upd.Mode, "mode", "", "fpm, apache или proxy")
	set.Flags().StringVar(&upd.Backend, "backend", "", "backend для proxy")
	set.Flags().StringSliceVar(&addAlias, "alias", nil, "добавить алиас")
	set.Flags().StringSliceVar(&rmAlias, "rm-alias", nil, "убрать алиас")
	set.Flags().BoolVar(&setWWW, "www", false, "добавить www.<domain>")
	set.Flags().BoolVar(&unsetWWW, "no-www", false, "убрать www.<domain>")
	set.Flags().StringVar(&docroot, "docroot", "", "подкаталог docroot (пустая строка — корень сайта)")
	set.Flags().StringVar(&upd.IP, "ip", "", "IP-адрес")
	set.Flags().StringVar(&upd.SSL, "ssl", "", "auto или none")
	set.Flags().StringVar(&upd.RedirectWWW, "redirect-www", "", "none, to_www или to_root")
	set.Flags().StringVar(&upd.FPMPM, "pm", "", "ondemand, dynamic или static")
	set.Flags().IntVar(&upd.FPMMaxChildren, "max-children", 0, "pm.max_children")
	set.Flags().StringVar(&upd.ClientMaxBody, "max-body", "", "client_max_body_size, например 128m")
	set.Flags().StringSliceVar(&updIni, "ini", nil, "php_value key=value (пустое значение удаляет)")
	set.Flags().Bool("http2", true, "HTTP/2")
	set.Flags().Bool("http3", false, "HTTP/3")
	set.Flags().Bool("https-redirect", true, "перенаправлять HTTP на HTTPS")
	set.Flags().Bool("static-by-nginx", true, "в режиме apache отдавать статику через nginx")
	set.Flags().Bool("allow-exec", false, "разрешить exec/system/proc_open")
	set.Flags().StringSliceVar(&allowFrom, "allow", nil, "заменить список разрешённых IP/CIDR")
	set.Flags().BoolVar(&allowAll, "allow-all", false, "снять ограничение по IP")
	set.Flags().StringVar(&preset, "preset", "", "пресет CMS: wordpress, joomla, bitrix, opencart; пустая строка — универсальный")

	for _, action := range []string{"apply", "suspend", "unsuspend"} {
		action := action
		short := map[string]string{"apply": "перегенерировать и применить конфигурацию", "suspend": "приостановить (503-заглушка, пул остановлен)", "unsuspend": "снова включить"}[action]
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

	var purge bool
	rm := &cobra.Command{Use: "rm <domain>", Short: "удалить сайт (--purge удаляет и файлы)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	rm.Flags().BoolVar(&purge, "purge", false, "удалить каталог сайта")

	presets := &cobra.Command{Use: "presets", Short: "пресеты CMS для новых сайтов", RunE: func(cmd *cobra.Command, _ []string) error {
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
	c.AddCommand(add, list, show, set, rm, siteLogsCmd(), siteNginxCmd(), sitePHPCmd(), presets)
	return c
}
