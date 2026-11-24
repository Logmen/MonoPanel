package confcheck

import (
	"strings"
	"testing"

	"monopanel/internal/phpmyadmin"
	"monopanel/internal/render"
)

// pmaModel is phpMyAdmin's model the way jobPMAInstall hands it to the
// templates (internal/api/ops_phpmyadmin.go).
func pmaModel() render.PhpMyAdmin {
	return render.PhpMyAdmin{
		Port: 2083, ServerName: "panel.example.com", CertPath: "/var/lib/monopanel/tls/panel.crt", KeyPath: "/var/lib/monopanel/tls/panel.key",
		Docroot: phpmyadmin.Tree(phpmyadmin.Version), Socket: "/run/monopanel/php/" + phpmyadmin.Socket,
		Name: phpmyadmin.Pool, User: phpmyadmin.User, ListenGroup: "monopanel-web", Tree: phpmyadmin.Tree(phpmyadmin.Version),
		StateDir: phpmyadmin.StateDir, RunDir: phpmyadmin.RunDir, DisableFunctions: render.DefaultDisableFunctions + ",exec",
	}
}

// phpMyAdmin's server block and pool pass where the API writes them.
func TestPhpMyAdminContentPasses(t *testing.T) {
	m := pmaModel()
	runAcceptances(t, []acceptance{
		{"nginx", "/etc/nginx/monopanel/http.d/monopanel-phpmyadmin.conf", renderT(t, "phpmyadmin/nginx.conf.tmpl", m)},
		{"nginx any name", "/etc/nginx/monopanel/http.d/monopanel-phpmyadmin.conf", renderT(t, "phpmyadmin/nginx.conf.tmpl", func() render.PhpMyAdmin { m := pmaModel(); m.ServerName = "_"; return m }())},
		{"pool debian", "/etc/php/8.4/fpm/pool.d/" + phpmyadmin.Pool + ".conf", renderT(t, "phpmyadmin/pool.conf.tmpl", m)},
		{"pool remi", "/etc/opt/remi/php84/php-fpm.d/" + phpmyadmin.Pool + ".conf", renderT(t, "phpmyadmin/pool.conf.tmpl", m)},
	})
}

// Only phpMyAdmin's pool runs as its system user, and it as nobody else:
// the system user is no account of the panel, and its pool is no site's.
func TestPhpMyAdminPoolUser(t *testing.T) {
	pool := renderT(t, "phpmyadmin/pool.conf.tmpl", pmaModel())
	const dir = "/etc/php/8.4/fpm/pool.d/"
	runRefusals(t, []refusal{
		{"a site's pool as the pool user", dir + "example.com.conf",
			strings.Replace(pool, "["+phpmyadmin.Pool+"]", "[example.com]", 1), []string{"only the pool [" + phpmyadmin.Pool + "] runs as " + phpmyadmin.User}},
		{"phpMyAdmin's pool as an account", dir + phpmyadmin.Pool + ".conf",
			strings.Replace(strings.Replace(pool, "user  = "+phpmyadmin.User, "user  = alex", 1), "group = "+phpmyadmin.User, "group = alex", 1), []string{"it runs as nobody else"}},
		{"phpMyAdmin's pool as root", dir + phpmyadmin.Pool + ".conf",
			strings.Replace(pool, "user  = "+phpmyadmin.User, "user  = root", 1), []string{"it runs as nobody else"}},
		{"another group", dir + phpmyadmin.Pool + ".conf",
			strings.Replace(pool, "group = "+phpmyadmin.User, "group = vmail", 1), []string{"primary group"}},
	})
	// nginx passes PHP to phpMyAdmin's socket and to no other name of its
	// shape.
	block := renderT(t, "phpmyadmin/nginx.conf.tmpl", pmaModel())
	refuse(t, "/etc/nginx/monopanel/http.d/monopanel-phpmyadmin.conf",
		strings.Replace(block, phpmyadmin.Socket, "monopanel-other.sock", 1), "the panel's pools alone")
}
