package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/client"
	"monopanel/internal/jobs"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

func humanBytes(b uint64) string {
	const unit = 1024.0
	f := float64(b)
	for _, s := range []string{"Б", "КБ", "МБ", "ГБ", "ТБ"} {
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= unit
	}
	return fmt.Sprintf("%.1f ПБ", f)
}

func humanDuration(sec float64) string {
	d := time.Duration(sec) * time.Second
	days := int(d.Hours()) / 24
	if days > 0 {
		return fmt.Sprintf("%dд %dч", days, int(d.Hours())%24)
	}
	return fmt.Sprintf("%dч %dм", int(d.Hours()), int(d.Minutes())%60)
}

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "сводка: панель, хост, сервисы, задачи", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.Status(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		fmt.Printf("MonoPanel %s, работает %s, схема БД v%d\n", st.Panel.Version, humanDuration(st.Panel.UptimeSeconds), st.Panel.SchemaVersion)
		if st.Panel.AgentOK {
			fmt.Printf("Агент:     работает (%s)\n", st.Panel.AgentVersion)
		} else {
			fmt.Printf("Агент:     НЕДОСТУПЕН: %s\n", st.Panel.AgentError)
		}
		if h := st.Host; h != nil {
			fmt.Printf("Хост:      %s · %s · ядро %s · %d CPU · uptime %s\n", h.Hostname, h.Release.PrettyName, h.Kernel, h.CPUs, humanDuration(h.UptimeSeconds))
			fmt.Printf("Нагрузка:  %.2f %.2f %.2f · память %s из %s\n", h.Load[0], h.Load[1], h.Load[2], humanBytes(h.MemTotalBytes-h.MemAvailableBytes), humanBytes(h.MemTotalBytes))
			for _, d := range h.Disks {
				fmt.Printf("Диск %-5s свободно %s из %s\n", d.Mount, humanBytes(d.FreeBytes), humanBytes(d.TotalBytes))
			}
		}
		if len(st.Services) > 0 {
			parts := make([]string, 0, len(st.Services))
			for _, s := range st.Services {
				parts = append(parts, strings.TrimSuffix(s.Unit, ".service")+"="+s.ActiveState)
			}
			fmt.Println("Сервисы:  ", strings.Join(parts, " "))
		}
		fmt.Printf("Аккаунты:  admin=%d user=%d · задачи: queued=%d running=%d done=%d failed=%d\n",
			st.Panel.Users["admin"], st.Panel.Users["user"], st.Panel.Jobs["queued"], st.Panel.Jobs["running"], st.Panel.Jobs["done"], st.Panel.Jobs["failed"])
		return nil
	}}
}

func followJob(cmd *cobra.Command, cl *client.Client, id int64) error {
	if g.noWait {
		if g.json {
			return printJSON(map[string]int64{"job_id": id})
		}
		fmt.Printf("задача #%d поставлена в очередь\n", id)
		return nil
	}
	if !g.json {
		fmt.Fprintf(os.Stderr, "задача #%d\n", id)
	}
	job, err := cl.WaitJob(cmd.Context(), id, func(e jobs.Event) {
		if g.json {
			return
		}
		switch e.Type {
		case "log":
			fmt.Fprintln(os.Stderr, "   ", e.Line)
		case "progress":
			fmt.Fprintf(os.Stderr, "  [%3d%%] %s\n", e.Progress, e.Message)
		}
	})
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(job)
	}
	if job.Status == store.JobFailed {
		return &exitError{code: 3, msg: fmt.Sprintf("задача #%d завершилась с ошибкой: %s", id, job.Error)}
	}
	fmt.Fprintf(os.Stderr, "задача #%d: %s\n", id, job.Status)
	return nil
}

func userCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: "аккаунты панели и unix-пользователи"}
	var req apitypes.CreateUserRequest
	var passwordStdin, generate bool
	add := &cobra.Command{Use: "add <login>", Short: "создать пользователя (unix-пользователь и каталоги создаются задачей)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Login = args[0]
		if passwordStdin {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			req.Password = strings.TrimRight(line, "\r\n")
		}
		var shown string
		if generate && req.Password == "" {
			req.Password, _ = auth.NewToken(15)
			shown = req.Password
		}
		res, err := cl.CreateUser(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			job, jerr := cl.WaitJob(cmd.Context(), res.JobID, nil)
			out := map[string]any{"user": res.User, "job": job}
			if shown != "" {
				out["password"] = shown
			}
			if err := printJSON(out); err != nil {
				return err
			}
			if jerr != nil {
				return jerr
			}
			if job.Status == store.JobFailed {
				return &exitError{code: 3, msg: job.Error}
			}
			return nil
		}
		fmt.Printf("пользователь %s создан (id %d)\n", res.User.Login, res.User.ID)
		if shown != "" {
			fmt.Printf("пароль: %s\n", shown)
		}
		return followJob(cmd, cl, res.JobID)
	}}
	add.Flags().StringVar(&req.Password, "password", "", "пароль для входа в панель")
	add.Flags().BoolVar(&passwordStdin, "password-stdin", false, "прочитать пароль из stdin")
	add.Flags().BoolVar(&generate, "generate", false, "сгенерировать пароль и показать его")
	add.Flags().StringVar(&req.Email, "email", "", "e-mail")
	add.Flags().StringVar(&req.Role, "role", "user", "роль: user или admin")
	add.Flags().BoolVar(&req.Shell, "shell", false, "разрешить SSH-shell (иначе только SFTP)")
	list := &cobra.Command{Use: "list", Short: "список пользователей", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		users, err := cl.ListUsers(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(users)
		}
		rows := make([][]string, 0, len(users))
		for _, u := range users {
			uid := "-"
			if u.UnixUID != nil {
				uid = strconv.Itoa(*u.UnixUID)
			}
			rows = append(rows, []string{u.Login, u.Role, u.Status, uid, u.Home, u.Email})
		}
		table([]string{"LOGIN", "ROLE", "STATUS", "UID", "HOME", "EMAIL"}, rows)
		return nil
	}}
	show := &cobra.Command{Use: "show <login>", Short: "показать пользователя", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		u, err := cl.GetUser(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return printJSON(u)
	}}
	totpReset := &cobra.Command{Use: "totp-reset <login>", Short: "сбросить 2FA пользователя (восстановление доступа)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.TOTPReset(cmd.Context(), args[0])
	}}
	var upd apitypes.UserUpdateRequest
	var shell, sftpOnly, genPw bool
	set := &cobra.Command{Use: "set <login>", Short: "изменить e-mail, пароль (панель + SFTP/SSH), режим shell/SFTP-only, статус", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if shell {
			t := true
			upd.Shell = &t
		}
		if sftpOnly {
			f := false
			upd.Shell = &f
		}
		shown := ""
		if genPw {
			upd.Password, _ = auth.NewToken(15)
			shown = upd.Password
		}
		res, err := cl.UpdateUser(cmd.Context(), args[0], upd)
		if err != nil {
			return err
		}
		if shown != "" && !g.json {
			fmt.Println("пароль:", shown)
		}
		if res.JobID != 0 {
			return followJob(cmd, cl, res.JobID)
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Println("обновлено")
		return nil
	}}
	set.Flags().StringVar(&upd.Email, "email", "", "e-mail")
	set.Flags().StringVar(&upd.Password, "password", "", "новый пароль")
	set.Flags().BoolVar(&genPw, "generate", false, "сгенерировать пароль")
	set.Flags().BoolVar(&shell, "shell", false, "разрешить SSH shell")
	set.Flags().BoolVar(&sftpOnly, "sftp-only", false, "только SFTP в chroot домашнего каталога")
	set.Flags().StringVar(&upd.Status, "status", "", "active или suspended")
	c.AddCommand(add, list, show, set, totpReset, userRmCmd())
	return c
}

