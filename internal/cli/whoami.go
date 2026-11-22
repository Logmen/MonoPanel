package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
)

// whoami is what mp whoami --json prints: the caller as the panel sees it,
// whether the account's 2FA is on, and which panel answered.
type whoami struct {
	apitypes.Principal
	// TOTPEnabled is absent for root over the socket, which has no account,
	// and when the status could not be read.
	TOTPEnabled *bool  `json:"totp_enabled,omitempty"`
	Server      string `json:"server,omitempty"`
	Socket      string `json:"socket,omitempty"`
}

// whoamiCmd shows who the panel takes the caller for. Scripts and the TUI
// run mp with credentials from the environment ($MP_SERVER, $MP_TOKEN), and
// this is the quick check that they are the intended ones: the account and
// its role, how the request is authenticated, the token and its scopes, the
// account's 2FA.
func whoamiCmd() *cobra.Command {
	return &cobra.Command{Use: "whoami", Short: T("кто вы для панели: аккаунт, роль, способ входа, токен, 2FA", "who you are to the panel: account, role, how you are authenticated, token, 2FA"), Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		me, err := cl.Me(cmd.Context())
		if err != nil {
			return err
		}
		out := whoami{Principal: *me, Server: g.server}
		if g.server == "" {
			if out.Socket, err = apiSocket(); err != nil {
				return err
			}
		}
		// 2FA guards an account's sign-in to the web interface; root over the
		// socket has no account to guard. The identity is the answer here, so
		// a status that cannot be read is shown as unknown, not as a failure.
		var totpErr error
		if me.UserID != 0 {
			st, err := cl.TOTPStatus(cmd.Context())
			if err != nil {
				totpErr = err
			} else {
				out.TOTPEnabled = &st.Enabled
			}
		}
		if g.json {
			return printJSON(out)
		}
		if me.UserID == 0 {
			fmt.Printf(T("Аккаунт:  %s (%s) — root на сервере, без аккаунта панели\n", "Account:  %s (%s) — root on the server, without a panel account\n"), me.Login, me.Role)
		} else {
			fmt.Printf(T("Аккаунт:  %s (%s)\n", "Account:  %s (%s)\n"), me.Login, me.Role)
		}
		fmt.Println(T("Вход:    ", "Auth:    "), authMethod(me))
		if len(me.Scopes) > 0 {
			fmt.Println(T("Scope:   ", "Scopes:  "), strings.Join(me.Scopes, ", "))
		}
		switch {
		case me.UserID == 0:
			fmt.Println(T("2FA:      — (у root на сервере нет аккаунта панели; 2FA аккаунта сбрасывает mp user totp-reset <логин>)", "2FA:      — (root on the server has no panel account; an account's 2FA is reset with mp user totp-reset <login>)"))
		case totpErr != nil:
			fmt.Printf(T("2FA:      неизвестно: %v\n", "2FA:      unknown: %v\n"), totpErr)
		case *out.TOTPEnabled:
			fmt.Println(T("2FA:      включена", "2FA:      on"))
		default:
			fmt.Println(T("2FA:      выключена (включить: mp totp setup)", "2FA:      off (to turn it on: mp totp setup)"))
		}
		where := out.Server
		if where == "" {
			where = out.Socket
		}
		fmt.Println(T("Панель:  ", "Panel:   "), where)
		return nil
	}}
}

// authMethod says how the panel authenticated the request.
func authMethod(me *apitypes.Principal) string {
	switch me.Via {
	case "peercred":
		if me.UserID == 0 {
			return T("сокет панели от root", "the panel socket, as root")
		}
		return T("сокет панели, по unix-пользователю аккаунта", "the panel socket, as the account's unix user")
	case "token":
		if me.TokenID == 0 {
			return T("API-токен", "API token")
		}
		return fmt.Sprintf(T("API-токен #%d %q", "API token #%d %q"), me.TokenID, me.TokenName)
	case "session":
		return T("сессия веб-интерфейса", "a web interface session")
	}
	return me.Via
}
