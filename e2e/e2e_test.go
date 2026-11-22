//go:build e2e

// Package e2e drives a running panel over its REST API: it creates a user, a
// site and a database on a real host, checks that nginx and php-fpm actually
// serve the site, then removes everything it created. It also checks what the
// API shows of the hardening of 0.8.20: changing one's own password takes the
// current one, the Services endpoint refuses a root shell, custom nginx
// directives cannot hand php-fpm ini lines, and a guest with a shell has its
// shared folder below <www_root>/.shares.
//
// It never touches objects it did not create, so it is safe against a host
// that also carries real sites; what it creates goes again however a test
// ends (t.Cleanup, registered as soon as the object exists).
//
//	MONOPANEL_URL=https://panel.example.com:8443 \
//	MONOPANEL_TOKEN=... \
//	go test -tags e2e ./e2e/ -v
//
// or simply: make e2e HOST=<ssh alias>
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

type client struct {
	base   string
	token  string
	cookie *http.Cookie
	http   *http.Client
	t      *testing.T
}

// newClient authenticates with an API token, or with an administrator's
// login and password when no token is available (`mp token create` needs a
// panel account, which the root socket does not have).
func newClient(t *testing.T) *client {
	t.Helper()
	base := os.Getenv("MONOPANEL_URL")
	token, login, password := os.Getenv("MONOPANEL_TOKEN"), os.Getenv("MONOPANEL_LOGIN"), os.Getenv("MONOPANEL_PASSWORD")
	if base == "" || (token == "" && password == "") {
		t.Skip("set MONOPANEL_URL and either MONOPANEL_TOKEN or MONOPANEL_LOGIN+MONOPANEL_PASSWORD")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: os.Getenv("MONOPANEL_INSECURE") == "1", //nolint:gosec // opt-in for self-signed panels
		MinVersion:         tls.VersionTLS12,
	}}
	c := &client{base: strings.TrimRight(base, "/") + "/api/v1", token: token, http: &http.Client{Transport: tr, Timeout: 60 * time.Second}, t: t}
	if token == "" {
		if login == "" {
			login = "admin"
		}
		// One sign-in for all the tests: the panel takes eight sign-ins (and
		// changes of one's own password) a minute from one address.
		adminSession.Lock()
		defer adminSession.Unlock()
		if adminSession.cookie == nil {
			c.login(login, password)
			adminSession.cookie = c.cookie
		}
		c.cookie = adminSession.cookie
	}
	return c
}

// adminSession is the session of the administrator the tests sign in as
// when they have a password rather than a token.
var adminSession struct {
	sync.Mutex
	cookie *http.Cookie
}

// login exchanges credentials for a session cookie. A 429 (the panel's
// sign-in limit, eight a minute from one address) waits for the next minute
// and tries once more.
func (c *client) login(login, password string) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]string{"login": login, "password": password})
	var res *http.Response
	var raw []byte
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.base+"/auth/login", bytes.NewReader(body))
		if err != nil {
			c.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err = c.http.Do(req)
		if err != nil {
			c.t.Fatalf("login: %v", err)
		}
		raw, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusTooManyRequests || attempt > 0 {
			break
		}
		c.t.Logf("login as %s: 429, waiting for the panel's one-minute window", login)
		time.Sleep(61 * time.Second)
	}
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("login as %s: %d %s", login, res.StatusCode, raw)
	}
	for _, ck := range res.Cookies() {
		if ck.Name == "mp_session" {
			c.cookie = ck
		}
	}
	if c.cookie == nil {
		c.t.Fatal("login succeeded but no session cookie was returned")
	}
}

// auth adds whichever credential this client holds.
func (c *client) auth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
		return
	}
	req.AddCookie(c.cookie)
}

// send performs a request and returns status and body, and an error only
// when there was no answer: the cleanups use it, which must not stop the
// ones after them.
func (c *client) send(method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw, nil
}

