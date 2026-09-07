package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func mailCmd() *cobra.Command {
	c := &cobra.Command{Use: "mail", Short: "почтовый сервер: домены, ящики, алиасы, DNS, вебпочта"}

	status := &cobra.Command{Use: "status", Short: "состояние почтового сервера", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.MailStatus(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		if !st.Installed {
			fmt.Println("Почтовый сервер не установлен: mp mail install --hostname mail.example.com")
			return nil
		}
		fmt.Printf("Сервер:   %s · postfix %s · dovecot %s\n", st.Hostname, st.Versions["postfix"], st.Versions["dovecot"])
		fmt.Printf("TLS:      %s%s\n", st.TLS, until(st))
		fmt.Printf("Объекты:  доменов %d · ящиков %d · алиасов %d · лимит письма %d МБ\n", st.Domains, st.Mailboxes, st.Aliases, st.MaxSizeMB)
		parts := make([]string, 0, len(st.Services))
		for _, s := range st.Services {
			parts = append(parts, strings.TrimSuffix(s.Unit, ".service")+"="+s.ActiveState)
		}
		fmt.Println("Сервисы: ", strings.Join(parts, " "))
		open, closed, foreign := []string{}, []string{}, []string{}
		for _, p := range st.Ports {
			label := fmt.Sprintf("%d/%s", p.Port, p.Name)
			switch {
			case p.Managed && p.Open:
				open = append(open, label)
			case p.Managed:
				closed = append(closed, label)
			case p.Open:
				foreign = append(foreign, label+" ("+p.Owner+")")
			}
		}
		fmt.Println("Порты:    слушают", strings.Join(open, " "))
		if len(closed) > 0 {
			fmt.Println("          не отвечают", strings.Join(closed, " "))
		}
		for _, f := range foreign {
			fmt.Println("          занят другим сервисом:", f)
		}
		if st.Webmail != "" {
			fmt.Printf("Вебпочта: %s (Roundcube %s)\n", st.WebmailURL, st.Versions["roundcube"])
		}
		for _, w := range st.Warnings {
			fmt.Println("!", w)
		}
		return nil
	}}

	var install apitypes.MailInstallRequest
	var noPOP3 bool
	inst := &cobra.Command{Use: "install", Short: "установить postfix, dovecot и opendkim", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if noPOP3 {
			f := false
			install.POP3 = &f
		}
		res, err := cl.MailInstall(cmd.Context(), install)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, res.JobID)
	}}
	inst.Flags().StringVar(&install.Hostname, "hostname", "", "имя почтового сервера (MX и имя в сертификате); по умолчанию FQDN хоста")
	inst.Flags().BoolVar(&noPOP3, "no-pop3", false, "не включать POP3")

	apply := &cobra.Command{Use: "apply", Short: "перегенерировать конфигурацию postfix и dovecot", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.MailApply(cmd.Context())
		if err != nil {
			return err
		}
		return followJob(cmd, cl, res.JobID)
	}}

	var set apitypes.MailSettingsRequest
	var pop3, dkim, port25 string
	var rbl []string
	settings := &cobra.Command{Use: "settings", Short: "изменить настройки сервера", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		for flag, dst := range map[string]**bool{"pop3": &set.POP3, "dkim": &set.DKIM, "port25": &set.Port25} {
			raw := map[string]string{"pop3": pop3, "dkim": dkim, "port25": port25}[flag]
			if raw == "" {
				continue
			}
			v, err := strconv.ParseBool(raw)
			if err != nil {
				return &exitError{code: 2, msg: "--" + flag + ": нужно true или false"}
			}
			*dst = &v
		}
		if cmd.Flags().Changed("rbl") {
			set.RBL = &rbl
		}
		st, err := cl.MailSettings(cmd.Context(), set)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		fmt.Printf("настройки сохранены: %s, POP3 %v, DKIM %v, порт 25 %v\n", st.Hostname, st.POP3, st.DKIM, st.Port25)
		return nil
	}}
	settings.Flags().StringVar(&set.Hostname, "hostname", "", "имя почтового сервера")
	settings.Flags().IntVar(&set.MaxSizeMB, "max-size", 0, "максимальный размер письма, МБ")
	settings.Flags().StringVar(&pop3, "pop3", "", "true|false — предлагать POP3")
	settings.Flags().StringVar(&dkim, "dkim", "", "true|false — подписывать письма")
	settings.Flags().StringVar(&port25, "port25", "", "true|false — принимать почту на 25 порту")
	settings.Flags().StringSliceVar(&rbl, "rbl", nil, "чёрные списки для входящих (пустое значение очищает)")

	c.AddCommand(status, inst, apply, settings, mailDomainCmd(), mailboxCmd(), mailAliasCmd(), webmailCmd())
	return c
}

