package parity

// Stack components (Extensions page); the trusted proxies of nginx and the
// daily upgrade of nginx, PHP and Apache are on the Settings page.
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
		op("stack-upgrade", cli("stack upgrade"), web(page)),
		op("stack-auto-upgrade", cli("stack auto-upgrade"), web("routes/settings/")),
		op("stack-auto-upgrade-set", cli("stack auto-upgrade"), web("routes/settings/"),
			field("enabled", flag("on", "off"))),
	)
}
