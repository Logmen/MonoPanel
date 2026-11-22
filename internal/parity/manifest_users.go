package parity

// Accounts and the folders shared with them (Users page; for an ordinary
// account it is its own Account page).
func init() {
	const page = "routes/users/"
	register(
		op("users-list", cli("user list"), web(page)),
		op("users-get", cli("user show"), web(page)),
		op("users-create", cli("user add"), web(page),
			field("quota_mb", flag("quota"))),
		op("users-update", cli("user set"), web(page),
			field("shell", flag("shell", "sftp-only")),
			// a secret: on stdin (or asked on a terminal), never on argv
			field("current_password", flag("current-password-stdin")),
			adminField("an ordinary account changes its own e-mail and password only: the handler refuses shell and status to anyone else", "shell", "status")),
		op("users-delete", cli("user rm"), web(page)),
		op("users-totp-reset", cli("user totp-reset"), web(page)),
		op("shares-list", cli("user share list"), web(page)),
		op("shares-create", cli("user share add"), web(page)),
		op("shares-update", cli("user share entry"), web(page),
			field("entry", flag("off"))),
		op("shares-delete", cli("user share rm"), web(page)),
	)
}
