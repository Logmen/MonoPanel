package parity

// systemd units of the panel and the stack (Services page).
func init() {
	const page = "routes/services/"
	actions := []string{"service start", "service stop", "service reload", "service restart", "service reload-or-restart", "service enable", "service disable"}
	register(
		op("services-list", cli("service status"), web(page)),
		op("services-get", cli("service status"), web(page)),
		op("services-action", cli(actions...), web(page),
			field("action", by(actions...))),
	)
}
