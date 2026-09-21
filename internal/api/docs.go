package api

import (
	"embed"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Stoplight Elements, вшитый в бинарник (см. elements/README.md).
//
//go:embed elements/web-components.min.js elements/styles.min.css
var elementsFS embed.FS

// docsCSP: только свой origin. 'unsafe-inline' для стилей нужен самому
// Elements (он расставляет style-атрибуты); скриптам inline и eval закрыты.
const docsCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func docsHTML(base string) string {
	return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="referrer" content="no-referrer">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>MonoPanel API Reference</title>
    <link rel="stylesheet" href="` + base + `/docs/elements.css">
    <script src="` + base + `/docs/elements.js"></script>
  </head>
  <body style="height: 100vh; margin: 0;">
    <elements-api apiDescriptionUrl="` + base + `/openapi.yaml" router="hash" layout="sidebar" tryItCredentialsPolicy="same-origin"></elements-api>
  </body>
</html>
`
}

// mountDocs serves the API reference from the panel itself: the page, the
// Elements script and its stylesheet, each with the docs CSP.
func mountDocs(r chi.Router, base string) {
	st := &staticServer{}
	read := func(name string) []byte {
		b, err := elementsFS.ReadFile("elements/" + name)
		if err != nil {
			panic(err) // go:embed guarantees the file; a miss is a build error
		}
		return b
	}
	files := map[string]*staticFile{
		"/docs":              st.prepare("docs.html", []byte(docsHTML(base))),
		"/docs/elements.js":  st.prepare("elements.js", read("web-components.min.js")),
		"/docs/elements.css": st.prepare("elements.css", read("styles.min.css")),
	}
	for p, f := range files {
		h := func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Security-Policy", docsCSP)
			f.serve(w, req)
		}
		r.Get(p, h)
		r.Head(p, h)
	}
}

// docsGate closes the API reference, the OpenAPI specification and the JSON
// schemas to anyone who is not signed in: a map of every operation is of no
// use to a visitor and of some use to a scanner. Any account may read it, a
// session or an API token both work; a migration token may not — it exists
// to read one account and nothing else.
func (s *Server) docsGate(base string) func(http.Handler) http.Handler {
	gated := func(p string) bool {
		p = strings.TrimPrefix(p, base)
		return p == "/docs" || strings.HasPrefix(p, "/docs/") || strings.HasPrefix(p, "/openapi") || strings.HasPrefix(p, "/schemas/")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !gated(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			p := s.authenticateRequest(r)
			switch {
			case p == nil && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/docs") && strings.Contains(r.Header.Get("Accept"), "text/html"):
				// A person following the link: the panel's sign-in page.
				http.Redirect(w, r, "/", http.StatusFound)
			case p == nil:
				docsRefuse(w, http.StatusUnauthorized, "authentication required")
			case hasMigrationScope(p.Scopes):
				docsRefuse(w, http.StatusForbidden, "token scope does not allow this operation")
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

func docsRefuse(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(`{"title":"` + http.StatusText(status) + `","status":` + strconv.Itoa(status) + `,"detail":"` + detail + `"}`)) //nolint:errcheck // a closed client is not our problem
}
