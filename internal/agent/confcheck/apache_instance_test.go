package confcheck

import (
	"strings"
	"testing"
)

// Every uid has an address of its own on the loopback network, never
// 127.0.0.1 (nginx's side, which mod_remoteip trusts) and never the
// loopback broadcast; a uid beyond the addresses has none.
func TestApacheInstanceAddr(t *testing.T) {
	for uid, want := range map[int]string{
		0: "127.100.0.0:81", 1000: "127.100.3.232:81", 1001: "127.100.3.233:81", 1500: "127.100.5.220:81",
		65535: "127.100.255.255:81", 65536: "127.101.0.0:81", 60001: "127.100.234.97:81", 10223614: "127.255.255.254:81",
	} {
		got, err := ApacheInstanceAddr(uid)
		if err != nil || got != want {
			t.Errorf("uid %d: %q %v, want %q", uid, got, err, want)
		}
	}
	for _, uid := range []int{-1, 10223615, 10223616, 1 << 30} {
		if got, err := ApacheInstanceAddr(uid); err == nil {
			t.Errorf("uid %d has no address, got %q", uid, got)
		}
	}
	seen := map[string]int{}
	for uid := 0; uid < 1<<17; uid += 7 {
		a, err := ApacheInstanceAddr(uid)
		if err != nil {
			t.Fatal(err)
		}
		if other, dup := seen[a]; dup {
			t.Fatalf("uids %d and %d share %s", other, uid, a)
		}
		seen[a] = uid
		if strings.HasPrefix(a, "127.0.0.1:") || !strings.HasSuffix(a, ":81") {
			t.Fatalf("uid %d: %s", uid, a)
		}
	}
}

// The unit and the check of an account's Apache name the login; nothing
// else, root included, reads as one.
func TestApacheInstanceNames(t *testing.T) {
	if l, ok := ApacheInstanceLoginOf("monopanel-apache@alex.service"); !ok || l != "alex" {
		t.Errorf("unit of alex: %q %v", l, ok)
	}
	if l, ok := ApacheInstanceCheckLogin(ApacheInstanceCheckArgv("shop-2")); !ok || l != "shop-2" {
		t.Errorf("check of shop-2: %q %v", l, ok)
	}
	for _, unit := range []string{"monopanel-apache@root.service", "monopanel-apache@.service", "monopanel-apache@alex", "monopanel-apache@alex.socket",
		"monopanel-apache@../x.service", "monopanel-apache@Alex.service", "monopanel-apache@a\\x2fb.service", "apache2@alex.service", "monopanel-apachex@alex.service"} {
		if l, ok := ApacheInstanceLoginOf(unit); ok {
			t.Errorf("%s read as the Apache of %q", unit, l)
		}
	}
	for _, argv := range [][]string{
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/root/httpd.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/alex/sites/x.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/../../../tmp/httpd.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/tmp/httpd.conf"},
		{"/usr/sbin/apache2", "-f", "/etc/apache2/monopanel/accounts/alex/httpd.conf"},
		{"/usr/sbin/apache2", "-t", "-f", "/etc/apache2/monopanel/accounts/alex/httpd.conf", "-C", "LoadModule x /tmp/x.so"},
		{"/usr/sbin/apache2ctl", "-t", "-f", "/etc/apache2/monopanel/accounts/alex/httpd.conf"},
	} {
		if l, ok := ApacheInstanceCheckLogin(argv); ok {
			t.Errorf("%v read as the check of %q", argv, l)
		}
	}
}

const (
	alexMain = "/etc/apache2/monopanel/accounts/alex/httpd.conf"
	alexSite = "/etc/apache2/monopanel/accounts/alex/sites/example.com.conf"
	alexCust = "/etc/apache2/monopanel/accounts/alex/sites/example.com.d/custom.conf"
)