func until(st *apitypes.MailStatus) string {
	if st.CertUntil == nil {
		return ""
	}
	return " до " + st.CertUntil.Format("2006-01-02")
}

func mailDomainCmd() *cobra.Command {
	c := &cobra.Command{Use: "domain", Short: "почтовые домены"}

	list := &cobra.Command{Use: "list", Short: "список доменов", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		domains, err := cl.MailDomains(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(domains)
		}
		rows := make([][]string, 0, len(domains))
		for _, d := range domains {
			state := "активен"
			if !d.Active {
				state = "выключен"
			}
			rows = append(rows, []string{d.Name, d.Login, state, strconv.Itoa(d.Mailboxes), strconv.Itoa(d.Aliases), d.DKIMSelector})
		}
		table([]string{"ДОМЕН", "ВЛАДЕЛЕЦ", "СОСТОЯНИЕ", "ЯЩИКОВ", "АЛИАСОВ", "DKIM"}, rows)
		return nil
	}}

	var req apitypes.MailDomainRequest
	var noDKIM bool
	add := &cobra.Command{Use: "add <domain>", Short: "добавить домен и выпустить ключ DKIM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Name = args[0]
		if noDKIM {
			f := false
			req.DKIM = &f
		}
		d, err := cl.CreateMailDomain(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(d)
		}
		fmt.Printf("домен %s добавлен\n", d.Name)
		fmt.Println("что прописать в DNS: mp mail domain dns", d.Name)
		return nil
	}}
	add.Flags().StringVar(&req.User, "user", "", "владелец (обязателен для администратора)")
	add.Flags().BoolVar(&noDKIM, "no-dkim", false, "не выпускать ключ DKIM")

	rm := &cobra.Command{Use: "rm <domain>", Short: "удалить домен вместе с ящиками и письмами", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.DeleteMailDomain(cmd.Context(), args[0]); err != nil {
			return err
		}
		fmt.Printf("домен %s удалён\n", args[0])
		return nil
	}}

	dkim := &cobra.Command{Use: "dkim <domain>", Short: "выпустить новый ключ DKIM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		d, err := cl.MailDKIM(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(d)
		}
		fmt.Printf("новый селектор %s; обновите TXT-запись %s._domainkey.%s\n", d.DKIMSelector, d.DKIMSelector, d.Name)
		return nil
	}}

	dnsCmd := &cobra.Command{Use: "dns <domain>", Short: "какие записи нужны в DNS и что опубликовано сейчас", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.MailDNS(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		rows := make([][]string, 0, len(res.Records))
		for _, r := range res.Records {
			mark := map[string]string{"ok": "✓", "missing": "нет", "mismatch": "≠", "unknown": "?"}[r.Status]
			rows = append(rows, []string{mark, r.Type, r.Name, r.Value})
		}
		table([]string{"", "ТИП", "ИМЯ", "ЗНАЧЕНИЕ"}, rows)
		for _, r := range res.Records {
			if r.Status != "ok" && r.Found != "" {
				fmt.Printf("\n%s %s: сейчас %q\n", r.Type, r.Name, r.Found)
			}
		}
		if res.OK {
			fmt.Println("\nвсе обязательные записи на месте")
		}
		return nil
	}}

	c.AddCommand(list, add, rm, dkim, dnsCmd)
	return c
}

