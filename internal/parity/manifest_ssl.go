package parity

// Certificates, the panel's own TLS and DNS providers for DNS-01 (SSL page).
func init() {
	const page = "routes/ssl/"
	const adminSSL = "only the SSL page uses it, and that page is for administrators"
	register(
		op("certificates-list", cli("ssl list"), web(page)),
		op("certificates-get", cli("ssl show"), web(page)),
		op("certificates-issue", cli("ssl issue"), web(page),
			field("names", arg("hostname")),
			field("key_type", flag("rsa"))),
		op("certificates-import", cli("ssl import"), web(page),
			field("name", arg("hostname")),
			field("certificate", flag("cert")),
			field("private_key", flag("key")),
			field("sites", flag("site"))),
		op("certificates-renew", cli("ssl renew"), web(page)),
		op("certificates-delete", cli("ssl rm"), web(page)),
		op("ssl-panel", cli("ssl panel"), web(page)),
		op("ssl-panel-issue", cli("ssl panel issue"), web(page),
			field("key_type", flag("rsa"))),
		op("ssl-panel-import", cli("ssl panel import"), web(page),
			field("certificate", flag("cert")),
			field("private_key", flag("key"))),
		op("ssl-panel-reset", cli("ssl panel self-signed"), web(page)),
		op("web-tls", cli("web tls"), web(page), adminPage(adminSSL)),
		op("dns-providers-list", cli("dns-provider list"), web(page)),
		op("dns-provider-types", cli("dns-provider types"), web(page), adminPage(adminSSL)),
		op("dns-providers-create", cli("dns-provider add"), web(page),
			field("credentials", flag("cred"))),
		op("dns-providers-delete", cli("dns-provider rm"), web(page)),
	)
}
