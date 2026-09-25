package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

func cronCmd() *cobra.Command {
	c := &cobra.Command{Use: "cron", Short: T("задания cron пользователей", "users' cron jobs")}
	var login string
	c.PersistentFlags().StringVar(&login, "user", "", T("логин пользователя (обязателен администратору для изменений; list без него показывает всех)", "user login (required for changes by an administrator; list without it shows everyone's)"))
	whose := func() (string, error) {
		if login != "" {
			return login, nil
		}
		cl, err := newClient()
		if err != nil {
			return "", err
		}
		me, err := cl.Me(cmd0.Context())
		if err != nil {
			return "", err
		}
		if me.UserID == 0 {
			return "", &exitError{code: 2, msg: T("укажите --user <login>", "specify --user <login>")}
		}
		return me.Login, nil
	}
	list := &cobra.Command{Use: "list", Short: T("список заданий (администратор без --user видит всех)", "list cron jobs (an administrator without --user sees everyone's)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cmd0 = cmd
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, all := login, false
		if u == "" {
			me, err := cl.Me(cmd.Context())
			if err != nil {
				return err
			}
			u, all = me.Login, me.UserID == 0
		}
		var jobs []*store.CronJob
		if all {
			jobs, err = cl.AllCronJobs(cmd.Context())
		} else {
			jobs, err = cl.CronJobs(cmd.Context(), u)
		}
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(jobs)
		}
		rows := make([][]string, 0, len(jobs))
		for _, j := range jobs {
			state := "on"
			if !j.Enabled {
				state = "off"
			}
			row := []string{strconv.FormatInt(j.ID, 10), state, j.Schedule, j.Command, j.Comment}
			if all {
				row = append([]string{j.Login}, row...)
			}
			rows = append(rows, row)
		}
		head := []string{"ID", "STATE", "SCHEDULE", "COMMAND", "COMMENT"}
		if all {
			head = append([]string{"USER"}, head...)
		}
		table(head, rows)
		return nil
	}}
	var req apitypes.CronRequest
	var disabled bool
	add := &cobra.Command{Use: "add", Short: T("добавить задание", "add a cron job"), RunE: func(cmd *cobra.Command, _ []string) error {
		cmd0 = cmd
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := whose()
		if err != nil {
			return err
		}
		if disabled {
			f := false
			req.Enabled = &f
		}
		j, err := cl.CronAdd(cmd.Context(), u, req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(j)
		}
		fmt.Printf(T("задание #%d добавлено в crontab %s\n", "cron job #%d added to %s's crontab\n"), j.ID, j.Login)
		return nil
	}}
	add.Flags().StringVar(&req.Schedule, "schedule", "", T("расписание, например \"*/5 * * * *\" или @daily", "schedule, e.g. \"*/5 * * * *\" or @daily"))
	add.Flags().StringVar(&req.Command, "command", "", T("команда (PATH начинается с ~/data/bin, где php — версия сайта)", "command (PATH starts with ~/data/bin, where php is the site's PHP version)"))
	add.Flags().StringVar(&req.Comment, "comment", "", T("комментарий", "comment"))
	add.Flags().BoolVar(&disabled, "disabled", false, T("создать выключенным", "create it disabled"))
	add.MarkFlagRequired("schedule")
	add.MarkFlagRequired("command")
	for _, action := range []string{"enable", "disable", "rm"} {
		action := action
		c.AddCommand(&cobra.Command{Use: action + " <id>", Short: map[string]string{"enable": T("включить задание", "enable a cron job"), "disable": T("выключить задание", "disable a cron job"), "rm": T("удалить задание", "delete a cron job")}[action], Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cmd0 = cmd
			cl, err := newClient()
			if err != nil {
				return err
			}
			u, err := whose()
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return &exitError{code: 2, msg: T("id должен быть числом", "id must be a number")}
			}
			if action == "rm" {
				return cl.CronDelete(cmd.Context(), u, id)
			}
			on := action == "enable"
			_, err = cl.CronUpdate(cmd.Context(), u, id, apitypes.CronUpdateRequest{Enabled: &on})
			return err
		}})
	}
	c.AddCommand(list, add)
	return c
}

var cmd0 *cobra.Command

