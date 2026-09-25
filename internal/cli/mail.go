package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func mailCmd() *cobra.Command {
	c := &cobra.Command{Use: "mail", Short: T("почтовый сервер: домены, ящики, алиасы, DNS, вебпочта", "mail server: domains, mailboxes, aliases, DNS, webmail")}

	status := &cobra.Command{Use: "status", Short: T("состояние почтового сервера", "mail server status"), RunE: func(cmd *cobra.Command, _ []string) error {
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
			fmt.Println(T("Почтовый сервер не установлен: mp mail install --hostname mail.example.com", "The mail server is not installed: mp mail install --hostname mail.example.com"))
			return nil
		}
		fmt.Printf(T("Сервер:   %s · postfix %s · dovecot %s\n", "Server:   %s · postfix %s · dovecot %s\n"), st.Hostname, st.Versions["postfix"], st.Versions["dovecot"])
		fmt.Printf("TLS:      %s%s\n", st.TLS, until(st))
		fmt.Printf(T("Объекты:  доменов %d · ящиков %d · алиасов %d · лимит письма %d МБ\n", "Objects:  domains %d · mailboxes %d · aliases %d · message size limit %d MB\n"), st.Domains, st.Mailboxes, st.Aliases, st.MaxSizeMB)
		parts := make([]string, 0, len(st.Services))
		for _, s := range st.Services {
			parts = append(parts, strings.TrimSuffix(s.Unit, ".service")+"="+s.ActiveState)
		}
		fmt.Println(T("Сервисы: ", "Services:"), strings.Join(parts, " "))
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
		fmt.Println(T("Порты:    слушают", "Ports:    listening"), strings.Join(open, " "))
		if len(closed) > 0 {
			fmt.Println(T("          не отвечают", "          not responding"), strings.Join(closed, " "))
		}
		for _, f := range foreign {
			fmt.Println(T("          занят другим сервисом:", "          taken by another service:"), f)
		}
		if st.Webmail != "" {
			fmt.Printf(T("Вебпочта: %s (Roundcube %s)\n", "Webmail:  %s (Roundcube %s)\n"), st.WebmailURL, st.Versions["roundcube"])
			if st.WebmailPort > 0 {
				fmt.Printf(T("          сайт %s, порт %d — на имени и сертификате почтового сервера\n", "          site %s, port %d — on the mail server's name and certificate\n"), st.Webmail, st.WebmailPort)
			}
		}
		for _, w := range st.Warnings {
			fmt.Println("!", w)
		}
		return nil
	}}

	var install apitypes.MailInstallRequest
	var noPOP3 bool
	inst := &cobra.Command{Use: "install", Short: T("установить postfix, dovecot и opendkim", "install postfix, dovecot and opendkim"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	inst.Flags().StringVar(&install.Hostname, "hostname", "", T("имя почтового сервера (MX и имя в сертификате); по умолчанию FQDN хоста", "mail server hostname (MX and the name in the certificate); defaults to the host's FQDN"))
	inst.Flags().BoolVar(&noPOP3, "no-pop3", false, T("не включать POP3", "do not enable POP3"))

	apply := &cobra.Command{Use: "apply", Short: T("перегенерировать конфигурацию postfix и dovecot", "regenerate the postfix and dovecot configuration"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	settings := &cobra.Command{Use: "settings", Short: T("изменить настройки сервера", "change the server settings"), RunE: func(cmd *cobra.Command, _ []string) error {
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
				return &exitError{code: 2, msg: "--" + flag + T(": нужно true или false", ": must be true or false")}
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
		fmt.Printf(T("настройки сохранены: %s, POP3 %v, DKIM %v, порт 25 %v\n", "settings saved: %s, POP3 %v, DKIM %v, port 25 %v\n"), st.Hostname, st.POP3, st.DKIM, st.Port25)
		return nil
	}}
	settings.Flags().StringVar(&set.Hostname, "hostname", "", T("имя почтового сервера", "mail server hostname"))
	settings.Flags().IntVar(&set.MaxSizeMB, "max-size", 0, T("максимальный размер письма, МБ", "maximum message size, MB"))
	settings.Flags().StringVar(&pop3, "pop3", "", T("true|false — предлагать POP3", "true|false — offer POP3"))
	settings.Flags().StringVar(&dkim, "dkim", "", T("true|false — подписывать письма", "true|false — sign messages"))
	settings.Flags().StringVar(&port25, "port25", "", T("true|false — принимать почту на 25 порту", "true|false — accept mail on port 25"))
	settings.Flags().StringSliceVar(&rbl, "rbl", nil, T("чёрные списки для входящих (пустое значение очищает)", "blocklists for incoming mail (an empty value clears them)"))
	var webmailPort int
	settings.Flags().IntVar(&webmailPort, "webmail-port", 0, T("порт для вебпочты на имени почтового сервера (0 — выключить)", "webmail port on the mail server's hostname (0 turns it off)"))
	settingsRunE := settings.RunE
	settings.RunE = func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("webmail-port") {
			set.WebmailPort = &webmailPort
		}
		return settingsRunE(cmd, args)
	}

	c.AddCommand(status, inst, apply, settings, mailDomainCmd(), mailboxCmd(), mailAliasCmd(), webmailCmd())
	return c
}

func until(st *apitypes.MailStatus) string {
	if st.CertUntil == nil {
		return ""
	}
	return T(" до ", " until ") + st.CertUntil.Format("2006-01-02")
}

func mailDomainCmd() *cobra.Command {
	c := &cobra.Command{Use: "domain", Short: T("почтовые домены", "mail domains")}

	list := &cobra.Command{Use: "list", Short: T("список доменов", "list domains"), RunE: func(cmd *cobra.Command, _ []string) error {
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
			state := T("активен", "active")
			if !d.Active {
				state = T("выключен", "disabled")
			}
			if d.Lenient {
				state += T(", приёмник", ", lenient")
			}
			if d.SendOnly {
				state += T(", только отправка", ", send only")
			}
			rows = append(rows, []string{d.Name, d.Login, state, strconv.Itoa(d.Mailboxes), strconv.Itoa(d.Aliases), d.DKIMSelector})
		}
		table([]string{T("ДОМЕН", "DOMAIN"), T("ВЛАДЕЛЕЦ", "OWNER"), T("СОСТОЯНИЕ", "STATE"), T("ЯЩИКОВ", "MAILBOXES"), T("АЛИАСОВ", "ALIASES"), "DKIM"}, rows)
		return nil
	}}

	var req apitypes.MailDomainRequest
	var noDKIM bool
	add := &cobra.Command{Use: "add <domain>", Short: T("добавить домен и выпустить ключ DKIM", "add a domain and issue a DKIM key"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf(T("домен %s добавлен\n", "domain %s added\n"), d.Name)
		fmt.Println(T("что прописать в DNS: mp mail domain dns", "what to publish in DNS: mp mail domain dns"), d.Name)
		return nil
	}}
	add.Flags().StringVar(&req.User, "user", "", T("владелец (обязателен для администратора)", "owner (required for an administrator)"))
	add.Flags().BoolVar(&noDKIM, "no-dkim", false, T("не выпускать ключ DKIM", "do not issue a DKIM key"))
	add.Flags().BoolVar(&req.Lenient, "lenient", false, T("домен-приёмник: принимать письма и от криво настроенных отправителей", "lenient domain: accept mail even from badly configured senders"))
	add.Flags().BoolVar(&req.SendOnly, "send-only", false, T("только отправка: почту домена принимает другой сервер (MX у почтового провайдера), здесь — подпись DKIM и отправка писем сайтов", "send only: another server receives the domain's mail (MX at your mail provider); this one signs (DKIM) and sends the sites' mail"))

	var lenient, activeFlag, sendOnly string
	set := &cobra.Command{Use: "set <domain>", Short: T("включить или выключить домен, сделать приёмником или «только отправка»", "enable or disable a domain, make it lenient or send-only"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		var patch apitypes.MailDomainUpdateRequest
		for name, raw := range map[string]string{"lenient": lenient, "active": activeFlag, "send-only": sendOnly} {
			if raw == "" {
				continue
			}
			v, err := strconv.ParseBool(raw)
			if err != nil {
				return &exitError{code: 2, msg: "--" + name + T(": нужно true или false", ": must be true or false")}
			}
			switch name {
			case "lenient":
				patch.Lenient = &v
			case "send-only":
				patch.SendOnly = &v
			default:
				patch.Active = &v
			}
		}
		d, err := cl.UpdateMailDomain(cmd.Context(), args[0], patch)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(d)
		}
		fmt.Printf(T("домен %s: активен %v, приём без строгих проверок %v, только отправка %v\n", "domain %s: active %v, lenient %v, send only %v\n"), d.Name, d.Active, d.Lenient, d.SendOnly)
		return nil
	}}
	set.Flags().StringVar(&lenient, "lenient", "", T("true|false — принимать письма без проверок HELO и домена отправителя", "true|false — accept mail without the HELO and sender domain checks"))
	set.Flags().StringVar(&activeFlag, "active", "", "true|false")
	set.Flags().StringVar(&sendOnly, "send-only", "", T("true|false — только отправка: почту домена принимает другой сервер (у домена не должно быть ящиков и алиасов)", "true|false — send only: another server receives the domain's mail (the domain must have no mailboxes or aliases)"))

	rm := &cobra.Command{Use: "rm <domain>", Short: T("удалить домен вместе с ящиками и письмами", "delete a domain along with its mailboxes and messages"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.DeleteMailDomain(cmd.Context(), args[0]); err != nil {
			return err
		}
		fmt.Printf(T("домен %s удалён\n", "domain %s deleted\n"), args[0])
		return nil
	}}

	dkim := &cobra.Command{Use: "dkim <domain>", Short: T("выпустить новый ключ DKIM", "issue a new DKIM key"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf(T("новый селектор %s; обновите TXT-запись %s._domainkey.%s\n", "new selector %s; update the TXT record %s._domainkey.%s\n"), d.DKIMSelector, d.DKIMSelector, d.Name)
		return nil
	}}

	dnsCmd := &cobra.Command{Use: "dns <domain>", Short: T("какие записи нужны в DNS и что опубликовано сейчас", "which DNS records are needed and what is published now"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			mark := map[string]string{"ok": "✓", "missing": T("нет", "missing"), "mismatch": "≠", "unknown": "?"}[r.Status]
			rows = append(rows, []string{mark, r.Type, r.Name, r.Value})
		}
		table([]string{"", T("ТИП", "TYPE"), T("ИМЯ", "NAME"), T("ЗНАЧЕНИЕ", "VALUE")}, rows)
		for _, r := range res.Records {
			if r.Status != "ok" && r.Found != "" {
				fmt.Printf(T("\n%s %s: сейчас %q\n", "\n%s %s: currently %q\n"), r.Type, r.Name, r.Found)
			}
		}
		if res.OK {
			fmt.Println(T("\nвсе обязательные записи на месте", "\nall required records are in place"))
		}
		return nil
	}}

	c.AddCommand(list, add, set, rm, dkim, dnsCmd)
	return c
}

