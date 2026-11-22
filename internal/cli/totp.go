package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/boombuler/barcode/qr"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
)

// totpCmd is the caller's own two-factor sign-in, the same card as Settings
// in the web interface. 2FA guards the web sign-in of a panel account; API
// tokens and mp over the socket never ask for codes.
func totpCmd() *cobra.Command {
	c := &cobra.Command{Use: "totp", Short: T("2FA своего аккаунта: коды из приложения при входе в веб-интерфейс", "2FA of your own account: codes from an app when signing in to the web interface"),
		Long: T(`Двухфакторный вход своего аккаунта: при входе в веб-интерфейс, кроме пароля, нужен код из приложения
(Google Authenticator, Aegis, 1Password …). Включение: mp totp setup, затем mp totp enable <код>.
API-токены и mp на сервере кодов не спрашивают. У root на сервере аккаунта панели нет: 2FA другого аккаунта
сбрасывает администратор — mp user totp-reset <логин>.`, `Two-factor sign-in of your own account: signing in to the web interface asks for a code from an app
(Google Authenticator, Aegis, 1Password …) besides the password. To turn it on: mp totp setup, then mp totp enable <code>.
API tokens and mp on the server never ask for codes. Root on the server has no panel account: an administrator
resets another account's 2FA with mp user totp-reset <login>.`)}

	status := &cobra.Command{Use: "status", Short: T("включена ли 2FA", "whether 2FA is on"), Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cl, err := newClient()
		if err != nil {
			return err
		}
		me, err := cl.Me(cmd.Context())
		if err != nil {
			return err
		}
		st, err := cl.TOTPStatus(cmd.Context())
		if err != nil {
			return err
		}
		if g.json {
			return printJSON(st)
		}
		switch {
		case me.UserID == 0:
			fmt.Println(T("2FA: нет — у root на сервере нет аккаунта панели (сбросить 2FA аккаунта: mp user totp-reset <логин>)", "2FA: none — root on the server has no panel account (to reset an account's 2FA: mp user totp-reset <login>)"))
		case st.Enabled:
			fmt.Printf(T("2FA аккаунта %s: включена\n", "2FA of %s: on\n"), me.Login)
		default:
			fmt.Printf(T("2FA аккаунта %s: выключена (включить: mp totp setup)\n", "2FA of %s: off (to turn it on: mp totp setup)\n"), me.Login)
		}
		return nil
	}}

	var forceQR bool
	setup := &cobra.Command{Use: "setup", Short: T("новый секрет: QR-код и ключ для приложения (2FA включится после mp totp enable)", "a new secret: the QR code and key for the app (2FA turns on after mp totp enable)"), Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cl, me, err := totpAccount(cmd.Context())
		if err != nil {
			return err
		}
		res, err := cl.TOTPSetup(cmd.Context())
		if err != nil {
			var ae *client.APIError
			if errors.As(err, &ae) && ae.Status == 409 {
				return &exitError{code: 2, msg: T("2FA уже включена: чтобы настроить её заново, сначала выключите её (mp totp disable спросит пароль)", "2FA is already on: to set it up anew, turn it off first (mp totp disable asks for the password)")}
			}
			return err
		}
		if g.json {
			return printJSON(res)
		}
		fmt.Printf(T("Аккаунт: %s\nКлюч:    %s\nURL:     %s\n", "Account: %s\nKey:     %s\nURL:     %s\n"), me.Login, res.Secret, res.URL)
		if forceQR || term.IsTerminal(int(os.Stdout.Fd())) {
			code, err := qrHalfBlocks(res.URL)
			if err != nil {
				return err
			}
			fmt.Print("\n" + code + "\n")
		}
		fmt.Println(T("Отсканируйте QR-код (или введите ключ) в приложении и подтвердите кодом из него:\n  mp totp enable <код>\nДо подтверждения 2FA выключена.",
			"Scan the QR code (or enter the key) in the app and confirm with a code from it:\n  mp totp enable <code>\nUntil then 2FA stays off."))
		return nil
	}}
	setup.Flags().BoolVar(&forceQR, "qr", false, T("рисовать QR-код, даже когда вывод — не терминал", "draw the QR code even when the output is not a terminal"))

	enable := &cobra.Command{Use: T("enable <код>", "enable <code>"), Short: T("подтвердить кодом из приложения и включить 2FA", "confirm with a code from the app and turn 2FA on"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cl, me, err := totpAccount(cmd.Context())
		if err != nil {
			return err
		}
		st, err := cl.TOTPEnable(cmd.Context(), args[0])
		if err != nil {
			var ae *client.APIError
			if errors.As(err, &ae) && ae.Status == 401 {
				return &exitError{code: 4, msg: T("код не подходит: введите текущий код из приложения (и проверьте время на телефоне)", "the code does not match: enter the current code from the app (and check the time on the phone)")}
			}
			return err
		}
		if g.json {
			return printJSON(st)
		}
		fmt.Printf(T("2FA аккаунта %s включена: вход в веб-интерфейс теперь спрашивает код из приложения\n", "2FA of %s is on: signing in to the web interface now asks for a code from the app\n"), me.Login)
		return nil
	}}

	var passwordStdin bool
	disable := &cobra.Command{Use: "disable", Short: T("выключить 2FA (нужен пароль аккаунта)", "turn 2FA off (needs the account password)"), Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cl, me, err := totpAccount(cmd.Context())
		if err != nil {
			return err
		}
		// The password is asked for on a terminal (without echo); anywhere
		// else it comes only with --password-stdin, never from a stray stdin.
		if !passwordStdin && !stdinIsTerminal() {
			return &exitError{code: 2, msg: T("stdin — не терминал: передайте пароль через --password-stdin", "stdin is not a terminal: pass the password with --password-stdin")}
		}
		password, err := readSecret(cmd.Context(), "password-stdin", fmt.Sprintf(T("Пароль аккаунта %s: ", "Password of %s: "), me.Login))
		if err != nil {
			return err
		}
		st, err := cl.TOTPDisable(cmd.Context(), password)
		if err != nil {
			var ae *client.APIError
			if errors.As(err, &ae) && ae.Status == 401 {
				return &exitError{code: 4, msg: T("неверный пароль", "wrong password")}
			}
			return err
		}
		if g.json {
			return printJSON(st)
		}
		fmt.Printf(T("2FA аккаунта %s выключена\n", "2FA of %s is off\n"), me.Login)
		return nil
	}}
	disable.Flags().BoolVar(&passwordStdin, "password-stdin", false, T("прочитать пароль аккаунта из stdin", "read the account password from stdin"))

	c.AddCommand(status, setup, enable, disable)
	return c
}