// do performs a request and returns status and body; it fails the test only
// when there is no answer, so callers can assert on expected errors too.
func (c *client) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	status, raw, err := c.send(method, path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	return status, raw
}

// doPaced is do for what the panel counts against its sign-in limit (eight
// a minute from one address), changing one's own password among it: a 429
// waits for the next minute and tries once more.
func (c *client) doPaced(method, path string, body any) (int, []byte) {
	c.t.Helper()
	status, raw := c.do(method, path, body)
	if status == http.StatusTooManyRequests {
		c.t.Logf("%s %s: 429, waiting for the panel's one-minute window", method, path)
		time.Sleep(61 * time.Second)
		status, raw = c.do(method, path, body)
	}
	return status, raw
}

// clone is a client of the same panel without credentials.
func (c *client) clone() *client {
	return &client{base: c.base, http: c.http, t: c.t}
}

// as is a client signed in as another account, with a session of its own.
func (c *client) as(login, password string) *client {
	c.t.Helper()
	n := c.clone()
	n.login(login, password)
	return n
}

// withToken is a client that authenticates with this API token.
func (c *client) withToken(token string) *client {
	n := c.clone()
	n.token = token
	return n
}

// must performs a request, requires the expected status and decodes the body.
func (c *client) must(method, path string, body any, want int, out any) {
	c.t.Helper()
	status, raw := c.do(method, path, body)
	if status != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode: %v: %s", method, path, err, raw)
		}
	}
}

// upload writes a file into a client's home through the panel's file API,
// which runs the write as that unix user (raw body, path in the query).
func (c *client) upload(login, path, content string) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		c.base+"/files/content?user="+url.QueryEscape(login)+"&path="+url.QueryEscape(path), strings.NewReader(content))
	if err != nil {
		c.t.Fatal(err)
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("upload %s: %v", path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("upload %s: %d %s", path, res.StatusCode, raw)
	}
}

// waitJob blocks until an asynchronous job finishes and fails on error.
func (c *client) waitJob(id int64) {
	c.t.Helper()
	if err := c.jobDone(id); err != nil {
		c.t.Fatal(err)
	}
}

// jobDone waits for a job and says how it ended, without failing the test:
// the cleanups wait with it.
func (c *client) jobDone(id int64) error {
	if id == 0 {
		return nil
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		status, raw, err := c.send(http.MethodGet, fmt.Sprintf("/jobs/%d", id), nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("job %d: %d %s", id, status, raw)
		}
		var job struct {
			Status, Error, Message string
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			return fmt.Errorf("job %d: %w: %s", id, err, raw)
		}
		switch job.Status {
		case "done":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("job %d %s: %s", id, job.Status, job.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job %d still %s after 5m", id, job.Status)
		}
		time.Sleep(time.Second)
	}
}

type jobRef struct {
	JobID int64 `json:"job_id"`
}

// remove is a cleanup: DELETE path, then the job it answers with, if any.
// It reports what went wrong without stopping the cleanups after it; 404
// means there is nothing left to remove.
func (c *client) remove(path string) {
	c.t.Helper()
	status, raw, err := c.send(http.MethodDelete, path, nil)
	switch {
	case err != nil:
		c.t.Errorf("cleanup: %v", err)
	case status == http.StatusNotFound:
	case status == http.StatusAccepted || status == http.StatusOK:
		var ref jobRef
		if json.Unmarshal(raw, &ref) == nil {
			if err := c.jobDone(ref.JobID); err != nil {
				c.t.Errorf("cleanup: DELETE %s: %v", path, err)
			}
		}
	case status == http.StatusNoContent:
	default:
		c.t.Errorf("cleanup: DELETE %s: %d %s", path, status, raw)
	}
}

// account is a user as GET /users/{login} returns it.
type account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Role  string `json:"role"`
	Home  string `json:"home"`
	Shell bool   `json:"shell"`
}

