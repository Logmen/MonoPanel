package api

import (
	"crypto/sha512"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The API reference is served by the panel alone: no third-party origin in
// the page, a CSP that allows only 'self', and the vendored Elements files
// pinned by the hashes of the npm release they came from.
func TestDocsAreSelfHosted(t *testing.T) {
	_, ts := testServer(t)
	get := func(p string) (*http.Response, string) {
		t.Helper()
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close() //nolint:errcheck // test
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	res, page := get("/api/v1/docs")
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
		res, body := get(p)
		sum := sha512.Sum384([]byte(body))
		if got := base64.StdEncoding.EncodeToString(sum[:]); res.StatusCode != 200 || got != sri {
			t.Errorf("%s: %d sha384-%s, want the @stoplight/elements@9.0.15 release", p, res.StatusCode, got)
		}
	}
	for _, p := range []string{"/api/v1/docs", "/api/v1/docs/elements.js", "/api/v1/docs/elements.css"} {
		res, _ := get(p)
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http") {
			t.Errorf("%s CSP: %q", p, csp)
		}
		if res.Header.Get("ETag") == "" || res.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s caching: %v", p, res.Header)
		}
	}
	if res, _ := get("/api/v1/openapi.yaml"); res.StatusCode != 200 {
		t.Fatalf("spec: %d", res.StatusCode)
	}
}
