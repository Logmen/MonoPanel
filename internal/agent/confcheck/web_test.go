package confcheck

import (
	"strings"
	"testing"
)

const (
	nginxMain = "/etc/nginx/nginx.conf"
	nginxSite = "/etc/nginx/monopanel/sites/example.com.conf"
	nginxCust = "/etc/nginx/monopanel/sites/example.com.d/custom.conf"
	nginxHTTP = "/etc/nginx/monopanel/http.d/50-x.conf"
)

func TestNginxRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"load_module", nginxMain, "user nginx;\nload_module /tmp/evil.so;\nevents {}\n", []string{"load_module", "line 2"}},
		{"load_module in a site", nginxSite, "load_module modules/x.so;\n", []string{"load_module"}},
		{"quoted directive name", nginxHTTP, "\"load_module\" /tmp/evil.so;\n", []string{"load_module"}},
		{"after a mid-word hash", nginxCust, "return 200 a#b; load_module /tmp/x.so;\n", []string{"load_module"}},
		{"after a comment", nginxCust, "gzip on; # load_module is fine in a comment\nload_module /tmp/x.so;\n", []string{"load_module", "line 2"}},
		{"env", nginxMain, "env LD_PRELOAD=/tmp/x.so;\n", []string{"env"}},
		{"ssl_engine", nginxMain, "ssl_engine pkcs11;\n", []string{"ssl_engine"}},
		{"perl", nginxHTTP, "perl_modules /tmp;\nperl_require evil.pm;\n", []string{"perl_modules"}},
		{"njs", nginxHTTP, "js_import /tmp/evil.js;\n", []string{"js_import"}},
		{"lua block", nginxHTTP, "init_by_lua_block { ngx.log(1); }\n", []string{"init_by_lua_block"}},
		{"lua block nginx cannot read", nginxHTTP, "content_by_lua_block { os.execute('id') }\n", nil},
		{"lua path", nginxHTTP, "lua_package_path '/tmp/?.lua';\n", []string{"lua_package_path"}},
		{"mruby", nginxHTTP, "mruby_init /tmp/x.rb;\n", []string{"mruby_init"}},
		{"master off", nginxMain, "master_process off;\n", []string{"master_process"}},
		{"user root", nginxMain, "user root;\n", []string{"user root"}},
		{"user group root", nginxMain, "user nginx root;\n", []string{"user nginx root"}},
		{"user of the API", nginxMain, "user monopanel;\n", []string{"user monopanel"}},
		{"user outside nginx.conf", nginxHTTP, "user nginx;\n", []string{"nginx.conf alone"}},
		{"include shadow", nginxCust, "include /etc/shadow;\n", []string{"/etc/shadow"}},
		{"include outside", nginxCust, "include /var/lib/monopanel/evil.conf;\n", []string{"/var/lib/monopanel/evil.conf"}},
		{"include dotdot", nginxCust, "include ../../var/lib/monopanel/x.conf;\n", []string{".."}},
		{"include migrated", nginxCust, "include monopanel/sites/example.com.d/custom.conf.migrated;\n", []string{"custom.conf.migrated"}},
		{"include any name", nginxCust, "include monopanel/sites/example.com.d/*;\n", []string{"*.conf"}},
		{"include README", nginxCust, "include /etc/nginx/monopanel/http.d/README;\n", []string{"README"}},
		{"include in a map", nginxHTTP, "map $a $b {\n    include /etc/shadow;\n}\n", []string{"/etc/shadow", "line 2"}},
		{"include in types", nginxHTTP, "types { include /tmp/x.conf; }\n", []string{"/tmp/x.conf"}},
		{"error_log cron", nginxCust, "error_log /etc/cron.d/x;\n", []string{"/etc/cron.d/x"}},
		{"error_log panel dir", nginxHTTP, "error_log /var/log/monopanel/x.log;\n", []string{"/var/log/monopanel/x.log"}},
		{"access_log home", nginxCust, "access_log /var/www/alex/data/logs/access.log main;\n", []string{"/var/www/alex/data/logs/access.log"}},
		{"access_log relative", nginxCust, "access_log logs/access.log;\n", []string{"logs/access.log"}},
		{"access_log dotdot", nginxCust, "access_log /var/log/nginx/../../../etc/cron.d/x main;\n", []string{"/etc/cron.d/x"}},
		{"access_log newline", nginxCust, "access_log \"/var/log/nginx/x\\n* * * * * root id\";\n", nil},
		{"deep access_log", nginxSite, "server { location / { if ($a) { access_log /root/.ssh/authorized_keys main; } } }\n", []string{"/root/.ssh/authorized_keys"}},
		{"stream access_log", nginxHTTP, "stream { server { listen 1; access_log /etc/ld.so.preload x; } }\n", []string{"/etc/ld.so.preload"}},
		{"ssl_key_log", nginxHTTP, "ssl_key_log /tmp/keys;\n", []string{"/tmp/keys"}},
		{"pid elsewhere", nginxMain, "pid /var/lib/monopanel/nginx.pid;\n", []string{"/var/lib/monopanel/nginx.pid"}},
		{"pid in a run subdir", nginxMain, "pid /run/monopanel/nginx.pid;\n", []string{"/run/monopanel/nginx.pid"}},
		{"lock_file", nginxMain, "lock_file /etc/passwd;\n", []string{"/etc/passwd"}},
		{"temp path", nginxCust, "client_body_temp_path /etc/cron.d;\n", []string{"/etc/cron.d"}},
		{"temp path in a home", nginxCust, "fastcgi_temp_path /var/www/alex/data/tmp 1 2;\n", []string{"/var/www/alex/data/tmp"}},
		{"cache path", nginxHTTP, "proxy_cache_path /tmp/cache levels=1:2 keys_zone=x:10m;\n", []string{"/tmp/cache"}},
		{"engine key", nginxSite, "ssl_certificate_key engine:pkcs11:/tmp/evil;\n", []string{"engine:"}},
		{"proxy engine key", nginxCust, "proxy_ssl_certificate_key \"engine:dynamic:x\";\n", []string{"engine:"}},
		{"xslt of shadow", nginxCust, "xslt_stylesheet /etc/shadow;\n", []string{"/etc/shadow"}},
		{"xml entities", nginxCust, "xml_entities /root/.ssh/id_rsa;\n", []string{"/root/.ssh/id_rsa"}},
		{"unterminated", nginxCust, "location / {\n", nil},
		{"stray brace", nginxCust, "}\n", nil},
		// ngx_create_paths chowns the path to the worker user, following
		// symlinks; below /var/cache/nginx the directories are the worker's.
		{"temp path below a worker's directory", nginxCust, "proxy_temp_path /var/cache/nginx/proxy_temp/evil;\n", []string{"/var/cache/nginx/proxy_temp/evil", "right in /var/cache/nginx"}},
		{"temp path in /var/lib/nginx", nginxCust, "client_body_temp_path /var/lib/nginx/body;\n", []string{"/var/lib/nginx/body"}},
		{"cache path below a worker's directory", nginxHTTP, "fastcgi_cache_path /var/cache/nginx/fastcgi_temp/x levels=1:2 keys_zone=x:1m;\n", []string{"fastcgi_cache_path"}},
		{"unix listen in the API's directory", nginxSite, "server { listen unix:/run/monopanel/x.sock; }\n", []string{"unix:/run/monopanel/x.sock"}},
		{"unix listen in a home", nginxSite, "server { listen unix:/var/www/alex/x.sock; }\n", []string{"unix:/var/www/alex/x.sock"}},
		{"directory named .conf", "/etc/nginx/monopanel/sites/evil.conf/README", "x\n", []string{"*.conf"}},
		// FastCGI goes to the sites' pools alone: the distributions' pools run
		// as www-data or apache.
		{"fastcgi to Debian's pool", nginxCust, "location ~ \\.php$ { fastcgi_pass unix:/run/php/php8.4-fpm.sock; }\n", []string{"unix:/run/php/php8.4-fpm.sock", "/run/monopanel/php/<domain>.sock"}},
		{"fastcgi to Debian's default socket", nginxSite, "server { location / { fastcgi_pass unix:/run/php/php-fpm.sock; } }\n", []string{"/run/php/php-fpm.sock"}},
		{"fastcgi to Remi's pool", nginxCust, "fastcgi_pass unix:/var/opt/remi/php84/run/php-fpm/www.sock;\n", []string{"www.sock"}},
		{"fastcgi to AppStream's pool", nginxCust, "fastcgi_pass unix:/run/php-fpm/www.sock;\n", []string{"/run/php-fpm/www.sock"}},
		{"fastcgi over TCP", nginxCust, "fastcgi_pass 127.0.0.1:9000;\n", []string{"127.0.0.1:9000"}},
		{"fastcgi to a port alone", nginxCust, "fastcgi_pass 9000;\n", []string{"9000"}},
		{"fastcgi over IPv6", nginxCust, "fastcgi_pass [::1]:9000;\n", []string{"[::1]:9000"}},
		{"fastcgi to an upstream", nginxCust, "fastcgi_pass php84-php-fpm;\n", []string{"php84-php-fpm"}},
		{"fastcgi to a variable", nginxCust, "fastcgi_pass $backend;\n", []string{"$backend"}},
		{"fastcgi to a variable socket", nginxCust, "fastcgi_pass unix:/run/monopanel/php/$host.sock;\n", []string{"$host"}},
		{"fastcgi to a braced variable", nginxCust, "fastcgi_pass \"unix:/run/monopanel/php/${host}.sock\";\n", []string{"${host}"}},
		{"fastcgi walking up", nginxCust, "fastcgi_pass unix:/run/monopanel/php/../../php/php8.4-fpm.sock;\n", []string{".."}},
		{"fastcgi with a doubled slash", nginxCust, "fastcgi_pass unix:/run/monopanel//php/example.com.sock;\n", []string{"//php"}},
		{"fastcgi below the socket directory", nginxCust, "fastcgi_pass unix:/run/monopanel/php/x/example.com.sock;\n", []string{"/php/x/"}},
		{"fastcgi to the agent", nginxCust, "fastcgi_pass unix:/run/monopanel/agent.sock;\n", []string{"agent.sock"}},
		{"fastcgi to an idle pool", nginxCust, "fastcgi_pass unix:/run/monopanel/php/.idle-8.4.sock;\n", []string{".idle-8.4.sock"}},
		{"fastcgi to a socket of no domain", nginxCust, "fastcgi_pass unix:/run/monopanel/php/localhost.sock;\n", []string{"localhost.sock"}},
		{"fastcgi to an upper-case socket", nginxCust, "fastcgi_pass unix:/run/monopanel/php/Example.com.sock;\n", []string{"Example.com.sock"}},
		{"fastcgi in upper case", nginxCust, "fastcgi_pass UNIX:/run/php/php8.4-fpm.sock;\n", []string{"UNIX:"}},
		{"fastcgi with a path after the socket", nginxCust, "fastcgi_pass unix:/run/monopanel/php/example.com.sock:/x;\n", []string{"sock:/x"}},
		{"fastcgi twice in one", nginxCust, "fastcgi_pass unix:/run/monopanel/php/example.com.sock unix:/run/php/php8.4-fpm.sock;\n", []string{"php8.4-fpm.sock"}},
		{"fastcgi without a target", nginxCust, "fastcgi_pass;\n", []string{"fastcgi_pass"}},
		{"fastcgi deep in a block", nginxSite, "server { location / { if ($a) { fastcgi_pass 10.0.0.1:9000; } } }\n", []string{"10.0.0.1:9000", "line 1"}},
		// php-fpm takes these as ini lines for the worker: extension= loads
		// any library into the pool.
		{"PHP_VALUE", nginxCust, "fastcgi_param PHP_VALUE \"upload_max_filesize=100M\";\n", []string{"PHP_VALUE"}},
		{"PHP_ADMIN_VALUE", nginxSite, "server { location ~ \\.php$ { fastcgi_param PHP_ADMIN_VALUE \"extension=/var/www/alex/evil.so\"; } }\n", []string{"PHP_ADMIN_VALUE"}},
		{"PHP_VALUE quoted", nginxCust, "fastcgi_param \"PHP_VALUE\" \"auto_prepend_file=/var/www/alex/x.php\";\n", []string{"PHP_VALUE"}},
		{"PHP_VALUE in lower case", nginxCust, "fastcgi_param php_admin_value x;\n", []string{"php_admin_value"}},
		// An upstream named like a path would stand in for a socket.
		{"upstream named like a socket", nginxHTTP, "upstream /run/monopanel/php/example.com.sock { server unix:/run/php/php8.4-fpm.sock; }\n", []string{"/run/monopanel/php/example.com.sock"}},
		// Other ways to hand a FastCGI server bytes of the client's: the
		// method nginx opens the upstream request with, and the stream
		// module's raw relay to a unix socket.
		{"method from the body", nginxCust, "location /x { proxy_method $request_body; proxy_pass http://unix:/run/php/php8.4-fpm.sock:/; }\n", []string{"proxy_method", "$request_body"}},
		{"method from a header", nginxCust, "proxy_method \"${http_x}\";\n", []string{"${http_x}"}},
		{"stream relay to a socket", nginxMain, "user nginx;\nstream {\n    server {\n        listen 127.0.0.1:19000;\n        proxy_pass unix:/var/opt/remi/php84/run/php-fpm/www.sock;\n    }\n}\n", []string{"unix:/var/opt/remi/php84/run/php-fpm/www.sock", "line 5"}},
		{"stream relay in an included file", nginxHTTP, "server { listen 127.0.0.1:19000; proxy_pass UNIX:/run/php/php8.4-fpm.sock; }\n", []string{"UNIX:"}},
		{"stream relay to a variable", nginxMain, "stream { map $remote_addr $to { default unix:/run/php/php8.4-fpm.sock; } server { listen 127.0.0.1:19000; proxy_pass $to; } }\n", []string{"$to"}},
		// An upstream stands in for proxy_pass's target: in a stream block a
		// unix server of one is the same relay.
		{"stream relay through an upstream", nginxMain, "user nginx;\nstream {\n    upstream fpm { server 127.0.0.1:9; server unix:/run/php-fpm/www.sock backup; }\n    server { listen 127.0.0.1:19000; proxy_pass fpm; }\n}\n", []string{"unix:/run/php-fpm/www.sock", "stream", "line 3"}},
		{"stream relay through an upper-case upstream", nginxMain, "stream { upstream fpm { server UNIX:/run/php-fpm/www.sock; } server { listen 127.0.0.1:19000; proxy_pass fpm; } }\n", []string{"UNIX:/run/php-fpm/www.sock"}},
		// Whatever a stream block includes would be checked as HTTP's.
		{"include in a stream block", nginxMain, "stream {\n    include /etc/nginx/monopanel/http.d/*.conf;\n}\n", []string{"include", "stream", "line 2"}},
		{"include in a stream server", nginxMain, "stream { server { listen 127.0.0.1:19000; include monopanel/snippets/x.conf; } }\n", []string{"stream"}},
		{"include in a stream map", nginxMain, "stream { map $remote_addr $to { include monopanel/snippets/map.conf; } }\n", []string{"stream"}},
		// The first bytes of a request to a FastCGI server: an empty method or
		// one of control bytes leaves them to the URI, a variable of the
		// target here (an address of the client's choosing with real_ip).
		{"empty method", nginxCust, "location /x { proxy_method \"\"; proxy_pass http://unix:/run/php-fpm/www.sock:$binary_remote_addr; }\n", []string{"proxy_method", "word of letters"}},
		{"method of control bytes", nginxCust, "proxy_method \"\x01\x01\";\n", []string{`\x01\x01`}},
		{"method of a digit", nginxCust, "proxy_method 1GET;\n", []string{"1GET"}},
		{"method of two words", nginxCust, "proxy_method GET POST;\n", []string{"GET POST"}},
		{"method with a blank", nginxCust, "proxy_method \"G T\";\n", []string{"G T"}},
		// uwsgi's first byte is the modifier, FastCGI's the version.
		{"uwsgi modifier of FastCGI's version", nginxCust, "location /x { uwsgi_pass unix:/run/php-fpm/www.sock; uwsgi_modifier1 1; uwsgi_pass_request_headers off; uwsgi_param \"\" $request_body; }\n", []string{"uwsgi_modifier1", "\"1\""}},
		{"uwsgi modifier to an upstream", nginxCust, "uwsgi_pass php-fpm; uwsgi_modifier1 14;\n", []string{"14"}},
		{"uwsgi modifier with a zero first", nginxCust, "uwsgi_modifier1 01;\n", []string{"01"}},
		{"uwsgi modifier empty", nginxCust, "uwsgi_modifier1 \"\";\n", []string{"uwsgi_modifier1"}},
		// EL's default.d: the distributions' locations pass PHP to their own
		// pools (php-fpm's php.conf: fastcgi_pass php-fpm).
		{"include of AppStream's PHP location", nginxSite, "server { root /var/www/x; include /etc/nginx/default.d/php.conf; }\n", []string{"/etc/nginx/default.d/php.conf", "default.d"}},
		{"include of Remi's PHP location", nginxCust, "include default.d/php84-php.conf;\n", []string{"default.d"}},
		{"include of every default location", nginxCust, "include /etc/nginx/default.d/*.conf;\n", []string{"default.d"}},
		{"include of default.d through a wildcard", nginxCust, "include /etc/nginx/d*/php.conf;\n", []string{"d*", "glob"}},
		// glob(3) takes a backslash for an escape and lets a wildcard in a
		// directory match "..": each of these reads the API's own file in
		// its data directory as root's configuration.
		{"include through escaped dots", nginxMain, "include /etc/nginx/\\.\\./\\.\\./var/lib/monopanel/e*.conf;\n", []string{"glob"}},
		{"include through wildcard directories", nginxMain, "include /etc/nginx/monopanel/.*/.*/.*/var/lib/monopanel/e*.conf;\n", []string{".*", "glob"}},
		{"include through bracket directories", nginxHTTP, "include monopanel/.[.]/.[.]/.[.]/var/lib/monopanel/x*.conf;\n", []string{"glob"}},
		{"include of an escaped name", nginxCust, "include /etc/nginx/monopanel/x\\*.conf;\n", []string{"glob"}},
	})
	// What the master binds right in /run is root's alone.
	pass(t, nginxSite, "server { listen unix:/run/nginx-example.sock; }\n")
	// A wildcard in the file name of an include, the distribution's
	// parameter files and http-level configuration stay; so do a method of
	// letters and uwsgi's own modifier.
	pass(t, nginxHTTP, "include /etc/nginx/conf.d/*.conf;\ninclude monopanel/sites/*.conf;\ninclude /etc/nginx/fastcgi_params;\ninclude /etc/nginx/fastcgi.conf;\n")
	pass(t, nginxCust, "location /dav/ { proxy_method PROPFIND; proxy_pass http://127.0.0.1:8080; }\nlocation /s/ { proxy_method M-SEARCH; proxy_pass http://127.0.0.1:1900; }\nlocation /py/ { include uwsgi_params; uwsgi_pass unix:/run/uwsgi/app.sock; uwsgi_modifier1 0; }\nlocation /py2/ { uwsgi_modifier1 00; uwsgi_pass 127.0.0.1:3031; }\n")
	pass(t, nginxMain, "user nginx;\nstream {\n    upstream db { server 10.0.0.5:3306; server db2.internal:3306 backup; }\n    server { listen 127.0.0.1:13306; proxy_pass db; }\n}\nhttp { upstream app { server unix:/run/app.sock; } include /etc/nginx/monopanel/http.d/*.conf; }\n")
	// A site's own pool, however the name is quoted; other parameters, and
	// upstreams for the proxy, stay as they were.
	pass(t, nginxCust, "location ~ \\.php$ { fastcgi_pass unix:/run/monopanel/php/shop.example.com.sock; }\nlocation /x/ { fastcgi_pass 'unix:/run/monopanel/php/xn--80ak6aa92e.com.sock'; fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name; fastcgi_param PHP_SELF $uri; }\n")
	pass(t, nginxHTTP, "upstream app { server 127.0.0.1:3000; server unix:/run/app.sock; keepalive 8; }\n")
	// Proxies of HTTP keep their targets, variables included, and a fixed
	// method; the stream module keeps TCP.
	pass(t, nginxCust, "location /a/ { proxy_pass http://unix:/run/app.sock:/; proxy_method POST; }\nlocation /b/ { proxy_pass http://$backend_host:3000; }\nlocation /c/ { proxy_pass https://203.0.113.7; }\n")
	pass(t, nginxMain, "user nginx;\nstream { upstream db { server 10.0.0.5:3306; } server { listen 127.0.0.1:13306; proxy_pass db; } server { listen 127.0.0.1:15432; proxy_pass 10.0.0.6:5432; } }\n")
}