func jobCmd() *cobra.Command {
	c := &cobra.Command{Use: "job", Short: "асинхронные задачи"}
	var limit int
	var status string
	list := &cobra.Command{Use: "list", Short: "последние задачи", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.ListJobs(cmd.Context(), limit, status)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, j := range list {
			rows = append(rows, []string{strconv.FormatInt(j.ID, 10), j.Type, j.Status, strconv.Itoa(j.Progress) + "%", j.RequestedBy, j.CreatedAt.Std().Local().Format("2006-01-02 15:04:05"), firstLine(j.Error, j.Message)})
		}
		table([]string{"ID", "TYPE", "STATUS", "PROGRESS", "BY", "CREATED", "MESSAGE"}, rows)
		return nil
	}}
	list.Flags().IntVar(&limit, "limit", 30, "сколько задач показать")
	list.Flags().StringVar(&status, "status", "", "фильтр: queued running done failed")
	show := &cobra.Command{Use: "show <id>", Short: "задача с журналом", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: "id задачи должен быть числом"}
		}
		j, err := cl.GetJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(j)
		}
		fmt.Printf("#%d %s — %s (%d%%) %s\n", j.ID, j.Type, j.Status, j.Progress, j.Message)
		if j.Error != "" {
			fmt.Println("ошибка:", j.Error)
		}
		fmt.Print(j.Log)
		return nil
	}}
	wait := &cobra.Command{Use: "wait <id>", Short: "дождаться завершения и показать журнал", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: "id задачи должен быть числом"}
		}
		g.noWait = false
		return followJob(cmd, cl, id)
	}}
	c.AddCommand(list, show, wait)
	return c
}

func firstLine(a, b string) string {
	s := a
	if s == "" {
		s = b
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func tokenCmd() *cobra.Command {
	c := &cobra.Command{Use: "token", Short: "API-токены для скриптов и биллинга"}
	var req apitypes.CreateTokenRequest
	var scopes string
	create := &cobra.Command{Use: "create", Short: "создать токен (показывается один раз)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if scopes != "" {
			req.Scopes = strings.Split(scopes, ",")
		}
		res, err := cl.CreateToken(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Println(res.Token)
		return nil
	}}
	create.Flags().StringVar(&req.Name, "name", "cli", "название токена")
	create.Flags().StringVar(&req.User, "user", "", "аккаунт, от имени которого выпустить токен (для администратора; по сокету от root — единственный администратор)")
	create.Flags().StringVar(&scopes, "scopes", "", "scope через запятую; migrate:user:<логин> ограничивает токен переездом, остальные — пометки")
	create.Flags().IntVar(&req.ExpiresInDays, "expires", 0, "срок в днях (0 = бессрочно)")
	var listUser string
	list := &cobra.Command{Use: "list", Short: "токены аккаунта (по умолчанию свои)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.ListTokens(cmd.Context(), listUser)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, t := range list {
			exp, last := "-", "-"
			if t.ExpiresAt != nil {
				exp = t.ExpiresAt.Local().Format("2006-01-02")
			}
			if t.LastUsedAt != nil {
				last = t.LastUsedAt.Local().Format("2006-01-02 15:04")
			}
			rows = append(rows, []string{strconv.FormatInt(t.ID, 10), t.Name, strings.Join(t.Scopes, ","), exp, last})
		}
		table([]string{"ID", "NAME", "SCOPES", "EXPIRES", "LAST USED"}, rows)
		return nil
	}}
	list.Flags().StringVar(&listUser, "user", "", "аккаунт (для администратора)")
	revoke := &cobra.Command{Use: "revoke <id>", Short: "отозвать токен (администратор — любой)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: "id токена должен быть числом"}
		}
		return cl.DeleteToken(cmd.Context(), id)
	}}
	c.AddCommand(create, list, revoke)
	return c
}

func serviceCmd() *cobra.Command {
	c := &cobra.Command{Use: "service", Short: "управляемые systemd-сервисы"}
	status := &cobra.Command{Use: "status [unit]", Short: "состояние сервисов", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			st, err := cl.Service(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(st)
		}
		list, err := cl.Services(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, s := range list {
			since := "-"
			if !s.ActiveSince.IsZero() {
				since = s.ActiveSince.Local().Format("2006-01-02 15:04")
			}
			rows = append(rows, []string{s.Unit, s.LoadState, s.ActiveState, s.SubState, s.UnitFileState, since})
		}
		table([]string{"UNIT", "LOAD", "ACTIVE", "SUB", "ENABLED", "SINCE"}, rows)
		return nil
	}}
	c.AddCommand(status)
	for _, action := range []string{"start", "stop", "reload", "restart", "enable", "disable"} {
		action := action
		c.AddCommand(&cobra.Command{Use: action + " <unit>", Short: action + " сервиса", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			st, err := cl.ServiceAction(cmd.Context(), args[0], action)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(st)
			}
			fmt.Printf("%s: %s (%s)\n", st.Unit, st.ActiveState, st.SubState)
			return nil
		}})
	}
	return c
}

