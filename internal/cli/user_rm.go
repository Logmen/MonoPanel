package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func userRmCmd() *cobra.Command {
	var purge, yes bool
	c := &cobra.Command{Use: "rm <login>", Short: "удалить пользователя: сайты, базы, cron, app-сервисы, сертификаты и unix-аккаунт (--purge удаляет и файлы)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !yes {
			what := "файлы останутся в /var/www/" + args[0]
			if purge {
				what = "ВСЕ файлы будут удалены"
			}
			fmt.Printf("Удалить пользователя %s вместе с сайтами и базами (%s)? [y/N] ", args[0], what)
			var answer string
			fmt.Scanln(&answer)
			if answer != "y" && answer != "Y" && answer != "yes" {
				return &exitError{code: 1, msg: "отменено"}
			}
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		ref, err := cl.DeleteUser(cmd.Context(), args[0], purge)
		if err != nil {
			return err
		}
		return followJob(cmd, cl, ref.JobID)
	}}
	c.Flags().BoolVar(&purge, "purge", false, "удалить домашний каталог со всеми файлами")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "не спрашивать подтверждение")
	return c
}
