package cli

import (
	"fmt"
	"monopanel/internal/termsafe"
	"monopanel/internal/tui"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/client"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

func humanBytes(b uint64) string {
	const unit = 1024.0
	f := float64(b)
	for _, s := range []string{T("Б", "B"), T("КБ", "KB"), T("МБ", "MB"), T("ГБ", "GB"), T("ТБ", "TB")} {
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= unit
	}
	return fmt.Sprintf(T("%.1f ПБ", "%.1f PB"), f)
}

func humanDuration(sec float64) string {
	d := time.Duration(sec) * time.Second
	days := int(d.Hours()) / 24
	if days > 0 {
		return fmt.Sprintf(T("%dд %dч", "%dd %dh"), days, int(d.Hours())%24)
	}
	return fmt.Sprintf(T("%dч %dм", "%dh %dm"), int(d.Hours()), int(d.Minutes())%60)
}

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: T("сводка: панель, хост, сервисы, задачи", "summary: panel, host, services, jobs"), RunE: func(cmd *cobra.Command, _ []string) error {
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
		fmt.Printf(T("MonoPanel %s, работает %s, схема БД v%d\n", "MonoPanel %s, up %s, database schema v%d\n"), st.Panel.Version, humanDuration(st.Panel.UptimeSeconds), st.Panel.SchemaVersion)
		if st.Panel.AgentOK {
			fmt.Printf(T("Агент:     работает (%s)\n", "Agent:     running (%s)\n"), st.Panel.AgentVersion)
		} else {
			fmt.Printf(T("Агент:     НЕДОСТУПЕН: %s\n", "Agent:     UNREACHABLE: %s\n"), st.Panel.AgentError)
		}
		if h := st.Host; h != nil {
			fmt.Printf(T("Хост:      %s · %s · ядро %s · %d CPU · uptime %s\n", "Host:      %s · %s · kernel %s · %d CPU · uptime %s\n"), h.Hostname, h.Release.PrettyName, h.Kernel, h.CPUs, humanDuration(h.UptimeSeconds))
			fmt.Printf(T("Нагрузка:  %.2f %.2f %.2f · память %s из %s\n", "Load:      %.2f %.2f %.2f · memory %s of %s\n"), h.Load[0], h.Load[1], h.Load[2], humanBytes(h.MemTotalBytes-h.MemAvailableBytes), humanBytes(h.MemTotalBytes))
			for _, d := range h.Disks {
				fmt.Printf(T("Диск %-5s свободно %s из %s\n", "Disk %-5s %s free of %s\n"), d.Mount, humanBytes(d.FreeBytes), humanBytes(d.TotalBytes))
			}
		}
		if len(st.Services) > 0 {
			parts := make([]string, 0, len(st.Services))
			for _, s := range st.Services {
				parts = append(parts, strings.TrimSuffix(s.Unit, ".service")+"="+s.ActiveState)
			}
			fmt.Println(T("Сервисы:  ", "Services: "), strings.Join(parts, " "))
		}
		fmt.Printf(T("Аккаунты:  admin=%d user=%d · задачи: queued=%d running=%d done=%d failed=%d\n", "Accounts:  admin=%d user=%d · jobs: queued=%d running=%d done=%d failed=%d\n"),
			st.Panel.Users["admin"], st.Panel.Users["user"], st.Panel.Jobs["queued"], st.Panel.Jobs["running"], st.Panel.Jobs["done"], st.Panel.Jobs["failed"])
		return nil
	}}
}

func followJob(cmd *cobra.Command, cl *client.Client, id int64) error {
	if g.noWait {
		if g.json {
			return printJSON(map[string]int64{"job_id": id})
		}
		fmt.Printf(T("задача #%d поставлена в очередь\n", "job #%d queued\n"), id)
		return nil
	}
	if !g.json {
		fmt.Fprintf(os.Stderr, T("задача #%d\n", "job #%d\n"), id)
	}
	job, err := cl.WaitJob(cmd.Context(), id, func(e jobs.Event) {
		if g.json {
			return
		}
		switch e.Type {
		case "log":
			fmt.Fprintln(os.Stderr, "   ", termsafe.Text(e.Line))
		case "progress":
			fmt.Fprintf(os.Stderr, "  [%3d%%] %s\n", e.Progress, termsafe.Line(e.Message))
		}
	})
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(job)
	}
	if job.Status == store.JobFailed {
		return &exitError{code: 3, msg: fmt.Sprintf(T("задача #%d завершилась с ошибкой: %s", "job #%d failed: %s"), id, job.Error)}
	}
	fmt.Fprintf(os.Stderr, T("задача #%d: %s\n", "job #%d: %s\n"), id, job.Status)
	return nil
}

func userCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: T("аккаунты панели и unix-пользователи", "panel accounts and unix users")}
	var req apitypes.CreateUserRequest
	var passwordStdin, generate bool
	var quota int
	add := &cobra.Command{Use: "add <login>", Short: T("создать пользователя (unix-пользователь и каталоги создаются задачей)", "create a user (the unix user and the directories are created by a job)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		for _, other := range []string{"password", "password-stdin"} {
			if err := apart(cmd, "generate", other); err != nil {
				return err
			}
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Login = args[0]
		if err := secretFlag(cmd, "password-stdin", "password", T("Пароль: ", "Password: "), &req.Password); err != nil {
			return err
		}
		if cmd.Flags().Changed("quota") {
			req.QuotaMB = &quota
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
		fmt.Printf(T("пользователь %s создан (id %d)\n", "user %s created (id %d)\n"), res.User.Login, res.User.ID)
		if shown != "" {
			fmt.Printf(T("пароль: %s\n", "password: %s\n"), shown)
		}
		return followJob(cmd, cl, res.JobID)
	}}
	add.Flags().StringVar(&req.Password, "password", "", T("пароль для входа в панель", "password for signing in to the panel"))
	add.Flags().BoolVar(&passwordStdin, "password-stdin", false, T("прочитать пароль из stdin", "read the password from stdin"))
	add.Flags().BoolVar(&generate, "generate", false, T("сгенерировать пароль и показать его", "generate a password and show it"))
	add.Flags().StringVar(&req.Email, "email", "", "e-mail")
	add.Flags().StringVar(&req.Role, "role", "user", T("роль: user или admin", "role: user or admin"))
	add.Flags().BoolVar(&req.Shell, "shell", false, T("разрешить SSH-shell (иначе только SFTP)", "allow an SSH shell (otherwise SFTP only)"))
	add.Flags().IntVar(&quota, "quota", 0, T("квота диска в МБ (хранится в аккаунте)", "disk quota in MB (kept with the account)"))
	list := &cobra.Command{Use: "list", Short: T("список пользователей", "list users"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	show := &cobra.Command{Use: "show <login>", Short: T("показать пользователя", "show a user"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	totpReset := &cobra.Command{Use: "totp-reset <login>", Short: T("сбросить 2FA пользователя (восстановление доступа)", "reset a user's 2FA (to restore access)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.TOTPReset(cmd.Context(), args[0]); err != nil {
			return err
		}
		if g.json {
			return printJSON(apitypes.TOTPStatus{Enabled: false})
		}
		fmt.Printf(T("2FA аккаунта %s сброшена: вход снова только по паролю\n", "2FA of %s is reset: signing in takes only the password again\n"), args[0])
		return nil
	}}
	var upd apitypes.UserUpdateRequest
	var shell, sftpOnly, genPw, setPasswordStdin bool
	set := &cobra.Command{Use: "set <login>", Short: T("изменить e-mail, пароль (панель + SFTP/SSH), режим shell/SFTP-only, статус", "change the e-mail, password (panel + SFTP/SSH), shell/SFTP-only mode or status"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		for _, pair := range [][2]string{{"shell", "sftp-only"}, {"generate", "password"}, {"generate", "password-stdin"}} {
			if err := apart(cmd, pair[0], pair[1]); err != nil {
				return err
			}
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		// Your own password over --server needs the current one; on stdin it
		// comes first, before the new one (--password-stdin).
		if cmd.Flags().Changed("password") || setPasswordStdin || genPw {
			if err := secretFlag(cmd, "current-password-stdin", "current-password", T("Текущий пароль: ", "Current password: "), &upd.CurrentPassword); err != nil {
				return err
			}
			if upd.CurrentPassword == "" && g.server != "" && stdinIsTerminal() {
				if me, err := cl.Me(cmd.Context()); err == nil && me.Login == args[0] {
					if upd.CurrentPassword, err = readSecret(cmd.Context(), "current-password-stdin", T("Текущий пароль: ", "Current password: ")); err != nil {
						return err
					}
				}
			}
		}
		if err := secretFlag(cmd, "password-stdin", "password", T("Новый пароль: ", "New password: "), &upd.Password); err != nil {
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
		if shown != "" && g.json {
			// The generated password is shown only here, so it goes into the
			// JSON too, as with user add.
			out := map[string]any{"user": res.User, "password": shown}
			if res.JobID == 0 || g.noWait {
				if res.JobID != 0 {
					out["job_id"] = res.JobID
				}
				return printJSON(out)
			}
			job, jerr := cl.WaitJob(cmd.Context(), res.JobID, nil)
			out["job"] = job
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
		if shown != "" {
			fmt.Println(T("пароль:", "password:"), shown)
		}
		if res.JobID != 0 {
			return followJob(cmd, cl, res.JobID)
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Println(T("обновлено", "updated"))
		return nil
	}}
	// Both secrets may come on stdin, the current password first: the TUI
	// pipes them in this order.
	set.Annotations = map[string]string{tui.StdinOrderAnnotation: "current-password-stdin,password-stdin"}
	set.Flags().StringVar(&upd.Email, "email", "", "e-mail")
	set.Flags().StringVar(&upd.CurrentPassword, "current-password", "", T("текущий пароль (нужен, чтобы сменить свой пароль через --server)", "the current password (changing your own password over --server needs it)"))
	set.Flags().Bool("current-password-stdin", false, T("прочитать текущий пароль из stdin; на stdin он идёт первой строкой, до нового", "read the current password from stdin; it comes first on stdin, before the new one"))
	set.Flags().StringVar(&upd.Password, "password", "", T("новый пароль", "new password"))
	set.Flags().BoolVar(&setPasswordStdin, "password-stdin", false, T("прочитать новый пароль из stdin", "read the new password from stdin"))
	set.Flags().BoolVar(&genPw, "generate", false, T("сгенерировать пароль", "generate a password"))
	set.Flags().BoolVar(&shell, "shell", false, T("разрешить SSH shell", "allow an SSH shell"))
	set.Flags().BoolVar(&sftpOnly, "sftp-only", false, T("только SFTP в chroot домашнего каталога", "SFTP only, chrooted to the home directory"))
	set.Flags().StringVar(&upd.Status, "status", "", T("active или suspended", "active or suspended"))
	c.AddCommand(add, list, show, set, totpReset, userRmCmd(), userShareCmd())
	return c
}

// auditCmd prints the audit log: sign-ins with their addresses, tokens, every
// change — the administrator sees all of it, an account only its own lines.
func auditCmd() *cobra.Command {
	var limit int
	var actor, action string
	c := &cobra.Command{Use: "audit", Short: T("журнал действий: входы, токены, изменения", "audit log: sign-ins, tokens, changes"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.ListAudit(cmd.Context(), limit, actor, action)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, e := range list {
			details := ""
			if len(e.Details) > 0 {
				parts := make([]string, 0, len(e.Details))
				for k, v := range e.Details {
					parts = append(parts, fmt.Sprintf("%s=%v", k, v))
				}
				sort.Strings(parts)
				details = strings.Join(parts, " ")
			}
			rows = append(rows, []string{e.Time.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, e.Target, e.IP, e.Result, details})
		}
		table([]string{"TIME", "WHO", "ACTION", "TARGET", "IP", "RESULT", "DETAILS"}, rows)
		return nil
	}}
	c.Flags().IntVarP(&limit, "limit", "n", 50, T("сколько записей показать (до 1000)", "how many entries to show (up to 1000)"))
	c.Flags().StringVar(&actor, "actor", "", T("только записи этого аккаунта", "only this account's entries"))
	c.Flags().StringVar(&action, "action", "", T("префикс действия: auth. — входы, token. — токены, site. — сайты", "action prefix: auth. — sign-ins, token. — tokens, site. — sites"))
	return c
}

func jobCmd() *cobra.Command {
	c := &cobra.Command{Use: "job", Short: T("асинхронные задачи", "asynchronous jobs")}
	var limit int
	var status string
	list := &cobra.Command{Use: "list", Short: T("последние задачи", "recent jobs"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	list.Flags().IntVar(&limit, "limit", 30, T("сколько задач показать (до 500)", "how many jobs to show (up to 500)"))
	list.Flags().StringVar(&status, "status", "", T("только в этом состоянии: queued, running, done, failed или cancelled", "only in this state: queued, running, done, failed or cancelled"))
	show := &cobra.Command{Use: "show <id>", Short: T("задача с журналом", "a job with its log"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: T("id задачи должен быть числом", "job id must be a number")}
		}
		j, err := cl.GetJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(j)
		}
		fmt.Printf("#%d %s — %s (%d%%) %s\n", j.ID, j.Type, j.Status, j.Progress, termsafe.Line(j.Message))
		when := func(t *time.Time) string {
			if t == nil {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		}
		created := j.CreatedAt.Std()
		fmt.Printf(T("кто: %s · создана %s · начата %s · завершена %s\n", "by: %s · created %s · started %s · finished %s\n"), j.RequestedBy, when(&created), when(j.StartedAt), when(j.FinishedAt))
		if p := strings.TrimSpace(string(j.Payload)); p != "" && p != "null" && p != "{}" {
			// the API has already hidden the secrets a payload may carry
			fmt.Println(T("параметры:", "payload:"), termsafe.Line(p))
		}
		if j.Error != "" {
			fmt.Println(T("ошибка:", "error:"), termsafe.Text(j.Error))
		}
		fmt.Print(termsafe.Text(j.Log))
		return nil
	}}
	wait := &cobra.Command{Use: "wait <id>", Short: T("дождаться завершения и показать журнал", "wait for the job to finish and show its log"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: T("id задачи должен быть числом", "job id must be a number")}
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

// apart refuses two flags that ask for opposite things (--shell and
// --sftp-only): which one won would otherwise be up to the code. A switch
// counts when it is on, any other flag when it is given a value: an emptied
// list asks for what --allow-all or --clear-from ask for (a TUI form sends
// --allow= when the field is cleared), an empty --password for no password.
func apart(cmd *cobra.Command, a, b string) error {
	on := func(name string) bool {
		f := cmd.Flags().Lookup(name)
		if f == nil || !f.Changed {
			return false
		}
		switch v := f.Value.String(); f.Value.Type() {
		case "bool":
			return v == "true"
		case "stringSlice", "stringArray":
			return v != "[]"
		default:
			return v != ""
		}
	}
	if on(a) && on(b) {
		return &exitError{code: 2, msg: fmt.Sprintf(T("--%s и --%s вместе не задаются", "--%s and --%s cannot be used together"), a, b)}
	}
	return nil
}

// commaList splits a comma-separated flag value the way the web forms do:
// every item trimmed, empty ones dropped, so "a, b," is a and b.
func commaList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func tokenCmd() *cobra.Command {
	c := &cobra.Command{Use: "token", Short: T("API-токены для скриптов и биллинга", "API tokens for scripts and billing")}
	var req apitypes.CreateTokenRequest
	var scopes string
	create := &cobra.Command{Use: "create", Short: T("создать токен (показывается один раз)", "create a token (shown only once)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Scopes = commaList(scopes)
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
	create.Flags().StringVar(&req.Name, "name", "cli", T("название токена", "token name"))
	create.Flags().StringVar(&req.User, "user", "", T("аккаунт, от имени которого выпустить токен (для администратора; по сокету от root — единственный администратор)", "account to issue the token for (administrators only; as root over the socket, defaults to the only administrator)"))
	create.Flags().StringVar(&scopes, "scopes", "", T("scope через запятую; migrate:user:<логин> ограничивает токен переездом, остальные — пометки", "comma-separated scopes; migrate:user:<login> limits the token to a migration, any other is just a label"))
	create.Flags().IntVar(&req.ExpiresInDays, "expires", 0, T("срок в днях (0 = бессрочно)", "lifetime in days (0 = never expires)"))
	var listUser string
	list := &cobra.Command{Use: "list", Short: T("токены аккаунта (по умолчанию свои)", "an account's tokens (your own by default)"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	list.Flags().StringVar(&listUser, "user", "", T("аккаунт (для администратора)", "account (administrators only)"))
	revoke := &cobra.Command{Use: "revoke <id>", Short: T("отозвать токен (администратор — любой)", "revoke a token (an administrator can revoke any)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return &exitError{code: 2, msg: T("id токена должен быть числом", "token id must be a number")}
		}
		return cl.DeleteToken(cmd.Context(), id)
	}}
	c.AddCommand(create, list, revoke)
	return c
}

func serviceCmd() *cobra.Command {
	c := &cobra.Command{Use: "service", Short: T("управляемые systemd-сервисы", "managed systemd services")}
	status := &cobra.Command{Use: "status [unit]", Short: T("состояние сервисов", "service status"), Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	for _, action := range []string{"start", "stop", "reload", "restart", "reload-or-restart", "enable", "disable"} {
		action := action
		short := action + T(" сервиса", " a service")
		if action == "reload-or-restart" {
			short = T("reload сервиса, а если он не умеет — restart", "reload a service, or restart it when it cannot reload")
		}
		c.AddCommand(&cobra.Command{Use: action + " <unit>", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			st, err := cl.ServiceAction(cmd.Context(), args[0], action)
			if err != nil {
				return err
			}
			if endsPanel(st.Unit, action) {
				// The API answers first and acts half a second later: the
				// state in the answer is the one before the action.
				if action == "stop" {
					if g.json {
						return printJSON(st)
					}
					fmt.Printf(T("%s останавливается; запустить снова: systemctl start %s\n", "%s is stopping; start it again with systemctl start %s\n"), st.Unit, st.Unit)
					return nil
				}
				if g.json {
					if err := waitRestarted(cmd.Context(), cl); err != nil {
						return err
					}
					return printJSON(st)
				}
				return waitRestarted(cmd.Context(), cl)
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

// panelUnit tells the panel's own units: stopping or restarting them takes
// the API (which requires the agent) down with them.
func panelUnit(unit string) bool {
	switch strings.TrimSuffix(unit, ".service") {
	case "monopanel-api", "monopanel-agent":
		return true
	}
	return false
}

// endsPanel tells whether the action takes the API that serves the command
// down for a while: a stop or restart of the panel's own units, and a restart
// of monopanel-api.socket, which holds the panel's port — systemd starts no
// socket while its service runs, so the API stops the socket and restarts
// itself, and its unit brings the socket back.
func endsPanel(unit, action string) bool {
	switch action {
	case "restart", "reload-or-restart":
		return panelUnit(unit) || unit == "monopanel-api.socket"
	case "stop":
		return panelUnit(unit)
	}
	return false
}

func stackCmd() *cobra.Command {
	c := &cobra.Command{Use: "stack", Short: T("компоненты веб-стека и расширения (nginx, apache, php, mysql, fail2ban, memcached, jpegoptim, git, composer, sphinx, valkey)", "web stack components and extensions (nginx, apache, php, mysql, fail2ban, memcached, jpegoptim, git, composer, sphinx, valkey)")}
	list := &cobra.Command{Use: "list", Short: T("установленные компоненты", "installed components"), RunE: func(cmd *cobra.Command, _ []string) error {
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
			state, ver := T("не установлен", "not installed"), "-"
			if comp.Installed {
				state, ver = T("установлен", "installed"), comp.Version
				if comp.Service != nil {
					state += " · " + comp.Service.ActiveState
				}
			}
			rows = append(rows, []string{comp.Name, ver, state})
		}
		table([]string{"COMPONENT", "VERSION", "STATE"}, rows)
		return nil
	}}
	install := &cobra.Command{Use: "install <component>", Short: T("установить компонент: nginx, apache, percona, mysql, fail2ban, memcached, jpegoptim, git, composer, sphinx, valkey (PHP: mp php install)", "install a component: nginx, apache, percona, mysql, fail2ban, memcached, jpegoptim, git, composer, sphinx, valkey (PHP: mp php install)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	remove := &cobra.Command{Use: "remove <component>", Short: T("удалить расширение: memcached, jpegoptim, git, composer, sphinx, valkey", "remove an extension: memcached, jpegoptim, git, composer, sphinx, valkey"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	memcached := &cobra.Command{Use: "memcached", Short: T("настройки memcached: без флагов показать, с флагами изменить и применить", "memcached settings: show them without flags, change and apply them with flags"), RunE: func(cmd *cobra.Command, _ []string) error {
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
		state := T("не установлен", "not installed")
		if st.Installed {
			state = T("установлен, 127.0.0.1:11211", "installed, 127.0.0.1:11211")
		}
		fmt.Printf(T("memcached: %s\nпамять:    %d MB\nсоединений: %d\n", "memcached:   %s\nmemory:      %d MB\nconnections: %d\n"), state, st.MemoryMB, st.MaxConnections)
		return nil
	}}
	memcached.Flags().IntVar(&mem.MemoryMB, "memory-mb", 128, T("размер кеша в МБ", "cache size in MB"))
	memcached.Flags().IntVar(&mem.MaxConnections, "max-conn", 1024, T("одновременных соединений", "concurrent connections"))
	c.AddCommand(list, install, remove, memcached, stackRealIPCmd())
	return c
}