func mailboxCmd() *cobra.Command {
	c := &cobra.Command{Use: "box", Short: T("почтовые ящики", "mailboxes")}
	var domain string
	c.PersistentFlags().StringVar(&domain, "domain", "", T("показывать только этот домен", "show only this domain"))

	list := &cobra.Command{Use: "list", Short: T("список ящиков", "list mailboxes"), RunE: func(cmd *cobra.Command, _ []string) error {
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
			quota := T("без лимита", "unlimited")
			if b.QuotaMB > 0 {
				quota = strconv.Itoa(b.QuotaMB) + T(" МБ", " MB")
			}
			state := T("активен", "active")
			if !b.Active {
				state = T("выключен", "disabled")
			}
			rows = append(rows, []string{b.Address, b.Name, quota, state})
		}
		table([]string{T("АДРЕС", "ADDRESS"), T("ИМЯ", "NAME"), T("КВОТА", "QUOTA"), T("СОСТОЯНИЕ", "STATE")}, rows)
		return nil
	}}

	var req apitypes.MailboxRequest
	add := &cobra.Command{Use: "add <user@example.com>", Short: T("создать ящик", "create a mailbox"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf(T("ящик %s создан\n", "mailbox %s created\n"), res.Mailbox.Address)
		if res.Password != "" {
			fmt.Printf(T("пароль: %s (показывается один раз)\n", "password: %s (shown only once)\n"), res.Password)
		}
		fmt.Println("IMAP: ", res.IMAP)
		fmt.Println("SMTP: ", res.SMTP)
		return nil
	}}
	add.Flags().StringVar(&req.Password, "password", "", T("пароль (пустой — сгенерируется)", "password (generated if empty)"))
	add.Flags().StringVar(&req.Name, "name", "", T("имя владельца ящика", "name of the mailbox owner"))
	add.Flags().IntVar(&req.QuotaMB, "quota", 0, T("квота в МБ (0 — без ограничения)", "quota in MB (0 — unlimited)"))

	var patch apitypes.MailboxUpdateRequest
	var active, name string
	var quota int
	set := &cobra.Command{Use: "set <user@example.com>", Short: T("изменить пароль, квоту или состояние", "change the password, quota or state"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
				return &exitError{code: 2, msg: T("--active: нужно true или false", "--active: must be true or false")}
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
			fmt.Printf(T("новый пароль %s: %s\n", "new password for %s: %s\n"), args[0], res.Password)
		} else {
			fmt.Printf(T("ящик %s изменён\n", "mailbox %s updated\n"), args[0])
		}
		return nil
	}}
	set.Flags().StringVar(&patch.Password, "password", "", T("новый пароль (без других флагов пароль генерируется)", "new password (with no other flags, one is generated)"))
	set.Flags().StringVar(&name, "name", "", T("имя владельца ящика", "name of the mailbox owner"))
	set.Flags().IntVar(&quota, "quota", 0, T("квота в МБ", "quota in MB"))
	set.Flags().StringVar(&active, "active", "", "true|false")

	var purge bool
	rm := &cobra.Command{Use: "rm <user@example.com>", Short: T("удалить ящик", "delete a mailbox"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.DeleteMailbox(cmd.Context(), args[0], purge); err != nil {
			return err
		}
		fmt.Printf(T("ящик %s удалён\n", "mailbox %s deleted\n"), args[0])
		return nil
	}}
	rm.Flags().BoolVar(&purge, "purge", false, T("удалить и письма с диска", "also delete the messages from disk"))

	c.AddCommand(list, add, set, rm)
	return c
}

