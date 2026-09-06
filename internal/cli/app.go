package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

// appOwner resolves --user or falls back to the caller's own login.
func appOwner(cmd *cobra.Command, login string) (string, error) {
	if login != "" {
		return login, nil
	}
	cl, err := newClient()
	if err != nil {
		return "", err
	}
	me, err := cl.Me(cmd.Context())
	if err != nil {
		return "", err
	}
	if me.UserID == 0 {
		return "", &exitError{code: 2, msg: "укажите --user <login>"}
	}
	return me.Login, nil
}

func appCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "app-сервисы пользователей: gunicorn, node, боты (systemd-юнит от имени пользователя)"}
	var login string
	c.PersistentFlags().StringVar(&login, "user", "", "логин владельца (обязателен для администратора)")

	list := &cobra.Command{Use: "list", Short: "список app-сервисов (администратор без --user видит все)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		var apps []*apitypes.AppStatus
		if login == "" {
			me, err := cl.Me(cmd.Context())
			if err != nil {
				return err
			}
			if me.UserID == 0 {
				apps, err = cl.AllApps(cmd.Context())
			} else {
				apps, err = cl.Apps(cmd.Context(), me.Login)
			}
			if err != nil {
				return err
			}
		} else if apps, err = cl.Apps(cmd.Context(), login); err != nil {
			return err
		}
		if g.json {
			return printJSON(apps)
		}
		rows := make([][]string, 0, len(apps))
		for _, a := range apps {
			state := a.App.Status
			if a.Service != nil {
				state = a.Service.ActiveState + "/" + a.Service.SubState
			}
			enabled := "on"
			if !a.App.Enabled {
				enabled = "off"
			}
			rows = append(rows, []string{a.App.Login, a.App.Name, state, enabled, a.App.Command, firstLine(a.App.LastError, "")})
		}
		table([]string{"USER", "NAME", "STATE", "ENABLED", "COMMAND", "ERROR"}, rows)
		return nil
	}}

	var req apitypes.AppRequest
	var disabled bool
	add := &cobra.Command{Use: "add <name>", Short: "создать app-сервис и запустить его", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		req.Name = args[0]
		if disabled {
			f := false
			req.Enabled = &f
		}
		res, err := cl.CreateApp(cmd.Context(), u, req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("app %s/%s: юнит %s, %s\n", res.App.Login, res.App.Name, unitOf(res), stateOf(res))
		return nil
	}}
	add.Flags().StringVar(&req.Command, "command", "", "команда: абсолютный путь и аргументы")
	add.Flags().StringVar(&req.WorkDir, "workdir", "", "рабочий каталог (по умолчанию ~/data)")
	add.Flags().StringVar(&req.EnvFile, "env-file", "", "EnvironmentFile внутри домашнего каталога")
	add.Flags().StringSliceVar(&req.Env, "env", nil, "переменная KEY=value (можно несколько раз)")
	add.Flags().StringVar(&req.Description, "description", "", "описание")
	add.Flags().StringVar(&req.Restart, "restart", "", "always (по умолчанию), on-failure или no")
	add.Flags().BoolVar(&disabled, "disabled", false, "создать, но не запускать и не включать автозапуск")
	add.MarkFlagRequired("command")

	var upd apitypes.AppUpdateRequest
	var updWorkDir, updEnvFile, updDesc string
	var updEnv []string
	var enable, disable bool
	set := &cobra.Command{Use: "set <name>", Short: "изменить app-сервис и перезапустить", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("workdir") {
			upd.WorkDir = &updWorkDir
		}
		if cmd.Flags().Changed("env-file") {
			upd.EnvFile = &updEnvFile
		}
		if cmd.Flags().Changed("description") {
			upd.Description = &updDesc
		}
		if cmd.Flags().Changed("env") {
			upd.Env = &updEnv
		}
		if enable {
			t := true
			upd.Enabled = &t
		}
		if disable {
			f := false
			upd.Enabled = &f
		}
		res, err := cl.UpdateApp(cmd.Context(), u, args[0], upd)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("app %s/%s: %s\n", res.App.Login, res.App.Name, stateOf(res))
		return nil
	}}
	set.Flags().StringVar(&upd.Command, "command", "", "новая команда")
	set.Flags().StringVar(&updWorkDir, "workdir", "", "рабочий каталог")
	set.Flags().StringVar(&updEnvFile, "env-file", "", "EnvironmentFile (пустая строка — убрать)")
	set.Flags().StringSliceVar(&updEnv, "env", nil, "заменить список переменных KEY=value")
	set.Flags().StringVar(&updDesc, "description", "", "описание")
	set.Flags().StringVar(&upd.Restart, "restart", "", "always, on-failure или no")
	set.Flags().BoolVar(&enable, "enable", false, "включить автозапуск и запустить")
	set.Flags().BoolVar(&disable, "disable", false, "остановить и выключить автозапуск")

	show := &cobra.Command{Use: "show <name>", Short: "показать app-сервис", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		res, err := cl.GetApp(cmd.Context(), u, args[0])
		if err != nil {
			return err
		}
		return printJSON(res)
	}}

	for _, action := range []string{"start", "stop", "restart"} {
		action := action
		short := map[string]string{"start": "запустить", "stop": "остановить", "restart": "перезапустить"}[action]
		c.AddCommand(&cobra.Command{Use: action + " <name>", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			u, err := appOwner(cmd, login)
			if err != nil {
				return err
			}
			res, err := cl.AppAction(cmd.Context(), u, args[0], action)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(res)
			}
			fmt.Printf("app %s/%s: %s\n", res.App.Login, res.App.Name, stateOf(res))
			return nil
		}})
	}

	var lines int
	logs := &cobra.Command{Use: "logs <name>", Short: "журнал app-сервиса (journalctl)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		res, err := cl.AppLogs(cmd.Context(), u, args[0], lines)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Println(strings.Join(res.Lines, "\n"))
		return nil
	}}
	logs.Flags().IntVarP(&lines, "lines", "n", 100, "сколько строк")

	rm := &cobra.Command{Use: "rm <name>", Short: "остановить и удалить app-сервис (файлы остаются)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		return cl.DeleteApp(cmd.Context(), u, args[0])
	}}

	c.AddCommand(list, add, set, show, logs, rm)
	return c
}

