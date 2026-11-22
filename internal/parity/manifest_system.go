package parity

// The host and the panel itself: status, health checks, metrics, journals,
// the audit log, SELinux, updates and the panel's own settings.
func init() {
	const settings = "routes/settings/"
	const dashboard = "routes/+page.svelte"
	register(
		op("health", noCLI("the clients' own version probe (the sign-in screen, mp setup); mp status and the TUI header show the version"),
			web("lib/state.svelte.ts")),
		op("system-status", cli("status"), web(dashboard)),
		op("system-doctor", cli("doctor"), web(dashboard)),
		op("system-metrics", cli("metrics"), web(dashboard)),
		// The Services page hands the path to the LogView component.
		op("system-logs", cli("logs"), webLoose("routes/services/", "lib/components/LogView.svelte")),
		op("system-audit", cli("audit"), web("routes/audit/"),
			adminField("an ordinary account sees its own entries only: the handler sets the actor to the caller", "actor")),
		op("system-console", noCLI("the console runs mp itself: in a terminal there is mp, and the TUI, already"),
			web("routes/console/")),
		op("selinux-status", cli("selinux"), web(settings)),
		op("selinux-set", cli("selinux"), web(settings),
			field("mode", arg("enforcing|permissive"))),
		op("update-status", cli("update"), web(settings)),
		op("update-check", cli("update check"), web(settings)),
		op("update-apply", cli("update apply"), web(settings)),
		op("update-settings", cli("update settings"), web(settings)),
		op("system-config-get", cli("config show"), web(settings)),
		op("system-config-set", cli("config set"), web(settings),
			field("hostname", arg("key")),
			field("listen", arg("key")),
			field("log_level", arg("key")),
			field("log_format", arg("key")),
			field("jobs_workers", arg("key"))),
		op("system-templates", cli("config templates"), web(settings)),
	)
}
