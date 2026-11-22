package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

// cmsFixture stubs the vendor downloads and gives the fake agent a client
// that answers the installers the way the real ones do.
func cmsFixture(t *testing.T, sources map[string][]byte) *siteFixture {
	t.Helper()
	f := newSiteFixture(t)
	if err := f.db.UpsertDBInstance(f.ctx, &store.DBInstance{Engine: "percona", Version: "8.4.11", Socket: "/var/run/mysqld/mysqld.sock", Service: "mysql.service", Status: store.DBReady}); err != nil {
		t.Fatal(err)
	}
	phar := []byte("<?php // wp-cli " + strings.Repeat("x", 2<<20))
	sum := sha512.Sum512(phar)
	prevF, prevB, prevT := cmsFetch, cmsFetchBytes, cmsLatestTag
	cmsFetch = func(_ context.Context, url string) (io.ReadCloser, error) {
		b, ok := sources[url]
		if !ok {
			return nil, fmt.Errorf("no test source for %s", url)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	cmsFetchBytes = func(_ context.Context, url string, _ int64) ([]byte, error) {
		switch url {
		case wpCLIURL:
			return phar, nil
		case wpCLIURL + ".sha512":
			return []byte(hex.EncodeToString(sum[:]) + "  wp-cli.phar\n"), nil
		}
		return nil, fmt.Errorf("no test source for %s", url)
	}
	cmsLatestTag = func(_ context.Context, repo string) (string, error) {
		return map[string]string{"joomla/joomla-cms": "6.1.3", "opencart/opencart": "4.1.0.4"}[repo], nil
	}
	t.Cleanup(func() { cmsFetch, cmsFetchBytes, cmsLatestTag = prevF, prevB, prevT })
	f.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		out := func(s string) *agent.RunAsUserResponse {
			return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(s))}
		}
		a := strings.Join(req.Args, " ")
		switch {
		case req.Args[0] == "list":
			return out("[]")
		case req.Args[0] == "read":
			return out("<?php // dist\n")
		case strings.Contains(a, "core version"):
			return out("7.1\n")
		case strings.Contains(a, "cli_install.php"):
			return out("SUCCESS: You have modified your OpenCart shop.\n")
		}
		return nil
	}
	return f
}

func (f *siteFixture) installCMS(t *testing.T, domain string, body map[string]any) (apitypes.CMSInstallResult, *store.Job) {
	t.Helper()
	var res apitypes.CMSInstallResult
	f.call(http.MethodPost, "/sites/"+domain+"/cms", body, http.StatusAccepted, &res)
	return res, f.waitJob(res.JobID)
}

// cmsJobPasswords returns the administrator and database passwords a CMS job
// carried (encrypted in its payload).
func (f *siteFixture) cmsJobPasswords(t *testing.T, job *store.Job) (admin, db string) {
	t.Helper()
	var p cmsPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		t.Fatal(err)
	}
	admin, err := f.s.secrets.Decrypt(p.AdminPasswordEnc)
	if err != nil {
		t.Fatal(err)
	}
	if db, err = f.s.secrets.Decrypt(p.DBPasswordEnc); err != nil {
		t.Fatal(err)
	}
	return admin, db
}

// installerRun is one run of a CMS installer the fake agent was asked for:
// fsop run's arguments, what follows the PHP binary on the command line, the
// arguments that came on stdin, and the $argv the installer gets from both.
type installerRun struct {
	argv, php, stdin, effective []string
}

// installerRuns decodes every fsop run the fake agent saw. A run with the
// bootstrap must bring its arguments on stdin, each ended by NUL; a run
// without it must have nothing there.
func (f *siteFixture) installerRuns(t *testing.T) []installerRun {
	t.Helper()
	var runs []installerRun
	for _, r := range f.agent.RunAs() {
		if len(r.Args) == 0 || r.Args[0] != "run" {
			continue
		}
		i := slices.Index(r.Args, "--")
		if i < 0 || len(r.Args) < i+3 {
			t.Fatalf("fsop run without -- <php> <script>: %q", r.Args)
		}
		run := installerRun{argv: r.Args, php: r.Args[i+2:]}
		in, err := base64.StdEncoding.DecodeString(r.StdinBase64)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case run.php[0] == "-r":
			if len(run.php) < 4 || run.php[1] != cmsArgvBootstrap || run.php[2] != "--" {
				t.Fatalf("php -r must run the bootstrap, then -- <script>: %q", run.php)
			}
			if len(in) == 0 || in[len(in)-1] != 0 {
				t.Fatalf("the bootstrap's arguments must come on stdin, each ended by NUL: %q", in)
			}
			run.stdin = strings.Split(string(in[:len(in)-1]), "\x00")
			run.effective = append(slices.Clone(run.php[3:]), run.stdin...)
		case len(in) > 0:
			t.Fatalf("stdin for an installer run without the bootstrap: %q", r.Args)
		default:
			run.effective = run.php
		}
		runs = append(runs, run)
	}
	return runs
}

// assertOffArgv fails when a secret sits on any command line the fake agent
// was asked to run — a tool, a stream, an fsop run as a client: every local
// user can read those in ps.
func (f *siteFixture) assertOffArgv(t *testing.T, secrets ...string) {
	t.Helper()
	var lines [][]string
	for _, r := range f.agent.RunAs() {
		lines = append(lines, r.Args)
	}
	for _, tc := range f.agent.Tools() {
		lines = append(lines, append([]string{tc.Name}, tc.Args...))
	}
	for _, st := range f.agent.Streams() {
		lines = append(lines, append([]string{st.Name}, st.Args...))
	}
	for _, secret := range secrets {
		if secret == "" {
			t.Fatal("an empty secret proves nothing")
		}
		for _, line := range lines {
			for _, a := range line {
				if strings.Contains(a, secret) {
					t.Fatalf("a secret is on a command line: %q", line)
				}
			}
		}
	}
}

