package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

var valkeyPurposes = []string{"cache", "sessions"}

func valkeyPurpose(arg string) (string, error) {
	for _, p := range valkeyPurposes {
		if arg == p {
			return p, nil
		}
	}
	return "", &exitError{code: 2, msg: T("экземпляр: cache или sessions", "instance: cache or sessions")}
}

// valkeyHint says how to connect: sessions go to PHP through the site, the
// cache is configured in the application.
func valkeyHint(st *apitypes.ValkeyStatus) string {
	if st.Instance.Purpose == "sessions" {
		return T("PHP-сессии сайта сюда: mp site set <домен> --sessions valkey (нужно расширение redis ветки PHP сайта: mp php ext enable <версия> redis)", "to move a site's PHP sessions here: mp site set <domain> --sessions valkey (the site's PHP branch needs the redis extension: mp php ext enable <version> redis)")
	}
	return T("подключение без пароля через unix-сокет ", "connect without a password over the unix socket ") + st.Socket + T(" — например, WordPress с Redis Object Cache: WP_REDIS_SCHEME unix, WP_REDIS_PATH ", " — for example, WordPress with Redis Object Cache: WP_REDIS_SCHEME unix, WP_REDIS_PATH ") + st.Socket
}

func valkeyCmd() *cobra.Command {
	c := &cobra.Command{Use: "valkey", Short: T("Valkey аккаунта: отдельные экземпляры для кеша и для PHP-сессий (unix-сокет, только для владельца)", "the account's Valkey: separate instances for the cache and for PHP sessions (a unix socket, for the owner only)")}
	var login string
	c.PersistentFlags().StringVar(&login, "user", "", T("логин владельца (обязателен для администратора)", "the owner's login (required for an administrator)"))

	list := &cobra.Command{Use: "list", Short: T("экземпляры (администратор без --user видит все)", "list instances (an administrator without --user sees them all)"), RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		var res *apitypes.ValkeyList
		if login == "" {
			me, err := cl.Me(cmd.Context())
			if err != nil {
				return err
			}
			if me.UserID == 0 {
				res, err = cl.ValkeyAll(cmd.Context())
			} else {
				res, err = cl.Valkey(cmd.Context(), me.Login)
			}
			if err != nil {
				return err
			}
		} else if res, err = cl.Valkey(cmd.Context(), login); err != nil {
			return err
		}
		if g.json {
			return printJSON(res)
		}
		if res.Engine == "" {
			fmt.Println(T("Valkey не установлен: mp stack install valkey", "Valkey is not installed: mp stack install valkey"))
		}
		rows := make([][]string, 0, len(res.Instances))
		for _, v := range res.Instances {
			state := v.Instance.Status
			if v.Service != nil {
				state = v.Service.ActiveState + "/" + v.Service.SubState
			}
			rows = append(rows, []string{v.Instance.Login, v.Instance.Purpose, fmt.Sprintf(T("%d МБ", "%d MB"), v.Instance.MemoryMB), state, v.Socket})
		}
		table([]string{"LOGIN", "PURPOSE", "MEMORY", "STATE", "SOCKET"}, rows)
		return nil
	}}

	var memory int
	add := &cobra.Command{Use: "add <cache|sessions>", Short: T("создать экземпляр или изменить его память и перезапустить", "create an instance, or change its memory and restart it"), Args: cobra.ExactArgs(1), ValidArgs: valkeyPurposes, RunE: func(cmd *cobra.Command, args []string) error {
		purpose, err := valkeyPurpose(args[0])
		if err != nil {
			return err
		}
		owner, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.ValkeyPut(cmd.Context(), owner, purpose, memory)
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		state := st.Instance.Status
		if st.Service != nil {
			state = st.Service.ActiveState
		}
		fmt.Printf(T("%s: %s, %d МБ, сокет %s\n%s\n", "%s: %s, %d MB, socket %s\n%s\n"), st.Instance.Name(), state, st.Instance.MemoryMB, st.Socket, valkeyHint(st))
		return nil
	}}
	add.Flags().IntVar(&memory, "memory", 0, T("память в МБ (при создании по умолчанию 128 для кеша, 64 для сессий)", "memory in MB (on creation, 128 for the cache and 64 for sessions by default)"))

	restart := &cobra.Command{Use: "restart <cache|sessions>", Short: T("перезапустить экземпляр (кеш после этого пуст)", "restart an instance (the cache is empty afterwards)"), Args: cobra.ExactArgs(1), ValidArgs: valkeyPurposes, RunE: func(cmd *cobra.Command, args []string) error {
		purpose, err := valkeyPurpose(args[0])
		if err != nil {
			return err
		}
		owner, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		st, err := cl.ValkeyRestart(cmd.Context(), owner, purpose)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", st.Instance.Name(), st.Service.ActiveState)
		return nil
	}}

	rm := &cobra.Command{Use: "rm <cache|sessions>", Short: T("остановить и удалить экземпляр вместе с его данными", "stop and delete an instance along with its data"), Args: cobra.ExactArgs(1), ValidArgs: valkeyPurposes, RunE: func(cmd *cobra.Command, args []string) error {
		purpose, err := valkeyPurpose(args[0])
		if err != nil {
			return err
		}
		owner, err := appOwner(cmd, login)
		if err != nil {
			return err
		}
		cl, err := newClient()
		if err != nil {
			return err
		}
		if err := cl.ValkeyDelete(cmd.Context(), owner, purpose); err != nil {
			return err
		}
		fmt.Printf(T("%s-%s удалён\n", "%s-%s deleted\n"), owner, purpose)
		return nil
	}}

	c.AddCommand(list, add, restart, rm)
	return c
}
