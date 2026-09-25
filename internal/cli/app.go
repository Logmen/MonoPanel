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
		return "", &exitError{code: 2, msg: T("укажите --user <login>", "specify --user <login>")}
	}
	return me.Login, nil
}

func appCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: T("app-сервисы пользователей: gunicorn, node, боты (systemd-юнит от имени пользователя)", "users' app services: gunicorn, node, bots (a systemd unit running as the user)")}
	var login string
	c.PersistentFlags().StringVar(&login, "user", "", T("логин владельца (обязателен для администратора)", "owner's login (required for an administrator)"))

	list := &cobra.Command{Use: "list", Short: T("список app-сервисов (администратор без --user видит все)", "list app services (an administrator without --user sees everyone's)"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	add := &cobra.Command{Use: "add <name>", Short: T("создать app-сервис и запустить его", "create an app service and start it"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf(T("app %s/%s: юнит %s, %s\n", "app %s/%s: unit %s, %s\n"), res.App.Login, res.App.Name, unitOf(res), stateOf(res))
		return nil
	}}
	add.Flags().StringVar(&req.Command, "command", "", T("команда: абсолютный путь и аргументы", "command: absolute path and arguments"))
	add.Flags().StringVar(&req.WorkDir, "workdir", "", T("рабочий каталог (по умолчанию ~/data)", "working directory (defaults to ~/data)"))
	add.Flags().StringVar(&req.EnvFile, "env-file", "", T("EnvironmentFile внутри домашнего каталога", "EnvironmentFile inside the home directory"))
	add.Flags().StringSliceVar(&req.Env, "env", nil, T("переменная KEY=value (можно несколько раз)", "variable KEY=value (can be repeated)"))
	add.Flags().StringVar(&req.Description, "description", "", T("описание", "description"))
	add.Flags().StringVar(&req.Restart, "restart", "", T("always (по умолчанию), on-failure или no", "always (default), on-failure or no"))
	add.Flags().BoolVar(&disabled, "disabled", false, T("создать, но не запускать и не включать автозапуск", "create, but neither start it nor enable autostart"))
	add.MarkFlagRequired("command")

	var upd apitypes.AppUpdateRequest
	var updWorkDir, updEnvFile, updDesc string
	var updEnv []string
	var enable, disable bool
	set := &cobra.Command{Use: "set <name>", Short: T("изменить app-сервис и перезапустить", "change an app service and restart it"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	set.Flags().StringVar(&upd.Command, "command", "", T("новая команда", "new command"))
	set.Flags().StringVar(&updWorkDir, "workdir", "", T("рабочий каталог", "working directory"))
	set.Flags().StringVar(&updEnvFile, "env-file", "", T("EnvironmentFile (пустая строка — убрать)", "EnvironmentFile (an empty string removes it)"))
	set.Flags().StringSliceVar(&updEnv, "env", nil, T("заменить список переменных KEY=value", "replace the list of KEY=value variables"))
	set.Flags().StringVar(&updDesc, "description", "", T("описание", "description"))
	set.Flags().StringVar(&upd.Restart, "restart", "", T("always, on-failure или no", "always, on-failure or no"))
	set.Flags().BoolVar(&enable, "enable", false, T("включить автозапуск и запустить", "enable autostart and start"))
	set.Flags().BoolVar(&disable, "disable", false, T("остановить и выключить автозапуск", "stop and disable autostart"))

	show := &cobra.Command{Use: "show <name>", Short: T("показать app-сервис", "show an app service"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		short := map[string]string{"start": T("запустить", "start an app service"), "stop": T("остановить", "stop an app service"), "restart": T("перезапустить", "restart an app service")}[action]
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
	logs := &cobra.Command{Use: "logs <name>", Short: T("журнал app-сервиса (journalctl)", "app service log (journalctl)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	logs.Flags().IntVarP(&lines, "lines", "n", 100, T("сколько строк", "number of lines"))

	rm := &cobra.Command{Use: "rm <name>", Short: T("остановить и удалить app-сервис (файлы остаются)", "stop and delete an app service (the files stay)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	c := &cobra.Command{Use: "real-ip", Short: T("доверенные прокси для реального IP клиента (Cloudflare, свои балансировщики)", "trusted proxies for the client's real IP (Cloudflare, your own load balancers)"), Long: T("Без флагов показывает текущие настройки. С флагами переписывает /etc/nginx/monopanel/http.d/10-real-ip.conf и перезагружает nginx.", "Without flags, shows the current settings. With flags, rewrites /etc/nginx/monopanel/http.d/10-real-ip.conf and reloads nginx."), RunE: func(cmd *cobra.Command, _ []string) error {
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
		fmt.Printf(T("nginx real_ip обновлён: cloudflare=%v, from=[%s] (%d записей)\n", "nginx real_ip updated: cloudflare=%v, from=[%s] (%d entries)\n"), res.Cloudflare, strings.Join(res.From, ", "), len(res.From))
		return nil
	}}
	c.Flags().BoolVar(&cloudflare, "cloudflare", false, T("доверять сетям Cloudflare (CF-Connecting-IP)", "trust Cloudflare networks (CF-Connecting-IP)"))
	c.Flags().BoolVar(&noCloudflare, "no-cloudflare", false, T("перестать доверять Cloudflare", "stop trusting Cloudflare"))
	c.Flags().StringSliceVar(&from, "from", nil, T("свои прокси IP/CIDR (заменяет список; X-Forwarded-For)", "your own proxies, IP/CIDR (replaces the list; X-Forwarded-For)"))
	c.Flags().BoolVar(&clear, "clear-from", false, T("очистить список своих прокси", "clear the list of your own proxies"))
	return c
}
