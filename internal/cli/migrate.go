package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func migrateCmd() *cobra.Command {
	c := &cobra.Command{Use: "migrate", Short: "перенос аккаунтов между панелями"}

	var grant apitypes.MigrationGrantRequest
	grantCmd := &cobra.Command{Use: "grant <user:логин>", Short: "разрешить другой панели забрать аккаунт (выдаёт одноразовый токен)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		grant.Scope = args[0]
		res, err := cl.MigrateGrant(cmd.Context(), grant)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("токен на %s действует до %s (показывается один раз):\n\n%s\n\n", res.Scope, res.ExpiresAt.Format("2006-01-02 15:04"), res.Token)
		fmt.Println("на принимающей панели:")
		fmt.Println(" ", res.Command)
		fmt.Println("\nТокен пускает только на чтение этого аккаунта и никуда больше.")
		return nil
	}}
	grantCmd.Flags().IntVar(&grant.Hours, "hours", 24, "срок жизни токена в часах")

	var src apitypes.MigrationSourceRequest
	addSourceFlags := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&src.Source, "source", "", "адрес исходной панели, например https://old.example.com:8443")
		cmd.Flags().StringVar(&src.Token, "token", "", "токен, выданный источником (mp migrate grant)")
		cmd.Flags().StringVar(&src.Scope, "scope", "", "что переносим: user:<логин>")
		cmd.Flags().StringVar(&src.As, "as", "", "принять под другим логином")
		cmd.Flags().BoolVar(&src.Insecure, "insecure", false, "не проверять сертификат источника")
		cmd.MarkFlagRequired("source") //nolint:errcheck // флаг объявлен строкой выше
		cmd.MarkFlagRequired("token")  //nolint:errcheck // флаг объявлен строкой выше
		cmd.MarkFlagRequired("scope")  //nolint:errcheck // флаг объявлен строкой выше
	}

	planCmd := &cobra.Command{Use: "plan", Short: "разбор: что приедет и что этому мешает (ничего не меняет)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		plan, err := cl.MigratePlan(cmd.Context(), src)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(plan)
		}
		b := plan.Bundle
		fmt.Printf("Источник: %s (MonoPanel %s, %s)\n", b.Hostname, b.Panel, b.Family)
		fmt.Printf("Аккаунт:  %s → %s\n", b.User.Login, plan.Login)
		fmt.Printf("Приедет:  сайтов %d, баз %d, заданий cron %d, app-сервисов %d, почтовых доменов %d, ящиков %d\n",
			len(b.Sites), len(b.Databases), len(b.Cron), len(b.Apps), len(b.MailDomains), len(b.Mailboxes))
		fmt.Printf("Объём:    файлы %s, почта %s\n", humanBytes(uint64(b.Sizes.FilesBytes)), humanBytes(uint64(b.Sizes.MailBytes)))
		if len(b.Sites) > 0 {
			rows := make([][]string, 0, len(b.Sites))
			for _, site := range b.Sites {
				rows = append(rows, []string{site.Domain, site.PHPVersion, site.Mode, strings.Join(site.Aliases, ", ")})
			}
			fmt.Println()
			table([]string{"САЙТ", "PHP", "РЕЖИМ", "АЛИАСЫ"}, rows)
		}
		for _, n := range b.Notes {
			fmt.Println("·", n)
		}
		for _, w := range plan.Warnings {
			fmt.Printf("! %s%s\n", w.Text, fixHint(w.Fix))
		}
		for _, c := range plan.Conflicts {
			fmt.Printf("✗ %s%s\n", c.Text, fixHint(c.Fix))
		}
		if plan.OK {
			fmt.Println("\nПрепятствий нет: mp migrate run …")
		} else {
			return &exitError{code: 3, msg: "перенос пока невозможен, см. отметки ✗"}
		}
		return nil
	}}
	addSourceFlags(planCmd)

	runCmd := &cobra.Command{Use: "run", Short: "перенести аккаунт сюда", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.MigrateRun(cmd.Context(), src)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, res.JobID)
	}}
	addSourceFlags(runCmd)

	c.AddCommand(grantCmd, planCmd, runCmd)
	return c
}

func fixHint(fix string) string {
	if fix == "" {
		return ""
	}
	return " → " + fix
}
