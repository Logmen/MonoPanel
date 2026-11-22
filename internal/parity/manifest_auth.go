package parity

// Signing in and the caller's own two-factor authentication.
func init() {
	const browserOnly = "browser sessions: mp and the TUI authenticate every request by the socket's peer uid or by an API token (--server/--token, MP_SERVER/MP_TOKEN), and tokens are managed in every client"
	register(
		op("auth-login", noCLI(browserOnly), web("lib/components/Login.svelte")),
		op("auth-logout", noCLI(browserOnly), web("lib/state.svelte.ts")),
		// Who is calling: every client asks it to adapt to the role (the web
		// navigation, the TUI menus, mp's default account for cron, app and
		// valkey); mp whoami prints it, mp totp status asks it too.
		op("auth-me", cli("whoami", "totp status"), web("lib/state.svelte.ts")),
		op("totp-status", cli("totp status", "whoami"), web("routes/settings/")),
		op("totp-setup", cli("totp setup"), web("routes/settings/")),
		op("totp-enable", cli("totp enable"), web("routes/settings/")),
		op("totp-disable", cli("totp disable"), web("routes/settings/"),
			field("password", flag("password-stdin"))),
	)
}
