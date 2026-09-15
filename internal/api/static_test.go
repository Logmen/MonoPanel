package api

import (
	"compress/gzip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"testing/fstest"

	"monopanel/web"
)

func staticFixture() *staticServer {
	big := strings.Repeat("function a(){return 'chunk'};\n", 400)
	index := "<!doctype html><script>boot()</script>" + strings.Repeat("<p>pad</p>\n", 200)
	return newStaticServer(fstest.MapFS{
		"index.html":                        {Data: []byte(index)},
		"_app/version.json":                 {Data: []byte(`{"version":"` + strings.Repeat("1", 1100) + `"}`)},
		"_app/immutable/chunks/C8FFAw1T.js": {Data: []byte(big)},
		"_app/immutable/assets/font.woff2":  {Data: []byte("wOF2" + strings.Repeat("\x00\x01", 2000))},
		"monaco/vs/editor-KLE6jdfb.js":      {Data: []byte(big)},
		"monaco/vs/loader.js":               {Data: []byte(big)},
		"monaco/vs/nls.messages-loader.js":  {Data: []byte(big)},
		"monaco/vs/editor/editor.main.css":  {Data: []byte(strings.Repeat(".a{color:red}\n", 300))},
		"favicon.png":                       {Data: []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 3000))},
		"robots.txt":                        {Data: []byte("User-agent: *\n")},
	})
}

func staticGet(t *testing.T, st *staticServer, method, p string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, p, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	return rec
}

func gunzip(t *testing.T, body io.Reader) string {
	t.Helper()
	zr, err := gzip.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	return string(plain)
}

