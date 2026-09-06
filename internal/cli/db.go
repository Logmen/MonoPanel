package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func dbCmd() *cobra.Command {
	c := &cobra.Command{Use: "db", Short: "базы данных MySQL / Percona"}
	engine := &cobra.Command{Use: "engine", Short: "состояние сервера БД", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.DBEngine(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		if !st.Installed {
			fmt.Println("сервер БД не установлен: mp stack install percona   (или mysql)")
			if st.Instance != nil && st.Instance.LastError != "" {
				fmt.Println("последняя ошибка:", st.Instance.LastError)
			}
			return nil
		}
		state := "-"
		if st.Service != nil {
			state = st.Service.ActiveState
		}
		fmt.Printf("%s %s · %s · сокет %s · native_password=%v · баз: %d\n", st.Instance.Engine, st.Instance.Version, state, st.Instance.Socket, st.Instance.NativePassword, st.Databases)
		return nil
	}}
	var req apitypes.DatabaseRequest
	var legacy bool
	create := &cobra.Command{Use: "create <name>", Short: "создать базу <login>_<name> и пользователя с тем же именем", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		req.Name = args[0]
		if cmd.Flags().Changed("legacy-auth") {
			req.LegacyAuth = &legacy
		}
		res, err := cl.CreateDatabase(cmd.Context(), req)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf("база %s создана (владелец %s)\n", res.Database.Name, res.Database.Login)
		for _, u := range res.Database.Users {
			fmt.Printf("пользователь: %s@%s (%s)\n", u.Name, u.Host, u.AuthPlugin)
		}
		if res.Password != "" {
			fmt.Printf("пароль: %s\n", res.Password)
		}
		fmt.Println("DSN:", res.DSN)
		return nil
	}}
	create.Flags().StringVar(&req.User, "user", "", "владелец (логин); обязателен для администратора")
	create.Flags().StringVar(&req.Password, "password", "", "пароль (иначе генерируется)")
	create.Flags().BoolVar(&legacy, "legacy-auth", false, "mysql_native_password для PHP < 7.4 (по умолчанию определяется по сайтам владельца)")
	list := &cobra.Command{Use: "list", Short: "список баз", RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		list, err := cl.Databases(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(list)
		}
		rows := make([][]string, 0, len(list))
		for _, d := range list {
			users := make([]string, 0, len(d.Users))
			for _, u := range d.Users {
				users = append(users, u.Name+"@"+u.Host)
			}
			rows = append(rows, []string{d.Name, d.Login, strings.Join(users, ","), humanBytes(uint64(d.SizeBytes)), d.Collation})
		}
		table([]string{"DATABASE", "OWNER", "ACCOUNTS", "SIZE", "COLLATION"}, rows)
		return nil
	}}
	rm := &cobra.Command{Use: "rm <name>", Short: "удалить базу и её пользователей", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.DeleteDatabase(cmd.Context(), args[0])
	}}
	var newPassword string
	passwd := &cobra.Command{Use: "passwd <name>", Short: "сменить пароль пользователя базы", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		res, err := cl.DatabasePassword(cmd.Context(), args[0], newPassword)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		if res.Password != "" {
			fmt.Println("новый пароль:", res.Password)
		} else {
			fmt.Println("пароль обновлён")
		}
		return nil
	}}
	passwd.Flags().StringVar(&newPassword, "password", "", "новый пароль (иначе генерируется)")
	c.AddCommand(engine, create, list, rm, passwd)
	return c
}