// WordPress: the preset is applied, the database made, the tarball streamed
// into tar with its top-level folder dropped, wp-cli fetched and verified,
// the installer run as the client with the generated credentials — which
// come back once, never land in the job and reach wp-cli on stdin, not on a
// command line.
func TestCMSInstallWordPress(t *testing.T) {
	f := cmsFixture(t, map[string][]byte{"https://wordpress.org/latest.tar.gz": tarGz(t, map[string]string{"wordpress/index.php": "<?php", "wordpress/wp-config-sample.php": "<?php"})})
	site := f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	res, job := f.installCMS(t, site.Domain, map[string]any{"cms": "wordpress", "title": "My blog"})
	if job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	if res.AdminLogin != "admin" || len(res.AdminPassword) != 16 || res.AdminEmail != "admin@wp.example.com" || res.Database != "alex_wordpress" || res.AdminURL != "http://wp.example.com/wp-admin/" {
		t.Fatalf("result: %+v", res)
	}
	if strings.Contains(string(job.Payload), res.AdminPassword) {
		t.Fatal("the administrator password must not sit in the job payload in the clear")
	}
	stored, _ := f.db.GetSiteByDomain(f.ctx, site.Domain)
	if stored.CMS != "wordpress" || stored.CMSVersion != "7.1" || stored.CMSAt == "" || stored.Preset != "wordpress" {
		t.Fatalf("site record: cms=%s version=%s at=%s preset=%s", stored.CMS, stored.CMSVersion, stored.CMSAt, stored.Preset)
	}
	tarred := false
	for _, st := range f.agent.Streams() {
		if st.Direction == "in" && st.Name == "tar" && strings.Contains(strings.Join(st.Args, " "), "--strip-components=1") && strings.Contains(strings.Join(st.Args, " "), "/var/www/alex/data/www/wp.example.com") && st.Bytes > 0 {
			tarred = true
		}
	}
	if !tarred {
		t.Fatalf("the tarball must be streamed into tar with its folder stripped: %+v", f.agent.Streams())
	}
	adminPassword, dbPassword := f.cmsJobPasswords(t, job)
	if adminPassword != res.AdminPassword {
		t.Fatal("the job's administrator password is not the one shown")
	}
	for _, r := range f.agent.RunAs() {
		if r.Args[0] == "run" && (r.Login != "alex" || !strings.HasPrefix(strings.Join(r.Args, " "), "run --cwd data/www/wp.example.com -- /usr/bin/php8.4 ")) {
			t.Fatalf("wp-cli must run as the client with the site's PHP, in the docroot: %s %q", r.Login, r.Args)
		}
	}
	// wp-cli gets every argument it got before, the passwords from stdin
	wp := func(args ...string) []string {
		return append([]string{wpCLIPhar, "--path=/var/www/alex/data/www/wp.example.com"}, args...)
	}
	config, install, version, chown, wpcli := false, false, false, false, false
	for _, run := range f.installerRuns(t) {
		switch {
		case slices.Equal(run.effective, wp("config", "create", "--dbname=alex_wordpress", "--dbuser=alex_wordpress", "--dbhost=localhost", "--skip-check", "--dbpass="+dbPassword)):
			config = slices.Equal(run.stdin, []string{"--dbpass=" + dbPassword})
		case slices.Equal(run.effective, wp("core", "install", "--url=http://wp.example.com", "--title=My blog", "--admin_user=admin", "--admin_email=admin@wp.example.com", "--skip-email", "--admin_password="+res.AdminPassword)):
			install = slices.Equal(run.stdin, []string{"--admin_password=" + res.AdminPassword})
		case slices.Equal(run.effective, wp("core", "version")):
			version = run.stdin == nil // nothing secret: no bootstrap
		default:
			t.Fatalf("unexpected wp-cli run: %q", run.effective)
		}
	}
	if !config || !install || !version {
		t.Fatalf("config=%v install=%v version=%v; runs: %+v", config, install, version, f.agent.RunAs())
	}
	f.assertOffArgv(t, adminPassword, dbPassword)
	for _, c := range f.agent.Calls() {
		switch {
		case c.Path == "/v1/chown" && strings.Contains(string(c.Body), `"recursive":true`) && strings.Contains(string(c.Body), "wp.example.com"):
			chown = true
		case c.Path == "/v1/config/apply" && strings.Contains(string(c.Body), wpCLIPhar):
			wpcli = true
		}
	}
	if !chown || !wpcli {
		t.Fatalf("chown=%v wpcli=%v", chown, wpcli)
	}
	if _, err := f.db.GetDatabaseByName(f.ctx, "alex_wordpress"); err != nil {
		t.Fatalf("database record: %v", err)
	}
}

