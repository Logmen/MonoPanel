package parity

// Valkey instances of the accounts: cache and sessions (Users page).
func init() {
	const page = "routes/users/"
	purpose := field("purpose", arg("cache|sessions"))
	register(
		op("valkey-list-all", cli("valkey list"), web(page)),
		op("valkey-list", cli("valkey list"), web(page)),
		op("valkey-put", cli("valkey add"), web(page), purpose,
			field("memory_mb", flag("memory"))),
		op("valkey-restart", cli("valkey restart"), web(page), purpose),
		op("valkey-delete", cli("valkey rm"), web(page), purpose),
	)
}
