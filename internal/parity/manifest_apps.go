package parity

// App services: systemd units that run as an account (Users page, app panel).
func init() {
	const page = "routes/users/"
	register(
		op("apps-list-all", cli("app list"), web(page)),
		op("apps-list", cli("app list"), web(page)),
		op("apps-create", cli("app add"), web(page),
			field("enabled", flag("disabled"))),
		op("apps-get", cli("app show"), web(page)),
		op("apps-update", cli("app set"), web(page),
			field("enabled", flag("enable", "disable"))),
		op("apps-logs", cli("app logs"), web(page)),
		op("apps-action", cli("app start", "app stop", "app restart"), web(page),
			field("action", by("app start", "app stop", "app restart"))),
		op("apps-delete", cli("app rm"), web(page)),
	)
}