// The CMS job records its install alone: the site was read minutes before,
// and what changed meanwhile — the address sites.move-ip gave it, a field mp
// site set changed — stays.
func TestCMSInstallKeepsWhatChangedMeanwhile(t *testing.T) {
	f := cmsFixture(t, map[string][]byte{"https://wordpress.org/latest.tar.gz": tarGz(t, map[string]string{"wordpress/index.php": "<?php", "wordpress/wp-config-sample.php": "<?php"})})
	f.s.SetHostIPs(func() []string { return []string{"10.0.0.1", "10.0.0.2"} })
	site := f.createSite(map[string]any{"domain": "wp.example.com", "user": "alex", "php_version": "8.4", "ssl": "none", "ip": "10.0.0.1"})
	// While wp-cli reports the version it installed, the last step before
	// the job writes its record.
	var once sync.Once
	f.agent.SetBefore(func(p string, body []byte) {
		if p == "/v1/runas" && strings.Contains(string(body), `"core","version"`) {
			once.Do(func() {
				if moved, err := f.db.MoveSiteIP(f.ctx, site.ID, "10.0.0.1", "10.0.0.2"); err != nil || !moved {
					t.Errorf("move-ip: %v %v", moved, err)
				}
				cur, err := f.db.GetSite(f.ctx, site.ID)
				if err != nil {
					t.Error(err)
					return
				}
				cur.ClientMaxBody = "512m"
				if err := f.db.UpdateSite(f.ctx, cur); err != nil {
					t.Error(err)
				}
			})
		}
	})
	t.Cleanup(func() { f.agent.SetBefore(nil) })
	if _, job := f.installCMS(t, site.Domain, map[string]any{"cms": "wordpress", "title": "My blog"}); job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	got, err := f.db.GetSite(f.ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IP != "10.0.0.2" || got.ClientMaxBody != "512m" {
		t.Fatalf("what changed during the install is lost: ip %s, client_max_body %s", got.IP, got.ClientMaxBody)
	}
	if got.CMS != "wordpress" || got.CMSVersion != "7.1" || got.CMSAt == "" || got.CMSDatabase != "alex_wordpress" || got.Preset != "wordpress" {
		t.Fatalf("the install's record: cms %s %s at %q database %s preset %s", got.CMS, got.CMSVersion, got.CMSAt, got.CMSDatabase, got.Preset)
	}
}

// Joomla: the release tarball from GitHub, its own CLI installer run as the
// client — the two passwords on stdin, every other option where it was — and
// installation/ removed afterwards.
func TestCMSInstallJoomla(t *testing.T) {
	src := tarGz(t, map[string]string{"index.php": "<?php", "installation/joomla.php": "<?php"})
	f := cmsFixture(t, map[string][]byte{"https://github.com/joomla/joomla-cms/releases/download/6.1.3/Joomla_6.1.3-Stable-Full_Package.tar.gz": src})
	site := f.createSite(map[string]any{"domain": "club.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	res, job := f.installCMS(t, site.Domain, map[string]any{"cms": "joomla", "title": "Our club", "admin_login": "chief", "admin_email": "chief@example.com"})
	if job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	adminPassword, dbPassword := f.cmsJobPasswords(t, job)
	if len(res.AdminPassword) != 16 || adminPassword != res.AdminPassword || res.Database != "alex_joomla" || res.AdminURL != "http://club.example.com/administrator/" {
		t.Fatalf("result: %+v", res)
	}
	runs := f.installerRuns(t)
	want := []string{"installation/joomla.php", "install", "--site-name=Our club", "--admin-user=Administrator", "--admin-username=chief", "--admin-email=chief@example.com",
		"--db-type=mysqli", "--db-host=localhost", "--db-user=alex_joomla", "--db-name=alex_joomla", "--db-prefix=j_", "--db-encryption=0", "--no-interaction",
		"--admin-password=" + adminPassword, "--db-pass=" + dbPassword}
	if len(runs) != 1 || !slices.Equal(runs[0].effective, want) || !slices.Equal(runs[0].stdin, []string{"--admin-password=" + adminPassword, "--db-pass=" + dbPassword}) ||
		!strings.HasPrefix(strings.Join(runs[0].argv, " "), "run --cwd data/www/club.example.com -- /usr/bin/php8.4 -r ") {
		t.Fatalf("installer runs: %+v", runs)
	}
	f.assertOffArgv(t, adminPassword, dbPassword)
	removed := slices.ContainsFunc(f.agent.RunAs(), func(r agent.RunAsUserRequest) bool {
		return strings.Join(r.Args, " ") == "rm data/www/club.example.com/installation"
	})
	if !removed {
		t.Fatalf("installation/ must be removed after the install: %+v", f.agent.RunAs())
	}
	stored, _ := f.db.GetSiteByDomain(f.ctx, site.Domain)
	if stored.CMS != "joomla" || stored.CMSVersion != "6.1.3" || stored.Preset != "joomla" || stored.CMSDatabase != "alex_joomla" {
		t.Fatalf("site record: %+v", stored)
	}
}

// OpenCart comes as a zip: it is transcoded to a tar stream with the
// vendor's two folders stripped; the dist configs are copied and the CLI
// installer run; a second install of the same CMS takes the next name and
// needs force for the docroot that is no longer empty.
func TestCMSInstallOpenCartAndForce(t *testing.T) {
	src := zipBytes(t, map[string]string{"upload/index.php": "<?php", "upload/config-dist.php": "<?php", "upload/admin/config-dist.php": "<?php", "README.md": "x", ".github/workflows/ci.yml": "x"})
	f := cmsFixture(t, map[string][]byte{"https://github.com/opencart/opencart/releases/download/4.1.0.4/opencart-4.1.0.4.zip": src})
	site := f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	res, job := f.installCMS(t, site.Domain, map[string]any{"cms": "opencart", "admin_login": "owner", "admin_password": "Sup3rSecretPass!", "admin_email": "o@example.com"})
	if job.Status != store.JobDone {
		t.Fatalf("install: %s %s", job.Status, job.Error)
	}
	if res.AdminPassword != "" || res.AdminLogin != "owner" || res.Database != "alex_opencart" {
		t.Fatalf("a given password is not echoed: %+v", res)
	}
	stripped := false
	for _, st := range f.agent.Streams() {
		if st.Direction == "in" && st.Name == "tar" && strings.Contains(strings.Join(st.Args, " "), "-xf - -C /var/www/alex/data/www/shop.example.com") && strings.HasSuffix(strings.Join(st.Args, " "), "--strip-components=1 upload") && st.Bytes > 0 {
			stripped = true
		}
	}
	if !stripped {
		t.Fatalf("zip must arrive as a tar stream, upload/ only, one component stripped: %+v", f.agent.Streams())
	}
	wrote := 0
	for _, r := range f.agent.RunAs() {
		a := strings.Join(r.Args, " ")
		if strings.HasPrefix(a, "write data/www/shop.example.com/") && strings.HasSuffix(a, "config.php") {
			wrote++
		}
	}
	// the installer reads "--option value" pairs: both password pairs come
	// on stdin, after the rest
	_, dbPassword := f.cmsJobPasswords(t, job)
	runs := f.installerRuns(t)
	want := []string{"install/cli_install.php", "install", "--username", "owner", "--email", "o@example.com", "--http_server", "http://shop.example.com/",
		"--db_driver", "mysqli", "--db_hostname", "localhost", "--db_username", "alex_opencart", "--db_database", "alex_opencart", "--db_port", "3306", "--db_prefix", "oc_",
		"--password", "Sup3rSecretPass!", "--db_password", dbPassword}
	if wrote != 2 || len(runs) != 1 || !slices.Equal(runs[0].effective, want) || !slices.Equal(runs[0].stdin, []string{"--password", "Sup3rSecretPass!", "--db_password", dbPassword}) {
		t.Fatalf("configs written=%d; installer runs: %+v", wrote, runs)
	}
	f.assertOffArgv(t, "Sup3rSecretPass!", dbPassword)
	stored, _ := f.db.GetSiteByDomain(f.ctx, site.Domain)
	if stored.CMSVersion != "4.1.0.4" || stored.Preset != "opencart" {
		t.Fatalf("site record: %+v", stored)
	}

	// The docroot now has files: without force the job refuses, with force it clears them.
	prev := f.agent.RunAsHook
	f.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		if req.Args[0] == "list" {
			return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(`[{"name":"index.php","type":"file"},{"name":"admin","type":"dir"}]`))}
		}
		return prev(req)
	}
	_, job = f.installCMS(t, site.Domain, map[string]any{"cms": "opencart"})
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "not empty") {
		t.Fatalf("a full docroot must stop the install: %s %s", job.Status, job.Error)
	}
	res, job = f.installCMS(t, site.Domain, map[string]any{"cms": "opencart", "force": true})
	if job.Status != store.JobDone || res.Database != "alex_opencart" {
		t.Fatalf("force reuses the database: %s %s; database %s", job.Status, job.Error, res.Database)
	}
	// the reinstall's new passwords take the same way
	adminPassword, dbPassword := f.cmsJobPasswords(t, job)
	if adminPassword != res.AdminPassword {
		t.Fatal("the job's administrator password is not the one shown")
	}
	runs = f.installerRuns(t)
	if len(runs) != 2 || !slices.Equal(runs[1].stdin, []string{"--password", adminPassword, "--db_password", dbPassword}) {
		t.Fatalf("the reinstall's passwords must reach the installer on stdin: %+v", runs)
	}
	f.assertOffArgv(t, adminPassword, dbPassword)
	dropped := false
	for _, tc := range f.agent.Tools() {
		if tc.Name == "mysql" && strings.Contains(tc.Stdin, "DROP DATABASE IF EXISTS `alex_opencart`") {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("force must empty the database first: %+v", f.agent.Tools())
	}
	removed := false
	for _, r := range f.agent.RunAs() {
		if strings.Join(r.Args, " ") == "rm data/www/shop.example.com/index.php data/www/shop.example.com/admin" {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("force must remove the docroot's entries first: %+v", f.agent.RunAs())
	}
}

// A fresh site holds only the placeholder page site apply wrote there. The
// install takes that for an empty docroot — removes the page and keeps the
// database — while an edited page or anything next to it still needs force.
func TestCMSInstallOverPlaceholder(t *testing.T) {
	src := zipBytes(t, map[string]string{"upload/index.php": "<?php", "upload/config-dist.php": "<?php", "upload/admin/config-dist.php": "<?php"})
	f := cmsFixture(t, map[string][]byte{"https://github.com/opencart/opencart/releases/download/4.1.0.4/opencart-4.1.0.4.zip": src})
	site := f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	welcome, err := f.s.placeholderPage(site.Domain)
	if err != nil {
		t.Fatal(err)
	}
	page, extra := welcome, ""
	prev := f.agent.RunAsHook
	f.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		reply := func(s string) *agent.RunAsUserResponse {
			return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(s))}
		}
		switch strings.Join(req.Args, " ") {
		case "list data/www/shop.example.com":
			return reply(fmt.Sprintf(`[{"name":"index.html","type":"file","size":%d}%s]`, len(page), extra))
		case "read data/www/shop.example.com/index.html":
			return reply(page)
		}
		return prev(req)
	}

	// Same size, other text: the owner's page, not the panel's.
	page = strings.Replace(welcome, "coming", "moving", 1)
	if _, job := f.installCMS(t, site.Domain, map[string]any{"cms": "opencart"}); job.Status != store.JobFailed || !strings.Contains(job.Error, "not empty") {
		t.Fatalf("an edited page must stop the install: %s %s", job.Status, job.Error)
	}
	page, extra = welcome, `,{"name":"app.php","type":"file","size":5}`
	if _, job := f.installCMS(t, site.Domain, map[string]any{"cms": "opencart"}); job.Status != store.JobFailed || !strings.Contains(job.Error, "not empty") {
		t.Fatalf("a file next to the placeholder must stop the install: %s %s", job.Status, job.Error)
	}

	extra = ""
	if _, job := f.installCMS(t, site.Domain, map[string]any{"cms": "opencart"}); job.Status != store.JobDone {
		t.Fatalf("the untouched placeholder must not need force: %s %s", job.Status, job.Error)
	}
	removed := false
	for _, r := range f.agent.RunAs() {
		if strings.Join(r.Args, " ") == "rm data/www/shop.example.com/index.html" {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("the placeholder must be removed before unpacking: %+v", f.agent.RunAs())
	}
	for _, tc := range f.agent.Tools() {
		if tc.Name == "mysql" && strings.Contains(tc.Stdin, "DROP DATABASE") {
			t.Fatalf("installing over the placeholder must keep the database: %s", tc.Stdin)
		}
	}
}