func firewallCmd() *cobra.Command {
	c := &cobra.Command{Use: "firewall", Short: T("nftables-firewall и fail2ban", "nftables firewall and fail2ban")}
	printStatus := func(st *apitypes.FirewallStatus) error {
		if g.json {
			return printJSON(st)
		}
		state := T("выключен", "disabled")
		if st.Enabled {
			state = T("включён", "enabled")
			if !st.Active {
				state += T(" (таблица не загружена!)", " (table not loaded!)")
			}
		}
		ports := make([]string, 0, len(st.SSHPorts))
		for _, p := range st.SSHPorts {
			ports = append(ports, strconv.Itoa(p))
		}
		fmt.Printf(T("firewall: %s · policy drop · всегда открыты: ssh %s, 80, 443, панель %d\n", "firewall: %s · policy drop · always open: ssh %s, 80, 443, panel %d\n"), state, strings.Join(ports, ","), st.PanelPort)
		for _, r := range st.Restricted {
			only := T("никого — deny без allow с источником", "no one — a deny with no per-source allow")
			if len(r.Sources) > 0 {
				only = strings.Join(r.Sources, ", ")
			}
			fmt.Printf(T("  порт %d закрыт для всех, кроме: %s\n", "  port %d is closed to everyone except: %s\n"), r.Port, only)
		}
		rows := make([][]string, 0, len(st.Rules))
		for _, r := range st.Rules {
			rows = append(rows, []string{strconv.FormatInt(r.ID, 10), r.Kind, r.Proto, r.Port, r.Source, r.Comment})
		}
		if len(rows) > 0 {
			table([]string{"ID", "KIND", "PROTO", "PORT", "SOURCE", "COMMENT"}, rows)
		}
		if st.Fail2ban != nil {
			fmt.Printf(T("fail2ban: установлен, running=%v\n", "fail2ban: installed, running=%v\n"), st.Fail2ban.Running)
			for _, j := range st.Fail2ban.Jails {
				fmt.Printf("  jail %-16s banned now %d, total %d %s\n", j.Name, j.Banned, j.Total, strings.Join(j.IPs, " "))
			}
		} else {
			fmt.Println(T("fail2ban: не установлен (mp stack install fail2ban)", "fail2ban: not installed (mp stack install fail2ban)"))
		}
		return nil
	}
	status := &cobra.Command{Use: "status", Short: T("состояние", "current state"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.Firewall(cmd.Context())
		if err != nil {
			return err
		}
		return printStatus(st)
	}}
	for _, action := range []string{"enable", "disable", "apply"} {
		action := action
		c.AddCommand(&cobra.Command{Use: action, Short: map[string]string{"enable": T("включить (policy drop, SSH/80/443/панель открыты)", "enable (policy drop, SSH/80/443/panel open)"), "disable": T("выключить (удалить таблицу)", "disable (remove the table)"), "apply": T("перегенерировать и применить", "regenerate and apply")}[action], RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			st, err := cl.FirewallAction(cmd.Context(), action)
			if err != nil {
				return err
			}
			return printStatus(st)
		}})
	}
	var rule apitypes.FirewallRuleRequest
	allow := &cobra.Command{Use: "allow", Short: T("открыть порт (опционально для источника)", "open a port (optionally for a source)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		rule.Kind = "allow"
		r, err := cl.FirewallRuleAdd(cmd.Context(), rule)
		if err != nil {
			return err
		}
		fmt.Printf(T("правило #%d добавлено\n", "rule #%d added\n"), r.ID)
		return nil
	}}
	allow.Flags().StringVar(&rule.Port, "port", "", T("порт или диапазон N-M", "port or range N-M"))
	allow.Flags().StringVar(&rule.Proto, "proto", "tcp", T("tcp, udp или any", "tcp, udp or any"))
	allow.Flags().StringVar(&rule.Source, "source", "", T("IP или CIDR", "IP or CIDR"))
	allow.Flags().StringVar(&rule.Comment, "comment", "", T("комментарий", "comment"))
	var deny apitypes.FirewallRuleRequest
	denyCmd := &cobra.Command{Use: "deny", Short: T("запретить источник (опционально порт)", "deny a source (optionally a port)"), Long: T(`Запретить источник, порт или порт для источника.

Порядок проверки в цепочке: сначала allow с источником, затем все deny, затем
всегда открытые порты и allow без источника. Поэтому deny на порт без
источника закрывает его для всех, кроме адресов из allow с источником —
так панель или SSH ограничиваются VPN:

  mp firewall allow --port 8443 --source 203.0.113.5
  mp firewall deny  --port 8443

Для SSH и порта панели deny без источника принимается только когда такой
allow уже есть, а deny, накрывающий ваш текущий адрес, отклоняется — так
нельзя запереть самого себя.`, `Deny a source, a port or a port for a source.

Order of checks in the chain: allow rules with a source first, then all deny
rules, then the always-open ports and allow rules without a source. A deny on
a port without a source therefore closes it to everyone except the sources
allowed explicitly — that is how the panel or SSH is limited to a VPN:

  mp firewall allow --port 8443 --source 203.0.113.5
  mp firewall deny  --port 8443

For SSH and the panel port a deny without a source is accepted only once an
allow with a source exists, and a deny covering your current address is
refused, so you cannot lock yourself out.`), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		deny.Kind = "deny"
		r, err := cl.FirewallRuleAdd(cmd.Context(), deny)
		if err != nil {
			return err
		}
		fmt.Printf(T("правило #%d добавлено\n", "rule #%d added\n"), r.ID)
		return nil
	}}
	denyCmd.Flags().StringVar(&deny.Port, "port", "", T("порт или диапазон", "port or range"))
	denyCmd.Flags().StringVar(&deny.Proto, "proto", "any", T("tcp, udp или any", "tcp, udp or any"))
	denyCmd.Flags().StringVar(&deny.Source, "source", "", T("IP или CIDR", "IP or CIDR"))
	denyCmd.Flags().StringVar(&deny.Comment, "comment", "", T("комментарий", "comment"))
	rm := &cobra.Command{Use: "rm <id>", Short: T("удалить правило", "delete a rule"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: T("id должен быть числом", "id must be a number")}
		}
		return cl.FirewallRuleDelete(cmd.Context(), id)
	}}
	for _, action := range []string{"ban", "unban"} {
		action := action
		c.AddCommand(&cobra.Command{Use: action + " <ip>", Short: action + T(" адреса", " an address"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			st, err := cl.FirewallBan(cmd.Context(), action, args[0])
			if err != nil {
				return err
			}
			return printStatus(st)
		}})
	}
	c.AddCommand(status, allow, denyCmd, rm)
	return c
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: T("диагностика: сервисы, конфиги, диск, сертификаты, DNS, дрейф", "diagnostics: services, configs, disk, certificates, DNS, drift"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		d, err := cl.Doctor(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(d)
		}
		fails := 0
		for _, c := range d.Checks {
			mark := map[string]string{"ok": " ok ", "warn": "WARN", "fail": "FAIL"}[c.Status]
			if c.Status == "fail" {
				fails++
			}
			fmt.Printf("[%s] %-32s %s\n", mark, c.Name, c.Detail)
		}
		fmt.Println(d.Summary)
		if fails > 0 {
			return &exitError{code: 3}
		}
		return nil
	}}
}

