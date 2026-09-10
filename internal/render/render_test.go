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
		Domain: "example.com", Aliases: []string{"www.example.com"}, Mode: mode, IP: "203.0.113.10",
		TLS: true, HTTP2: true, HTTP3: true, RedirectHTTPS: true, HSTS: true, RedirectWWW: "to_root",
		CertPath: "/var/lib/monopanel/certs/example.com/fullchain.pem", KeyPath: "/var/lib/monopanel/certs/example.com/privkey.pem",
		Docroot: "/var/www/alex/data/www/example.com", LogDir: "/var/www/alex/data/logs",
		IncludeDir: "monopanel/sites/example.com.d", ClientMaxBodySize: "64m", StaticByNginx: true,
		ApacheBackend: "127.0.0.1:8080", FPMSocket: "/run/monopanel/php/example.com.sock", ProxyTimeout: 150,
	}
}

func examplePool() Pool {
	return Pool{
		Name: "example.com", User: "alex", Group: "alex", Socket: "/run/monopanel/php/example.com.sock", ListenGroup: "monopanel-web",
		PM: "ondemand", MaxChildren: 8, TerminateTimeout: 150, Home: "/var/www/alex", DataDir: "/var/www/alex/data",
		TmpDir: "/var/www/alex/data/tmp", LogDir: "/var/www/alex/data/logs", BinDir: "/var/www/alex/data/bin",
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
		{"nginx-site-fpm.conf", "nginx/site.conf.tmpl", exampleSite("fpm"), []string{"fastcgi_pass unix:/run/monopanel/php/example.com.sock;", "return 301 https://$host$request_uri;", "listen 203.0.113.10:443 quic;", "if ($host = www.example.com)", "add_header Strict-Transport-Security \"max-age=31536000\" always;"}},
		{"nginx-site-proxy.conf", "nginx/site.conf.tmpl", func() Site { s := exampleSite("proxy"); s.Backend = "http://127.0.0.1:3000"; return s }(), []string{"proxy_pass http://127.0.0.1:3000;", "proxy-app.conf"}},
		{"nginx-site-apache.conf", "nginx/site.conf.tmpl", exampleSite("apache"), []string{"proxy_pass http://127.0.0.1:8080;", "try_files $uri @apache;", "proxy-apache.conf"}},
		{"apache-site.conf", "apache/site.conf.tmpl", exampleSite("apache"), []string{"<VirtualHost 127.0.0.1:8080>", "ServerAlias www.example.com", "proxy:unix:/run/monopanel/php/example.com.sock|fcgi://localhost", "ProxyTimeout 150"}},
		{"apache-main.conf", "apache/httpd.conf.tmpl", ApacheMain{Listen: "127.0.0.1:8080", Backend: "127.0.0.1:8080", SitesDir: "/etc/apache2/monopanel/sites"}, []string{"Listen 127.0.0.1:8080", "RemoteIPHeader X-Real-IP"}},
		{"mysql.cnf", "mysql/monopanel.cnf.tmpl", MySQLConf{RAMMB: 4096, BindAddress: "127.0.0.1", BufferPoolMB: 1024, RedoLogMB: 256, MaxConnections: 200, SlowLog: "/var/log/mysql/monopanel-slow.log", NativePassword: true}, []string{"innodb_buffer_pool_size = 1024M", "mysql_native_password = ON"}},
		{"firewall.nft", "nftables/monopanel.nft.tmpl", Firewall{Policy: "drop", Allow: []string{"tcp dport { 22, 80, 443, 8443 } accept"}, Deny: []string{"ip saddr 203.0.113.7 drop"}}, []string{"policy drop;", "tcp dport { 22, 80, 443, 8443 } accept", "ip saddr 203.0.113.7 drop"}},
		{"fail2ban-jail.local", "fail2ban/jail.local.tmpl", Fail2ban{BanTime: "1h", FindTime: "10m", MaxRetry: 5, PanelPort: "8443", Nginx: true}, []string{"[monopanel]", "port = 8443", "enabled = true"}},
		{"crontab", "cron/crontab.tmpl", Crontab{Login: "alex", BinDir: "/var/www/alex/data/bin", Jobs: []CronLine{{Schedule: "*/5 * * * *", Command: "php cron.php", Enabled: true, Comment: "wp"}, {Schedule: "@daily", Command: "echo x", Enabled: false}}}, []string{"*/5 * * * * php cron.php  # wp", "#DISABLED# @daily echo x"}},
		{"php-ini.conf", "php/monopanel.ini.tmpl", PHPIni{Version: "8.4", Timezone: "UTC", OpcacheMemory: 128}, []string{"opcache.memory_consumption = 128", "date.timezone = UTC"}},
		{"nginx-site-suspended.conf", "nginx/site-suspended.conf.tmpl", exampleSite("fpm"), []string{"return 503", "listen 203.0.113.10:443 ssl;"}},
		{"site-index.html", "site/index.html.tmpl", Welcome{Domain: "example.com"}, []string{"Скоро здесь будет сайт", "example.com", "prefers-reduced-motion"}},
		{"nginx-site-allow.conf", "nginx/site.conf.tmpl", func() Site {
			s := exampleSite("fpm")
			s.AllowFrom = []string{"203.0.113.0/24", "2001:db8::1"}
			return s
		}(), []string{"allow 203.0.113.0/24;", "allow 2001:db8::1;", "deny  all;"}},
		{"nginx-real-ip.conf", "nginx/real-ip.conf.tmpl", RealIP{Cloudflare: true, CloudflareRanges: CloudflareRanges}, []string{"set_real_ip_from 173.245.48.0/20;", "real_ip_header    CF-Connecting-IP;"}},
		{"systemd-app.service", "systemd/app.service.tmpl", AppUnit{Login: "alex", Name: "web", Description: "gunicorn", Command: "/var/www/alex/data/venv/bin/gunicorn --bind 127.0.0.1:5000 app:app", WorkDir: "/var/www/alex/data/www/app.example.com", EnvFile: "/var/www/alex/data/.env", Env: []string{"PORT=5000"}, Restart: "always"}, []string{"User=alex", "ExecStart=/var/www/alex/data/venv/bin/gunicorn --bind 127.0.0.1:5000 app:app", "EnvironmentFile=/var/www/alex/data/.env", "Environment=\"PORT=5000\"", "Restart=always"}},
		{"php-fpm-pool.conf", "php-fpm/pool.conf.tmpl", examplePool(), []string{"[example.com]", "listen.group = monopanel-web", "php_value[memory_limit] = 256M", "pm = ondemand"}},
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