// Sites made by earlier versions hold their Russian-only placeholder: that is
// the panel's page too, not the owner's.
func TestCMSInstallOverOldPlaceholder(t *testing.T) {
	src := zipBytes(t, map[string]string{"upload/index.php": "<?php", "upload/config-dist.php": "<?php", "upload/admin/config-dist.php": "<?php"})
	f := cmsFixture(t, map[string][]byte{"https://github.com/opencart/opencart/releases/download/4.1.0.4/opencart-4.1.0.4.zip": src})
	site := f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var old bytes.Buffer
	if err := placeholderRUTemplate.Execute(&old, render.Welcome{Domain: site.Domain}); err != nil || !strings.Contains(old.String(), "<h1>Скоро здесь будет сайт</h1>") {
		t.Fatalf("the old placeholder: %v\n%s", err, old.String())
	}
	prev := f.agent.RunAsHook
	f.agent.RunAsHook = func(req agent.RunAsUserRequest) *agent.RunAsUserResponse {
		reply := func(s string) *agent.RunAsUserResponse {
			return &agent.RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString([]byte(s))}
		}
		switch strings.Join(req.Args, " ") {
		case "list data/www/shop.example.com":
			return reply(fmt.Sprintf(`[{"name":"index.html","type":"file","size":%d}]`, old.Len()))
		case "read data/www/shop.example.com/index.html":
			return reply(old.String())
		}
		return prev(req)
	}
	if _, job := f.installCMS(t, site.Domain, map[string]any{"cms": "opencart"}); job.Status != store.JobDone {
		t.Fatalf("the placeholder of earlier versions must not need force: %s %s", job.Status, job.Error)
	}
}

