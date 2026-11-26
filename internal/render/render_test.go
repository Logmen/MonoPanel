package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func exampleSite(mode string) Site {
	return Site{
		Domain: "example.com", Aliases: []string{"www.example.com"}, Mode: mode, IP: "203.0.113.10", IPv6: "2001:db8::10",
		TLS: true, HTTP2: true, HTTP3: true, RedirectHTTPS: true, HSTS: true, RedirectWWW: "to_root", SecurityHeaders: true, RateLimit: 10, NoPHPPaths: []string{"upload/exchange"},
		CertPath: "/var/lib/monopanel/certs/example.com/fullchain.pem", KeyPath: "/var/lib/monopanel/certs/example.com/privkey.pem",
		Docroot: "/var/www/alex/data/www/example.com", LogDir: "/var/log/monopanel/sites/alex",
		IncludeDir: "monopanel/sites/example.com.d", ClientMaxBodySize: "64m", StaticByNginx: true,
		ApacheBackend: "127.100.3.233:81", ApacheBackendConf: "monopanel/apache/alex.conf", FPMSocket: "/run/monopanel/php/example.com.sock", ProxyTimeout: 150,
	}
}

// exampleAccount is the Apache of alex (uid 1001) as apache.go hands it over.
func exampleAccount() ApacheAccount {
	return ApacheAccount{Login: "alex", Listen: "127.100.3.233:81", RunDir: "/run/monopanel-apache/alex", Home: "/var/www/alex",
		ErrorLog: "/var/log/monopanel/sites/alex/apache.error.log", SitesDir: "/etc/apache2/monopanel/accounts/alex/sites", Backend: "/etc/apache2/monopanel/accounts/alex/backend.conf"}
}

func examplePool() Pool {
	return Pool{
		Name: "example.com", User: "alex", Group: "alex", Socket: "/run/monopanel/php/example.com.sock", ListenGroup: "monopanel-web",
		PM: "ondemand", MaxChildren: 8, TerminateTimeout: 150, Home: "/var/www/alex", DataDir: "/var/www/alex/data",
		TmpDir: "/var/www/alex/data/tmp", LogDir: "/var/log/monopanel/sites/alex", BinDir: "/var/www/alex/data/bin",
		SendmailFrom: "noreply@example.com", DisableFunctions: DefaultDisableFunctions, OpenBasedir: true,
		Values: []KV{{"memory_limit", "256M"}, {"upload_max_filesize", "64M"}, {"post_max_size", "64M"}, {"max_execution_time", "120"}, {"date.timezone", "Europe/Moscow"}, {"display_errors", "Off"}},
	}
}