// totpAccount is the caller's account. Root over the socket has none, and the
// API would only answer 422: say what to do instead.
func totpAccount(ctx context.Context) (*client.Client, *apitypes.Principal, error) {
	cl, err := newClient()
	if err != nil {
		return nil, nil, err
	}
	me, err := cl.Me(ctx)
	if err != nil {
		return nil, nil, err
	}
	if me.UserID == 0 {
		return nil, nil, &exitError{code: 2, msg: T("у root на сервере нет аккаунта панели, а 2FA — защита входа аккаунта в веб-интерфейс. Включите её от имени аккаунта: в веб-интерфейсе (Настройки) или этой же командой с его токеном (mp --server … --token …). Сбросить 2FA аккаунта: mp user totp-reset <логин>",
			"root on the server has no panel account, and 2FA guards an account's sign-in to the web interface. Turn it on as the account: in the web interface (Settings) or with this command and the account's token (mp --server … --token …). To reset an account's 2FA: mp user totp-reset <login>")}
	}
	return cl, me, nil
}

// qrHalfBlocks draws text as a QR code for a terminal: two module rows per line
// in half blocks. Light modules are white blocks on a black background, set
// explicitly, so the code scans the same in dark and light themes; the quiet
// zone is two modules, enough for phone cameras. The code has an odd number of
// rows: the lower half of the last line keeps the terminal's background.
func qrHalfBlocks(text string) (string, error) {
	code, err := qr.Encode(text, qr.M, qr.Auto)
	if err != nil {
		return "", err
	}
	const quiet = 2
	b := code.Bounds()
	n := b.Dx()
	size := n + 2*quiet
	light := func(x, y int) bool {
		if y >= size {
			return false // below the code: background
		}
		x, y = x-quiet, y-quiet
		if x < 0 || y < 0 || x >= n || y >= n {
			return true // quiet zone
		}
		r, _, _, _ := code.At(b.Min.X+x, b.Min.Y+y).RGBA()
		return r >= 0x8000
	}
	var sb strings.Builder
	for y := 0; y < size; y += 2 {
		if y+1 < size {
			sb.WriteString("\x1b[40;97m")
		} else {
			sb.WriteString("\x1b[49;97m")
		}
		for x := 0; x < size; x++ {
			switch top, bottom := light(x, y), light(x, y+1); {
			case top && bottom:
				sb.WriteString("█")
			case top:
				sb.WriteString("▀")
			case bottom:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\x1b[0m\n")
	}
	return sb.String(), nil
}
