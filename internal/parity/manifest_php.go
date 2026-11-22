package parity

// PHP branches, their extensions and the server-wide php.ini layer.
func init() {
	const page = "routes/php/"
	register(
		op("php-list", cli("php list"), web(page),
			adminPage("the PHP page is for administrators; ordinary accounts meet the installed branches only as the choices of the site forms")),
		op("php-install", cli("php install"), web(page)),
		op("php-remove", cli("php remove"), web(page)),
		op("php-extensions", cli("php ext list"), web(page)),
		op("php-extension-toggle", cli("php ext enable", "php ext disable"), web(page),
			field("name", arg("extension")),
			field("enabled", by("php ext enable", "php ext disable"))),
		op("php-settings-get", cli("php ini"), web(page)),
		op("php-settings-set", cli("php ini set", "php ini unset"), web(page),
			field("php_ini", arg("key=value", "key"))),
	)
}