func TestGolden(t *testing.T) {
	r := New("")
	cases := []struct {
		golden string
		tmpl   string
		data   any
		must   []string
	}{
		{"nginx-main.conf", "nginx/nginx.conf.tmpl", NginxMain{User: "nginx"}, []string{"user  nginx;", "include /etc/nginx/monopanel/sites/*.conf;"}},
		{"nginx-ip-default.conf", "nginx/ip-default.conf.tmpl", IPDefault{IP: "203.0.113.10", TLS: true, HTTP3: true, CertPath: "/c.pem", KeyPath: "/k.pem", PanelHost: "panel.example.com", PanelPort: 8443}, []string{"quic reuseport", "default_server"}},
		{"nginx-ip-default-nocert.conf", "nginx/ip-default.conf.tmpl", IPDefault{IP: "[2001:db8::10]", TLS: true, PanelHost: "panel.example.com", PanelPort: 8443}, []string{"listen [2001:db8::10]:443 ssl default_server;", "ssl_reject_handshake on;", "listen [2001:db8::10]:80 default_server;"}},
		{"nginx-ip-default-nosni.conf", "nginx/ip-default.conf.tmpl", IPDefault{IP: "203.0.113.10", TLS: true, CertPath: "/c.pem", KeyPath: "/k.pem", NoSNISite: "kz.example.com"}, []string{"listen 203.0.113.10:443 ssl default_server;", "ssl_certificate     /c.pem;", "return 301 https://kz.example.com$request_uri;", `server_name "~[a-z]";`, "ssl_reject_handshake on;"}},
		{"nginx-site-fpm.conf", "nginx/site.conf.tmpl", exampleSite("fpm"), []string{"location ~ ^/[^/]+\\.php(?:/|$) {", "location ~ ^/upload/exchange/.*\\.(?:php|phtml|phar)(?:/|$) { return 403; }", "fastcgi_pass unix:/run/monopanel/php/example.com.sock;", "return 301 https://$host$request_uri;", "listen 203.0.113.10:443 quic;", "listen [2001:db8::10]:443 ssl;", "listen [2001:db8::10]:443 quic;", "if ($host = www.example.com)", "add_header Strict-Transport-Security \"max-age=31536000\" always;", "add_header X-Frame-Options SAMEORIGIN always;", "map $uri $mp_example_com_key", "limit_req_zone $mp_example_com_key zone=mp_example_com:2m rate=10r/s;", "limit_req zone=mp_example_com burst=20 nodelay;", "limit_req_status 429;", "access_log /var/log/monopanel/sites/alex/example.com.access.log main;", "error_log  /var/log/monopanel/sites/alex/example.com.error.log;"}},
		{"nginx-site-proxy.conf", "nginx/site.conf.tmpl", func() Site { s := exampleSite("proxy"); s.Backend = "http://127.0.0.1:3000"; return s }(), []string{"proxy_pass http://127.0.0.1:3000;", "proxy-app.conf"}},
		{"nginx-site-apache.conf", "nginx/site.conf.tmpl", exampleSite("apache"), []string{"proxy_pass http://127.100.3.233:81;", "try_files $uri @apache;", "proxy-apache.conf",
			// The account's secret in both locations that proxy to its Apache,
			// at the level of the snippet's headers.
			"    location / {\n        include monopanel/snippets/proxy-apache.conf;\n        include monopanel/apache/alex.conf;\n        proxy_pass http://127.100.3.233:81;\n    }\n",
			"    location @apache {\n        include monopanel/snippets/proxy-apache.conf;\n        include monopanel/apache/alex.conf;\n        proxy_pass http://127.100.3.233:81;\n    }\n"}},
		{"apache-site.conf", "apache/site.conf.tmpl", func() Site {
			s := exampleSite("apache")
			s.IncludeDir = "/etc/apache2/monopanel/accounts/alex/sites/example.com.d"
			return s
		}(), []string{"<VirtualHost 127.100.3.233:81>", "ServerAlias www.example.com", "proxy:unix:/run/monopanel/php/example.com.sock|fcgi://localhost", "ProxyTimeout 150", "ErrorLog  /var/log/monopanel/sites/alex/example.com.apache.error.log", "CustomLog /var/log/monopanel/sites/alex/example.com.apache.access.log combined",
			"        AllowOverride All\n", "IncludeOptional /etc/apache2/monopanel/accounts/alex/sites/example.com.d/*.conf",
			// The shared folder: no .htaccess of the guest's read, no PHP from it.
			"    <Directory /var/www/alex/data/www/example.com/upload/exchange>\n        AllowOverride None\n        Options +SymLinksIfOwnerMatch\n        <FilesMatch \"\\.(?:php|phtml|phar)$\">\n            Require all denied\n",
			// Inherits the rewrite rule that refuses what nginx did not send.
			"\n    RewriteEngine On\n    DocumentRoot /var/www/alex/data/www/example.com\n"}},
		{"apache-account.conf", "apache/account.conf.tmpl", exampleAccount(), []string{"ServerRoot /etc/apache2\n", "PidFile /run/monopanel-apache/alex/httpd.pid\n", "DefaultRuntimeDir /run/monopanel-apache/alex\n",
			"Mutex file:/run/monopanel-apache/alex default\n", "User alex\nGroup alex\n", "Listen 127.100.3.233:81\n", "LoadModule mpm_event_module /usr/lib/apache2/modules/mod_mpm_event.so\n",
			"LoadModule proxy_fcgi_module /usr/lib/apache2/modules/mod_proxy_fcgi.so\n", "ThreadsPerChild 16\n", "MaxRequestWorkers 64\n", "ErrorLog /var/log/monopanel/sites/alex/apache.error.log\n",
			"TypesConfig /etc/mime.types\n", "RemoteIPInternalProxy 127.0.0.1\n", "<Directory /var/www/alex>\n", "    AllowOverride All\n", "IncludeOptional /etc/apache2/monopanel/accounts/alex/sites/*.conf\n",
			// The check of nginx's secret at the top level, and the directories
			// with mod_rewrite off, which the virtual hosts switch on.
			"\nInclude /etc/apache2/monopanel/accounts/alex/backend.conf\n", "<Directory />\n    Options FollowSymLinks\n    AllowOverride None\n    RewriteEngine Off\n"}},
		{"mysql.cnf", "mysql/monopanel.cnf.tmpl", MySQLConf{RAMMB: 4096, NativePassword: true, Settings: []MySQLSetting{{Key: "bind-address", Value: "127.0.0.1"}, {Key: "innodb_buffer_pool_size", Value: "1024M"}, {Key: "innodb_strict_mode", Value: "OFF", Comment: []string{"Strict mode refuses oversized rows."}}, {Key: "disable_log_bin", Flag: true}, {Key: "sql_mode", Value: "''", Comment: []string{"set in the panel; the panel's value: NO_ENGINE_SUBSTITUTION"}}}}, []string{"innodb_buffer_pool_size = 1024M", "# Strict mode refuses oversized rows.\ninnodb_strict_mode = OFF", "\ndisable_log_bin\n", "sql_mode = ''", "mysql_native_password = ON"}},
		{"firewall.nft", "nftables/monopanel.nft.tmpl", Firewall{Policy: "drop", Except: []string{"ip saddr 198.51.100.4 tcp dport 8443 accept"}, Deny: []string{"ip saddr 203.0.113.7 drop", "tcp dport 8443 drop"}, Allow: []string{"tcp dport { 22, 80, 443, 8443 } accept"}}, []string{"policy drop;", "tcp dport { 22, 80, 443, 8443 } accept", "ip saddr 203.0.113.7 drop", "ip saddr 198.51.100.4 tcp dport 8443 accept"}},
		{"fail2ban-jail.local", "fail2ban/jail.local.tmpl", Fail2ban{BanTime: "1h", FindTime: "10m", MaxRetry: 5, PanelPort: "8443", Nginx: true, IgnoreIPs: []string{"198.51.100.7", "203.0.113.0/24"}, AccessLogs: []string{"/var/log/monopanel/sites/alex/shop.example.com.access.log"}, ErrorLogs: []string{"/var/log/monopanel/sites/alex/shop.example.com.error.log"}}, []string{"[monopanel]", "port = 8443", "enabled = true", "ignoreip = 127.0.0.1/8 ::1 198.51.100.7 203.0.113.0/24", "          /var/log/monopanel/sites/alex/shop.example.com.access.log"}},
		{"crontab", "cron/crontab.tmpl", Crontab{Login: "alex", BinDir: "/var/www/alex/data/bin", Jobs: []CronLine{{Schedule: "*/5 * * * *", Command: "php cron.php", Enabled: true, Comment: "wp"}, {Schedule: "@daily", Command: "echo x", Enabled: false}}}, []string{"*/5 * * * * php cron.php  # wp", "#DISABLED# @daily echo x"}},
		{"php-ini.conf", "php/monopanel.ini.tmpl", PHPIni{Version: "8.4", Timezone: "Asia/Almaty", OpcacheMemory: 128, Global: []KV{{Key: "max_input_vars", Value: "10000"}, {Key: "memory_limit", Value: "512M"}}}, []string{"opcache.memory_consumption = 128", "date.timezone = Asia/Almaty", "\nmax_input_vars = 10000\nmemory_limit = 512M\n"}},
		{"nginx-site-webmail-port.conf", "mail/webmail.conf.tmpl", Webmail{IP: "127.0.0.1", Port: 2096, ServerName: "example.com", CertPath: "/var/lib/monopanel/certs/example.com/fullchain.pem", KeyPath: "/var/lib/monopanel/certs/example.com/privkey.pem", Docroot: "/var/www/alex/data/www/example.com", LogDir: "/var/log/monopanel/sites/alex", Socket: "/run/monopanel/php/example.com.sock", ClientMaxBodySize: "64m"}, []string{"listen 127.0.0.1:2096 ssl;", "location ~ ^/[^/]+\\.php(?:/|$) {", "if (!-f $document_root$fastcgi_script_name) { return 404; }", "access_log /var/log/monopanel/sites/alex/webmail.access.log main;"}},
		{"nginx-site-suspended.conf", "nginx/site-suspended.conf.tmpl", exampleSite("fpm"), []string{"return 503", "listen 203.0.113.10:443 ssl;", "The site is temporarily unavailable", "Сайт временно недоступен"}},
		{"site-index.html", "site/index.html.tmpl", Welcome{Domain: "example.com"}, []string{"<html lang=\"en\">", "A website is coming soon", "Скоро здесь будет сайт", "example.com", "prefers-reduced-motion"}},
		{"nginx-site-allow.conf", "nginx/site.conf.tmpl", func() Site {
			s := exampleSite("fpm")
			s.AllowFrom = []string{"203.0.113.0/24", "2001:db8::1"}
			return s
		}(), []string{"allow 203.0.113.0/24;", "allow 2001:db8::1;", "deny  all;"}},
		{"nginx-real-ip.conf", "nginx/real-ip.conf.tmpl", RealIP{Cloudflare: true, CloudflareRanges: CloudflareRanges}, []string{"set_real_ip_from 173.245.48.0/20;", "real_ip_header    CF-Connecting-IP;"}},
		{"systemd-app.service", "systemd/app.service.tmpl", AppUnit{Login: "alex", Name: "web", Description: "gunicorn", Command: "/var/www/alex/data/venv/bin/gunicorn --bind 127.0.0.1:5000 app:app", WorkDir: "/var/www/alex/data/www/app.example.com", EnvFile: "/var/www/alex/data/.env", PanelEnvFile: "/etc/monopanel/apps/monopanel-app-alex-web.env", Env: []string{"PORT=5000"}, Restart: "always", Binary: "/usr/bin/monopanel"}, []string{"User=alex", "ExecStart=/usr/bin/monopanel app-exec --env-file /var/www/alex/data/.env -- /var/www/alex/data/venv/bin/gunicorn --bind 127.0.0.1:5000 app:app", "EnvironmentFile=/etc/monopanel/apps/monopanel-app-alex-web.env", "Restart=always"}},
		{"systemd-valkey-cache.service", "systemd/valkey.service.tmpl", ValkeyUnit{Login: "alex", Purpose: "cache", Name: "alex-cache", Engine: "valkey", Binary: "/usr/bin/valkey-server", Socket: "/run/monopanel-valkey/alex-cache/valkey.sock", DataDir: "/var/lib/monopanel-valkey/alex-cache", MemoryMB: 128, MemoryMax: 320}, []string{"User=alex", "ExecStart=/usr/bin/valkey-server --supervised systemd --daemonize no --port 0 \\", "--maxmemory-policy allkeys-lru --save \"\"", "RuntimeDirectory=monopanel-valkey/alex-cache", "MemoryMax=320M"}},
		{"systemd-valkey-sessions.service", "systemd/valkey.service.tmpl", ValkeyUnit{Login: "alex", Purpose: "sessions", Name: "alex-sessions", Engine: "redis", Binary: "/usr/bin/redis-server", Socket: "/run/monopanel-valkey/alex-sessions/valkey.sock", DataDir: "/var/lib/monopanel-valkey/alex-sessions", MemoryMB: 64, MemoryMax: 192, Sessions: true}, []string{"ExecStart=/usr/bin/redis-server --supervised systemd --daemonize no --port 0 \\", "--maxmemory-policy volatile-lru --save 60 1", "StateDirectory=monopanel-valkey/alex-sessions"}},
		{"php-fpm-pool-valkey.conf", "php-fpm/pool.conf.tmpl", func() Pool {
			p := examplePool()
			p.SessionSocket = "/run/monopanel-valkey/alex-sessions/valkey.sock"
			return p
		}(), []string{"php_value[session.save_handler] = redis", "php_value[session.save_path] = \"unix:///run/monopanel-valkey/alex-sessions/valkey.sock\""}},
		{"logrotate-sites", "logrotate/sites.tmpl", SiteLogrotate{NginxPID: "/run/nginx.pid", Accounts: []LogrotateAccount{
			{Login: "alex", LogDir: "/var/log/monopanel/sites/alex", ApachePID: "/run/monopanel-apache/alex/httpd.pid"},
			{Login: "shop", LogDir: "/var/log/monopanel/sites/shop", ApachePID: "/run/monopanel-apache/shop/httpd.pid"}}}, []string{
			"/var/log/monopanel/sites/alex/*.access.log /var/log/monopanel/sites/alex/*[!p].error.log /var/log/monopanel/sites/alex/*[!h]p.error.log /var/log/monopanel/sites/alex/*[!p]hp.error.log /var/log/monopanel/sites/alex/*[!.]php.error.log /var/log/monopanel/sites/alex/*.php.slow.log {",
			"create 0640 root alex", "\n/var/log/monopanel/sites/alex/*.php.error.log {", "create 0660 alex alex",
			"create 0640 root shop", "\n/var/log/monopanel/sites/shop/*.php.error.log {", "create 0660 shop shop",
			"kill -USR1 \"$(cat /run/nginx.pid)\"", "kill -USR1 \"$(cat /run/monopanel-apache/alex/httpd.pid)\"", "kill -USR1 \"$(cat /run/monopanel-apache/shop/httpd.pid)\""}},
		{"logrotate-sites-shared-apache", "logrotate/sites.tmpl", SiteLogrotate{NginxPID: "/run/nginx.pid", SharedApachePID: "/run/apache2/apache2.pid", Accounts: []LogrotateAccount{
			{Login: "alex", LogDir: "/var/log/monopanel/sites/alex", ApachePID: "/run/monopanel-apache/alex/httpd.pid"}}}, []string{
			"kill -USR1 \"$(cat /run/apache2/apache2.pid)\"", "kill -USR1 \"$(cat /run/monopanel-apache/alex/httpd.pid)\""}},
		{"logrotate-sites-nginx-only", "logrotate/sites.tmpl", SiteLogrotate{NginxPID: "/run/nginx.pid", Accounts: []LogrotateAccount{{Login: "alex", LogDir: "/var/log/monopanel/sites/alex"}}}, []string{
			"kill -USR1 \"$(cat /run/nginx.pid)\" 2>/dev/null || true\n\tendscript"}},
		{"php-fpm-pool.conf", "php-fpm/pool.conf.tmpl", examplePool(), []string{"[example.com]", "listen.group = monopanel-web", "php_value[memory_limit] = 256M", "pm = ondemand", "slowlog = /var/log/monopanel/sites/alex/example.com.php.slow.log", "php_admin_value[error_log]         = /var/log/monopanel/sites/alex/example.com.php.error.log"}},
	}
	presets := []string{"nginx/presets/wordpress.conf.tmpl", "nginx/presets/joomla.conf.tmpl", "nginx/presets/bitrix.conf.tmpl", "nginx/presets/opencart.conf.tmpl"}
	for _, pr := range []struct{ id, must string }{{"wordpress", "location = /xmlrpc.php"}, {"joomla", "location /api/ { try_files $uri $uri/ /api/index.php?$args; }"}, {"bitrix", "SCRIPT_FILENAME $document_root/bitrix/urlrewrite.php"}, {"opencart", "rewrite ^/(.+)$ /index.php?_route_=$1 last;"}} {
		pr := pr
		cases = append(cases, struct {
			golden string
			tmpl   string
			data   any
			must   []string
		}{"nginx-site-" + pr.id + ".conf", "nginx/site.conf.tmpl", func() Site { s := exampleSite("fpm"); s.Preset = pr.id; return s }(), []string{pr.must, "fastcgi_pass unix:/run/monopanel/php/example.com.sock;", "add_header Strict-Transport-Security"}})
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			got, err := r.RenderSet(c.tmpl, c.data, presets...)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, "<no value>") {
				t.Fatal("template rendered <no value>")
			}
			for _, m := range c.must {
				if !strings.Contains(got, m) {
					t.Errorf("missing %q in output:\n%s", m, got)
				}
			}
			path := filepath.Join("testdata", c.golden+".golden")
			if *update {
				os.MkdirAll("testdata", 0o755)
				os.WriteFile(path, []byte(got), 0o644)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("golden missing (run go test ./internal/render -update): %v", err)
			}
			if string(want) != got {
				t.Errorf("output differs from %s:\n--- want\n%s\n--- got\n%s", path, want, got)
			}
		})
	}
}