// newUser creates an account for the test (role "" for a plain one) and
// removes it, home and all, when the test ends, however it ends.
func (c *client) newUser(t *testing.T, login, password string, shell bool, role string) *account {
	t.Helper()
	body := map[string]any{"login": login, "password": password, "shell": shell}
	if role != "" {
		body["role"] = role
	}
	status, raw := c.do(http.MethodPost, "/users", body)
	if status != http.StatusAccepted {
		t.Fatalf("create user %s: %d %s", login, status, raw)
	}
	t.Cleanup(func() { c.remove("/users/" + login + "?purge=true") })
	var ref jobRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("create user %s: %v: %s", login, err, raw)
	}
	c.waitJob(ref.JobID)
	var u account
	c.must(http.MethodGet, "/users/"+login, nil, http.StatusOK, &u)
	if u.Home == "" {
		t.Fatalf("user %s has no home after its provisioning", login)
	}
	return &u
}

// newSite creates a site on php-fpm without TLS for the test and removes it
// with its files when the test ends.
func (c *client) newSite(t *testing.T, domain, owner, php string) {
	t.Helper()
	status, raw := c.do(http.MethodPost, "/sites", map[string]any{"domain": domain, "user": owner, "php_version": php, "ssl": "none"})
	if status != http.StatusAccepted {
		t.Fatalf("create site %s: %d %s", domain, status, raw)
	}
	t.Cleanup(func() { c.remove("/sites/" + domain + "?purge=true") })
	var ref jobRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("create site %s: %v: %s", domain, err, raw)
	}
	c.waitJob(ref.JobID)
}

// installedPHP is a PHP branch the host has installed; the test is skipped
// on a host without one.
func (c *client) installedPHP(t *testing.T) string {
	t.Helper()
	var php struct {
		Installed []struct{ Version, Status string }
	}
	c.must(http.MethodGet, "/php/versions", nil, http.StatusOK, &php)
	for _, v := range php.Installed {
		if v.Status == "installed" {
			return v.Version
		}
	}
	t.Skip("no PHP branch installed on the target host")
	return ""
}

// secret is a random password for an account of the tests: never logged.
func secret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "E2e-" + hex.EncodeToString(b) + "-Aa1"
}

// problem is the detail of an API error (RFC 9457), or the raw body.
func problem(raw []byte) string {
	var p struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &p) == nil && p.Detail != "" {
		return p.Detail
	}
	return string(raw)
}