func unitOf(a *apitypes.AppStatus) string {
	if a.Service != nil && a.Service.Unit != "" {
		return a.Service.Unit
	}
	return a.App.Unit()
}

func stateOf(a *apitypes.AppStatus) string {
	if a.Service != nil {
		return a.Service.ActiveState + " (" + a.Service.SubState + ")"
	}
	return a.App.Status
}

func stackRealIPCmd() *cobra.Command {
	var cloudflare, noCloudflare, clear bool
	var from []string
	c := &cobra.Command{Use: "real-ip", Short: "доверенные прокси для реального IP клиента (Cloudflare, свои балансировщики)", Long: "Без флагов показывает текущие настройки. С флагами переписывает /etc/nginx/monopanel/http.d/10-real-ip.conf и перезагружает nginx.", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		cur, err := cl.RealIP(cmd.Context())
		if err != nil {
			return err
		}
		if !cloudflare && !noCloudflare && !clear && !cmd.Flags().Changed("from") {
			if g.json {
				return printJSON(cur)
			}
			fmt.Printf("cloudflare: %v\nfrom:       %s\n", cur.Cloudflare, strings.Join(cur.From, ", "))
			return nil
		}
		next := *cur
		if cloudflare {
			next.Cloudflare = true
		}
		if noCloudflare {
			next.Cloudflare = false
		}
		if clear {
			next.From = []string{}
		}
		if cmd.Flags().Changed("from") {
			next.From = from
		}
		res, err := cl.SetRealIP(cmd.Context(), next)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("nginx real_ip обновлён: cloudflare=%v, from=[%s] (%d записей)\n", res.Cloudflare, strings.Join(res.From, ", "), len(res.From))
		return nil
	}}
	c.Flags().BoolVar(&cloudflare, "cloudflare", false, "доверять сетям Cloudflare (CF-Connecting-IP)")
	c.Flags().BoolVar(&noCloudflare, "no-cloudflare", false, "перестать доверять Cloudflare")
	c.Flags().StringSliceVar(&from, "from", nil, "свои прокси IP/CIDR (заменяет список; X-Forwarded-For)")
	c.Flags().BoolVar(&clear, "clear-from", false, "очистить список своих прокси")
	return c
}
