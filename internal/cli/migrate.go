package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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
	var passwordStdin bool
	var keyFile string
	addSourceFlags := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&src.Panel, "from", "monopanel", "откуда переносим: monopanel | fastpanel | bitrixvm")
		cmd.Flags().StringVar(&src.Source, "source", "", "MonoPanel: https://old.example.com:8443; чужая панель: root@old.example.com[:22]")
		cmd.Flags().StringVar(&src.Token, "token", "", "токен, выданный источником (mp migrate grant) — только для MonoPanel")
		cmd.Flags().StringVar(&src.Scope, "scope", "", "что переносим: user:<логин> (BitrixVM: всегда user:bitrix)")
		cmd.Flags().StringVar(&src.As, "as", "", "принять под другим логином")
		cmd.Flags().BoolVar(&src.Insecure, "insecure", false, "не проверять сертификат источника (MonoPanel)")
		cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "прочитать пароль ssh источника из stdin (чужие панели)")
		cmd.Flags().StringVar(&keyFile, "key", "", "приватный ключ ssh для источника (по умолчанию ~/.ssh/id_ed25519 или id_rsa)")
		cmd.Flags().StringVar(&src.Domain, "domain", "", "BitrixVM: доменное имя основного сайта (в его nginx стоит server_name _)")
		cmd.MarkFlagRequired("source") //nolint:errcheck // флаг объявлен строкой выше
	}
	// prepareSource reads the ssh credentials for a foreign source and
	// checks the flags that only make sense for a MonoPanel one.
	prepareSource := func() error {
		switch src.Panel {
		case "", "monopanel":
			if src.Token == "" || src.Scope == "" {
				return &exitError{code: 2, msg: "для переезда с MonoPanel нужны --token и --scope"}
			}
			return nil
		case "fastpanel", "bitrixvm":
		default:
			return &exitError{code: 2, msg: "--from: monopanel, fastpanel или bitrixvm"}
		}
		if passwordStdin {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			src.Password = strings.TrimRight(line, "\r\n")
		}
		if keyFile == "" && src.Password == "" {
			home, _ := os.UserHomeDir()
			for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
				if _, err := os.Stat(filepath.Join(home, ".ssh", name)); err == nil {
					keyFile = filepath.Join(home, ".ssh", name)
					break
				}
			}
		}
		if keyFile != "" {
			pem, err := os.ReadFile(keyFile)
			if err != nil {
				return fmt.Errorf("ключ ssh: %w", err)
			}
			src.Key = string(pem)
		}
		if src.Key == "" && src.Password == "" {
			return &exitError{code: 2, msg: "как войти на источник по ssh: --key <файл> или --password-stdin"}
		}
		return nil
	}

	planCmd := &cobra.Command{Use: "plan", Short: "разбор: что приедет и что этому мешает (ничего не меняет)", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := prepareSource(); err != nil {
			return err
		}
		if src.Panel != "" && src.Panel != "monopanel" {
			fmt.Fprintln(os.Stderr, "читаю источник по ssh — на старом сервере с медленным DNS каждая команда может занимать секунды…")
		}
		plan, err := cl.MigratePlan(cmd.Context(), src)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(plan)
		}
		b := plan.Bundle
		fmt.Printf("Источник: %s (%s, %s)\n", b.Hostname, b.Panel, b.Family)
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
		if err := prepareSource(); err != nil {
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
