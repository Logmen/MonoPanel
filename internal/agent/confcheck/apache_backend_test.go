package confcheck

import (
	"os"
	"strings"
	"testing"

	"monopanel/internal/render"
)

const (
	testBackendSecret = "9e1205b19a5d7ed7ddd1802c6d32463e8fb13ccae81acb52a0eea80e6aeaa77d"
	alexBackend       = "/etc/apache2/monopanel/accounts/alex/backend.conf"
	alexNginxBackend  = "/etc/nginx/monopanel/apache/alex.conf"
	alexSiteNginx     = "/etc/nginx/monopanel/sites/example.com.conf"
)

// alexHTTPD is the httpd.conf of alex's Apache as the panel writes it.
func alexHTTPD(t *testing.T) string {
	t.Helper()
	return renderT(t, "apache/account.conf.tmpl", render.ApacheAccount{Login: "alex", Listen: "127.100.3.233:81", RunDir: "/run/monopanel-apache/alex",
		Home: "/var/www/alex", ErrorLog: "/var/log/monopanel/sites/alex/apache.error.log", SitesDir: "/etc/apache2/monopanel/accounts/alex/sites", Backend: alexBackend})
}

// scripts/check-templates.sh runs apache2 -t on the goldens with the form
// of backend.conf from this golden (and the secret its nginx side uses):
// the form apache2 checks is the one the agent writes.
// MP_UPDATE_GOLDEN=1 writes it anew.
func TestApacheBackendGolden(t *testing.T) {
	const golden = "../../render/testdata/apache-backend.conf.golden"
	want := ApacheBackendConf("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if os.Getenv("MP_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(golden)
	if err != nil || string(got) != want {
		t.Fatalf("%s is not ApacheBackendConf (MP_UPDATE_GOLDEN=1 go test -run TestApacheBackendGolden ./internal/agent/confcheck): %v\n%s", golden, err, got)
	}
}

// The two files hold exactly their one form with a secret of 64 hex
// digits, and are the account's alone.
func TestApacheBackendFiles(t *testing.T) {
	pass(t, alexBackend, ApacheBackendConf(testBackendSecret))
	pass(t, alexNginxBackend, NginxApacheBackendConf(testBackendSecret))
	pass(t, "/etc/nginx/monopanel/apache/shop.conf", NginxApacheBackendConf(strings.Repeat("0", 64)))
	if !ValidApacheBackendSecret(testBackendSecret) || ValidApacheBackendSecret(strings.ToUpper(testBackendSecret)) || ValidApacheBackendSecret(testBackendSecret[1:]) {
		t.Fatal("ValidApacheBackendSecret")
	}
	apache := ApacheBackendConf(testBackendSecret)
	nginx := NginxApacheBackendConf(testBackendSecret)
	runRefusals(t, []refusal{
		// backend.conf: anything but the form is read as the account's Apache.
		{"the spec's unset", alexBackend, strings.Replace(apache, "ProxyFCGISetEnvIf \"true\" !HTTP_X_MONOPANEL_BACKEND", "RequestHeader unset X-MonoPanel-Backend", 1), []string{"exactly"}},
		{"granted instead", alexBackend, strings.Replace(apache, "Require all denied", "Require all granted", 1), []string{"exactly"}},
		// The form before the rewrite rule: a site's .htaccess undid it.
		{"the <If> alone", alexBackend, "<If \"req_novary('X-MonoPanel-Backend') != '" + testBackendSecret + "'\">\n    Require all denied\n</If>\nProxyFCGISetEnvIf \"true\" !HTTP_X_MONOPANEL_BACKEND\n", []string{"exactly"}},
		{"two secrets", alexBackend, strings.Replace(apache, " !="+testBackendSecret+" ", " !="+strings.Repeat("0", 64)+" ", 1), []string{"exactly"}},
		{"the rule without its condition", alexBackend, strings.Replace(apache, "RewriteCond %{HTTP:X-MonoPanel-Backend} !="+testBackendSecret+" [NV]\n", "", 1), []string{"exactly"}},
		{"inherited after the virtual host's", alexBackend, strings.Replace(apache, "InheritDownBefore", "InheritDown", 1), []string{"exactly"}},
		{"a directive more", alexBackend, apache + "LoadModule cgid_module /usr/lib/apache2/modules/mod_cgid.so\n", []string{"exactly"}},
		{"a comment more", alexBackend, "# x\n" + apache, []string{"exactly"}},
		{"short secret", alexBackend, ApacheBackendConf(testBackendSecret[2:]), []string{"exactly"}},
		{"upper-case secret", alexBackend, ApacheBackendConf(strings.ToUpper(testBackendSecret)), []string{"exactly"}},
		{"no line feed", alexBackend, strings.TrimSuffix(apache, "\n"), []string{"exactly"}},
		{"empty", alexBackend, "", []string{"exactly"}},
		{"the backend of root", "/etc/apache2/monopanel/accounts/root/backend.conf", ApacheBackendConf(testBackendSecret), []string{"root"}},
		{"the backend of a sudoer", "/etc/apache2/monopanel/accounts/ubuntu/backend.conf", ApacheBackendConf(testBackendSecret), []string{"ubuntu"}},
		{"a backend among the sites", "/etc/apache2/monopanel/accounts/alex/sites/backend.conf", apache, nil},
		// nginx's side.
		{"nginx: another header", alexNginxBackend, strings.Replace(nginx, "proxy_set_header X-MonoPanel-Backend", "proxy_set_header X-Real-IP", 1), []string{"exactly"}},
		{"nginx: a directive more", alexNginxBackend, nginx + "proxy_pass http://203.0.113.1;\n", []string{"exactly"}},
		{"nginx: unquoted", alexNginxBackend, "proxy_set_header X-MonoPanel-Backend " + testBackendSecret + ";\n", []string{"exactly"}},
		{"nginx: a variable", alexNginxBackend, "proxy_set_header X-MonoPanel-Backend \"$http_x_monopanel_backend\";\n", []string{"exactly"}},
		{"nginx: short secret", alexNginxBackend, NginxApacheBackendConf(testBackendSecret[1:]), []string{"exactly"}},
		{"nginx: README", "/etc/nginx/monopanel/apache/README", "x\n", []string{"<login>.conf"}},
		{"nginx: odd name", "/etc/nginx/monopanel/apache/Alex.conf", nginx, []string{"<login>.conf"}},
		{"nginx: a directory below", "/etc/nginx/monopanel/apache/alex/x.conf", nginx, []string{"<login>.conf"}},
		{"nginx: root", "/etc/nginx/monopanel/apache/root.conf", nginx, []string{"<login>.conf"}},
		{"nginx: a system account", "/etc/nginx/monopanel/apache/www-data.conf", nginx, []string{"www-data"}},
		{"nginx: an unknown account", "/etc/nginx/monopanel/apache/ghost.conf", nginx, []string{"ghost"}},
	})
	for _, p := range []string{alexBackend, alexNginxBackend} {
		for _, c := range []struct{ owner, group string }{{"", ""}, {"root", "root"}} {
			if err := CheckOwnership(p, c.owner, c.group, 0o600); err != nil {
				t.Errorf("%s %s:%s 0600: %v", p, c.owner, c.group, err)
			}
		}
		for _, c := range []struct {
			owner, group string
			mode         uint32
		}{{"", "", 0}, {"", "", 0o644}, {"", "", 0o640}, {"root", "root", 0o400}, {"monopanel", "", 0o600}, {"", "monopanel", 0o600}, {"alex", "alex", 0o600}, {"", "www-data", 0o600}} {
			if err := CheckOwnership(p, c.owner, c.group, c.mode); err == nil || !strings.Contains(err.Error(), "0600") {
				t.Errorf("%s %s:%s %04o: %v, want a refusal", p, c.owner, c.group, c.mode, err)
			}
		}
	}
	// Other files keep their rules.
	if err := CheckOwnership("/etc/nginx/monopanel/sites/example.com.conf", "", "", 0o644); err != nil {
		t.Fatal(err)
	}
}

// The account's httpd.conf includes its own backend.conf, plainly, at the
// top level; nothing else includes one.
func TestApacheBackendInclude(t *testing.T) {
	main := alexHTTPD(t)
	pass(t, alexMain, main)
	line := "\nInclude " + alexBackend + "\n"
	if !strings.Contains(main, line) {
		t.Fatalf("the template does not include the check:\n%s", main)
	}
	without := strings.Replace(main, line, "\n", 1)
	runRefusals(t, []refusal{
		{"without the check", alexMain, without, []string{"without", alexBackend}},
		{"another account's check", alexMain, strings.Replace(main, alexBackend, "/etc/apache2/monopanel/accounts/shop/backend.conf", 1), []string{"accounts/shop/backend.conf"}},
		{"optional", alexMain, strings.Replace(main, line, "\nIncludeOptional "+alexBackend+"\n", 1), []string{"with Include"}},
		{"in <Location \"/\">", alexMain, strings.Replace(main, line, "\n<Location \"/\">\n    Include "+alexBackend+"\n</Location>\n", 1), []string{"top level"}},
		{"in an IfModule", alexMain, without + "<IfModule nothing.c>\nInclude " + alexBackend + "\n</IfModule>\n", []string{"top level"}},
		{"in a virtual host", alexMain, without + "<VirtualHost 127.100.3.233:81>\nRewriteEngine On\nInclude " + alexBackend + "\n</VirtualHost>\n", []string{"top level"}},
		{"once more in a section", alexMain, main + "<IfDefine X>\nInclude " + alexBackend + "\n</IfDefine>\n", []string{"top level"}},
		{"relative", alexMain, strings.Replace(main, line, "\nInclude monopanel/accounts/alex/backend.conf\n", 1), nil},
		{"a site includes it", alexSite, "Include " + alexBackend + "\n", []string{"example.com.d/*.conf"}},
		{"custom directives include it", alexCust, "Include " + alexBackend + "\n", []string{"include nothing"}},
		{"elsewhere in /etc/apache2", "/etc/apache2/conf-available/x.conf", "Include " + alexBackend + "\n", []string{"accounts"}},
		{"a wildcard into the accounts", "/etc/apache2/monopanel/httpd.conf", "IncludeOptional /etc/apache2/*/accounts/*/backend.conf\n", []string{"accounts"}},
		{"an account's httpd.conf elsewhere", "/etc/apache2/conf-available/x.conf", "IncludeOptional /etc/apache2/monopanel/accounts/*/httpd.conf\n", []string{"accounts"}},
	})
	// An <If> in a location, or in another <If>, merges after the check's
	// <If>; elsewhere before it.
	runRefusals(t, []refusal{
		{"an If in a site's location", alexSite, "<Location /admin>\n    <If \"true\">\n        Require all granted\n    </If>\n</Location>\n", []string{"<if> in a <location>"}},
		{"an ElseIf in custom directives", alexCust, "<LocationMatch \"^/x\">\n    <If \"false\">\n    </If>\n    <ElseIf \"true\">\n        Require all granted\n    </ElseIf>\n</LocationMatch>\n", []string{"in a <locationmatch>"}},
		{"an Else deeper down", alexCust, "<IfModule mod_rewrite.c>\n<Location />\n<IfModule mod_headers.c>\n<Else>\nRequire all granted\n</Else>\n</IfModule>\n</Location>\n</IfModule>\n", []string{"<else> in a <location>"}},
		{"after the check in httpd.conf", alexMain, main + "<Location />\n    <If \"true\">\n        Require all granted\n    </If>\n</Location>\n", []string{"<if> in a <location>"}},
		{"an If in an If", alexCust, "<If \"true\">\n    <If \"-R '203.0.113.5'\">\n        Require all granted\n    </If>\n</If>\n", []string{"<if> in a <if>"}},
		{"an Else in an If in a Proxy", alexCust, "<Proxy \"fcgi://127.0.0.1:9000/\">\n<If \"true\">\n<If \"false\">\n</If>\n<Else>\nSatisfy Any\n</Else>\n</If>\n</Proxy>\n", []string{"<if> in a <if>"}},
		{"an If in an Else", alexCust, "<If \"false\">\n</If>\n<Else>\n<If \"true\">\nRequire all granted\n</If>\n</Else>\n", []string{"<if> in a <else>"}},
	})
	pass(t, alexCust, "<Directory /var/www/alex/data/www/example.com/x>\n    <If \"%{QUERY_STRING} =~ /debug/\">\n        Require ip 203.0.113.5\n    </If>\n</Directory>\n<If \"%{HTTP_HOST} == 'example.com'\">\n    Header set X-Host example\n</If>\n<ElseIf \"true\">\n    Header set X-Host other\n</ElseIf>\n<Location /x>\n    Require all granted\n</Location>\n")
	// The shared layout of 0.8.19 includes what it did.
	pass(t, "/etc/apache2/monopanel/httpd.conf", "IncludeOptional /etc/apache2/monopanel/sites/*.conf\nInclude /etc/apache2/monopanel/extra/*.conf\n")
}

// What would switch off the rewriting with which backend.conf refuses what
// nginx did not send is refused in the files of an account's Apache:
// rewrite directives at the server level, a virtual host without
// RewriteEngine On, RewriteEngine Off where it applies before the URL is
// mapped, options that put a virtual host's rules ahead of the check or drop
// it. What runs after the mapping, and a virtual host's own rules, pass.
func TestApacheBackendRewriteGuard(t *testing.T) {
	main := alexHTTPD(t)
	vhost := renderT(t, "apache/site.conf.tmpl", site("apache", func(s *render.Site) {
		s.IncludeDir = "/etc/apache2/monopanel/accounts/alex/sites/example.com.d"
	}))
	pass(t, alexSite, vhost)
	on := "\n    RewriteEngine On\n"
	if !strings.Contains(vhost, on) {
		t.Fatalf("the virtual host does not switch mod_rewrite on:\n%s", vhost)
	}
	inVhost := func(directives string) string {
		return strings.Replace(vhost, "    IncludeOptional ", directives+"    IncludeOptional ", 1)
	}
	runRefusals(t, []refusal{
		// The server level is backend.conf's.
		{"httpd.conf: a rule ahead of the check", alexMain, strings.Replace(main, "\nInclude "+alexBackend+"\n", "\nRewriteEngine On\nRewriteRule ^ - [L]\nInclude "+alexBackend+"\n", 1), []string{"backend.conf's alone"}},
		{"httpd.conf: options", alexMain, main + "RewriteOptions IgnoreInherit\n", []string{"backend.conf's alone"}},
		{"httpd.conf: the engine off", alexMain, main + "RewriteEngine Off\n", []string{"backend.conf's alone"}},
		{"httpd.conf: in an IfModule", alexMain, main + "<IfModule rewrite_module>\nRewriteEngine Off\n</IfModule>\n", []string{"backend.conf's alone"}},
		{"a site's file outside its virtual host", alexSite, "RewriteEngine On\nRewriteRule ^ - [L]\n" + vhost, []string{"backend.conf's alone"}},
		// A virtual host switches mod_rewrite on itself.
		{"a virtual host without it", alexSite, strings.Replace(vhost, on, "\n", 1), []string{"without RewriteEngine On"}},
		{"on in an IfModule only", alexSite, strings.Replace(vhost, on, "\n    <IfModule rewrite_module>\n    RewriteEngine On\n    </IfModule>\n", 1), []string{"without RewriteEngine On"}},
		{"on in a Directory only", alexSite, strings.Replace(vhost, on, "\n    <Directory /var/www/alex>\n    RewriteEngine On\n    </Directory>\n", 1), []string{"without RewriteEngine On"}},
		{"httpd.conf: a virtual host without it", alexMain, main + "<VirtualHost 127.100.3.233:81>\nServerName x.example\n</VirtualHost>\n", []string{"without RewriteEngine On"}},
		// RewriteEngine Off before the URL is mapped.
		{"off in the virtual host", alexSite, inVhost("    RewriteEngine Off\n"), []string{"RewriteEngine Off goes into"}},
		{"off in custom directives", alexCust, "RewriteEngine off\n", []string{"RewriteEngine Off goes into"}},
		{"off in an IfModule of custom directives", alexCust, "<IfModule rewrite_module>\n    RewriteEngine Off\n</IfModule>\n", []string{"RewriteEngine Off goes into"}},
		{"off in a location", alexCust, "<Location \"/\">\n    RewriteEngine Off\n</Location>\n", []string{"RewriteEngine Off goes into"}},
		{"off in a LocationMatch", alexCust, "<LocationMatch \".\">\n    RewriteEngine \"Off\"\n</LocationMatch>\n", []string{"RewriteEngine Off goes into"}},
		{"off in an If", alexCust, "<If \"true\">\n    RewriteEngine Off\n</If>\n", []string{"RewriteEngine Off goes into"}},
		{"httpd.conf: off in a location", alexMain, main + "<Location \"/\">\nRewriteEngine Off\n</Location>\n", []string{"RewriteEngine Off goes into"}},
		// Options that put the virtual host's rules first, or drop the check.
		{"the virtual host's rules first", alexCust, "RewriteOptions Inherit\nRewriteRule ^/admin/ - [L]\n", []string{"Inherit"}},
		{"none of the server's", alexCust, "RewriteOptions AllowNoSlash IgnoreInherit\n", []string{"IgnoreInherit"}},
		{"in the virtual host itself", alexSite, inVhost("    RewriteOptions inherit\n"), []string{"inherit"}},
	})
	// After the mapping, and the virtual host's own rules, which run after
	// the check (InheritDownBefore).
	pass(t, alexCust, "RewriteEngine On\nRewriteRule ^/old$ /new [R=301,L]\nRewriteOptions InheritBefore AllowNoSlash\nRewriteMap lower int:tolower\n"+
		"<Directory /var/www/alex/data/www/example.com/static>\n    RewriteEngine Off\n    <If \"true\">\n        RewriteEngine Off\n    </If>\n</Directory>\n"+
		"<FilesMatch \"\\.txt$\">\n    RewriteEngine Off\n</FilesMatch>\n<Proxy \"fcgi://127.0.0.1:9000/\">\n    RewriteEngine Off\n</Proxy>\n"+
		"<Location /app>\n    RewriteEngine On\n    RewriteOptions Inherit\n    RewriteRule ^ /index.php [L]\n</Location>\n")
	pass(t, alexSite, inVhost("    RewriteRule ^/old$ /new [R=301,L]\n    <Directory /var/www/alex/data/www/example.com>\n        RewriteEngine Off\n    </Directory>\n"))
	// .htaccess-like per-directory rules of httpd.conf's own sections pass.
	pass(t, alexMain, strings.Replace(main, "\nInclude "+alexBackend+"\n", "\n<Directory /var/www/alex/data>\n    RewriteEngine On\n    RewriteRule ^x$ y [L]\n</Directory>\nInclude "+alexBackend+"\n", 1))
}

// A location that proxies to an account's Apache may include that account's
// secret, next to its other headers, and nothing below it proxies; no other
// place includes one.
func TestNginxApacheBackendInclude(t *testing.T) {
	loc := func(body string) string { return "server {\n    listen 80;\n    location / {\n" + body + "    }\n}\n" }
	proxy := "        include monopanel/snippets/proxy-apache.conf;\n"
	own := "        include monopanel/apache/alex.conf;\n"
	toAlex := "        proxy_pass http://127.100.3.233:81;\n"
	pass(t, alexSiteNginx, loc(proxy+own+toAlex))
	pass(t, alexSiteNginx, loc(proxy+"        include /etc/nginx/monopanel/apache/alex.conf;\n"+toAlex))
	pass(t, alexSiteNginx, "server {\n    location @apache {\n"+proxy+own+toAlex+"    }\n}\n")
	pass(t, alexSiteNginx, loc(own+"        proxy_pass http://127.100.3.233:81/app/;\n"))
	pass(t, alexSiteNginx, loc(proxy+own+toAlex+"        if ($request_method = POST) { return 405; }\n"))
	// A site of alex's in Apache mode as the panel renders it.
	pass(t, alexSiteNginx, renderT(t, "nginx/site.conf.tmpl", site("apache", func(s *render.Site) { tls(s); s.StaticByNginx = true })))
	runRefusals(t, []refusal{
		{"another account's secret", alexSiteNginx, loc(proxy + "        include monopanel/apache/shop.conf;\n" + toAlex), []string{"shop", "127.100.3.234:81"}},
		{"to another Apache", alexSiteNginx, loc(proxy + own + "        proxy_pass http://127.100.3.234:81;\n"), []string{"127.100.3.233:81"}},
		{"to the outside", alexSiteNginx, loc(proxy + own + "        proxy_pass http://203.0.113.9:81;\n"), []string{"127.100.3.233:81"}},
		{"to a variable", alexSiteNginx, loc(proxy + own + "        proxy_pass http://127.100.3.233:81$request_uri;\n"), []string{"127.100.3.233:81"}},
		{"a host that starts with the address", alexSiteNginx, loc(proxy + own + "        proxy_pass http://127.100.3.233:8100;\n"), []string{"127.100.3.233:81"}},
		{"without a proxy_pass", alexSiteNginx, loc(proxy + own + "        return 200;\n"), []string{"127.100.3.233:81"}},
		{"two proxy_pass", alexSiteNginx, loc(proxy + own + toAlex + toAlex), []string{"127.100.3.233:81"}},
		{"a proxy_pass of two words", alexSiteNginx, loc(proxy + own + "        proxy_pass http://127.100.3.233:81 x;\n"), []string{"127.100.3.233:81"}},
		{"a path from a variable", alexSiteNginx, loc(proxy + own + "        proxy_pass http://127.100.3.233:81/$uri;\n"), []string{"127.100.3.233:81"}},
		{"at the server level", alexSiteNginx, "server {\n    listen 80;\n    include monopanel/apache/alex.conf;\n    location / {\n" + proxy + toAlex + "    }\n}\n", []string{"location"}},
		{"at the http level", "/etc/nginx/monopanel/http.d/x.conf", "include monopanel/apache/alex.conf;\n", []string{"location"}},
		{"in an if", alexSiteNginx, loc(proxy + toAlex + "        if ($x) {\n    include monopanel/apache/alex.conf;\n        }\n"), []string{"location"}},
		{"in an if that proxies there", alexSiteNginx, loc(proxy + toAlex + "        if ($x) {\n    include monopanel/apache/alex.conf;\n" + toAlex + "        }\n"), []string{"location"}},
		{"a nested location proxies", alexSiteNginx, loc(proxy + own + toAlex + "        location /x { proxy_pass http://203.0.113.9; }\n"), []string{"location", "inherits"}},
		{"an if proxies", alexSiteNginx, loc(proxy + own + toAlex + "        if ($x) { proxy_pass http://203.0.113.9; }\n"), []string{"if", "inherits"}},
		{"a wildcard", alexSiteNginx, loc(proxy + "        include monopanel/apache/*.conf;\n" + toAlex), []string{"by its name"}},
		{"the directory", alexSiteNginx, loc(proxy + "        include monopanel/apache;\n" + toAlex), []string{"by its name"}},
		{"root's", alexSiteNginx, loc(proxy + "        include monopanel/apache/root.conf;\n" + toAlex), []string{"by its name"}},
		{"an unknown account's", alexSiteNginx, loc(proxy + "        include monopanel/apache/ghost.conf;\n" + toAlex), []string{"ghost"}},
		{"in a map", "/etc/nginx/monopanel/http.d/x.conf", "map $x $y {\n    include monopanel/apache/alex.conf;\n}\n", []string{"location"}},
		{"in custom directives at the server level", "/etc/nginx/monopanel/sites/example.com.d/custom.conf", "include monopanel/apache/alex.conf;\n", []string{"location"}},
	})
	// A custom location to alex's Apache with alex's secret passes.
	pass(t, "/etc/nginx/monopanel/sites/example.com.d/custom.conf", "location /app/ {\n"+proxy+own+toAlex+"}\n")

	// A proxy_pass to a hosting account's Apache comes with its secret, or
	// the Apache answers the location 403: a template override or custom
	// directives without it fail when they are applied.
	runRefusals(t, []refusal{
		{"alex's Apache without the secret", alexSiteNginx, loc(proxy + toAlex), []string{"alex", "include monopanel/apache/alex.conf;"}},
		{"a path on it without the secret", "/etc/nginx/monopanel/sites/example.com.d/custom.conf", "location /app/ {\n" + proxy + "        proxy_pass http://127.100.3.233:81/app/;\n}\n", []string{"include monopanel/apache/alex.conf;"}},
		{"shop's Apache without shop's secret", alexSiteNginx, loc(proxy + "        proxy_pass http://127.100.3.234:81;\n"), []string{"shop", "include monopanel/apache/shop.conf;"}},
		{"with a variable", alexSiteNginx, loc(proxy + "        proxy_pass http://127.100.3.233:81$request_uri;\n"), []string{"include monopanel/apache/alex.conf;"}},
		{"in upper case", alexSiteNginx, loc(proxy + "        proxy_pass HTTP://127.100.3.233:81;\n"), []string{"include monopanel/apache/alex.conf;"}},
		{"from an if", alexSiteNginx, loc(proxy + "        if ($x) { proxy_pass http://127.100.3.233:81; }\n        return 404;\n"), []string{"include monopanel/apache/alex.conf;"}},
	})
	// What is no hosting account's Apache needs none.
	pass(t, alexSiteNginx, loc(proxy+"        proxy_pass http://127.0.0.1:81;\n"))
	pass(t, alexSiteNginx, loc(proxy+"        proxy_pass http://127.100.0.33:81;\n"))
	pass(t, alexSiteNginx, loc(proxy+"        proxy_pass http://127.100.3.233:8080;\n"))
	pass(t, alexSiteNginx, loc(proxy+"        proxy_pass https://127.100.3.233:81;\n"))
}

// Nothing but the account's files names the header: neither a template
// override nor custom directives set, drop, test or copy it.
func TestBackendHeaderNamedNowhereElse(t *testing.T) {
	secret := testBackendSecret
	runRefusals(t, []refusal{
		{"nginx: set in a location", alexSiteNginx, "server {\n    location / {\n        proxy_set_header X-MonoPanel-Backend \"" + secret + "\";\n        proxy_pass http://127.100.3.233:81;\n    }\n}\n", []string{"X-MonoPanel-Backend"}},
		{"nginx: set at the server level", alexSiteNginx, "server {\n    proxy_set_header x-monopanel-backend \"" + secret + "\";\n}\n", []string{"X-MonoPanel-Backend"}},
		{"nginx: emptied", "/etc/nginx/monopanel/sites/example.com.d/custom.conf", "location /x {\n    proxy_set_header X-MonoPanel-Backend \"\";\n    proxy_pass http://127.100.3.233:81;\n}\n", []string{"X-MonoPanel-Backend"}},
		{"nginx: headers-more", "/etc/nginx/monopanel/sites/example.com.d/custom.conf", "more_set_input_headers 'X-MonoPanel-Backend: x';\n", []string{"X-MonoPanel-Backend"}},
		{"nginx: the client's copy", "/etc/nginx/monopanel/sites/example.com.d/custom.conf", "location /x {\n    proxy_set_header X-Real-IP $http_x_monopanel_backend;\n    proxy_pass http://127.0.0.1:3000;\n}\n", []string{"X-MonoPanel-Backend"}},
		{"nginx: in a snippet", "/etc/nginx/monopanel/snippets/proxy-apache.conf", "proxy_set_header X-MonoPanel-Backend \"" + secret + "\";\n", []string{"X-MonoPanel-Backend"}},
		{"apache: forged early", alexMain, "RequestHeader set X-MonoPanel-Backend \"" + secret + "\" early\n", []string{"X-MonoPanel-Backend"}},
		{"apache: dropped early", alexSite, "RequestHeader unset X-MonoPanel-Backend early\n", []string{"X-MonoPanel-Backend"}},
		{"apache: an If of a site's", alexCust, "<If \"req('x-monopanel-backend') == ''\">\n    Require all granted\n</If>\n", []string{"X-MonoPanel-Backend"}},
		{"apache: into a variable", alexCust, "SetEnvIf X-MonoPanel-Backend (.*) LEAK=$1\n", []string{"X-MonoPanel-Backend"}},
		{"apache: into a log", alexCust, "LogFormat \"%{X-MonoPanel-Backend}i\" leak\n", []string{"X-MonoPanel-Backend"}},
		{"apache: back to PHP", alexCust, "ProxyFCGISetEnvIf \"true\" HTTP_X_MONOPANEL_BACKEND \"%{HTTP:X-MonoPanel-Backend}\"\n", []string{"X-MonoPanel-Backend"}},
		{"apache: elsewhere in /etc/apache2", "/etc/apache2/conf-available/x.conf", "RequestHeader set X-MonoPanel-Backend x early\n", []string{"X-MonoPanel-Backend"}},
	})
}
