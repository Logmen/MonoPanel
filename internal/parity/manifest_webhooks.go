package parity

// Webhooks for job and certificate events (Settings page).
func init() {
	const page = "routes/settings/"
	register(
		op("webhooks-list", cli("webhook list"), web(page)),
		op("webhooks-create", cli("webhook add"), web(page)),
		op("webhooks-test", cli("webhook test"), web(page)),
		op("webhooks-delete", cli("webhook rm"), web(page)),
	)
}