// TestPanelLifecycle walks the full path a customer takes: an account, a site
// with a CMS preset, a database, then removal of all of it.
func TestPanelLifecycle(t *testing.T) {
	c := newClient(t)
	suffix := time.Now().Format("150405")
	login := "e2e" + suffix
	domain := "e2e-" + suffix + ".test"

	var health struct{ Status, Version string }
	c.must(http.MethodGet, "/health", nil, http.StatusOK, &health)
	if health.Status != "ok" {
		t.Fatalf("panel unhealthy: %+v", health)
	}
	t.Logf("panel %s", health.Version)

	// The PHP branch the site will use has to exist already.
	var php struct {
		Installed []struct{ Version, Status string }
	}
	c.must(http.MethodGet, "/php/versions", nil, http.StatusOK, &php)
	phpVersion := ""
	for _, v := range php.Installed {
		if v.Status == "installed" {
			phpVersion = v.Version
		}
	}
	if phpVersion == "" {
		t.Skip("no PHP branch installed on the target host")
	}

	// --- user -------------------------------------------------------------
	var user struct {
		User  struct{ Login string }
		JobID int64 `json:"job_id"`
	}
	c.must(http.MethodPost, "/users", map[string]any{"login": login, "password": "E2e-" + suffix + "-passw0rd", "shell": true}, http.StatusAccepted, &user)
	t.Cleanup(func() {
		status, raw := c.do(http.MethodDelete, "/users/"+login+"?purge=true", nil)
		if status != http.StatusAccepted {
			t.Errorf("cleanup: delete user %s: %d %s", login, status, raw)
			return
		}
		var ref jobRef
		json.Unmarshal(raw, &ref) //nolint:errcheck // best effort cleanup
		c.waitJob(ref.JobID)
	})
	c.waitJob(user.JobID)

	// --- site -------------------------------------------------------------
	var site struct {
		Site struct {
			Domain, Status, Preset, IP string
		}
		JobID int64 `json:"job_id"`
	}
	c.must(http.MethodPost, "/sites", map[string]any{
		"domain": domain, "user": login, "php_version": phpVersion, "ssl": "none", "preset": "wordpress",
	}, http.StatusAccepted, &site)
	c.waitJob(site.JobID)

	var stored struct{ Status, Preset, IP, LastError string }
	c.must(http.MethodGet, "/sites/"+domain, nil, http.StatusOK, &stored)
	if stored.Status != "active" {
		t.Fatalf("site status %s: %s", stored.Status, stored.LastError)
	}
	if stored.Preset != "wordpress" {
		t.Errorf("preset = %q", stored.Preset)
	}

	// --- the site actually answers ---------------------------------------
	// Placeholder page, then a PHP file written through the panel's file API.
	if code, body := fetch(t, stored.IP, domain, "/"); code != http.StatusOK {
		t.Errorf("GET / = %d (%s)", code, firstLine(body))
	}
	c.upload(login, "data/www/"+domain+"/probe.php", "<?php echo 'e2e-ok ', PHP_VERSION;")
	code, body := fetch(t, stored.IP, domain, "/probe.php")
	if code != http.StatusOK || !strings.Contains(body, "e2e-ok") {
		t.Errorf("PHP did not execute: %d %s", code, firstLine(body))
	} else {
		t.Logf("php responds: %s", firstLine(body))
	}
	// The WordPress preset must block PHP under uploads.
	c.must(http.MethodPost, "/files/op", map[string]any{"user": login, "op": "mkdir", "path": "data/www/" + domain + "/wp-content/uploads"}, http.StatusOK, nil)
	c.upload(login, "data/www/"+domain+"/wp-content/uploads/evil.php", "<?php echo 'pwned';")
	if code, _ := fetch(t, stored.IP, domain, "/wp-content/uploads/evil.php"); code != http.StatusForbidden {
		t.Errorf("preset must deny PHP in uploads, got %d", code)
	}

	// --- database ---------------------------------------------------------
	var db struct {
		Database struct{ Name string }
		Password string
	}
	status, raw := c.do(http.MethodPost, "/databases", map[string]any{"name": "shop", "user": login})
	switch status {
	case http.StatusCreated:
		json.Unmarshal(raw, &db) //nolint:errcheck // checked below
		if db.Database.Name == "" || db.Password == "" {
			// not the body: it carries the database password
			t.Errorf("database response incomplete: name %q, password given %v", db.Database.Name, db.Password != "")
		} else {
			t.Logf("database %s created", db.Database.Name)
		}
	case http.StatusUnprocessableEntity:
		t.Log("no database engine on the host, skipping the database step")
	default:
		t.Errorf("create database: %d %s", status, raw)
	}

	// --- site removal -----------------------------------------------------
	var ref jobRef
	c.must(http.MethodDelete, "/sites/"+domain+"?purge=true", nil, http.StatusAccepted, &ref)
	c.waitJob(ref.JobID)
	if status, _ := c.do(http.MethodGet, "/sites/"+domain, nil); status != http.StatusNotFound {
		t.Errorf("site still present after delete: %d", status)
	}
	// nginx no longer has a server block for the host: either the default
	// server answers with something else, or it closes the connection.
	if code, _, err := tryFetch(t, stored.IP, domain, "/probe.php"); err == nil && code == http.StatusOK {
		t.Error("site still served after delete")
	}
}

