package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func cronCmd() *cobra.Command {
	c := &cobra.Command{Use: "cron", Short: "задания cron пользователей"}
	var login string
	c.PersistentFlags().StringVar(&login, "user", "", "логин пользователя (обязателен для администратора)")
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
			return "", &exitError{code: 2, msg: "укажите --user <login>"}
		}
		return me.Login, nil
	}
	list := &cobra.Command{Use: "list", Short: "список заданий", RunE: func(cmd *cobra.Command, _ []string) error {
		cmd0 = cmd
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := whose()
		if err != nil {
			return err
		}
		jobs, err := cl.CronJobs(cmd.Context(), u)
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
			rows = append(rows, []string{strconv.FormatInt(j.ID, 10), state, j.Schedule, j.Command, j.Comment})
		}
		table([]string{"ID", "STATE", "SCHEDULE", "COMMAND", "COMMENT"}, rows)
		return nil
	}}
	var req apitypes.CronRequest
	var disabled bool
	add := &cobra.Command{Use: "add", Short: "добавить задание", RunE: func(cmd *cobra.Command, _ []string) error {
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
		fmt.Printf("задание #%d добавлено в crontab %s\n", j.ID, j.Login)
		return nil
	}}
	add.Flags().StringVar(&req.Schedule, "schedule", "", "расписание, например \"*/5 * * * *\" или @daily")
	add.Flags().StringVar(&req.Command, "command", "", "команда (PATH начинается с ~/data/bin, где php — версия сайта)")
	add.Flags().StringVar(&req.Comment, "comment", "", "комментарий")
	add.Flags().BoolVar(&disabled, "disabled", false, "создать выключенным")
	add.MarkFlagRequired("schedule")
	add.MarkFlagRequired("command")
	for _, action := range []string{"enable", "disable", "rm"} {
		action := action
		c.AddCommand(&cobra.Command{Use: action + " <id>", Short: map[string]string{"enable": "включить", "disable": "выключить", "rm": "удалить"}[action] + " задание", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
				return &exitError{code: 2, msg: "id должен быть числом"}
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
	c := &cobra.Command{Use: "firewall", Short: "nftables-firewall и fail2ban"}
	printStatus := func(st *apitypes.FirewallStatus) error {
		if g.json {
			return printJSON(st)
		}
		state := "выключен"
		if st.Enabled {
			state = "включён"
			if !st.Active {
				state += " (таблица не загружена!)"
			}
		}
		ports := make([]string, 0, len(st.SSHPorts))
		for _, p := range st.SSHPorts {
			ports = append(ports, strconv.Itoa(p))
		}
		fmt.Printf("firewall: %s · policy drop · всегда открыты: ssh %s, 80, 443, панель %d\n", state, strings.Join(ports, ","), st.PanelPort)
		rows := make([][]string, 0, len(st.Rules))
		for _, r := range st.Rules {
			rows = append(rows, []string{strconv.FormatInt(r.ID, 10), r.Kind, r.Proto, r.Port, r.Source, r.Comment})
		}
		if len(rows) > 0 {
			table([]string{"ID", "KIND", "PROTO", "PORT", "SOURCE", "COMMENT"}, rows)
		}
		if st.Fail2ban != nil {
			fmt.Printf("fail2ban: установлен, running=%v\n", st.Fail2ban.Running)
			for _, j := range st.Fail2ban.Jails {
				fmt.Printf("  jail %-16s banned now %d, total %d %s\n", j.Name, j.Banned, j.Total, strings.Join(j.IPs, " "))
			}
		} else {
			fmt.Println("fail2ban: не установлен (mp stack install fail2ban)")
		}
		return nil
	}
	status := &cobra.Command{Use: "status", Short: "состояние", RunE: func(cmd *cobra.Command, _ []string) error {
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
		c.AddCommand(&cobra.Command{Use: action, Short: map[string]string{"enable": "включить (policy drop, SSH/80/443/панель открыты)", "disable": "выключить (удалить таблицу)", "apply": "перегенерировать и применить"}[action], RunE: func(cmd *cobra.Command, _ []string) error {
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
	allow := &cobra.Command{Use: "allow", Short: "открыть порт (опционально для источника)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		rule.Kind = "allow"
		r, err := cl.FirewallRuleAdd(cmd.Context(), rule)
		if err != nil {
			return err
		}
		fmt.Printf("правило #%d добавлено\n", r.ID)
		return nil
	}}
	allow.Flags().StringVar(&rule.Port, "port", "", "порт или диапазон N-M")
	allow.Flags().StringVar(&rule.Proto, "proto", "tcp", "tcp, udp или any")
	allow.Flags().StringVar(&rule.Source, "source", "", "IP или CIDR")
	allow.Flags().StringVar(&rule.Comment, "comment", "", "комментарий")
	var deny apitypes.FirewallRuleRequest
	denyCmd := &cobra.Command{Use: "deny", Short: "запретить источник (опционально порт)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		deny.Kind = "deny"
		r, err := cl.FirewallRuleAdd(cmd.Context(), deny)
		if err != nil {
			return err
		}
		fmt.Printf("правило #%d добавлено\n", r.ID)
		return nil
	}}
	denyCmd.Flags().StringVar(&deny.Port, "port", "", "порт или диапазон")
	denyCmd.Flags().StringVar(&deny.Proto, "proto", "any", "tcp, udp или any")
	denyCmd.Flags().StringVar(&deny.Source, "source", "", "IP или CIDR")
	denyCmd.Flags().StringVar(&deny.Comment, "comment", "", "комментарий")
	rm := &cobra.Command{Use: "rm <id>", Short: "удалить правило", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: "id должен быть числом"}
		}
		return cl.FirewallRuleDelete(cmd.Context(), id)
	}}
	for _, action := range []string{"ban", "unban"} {
		action := action
		c.AddCommand(&cobra.Command{Use: action + " <ip>", Short: action + " адреса", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	return &cobra.Command{Use: "doctor", Short: "диагностика: сервисы, конфиги, диск, сертификаты, DNS, дрейф", RunE: func(cmd *cobra.Command, _ []string) error {
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
	c := &cobra.Command{Use: "logs <unit>", Short: "журнал сервиса: nginx, apache2, mysql, php8.4-fpm, monopanel-api, fail2ban …", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	c.Flags().IntVarP(&lines, "lines", "n", 100, "сколько строк")
	return c
}

func siteLogsCmd() *cobra.Command {
	var lines int
	var typ string
	c := &cobra.Command{Use: "logs <domain>", Short: "хвост лога сайта", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Fprintf(os.Stderr, "%s (%d байт)\n", t.Path, t.Size)
		for _, l := range t.Lines {
			fmt.Println(l)
		}
		return nil
	}}
	c.Flags().IntVarP(&lines, "lines", "n", 100, "сколько строк")
	c.Flags().StringVar(&typ, "type", "access", "access, error, php, slow, apache-access, apache-error")
	return c
}

func metricsCmd() *cobra.Command {
	var rng string
	c := &cobra.Command{Use: "metrics", Short: "метрики хоста (CPU, load, память, диск, сеть)", RunE: func(cmd *cobra.Command, _ []string) error {
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
			fmt.Println("данных пока нет (сэмплер пишет точку раз в минуту)")
			return nil
		}
		table([]string{"TIME", "CPU", "LOAD1", "MEM", "DISK", "RX", "TX"}, rows)
		return nil
	}}
	c.Flags().StringVar(&rng, "range", "1h", "1h, 6h, 24h, 7d, 30d")
	return c
}