func logsCmd() *cobra.Command {
	var lines int
	c := &cobra.Command{Use: "logs <unit>", Short: T("журнал сервиса: nginx, apache2, mysql, php8.4-fpm, monopanel-api, fail2ban …", "service journal: nginx, apache2, mysql, php8.4-fpm, monopanel-api, fail2ban …"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		t, err := cl.ServiceLogs(cmd.Context(), args[0], lines)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(t)
		}
		for _, l := range t.Lines {
			fmt.Println(l)
		}
		return nil
	}}
	c.Flags().IntVarP(&lines, "lines", "n", 100, T("сколько строк", "number of lines"))
	return c
}

func siteLogsCmd() *cobra.Command {
	var lines int
	var typ string
	c := &cobra.Command{Use: "logs <domain>", Short: T("хвост лога сайта", "tail of a site's log"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		t, err := cl.SiteLogs(cmd.Context(), args[0], typ, lines)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(t)
		}
		fmt.Fprintf(os.Stderr, T("%s (%d байт)\n", "%s (%d bytes)\n"), t.Path, t.Size)
		for _, l := range t.Lines {
			fmt.Println(l)
		}
		return nil
	}}
	c.Flags().IntVarP(&lines, "lines", "n", 100, T("сколько строк", "number of lines"))
	c.Flags().StringVar(&typ, "type", "access", "access, error, php, slow, apache-access, apache-error")
	return c
}

func metricsCmd() *cobra.Command {
	var rng string
	c := &cobra.Command{Use: "metrics", Short: T("метрики хоста (CPU, load, память, диск, сеть)", "host metrics (CPU, load, memory, disk, network)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		m, err := cl.Metrics(cmd.Context(), rng)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(m)
		}
		rows := make([][]string, 0, len(m.Points))
		for _, p := range m.Points {
			rows = append(rows, []string{time.Unix(p.TS, 0).Local().Format("01-02 15:04"), fmt.Sprintf("%.1f%%", p.CPU), fmt.Sprintf("%.2f", p.Load1),
				humanBytes(uint64(p.MemUsed)) + "/" + humanBytes(uint64(p.MemTotal)), humanBytes(uint64(p.DiskUsed)) + "/" + humanBytes(uint64(p.DiskTotal)),
				humanBytes(uint64(p.NetRx)), humanBytes(uint64(p.NetTx))})
		}
		if len(rows) == 0 {
			fmt.Println(T("данных пока нет (сэмплер пишет точку раз в минуту)", "no data yet (the sampler records a point once a minute)"))
			return nil
		}
		table([]string{"TIME", "CPU", "LOAD1", "MEM", "DISK", "RX", "TX"}, rows)
		return nil
	}}
	c.Flags().StringVar(&rng, "range", "1h", "1h, 6h, 24h, 7d, 30d")
	return c
}