func stackCmd() *cobra.Command {
	c := &cobra.Command{Use: "stack", Short: "компоненты веб-стека и расширения (nginx, apache, php, mysql, memcached, jpegoptim, git, composer)"}
	list := &cobra.Command{Use: "list", Short: "установленные компоненты", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		comps, err := cl.Stack(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(comps)
		}
		rows := make([][]string, 0, len(comps))
		for _, comp := range comps {
			state, ver := "не установлен", "-"
			if comp.Installed {
				state, ver = "установлен", comp.Version
				if comp.Service != nil {
					state += " · " + comp.Service.ActiveState
				}
			}
			rows = append(rows, []string{comp.Name, ver, state})
		}
		table([]string{"COMPONENT", "VERSION", "STATE"}, rows)
		return nil
	}}
	install := &cobra.Command{Use: "install <component>", Short: "установить компонент: nginx, apache, percona, mysql, fail2ban, memcached, jpegoptim, git, composer (PHP: mp php install)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.StackInstall(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	remove := &cobra.Command{Use: "remove <component>", Short: "удалить расширение: memcached, jpegoptim, git, composer", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.StackRemove(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	var mem apitypes.MemcachedUpdate
	memcached := &cobra.Command{Use: "memcached", Short: "настройки memcached: без флагов показать, с флагами изменить и применить", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		var st *apitypes.MemcachedSettings
		if cmd.Flags().Changed("memory-mb") || cmd.Flags().Changed("max-conn") {
			cur, cerr := cl.MemcachedSettings(cmd.Context())
			if cerr != nil {
				return cerr
			}
			if !cmd.Flags().Changed("memory-mb") {
				mem.MemoryMB = cur.MemoryMB
			}
			if !cmd.Flags().Changed("max-conn") {
				mem.MaxConnections = cur.MaxConnections
			}
			st, err = cl.SetMemcachedSettings(cmd.Context(), mem)
		} else {
			st, err = cl.MemcachedSettings(cmd.Context())
		}
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		state := "не установлен"
		if st.Installed {
			state = "установлен, 127.0.0.1:11211"
		}
		fmt.Printf("memcached: %s\nпамять:    %d MB\nсоединений: %d\n", state, st.MemoryMB, st.MaxConnections)
		return nil
	}}
	memcached.Flags().IntVar(&mem.MemoryMB, "memory-mb", 128, "размер кеша в МБ")
	memcached.Flags().IntVar(&mem.MaxConnections, "max-conn", 1024, "одновременных соединений")
	c.AddCommand(list, install, remove, memcached, stackRealIPCmd())
	return c
}

func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "конфигурация и шаблоны"}
	show := &cobra.Command{Use: "show", Short: "эффективная конфигурация", RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(cfg)
		}
		fmt.Println("# файл:", cfg.Path())
		tmp := os.TempDir() + "/mp-config-show.yaml"
		if err := cfg.Save(tmp); err != nil {
			return err
		}
		defer os.Remove(tmp)
		b, _ := os.ReadFile(tmp)
		fmt.Print(string(b))
		return nil
	}}
	templates := &cobra.Command{Use: "templates", Short: "список встроенных шаблонов", RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		r := render.New(cfg.TemplatesDir)
		list, err := r.List()
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		for _, name := range list {
			_, overridden, _ := r.Source(name)
			mark := ""
			if overridden {
				mark = "  (переопределён в " + cfg.TemplatesDir + ")"
			}
			fmt.Println(name + mark)
		}
		return nil
	}}
	c.AddCommand(show, templates, configSetCmd())
	return c
}
