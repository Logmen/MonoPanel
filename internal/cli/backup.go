package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func backupCmd() *cobra.Command {
	c := &cobra.Command{Use: "backup", Short: T("бэкапы через restic (local, SFTP, S3, B2, REST)", "backups with restic (local, SFTP, S3, B2, REST)")}
	target := &cobra.Command{Use: "target", Short: T("репозитории", "repositories")}
	var req apitypes.BackupTargetRequest
	var env []string
	var passwordStdin, envStdin bool
	add := &cobra.Command{Use: "add <name>", Short: T("зарегистрировать репозиторий (инициализируется при первом запуске)", "register a repository (initialised on the first run)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		// Both can come from stdin: the password on the first line, the
		// variables on the lines after it.
		if err := shareStdin(cmd, []string{"password-stdin"}, []string{"env-stdin"}); err != nil {
			return err
		}
		if err := secretFlag(cmd, "password-stdin", "password", T("Пароль репозитория: ", "Repository password: "), &req.Password); err != nil {
			return err
		}
		req.Name = args[0]
		req.Env = map[string]string{}
		for _, e := range env {
			k, v, ok := strings.Cut(e, "=")
			if !ok {
				return &exitError{code: 2, msg: T("--env ожидает KEY=VALUE", "--env expects KEY=VALUE")}
			}
			req.Env[k] = v
		}
		if envStdin {
			if err := readKeyValues(cmd.Context(), "env-stdin", req.Env); err != nil {
				return err
			}
		}
		res, err := cl.BackupTargetCreate(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf(T("цель %s (%s) → %s; хранить daily %d / weekly %d / monthly %d\n", "target %s (%s) → %s; keep daily %d / weekly %d / monthly %d\n"), res.Target.Name, res.Target.Type, res.Target.Repository, res.Target.KeepDaily, res.Target.KeepWeekly, res.Target.KeepMonthly)
		if res.Password != "" {
			fmt.Println(T("пароль репозитория (сохраните, без него бэкапы не восстановить):", "repository password (save it: the backups cannot be restored without it):"), res.Password)
		}
		return nil
	}}
	add.Flags().StringVar(&req.Type, "type", "local", "local, sftp, s3, b2, rest")
	add.Flags().StringVar(&req.Repository, "repo", "", T("путь или URL репозитория restic", "path or URL of the restic repository"))
	add.Flags().StringVar(&req.Password, "password", "", T("пароль репозитория (иначе генерируется)", "repository password (generated if omitted)"))
	add.Flags().BoolVar(&passwordStdin, "password-stdin", false, T("прочитать пароль репозитория из первой строки stdin (с --env-stdin переменные идут следующими строками)", "read the repository password from the first line of stdin (with --env-stdin, the variables follow on the next lines)"))
	add.Flags().StringSliceVar(&env, "env", nil, T("переменные для облака, например AWS_ACCESS_KEY_ID=…", "variables for cloud storage, for example AWS_ACCESS_KEY_ID=…"))
	add.Flags().BoolVar(&envStdin, "env-stdin", false, T("прочитать переменные KEY=VALUE из stdin, по одной в строке, до пустой строки или конца (с --password-stdin — после строки с паролем)", "read the KEY=VALUE variables from stdin, one per line, up to an empty line or the end (with --password-stdin, after the password line)"))
	add.Flags().IntVar(&req.KeepDaily, "keep-daily", 7, T("хранить дневных снимков", "daily snapshots to keep"))
	add.Flags().IntVar(&req.KeepWeekly, "keep-weekly", 4, T("хранить недельных снимков", "weekly snapshots to keep"))
	add.Flags().IntVar(&req.KeepMonthly, "keep-monthly", 3, T("хранить месячных снимков", "monthly snapshots to keep"))
	add.Flags().StringVar(&req.Schedule, "schedule", "", backupScheduleHelp())
	add.Flags().StringVar(&req.ScheduleScope, "scope", "", backupScheduleScopeHelp())
	add.MarkFlagRequired("repo")
	list := &cobra.Command{Use: "list", Short: T("список репозиториев", "list repositories"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ts, err := cl.BackupTargets(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(ts)
		}
		rows := make([][]string, 0, len(ts))
		for _, t := range ts {
			last := "-"
			if t.LastRunAt != nil {
				last = t.LastRunAt.Local().Format("2006-01-02 15:04") + " " + t.LastStatus
			}
			schedule, next := t.Schedule, "-"
			if schedule == "" {
				schedule = T("вручную", "by hand")
			} else if t.ScheduleZone != "" {
				schedule += " " + t.ScheduleZone
			}
			if t.NextRunAt != nil {
				next = t.NextRunAt.Local().Format("2006-01-02 15:04")
			}
			scope := t.ScheduleScope
			if scope == "" {
				scope = "server"
			}
			rows = append(rows, []string{strconv.FormatInt(t.ID, 10), t.Name, t.Type, t.Repository, fmt.Sprintf("%d/%d/%d", t.KeepDaily, t.KeepWeekly, t.KeepMonthly), schedule, scope, next, last})
		}
		table([]string{"ID", "NAME", "TYPE", "REPOSITORY", "KEEP D/W/M", "SCHEDULE", "SCOPE", "NEXT RUN", "LAST RUN"}, rows)
		return nil
	}}
	rm := &cobra.Command{Use: "rm <name>", Short: T("забыть репозиторий (данные не удаляются)", "forget a repository (its data is not deleted)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.BackupTargetDelete(cmd.Context(), args[0])
	}}
	var upd apitypes.BackupTargetUpdate
	var updSchedule, updScope string
	var updKeep [3]int
	set := &cobra.Command{Use: "set <name>", Short: T("изменить расписание и сколько снимков хранить", "change the schedule and how many snapshots to keep"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("schedule") {
			upd.Schedule = &updSchedule
		}
		if cmd.Flags().Changed("scope") {
			upd.ScheduleScope = &updScope
		}
		for i, f := range []struct {
			name string
			dst  **int
		}{{"keep-daily", &upd.KeepDaily}, {"keep-weekly", &upd.KeepWeekly}, {"keep-monthly", &upd.KeepMonthly}} {
			if cmd.Flags().Changed(f.name) {
				*f.dst = &updKeep[i]
			}
		}
		if upd.Schedule == nil && upd.ScheduleScope == nil && upd.KeepDaily == nil && upd.KeepWeekly == nil && upd.KeepMonthly == nil {
			return fmt.Errorf("%s", T("нечего менять: --schedule, --scope, --keep-daily, --keep-weekly или --keep-monthly", "nothing to change: --schedule, --scope, --keep-daily, --keep-weekly or --keep-monthly"))
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.BackupTargetUpdate(cmd.Context(), args[0], upd)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		t := res.Target
		schedule := t.Schedule
		if schedule == "" {
			schedule = T("вручную", "by hand")
		} else if t.ScheduleZone != "" {
			schedule += " " + t.ScheduleZone
		}
		fmt.Printf(T("%s: расписание %s (%s), хранить %d/%d/%d", "%s: schedule %s (%s), keep %d/%d/%d"), t.Name, schedule, t.ScheduleScope, t.KeepDaily, t.KeepWeekly, t.KeepMonthly)
		if t.NextRunAt != nil {
			fmt.Printf(T(", следующий бэкап %s", ", next backup %s"), t.NextRunAt.Local().Format("2006-01-02 15:04"))
		}
		fmt.Println()
		return nil
	}}
	set.Flags().StringVar(&updSchedule, "schedule", "", backupScheduleHelp())
	set.Flags().StringVar(&updScope, "scope", "", backupScheduleScopeHelp())
	set.Flags().IntVar(&updKeep[0], "keep-daily", 0, T("хранить дневных снимков", "daily snapshots to keep"))
	set.Flags().IntVar(&updKeep[1], "keep-weekly", 0, T("хранить недельных снимков", "weekly snapshots to keep"))
	set.Flags().IntVar(&updKeep[2], "keep-monthly", 0, T("хранить месячных снимков", "monthly snapshots to keep"))
	target.AddCommand(add, list, set, rm)

	var tref, scope string
	run := &cobra.Command{Use: "run", Short: T("выполнить бэкап сейчас", "run a backup now"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.BackupRun(cmd.Context(), tref, scope)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	run.Flags().StringVar(&tref, "target", "", T("имя репозитория", "repository name"))
	run.Flags().StringVar(&scope, "scope", "server", "server | user:<login> | site:<domain> | db:<name>")
	run.MarkFlagRequired("target")
	var listTarget string
	var listLimit int
	runs := &cobra.Command{Use: "list", Short: T("выполненные бэкапы", "list the backups that have run"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.Backups(cmd.Context(), listTarget, listLimit)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, b := range list {
			snap := b.SnapshotID
			if len(snap) > 8 {
				snap = snap[:8]
			}
			rows = append(rows, []string{strconv.FormatInt(b.ID, 10), strconv.FormatInt(b.TargetID, 10), b.Scope, b.Status, snap, humanBytes(uint64(b.SizeBytes)), strconv.FormatInt(b.Files, 10), b.StartedAt.Std().Local().Format("2006-01-02 15:04"), firstLine(b.Error, "")})
		}
		table([]string{"ID", "TARGET", "SCOPE", "STATUS", "SNAPSHOT", "SIZE", "FILES", "STARTED", "ERROR"}, rows)
		return nil
	}}
	runs.Flags().StringVar(&listTarget, "target", "", T("фильтр по репозиторию", "filter by repository"))
	runs.Flags().IntVarP(&listLimit, "limit", "n", 50, T("сколько запусков показать (до 500)", "how many runs to show (up to 500)"))
	var snapTarget string
	snaps := &cobra.Command{Use: "snapshots", Short: T("снимки в репозитории", "list the snapshots in a repository"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.BackupSnapshots(cmd.Context(), snapTarget)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, s := range list {
			when := s.Time
			if len(when) > 19 {
				when = when[:19]
			}
			rows = append(rows, []string{s.ShortID, when, strings.Join(s.Tags, ","), strings.Join(s.Paths, " ")})
		}
		table([]string{"ID", "TIME", "TAGS", "PATHS"}, rows)
		return nil
	}}
	snaps.Flags().StringVar(&snapTarget, "target", "", T("имя репозитория", "repository name"))
	snaps.MarkFlagRequired("target")
	var rreq apitypes.RestoreRequest
	restore := &cobra.Command{Use: "restore <snapshot>", Short: T("восстановить снимок (по умолчанию в /var/lib/monopanel-agent/restore/<snapshot>)", "restore a snapshot (into /var/lib/monopanel-agent/restore/<snapshot> by default)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		rreq.Snapshot = args[0]
		ref, err := cl.BackupRestore(cmd.Context(), rreq)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	restore.Flags().StringVar(&rreq.Target, "target", "", T("имя репозитория", "repository name"))
	restore.Flags().StringSliceVar(&rreq.Include, "include", nil, T("восстановить только эти пути", "restore only these paths"))
	restore.Flags().BoolVar(&rreq.InPlace, "in-place", false, T("восстановить поверх текущих файлов (нужен --include или --scope); база при этом заливается обратно", "restore over the current files (needs --include or --scope); a database is loaded back then"))
	restore.Flags().StringVar(&rreq.Scope, "scope", "", T("что восстановить: user:<логин> (домашний каталог и базы аккаунта), site:<домен> (файлы сайта), db:<имя> (база); без --include", "what to restore: user:<login> (the home and the account's databases), site:<domain> (the site's files), db:<name> (the database); not with --include"))
	restore.MarkFlagRequired("target")
	var lsTarget string
	ls := &cobra.Command{Use: "ls <snapshot> [directory]", Short: T("что лежит в каталоге снимка (без каталога — пути снимка): выбрать, что восстановить, вплоть до файла", "what a directory of a snapshot holds (without one, the snapshot's paths): to pick what to restore, down to a file"), Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		dir := ""
		if len(args) == 2 {
			dir = args[1]
		}
		l, err := cl.BackupSnapshotLs(cmd.Context(), lsTarget, args[0], dir)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(l)
		}
		rows := make([][]string, 0, len(l.Entries))
		for _, e := range l.Entries {
			size := ""
			if e.Type == "file" {
				size = humanSize(e.Size)
			}
			when := e.MTime
			if len(when) >= 16 {
				when = strings.Replace(when[:16], "T", " ", 1)
			}
			rows = append(rows, []string{e.Type, size, when, e.Path})
		}
		table([]string{"TYPE", "SIZE", "MODIFIED", "PATH"}, rows)
		if l.Truncated {
			fmt.Println(T("каталог больше, чем показано: остальное восстанавливается по пути (--include)", "the directory holds more than shown: the rest is restored by its path (--include)"))
		}
		return nil
	}}
	ls.Flags().StringVar(&lsTarget, "target", "", T("имя репозитория", "repository name"))
	c.AddCommand(target, run, runs, snaps, ls, restore)
	return c
}

// backupScheduleHelp is what --schedule takes.
func backupScheduleHelp() string {
	return T("«daily ЧЧ:ММ» — каждый день, «weekly sun ЧЧ:ММ» — раз в неделю (mon…sun), по времени сервера; none — только вручную; daily без времени — 03:00",
		"\"daily HH:MM\" — every day, \"weekly sun HH:MM\" — once a week (mon…sun), in the server's time; none — by hand only; daily alone is 03:00")
}

// backupScheduleScopeHelp is what --scope of a target takes.
func backupScheduleScopeHelp() string {
	return T("что бэкапить по расписанию: server (весь сервер, по умолчанию), user:<логин>, site:<домен>, db:<имя>",
		"what the schedule backs up: server (the whole server, the default), user:<login>, site:<domain>, db:<name>")
}

// humanSize is a size in bytes for a person.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	f, suffix := float64(b)/unit, "KMGTP"
	i := 0
	for f >= unit && i < len(suffix)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", f, suffix[i])
}
