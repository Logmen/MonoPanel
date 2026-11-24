package parity

// Databases and the database server.
func init() {
	const page = "routes/databases/"
	register(
		op("databases-list", cli("db list"), web(page)),
		op("databases-create", cli("db create"), web(page),
			adminField("an ordinary account's database is always its own: the handler ignores the owner it names", "user")),
		op("databases-password", cli("db passwd"), web(page)),
		op("databases-delete", cli("db rm"), web(page)),
		op("db-engine", cli("db engine"), web(page)),
		op("db-engine-tune", cli("db engine tune"), web(page)),
		op("db-config-get", cli("db config"), web(page)),
		op("db-config-set", cli("db config set", "db config unset"), web(page),
			field("settings", arg("key=value", "key"))),
		op("phpmyadmin-status", cli("db phpmyadmin status", "db phpmyadmin"), web(page)),
		op("phpmyadmin-install", cli("db phpmyadmin install"), web(page),
			field("php_version", flag("php"))),
		op("phpmyadmin-remove", cli("db phpmyadmin remove"), web(page)),
		op("databases-phpmyadmin", cli("db phpmyadmin open"), web(page)),
	)
}
