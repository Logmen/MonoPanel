package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func migrateCmd() *cobra.Command {
	c := &cobra.Command{Use: "migrate", Short: T("перенос аккаунтов между панелями", "move accounts between panels")}

	var grant apitypes.MigrationGrantRequest
	grantCmd := &cobra.Command{Use: T("grant <user:логин>", "grant <user:login>"), Short: T("разрешить другой панели забрать аккаунт (выдаёт одноразовый токен)", "allow another panel to take an account (issues a one-off token)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf(T("токен на %s действует до %s (показывается один раз):\n\n%s\n\n", "token for %s, valid until %s (shown only once):\n\n%s\n\n"), res.Scope, res.ExpiresAt.Format("2006-01-02 15:04"), res.Token)
		fmt.Println(T("на принимающей панели:", "on the target panel:"))
		fmt.Println(" ", res.Command)
		fmt.Println(T("\nТокен пускает только на чтение этого аккаунта и никуда больше.", "\nThe token grants read-only access to this account and nothing else."))
		return nil
	}}
	grantCmd.Flags().IntVar(&grant.Hours, "hours", 24, T("срок жизни токена в часах", "token lifetime in hours"))

	var src apitypes.MigrationSourceRequest
	var passwordStdin, tokenStdin bool
	var keyFile string
	addSourceFlags := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&src.Panel, "from", "monopanel", T("откуда переносим: monopanel | fastpanel | bitrixvm", "the panel to move from: monopanel | fastpanel | bitrixvm"))
		cmd.Flags().StringVar(&src.Source, "source", "", T("MonoPanel: https://old.example.com:8443; чужая панель: root@old.example.com[:22]", "MonoPanel: https://old.example.com:8443; a foreign panel: root@old.example.com[:22]"))
		cmd.Flags().StringVar(&src.Token, "token", "", T("токен, выданный источником (mp migrate grant) — только для MonoPanel", "the token issued by the source (mp migrate grant) — MonoPanel only"))
		cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, T("прочитать токен источника из stdin", "read the source's token from stdin"))
		cmd.Flags().StringVar(&src.Scope, "scope", "", T("что переносим: user:<логин> (BitrixVM: всегда user:bitrix)", "what to move: user:<login> (BitrixVM: always user:bitrix)"))
		cmd.Flags().StringVar(&src.As, "as", "", T("принять под другим логином", "take the account under another login"))
		cmd.Flags().BoolVar(&src.Insecure, "insecure", false, T("не проверять сертификат источника (MonoPanel)", "do not verify the source's certificate (MonoPanel)"))
		cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, T("прочитать пароль ssh источника из stdin (чужие панели)", "read the source's ssh password from stdin (foreign panels)"))
		cmd.Flags().StringVar(&keyFile, "key", "", T("приватный ключ ssh для источника (по умолчанию ~/.ssh/id_ed25519 или id_rsa)", "private ssh key for the source (defaults to ~/.ssh/id_ed25519 or id_rsa)"))
		cmd.Flags().StringVar(&src.Domain, "domain", "", T("BitrixVM: доменное имя основного сайта (в его nginx стоит server_name _)", "BitrixVM: the domain name of the main site (its nginx config has server_name _)"))
		cmd.MarkFlagRequired("source") //nolint:errcheck // флаг объявлен строкой выше
	}
	// prepareSource reads the ssh credentials for a foreign source and
	// checks the flags that only make sense for a MonoPanel one.
	prepareSource := func(cmd *cobra.Command) error {
		if err := shareStdin(cmd, []string{"token-stdin", "password-stdin"}, nil); err != nil {
			return err
		}
		if err := secretFlag(cmd, "token-stdin", "token", T("Токен источника: ", "Source token: "), &src.Token); err != nil {
			return err
		}
		switch src.Panel {
		case "", "monopanel":
			if src.Token == "" || src.Scope == "" {
				return &exitError{code: 2, msg: T("для переезда с MonoPanel нужны --token и --scope", "a move from MonoPanel needs --token and --scope")}
			}
			return nil
		case "fastpanel", "bitrixvm":
		default:
			return &exitError{code: 2, msg: T("--from: monopanel, fastpanel или bitrixvm", "--from: monopanel, fastpanel or bitrixvm")}
		}
		if err := secretFlag(cmd, "password-stdin", "", T("Пароль ssh источника: ", "The source's ssh password: "), &src.Password); err != nil {
			return err
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
				return fmt.Errorf(T("ключ ssh: %w", "ssh key: %w"), err)
			}
			src.Key = string(pem)
		}
		if src.Key == "" && src.Password == "" {
			return &exitError{code: 2, msg: T("как войти на источник по ssh: --key <файл> или --password-stdin", "how to log in to the source over ssh: --key <file> or --password-stdin")}
		}
		return nil
	}

	planCmd := &cobra.Command{Use: "plan", Short: T("разбор: что приедет и что этому мешает (ничего не меняет)", "dry run: what will arrive and what stands in the way (changes nothing)"), Long: migrateSourceNote(T("Разбор переезда: что приедет и что этому мешает. Ничего не меняет ни здесь, ни на источнике.", "A dry run of the move: what will arrive and what stands in the way. Changes nothing, here or on the source.")), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := prepareSource(cmd); err != nil {
			return err
		}
		if src.Panel != "" && src.Panel != "monopanel" {
			fmt.Fprintln(os.Stderr, T("читаю источник по ssh — на старом сервере с медленным DNS каждая команда может занимать секунды…", "reading the source over ssh — on an old server with slow DNS each command can take seconds…"))
		}
		plan, err := cl.MigratePlan(cmd.Context(), src)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(plan)
		}
		b := plan.Bundle
		fmt.Printf(T("Источник: %s (%s, %s)\n", "Source:   %s (%s, %s)\n"), b.Hostname, b.Panel, b.Family)
		fmt.Printf(T("Аккаунт:  %s → %s\n", "Account:  %s → %s\n"), b.User.Login, plan.Login)
		fmt.Printf(T("Приедет:  сайтов %d, баз %d, заданий cron %d, app-сервисов %d, экземпляров Valkey %d, почтовых доменов %d, ящиков %d\n",
			"Arriving: sites %d, databases %d, cron jobs %d, app services %d, Valkey instances %d, mail domains %d, mailboxes %d\n"),
			len(b.Sites), len(b.Databases), len(b.Cron), len(b.Apps), len(b.Valkey), len(b.MailDomains), len(b.Mailboxes))
		fmt.Printf(T("Объём:    файлы %s, почта %s\n", "Size:     files %s, mail %s\n"), humanBytes(uint64(b.Sizes.FilesBytes)), humanBytes(uint64(b.Sizes.MailBytes)))
		if len(b.Sites) > 0 {
			rows := make([][]string, 0, len(b.Sites))
			for _, site := range b.Sites {
				rows = append(rows, []string{site.Domain, site.PHPVersion, site.Mode, strings.Join(site.Aliases, ", ")})
			}
			fmt.Println()
			table([]string{T("САЙТ", "SITE"), "PHP", T("РЕЖИМ", "MODE"), T("АЛИАСЫ", "ALIASES")}, rows)
		}
		// The whole crontab: what runs on the old server by schedule is
		// easy to overlook, and a planted job would otherwise move silently.
		if len(b.Cron) > 0 {
			rows := make([][]string, 0, len(b.Cron))
			for _, j := range b.Cron {
				mark := j.Comment
				// The server marks the jobs it disabled itself: "disabled …",
				// or "выключено …" on a server from before the English messages.
				if !j.Enabled && !strings.Contains(mark, "выключено") && !strings.Contains(mark, "disabled") {
					mark = strings.TrimSuffix(T("выключено; ", "disabled; ")+mark, "; ")
				}
				rows = append(rows, []string{j.Schedule, j.Command, mark})
			}
			fmt.Println()
			table([]string{"CRON", T("КОМАНДА", "COMMAND"), T("ПОМЕТКА", "NOTE")}, rows)
		}
		for _, n := range b.Notes {
			fmt.Println("·", n)
		}
		for _, w := range plan.Warnings {
			fmt.Printf("! %s%s\n", issueText(w), fixHint(w.Fix))
		}
		for _, c := range plan.Conflicts {
			fmt.Printf("✗ %s%s\n", issueText(c), fixHint(c.Fix))
		}
		if plan.OK {
			fmt.Println(T("\nПрепятствий нет: mp migrate run …", "\nNothing stands in the way: mp migrate run …"))
		} else {
			return &exitError{code: 3, msg: T("перенос пока невозможен, см. отметки ✗", "the move is not possible yet, see the ✗ marks")}
		}
		return nil
	}}
	addSourceFlags(planCmd)

	runCmd := &cobra.Command{Use: "run", Short: T("перенести аккаунт сюда", "move the account here"), Long: migrateSourceNote(T("Переносит аккаунт с источника сюда задачей: файлы, сайты, базы, почту, сертификаты, cron, app-сервисы и Valkey. Сначала — mp migrate plan с теми же флагами.", "Moves the account from the source here in a job: its files, sites, databases, mail, certificates, cron, app services and Valkey. Run mp migrate plan with the same flags first.")), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := prepareSource(cmd); err != nil {
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

// migrateSourceNote is the long help of plan and run: what the command does,
// then the flags that look like the global ones but describe the source panel.
func migrateSourceNote(what string) string {
	return what + "\n\n" + T(`Флаги --token и --insecure здесь относятся к источнику. Свой токен и проверку сертификата для --server
эта команда берёт из окружения: $MP_TOKEN и $MP_INSECURE=1.`, `The --token and --insecure flags here belong to the source. For --server, this command takes its own token
and certificate check from the environment: $MP_TOKEN and $MP_INSECURE=1.`)
}

// issueText names what an issue is about: two sites already served here give
// two lines that differ only by the domain.
func issueText(i apitypes.MigrationIssue) string {
	if i.Target == "" {
		return i.Text
	}
	return i.Target + ": " + i.Text
}

func fixHint(fix string) string {
	if fix == "" {
		return ""
	}
	return " → " + fix
}
