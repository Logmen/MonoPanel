package api

import (
	"embed"
	"net/http"

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
		r.Get(p, func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Security-Policy", docsCSP)
			f.serve(w, req)
		})
	}
}
