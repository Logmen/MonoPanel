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
	return "", &exitError{code: 2, msg: "экземпляр: cache или sessions"}
}

// valkeyHint says how to connect: sessions go to PHP through the site, the
// cache is configured in the application.
func valkeyHint(st *apitypes.ValkeyStatus) string {
	if st.Instance.Purpose == "sessions" {
		return "PHP-сессии сайта сюда: mp site set <домен> --sessions valkey (нужно расширение redis ветки PHP сайта: mp php ext enable <версия> redis)"
	}
	return "подключение без пароля через unix-сокет " + st.Socket + " — например, WordPress с Redis Object Cache: WP_REDIS_SCHEME unix, WP_REDIS_PATH " + st.Socket
}

func valkeyCmd() *cobra.Command {
	c := &cobra.Command{Use: "valkey", Short: "Valkey аккаунта: отдельные экземпляры для кеша и для PHP-сессий (unix-сокет, только для владельца)"}
	var login string
	c.PersistentFlags().StringVar(&login, "user", "", "логин владельца (обязателен для администратора)")

	list := &cobra.Command{Use: "list", Short: "экземпляры (администратор без --user видит все)", RunE: func(cmd *cobra.Command, _ []string) error {
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
			fmt.Println("Valkey не установлен: mp stack install valkey")
		}
		rows := make([][]string, 0, len(res.Instances))
		for _, v := range res.Instances {
			state := v.Instance.Status
			if v.Service != nil {
				state = v.Service.ActiveState + "/" + v.Service.SubState
			}
			rows = append(rows, []string{v.Instance.Login, v.Instance.Purpose, fmt.Sprintf("%d МБ", v.Instance.MemoryMB), state, v.Socket})
		}
		table([]string{"LOGIN", "PURPOSE", "MEMORY", "STATE", "SOCKET"}, rows)
		return nil
	}}

	var memory int
	add := &cobra.Command{Use: "add <cache|sessions>", Short: "создать экземпляр или изменить его память и перезапустить", Args: cobra.ExactArgs(1), ValidArgs: valkeyPurposes, RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf("%s: %s, %d МБ, сокет %s\n%s\n", st.Instance.Name(), state, st.Instance.MemoryMB, st.Socket, valkeyHint(st))
		return nil
	}}
	add.Flags().IntVar(&memory, "memory", 0, "память в МБ (при создании по умолчанию 128 для кеша, 64 для сессий)")

	restart := &cobra.Command{Use: "restart <cache|sessions>", Short: "перезапустить экземпляр (кеш после этого пуст)", Args: cobra.ExactArgs(1), ValidArgs: valkeyPurposes, RunE: func(cmd *cobra.Command, args []string) error {
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

	rm := &cobra.Command{Use: "rm <cache|sessions>", Short: "остановить и удалить экземпляр вместе с его данными", Args: cobra.ExactArgs(1), ValidArgs: valkeyPurposes, RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf("%s-%s удалён\n", owner, purpose)
		return nil
	}}

	c.AddCommand(list, add, restart, rm)
	return c
}
