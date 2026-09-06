//go:build e2e

// Package e2e drives a running panel over its REST API: it creates a user, a
// site and a database on a real host, checks that nginx and php-fpm actually
// serve the site, then removes everything it created.
//
// It never touches objects it did not create, so it is safe against a host
// that also carries real sites.
//
//	MONOPANEL_URL=https://toolkit.onehost.kz:8443 \
//	MONOPANEL_TOKEN=... \
//	go test -tags e2e ./e2e/ -v
//
// or simply: make e2e HOST=toolkit
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
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
		c.login(login, password)
	}
	return c
}

// login exchanges credentials for a session cookie.
func (c *client) login(login, password string) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]string{"login": login, "password": password})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.base+"/auth/login", bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("login: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
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

// do performs a request and returns status and body; it never fails the test
// itself so callers can assert on expected errors too.
func (c *client) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
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
	if id == 0 {
		return
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var job struct {
			Status, Error, Message string
		}
		c.must(http.MethodGet, fmt.Sprintf("/jobs/%d", id), nil, http.StatusOK, &job)
		switch job.Status {
		case "done":
			return
		case "failed":
			c.t.Fatalf("job %d failed: %s", id, job.Error)
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("job %d still %s after 5m", id, job.Status)
		}
		time.Sleep(time.Second)
	}
}

type jobRef struct {
	JobID int64 `json:"job_id"`
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
			t.Errorf("database response incomplete: %s", raw)
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
func TestDoctorIsClean(t *testing.T) {
	c := newClient(t)
	var doctor struct {
		Summary string
		Checks  []struct{ Name, Status, Detail string }
	}
	c.must(http.MethodGet, "/system/doctor", nil, http.StatusOK, &doctor)
	for _, ch := range doctor.Checks {
		if ch.Status == "fail" {
			t.Errorf("doctor: %s — %s", ch.Name, ch.Detail)
		}
	}
	t.Logf("doctor: %s", doctor.Summary)
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
