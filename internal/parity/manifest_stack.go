package parity

// Stack components (Extensions page); the trusted proxies of nginx are on
// the Settings page.
func init() {
	const page = "routes/stack/"
	register(
		op("stack-list", cli("stack list"), web(page)),
		op("stack-install", cli("stack install"), web(page)),
		op("stack-remove", cli("stack remove"), web(page)),
		op("stack-memcached", cli("stack memcached"), web(page)),
		op("stack-memcached-update", cli("stack memcached"), web(page),
			field("max_connections", flag("max-conn"))),
		op("nginx-real-ip-get", cli("stack real-ip"), web("routes/settings/")),
		op("nginx-real-ip-set", cli("stack real-ip"), web("routes/settings/")),
	)
}