// What decides who an account's Apache runs as, what its root parent binds,
// writes, loads and reads, is the httpd.conf's alone and has exactly the
// account's values; the rest of the checks hold as for all of /etc/apache2.
func TestApacheInstanceRefusals(t *testing.T) {
	runRefusals(t, []refusal{
		{"user of another account", alexMain, "User shop\n", []string{"User shop", "alex"}},
		{"user root", alexMain, "User root\n", []string{"User root"}},
		{"user www-data", alexMain, "User www-data\n", []string{"User www-data"}},
		{"numeric user", alexMain, "User #1001\n", []string{"User #1001"}},
		{"group of another account", alexMain, "Group shop\n", []string{"Group shop"}},
		{"group web", alexMain, "Group monopanel-web\n", []string{"Group monopanel-web"}},
		{"user in a site", alexSite, "User alex\n", []string{"only the httpd.conf"}},
		{"group in custom directives", alexCust, "Group alex\n", []string{"only the httpd.conf"}},
		{"listen of another account", alexMain, "Listen 127.100.3.234:81\n", []string{"127.100.3.234:81", "127.100.3.233:81"}},
		{"listen on the shared port", alexMain, "Listen 127.0.0.1:8080\n", []string{"127.0.0.1:8080"}},
		{"listen on every address", alexMain, "Listen 81\n", []string{"Listen 81"}},
		{"listen with a protocol", alexMain, "Listen 127.100.3.233:81 https\n", []string{"https"}},
		{"second listen in a site", alexSite, "Listen 127.100.3.233:8081\n", []string{"only the httpd.conf"}},
		{"pid file of the shared Apache", alexMain, "PidFile /run/apache2/apache2.pid\n", []string{"PidFile"}},
		{"pid file of another account", alexMain, "PidFile /run/monopanel-apache/shop/httpd.pid\n", []string{"/run/monopanel-apache/shop"}},
		{"pid file in a site", alexSite, "PidFile /run/monopanel-apache/alex/httpd.pid\n", []string{"only the httpd.conf"}},
		{"runtime directory elsewhere", alexMain, "DefaultRuntimeDir /tmp\n", []string{"DefaultRuntimeDir"}},
		{"runtime directory in custom directives", alexCust, "DefaultRuntimeDir /run/monopanel-apache/alex\n", []string{"only the httpd.conf"}},
		{"mutex elsewhere", alexMain, "Mutex file:/tmp default\n", []string{"Mutex"}},
		{"mutex of another account", alexMain, "Mutex file:/run/monopanel-apache/shop default\n", []string{"shop"}},
		{"mutex of one module", alexMain, "Mutex file:/run/monopanel-apache/alex rewrite-map\n", []string{"rewrite-map"}},
		{"server root elsewhere", alexMain, "ServerRoot /var/www/alex\n", []string{"ServerRoot"}},
		{"server root in a site", alexSite, "ServerRoot /etc/apache2\n", []string{"only the httpd.conf"}},
		{"module off the list", alexMain, "LoadModule cgid_module /usr/lib/apache2/modules/mod_cgid.so\n", []string{"LoadModule cgid_module"}},
		{"status", alexMain, "LoadModule status_module /usr/lib/apache2/modules/mod_status.so\n", []string{"status_module"}},
		{"lua", alexMain, "LoadModule lua_module /usr/lib/apache2/modules/mod_lua.so\n", []string{"lua_module"}},
		{"include module", alexMain, "LoadModule include_module /usr/lib/apache2/modules/mod_include.so\n", []string{"include_module"}},
		{"module from the home", alexMain, "LoadModule rewrite_module /var/www/alex/mod_rewrite.so\n", []string{"/var/www/alex/mod_rewrite.so"}},
		{"module under another name", alexMain, "LoadModule rewrite_module /usr/lib/apache2/modules/mod_cgid.so\n", []string{"mod_cgid.so"}},
		{"module through a dotdot", alexMain, "LoadModule rewrite_module /usr/lib/apache2/modules/../../../../tmp/mod_rewrite.so\n", nil},
		{"module in a site", alexSite, "LoadModule rewrite_module /usr/lib/apache2/modules/mod_rewrite.so\n", []string{"only the httpd.conf"}},
		{"load file", alexMain, "LoadFile /lib/x86_64-linux-gnu/libc.so.6\n", []string{"LoadFile"}},
		{"sites of another account", alexMain, "IncludeOptional /etc/apache2/monopanel/accounts/shop/sites/*.conf\n", []string{"accounts/shop"}},
		{"the shared sites", alexMain, "IncludeOptional /etc/apache2/monopanel/sites/*.conf\n", []string{"/etc/apache2/monopanel/sites/*.conf"}},
		{"ports.conf", alexMain, "Include /etc/apache2/ports.conf\n", []string{"ports.conf"}},
		{"relative include", alexMain, "IncludeOptional monopanel/accounts/alex/sites/*.conf\n", []string{"monopanel/accounts/alex/sites/*.conf"}},
		{"another site's directives", alexSite, "IncludeOptional /etc/apache2/monopanel/accounts/alex/sites/other.com.d/*.conf\n", []string{"other.com.d"}},
		{"a site includes the sites", alexSite, "IncludeOptional /etc/apache2/monopanel/accounts/alex/sites/*.conf\n", []string{"example.com.d/*.conf"}},
		{"custom directives include", alexCust, "Include /etc/apache2/monopanel/accounts/alex/sites/example.com.d/*.conf\n", []string{"include nothing"}},
		{"variable", alexMain, "ErrorLog ${APACHE_LOG_DIR}/error.log\n", []string{"variable", "envvars"}},
		{"undefine foreground", alexMain, "UnDefine FOREGROUND\n", []string{"UnDefine"}},
		{"define in custom directives", alexCust, "Define X LoadModule\n", []string{"Define"}},
		{"log of another account", alexSite, "CustomLog /var/log/monopanel/sites/shop/shop.example.com.access.log combined\n", []string{"sites/shop", "/var/log/monopanel/sites/alex"}},
		{"log of the distribution's Apache", alexCust, "ErrorLog /var/log/apache2/error.log\n", []string{"/var/log/apache2/error.log"}},
		{"relative log", alexMain, "ErrorLog logs/error_log\n", []string{"logs/error_log"}},
		{"log of the site tree", alexMain, "ErrorLog /var/log/monopanel/sites/error.log\n", []string{"/var/log/monopanel/sites/error.log"}},
		{"piped log", alexCust, "CustomLog \"|/bin/sh -c id\" combined\n", []string{"piped"}},
		{"dotdot log", alexCust, "ErrorLog /var/log/monopanel/sites/alex/../shop/x.log\n", nil},
		// What the checks refuse everywhere in /etc/apache2 stays refused.
		{"core dumps", alexMain, "CoreDumpDirectory /tmp\n", []string{"CoreDumpDirectory"}},
		{"scoreboard", alexMain, "ScoreBoardFile /tmp/sb\n", []string{"ScoreBoardFile"}},
		{"chroot", alexMain, "ChrootDir /var/www/alex\n", []string{"ChrootDir"}},
		{"rewrite program", alexCust, "RewriteMap x prg:/bin/sh\n", []string{"prg:"}},
		{"suexec", alexSite, "SuexecUserGroup root root\n", []string{"SuexecUserGroup"}},
		{"itk", alexCust, "AssignUserID root root\n", []string{"AssignUserID"}},
		{"ruid2", alexCust, "RUidGid root root\n", []string{"RUidGid"}},
		{"perl", alexCust, "PerlRequire /tmp/x.pl\n", []string{"PerlRequire"}},
		{"lua hook", alexCust, "LuaHookTranslateName /tmp/x.lua hook\n", []string{"LuaHookTranslateName"}},
		{"macro", alexMain, "<Macro X>\n</Macro>\n", []string{"Macro"}},
		{"exec passphrase", alexMain, "SSLPassPhraseDialog exec:/tmp/x\n", []string{"exec:"}},
		{"types of root's", alexCust, "TypesConfig /etc/shadow\n", []string{"TypesConfig"}},
		// The directory of an account that is no hosting account.
		{"root's Apache", "/etc/apache2/monopanel/accounts/root/httpd.conf", "ServerTokens Prod\n", []string{"root"}},
		{"system account's", "/etc/apache2/monopanel/accounts/www-data/httpd.conf", "ServerTokens Prod\n", []string{"www-data", "system account"}},
		{"the API's account", "/etc/apache2/monopanel/accounts/monopanel/httpd.conf", "ServerTokens Prod\n", []string{"monopanel"}},
		{"the cloud image's sudoer", "/etc/apache2/monopanel/accounts/ubuntu/httpd.conf", "ServerTokens Prod\n", []string{"ubuntu"}},
		{"a docker member", "/etc/apache2/monopanel/accounts/evil/sites/example.com.conf", "ServerName example.com\n", []string{"evil", "docker"}},
		{"a member of a group 0", "/etc/apache2/monopanel/accounts/zero/httpd.conf", "ServerTokens Prod\n", []string{"zero"}},
		{"an unknown account", "/etc/apache2/monopanel/accounts/ghost/httpd.conf", "ServerTokens Prod\n", []string{"ghost"}},
		{"a sudoer's README", "/etc/apache2/monopanel/accounts/sysop/sites/README", "x\n", []string{"sysop"}},
		// Files an account's Apache does not read.
		{"stray file", "/etc/apache2/monopanel/accounts/alex/extra.conf", "ServerTokens Prod\n", []string{"nothing else"}},
		{"deeper custom directives", "/etc/apache2/monopanel/accounts/alex/sites/example.com.d/x.d/y.conf", "Header set X 1\n", []string{"nothing else"}},
		{"file beside the directories", "/etc/apache2/monopanel/accounts/alex.conf", "ServerTokens Prod\n", []string{"nothing else"}},
		{"odd login", "/etc/apache2/monopanel/accounts/Alex/httpd.conf", "ServerTokens Prod\n", []string{"nothing else"}},
		{"a README with directives", "/etc/apache2/monopanel/accounts/alex/sites/README", "LoadModule rewrite_module /usr/lib/apache2/modules/mod_rewrite.so\n", []string{"only the httpd.conf"}},
		{"a directory named .conf", "/etc/apache2/monopanel/accounts/alex/sites/evil.conf/README", "x\n", []string{"*.conf"}},
	})
}

// A logrotate stanza reopens its own account's Apache, no other.
func TestLogrotateReopensTheAccountsApache(t *testing.T) {
	own := lrStanza + "\tpostrotate\n" + lrNginxUSR + "\t\t[ ! -s /run/monopanel-apache/alex/httpd.pid ] || kill -USR1 \"$(cat /run/monopanel-apache/alex/httpd.pid)\" 2>/dev/null || true\n\tendscript\n}\n"
	pass(t, lr, own)
	refuse(t, lr, strings.ReplaceAll(own, "monopanel-apache/alex/", "monopanel-apache/shop/"), "monopanel-apache/shop")
	refuse(t, lr, strings.ReplaceAll(own, "monopanel-apache/alex/", "monopanel-apache/../alex/"), "monopanel-apache/../alex")
}
