package parity

// Backups: restic repositories, runs, snapshots and restores.
func init() {
	const page = "routes/backups/"
	register(
		op("backup-targets-list", cli("backup target list"), web(page)),
		op("backup-targets-create", cli("backup target add"), web(page),
			field("repository", flag("repo"))),
		op("backup-targets-delete", cli("backup target rm"), web(page),
			field("ref", arg("name"))),
		op("backup-snapshots", cli("backup snapshots"), web(page),
			field("ref", flag("target"))),
		op("backups-list", cli("backup list"), web(page)),
		op("backups-run", cli("backup run"), web(page)),
		op("backups-restore", cli("backup restore"), web(page)),
	)
}