func TestOverrideAndList(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "nginx"), 0o755)
	os.WriteFile(filepath.Join(dir, "nginx", "nginx.conf.tmpl"), []byte("custom {{ .User }}"), 0o644)
	r := New(dir)
	got, err := r.Render("nginx/nginx.conf.tmpl", NginxMain{User: "x"})
	if err != nil || got != "custom x" {
		t.Fatalf("override: %q %v", got, err)
	}
	if _, overridden, _ := r.Source("nginx/site.conf.tmpl"); overridden {
		t.Fatal("site template must not be overridden")
	}
	list, _ := r.List()
	if len(list) < 8 {
		t.Fatalf("list too short: %v", list)
	}
	sn, err := r.Snippets("nginx/snippets")
	if err != nil || sn["acme.conf"] == "" || sn["fastcgi.conf"] == "" {
		t.Fatalf("snippets: %v %v", sn, err)
	}
	if _, err := r.Render("nginx/site.conf.tmpl", map[string]any{"Domain": "x"}); err == nil {
		t.Fatal("missing keys must error")
	}
}

// A value with a line break never reaches a configuration file: in each
// format the panel renders it would start a directive of its own. The error
// names the field.
func TestRenderRefusesLineBreaks(t *testing.T) {
	r := New("")
	presets := []string{"nginx/presets/wordpress.conf.tmpl", "nginx/presets/joomla.conf.tmpl", "nginx/presets/bitrix.conf.tmpl", "nginx/presets/opencart.conf.tmpl"}
	site := func(edit func(*Site)) Site { s := exampleSite("fpm"); edit(&s); return s }
	pool := func(edit func(*Pool)) Pool { p := examplePool(); edit(&p); return p }
	for _, c := range []struct {
		tmpl  string
		data  any
		field string
	}{
		{"nginx/site.conf.tmpl", site(func(s *Site) { s.ClientMaxBodySize = "1m;\naccess_log /etc/cron.d/x main" }), "Site.ClientMaxBodySize"},
		{"nginx/site.conf.tmpl", site(func(s *Site) { s.Aliases = []string{"www.example.com", "x.example.com;\r\nlisten 1"} }), "Site.Aliases[1]"},
		{"nginx/site.conf.tmpl", site(func(s *Site) { s.Docroot = "/var/www/alex/data/www/example.com\x00" }), "Site.Docroot"},
		{"apache/site.conf.tmpl", site(func(s *Site) { s.Domain = "example.com\nInclude /etc/shadow" }), "Site.Domain"},
		{"php-fpm/pool.conf.tmpl", pool(func(p *Pool) { p.Values = append(p.Values, KV{"memory_limit", "1G\nslowlog = /etc/cron.d/x"}) }), "Pool.Values[6].Value"},
		{"systemd/app.service.tmpl", &AppUnit{Login: "alex", Name: "web", Command: "/bin/true", WorkDir: "/var/www/alex/data", Restart: "always", Env: []string{"A=1\nExecStartPre=+/bin/sh"}}, "AppUnit.Env[0]"},
		{"cron/crontab.tmpl", Crontab{Login: "alex", BinDir: "/var/www/alex/data/bin", Jobs: []CronLine{{Schedule: "@daily", Command: "true", Comment: "x\n* * * * * id"}}}, "Crontab.Jobs[0].Comment"},
		{"mail/dovecot.conf.tmpl", Dovecot{Hostname: "mail.example.com\r", MailBase: "/var/mail", UsersFile: "/etc/dovecot/users", VmailUser: "vmail"}, "Dovecot.Hostname"},
	} {
		_, err := r.RenderSet(c.tmpl, c.data, presets...)
		if err == nil || !strings.Contains(err.Error(), c.field+" holds a line break") || !strings.Contains(err.Error(), c.tmpl) {
			t.Errorf("%s with a line break in %s: %v", c.tmpl, c.field, err)
		}
		if _, err := r.Render(c.tmpl, c.data); err == nil {
			t.Errorf("Render %s let %s through", c.tmpl, c.field)
		}
	}
	// Maps are walked too, keys included.
	if err := oneLineValues(map[string]string{"ok": "x", "bad\nkey": "y"}); err == nil || !strings.Contains(err.Error(), `data["bad\nkey"]`) {
		t.Errorf("map key: %v", err)
	}
	// Tabs and ordinary values stay as they were.
	if err := oneLineValues(CronLine{Schedule: "@daily", Command: "printf 'a\tb'", Enabled: true}); err != nil {
		t.Errorf("a tab is not a line break: %v", err)
	}
}

// What nginx answers 404 for in every site (snippets/deny-dotfiles.conf) an
// account's Apache refuses too, with the same expressions: any local process
// can ask the Apache straight, past nginx — another account's for a site's
// .env or .git/config.
func TestAccountApacheDeniesWhatNginxDenies(t *testing.T) {
	r := New("")
	snippet, _, err := r.Source("nginx/snippets/deny-dotfiles.conf")
	if err != nil {
		t.Fatal(err)
	}
	conf, err := r.Render("apache/account.conf.tmpl", exampleAccount())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(snippet), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "location" || (f[1] != "~" && f[1] != "~*") {
			continue
		}
		re := f[2]
		if f[1] == "~*" {
			re = "(?i)" + re
		}
		if want := "<LocationMatch \"" + re + "\">\n    Require all denied\n</LocationMatch>\n"; !strings.Contains(conf, want) {
			t.Errorf("the account's Apache lacks\n%s", want)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no location of nginx's snippet read")
	}
}
