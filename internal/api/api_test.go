package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/auth"
	"monopanel/internal/config"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rel, _ := osprofile.ParseOSRelease(strings.NewReader("ID=debian\nVERSION_ID=13\n"))
	profile, _ := osprofile.FromRelease(rel)
	runner := jobs.NewRunner(db, 1, nil)
	s := New(cfg, db, agent.NewClient(filepath.Join(t.TempDir(), "none.sock")), runner, profile, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	h, _ := auth.HashPassword("secret-password")
	if err := db.CreateUser(ctx, &store.User{Login: "admin", Role: store.RoleAdmin, PasswordHash: h}); err != nil {
		t.Fatal(err)
	}
	return s, ts
}

func TestHealthAndOpenAPI(t *testing.T) {
	_, ts := testServer(t)
	res, err := http.Get(ts.URL + "/api/v1/health")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("health: %v %v", res, err)
	}
	res, _ = http.Get(ts.URL + "/api/v1/openapi.json")
	if res.StatusCode != 200 {
		t.Fatalf("openapi: %d", res.StatusCode)
	}
	var spec map[string]any
	json.NewDecoder(res.Body).Decode(&spec)
	paths := spec["paths"].(map[string]any)
	for _, p := range []string{"/users", "/jobs/{id}/events", "/auth/login", "/stack/install", "/services/{unit}"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("missing path %s in OpenAPI", p)
		}
	}
	res, _ = http.Get(ts.URL + "/")
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("ui: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, _ = http.Get(ts.URL + "/some/spa/route")
	if res.StatusCode != 200 {
		t.Fatalf("spa fallback: %d", res.StatusCode)
	}
}

func TestAuthFlow(t *testing.T) {
	_, ts := testServer(t)
	res, _ := http.Get(ts.URL + "/api/v1/auth/me")
	if res.StatusCode != 401 {
		t.Fatalf("unauthenticated me: %d", res.StatusCode)
	}
	body := strings.NewReader(`{"login":"admin","password":"wrong"}`)
	res, _ = http.Post(ts.URL+"/api/v1/auth/login", "application/json", body)
	if res.StatusCode != 401 {
		t.Fatalf("bad login: %d", res.StatusCode)
	}
	res, _ = http.Post(ts.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"login":"admin","password":"secret-password"}`))
	if res.StatusCode != 200 {
		t.Fatalf("login: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v", cookie)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/me", nil)
	req.AddCookie(cookie)
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("me with cookie: %d", res.StatusCode)
	}
	var me map[string]any
	json.NewDecoder(res.Body).Decode(&me)
	if me["login"] != "admin" || me["role"] != "admin" || me["via"] != "session" {
		t.Fatalf("me: %v", me)
	}

	// cross-site mutation with a session cookie is rejected
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/tokens", strings.NewReader(`{"name":"t"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(cookie)
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 403 {
		t.Fatalf("csrf: %d", res.StatusCode)
	}

	// token creation + bearer auth
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/tokens", strings.NewReader(`{"name":"cli","scopes":["admin"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 201 {
		t.Fatalf("token create: %d", res.StatusCode)
	}
	var tok map[string]any
	json.NewDecoder(res.Body).Decode(&tok)
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+tok["token"].(string))
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("bearer users: %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer nope")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 401 {
		t.Fatalf("bad bearer: %d", res.StatusCode)
	}

	// validation error is problem+json
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/users", strings.NewReader(`{"login":"Bad Login!"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok["token"].(string))
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 422 || !strings.Contains(res.Header.Get("Content-Type"), "problem+json") {
		t.Fatalf("validation: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestSelfSigned(t *testing.T) {
	dir := t.TempDir()
	c, k := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	created, err := EnsureSelfSigned(c, k, []string{"panel.example.com", "203.0.113.10"})
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	created, _ = EnsureSelfSigned(c, k, nil)
	if created {
		t.Fatal("must not recreate")
	}
	fp, err := CertFingerprint(c)
	if err != nil || len(fp) != 95 {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
}

func TestCertificateEndpoints(t *testing.T) {
	_, ts := testServer(t)
	res, _ := http.Post(ts.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"login":"admin","password":"secret-password"}`))
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	do := func(method, path, body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(method, ts.URL+"/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", ts.URL)
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		return res, out
	}
	if res, _ := do(http.MethodPost, "/certificates", `{"names":["*.example.com"]}`); res.StatusCode != 422 {
		t.Fatalf("wildcard must be rejected: %d", res.StatusCode)
	}
	res, out := do(http.MethodPost, "/certificates", `{"names":["Panel.Example.com.","panel.example.com","www.example.com"],"staging":true,"email":"ops@example.com"}`)
	if res.StatusCode != 202 || out["job_id"] == nil {
		t.Fatalf("issue: %d %v", res.StatusCode, out)
	}
	cert := out["certificate"].(map[string]any)
	if cert["name"] != "panel.example.com" || len(cert["names"].([]any)) != 2 || cert["status"] != "pending" || !strings.Contains(cert["directory_url"].(string), "staging") {
		t.Fatalf("certificate record: %v", cert)
	}
	res, list := do(http.MethodGet, "/certificates", "")
	_ = list
	if res.StatusCode != 200 {
		t.Fatalf("list: %d", res.StatusCode)
	}
	res, tlsInfo := do(http.MethodGet, "/web/tls", "")
	if res.StatusCode != 200 || tlsInfo["source"] == nil {
		t.Fatalf("web/tls: %d %v", res.StatusCode, tlsInfo)
	}
}

func TestUICSPHashes(t *testing.T) {
	csp := uiCSP([]byte(`<html><script src="/a.js"></script><script>console.log(1)</script></html>`))
	if !strings.Contains(csp, "script-src 'self' 'sha256-") || strings.Count(csp, "sha256-") != 1 {
		t.Fatalf("csp: %s", csp)
	}
	if strings.Contains(csp, "unsafe-inline'; ") && !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("csp: %s", csp)
	}
}
