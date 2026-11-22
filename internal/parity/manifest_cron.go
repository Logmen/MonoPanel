package parity

// Cron jobs of the accounts (Users page, cron panel).
func init() {
	const page = "routes/users/"
	register(
		op("cron-list-all", cli("cron list"), web(page)),
		op("cron-list", cli("cron list"), web(page)),
		op("cron-create", cli("cron add"), web(page),
			field("enabled", flag("disabled"))),
		op("cron-update", cli("cron set", "cron enable", "cron disable"), web(page),
			field("enabled", flag("enable", "disable"), by("cron enable", "cron disable"))),
		op("cron-delete", cli("cron rm"), web(page)),
	)
}