// Text assets go out gzipped when the client accepts it and identical to the
// source otherwise; fonts and images are never recompressed.
func TestStaticCompression(t *testing.T) {
	st := staticFixture()
	rec := staticGet(t, st, http.MethodGet, "/monaco/vs/editor-KLE6jdfb.js", map[string]string{"Accept-Encoding": "gzip, br"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip: %d %v", rec.Code, rec.Header())
	}
	plain := gunzip(t, rec.Body)
	if !strings.HasPrefix(plain, "function a()") || rec.Body.Len() >= len(plain)/2 {
		t.Fatalf("gzip body: %d bytes for %d plain", rec.Body.Len(), len(plain))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("content type: %s", ct)
	}
	rec = staticGet(t, st, http.MethodGet, "/monaco/vs/editor-KLE6jdfb.js", nil)
	if rec.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(rec.Body.String(), "function a()") || rec.Header().Get("Content-Length") != "12000" {
		t.Fatalf("identity: %v len %s", rec.Header(), rec.Header().Get("Content-Length"))
	}
	for p, ct := range map[string]string{"/monaco/vs/editor/editor.main.css": "text/css", "/_app/version.json": "application/json"} {
		rec = staticGet(t, st, http.MethodGet, p, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Header().Get("Content-Encoding") != "gzip" || !strings.HasPrefix(rec.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %v", p, rec.Header())
		}
	}
	for _, p := range []string{"/_app/immutable/assets/font.woff2", "/favicon.png", "/robots.txt"} {
		rec = staticGet(t, st, http.MethodGet, p, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Vary") != "" {
			t.Fatalf("%s must not be compressed: %v", p, rec.Header())
		}
	}
	// Accept-Encoding is parsed, not substring-matched.
	for hdr, want := range map[string]bool{"gzip": true, "GZIP": true, "br, gzip;q=0.5": true, "*": true, "gzip;q=0, identity": false, "identity;q=1, gzip;q=0": false, "br": false, "": false, "*;q=0": false} {
		if got := acceptsGzip(hdr); got != want {
			t.Errorf("acceptsGzip(%q) = %v", hdr, got)
		}
	}
}

// Hashed assets are cached for a year, everything else revalidates through
// the ETag and gets a 304 when unchanged.
func TestStaticCachingHeaders(t *testing.T) {
	st := staticFixture()
	for p, want := range map[string]string{
		"/_app/immutable/chunks/C8FFAw1T.js": "public, max-age=31536000, immutable",
		"/monaco/vs/editor-KLE6jdfb.js":      "public, max-age=31536000, immutable",
		"/monaco/vs/loader.js":               "no-cache",
		"/monaco/vs/nls.messages-loader.js":  "no-cache",
		"/monaco/vs/editor/editor.main.css":  "no-cache",
		"/_app/version.json":                 "no-cache",
		"/":                                  "no-cache",
	} {
		rec := staticGet(t, st, http.MethodGet, p, nil)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != want {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	rec := staticGet(t, st, http.MethodGet, "/monaco/vs/loader.js", nil)
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("etag: %q", etag)
	}
	for _, inm := range []string{etag, strings.TrimPrefix(etag, "W/"), `W/"other", ` + etag, "*"} {
		rec = staticGet(t, st, http.MethodGet, "/monaco/vs/loader.js", map[string]string{"If-None-Match": inm, "Accept-Encoding": "gzip"})
		h := rec.Header()
		if rec.Code != 304 || rec.Body.Len() != 0 || h.Get("ETag") != etag || h.Get("Cache-Control") != "no-cache" || h.Get("Vary") != "Accept-Encoding" || h.Get("Content-Encoding") != "" || h.Get("Content-Length") != "" {
			t.Fatalf("304 for %q: %d body %d %v", inm, rec.Code, rec.Body.Len(), h)
		}
	}
	rec = staticGet(t, st, http.MethodGet, "/monaco/vs/loader.js", map[string]string{"If-None-Match": `"stale"`})
	if rec.Code != 200 {
		t.Fatalf("stale etag: %d", rec.Code)
	}
	rec = staticGet(t, st, http.MethodGet, "/monaco/vs/editor/editor.main.css", map[string]string{"If-None-Match": etag})
	if rec.Code != 200 {
		t.Fatalf("etag of another file must not match: %d", rec.Code)
	}
}

// Unknown app routes are the SPA shell (gzipped like any other text) so the
// client router can take over; missing assets and stray /api paths are 404;
// HEAD carries the headers but no body; other methods are refused.
func TestStaticFallbackAndMethods(t *testing.T) {
	st := staticFixture()
	for _, p := range []string{"/", "/sites/example.com", "/index.html", "/some/spa/route/", "/../index.html"} {
		rec := staticGet(t, st, http.MethodGet, p, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(gunzip(t, rec.Body), "boot()") {
			t.Errorf("%s: %d %v", p, rec.Code, rec.Header())
		}
	}
	shell := staticGet(t, st, http.MethodGet, "/", nil)
	if staticGet(t, st, http.MethodGet, "/sites/x", nil).Header().Get("ETag") != shell.Header().Get("ETag") {
		t.Fatal("the fallback must share the shell's ETag")
	}
	for _, p := range []string{"/_app/immutable/chunks/GONE.js", "/monaco/vs/nothing.js", "/monaco/vs/", "/api/v2/x", "/api/health", "/monaco/vs/loader.js/"} {
		if rec := staticGet(t, st, http.MethodGet, p, nil); rec.Code != 404 {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	for _, ae := range []string{"gzip", ""} {
		head := staticGet(t, st, http.MethodHead, "/monaco/vs/loader.js", map[string]string{"Accept-Encoding": ae})
		get := staticGet(t, st, http.MethodGet, "/monaco/vs/loader.js", map[string]string{"Accept-Encoding": ae})
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") || head.Header().Get("Content-Encoding") != get.Header().Get("Content-Encoding") {
			t.Fatalf("head (%q): %d %v vs get %v", ae, head.Code, head.Header(), get.Header())
		}
	}
	if rec := staticGet(t, st, http.MethodPost, "/monaco/vs/loader.js", nil); rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("post: %d %v", rec.Code, rec.Header())
	}
}

// The immutable rule is pinned against the real names the build produces:
// only content-hashed files may be cached for a year.
func TestStaticImmutableRealNames(t *testing.T) {
	for p, want := range map[string]bool{
		"_app/immutable/chunks/C8FFAw1T.js":                              true,
		"_app/immutable/assets/0.CtAQq4fZ.css":                           true,
		"_app/immutable/assets/exo-2-cyrillic-400-normal.DeqPVnRv.woff2": true,
		"monaco/vs/editor-KLE6jdfb.js":                                   true,
		"monaco/vs/index-049xrXrp.js":                                    true,
		"monaco/vs/assets/editor.worker-lj3bdIIn.js":                     true,
		"monaco/vs/monaco.contribution-9cKT3C7t.js":                      true,
		"_app/version.json":                                              false,
		"index.html":                                                     false,
		"monaco/vs/loader.js":                                            false,
		"monaco/vs/editor.js":                                            false,
		"monaco/vs/editor/editor.main.js":                                false,
		"monaco/vs/editor/editor.main.css":                               false,
		"monaco/vs/nls.messages-loader.js":                               false,
		"monaco/vs/editor/editor.main.nls-de.js":                         false,
		"monaco/vs/basic-languages/monaco.contribution.js":               false,
		"monaco/README.md":                                               false,
	} {
		if got := immutableRe.MatchString(p); got != want {
			t.Errorf("immutable(%s) = %v", p, got)
		}
	}
	for ext, want := range map[string]bool{".js": true, ".css": true, ".json": true, ".html": true, ".svg": true, ".woff2": false, ".woff": false, ".png": false, ".ttf": false} {
		if got := compressible(mime.TypeByExtension(ext)); got != want {
			t.Errorf("compressible(%s → %s) = %v", ext, mime.TypeByExtension(ext), got)
		}
	}
	// With a real build embedded, every immutable file must be under one of
	// the two hashed schemes and every Monaco entry point must revalidate.
	sub, _ := fs.Sub(web.FS, "build")
	if _, err := fs.Stat(sub, "monaco/vs/loader.js"); err != nil {
		t.Skip("placeholder build embedded; run make web for the full check")
	}
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // walk of an embed FS
		if err != nil || d.IsDir() {
			return nil
		}
		if immutableRe.MatchString(p) && !strings.HasPrefix(p, "_app/immutable/") && !(strings.HasPrefix(p, "monaco/vs/") && path.Ext(p) == ".js") {
			t.Errorf("%s marked immutable outside the hashed schemes", p)
		}
		return nil
	})
	for _, p := range []string{"index.html", "_app/version.json", "monaco/vs/loader.js", "monaco/vs/editor/editor.main.js", "monaco/vs/editor/editor.main.css"} {
		if immutableRe.MatchString(p) {
			t.Errorf("%s must revalidate", p)
		}
		if _, err := fs.Stat(sub, p); err != nil {
			t.Errorf("%s missing from the build", p)
		}
	}
}