func mailboxCmd() *cobra.Command {
	c := &cobra.Command{Use: "box", Short: "почтовые ящики"}
	var domain string
	c.PersistentFlags().StringVar(&domain, "domain", "", "показывать только этот домен")

	list := &cobra.Command{Use: "list", Short: "список ящиков", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		boxes, err := cl.Mailboxes(cmd.Context(), domain)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(boxes)
		}
		rows := make([][]string, 0, len(boxes))
		for _, b := range boxes {
			quota := "без лимита"
			if b.QuotaMB > 0 {
				quota = strconv.Itoa(b.QuotaMB) + " МБ"
			}
			state := "активен"
			if !b.Active {
				state = "выключен"
			}
			rows = append(rows, []string{b.Address, b.Name, quota, state})
		}
		table([]string{"АДРЕС", "ИМЯ", "КВОТА", "СОСТОЯНИЕ"}, rows)
		return nil
	}}

	var req apitypes.MailboxRequest
	add := &cobra.Command{Use: "add <user@example.com>", Short: "создать ящик", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Address = args[0]
		res, err := cl.CreateMailbox(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("ящик %s создан\n", res.Mailbox.Address)
		if res.Password != "" {
			fmt.Printf("пароль: %s (показывается один раз)\n", res.Password)
		}
		fmt.Println("IMAP: ", res.IMAP)
		fmt.Println("SMTP: ", res.SMTP)
		return nil
	}}
	add.Flags().StringVar(&req.Password, "password", "", "пароль (пустой — сгенерируется)")
	add.Flags().StringVar(&req.Name, "name", "", "имя владельца ящика")
	add.Flags().IntVar(&req.QuotaMB, "quota", 0, "квота в МБ (0 — без ограничения)")

	var patch apitypes.MailboxUpdateRequest
	var active, name string
	var quota int
	set := &cobra.Command{Use: "set <user@example.com>", Short: "изменить пароль, квоту или состояние", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("quota") {
			patch.QuotaMB = &quota
		}
		if cmd.Flags().Changed("name") {
			patch.Name = &name
		}
		if active != "" {
			v, err := strconv.ParseBool(active)
			if err != nil {
				return &exitError{code: 2, msg: "--active: нужно true или false"}
			}
			patch.Active = &v
		}
		res, err := cl.UpdateMailbox(cmd.Context(), args[0], patch)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		if res.Password != "" {
			fmt.Printf("новый пароль %s: %s\n", args[0], res.Password)
		} else {
			fmt.Printf("ящик %s изменён\n", args[0])
		}
		return nil
	}}
	set.Flags().StringVar(&patch.Password, "password", "", "новый пароль (без других флагов пароль генерируется)")
	set.Flags().StringVar(&name, "name", "", "имя владельца ящика")
	set.Flags().IntVar(&quota, "quota", 0, "квота в МБ")
	set.Flags().StringVar(&active, "active", "", "true|false")

	var purge bool
	rm := &cobra.Command{Use: "rm <user@example.com>", Short: "удалить ящик", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.DeleteMailbox(cmd.Context(), args[0], purge); err != nil {
			return err
		}
		fmt.Printf("ящик %s удалён\n", args[0])
		return nil
	}}
	rm.Flags().BoolVar(&purge, "purge", false, "удалить и письма с диска")

	c.AddCommand(list, add, set, rm)
	return c
}

func mailAliasCmd() *cobra.Command {
	c := &cobra.Command{Use: "alias", Short: "почтовые алиасы и catch-all"}
	var domain string
	c.PersistentFlags().StringVar(&domain, "domain", "", "показывать только этот домен")

	list := &cobra.Command{Use: "list", Short: "список алиасов", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		aliases, err := cl.MailAliases(cmd.Context(), domain)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(aliases)
		}
		rows := make([][]string, 0, len(aliases))
		for _, a := range aliases {
			rows = append(rows, []string{a.Address, strings.Join(a.Destinations(), ", ")})
		}
		table([]string{"АДРЕС", "КУДА"}, rows)
		return nil
	}}

	add := &cobra.Command{Use: "add <info@example.com> <dest> [dest...]", Short: "создать алиас (@example.com — catch-all)", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		a, err := cl.CreateMailAlias(cmd.Context(), apitypes.MailAliasRequest{Address: args[0], Destinations: args[1:]})
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(a)
		}
		fmt.Printf("%s → %s\n", a.Address, strings.Join(a.Destinations(), ", "))
		return nil
	}}

	rm := &cobra.Command{Use: "rm <info@example.com>", Short: "удалить алиас", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.DeleteMailAlias(cmd.Context(), args[0]); err != nil {
			return err
		}
		fmt.Printf("алиас %s удалён\n", args[0])
		return nil
	}}

	c.AddCommand(list, add, rm)
	return c
}

func webmailCmd() *cobra.Command {
	var req apitypes.WebmailRequest
	c := &cobra.Command{Use: "webmail <domain>", Short: "поставить Roundcube отдельным сайтом панели", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Domain = args[0]
		res, err := cl.InstallWebmail(cmd.Context(), req)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, res.JobID)
	}}
	c.Flags().StringVar(&req.User, "user", "", "владелец сайта (обязателен для администратора)")
	c.Flags().StringVar(&req.PHPVersion, "php", "", "версия PHP (по умолчанию самая новая установленная)")
	return c
}