// Force empties only the database the panel made for this site's own CMS. A
// database that merely carries the name — made by hand, or another site's — is
// left alone and the CMS gets a new one; a deleted database is forgotten, so a
// later one of the same name is never taken for the site's.
func TestCMSForceDropsOnlyItsOwnDatabase(t *testing.T) {
	src := zipBytes(t, map[string]string{"upload/index.php": "<?php", "upload/config-dist.php": "<?php", "upload/admin/config-dist.php": "<?php"})
	f := cmsFixture(t, map[string][]byte{"https://github.com/opencart/opencart/releases/download/4.1.0.4/opencart-4.1.0.4.zip": src})
	a := f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	b := f.createSite(map[string]any{"domain": "shop2.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	var manual apitypes.DatabaseResponse
	f.call(http.MethodPost, "/databases", map[string]any{"name": "opencart", "user": "alex"}, http.StatusCreated, &manual)
	if manual.Database == nil || manual.Database.Name != "alex_opencart" {
		t.Fatalf("hand-made database: %+v", manual.Database)
	}
	drops := func() []string {
		var out []string
		for _, tc := range f.agent.Tools() {
			if tc.Name == "mysql" && strings.Contains(tc.Stdin, "DROP DATABASE") {
				out = append(out, strings.TrimSpace(tc.Stdin))
			}
		}
		return out
	}
	force := func(site *store.Site, want string) {
		t.Helper()
		res, job := f.installCMS(t, site.Domain, map[string]any{"cms": "opencart", "force": true})
		if job.Status != store.JobDone || res.Database != want {
			t.Fatalf("%s: %s %s, database %q, want %q", site.Domain, job.Status, job.Error, res.Database, want)
		}
	}

	force(a, "alex_opencart2")
	force(b, "alex_opencart3")
	if d := drops(); len(d) != 0 {
		t.Fatalf("no site had a CMS yet, yet force dropped: %q", d)
	}
	force(a, "alex_opencart2")
	if d := drops(); len(d) != 1 || !strings.Contains(d[0], "`alex_opencart2`") {
		t.Fatalf("a reinstall over the site's own CMS must empty its database only: %q", d)
	}

	f.call(http.MethodDelete, "/databases/alex_opencart2", nil, http.StatusNoContent, nil)
	if stored, _ := f.db.GetSiteByDomain(f.ctx, a.Domain); stored.CMSDatabase != "" {
		t.Fatalf("a deleted database must be forgotten by the site: %q", stored.CMSDatabase)
	}
	before := len(drops()) // the deletion itself drops it
	force(a, "alex_opencart2")
	if d := drops(); len(d) != before {
		t.Fatalf("a new database of a deleted one's name must not be dropped: %q", d[before:])
	}
}

// A CMS needs PHP, a database server and a docroot: proxy sites and
// unknown CMS names are refused up front.
func TestCMSInstallRefusals(t *testing.T) {
	f := cmsFixture(t, nil)
	site := f.createSite(map[string]any{"domain": "app.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "drupal"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "wordpress", "admin_password": "short"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "joomla", "edition": "start"}, http.StatusUnprocessableEntity, nil)
	// a NUL ends an installer argument: refused before any job
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "wordpress", "admin_password": "Sup3rSecret\x00Pass"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "joomla", "title": "a\x00b"}, http.StatusUnprocessableEntity, nil)
	var list []apitypes.CMSInfo
	f.call(http.MethodGet, "/cms", nil, http.StatusOK, &list)
	if len(list) != 4 || list[3].ID != "bitrix" || len(list[3].Editions) != 4 {
		t.Fatalf("catalogue: %+v", list)
	}
}

// On stdin a NUL would split a secret into two arguments: cmsRun refuses it
// before the agent is asked to run anything.
func TestCMSRunRefusesNUL(t *testing.T) {
	f := cmsFixture(t, nil)
	site := f.createSite(map[string]any{"domain": "shop.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	owner, err := f.db.GetUserByID(f.ctx, site.UserID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.agent.RunAs())
	in := cmsInstall{site: site, owner: owner, layout: f.s.layoutFor(site, owner), rel: "data/www/shop.example.com"}
	if _, err := f.s.cmsRun(f.ctx, in, "install/cli_install.php", []string{"install"}, "--password", "Sup3r\x00Secret"); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("a NUL in a secret must be refused: %v", err)
	}
	if runs := f.agent.RunAs(); len(runs) != before {
		t.Fatalf("nothing may run: %+v", runs[before:])
	}
}

// The bootstrap under a real PHP: the installer sees what it saw with the
// passwords on its command line — $argv, $argc, $_SERVER, its own path, an
// empty stdin, the same exit code — while that command line, which every
// local user can read, does not hold them. A phar (wp-cli) included this way
// runs as when executed. Missing stdin stops it before the installer starts.
// Needs php on PATH (the CI runners have it).
func TestCMSArgvBootstrapPHP(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php is not installed")
	}
	if exec.Command(php, "-r", `exit(function_exists("json_encode") ? 0 : 1);`).Run() != nil {
		t.Skip("this php has no json_encode to report what the installer sees")
	}
	// the real path: PHP reports the scripts' paths with symlinks resolved
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// what an installer may look at; /proc/self/cmdline is what ps shows
	const view = `$s = $_SERVER; unset($s['REQUEST_TIME'], $s['REQUEST_TIME_FLOAT']); ksort($s);
$g = array_keys($GLOBALS); sort($g);
$cmd = @file_get_contents('/proc/self/cmdline');
echo json_encode(array('argv' => $argv, 'argc' => $argc, 'globals' => $g, 'global_argv' => $GLOBALS['argv'], 'server' => $s, 'file' => __FILE__,
	'included' => get_included_files(), 'stdin' => stream_get_contents(STDIN), 'exposed' => $cmd === false ? null : strpos($cmd, 'Pa55-w0rd') !== false));
exit(3);`
	if err := os.MkdirAll(filepath.Join(dir, "installation"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installation", "joomla.php"), []byte("<?php\n"+view+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	type result struct {
		view   map[string]any
		code   int
		stdout string
		stderr string
	}
	run := func(stdin []byte, args ...string) result {
		t.Helper()
		// no prepend/append file: -r skips them, a direct run would not
		cmd := exec.Command(php, append([]string{"-d", "auto_prepend_file=", "-d", "auto_append_file="}, args...)...)
		cmd.Dir, cmd.Env = dir, []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "LANG=C.UTF-8"}
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		var res result
		var ee *exec.ExitError
		if err := cmd.Run(); errors.As(err, &ee) {
			res.code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		res.stdout, res.stderr = stdout.String(), stderr.String()
		// PHP before 7.4 prints the shebang of an included phar's stub; wp-cli's
		// output is not read where the bootstrap is used
		out := res.stdout
		if strings.HasPrefix(out, "#!") {
			_, out, _ = strings.Cut(out, "\n")
		}
		if out != "" {
			if err := json.Unmarshal([]byte(out), &res.view); err != nil {
				t.Fatalf("installer output: %v\n%s\n%s", err, res.stdout, res.stderr)
			}
		}
		return res
	}
	nul := func(args ...string) []byte {
		var b []byte
		for _, a := range args {
			b = append(append(b, a...), 0)
		}
		return b
	}
	same := func(name string, direct, boot result) {
		t.Helper()
		if direct.code != 3 || boot.code != 3 {
			t.Fatalf("%s: exit codes direct=%d bootstrap=%d\n%s\n%s", name, direct.code, boot.code, direct.stderr, boot.stderr)
		}
		if direct.view["exposed"] != nil && (direct.view["exposed"] != true || boot.view["exposed"] != false) {
			t.Fatalf("%s: the secret on the command line: direct=%v bootstrap=%v", name, direct.view["exposed"], boot.view["exposed"])
		}
		delete(direct.view, "exposed")
		delete(boot.view, "exposed")
		if !reflect.DeepEqual(direct.view, boot.view) {
			t.Fatalf("%s: the installer sees another world through the bootstrap\ndirect:    %v\nbootstrap: %v", name, direct.view, boot.view)
		}
	}

	// Joomla/OpenCart: a script relative to the docroot; values with spaces,
	// a newline, a pair of separate arguments and an empty one
	secrets := []string{"--admin-password=Pa55-w0rd x", "--db_password", "Pa55-w0rd\nline", ""}
	args := []string{"install", "--site-name=Our club"}
	same("script", run(nil, append(append([]string{"installation/joomla.php"}, args...), secrets...)...),
		run(nul(secrets...), append([]string{"-r", cmsArgvBootstrap, "--", "installation/joomla.php"}, args...)...))

	// No arguments on stdin: an error, and the installer never starts.
	for _, stdin := range [][]byte{nil, {}, []byte("--db-pass=Pa55-w0rd")} {
		res := run(stdin, "-r", cmsArgvBootstrap, "--", "installation/joomla.php", "install")
		if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "secret arguments did not arrive on stdin") {
			t.Fatalf("stdin %q: exit %d, stdout %q, stderr %q", stdin, res.code, res.stdout, res.stderr)
		}
	}

	// wp-cli: an absolute path to a phar whose stub maps itself and
	// includes its boot file, as wp-cli.phar's does.
	pharPath := filepath.Join(dir, "wp-cli.phar")
	build := `$p = new Phar($argv[1], 0, 'wp-cli.phar'); $p->addFromString('php/boot-phar.php', '<?php include "phar://wp-cli.phar/php/main.php";'); ` +
		`$p->addFromString('php/main.php', file_get_contents($argv[2])); $p->setStub("#!/usr/bin/env php\n<?php\nPhar::mapPhar();\ninclude 'phar://wp-cli.phar/php/boot-phar.php';\n__HALT_COMPILER(); ?>");`
	viewFile := filepath.Join(dir, "view.php")
	if err := os.WriteFile(viewFile, []byte("<?php\n"+view+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(php, "-d", "phar.readonly=0", "-r", build, "--", pharPath, viewFile).CombinedOutput(); err != nil {
		t.Logf("no phar support to build a test phar (%v: %s); the phar case is skipped", err, out)
		return
	}
	wp := []string{pharPath, "--path=" + dir, "config", "create", "--dbname=alex_wordpress"}
	boot := run(nul("--dbpass=Pa55-w0rd"), append([]string{"-r", cmsArgvBootstrap, "--"}, wp...)...)
	if file, _ := boot.view["file"].(string); !strings.HasPrefix(file, "phar://"+pharPath+"/") {
		t.Fatalf("the phar's own code must have run: %v\n%s", boot.view["file"], boot.stderr)
	}
	same("phar", run(nil, append(wp, "--dbpass=Pa55-w0rd")...), boot)
}

// zipToTar keeps names, sizes and contents.
func TestZipToTar(t *testing.T) {
	src := zipBytes(t, map[string]string{"a/b.txt": "hello", "a/c/d.php": "<?php echo 1;"})
	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := zipToTar(zr, &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if got["a/b.txt"] != "hello" || got["a/c/d.php"] != "<?php echo 1;" {
		t.Fatalf("tar content: %v", got)
	}
}

// The Bitrix wizard driver walks a wizard the way the real one behaves:
// plain steps, the AJAX module step with its Post({...}) hints, the
// solution radios selected through JavaScript, then the site itself.
func TestBitrixWizardDriver(t *testing.T) {
	seen := map[string]string{}
	page := func(step, next, extra string) string {
		return `<html><body><form action="/" method="post" name="__wizard_form"><input type="hidden" name="CurrentStepID" value="` + step + `"><input type="hidden" name="NextStepID" value="` + next + `">` + extra + `<input type="submit" name="StepNext" value="Далее"></form></body></html>`
	}
	ajaxCalls, downloadCalls := 0, 0
	filesFailedOnce := false
	prevDelay := bxRetryDelay
	bxRetryDelay = 0
	t.Cleanup(func() { bxRetryDelay = prevDelay })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, page("welcome", "agreement", ""))
			return
		}
		r.ParseForm()
		for k, v := range r.PostForm {
			if strings.HasPrefix(k, "__wiz_") {
				seen[k] = v[0]
			}
		}
		switch r.PostFormValue("CurrentStepID") {
		case "welcome":
			fmt.Fprint(w, page("agreement", "check_license_key", `<input type="hidden" name="__wiz_agree_license" value=""><input type="checkbox" name="__wiz_agree_license" value="Y">`))
		case "agreement":
			if r.PostFormValue("__wiz_agree_license") != "Y" {
				fmt.Fprint(w, page("agreement", "check_license_key", `Примите лицензионное соглашение<input type="checkbox" name="__wiz_agree_license" value="Y">`))
				return
			}
			fmt.Fprint(w, page("check_license_key", "requirements", `<input type="hidden" name="__wiz_dbType" value="mysql"><input type="checkbox" name="__wiz_lic_key_variant" value="Y" checked><input type="text" name="__wiz_user_name" value=""><input type="text" name="__wiz_user_surname" value=""><input type="text" name="__wiz_email" value="">`))
		case "check_license_key":
			fmt.Fprint(w, page("requirements", "create_database", `<input type="hidden" name="__wiz_license" value="S26-TRIAL">`))
		case "requirements":
			fmt.Fprint(w, page("create_database", "create_modules", `<input type="text" name="__wiz_host" value="localhost"><input type="radio" name="__wiz_create_user" value="N" checked><input type="radio" name="__wiz_create_user" value="Y"><input type="text" name="__wiz_user" value=""><input type="password" name="__wiz_password" value=""><input type="radio" name="__wiz_create_database" value="N" checked><input type="text" name="__wiz_database" value="sitemanager">`))
		case "create_database":
			if r.PostFormValue("__wiz_database") != "alex_bitrix" {
				fmt.Fprint(w, page("create_database", "create_modules", `<div id="error_text">Не указан пользователь</div>`))
				return
			}
			fmt.Fprint(w, page("create_modules", "update_modules", `<input type="hidden" name="__wiz_nextStep" value="main"><input type="hidden" name="__wiz_nextStepStage" value="database"><script>var ajaxForm = new CAjaxForm("__wizard_form", "iframe-post-form", {"nextStep": "__wiz_nextStep", "nextStepStage": "__wiz_nextStepStage"}); ajaxForm.Post({}, "x");</script>`))
		case "create_modules":
			ajaxCalls++
			switch r.PostFormValue("__wiz_nextStep") + "/" + r.PostFormValue("__wiz_nextStepStage") {
			case "main/database":
				fmt.Fprint(w, `[response] window.ajaxForm.SetStatus('10'); window.ajaxForm.Post({'nextStep': 'main', 'nextStepStage': 'files'}, 'files'); [/response]`)
			case "main/files":
				// The update server hiccups once: the wizard answers with ShowError,
				// and the driver has to ask again instead of giving up.
				if !filesFailedOnce {
					filesFailedOnce = true
					fmt.Fprint(w, `[response] window.ajaxForm.ShowError('Ошибка соединения с сервером обновлений: [101] Network is unreachable. '); [/response]`)
					return
				}
				fmt.Fprint(w, `[response] window.ajaxForm.SetStatus('90'); window.ajaxForm.Post({'nextStep': '__finish', 'nextStepStage': ''}, ''); [/response]`)
			case "__finish/":
				fmt.Fprint(w, `[response] window.ajaxForm.Post({'nextStep': '__finish', 'nextStepStage': ''}, ''); [/response]`)
			case "/":
				fmt.Fprint(w, page("create_admin", "select_wizard", `<input type="text" name="__wiz_login" value="admin"><input type="password" name="__wiz_admin_password" value=""><input type="password" name="__wiz_admin_password_confirm" value=""><input type="text" name="__wiz_email" value="">`))
			}
		case "create_admin":
			fmt.Fprint(w, page("select_wizard", "finish", `<input type="radio" name="redio" onclick="SelectSolution(this, 'bitrix.sitecorporate:bitrix:corp_furniture');"><input type="radio" name="redio" onclick="SelectSolution(this, '@');"><input type="hidden" id="id___wiz_selected_wizard" name="__wiz_selected_wizard" value="">`))
		case "select_wizard":
			switch r.PostFormValue("__wiz_selected_wizard") {
			case "":
				fmt.Fprint(w, page("select_wizard", "finish", `Не указан мастер установки<input type="hidden" name="__wiz_selected_wizard" value="">`))
			case "@":
				fmt.Fprint(w, page("load_module", "select_wizard1", `<input type="hidden" id="id___wiz_selected_module" name="__wiz_selected_module" value=""><div onclick="SelectSolutionMP(this, 'nsandrey.emptyinstall');"><input type="radio" id="id_radio_nsandrey.emptyinstall" name="redio"></div>`))
			default:
				fmt.Fprint(w, `<html><head><title>Мебельная компания</title></head><body>Сайт работает</body></html>`)
			}
		case "load_module":
			if r.PostFormValue("__wiz_selected_module") != "nsandrey.emptyinstall" {
				fmt.Fprint(w, page("select_wizard", "finish", `Не указан мастер установки<input type="hidden" name="__wiz_selected_wizard" value="">`))
				return
			}
			fmt.Fprint(w, page("load_module_action", "finish", `<input type="hidden" name="__wiz_nextStep" value="load_module"><input type="hidden" name="__wiz_nextStepStage" value=""><script>var ajaxForm = new CAjaxForm("__wizard_form", "iframe-post-form", {"nextStep": "__wiz_nextStep", "nextStepStage": "__wiz_nextStepStage"});</script>`))
		case "load_module_action":
			// the marketplace download answers the same hint while it works, then the page
			downloadCalls++
			if downloadCalls < 4 {
				fmt.Fprint(w, `[response] window.ajaxForm.SetStatus('40'); window.ajaxForm.Post({'nextStep': 'do_update_module', 'nextStepStage': 'nsandrey.emptyinstall'}, 'Загрузка'); [/response]`)
				return
			}
			fmt.Fprint(w, `<html><head><title>Чистая установка</title></head><body>Сайт работает</body></html>`)
		default:
			http.Error(w, "unknown step "+r.PostFormValue("CurrentStepID"), 500)
		}
	}))
	defer srv.Close()
	ov := map[string]string{"__wiz_agree_license": "Y", "__wiz_user_name": "Site", "__wiz_user_surname": "Administrator", "__wiz_email": "a@example.com", "__wiz_user": "alex_bitrix", "__wiz_password": "dbpw", "__wiz_database": "alex_bitrix", "__wiz_login": "admin", "__wiz_admin_password": "Sup3rSecretPass!", "__wiz_admin_password_confirm": "Sup3rSecretPass!", "__wiz_admin_email": "a@example.com"}
	w := newBitrixWizard(srv.URL, "example.com", "", ov, nil)
	if err := w.run(context.Background()); err != nil {
		t.Fatalf("wizard: %v\n%s", err, strings.Join(w.log, "\n"))
	}
	if seen["__wiz_agree_license"] != "Y" || seen["__wiz_database"] != "alex_bitrix" || seen["__wiz_selected_wizard"] != "bitrix.sitecorporate:bitrix:corp_furniture" || seen["__wiz_admin_password"] != "Sup3rSecretPass!" || seen["__wiz_license"] != "S26-TRIAL" {
		t.Fatalf("fields sent: %v", seen)
	}
	if ajaxCalls != 5 {
		t.Fatalf("ajax calls: %d, want 5 (database, files with one retry, finish, final submit)", ajaxCalls)
	}
	if joined := strings.Join(w.log, "\n"); !strings.Contains(joined, "wizard finished") || !strings.Contains(joined, "retrying") {
		t.Fatalf("log: %v", w.log)
	}

	// The marketplace path: "@" on the solution step, the solution id on the next.
	seen = map[string]string{}
	ov["__wiz_selected_wizard"], ov["__wiz_selected_module"] = "@", "nsandrey.emptyinstall"
	w = newBitrixWizard(srv.URL, "example.com", "", ov, nil)
	if err := w.run(context.Background()); err != nil {
		t.Fatalf("marketplace wizard: %v\n%s", err, strings.Join(w.log, "\n"))
	}
	if seen["__wiz_selected_wizard"] != "@" || seen["__wiz_selected_module"] != "nsandrey.emptyinstall" {
		t.Fatalf("marketplace fields sent: %v", seen)
	}
	if downloadCalls != 4 {
		t.Fatalf("a repeated download hint means keep polling: %d calls", downloadCalls)
	}
}

var _ = json.Marshal
