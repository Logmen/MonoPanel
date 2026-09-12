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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
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

// WordPress: the preset is applied, the database made, the tarball streamed
// into tar with its top-level folder dropped, wp-cli fetched and verified,
// the installer run as the client with the generated credentials — which
// come back once and never land in the job.
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
	config, install, chown, wpcli := false, false, false, false
	for _, r := range f.agent.RunAs() {
		a := strings.Join(r.Args, " ")
		if r.Login == "alex" && strings.HasPrefix(a, "run --cwd data/www/wp.example.com -- /usr/bin/php8.4 "+wpCLIPhar) {
			if strings.Contains(a, "config create --dbname=alex_wordpress --dbuser=alex_wordpress --dbpass=") {
				config = true
			}
			if strings.Contains(a, "core install --url=http://wp.example.com --title=My blog --admin_user=admin --admin_password="+res.AdminPassword+" --admin_email=admin@wp.example.com --skip-email") {
				install = true
			}
		}
	}
	for _, c := range f.agent.Calls() {
		switch {
		case c.Path == "/v1/chown" && strings.Contains(string(c.Body), `"recursive":true`) && strings.Contains(string(c.Body), "wp.example.com"):
			chown = true
		case c.Path == "/v1/file/ensure" && strings.Contains(string(c.Body), wpCLIPhar):
			wpcli = true
		}
	}
	if !config || !install || !chown || !wpcli {
		t.Fatalf("config=%v install=%v chown=%v wpcli=%v; runs: %+v", config, install, chown, wpcli, f.agent.RunAs())
	}
	if _, err := f.db.GetDatabaseByName(f.ctx, "alex_wordpress"); err != nil {
		t.Fatalf("database record: %v", err)
	}
}

// OpenCart comes as a zip: it is transcoded to a tar stream with the
// vendor's two folders stripped; the dist configs are copied and the CLI
// installer run; a second install of the same CMS takes the next name and
// needs force for the docroot that is no longer empty.
func TestCMSInstallOpenCartAndForce(t *testing.T) {
	src := zipBytes(t, map[string]string{"opencart-4.1.0.4/upload/index.php": "<?php", "opencart-4.1.0.4/upload/config-dist.php": "<?php", "opencart-4.1.0.4/upload/admin/config-dist.php": "<?php", "opencart-4.1.0.4/README.md": "x"})
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
		if st.Direction == "in" && st.Name == "tar" && strings.Contains(strings.Join(st.Args, " "), "-xf - -C /var/www/alex/data/www/shop.example.com") && strings.Contains(strings.Join(st.Args, " "), "--strip-components=2") && st.Bytes > 0 {
			stripped = true
		}
	}
	if !stripped {
		t.Fatalf("zip must arrive as a tar stream with two components stripped: %+v", f.agent.Streams())
	}
	wrote, ran := 0, false
	for _, r := range f.agent.RunAs() {
		a := strings.Join(r.Args, " ")
		if strings.HasPrefix(a, "write data/www/shop.example.com/") && strings.HasSuffix(a, "config.php") {
			wrote++
		}
		if strings.Contains(a, "install/cli_install.php install --username owner --email o@example.com --password Sup3rSecretPass! --http_server http://shop.example.com/ --db_driver mysqli") {
			ran = true
		}
	}
	if wrote != 2 || !ran {
		t.Fatalf("configs written=%d installer=%v: %+v", wrote, ran, f.agent.RunAs())
	}
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
	if job.Status != store.JobDone || res.Database != "alex_opencart2" {
		t.Fatalf("force: %s %s; database %s", job.Status, job.Error, res.Database)
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

// A CMS needs PHP, a database server and a docroot: proxy sites and
// unknown CMS names are refused up front.
func TestCMSInstallRefusals(t *testing.T) {
	f := cmsFixture(t, nil)
	site := f.createSite(map[string]any{"domain": "app.example.com", "user": "alex", "php_version": "8.4", "ssl": "none"})
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "drupal"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "wordpress", "admin_password": "short"}, http.StatusUnprocessableEntity, nil)
	f.call(http.MethodPost, "/sites/"+site.Domain+"/cms", map[string]any{"cms": "joomla", "edition": "start"}, http.StatusUnprocessableEntity, nil)
	var list []apitypes.CMSInfo
	f.call(http.MethodGet, "/cms", nil, http.StatusOK, &list)
	if len(list) != 4 || list[3].ID != "bitrix" || len(list[3].Editions) != 2 {
		t.Fatalf("catalogue: %+v", list)
	}
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
	ajaxCalls := 0
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
				fmt.Fprint(w, `[response] window.ajaxForm.SetStatus('90'); window.ajaxForm.Post({'nextStep': '__finish', 'nextStepStage': ''}, ''); [/response]`)
			case "__finish/":
				fmt.Fprint(w, `[response] window.ajaxForm.Post({'nextStep': '__finish', 'nextStepStage': ''}, ''); [/response]`)
			case "/":
				fmt.Fprint(w, page("create_admin", "select_wizard", `<input type="text" name="__wiz_login" value="admin"><input type="password" name="__wiz_admin_password" value=""><input type="password" name="__wiz_admin_password_confirm" value=""><input type="text" name="__wiz_email" value="">`))
			}
		case "create_admin":
			fmt.Fprint(w, page("select_wizard", "finish", `<input type="radio" name="redio" onclick="SelectSolution(this, 'bitrix.sitecorporate:bitrix:corp_furniture');"><input type="radio" name="redio" onclick="SelectSolution(this, '@');"><input type="hidden" id="id___wiz_selected_wizard" name="__wiz_selected_wizard" value="">`))
		case "select_wizard":
			if r.PostFormValue("__wiz_selected_wizard") == "" {
				fmt.Fprint(w, page("select_wizard", "finish", `Не указан мастер установки<input type="hidden" name="__wiz_selected_wizard" value="">`))
				return
			}
			fmt.Fprint(w, `<html><head><title>Мебельная компания</title></head><body>Сайт работает</body></html>`)
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
	if ajaxCalls != 4 {
		t.Fatalf("ajax calls: %d, want 4 (database, files, finish, final submit)", ajaxCalls)
	}
	if !strings.Contains(strings.Join(w.log, "\n"), "wizard finished") {
		t.Fatalf("log: %v", w.log)
	}
}

var _ = json.Marshal
