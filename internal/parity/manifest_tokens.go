package parity

// API tokens: one's own on the Settings page, any account's on the Users page.
func init() {
	const own, others = "routes/settings/", "routes/users/"
	const another = "naming another account is the administrators' business: the handler answers 403 to anyone else"
	register(
		op("tokens-list", cli("token list"), web(own), web(others),
			adminField(another, "user")),
		op("tokens-create", cli("token create"), web(own), web(others),
			field("expires_in_days", flag("expires")),
			adminField(another, "user")),
		op("tokens-delete", cli("token revoke"), web(own), web(others)),
	)
}
