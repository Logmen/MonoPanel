package parity

// The firewall and fail2ban. The page posts its actions as /firewall/${action}.
func init() {
	const page = "routes/firewall/"
	register(
		op("firewall-status", cli("firewall status"), web(page)),
		op("firewall-enable", cli("firewall enable"), webDyn("enable", page)),
		op("firewall-disable", cli("firewall disable"), webDyn("disable", page)),
		op("firewall-apply", cli("firewall apply"), webDyn("apply", page)),
		op("firewall-rule-add", cli("firewall allow", "firewall deny"), web(page),
			field("kind", by("firewall allow", "firewall deny"))),
		op("firewall-rule-delete", cli("firewall rm"), web(page)),
		op("firewall-ban", cli("firewall ban"), webDyn("ban", page)),
		op("firewall-unban", cli("firewall unban"), webDyn("unban", page)),
		op("firewall-trust", cli("firewall trust"), webDyn("trust", page)),
		op("firewall-untrust", cli("firewall untrust"), webDyn("untrust", page)),
	)
}