func mailAliasCmd() *cobra.Command {
	c := &cobra.Command{Use: "alias", Short: T("почтовые алиасы и catch-all", "mail aliases and catch-all")}
	var domain string
	c.PersistentFlags().StringVar(&domain, "domain", "", T("показывать только этот домен", "show only this domain"))

	list := &cobra.Command{Use: "list", Short: T("список алиасов", "list aliases"), RunE: func(cmd *cobra.Command, _ []string) error {
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
		table([]string{T("АДРЕС", "ADDRESS"), T("КУДА", "DESTINATIONS")}, rows)
		return nil
	}}

	add := &cobra.Command{Use: "add <info@example.com> <dest> [dest...]", Short: T("создать алиас (@example.com — catch-all)", "create an alias (@example.com — catch-all)"), Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
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

	rm := &cobra.Command{Use: "rm <info@example.com>", Short: T("удалить алиас", "delete an alias"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.DeleteMailAlias(cmd.Context(), args[0]); err != nil {
			return err
		}
		fmt.Printf(T("алиас %s удалён\n", "alias %s deleted\n"), args[0])
		return nil
	}}

	c.AddCommand(list, add, rm)
	return c
}

func webmailCmd() *cobra.Command {
	var req apitypes.WebmailRequest
	c := &cobra.Command{Use: "webmail <domain>", Short: T("поставить Roundcube отдельным сайтом панели", "install Roundcube as a separate panel site"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	c.Flags().StringVar(&req.User, "user", "", T("владелец сайта (обязателен для администратора)", "site owner (required for an administrator)"))
	c.Flags().StringVar(&req.PHPVersion, "php", "", T("версия PHP (по умолчанию самая новая установленная)", "PHP version (defaults to the newest installed)"))
	c.Flags().IntVar(&req.Port, "port", 0, T("открыть вебпочту ещё и на этом порту почтового хоста (например 2096) — без своей записи в DNS", "also serve webmail on this port of the mail host (e.g. 2096) — no DNS record of its own"))
	return c
}
