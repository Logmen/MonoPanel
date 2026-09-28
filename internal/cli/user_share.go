package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

// userShareCmd manages shared folders: a folder of one account's site that
// another account reaches over SFTP, without touching anyone's home.
func userShareCmd() *cobra.Command {
	c := &cobra.Command{Use: "share", Short: T("общие папки: каталог сайта другого аккаунта по SFTP", "shared folders: a folder of another account's site over SFTP")}

	list := &cobra.Command{Use: "list <login>", Short: T("общие папки аккаунта", "the account's shared folders"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.ListShares(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, st := range list {
			mounted := T("нет", "no")
			if st.Mounted {
				mounted = T("да", "yes")
			}
			php := T("разрешён", "allowed")
			if st.Share.NoPHP {
				php = T("запрещён", "denied")
			}
			rows = append(rows, []string{st.Share.Name, st.Where, st.Share.Domain + "/" + st.Share.Path, st.Share.OwnerLogin, mounted, php})
		}
		table([]string{"NAME", "IN HOME", "SITE FOLDER", "OWNER", "MOUNTED", "PHP"}, rows)
		return nil
	}}

	var req apitypes.ShareRequest
	add := &cobra.Command{Use: "add <login> --site <domain> --path <folder>", Short: T("дать аккаунту папку сайта другого аккаунта", "give the account a folder of another account's site"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		if req.Site == "" || req.Path == "" {
			return &exitError{code: 2, msg: T("нужны --site и --path", "--site and --path are required")}
		}
		st, err := cl.CreateShare(cmd.Context(), args[0], req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		fmt.Printf(T("папка %s → %s смонтирована в %s, ACL выданы\n", "folder %s → %s mounted at %s, ACLs granted\n"), st.Target, st.Share.Login, st.Where)
		if st.Share.NoPHP {
			fmt.Println(T("PHP в этой папке на сайте запрещён; конфигурация сайта применяется", "PHP in this folder is denied on the site; the site configuration is being applied"))
		} else {
			fmt.Println(T("внимание: файлы .php из этой папки сайт выполнит от имени своего аккаунта; для обмена файлами добавьте --no-php", "note: .php files from this folder run on the site as its account; for file exchange add --no-php"))
		}
		if st.JobID > 0 {
			return followJob(cmd, cl, st.JobID)
		}
		return nil
	}}
	add.Flags().StringVar(&req.Site, "site", "", T("домен сайта, чью папку даём", "domain of the site whose folder is shared"))
	add.Flags().StringVar(&req.Path, "path", "", T("папка внутри docroot сайта, например kaspy или upload/exchange (создаётся, если нет)", "folder inside the site's docroot, e.g. kaspy or upload/exchange (created if missing)"))
	add.Flags().StringVar(&req.Name, "name", "", T("имя папки в доме гостя (по умолчанию последний сегмент пути)", "folder name in the guest's home (default: the last path segment)"))
	add.Flags().BoolVar(&req.NoPHP, "no-php", false, T("не выполнять PHP из этой папки на сайте", "never execute PHP from this folder on the site"))

	rm := &cobra.Command{Use: "rm <login> <name>", Short: T("отобрать папку: размонтировать и снять ACL гостя, файлы остаются у сайта", "take the folder away: unmount and drop the guest's ACLs, the files stay with the site"), Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.DeleteShare(cmd.Context(), args[0], args[1])
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		fmt.Printf(T("папка %s у %s отобрана\n", "folder %s taken away from %s\n"), args[1], args[0])
		if st.JobID > 0 {
			return followJob(cmd, cl, st.JobID)
		}
		return nil
	}}
	c.AddCommand(list, add, rm)
	return c
}
