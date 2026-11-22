package parity

// Moving accounts between servers. The receiving panel's job reads the
// source with the grant token; people use grant, plan and run.
func init() {
	const page = "routes/migrate/"
	const s2s = "server to server: the receiving panel's migration job calls it with the grant token, never a person"
	register(
		op("migrate-grant", cli("migrate grant"), web(page),
			field("scope", arg("user:login"))),
		op("migrate-plan", cli("migrate plan"), web(page),
			field("panel", flag("from")),
			field("password", flag("password-stdin"))),
		op("migrate-run", cli("migrate run"), web(page),
			field("panel", flag("from")),
			field("password", flag("password-stdin"))),
		op("migrate-plan-source", noCLI(s2s), noWeb(s2s)),
		op("migrate-state", noCLI(s2s), noWeb(s2s)),
		op("migrate-files", noCLI(s2s), noWeb(s2s)),
		op("migrate-dump", noCLI(s2s), noWeb(s2s)),
	)
}