// fastcgi_pass follows the run directory of config.yaml.
func TestNginxFastCGIFollowsTheRunDir(t *testing.T) {
	t.Cleanup(func() { Configure(Layout{}) })
	conf := "fastcgi_pass unix:/srv/run/php/example.com.sock;\n"
	refuse(t, nginxCust, conf, "/srv/run/php/example.com.sock")
	Configure(Layout{RunDir: "/srv/run"})
	pass(t, nginxCust, conf)
	refuse(t, nginxCust, "fastcgi_pass unix:/run/monopanel/php/example.com.sock;\n", "unix:/srv/run/php/<domain>.sock")
	// The idle pools follow it as well.
	p := "/etc/php/8.4/fpm/pool.d/www.conf"
	moved, _ := IdlePool("/srv/run", p)
	pass(t, p, moved)
	def, _ := IdlePool("/run/monopanel", p)
	refuse(t, p, def, "idle pool")
}

const (
	apacheMain = "/etc/apache2/monopanel/httpd.conf"
	apacheSite = "/etc/apache2/monopanel/sites/example.com.conf"
)

func TestApacheRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"LoadModule", apacheMain, "ServerTokens Prod\nLoadModule evil_module /tmp/evil.so\n", []string{"LoadModule", "line 2"}},
		{"lower case", apacheMain, "loadmodule evil_module /tmp/evil.so\n", []string{"loadmodule"}},
		{"quoted name", apacheMain, "\"LoadModule\" evil_module /tmp/evil.so\n", []string{"LoadModule"}},
		{"continued name", apacheMain, "Load\\\nModule evil_module /tmp/evil.so\n", []string{"LoadModule"}},
		{"indented in a section", apacheSite, "<VirtualHost 127.0.0.1:8080>\n    <IfModule mod_x.c>\n        LoadFile /tmp/libevil.so\n    </IfModule>\n</VirtualHost>\n", []string{"LoadFile", "line 3"}},
		{"variable directive", apacheMain, "${EVIL} evil_module /tmp/evil.so\n", []string{"${EVIL}"}},
		{"unterminated variable", apacheMain, "ErrorLog ${APACHE_LOG_DIR/x\n", []string{"${"}},
		{"redefined variable", apacheMain, "Define APACHE_LOG_DIR /etc/cron.d\n", []string{"APACHE_LOG_DIR"}},
		{"variable on EL", "/etc/httpd/monopanel/httpd.conf", "ErrorLog ${APACHE_LOG_DIR}/x.conf\n", []string{"envvars"}},
		{"piped error log", apacheMain, "ErrorLog \"|/bin/sh -c id\"\n", []string{"piped"}},
		{"piped custom log", apacheSite, "CustomLog \"||/tmp/logger\" combined\n", []string{"piped"}},
		{"log into cron", apacheSite, "TransferLog /etc/cron.d/x\n", []string{"/etc/cron.d/x"}},
		{"log into a home", apacheSite, "CustomLog /var/www/alex/data/logs/a.log combined\n", []string{"/var/www/alex/data/logs/a.log"}},
		{"log relative", apacheSite, "ErrorLog conf-enabled/evil.conf\n", []string{"conf-enabled/evil.conf"}},
		{"log dotdot", apacheSite, "ErrorLog /var/log/apache2/../../../etc/x\n", []string{"/var/log/apache2/../../../etc/x"}},
		{"global log", apacheMain, "GlobalLog \"|/tmp/x\" combined\n", []string{"piped"}},
		{"include outside", apacheSite, "Include /etc/shadow\n", []string{"/etc/shadow"}},
		{"include optional tmp", apacheSite, "IncludeOptional /tmp/*.conf\n", []string{"/tmp/*.conf"}},
		{"include a directory", apacheMain, "Include /etc/apache2/monopanel/\n", []string{"*.conf"}},
		{"include dotdot", apacheMain, "Include ../../tmp/x.conf\n", []string{".."}},
		{"include README", apacheMain, "IncludeOptional /etc/apache2/monopanel/sites/README\n", []string{"README"}},
		{"user root", apacheMain, "User root\n", []string{"User root"}},
		{"group zero", apacheMain, "Group #0\n", []string{"Group #0"}},
		{"rewrite program", apacheSite, "RewriteMap evil prg:/bin/sh\n", []string{"prg:"}},
		{"passphrase program", apacheMain, "SSLPassPhraseDialog exec:/tmp/x\n", []string{"exec:"}},
		{"random seed program", apacheMain, "SSLRandomSeed startup exec:/tmp/x 16\n", []string{"exec:"}},
		{"crypto device", apacheMain, "SSLCryptoDevice pkcs11\n", []string{"SSLCryptoDevice"}},
		{"session cache file", apacheMain, "SSLSessionCache shmcb:/etc/cron.d/x(512000)\n", []string{"shmcb:/etc/cron.d/x"}},
		{"pid file", apacheMain, "PidFile /etc/passwd\n", []string{"PidFile"}},
		{"scoreboard", apacheMain, "ScoreBoardFile /var/lib/monopanel/sb\n", []string{"ScoreBoardFile"}},
		{"mutex", apacheMain, "Mutex file:/tmp default\n", []string{"Mutex"}},
		{"server root", apacheMain, "ServerRoot /var/lib/monopanel\n", []string{"ServerRoot"}},
		{"script socket", apacheMain, "ScriptSock /etc/shadow\n", []string{"ScriptSock"}},
		{"macro", apacheMain, "<Macro Load $m>\n$m x /tmp/x.so\n</Macro>\nUse Load LoadModule\n", []string{"Macro"}},
		{"perl", apacheMain, "PerlRequire /tmp/x.pl\n", []string{"PerlRequire"}},
		{"perl section", apacheMain, "<Perl>\nsystem('id');\n</Perl>\n", []string{"<perl>"}},
		{"lua", apacheSite, "LuaHookTranslateName /tmp/x.lua hook\n", []string{"LuaHookTranslateName"}},
		{"wsgi", apacheSite, "WSGIDaemonProcess x user=root\n", []string{"WSGIDaemonProcess"}},
		{"itk", apacheSite, "AssignUserID root root\n", []string{"AssignUserID"}},
		{"ruid2", apacheSite, "RUidGid root root\n", []string{"RUidGid"}},
		{"suexec", apacheSite, "SuexecUserGroup ubuntu ubuntu\n", []string{"SuexecUserGroup"}},
		{"types file", apacheMain, "TypesConfig /etc/shadow\n", []string{"TypesConfig"}},
		{"envvars", "/etc/apache2/envvars", "export LD_PRELOAD=/tmp/x.so\n", []string{"envvars"}},
		{"load file name", "/etc/apache2/monopanel/evil.load", "LoadModule x /tmp/x.so\n", []string{"evil.load"}},
		// apr_isspace splits words at \v and \f: "LoadModule\vx" is LoadModule.
		{"vertical tab", apacheSite, "ServerName x\nLoadModule\vevil_module\v/var/www/alex/evil.so\n", []string{"'\\v'", "line 2"}},
		{"form feed", apacheMain, "LoadModule\fevil_module /var/www/alex/evil.so\n", []string{"'\\f'"}},
		// ap_build_config_sub drops the ">" that ends a directive's name.
		{"name ends with >", apacheMain, "LoadModule> evil_module /var/www/alex/evil.so\n", []string{"LoadModule"}},
		{"file name ends with >", apacheMain, "LoadFile> /var/www/alex/libevil.so\n", []string{"LoadFile"}},
		// IncludeOptional sites/*.conf matches a directory named *.conf and
		// reads every file in it, the README included.
		{"README in a .conf directory", "/etc/apache2/monopanel/sites/evil.conf/README", "LoadModule evil_module /var/www/alex/evil.so\n", []string{"*.conf"}},
		{"conf in a .conf directory", "/etc/httpd/monopanel/sites/evil.conf/x.conf", "ServerTokens Prod\n", []string{"*.conf"}},
		{"README content", "/etc/apache2/monopanel/sites/example.com.d/README", "LoadModule evil_module /var/www/alex/evil.so\n", []string{"LoadModule"}},
		{"migrated file", "/etc/apache2/monopanel/sites/example.com.d/custom.conf.migrated", "x\n", []string{"custom.conf.migrated"}},
		{"piped argument of another module", apacheSite, "SecAuditLog2 \"|/bin/sh -c id\"\n", []string{"piped"}},
		{"second audit log into cron", apacheSite, "SecAuditLog2 /etc/cron.d/x\n", []string{"/etc/cron.d/x"}},
		{"exec argument", apacheMain, "SSLPassPhraseDialog \"exec:/var/www/alex/x\"\n", []string{"exec:"}},
		{"ldap cache file", apacheMain, "LDAPSharedCacheFile /etc/passwd\n", []string{"LDAPSharedCacheFile"}},
		{"proxy list of root's secrets", apacheMain, "RemoteIPTrustedProxyList /etc/shadow\n", []string{"RemoteIPTrustedProxyList"}},
		{"internal proxy list", "/etc/httpd/monopanel/httpd.conf", "RemoteIPInternalProxyList /root/.ssh/id_rsa\n", []string{"RemoteIPInternalProxyList"}},
	})
}

