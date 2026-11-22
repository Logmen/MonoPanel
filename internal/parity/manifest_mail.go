package parity

// Mail: the server, domains with DKIM, mailboxes, aliases and webmail.
func init() {
	const page = "routes/mail/"
	register(
		op("mail-status", cli("mail status"), web(page)),
		op("mail-install", cli("mail install"), web(page)),
		op("mail-settings", cli("mail settings"), web(page),
			field("max_size_mb", flag("max-size"))),
		op("mail-apply", cli("mail apply"), web(page)),
		op("mail-webmail", cli("mail webmail"), web(page),
			field("php_version", flag("php"))),
		op("mail-domains-list", cli("mail domain list"), web(page)),
		op("mail-domains-create", cli("mail domain add"), web(page),
			field("name", arg("domain")),
			adminField("an ordinary account's mail domain is always its own: the handler takes the owner only from administrators", "user")),
		op("mail-domains-update", cli("mail domain set"), web(page),
			field("name", arg("domain"))),
		op("mail-domains-delete", cli("mail domain rm"), web(page),
			field("name", arg("domain"))),
		op("mail-domain-dkim", cli("mail domain dkim"), web(page),
			field("name", arg("domain"))),
		op("mail-domain-dns", cli("mail domain dns"), web(page),
			field("name", arg("domain"))),
		op("mailboxes-list", cli("mail box list"), web(page)),
		op("mailboxes-create", cli("mail box add"), web(page),
			field("address", arg("user@example.com")),
			field("quota_mb", flag("quota"))),
		op("mailboxes-update", cli("mail box set"), web(page),
			field("address", arg("user@example.com")),
			field("quota_mb", flag("quota"))),
		op("mailboxes-delete", cli("mail box rm"), web(page),
			field("address", arg("user@example.com"))),
		op("mail-aliases-list", cli("mail alias list"), web(page)),
		op("mail-aliases-create", cli("mail alias add"), web(page),
			field("address", arg("info@example.com")),
			field("destinations", arg("dest"))),
		op("mail-aliases-delete", cli("mail alias rm"), web(page),
			field("address", arg("info@example.com"))),
	)
}
