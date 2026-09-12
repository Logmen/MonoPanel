package cli

import "testing"

// fsop run executes nothing but a PHP interpreter, and extract can drop a
// distribution's top-level folder.
func TestFsopRunAllowsOnlyPHPAndStripsComponents(t *testing.T) {
	home := "/var/www/alex"
	for bin, ok := range map[string]bool{
		"php": true, home + "/data/bin/php": true, "/usr/bin/php8.4": true, "/usr/bin/php84": true, "/opt/remi/php84/root/usr/bin/php": true,
		"/bin/sh": false, "/usr/bin/php-cgi": false, home + "/data/www/x/php": false, "/usr/bin/python3": false,
	} {
		if got := phpBinaryAllowed(home, bin); got != ok {
			t.Errorf("phpBinaryAllowed(%q) = %v, want %v", bin, got, ok)
		}
	}
	for _, c := range []struct {
		name string
		n    int
		want string
	}{
		{"wordpress/wp-config.php", 1, "wp-config.php"},
		{"wordpress/", 1, ""},
		{"opencart-4.1.0.4/upload/index.php", 2, "upload/index.php"[len("upload/"):]},
		{"opencart-4.1.0.4/upload/", 2, ""},
		{"index.php", 0, "index.php"},
		{"./bitrix/.settings.php", 1, "bitrix/.settings.php"},
	} {
		if got := stripComponents(c.name, c.n); got != c.want {
			t.Errorf("stripComponents(%q, %d) = %q, want %q", c.name, c.n, got, c.want)
		}
	}
}