const (
	phpCLI  = "/etc/php/8.4/cli/conf.d/99-monopanel.ini"
	phpFPM  = "/etc/php/8.4/fpm/conf.d/99-monopanel.ini"
	phpRemi = "/etc/opt/remi/php84/php.d/99-monopanel.ini"
	phpPool = "/etc/php/8.4/fpm/pool.d/example.com.conf"
	phpMain = "/etc/php/8.4/fpm/php-fpm.conf"
)

const goodPool = "[example.com]\nuser = alex\ngroup = alex\nlisten = /run/monopanel/php/example.com.sock\n"

// The distributions' own pool files.
const (
	debianWWW = "/etc/php/8.4/fpm/pool.d/www.conf"
	remiWWW   = "/etc/opt/remi/php84/php-fpm.d/www.conf"
)

// idle is the idle pool of the distribution's pool file p, in the default
// run directory.
func idle(t *testing.T, p string) string {
	t.Helper()
	content, ok := IdlePool("/run/monopanel", p)
	if !ok {
		t.Fatalf("no idle pool for %s", p)
	}
	return content
}

func TestPHPRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"extension path", phpFPM, "extension=/tmp/evil.so\n", []string{"/tmp/evil.so"}},
		{"zend_extension path", phpCLI, "zend_extension = ../../../tmp/evil.so\n", []string{"evil.so"}},
		{"extension upper case", phpRemi, "EXTENSION=/tmp/evil.so\n", []string{"/tmp/evil.so"}},
		{"extension quoted", phpFPM, "extension = \"/tmp/evil.so\"\n", []string{"/tmp/evil.so"}},
		{"extension joined", phpFPM, "extension = \"redis\" \"/../../tmp/evil.so\"\n", []string{"redis"}},
		{"extension variable", phpFPM, "extension = ${EVIL}\n", []string{"${EVIL}"}},
		{"extension variable in quotes", phpFPM, "extension = \"${EVIL}\"\n", []string{"${EVIL}"}},
		{"prepend", phpCLI, "memory_limit = 256M\nauto_prepend_file = /var/lib/monopanel/x.php\n", []string{"auto_prepend_file", "line 2"}},
		{"append in a path section", phpRemi, "[PATH=/]\nauto_append_file = /tmp/x.php\n", []string{"auto_append_file"}},
		{"extension dir", phpCLI, "extension_dir = /tmp\n", []string{"extension_dir"}},
		{"include path", phpCLI, "include_path = \".:/var/lib/monopanel\"\n", []string{"include_path"}},
		{"preload", phpFPM, "opcache.preload = /tmp/x.php\nopcache.preload_user = root\n", []string{"opcache.preload"}},
		{"file cache", phpRemi, "opcache.file_cache = /var/lib/monopanel/opcache\n", []string{"opcache.file_cache"}},
		{"ffi", phpCLI, "ffi.enable = On\n", []string{"ffi.enable"}},
		{"ffi preload", phpFPM, "ffi.preload = /tmp/x.h\n", []string{"ffi.preload"}},
		{"sendmail", phpCLI, "sendmail_path = /tmp/x\n", []string{"sendmail_path"}},
		{"mail log", phpCLI, "mail.log = /etc/cron.d/x\n", []string{"mail.log"}},
		{"error log", phpCLI, "error_log = /etc/cron.d/x\n", []string{"error_log"}},
		{"sessions", phpCLI, "session.save_path = /var/lib/monopanel\n", []string{"session.save_path"}},
		{"unserialize callback", phpCLI, "unserialize_callback_func = system\n", []string{"unserialize_callback_func"}},
		{"insecure rsh", phpCLI, "imap.enable_insecure_rsh = On\n", []string{"imap.enable_insecure_rsh"}},
		{"output handler", phpFPM, "output_handler = system\n", []string{"output_handler"}},
		{"phar cache", phpFPM, "phar.cache_list = /var/lib/monopanel/x.phar\n", []string{"phar.cache_list"}},
		{"xdebug log", phpCLI, "xdebug.log = /etc/cron.d/x\n", []string{"xdebug.log"}},
		{"pool root", phpPool, "[example.com]\nuser = root\ngroup = alex\n", []string{"= root", "root is root"}},
		{"pool sudoer", phpPool, "[example.com]\nuser = ubuntu\n", []string{"ubuntu", "not an account of the panel"}},
		{"pool system account", phpPool, "[example.com]\nuser = www-data\n", []string{"www-data", "system account"}},
		{"pool API user", phpPool, "[example.com]\nuser = monopanel\n", []string{"monopanel"}},
		{"pool docker member", phpPool, "[example.com]\nuser = evil\n", []string{"evil", "docker"}},
		{"pool outside the homes", phpPool, "[example.com]\nuser = stray\n", []string{"stray"}},
		{"pool home too deep", phpPool, "[example.com]\nuser = deep\n", []string{"deep"}},
		{"pool unknown", phpPool, "[example.com]\nuser = ghost\n", []string{"ghost"}},
		{"pool quoted variable", phpPool, "[example.com]\nuser = \"${USER}\"\n", []string{"${USER}"}},
		{"pool group root", phpPool, "[example.com]\nuser = alex\ngroup = root\n", []string{"root"}},
		{"pool group sudo", phpPool, "[example.com]\nuser = alex\ngroup = sudo\n", []string{"sudo"}},
		{"pool group of the API", phpPool, "[example.com]\nuser = alex\ngroup = monopanel\n", []string{"monopanel"}},
		{"pool socket elsewhere", phpPool, goodPool + "listen = /etc/cron.d/x\n", []string{"/etc/cron.d/x"}},
		{"pool socket dotdot", phpPool, goodPool + "listen = /run/monopanel/php/../../../etc/x\n", []string{"listen"}},
		{"pool socket pool variable", phpPool, goodPool + "listen = /run/monopanel/php/$pool.sock\n", []string{"$pool"}},
		{"pool status socket", phpPool, goodPool + "pm.status_listen = /var/lib/monopanel/s.sock\n", []string{"pm.status_listen"}},
		{"slowlog", phpPool, goodPool + "slowlog = /etc/cron.d/x\n", []string{"/etc/cron.d/x"}},
		{"access log home", phpPool, goodPool + "access.log = /var/www/alex/data/logs/fpm.log\n", []string{"access.log"}},
		{"slowlog joined", phpPool, goodPool + "slowlog = \"/var/log/monopanel/sites/alex/x\" \"/../../../../etc/x\"\n", []string{"slowlog"}},
		{"chroot", phpPool, goodPool + "chroot = /var/lib/monopanel\n", []string{"chroot"}},
		{"socket for everybody", phpPool, goodPool + "listen.mode = 0666\n", []string{"listen.mode"}},
		{"non-ascii key", phpPool, goodPool + "php_value\u0130[x] = 1\n", nil},
		{"prefix", phpPool, goodPool + "prefix = /tmp\n", []string{"prefix"}},
		{"include in a pool", phpPool, goodPool + "include = /var/lib/monopanel/evil.conf\n", []string{"include"}},
		{"pool extension", phpPool, goodPool + "php_admin_value[extension] = /tmp/evil.so\n", []string{"/tmp/evil.so"}},
		{"global in a pool file", phpPool, "[global]\nerror_log = /etc/cron.d/x\n" + goodPool, []string{"outside a pool"}},
		{"before the first pool", phpPool, "pid = /etc/passwd\n" + goodPool, []string{"outside a pool"}},
		{"main pid", phpMain, "[global]\npid = /var/lib/monopanel/fpm.pid\n", []string{"pid"}},
		{"main error log in the panel's dir", phpMain, "[global]\nerror_log = /var/log/monopanel/fpm.log\n", []string{"error_log"}},
		{"main include", phpMain, "[global]\ninclude = /var/lib/monopanel/*.conf\n", []string{"include"}},
		{"main unknown", phpMain, "[global]\nfoo = bar\n", []string{"foo"}},
		{"pear.conf", "/etc/opt/remi/php84/pear.conf", "#PEAR_Config 0.9\na:1:{s:7:\"php_dir\";s:4:\"/tmp\";}\n", nil},
		{"other file", "/etc/php/8.4/cli/php-cli.sh", "x\n", []string{"php-cli.sh"}},
		// EL's and Remi's php-fpm units name no configuration: fpm.config
		// would hand the root master a file of the API's.
		{"fpm config", phpRemi, "fpm.config = /var/www/alex/fpm.conf\n", []string{"fpm.config"}},
		{"cli pager", phpCLI, "cli.pager = /var/www/alex/x\n", []string{"cli.pager"}},
		{"cli prompt", phpCLI, "cli.prompt = \"`system('id')`\"\n", []string{"cli.prompt"}},
		{"xdebug trace name", phpCLI, "xdebug.trace_output_name = ../../../etc/cron.d/x\n", []string{"xdebug.trace_output_name"}},
		{"APM log file", phpFPM, "blackfire.log_file = /etc/cron.d/x\n", []string{"blackfire.log_file"}},
		{"tracer hook", phpRemi, "datadog.trace.request_init_hook = /var/www/alex/x.php\n", []string{"request_init_hook"}},
		{"agent daemon", phpCLI, "newrelic.daemon.pidfile = /etc/passwd\n", []string{"newrelic.daemon.pidfile"}},
		{"extension outside the module directory", phpRemi, "zend_extension = /opt/remi/php84/root/usr/lib64/php/modules/../../../../../../tmp/x.so\n", []string{"zend_extension"}},
		{"extension in another directory", phpFPM, "extension = /usr/lib/php/x/evil.so\n", []string{"extension"}},
		// Only the panel's socket directory: Debian's /run/php is www-data's.
		{"socket in the distribution's directory", phpPool, goodPool + "listen = /run/php/php8.4-fpm.sock\n", []string{"/run/php/php8.4-fpm.sock"}},
		{"socket named like a file", phpPool, "[example.com]\nuser = alex\ngroup = alex\nlisten = /run/monopanel/php/passwd\n", []string{"*.sock"}},
		{"slowlog in EL's apache-owned directory", "/etc/opt/remi/php84/php-fpm.d/example.com.conf", goodPool + "slowlog = /var/log/php-fpm/example.com.log\n", []string{"slowlog"}},
		{"access log in Remi's log directory", "/etc/opt/remi/php84/php-fpm.d/example.com.conf", goodPool + "access.log = /var/opt/remi/php84/log/php-fpm/x.log\n", []string{"access.log"}},
		{"pool group of the mail", phpPool, "[example.com]\nuser = alex\ngroup = vmail\n", []string{"group", "primary group of its user alex"}},
		{"pool group of the web server", phpPool, "[example.com]\ngroup = www-data\nuser = alex\n", []string{"www-data"}},
		{"pool group before its user", phpPool, "[example.com]\ngroup = shop\nuser = alex\n", []string{"shop"}},
		{"pool group without a user", phpPool, "[example.com]\ngroup = alex\n", []string{"no user"}},
		// The scanner eats the blanks around an offset; quotes make one PHP
		// builds itself.
		{"offset with blanks", phpPool, goodPool + "php_admin_value[ extension ] = /tmp/evil.so\n", []string{"/tmp/evil.so"}},
		{"quoted offset", phpPool, goodPool + "php_admin_value[\"extension\"] = /tmp/evil.so\n", nil},
		{"blank before the offset", phpPool, goodPool + "php_admin_value [extension] = /tmp/evil.so\n", []string{"/tmp/evil.so"}},
		// The distributions' pools hold the idle pool and nothing else.
		{"Debian's stock pool", debianWWW, "[www]\nuser = www-data\ngroup = www-data\nlisten = /run/php/php8.4-fpm.sock\nlisten.owner = www-data\nlisten.group = www-data\npm = dynamic\npm.max_children = 5\n", []string{debianWWW, "idle pool"}},
		{"Remi's stock pool", remiWWW, "[www]\nuser = apache\ngroup = apache\nlisten = /var/opt/remi/php84/run/php-fpm/www.sock\nlisten.acl_users = apache,nginx\npm = dynamic\npm.max_children = 50\nslowlog = /var/opt/remi/php84/log/php-fpm/www-slow.log\n", []string{"idle pool"}},
		{"a site's pool in the distribution's place", debianWWW, renderT(t, "php-fpm/pool.conf.tmpl", pool(nil)), []string{"idle pool"}},
		{"idle pool with a line more", debianWWW, idle(t, debianWWW) + "php_admin_value[extension] = redis\n", []string{"idle pool"}},
		{"idle pool for everybody", debianWWW, strings.Replace(idle(t, debianWWW), "listen.mode = 0600", "listen.mode = 0666", 1), []string{"idle pool"}},
		{"idle pool of another branch", debianWWW, idle(t, "/etc/php/8.3/fpm/pool.d/www.conf"), []string{"idle pool"}},
		{"Debian's idle pool on Remi", remiWWW, idle(t, debianWWW), []string{"idle pool"}},
		// Elsewhere the idle pool is a pool like any other: nobody is no
		// account of the panel.
		{"idle pool as a site's", phpPool, idle(t, debianWWW), []string{"nobody"}},
		// The root master's log stays out of apache's directories.
		{"main error log in EL's apache-owned directory", "/etc/opt/remi/php84/php-fpm.conf", "[global]\nerror_log = /var/log/php-fpm/error.log\n", []string{"error_log", "/var/log/php-fpm/error.log"}},
		{"main error log in Remi's log directory", "/etc/opt/remi/php84/php-fpm.conf", "include=/etc/opt/remi/php84/php-fpm.d/*.conf\n[global]\nerror_log = /var/opt/remi/php84/log/php-fpm/error.log\n", []string{"error_log", "web server's account"}},
		{"main error log below /var/log", phpMain, "[global]\nerror_log = /var/log/php/fpm.log\n", []string{"error_log"}},
		// php-fpm merges the sections of one name, case aside, across its
		// files: [www] elsewhere sets the idle pool of www.conf, which runs
		// as nobody, here on a socket nginx may open.
		{"the idle pool merged from a file of its own", phpPool, "[www]\nlisten = /run/monopanel/php/evil.example.com.sock\nlisten.owner = nginx\nlisten.mode = 0660\n", []string{"[www]", "idle pool", "line 2"}},
		{"the idle pool merged in upper case", "/etc/opt/remi/php84/php-fpm.d/zz.conf", "[WWW]\nlisten.acl_users = nginx\n", []string{"[WWW]"}},
		{"the idle pool merged under quotes", phpPool, "[\"www\"]\nlisten = 127.0.0.1:9000\n", []string{"plain word"}},
		{"the idle pool merged with a variable", phpPool, "[w${EMPTY}ww]\nlisten = 127.0.0.1:9000\n", []string{"plain word"}},
		{"the idle pool merged in php-fpm.conf", phpMain, "[global]\nerror_log = syslog\n[www]\nlisten.acl_groups = nginx\n", []string{"[www]"}},
		// php-fpm globs its includes: a wildcard in a directory matches "..".
		{"main include climbing out", phpMain, "[global]\ninclude = /etc/php/8.4/fpm/pool.d/.*/.*/.*/.*/.*/var/lib/monopanel/*.conf\n", []string{"include", "file name alone"}},
		{"main include of every pool directory", phpMain, "include=/etc/php/8.4/fpm/pool.d/*/*.conf\n", []string{"include"}},
	})
	// A site's pool with the name of a site whose first label is www is a
	// pool of its own.
	pass(t, phpPool, "[www.example.com]\nlisten.mode = 0660\n")
	// The distribution's own module directory holds what its packages put
	// there; a full path into it is as good as the bare name.
	pass(t, phpRemi, "zend_extension = /opt/remi/php84/root/usr/lib64/php/modules/opcache.so\n")
	pass(t, phpFPM, "zend_extension=/usr/lib/php/20240924/opcache.so\nopcache.lockfile_path = /tmp\nlog_errors = On\nsession.cookie_path = /\n")
}

