package parity

// Backups: restic repositories, runs, snapshots and restores.
func init() {
	const page = "routes/backups/"
	register(
		op("backup-targets-list", cli("backup target list"), web(page)),
		op("backup-targets-create", cli("backup target add"), web(page),
			field("repository", flag("repo")),
			field("schedule_scope", flag("scope"))),
		op("backup-targets-update", cli("backup target set"), web(page),
			field("ref", arg("name")),
			field("schedule_scope", flag("scope"))),
		op("backup-targets-delete", cli("backup target rm"), web(page),
			field("ref", arg("name"))),
		op("backup-snapshots", cli("backup snapshots"), web(page),
			field("ref", flag("target"))),
		op("backup-snapshot-ls", cli("backup ls"), web(page),
			field("ref", flag("target")),
			field("snapshot", arg("snapshot")),
			field("path", arg("directory"))),
		op("backups-list", cli("backup list"), web(page)),
		op("backups-run", cli("backup run"), web(page)),
		op("backups-restore", cli("backup restore"), web(page)),
	)
}
