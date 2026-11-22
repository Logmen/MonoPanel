package parity

// Sites: the list (routes/sites/+page.svelte), the site page
// (routes/sites/[domain]/) and the CMS installer. Both pages post the site
// actions as /sites/${domain}/${action}.
func init() {
	const list, site = "routes/sites/+page.svelte", "routes/sites/[domain]/"
	// Suspending is the administrators' business: the handler answers 403 to
	// anyone else, and the web UI shows the button to administrators only.
	const refused = "the handler refuses ordinary accounts (administrator role required, internal/api/ops_sites.go)"
	register(
		op("sites-list", cli("site list"), web(list)),
		op("sites-create", cli("site add"), web(list),
			adminField("an ordinary account's site is always its own: the handler ignores the owner it names", "user"),
			field("aliases", flag("alias")),
			field("allow_from", flag("allow")),
			field("client_max_body", flag("max-body")),
			field("fpm_max_children", flag("max-children")),
			field("fpm_pm", flag("pm")),
			field("php_ini", flag("ini")),
			field("php_version", flag("php")),
			field("redirect_https", flag("no-https-redirect"))),
		op("sites-presets", cli("site presets"), web(list)),
		op("sites-move-ip", cli("site move-ip"), web(list),
			field("to", arg("new-address"))),
		op("sites-apply", cli("site apply"), webDyn("apply", list)),
		op("sites-suspend", cli("site suspend"), webDyn("suspend", list), admin(refused)),
		op("sites-unsuspend", cli("site unsuspend"), webDyn("unsuspend", list), admin(refused)),
		op("sites-delete", cli("site rm"), web(list)),
		op("sites-get", cli("site show"), web(site)),
		op("sites-update", cli("site set"), web(site),
			field("aliases", flag("alias", "rm-alias")),
			field("allow_from", flag("allow", "allow-all")),
			field("client_max_body", flag("max-body")),
			field("fpm_max_children", flag("max-children")),
			field("fpm_pm", flag("pm")),
			field("php_ini", flag("ini")),
			field("php_version", flag("php")),
			field("redirect_https", flag("https-redirect")),
			field("session_store", flag("sessions"))),
		op("sites-php-get", cli("site php"), web(site)),
		op("sites-nginx-get", cli("site nginx"), web(site)),
		op("sites-nginx-set", cli("site nginx"), web(site),
			field("custom", flag("set", "clear"))),
		op("sites-logs", cli("site logs"), web(site)),
		op("sites-tls-issue", cli("site tls"), web(site), web("routes/ssl/")),
		// The site page fixes any site; the dashboard fixes the ones the
		// health checks found broken.
		op("sites-fix", cli("site fix"), webDyn("fix", site), web("routes/+page.svelte")),
		op("cms-list", cli("cms list"), web(site)),
		op("sites-cms-install", cli("cms install"), web(site)),
	)
}