const (
	mysqlConf = "/etc/mysql/conf.d/zz-monopanel.cnf"
	mysqlEL   = "/etc/my.cnf.d/zz-monopanel.cnf"
)

func TestMySQLRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"plugin load add", mysqlConf, "[mysqld]\nplugin-load-add=/tmp/evil.so\n", []string{"plugin-load-add"}},
		{"loose plugin load", mysqlEL, "[mysqld]\nloose_plugin_load = evil=/tmp/evil.so\n", []string{"loose_plugin_load"}},
		{"early plugin", mysqlEL, "[mysqld]\nearly-plugin-load=keyring_file.so\n", []string{"early-plugin-load"}},
		{"abbreviation", mysqlConf, "[mysqld]\nplugin=/tmp\n", []string{"plugin"}},
		{"plugin dir", mysqlConf, "[mysqld]\nplugin_dir = /var/lib/monopanel\n", []string{"plugin_dir"}},
		{"init file", mysqlConf, "[mysqld]\ninit-file=/var/lib/monopanel/x.sql\n", []string{"init-file"}},
		{"init connect", mysqlConf, "[mysqld]\ninit_connect = \"SET GLOBAL x=1\"\n", []string{"init_connect"}},
		{"malloc lib", mysqlConf, "[mysqld]\nmalloc-lib=/tmp/evil.so\n", []string{"malloc-lib"}},
		{"user root", mysqlConf, "[mysqld]\nuser = root\n", []string{"\"user\" = root"}},
		{"datadir", mysqlEL, "[mysqld]\ndatadir = /var/lib/monopanel/db\n", []string{"datadir"}},
		{"skip grant tables", mysqlConf, "[mysqld]\nskip-grant-tables\n", []string{"skip-grant-tables"}},
		{"secure file priv", mysqlConf, "[mysqld]\nsecure_file_priv = \"\"\n", []string{"secure_file_priv"}},
		{"slow log elsewhere", mysqlConf, "[mysqld]\nslow_query_log_file = /etc/cron.d/x\n", []string{"slow_query_log_file"}},
		{"client group", mysqlConf, "[client]\nplugin-dir=/tmp\ndefault-auth=evil\n", []string{"[client]"}},
		{"mysql group", mysqlConf, "[mysql]\npager = /bin/sh\n", []string{"[mysql]"}},
		{"mysqld_safe group", mysqlConf, "[mysqld_safe]\nledir = /tmp\n", []string{"[mysqld_safe]"}},
		{"include outside", mysqlConf, "!include /var/lib/monopanel/x.cnf\n", []string{"/var/lib/monopanel/x.cnf"}},
		{"includedir dotdot", "/etc/mysql/my.cnf", "!includedir /etc/mysql/../../tmp/\n", []string{".."}},
		{"unknown bang", mysqlConf, "!exec /tmp/x\n", []string{"!exec"}},
		{"option outside a group", mysqlConf, "plugin-load=x\n", nil},
		{"shell script", "/etc/mysql/debian-start", "[mysqld]\nport=$(touch /tmp/x)\n", []string{"debian-start"}},
		// my_isspace of latin1 takes 0xA0 for a blank: a lone byte in front
		// hides the name, or a [group], from strings.TrimSpace.
		{"no-break space before an option", mysqlConf, "[mysqld]\n\xa0plugin_dir = /var/www/alex/p\n", []string{"plugin_dir"}},
		{"no-break space before =", mysqlConf, "[mysqld]\nplugin_dir\xa0= /var/www/alex/p\n", []string{"plugin_dir"}},
		{"no-break space before a group", mysqlConf, "[mysqld]\n\xa0[mysqldump]\nresult-file = /etc/cron.d/x\n", []string{"[mysqldump]"}},
		{"no-break space in a group", mysqlConf, "[client\xa0]\nplugin-dir=/tmp\n", []string{"[client]"}},
		// fgets reads 4094 bytes at a time: the rest of a longer line is a
		// line of its own.
		{"long line", mysqlConf, "[mysqld]\n#" + strings.Repeat("x", 4093) + "plugin_dir=/var/www/alex/p\n", []string{"4094"}},
		{"galera provider", mysqlConf, "[mysqld]\nwsrep_provider = /var/www/alex/libgalera.so\n", []string{"wsrep_provider"}},
		{"galera notify", mysqlEL, "[mysqld]\nloose-wsrep-notify-cmd = /var/www/alex/x\n", []string{"wsrep"}},
		{"include with a tab", mysqlConf, "!include\t/var/lib/monopanel/x.cnf\n", []string{"/var/lib/monopanel/x.cnf"}},
		{"include after blanks", mysqlConf, "!  includedir /tmp/\n", []string{"/tmp"}},
	})
	// remove_end_comment cuts a "#" outside quotes before the name is read.
	pass(t, mysqlConf, "[mysqld]\nuser = mysql # the server's own\nmax_connections = 200 # tuned\n!includedir /etc/mysql/conf.d/\n")
}