// TestDoctorIsClean asserts the host's own health check reports no failures.
// A service the doctor catches reloading — the test before it removed a site,
// and the site's php-fpm pool went with a reload — fails one look only: a
// failure counts when a second look, ten seconds later, still has it.
func TestDoctorIsClean(t *testing.T) {
	c := newClient(t)
	type report struct {
		Summary string
		Checks  []struct{ Name, Status, Detail string }
	}
	look := func() report {
		var r report
		c.must(http.MethodGet, "/system/doctor", nil, http.StatusOK, &r)
		return r
	}
	doctor := look()
	for _, ch := range doctor.Checks {
		if ch.Status == "fail" {
			t.Logf("doctor: %s — %s; looking again in 10s", ch.Name, ch.Detail)
			time.Sleep(10 * time.Second)
			doctor = look()
			break
		}
	}
	for _, ch := range doctor.Checks {
		switch ch.Status {
		case "fail":
			t.Errorf("doctor: %s — %s", ch.Name, ch.Detail)
		case "warn":
			// not a failure here (any host's own warnings count): the
			// testbed's doctor stage holds them against its expected list
			t.Logf("doctor warns: %s — %s", ch.Name, ch.Detail)
		}
	}
	t.Logf("doctor: %s", doctor.Summary)
}

// TestOwnPasswordNeedsCurrent: changing one's own password over a browser
// session or an API token takes the current password (0.8.20), so that a
// stolen session or token cannot set a new one and with it turn two-factor
// sign-in off; an administrator still sets another account's password
// without it. The account is a throwaway administrator, removed at the end
// (a run killed before that leaves an e2eadm… behind, and with two
// administrators `mp token create` as root asks which one: testbed.sh e2e
// removes such a leftover first, elsewhere mp user rm it).
func TestOwnPasswordNeedsCurrent(t *testing.T) {
	c := newClient(t)
	login := "e2eadm" + time.Now().Format("150405")
	pw := secret(t)
	c.newUser(t, login, pw, false, "admin")

	// refused: a change of the account's own password that does not come
	// with the right current one gets 403 naming current_password (the
	// passwords themselves stay out of the messages)
	refused := func(via string, cl *client, body map[string]any) {
		t.Helper()
		what := "without current_password"
		if _, ok := body["current_password"]; ok {
			what = "with a wrong current_password"
		}
		status, raw := cl.doPaced(http.MethodPatch, "/users/"+login, body)
		if status != http.StatusForbidden || !strings.Contains(string(raw), "current_password") {
			t.Errorf("own password over %s %s: %d %s, want 403 naming current_password", via, what, status, problem(raw))
		}
	}
	changed := func(via string, cl *client, body map[string]any) {
		t.Helper()
		status, raw := cl.doPaced(http.MethodPatch, "/users/"+login, body)
		if status != http.StatusOK && status != http.StatusAccepted {
			t.Fatalf("own password over %s with the current one: %d %s", via, status, problem(raw))
		}
	}

	// over a session of its own
	sess := c.as(login, pw)
	pw2 := secret(t)
	refused("a session", sess, map[string]any{"password": pw2})
	refused("a session", sess, map[string]any{"password": pw2, "current_password": pw + "x"})
	changed("a session", sess, map[string]any{"password": pw2, "current_password": pw})
	// the session the change came from stays
	sess.must(http.MethodGet, "/auth/me", nil, http.StatusOK, nil)

	// over an API token of its own
	var tok struct {
		Token  string `json:"token"`
		Record struct {
			ID int64 `json:"id"`
		} `json:"record"`
	}
	status, raw := sess.do(http.MethodPost, "/tokens", map[string]any{"name": "e2e-password"})
	if status != http.StatusCreated {
		t.Fatalf("token for %s: %d %s", login, status, problem(raw))
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.Token == "" {
		t.Fatalf("token for %s: %v", login, err)
	}
	t.Cleanup(func() { c.remove(fmt.Sprintf("/tokens/%d", tok.Record.ID)) })
	viaToken := c.withToken(tok.Token)
	pw3 := secret(t)
	refused("a token", viaToken, map[string]any{"password": pw3})
	refused("a token", viaToken, map[string]any{"password": pw3, "current_password": pw})
	changed("a token", viaToken, map[string]any{"password": pw3, "current_password": pw2})

	// an administrator sets another account's password without it
	status, raw = c.do(http.MethodPatch, "/users/"+login, map[string]any{"password": secret(t)})
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Errorf("an administrator setting %s's password: %d %s", login, status, problem(raw))
	}
}

