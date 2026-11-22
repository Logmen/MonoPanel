package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/termsafe"
)

// stackUpdateCell is the UPDATE column of mp stack list: the version the
// component's repository offers, and from where.
func stackUpdateCell(c apitypes.StackComponent) string {
	if c.Upgrade == nil {
		return "-"
	}
	s := "→ " + c.Upgrade.To
	if c.Upgrade.Origin != "" {
		s += " (" + c.Upgrade.Origin + ")"
	}
	return s
}

// stackReplaced tells that a component's service, or one of its accounts'
// Apaches, still runs the binary an upgrade replaced: the old code until a
// restart — mp stack upgrade's after its check for nginx, PHP, Apache and
// the database server, one by hand for the rest (fail2ban, the tools).
func stackReplaced(c apitypes.StackComponent) bool {
	return c.Installed && ((c.Service != nil && c.Service.BinaryReplaced) || (c.Instances != nil && c.Instances.Replaced > 0))
}

// printUpdatesHint says how the updates that wait get installed, and the
// restarts that wait, row by row: what mp stack upgrade restarts after its
// check, and what only a restart by hand brings onto the new binary.
// daily is the daily upgrade, asked for only when updates wait (nil: it
// could not be read, and nothing is said of it).
func printUpdatesHint(comps []apitypes.StackComponent, daily func() *apitypes.StackAutoUpgrade) {
	stack, db := false, false
	for _, c := range comps {
		switch {
		case c.Upgrade == nil:
		case c.Upgrade.Component == "db":
			db = true
		default:
			stack = true
		}
	}
	if stack {
		switch auto := daily(); {
		case auto == nil:
			fmt.Println(T("ждут обновления: mp stack upgrade (nginx, PHP, Apache)", "updates wait: mp stack upgrade (nginx, PHP, Apache)"))
		case !auto.Enabled:
			fmt.Println(T("ждут обновления: mp stack upgrade (nginx, PHP, Apache); ежедневное обновление выключено: mp stack auto-upgrade --on",
				"updates wait: mp stack upgrade (nginx, PHP, Apache); the daily upgrade is off: mp stack auto-upgrade --on"))
		default:
			fmt.Printf(T("ждут обновления: mp stack upgrade (nginx, PHP, Apache), или их поставит ежедневное обновление в %02d:00\n",
				"updates wait: mp stack upgrade (nginx, PHP, Apache), or the daily upgrade at %02d:00 installs them\n"), auto.Hour)
		}
	}
	if db {
		fmt.Println(T("обновление сервера баз данных: mp stack upgrade db (сервер перезапустится)", "the database server's update: mp stack upgrade db (the server restarts)"))
	}
	for _, c := range comps {
		if !stackReplaced(c) {
			continue
		}
		name := termsafe.Line(c.Name)
		switch comp := c.UpgradeComponent(); {
		case comp == "db":
			fmt.Printf(T("%s работает на бинарнике, который заменило обновление: mp stack upgrade db перезапустит сервер после mysqld --validate-config (сайты на это время останутся без баз)\n",
				"%s runs a binary an upgrade replaced: mp stack upgrade db restarts the server after mysqld --validate-config (the sites lose their databases meanwhile)\n"), name)
		case comp != "":
			fmt.Printf(T("%s работает на бинарнике, который заменило обновление: mp stack upgrade %s перезапустит его после проверки конфигурации\n",
				"%s runs a binary an upgrade replaced: mp stack upgrade %s restarts it after checking the configuration\n"), name, comp)
		case c.Service != nil && c.Service.Unit != "":
			// mp stack upgrade restarts none of these.
			fmt.Printf(T("%s работает на бинарнике, который заменило обновление: новая версия заработает после systemctl restart %s\n",
				"%s runs a binary an upgrade replaced: the new version takes effect after systemctl restart %s\n"), name, termsafe.Line(c.Service.Unit))
		}
	}
}

func stackUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use: "upgrade [component]",
		Short: T("обновить пакеты панели из их репозиториев: nginx, php, apache, db или all (по умолчанию all: nginx, PHP и Apache; сервер БД — только как db)",
			"upgrade the panel's packages from their repositories: nginx, php, apache, db or all (the default, all: nginx, PHP and Apache; the database server only as db)"),
		Long: T("Ставит то, что предлагают репозитории, которые подключила панель (nginx.org, Sury или ppa:ondrej/php, Remi, Percona или MySQL), и только для её уже установленных пакетов. Потом проверяет конфигурацию новой версией (nginx -t, php-fpm -t, apache2 -t, mysqld --validate-config) и перезагружает или перезапускает службы: nginx перезагружается (пакет nginx.org сам подменяет работающий бинарник), php-fpm каждой обновлённой ветки перезапускается, Apache каждого аккаунта — мягко перезапускается или перезапускается, если работает заменённый бинарник. Если проверка не прошла, служба работает как работала, ничего не откатывается, а задача завершается ошибкой. Сервер баз данных обновляется только по mp stack upgrade db: его перезапуск — это простой сайтов.",
			"Installs what the repositories the panel added offer (nginx.org, Sury or ppa:ondrej/php, Remi, Percona or MySQL), for the panel's packages that are installed only. Then checks the configuration with the new version (nginx -t, php-fpm -t, apache2 -t, mysqld --validate-config) and reloads or restarts the services: nginx reloads (nginx.org's package swaps the running binary itself), the php-fpm of every upgraded branch restarts, each account's Apache gets a graceful restart, or a restart when it runs the replaced binary. When a check fails, the service runs on as it was, nothing is rolled back and the job fails. The database server is upgraded only with mp stack upgrade db: its restart is downtime for the sites."),
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"nginx", "php", "apache", "db", "all"},
		RunE: func(cmd *cobra.Command, args []string) error {
			comp := "all"
			if len(args) == 1 {
				comp = args[0]
			}
			cl, err := newClient()
			if err != nil {
				return err
			}
			ref, err := cl.StackUpgrade(cmd.Context(), comp)
			if err != nil {
				return err
			}
			return followJob(cmd, cl, ref.JobID)
		},
	}
}

func stackAutoUpgradeCmd() *cobra.Command {
	var on, off bool
	var hour int
	c := &cobra.Command{Use: "auto-upgrade", Short: T("ежедневное обновление nginx, PHP и Apache: без флагов показать, с --on, --off, --hour изменить", "the daily upgrade of nginx, PHP and Apache: show it without flags, change it with --on, --off, --hour"),
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if err := apart(cmd, "on", "off"); err != nil {
				return err
			}
			cl, err := newClient()
			if err != nil {
				return err
			}
			var st *apitypes.StackAutoUpgrade
			if cmd.Flags().Changed("on") || cmd.Flags().Changed("off") || cmd.Flags().Changed("hour") {
				var req apitypes.StackAutoUpgradeUpdate
				if cmd.Flags().Changed("on") {
					req.Enabled = &on
				}
				if cmd.Flags().Changed("off") {
					enabled := !off
					req.Enabled = &enabled
				}
				if cmd.Flags().Changed("hour") {
					req.Hour = &hour
				}
				st, err = cl.SetStackAutoUpgrade(cmd.Context(), req)
			} else {
				st, err = cl.StackAutoUpgrade(cmd.Context())
			}
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(st)
			}
			printAutoUpgrade(st)
			return nil
		}}
	c.Flags().BoolVar(&on, "on", false, T("включить", "switch it on"))
	c.Flags().BoolVar(&off, "off", false, T("выключить: обновлять только вручную (mp stack upgrade)", "switch it off: upgrade by hand only (mp stack upgrade)"))
	c.Flags().IntVar(&hour, "hour", 4, T("час запуска по времени сервера, 0–23", "the hour it runs, server time, 0–23"))
	return c
}

// printAutoUpgrade shows the daily upgrade and the last upgrade's outcome;
// the times are the server's (the hour is in its zone).
func printAutoUpgrade(st *apitypes.StackAutoUpgrade) {
	if st.Enabled {
		fmt.Printf(T("ежедневное обновление nginx, PHP и Apache: включено, в %02d:00 (%s)\n", "daily upgrade of nginx, PHP and Apache: on, at %02d:00 (%s)\n"), st.Hour, termsafe.Line(st.Timezone))
		if st.NextRun != nil {
			fmt.Printf(T("следующий запуск: %s\n", "next run: %s\n"), st.NextRun.Format("2006-01-02 15:04"))
		}
	} else {
		fmt.Printf(T("ежедневное обновление nginx, PHP и Apache: выключено (час %02d:00, %s); вручную: mp stack upgrade\n", "daily upgrade of nginx, PHP and Apache: off (hour %02d:00, %s); by hand: mp stack upgrade\n"), st.Hour, termsafe.Line(st.Timezone))
	}
	l := st.Last
	if l == nil {
		return
	}
	how := T("вручную", "by hand")
	if l.Scheduled {
		how = T("по расписанию", "scheduled")
	}
	fmt.Printf(T("последнее обновление: %s, %s, задача #%d\n", "last upgrade: %s, %s, job #%d\n"), l.At.Local().Format("2006-01-02 15:04"), how, l.JobID)
	for _, u := range l.Upgraded {
		fmt.Println("  " + termsafe.Line(u))
	}
	switch {
	case l.Error != "":
		fmt.Printf(T("  ошибка: %s\n", "  error: %s\n"), termsafe.Line(l.Error))
	case len(l.Upgraded) == 0:
		fmt.Println(T("  обновлять было нечего", "  nothing to upgrade"))
	}
}
