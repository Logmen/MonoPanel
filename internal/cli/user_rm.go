package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func userRmCmd() *cobra.Command {
	var purge, yes bool
	c := &cobra.Command{Use: "rm <login>", Short: T("удалить пользователя: сайты, базы, cron, app-сервисы, экземпляры Valkey, сертификаты и unix-аккаунт (--purge удаляет и файлы)", "delete a user: sites, databases, cron, app services, Valkey instances, certificates and the unix account (--purge deletes the files too)"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !yes {
			what := T("файлы останутся в /var/www/", "the files stay in /var/www/") + args[0]
			if purge {
				what = T("ВСЕ файлы будут удалены", "ALL files will be deleted")
			}
			fmt.Printf(T("Удалить пользователя %s вместе с сайтами и базами (%s)? [y/N] ", "Delete user %s with their sites and databases (%s)? [y/N] "), args[0], what)
			var answer string
			fmt.Scanln(&answer) //nolint:errcheck // best effort; the caller reports the real failure
			if answer != "y" && answer != "Y" && answer != "yes" {
				return &exitError{code: 1, msg: T("отменено", "cancelled")}
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
	c.Flags().BoolVar(&purge, "purge", false, T("удалить домашний каталог со всеми файлами", "delete the home directory with all its files"))
	c.Flags().BoolVarP(&yes, "yes", "y", false, T("не спрашивать подтверждение", "do not ask for confirmation"))
	return c
}
