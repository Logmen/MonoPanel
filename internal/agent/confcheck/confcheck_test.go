package confcheck

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The accounts of the fixture host (testdata/etc): alex and shop are the
// panel's accounts, vmail its mail user, monopanel the API's user; ubuntu is
// the cloud image's sudoer, evil a panel-like account in the docker group,
// stray and deep ordinary accounts outside the panel's homes, sysop a
// system account in the sudo group, zero a panel-like account in wheel0, a
// second name of gid 0.
func TestMain(m *testing.M) {
	etcDir = "testdata/etc"
	os.Exit(m.Run())
}

func pass(t *testing.T, path, content string) {
	t.Helper()
	if err := Check(path, []byte(content)); err != nil {
		t.Errorf("%s: refused, want it to pass: %v\n--- content\n%s", path, err, content)
	}
}

// refuse wants Check to refuse content at path with a message holding every
// one of want.
func refuse(t *testing.T, path, content string, want ...string) {
	t.Helper()
	err := Check(path, []byte(content))
	if err == nil {
		t.Errorf("%s: passed, want a refusal\n--- content\n%s", path, content)
		return
	}
	if !strings.Contains(err.Error(), "is not allowed") && !errors.Is(err, errUnreadable) {
		t.Errorf("%s: %v: the refusal does not say what is not allowed", path, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s: %v: want %q in the message", path, err, w)
		}
	}
}

type refusal struct {
	name    string
	path    string
	content string
	want    []string
}

func runRefusals(t *testing.T, cases []refusal) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refuse(t, c.path, c.content, c.want...)
		})
	}
}

type acceptance struct {
	name    string
	path    string
	content string
}

func runAcceptances(t *testing.T, cases []acceptance) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pass(t, c.path, c.content)
		})
	}
}

