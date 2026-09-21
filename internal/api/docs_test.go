package api

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"monopanel/internal/auth"
	"monopanel/internal/store"
)

type docsClient struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
	bearer string
	accept string
}

func (c docsClient) get(p string) (*http.Response, string) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.base+p, nil)
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.accept != "" {
		req.Header.Set("Accept", c.accept)
	}
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck // test
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func docsLogin(t *testing.T, base string) *http.Cookie {
	t.Helper()
	res, err := http.Post(base+"/api/v1/auth/login", "application/json", strings.NewReader(`{"login":"admin","password":"secret-password"}`))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("login: %v %v", res, err)
	}
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

// The API reference is served by the panel alone: no third-party origin in
// the page, a CSP that allows only 'self', and the vendored Elements files
// pinned by the hashes of the npm release they came from.
func TestDocsAreSelfHosted(t *testing.T) {
	_, ts := testServer(t)
	c := docsClient{t: t, base: ts.URL, cookie: docsLogin(t, ts.URL)}
	res, page := c.get("/api/v1/docs")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("docs page: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if strings.Contains(page, "unpkg.com") || strings.Contains(page, "://") {
		t.Fatalf("docs page references another origin:\n%s", page)
	}
	for _, want := range []string{`src="/api/v1/docs/elements.js"`, `href="/api/v1/docs/elements.css"`, `apiDescriptionUrl="/api/v1/openapi.yaml"`} {
		if !strings.Contains(page, want) {
			t.Errorf("docs page lacks %s", want)
		}
	}
	for p, sri := range map[string]string{
		"/api/v1/docs/elements.js":  "xjOcq9PZ/k+pGtPS/xcsCRXGjKKfTlIa4H1IYEnC+97jNa6sAMWTNrV6hY08W3GL",
		"/api/v1/docs/elements.css": "iVQBHadsD+eV0M5+ubRCEVXrXEBj+BqcuwjUwPoVJc0Pb1fmrhYSAhL+BFProHdV",
	} {
		res, body := c.get(p)
		sum := sha512.Sum384([]byte(body))
		if got := base64.StdEncoding.EncodeToString(sum[:]); res.StatusCode != 200 || got != sri {
			t.Errorf("%s: %d sha384-%s, want the @stoplight/elements@9.0.15 release", p, res.StatusCode, got)
		}
	}
	for _, p := range []string{"/api/v1/docs", "/api/v1/docs/elements.js", "/api/v1/docs/elements.css"} {
		res, _ := c.get(p)
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http") {
			t.Errorf("%s CSP: %q", p, csp)
		}
		if res.Header.Get("ETag") == "" || res.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s caching: %v", p, res.Header)
		}
	}
}

// The reference, the specification and the schemas are for signed-in
// accounts only: a session or an API token opens them, nothing else does,
// and a migration token stays confined to /migrate. Health and login, which
// a visitor needs, stay open.
func TestDocsRequireAuthentication(t *testing.T) {
	s, ts := testServer(t)
	gated := []string{"/api/v1/docs", "/api/v1/docs/elements.js", "/api/v1/docs/elements.css", "/api/v1/openapi.json", "/api/v1/openapi.yaml", "/api/v1/openapi-3.0.json", "/api/v1/openapi-3.0.yaml", "/api/v1/schemas/Health.json"}

	anon := docsClient{t: t, base: ts.URL}
	for _, p := range gated {
		res, body := anon.get(p)
		if res.StatusCode != 401 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/problem+json") || strings.Contains(body, "paths") || res.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("anonymous %s: %d %s", p, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	// A person who follows the link lands on the sign-in page instead.
	browser := docsClient{t: t, base: ts.URL, accept: "text/html,application/xhtml+xml"}
	if res, _ := browser.get("/api/v1/docs"); res.StatusCode != 302 || res.Header.Get("Location") != "/" {
		t.Errorf("browser without a session: %d → %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := browser.get("/api/v1/openapi.json"); res.StatusCode != 401 {
		t.Errorf("the spec never redirects: %d", res.StatusCode)
	}
	bad := docsClient{t: t, base: ts.URL, bearer: "nope"}
	if res, _ := bad.get("/api/v1/openapi.json"); res.StatusCode != 401 {
		t.Errorf("bad token: %d", res.StatusCode)
	}
	for _, p := range []string{"/api/v1/health"} {
		if res, _ := anon.get(p); res.StatusCode != 200 {
			t.Errorf("%s must stay open: %d", p, res.StatusCode)
		}
	}

	ctx := context.Background()
	admin, err := s.db.GetUserByLogin(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	mint := func(name string, scopes []string) string {
		tok := "test-token-" + name
		if err := s.db.CreateAPIToken(ctx, &store.APIToken{UserID: admin.ID, Name: name, Hash: auth.HashToken(tok), Scopes: scopes}); err != nil {
			t.Fatal(err)
		}
		return tok
	}
	for name, c := range map[string]docsClient{
		"session": {t: t, base: ts.URL, cookie: docsLogin(t, ts.URL)},
		"token":   {t: t, base: ts.URL, bearer: mint("plain", nil)},
	} {
		for _, p := range gated {
			if res, _ := c.get(p); res.StatusCode != 200 {
				t.Errorf("%s %s: %d", name, p, res.StatusCode)
			}
		}
	}
	mig := docsClient{t: t, base: ts.URL, bearer: mint("move", []string{migrateScopePrefix + "alex"})}
	for _, p := range gated {
		if res, _ := mig.get(p); res.StatusCode != 403 {
			t.Errorf("migration token %s: %d", p, res.StatusCode)
		}
	}
}