// TestServicesRefuseRootShell: the Services endpoint acts on no unit that is
// a root shell — debug-shell.service, on tty9 — and says why with 403,
// while it still reports the unit's state. Should a regression let the start
// through, the shell is stopped again at once.
func TestServicesRefuseRootShell(t *testing.T) {
	c := newClient(t)
	const unit = "/services/debug-shell.service"
	status, raw := c.do(http.MethodPost, unit, map[string]any{"action": "start"})
	if status == http.StatusOK {
		c.do(http.MethodPost, unit, map[string]any{"action": "stop"})
		t.Fatalf("debug-shell.service was started (and stopped again): %s", raw)
	}
	if detail := problem(raw); status != http.StatusForbidden || !strings.Contains(detail, "is not allowed") || !strings.Contains(detail, "root shell") {
		t.Errorf("start debug-shell.service: %d %s, want 403 with the reason", status, detail)
	} else {
		t.Logf("refused: %s", detail)
	}
	var st struct {
		Unit        string `json:"unit"`
		LoadState   string `json:"load_state"`
		ActiveState string `json:"active_state"`
	}
	c.must(http.MethodGet, unit, nil, http.StatusOK, &st)
	if st.ActiveState == "active" {
		c.do(http.MethodPost, unit, map[string]any{"action": "stop"})
		t.Errorf("debug-shell.service is active (stopped again)")
	}
	t.Logf("%s: %s, %s", st.Unit, st.LoadState, st.ActiveState)
}

// TestCustomNginxRefusesPHPValue: a site's custom nginx directives may not
// hand php-fpm ini lines — fastcgi_param PHP_VALUE or PHP_ADMIN_VALUE, with
// which extension= loads any library into the pool — and the panel says so
// and writes nothing; a harmless directive still goes through. On a
// throwaway account and site.
func TestCustomNginxRefusesPHPValue(t *testing.T) {
	c := newClient(t)
	php := c.installedPHP(t)
	suffix := time.Now().Format("150405")
	owner, domain := "e2engx"+suffix, "e2e-nginx-"+suffix+".test"
	c.newUser(t, owner, secret(t), false, "")
	c.newSite(t, domain, owner, php)

	var cur struct {
		Custom string `json:"custom"`
	}
	for _, param := range []string{"PHP_VALUE", "php_admin_value"} {
		line := "fastcgi_param " + param + ` "auto_prepend_file=/etc/hostname";` + "\n"
		status, raw := c.do(http.MethodPut, "/sites/"+domain+"/nginx", map[string]any{"custom": line})
		detail := problem(raw)
		if status != http.StatusUnprocessableEntity || !strings.Contains(detail, "the panel does not take these directives") || !strings.Contains(detail, param) {
			t.Errorf("custom fastcgi_param %s: %d %s, want 422 with the panel's refusal", param, status, detail)
		} else {
			t.Logf("refused: %s", detail)
		}
	}
	c.must(http.MethodGet, "/sites/"+domain+"/nginx", nil, http.StatusOK, &cur)
	if cur.Custom != "" {
		t.Errorf("refused directives were written: %q", cur.Custom)
	}

	c.must(http.MethodPut, "/sites/"+domain+"/nginx", map[string]any{"custom": "add_header X-E2E-Custom \"1\";\n"}, http.StatusOK, &cur)
	if !strings.Contains(cur.Custom, "X-E2E-Custom") {
		t.Errorf("a harmless directive was not kept: %q", cur.Custom)
	}
	c.must(http.MethodPut, "/sites/"+domain+"/nginx", map[string]any{"custom": ""}, http.StatusOK, &cur)
	if cur.Custom != "" {
		t.Errorf("emptied custom directives remain: %q", cur.Custom)
	}
}