// Paths without rules pass, whatever they hold: the agent's own tests write
// into temporary directories, and units are the units RPC's business.
func TestPathsWithoutRulesPass(t *testing.T) {
	for _, p := range []string{"/tmp/agent-test/sites/a.conf", "/home/alex/.cache/x/y.conf", "relative/path", "/etc/systemd/system/monopanel-app-alex-web.service"} {
		if err := Check(p, []byte("load_module /tmp/x.so;\nExecStart=+/bin/sh\n")); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// The path is cleaned first: a ".." or a doubled slash does not lead out of
// a family's rules.
func TestPathIsCleaned(t *testing.T) {
	refuse(t, "/etc/nginx/monopanel/../nginx.conf", "user root;\n", "user root")
	refuse(t, "/etc//nginx/monopanel/http.d/x.conf", "load_module /tmp/x.so;\n", "load_module")
	refuse(t, "/tmp/../etc/cron.d/x", "* * * * * root id\n", "/etc/cron.d/x")
}

func TestNeverPlaces(t *testing.T) {
	for _, p := range []string{
		"/etc/cron.d/monopanel", "/etc/crontab", "/var/spool/cron/crontabs/root", "/etc/sudoers.d/90-x", "/etc/sudoers",
		"/etc/ld.so.preload", "/etc/ld.so.conf.d/x.conf", "/etc/profile.d/x.sh", "/etc/pam.d/sshd", "/etc/security/limits.conf",
		"/etc/passwd", "/etc/shadow", "/etc/apt/apt.conf.d/99x", "/etc/apt/sources.list", "/etc/dnf/dnf.conf", "/etc/yum.conf",
		"/etc/pki/rpm-gpg/RPM-GPG-KEY-x", "/usr/share/keyrings/x.gpg", "/opt/monopanel/sphinx/bin/searchd",
		"/var/log/mysql/x", "/var/lib/sphinx/bitrix.spa", "/var/lib/sphinxsearch/data/x", "/var/log/sphinx/searchd.log",
		"/var/mail/monopanel/example.com/a/cur/x", "/var/lib/monopanel/panel.db", "/var/lib/monopanel-valkey/alex-cache/dump.rdb",
		"/etc/init.d/x", "/etc/rc.local", "/etc/udev/rules.d/x.rules", "/etc/modprobe.d/x.conf",
	} {
		refuse(t, p, "x\n", p)
	}
}

// Configure moves the trees the rules name; empty fields keep the defaults.
func TestConfigure(t *testing.T) {
	t.Cleanup(func() { Configure(Layout{}) })
	conf := "access_log /srv/logs/sites/alex/example.com.access.log main;\n"
	refuse(t, "/etc/nginx/monopanel/sites/example.com.conf", conf, "/srv/logs/sites/alex")
	Configure(Layout{LogDir: "/srv/logs/"})
	pass(t, "/etc/nginx/monopanel/sites/example.com.conf", conf)
	if l := layout(); l.LogDir != "/srv/logs" || l.RunDir != "/run/monopanel" || l.WWWRoot != "/var/www" || l.ServiceUser != "monopanel" {
		t.Errorf("layout after Configure: %+v", l)
	}
	refuse(t, "/etc/nginx/monopanel/sites/example.com.conf", "access_log /var/log/monopanel/sites/alex/x.log;\n", "/var/log/monopanel/sites/alex/x.log")
	Configure(Layout{})
	pass(t, "/etc/nginx/monopanel/sites/example.com.conf", "access_log /var/log/monopanel/sites/alex/x.log;\n")
}

// Text configurations hold neither NUL nor carriage returns: some daemons
// break lines at a CR where the check would not.
func TestControlCharacters(t *testing.T) {
	refuse(t, "/etc/postfix/main.cf", "myhostname = a\rdaemon_directory = /tmp\n", "carriage return")
	refuse(t, "/etc/php/8.4/cli/conf.d/99-monopanel.ini", "memory_limit = 1G\rauto_prepend_file = /tmp/x.php\n", "carriage return")
	refuse(t, "/etc/apache2/monopanel/httpd.conf", "ServerTokens Prod \\\r\nLoadModule x /tmp/x.so\n", "carriage return")
	refuse(t, "/etc/nginx/monopanel/http.d/x.conf", "gzip on;\x00", "NUL")
}

// A file the check cannot read the way the daemon does is refused, and the
// refusal says where.
func TestUnreadableIsRefused(t *testing.T) {
	for _, c := range []struct{ path, content string }{
		{"/etc/nginx/monopanel/http.d/x.conf", "server {\n listen 80;\n"},
		{"/etc/nginx/monopanel/http.d/x.conf", "gzip on;\n}\n"},
		{"/etc/nginx/monopanel/http.d/x.conf", "return 200 \"unterminated;\n"},
		{"/etc/dovecot/dovecot.conf", "service imap {\n"},
		{"/etc/postfix/master.cf", "smtp inet n - n\n"},
		{"/etc/php/8.4/fpm/pool.d/x.conf", "[x\nuser = alex\n"},
		{"/etc/mysql/conf.d/x.cnf", "datadir2 = 1\n"},
	} {
		err := Check(c.path, []byte(c.content))
		if err == nil || !strings.Contains(err.Error(), "line ") {
			t.Errorf("%s %q: %v, want a refusal naming the line", c.path, c.content, err)
		}
	}
}

// What the API writes keeps its owners and modes; a file given to another
// account, or writable by group or others, would change after the check.
func TestCheckOwnership(t *testing.T) {
	for _, c := range []struct {
		path, owner, group string
		mode               uint32
	}{
		{"/etc/nginx/monopanel/sites/example.com.conf", "", "", 0o644},
		{"/etc/nginx/monopanel/sites/example.com.conf", "", "", 0},
		{"/etc/monopanel/apps/monopanel-app-alex-web.env", "root", "root", 0o600},
		{"/etc/opendkim/keys/example.com/mail.private", "opendkim", "opendkim", 0o600},
		{"/etc/dovecot/monopanel/users", "root", "dovecot", 0o640},
		{"/usr/local/bin/composer", "", "", 0o755},
		{"/usr/local/lib/monopanel/composer.phar", "", "", 0o755},
	} {
		if err := CheckOwnership(c.path, c.owner, c.group, c.mode); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []struct {
		path, owner, group string
		mode               uint32
	}{
		{"/etc/nginx/monopanel/sites/example.com.conf", "monopanel", "", 0o644},
		{"/etc/nginx/monopanel/sites/example.com.conf", "", "monopanel", 0o644},
		{"/etc/nginx/monopanel/sites/example.com.conf", "", "", 0o666},
		{"/etc/nginx/monopanel/sites/example.com.conf", "", "", 0o664},
		{"/usr/local/bin/composer", "", "", 0o4755},
		{"/etc/opendkim/KeyTable", "opendkim", "", 0o644},
		{"/etc/dovecot/monopanel/users", "dovecot", "dovecot", 0o640},
		{"/etc/opendkim/keys/../../nginx/nginx.conf", "opendkim", "", 0o644},
	} {
		if err := CheckOwnership(c.path, c.owner, c.group, c.mode); err == nil {
			t.Errorf("%+v: passed", c)
		}
	}
}

// Root's group is gid 0, whatever name /etc/group gives it.
func TestGidZeroUnderAnotherName(t *testing.T) {
	refuse(t, "/etc/php/8.4/fpm/pool.d/x.conf", "[x]\nuser = zero\ngroup = zero\n", "zero", "group 0")
	refuse(t, "/etc/postfix/master.cf", "x unix - n n - - pipe user=vmail:wheel0 argv=/bin/id\n", "wheel0", "gid 0")
	refuse(t, "/etc/dovecot/dovecot.conf", "first_valid_uid = 990\nlast_valid_uid = 990\nmail_access_groups = wheel0\n", "wheel0")
}

// Control characters other than tabs and line feeds are refused in every
// text family, and the refusal names the line.
func TestOddControlsAreRefused(t *testing.T) {
	for _, p := range []string{"/etc/postfix/main.cf", "/etc/dovecot/dovecot.conf", "/etc/logrotate.d/x", "/etc/ssh/sshd_config.d/x.conf", "/etc/mysql/conf.d/x.cnf", "/etc/opendkim.conf", "/etc/php/8.4/cli/conf.d/x.ini"} {
		refuse(t, p, "# x\n\x1b[31m\n", "control character", "line 2")
	}
}
