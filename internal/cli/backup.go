package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func backupCmd() *cobra.Command {
	c := &cobra.Command{Use: "backup", Short: "бэкапы через restic (local, SFTP, S3, B2, REST)"}
	target := &cobra.Command{Use: "target", Short: "репозитории"}
	var req apitypes.BackupTargetRequest
	var env []string
	add := &cobra.Command{Use: "add <name>", Short: "зарегистрировать репозиторий (инициализируется при первом запуске)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Name = args[0]
		req.Env = map[string]string{}
		for _, e := range env {
			k, v, ok := strings.Cut(e, "=")
			if !ok {
				return &exitError{code: 2, msg: "--env ожидает KEY=VALUE"}
			}
			req.Env[k] = v
		}
		res, err := cl.BackupTargetCreate(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("цель %s (%s) → %s; хранить daily %d / weekly %d / monthly %d\n", res.Target.Name, res.Target.Type, res.Target.Repository, res.Target.KeepDaily, res.Target.KeepWeekly, res.Target.KeepMonthly)
		if res.Password != "" {
			fmt.Println("пароль репозитория (сохраните, без него бэкапы не восстановить):", res.Password)
		}
		return nil
	}}
	add.Flags().StringVar(&req.Type, "type", "local", "local, sftp, s3, b2, rest")
	add.Flags().StringVar(&req.Repository, "repo", "", "путь или URL репозитория restic")
	add.Flags().StringVar(&req.Password, "password", "", "пароль репозитория (иначе генерируется)")
	add.Flags().StringSliceVar(&env, "env", nil, "переменные для облака, например AWS_ACCESS_KEY_ID=…")
	add.Flags().IntVar(&req.KeepDaily, "keep-daily", 7, "хранить дневных снимков")
	add.Flags().IntVar(&req.KeepWeekly, "keep-weekly", 4, "хранить недельных снимков")
	add.Flags().IntVar(&req.KeepMonthly, "keep-monthly", 3, "хранить месячных снимков")
	add.Flags().StringVar(&req.Schedule, "schedule", "", "daily — ежедневный бэкап всего сервера")
	add.MarkFlagRequired("repo")
	list := &cobra.Command{Use: "list", Short: "список репозиториев", RunE: func(cmd *cobra.Command, _ []string) error {
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
			rows = append(rows, []string{strconv.FormatInt(t.ID, 10), t.Name, t.Type, t.Repository, fmt.Sprintf("%d/%d/%d", t.KeepDaily, t.KeepWeekly, t.KeepMonthly), t.Schedule, last})
		}
		table([]string{"ID", "NAME", "TYPE", "REPOSITORY", "KEEP D/W/M", "SCHEDULE", "LAST RUN"}, rows)
		return nil
	}}
	rm := &cobra.Command{Use: "rm <name>", Short: "забыть репозиторий (данные не удаляются)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.BackupTargetDelete(cmd.Context(), args[0])
	}}
	target.AddCommand(add, list, rm)

	var tref, scope string
	run := &cobra.Command{Use: "run", Short: "выполнить бэкап сейчас", RunE: func(cmd *cobra.Command, _ []string) error {
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
	run.Flags().StringVar(&tref, "target", "", "имя репозитория")
	run.Flags().StringVar(&scope, "scope", "server", "server | user:<login> | site:<domain> | db:<name>")
	run.MarkFlagRequired("target")
	var listTarget string
	runs := &cobra.Command{Use: "list", Short: "выполненные бэкапы", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.Backups(cmd.Context(), listTarget, 50)
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
	runs.Flags().StringVar(&listTarget, "target", "", "фильтр по репозиторию")
	var snapTarget string
	snaps := &cobra.Command{Use: "snapshots", Short: "снимки в репозитории", RunE: func(cmd *cobra.Command, _ []string) error {
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
			rows = append(rows, []string{s.ShortID, s.Time[:19], strings.Join(s.Tags, ","), strings.Join(s.Paths, " ")})
		}
		table([]string{"ID", "TIME", "TAGS", "PATHS"}, rows)
		return nil
	}}
	snaps.Flags().StringVar(&snapTarget, "target", "", "имя репозитория")
	snaps.MarkFlagRequired("target")
	var rreq apitypes.RestoreRequest
	restore := &cobra.Command{Use: "restore <snapshot>", Short: "восстановить снимок (по умолчанию в /var/lib/monopanel/restore/<snapshot>)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	restore.Flags().StringVar(&rreq.Target, "target", "", "имя репозитория")
	restore.Flags().StringSliceVar(&rreq.Include, "include", nil, "восстановить только эти пути")
	restore.Flags().BoolVar(&rreq.InPlace, "in-place", false, "восстановить поверх текущих файлов (нужен --include)")
	restore.MarkFlagRequired("target")
	c.AddCommand(target, run, runs, snaps, restore)
	return c
}
