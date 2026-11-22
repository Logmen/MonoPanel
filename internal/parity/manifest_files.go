package parity

// The file manager: an account's home, run as the account.
func init() {
	const fm = "lib/components/FileManager.svelte"
	ops := []string{"files mkdir", "files touch", "files rm", "files mv", "files chmod", "files extract", "files size"}
	register(
		op("files-list", cli("files ls"), web(fm)),
		op("files-download", cli("files get"), web(fm),
			field("path", arg("remote"))),
		op("files-upload", cli("files put"), web(fm),
			field("path", arg("remote"))),
		op("files-op", cli(ops...), web(fm),
			field("op", by(ops...)),
			field("paths", arg("path")),
			field("dest", arg("dest", "dst"))),
	)
}
