package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

func dbCmd() *cobra.Command {
	c := &cobra.Command{Use: "db", Short: T("базы данных MySQL / Percona", "MySQL / Percona databases")}
	engine := &cobra.Command{Use: "engine", Short: T("состояние сервера БД", "database server status"), RunE: func(cmd *cobra.Command, _ []string) error {
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
			fmt.Println(T("сервер БД не установлен: mp stack install percona   (или mysql)", "the database server is not installed: mp stack install percona   (or mysql)"))
			if st.Instance != nil && st.Instance.LastError != "" {
				fmt.Println(T("последняя ошибка:", "last error:"), st.Instance.LastError)
			}
			return nil
		}
		state := "-"
		if st.Service != nil {
			state = st.Service.ActiveState
		}
		fmt.Printf(T("%s %s · %s · сокет %s · native_password=%v · баз: %d\n", "%s %s · %s · socket %s · native_password=%v · databases: %d\n"), st.Instance.Engine, st.Instance.Version, state, st.Instance.Socket, st.Instance.NativePassword, st.Databases)
		return nil
	}}
	var req apitypes.DatabaseRequest
	var legacy bool
	create := &cobra.Command{Use: "create <name>", Short: T("создать базу <login>_<name> и пользователя с тем же именем", "create the database <login>_<name> and a user with the same name"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf(T("база %s создана (владелец %s)\n", "database %s created (owner %s)\n"), res.Database.Name, res.Database.Login)
		for _, u := range res.Database.Users {
			fmt.Printf(T("пользователь: %s@%s (%s)\n", "user: %s@%s (%s)\n"), u.Name, u.Host, u.AuthPlugin)
		}
		if res.Password != "" {
			fmt.Printf(T("пароль: %s\n", "password: %s\n"), res.Password)
		}
		fmt.Println("DSN:", res.DSN)
		return nil
	}}
	create.Flags().StringVar(&req.User, "user", "", T("владелец (логин); обязателен для администратора", "owner (login); required for an administrator"))
	create.Flags().StringVar(&req.Password, "password", "", T("пароль (иначе генерируется)", "password (generated if omitted)"))
	create.Flags().BoolVar(&legacy, "legacy-auth", false, T("mysql_native_password для PHP < 7.4 (по умолчанию определяется по сайтам владельца)", "mysql_native_password for PHP < 7.4 (by default chosen from the owner's sites)"))
	list := &cobra.Command{Use: "list", Short: T("список баз", "list databases"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	rm := &cobra.Command{Use: "rm <name>", Short: T("удалить базу и её пользователей", "delete a database and its users"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		return cl.DeleteDatabase(cmd.Context(), args[0])
	}}
	var newPassword string
	passwd := &cobra.Command{Use: "passwd <name>", Short: T("сменить пароль пользователя базы", "change the database user's password"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			fmt.Println(T("новый пароль:", "new password:"), res.Password)
		} else {
			fmt.Println(T("пароль обновлён", "password updated"))
		}
		return nil
	}}
	passwd.Flags().StringVar(&newPassword, "password", "", T("новый пароль (иначе генерируется)", "new password (generated if omitted)"))
	tune := &cobra.Command{Use: "tune", Short: T("перегенерировать zz-monopanel.cnf под этот хост и текущие умолчания панели и перезапустить сервер БД", "regenerate zz-monopanel.cnf for this host and the panel's current defaults, then restart the database server"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.DBEngineTune(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		state := "?"
		if st.Service != nil {
			state = st.Service.ActiveState
		}
		fmt.Printf(T("конфигурация перезаписана, %s %s: %s\n", "configuration rewritten, %s %s: %s\n"), st.Instance.Engine, st.Instance.Version, state)
		return nil
	}}
	engine.AddCommand(tune)
	c.AddCommand(engine, create, list, rm, passwd)
	return c
}