// TestShareMountPoints: a shared folder of a guest with a shell is mounted
// below <www_root>/.shares/<guest>/ — its home is its own, so only a symlink
// to the folder is there (0.8.20) — and an SFTP-only guest's inside its home,
// the chroot. Throwaway owner, site and guests.
func TestShareMountPoints(t *testing.T) {
	c := newClient(t)
	php := c.installedPHP(t)
	suffix := time.Now().Format("150405")
	owner, domain := "e2eown"+suffix, "e2e-share-"+suffix+".test"
	c.newUser(t, owner, secret(t), false, "")
	c.newSite(t, domain, owner, php)
	shellGuest := c.newUser(t, "e2esh"+suffix, secret(t), true, "")
	sftpGuest := c.newUser(t, "e2esf"+suffix, secret(t), false, "")

	type shareStatus struct {
		Share struct {
			Name string `json:"name"`
		} `json:"share"`
		Mounted    bool   `json:"mounted"`
		Where      string `json:"where"`
		MountPoint string `json:"mount_point"`
	}
	const name = "e2eshare"
	share := func(g *account) shareStatus {
		t.Helper()
		p := "/users/" + g.Login + "/shares"
		status, raw := c.do(http.MethodPost, p, map[string]any{"site": domain, "path": "shared", "name": name})
		if status != http.StatusCreated {
			t.Fatalf("share for %s: %d %s", g.Login, status, problem(raw))
		}
		t.Cleanup(func() { c.remove(p + "/" + name) })
		var st shareStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("share for %s: %v: %s", g.Login, err, raw)
		}
		return st
	}
	listed := func(g *account) string {
		t.Helper()
		var list []shareStatus
		c.must(http.MethodGet, "/users/"+g.Login+"/shares", nil, http.StatusOK, &list)
		for _, st := range list {
			if st.Share.Name == name {
				return st.MountPoint
			}
		}
		return ""
	}

	st := share(shellGuest)
	want := path.Join(path.Dir(shellGuest.Home), ".shares", shellGuest.Login, name)
	if st.MountPoint != want || !st.Mounted || st.Where != path.Join(shellGuest.Home, name) {
		t.Errorf("shell guest: mount_point %q (mounted %v), where %q; want %q mounted, where %q", st.MountPoint, st.Mounted, st.Where, want, path.Join(shellGuest.Home, name))
	}
	if !strings.HasPrefix(st.MountPoint, "/var/www/.shares/") {
		t.Logf("the web root is not /var/www here: mount_point %s", st.MountPoint)
	}
	if got := listed(shellGuest); got != want {
		t.Errorf("GET shares of %s: mount_point %q, want %q", shellGuest.Login, got, want)
	}

	st = share(sftpGuest)
	want = path.Join(sftpGuest.Home, name)
	if st.MountPoint != want || st.Where != want || !st.Mounted {
		t.Errorf("SFTP guest: mount_point %q (mounted %v), where %q; want both %q, mounted", st.MountPoint, st.Mounted, st.Where, want)
	}
	if got := listed(sftpGuest); got != want {
		t.Errorf("GET shares of %s: mount_point %q, want %q", sftpGuest.Login, got, want)
	}
}

// fetch requests a path from the site and fails the test if the host does not
// answer at all; use tryFetch where no answer is a valid outcome.
func fetch(t *testing.T, ip, host, path string) (int, string) {
	t.Helper()
	code, body, err := tryFetch(t, ip, host, path)
	if err != nil {
		t.Fatalf("GET %s%s: %v", host, path, err)
	}
	return code, body
}

// tryFetch requests a path from the site over plain HTTP against the host's IP.
func tryFetch(t *testing.T, ip, host, path string) (int, string, error) {
	t.Helper()
	if ip == "" {
		t.Skip("site has no IP to probe")
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort(ip, "80"))
	}}
	cl := &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cl.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	return res.StatusCode, string(body), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
