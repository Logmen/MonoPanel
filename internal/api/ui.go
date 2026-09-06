package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	"monopanel/web"
)

var inlineScriptRe = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)

// uiCSP builds the Content-Security-Policy for the SPA: SvelteKit boots from
// an inline script, so its hash is allowed explicitly instead of 'unsafe-inline'.
func uiCSP(index []byte) string {
	hashes := []string{}
	for _, m := range inlineScriptRe.FindAllSubmatch(index, -1) {
		if strings.Contains(string(m[1]), "src=") {
			continue
		}
		sum := sha256.Sum256(m[2])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	script := "'self'"
	if len(hashes) > 0 {
		script += " " + strings.Join(hashes, " ")
	}
	return "default-src 'self'; script-src " + script + "; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'"
}

// uiHandler serves the embedded SPA with an index.html fallback.
func (s *Server) uiHandler() http.Handler {
	sub, err := fs.Sub(web.FS, "build")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServerFS(sub)
	index, _ := fs.ReadFile(sub, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" && !strings.HasSuffix(p, "/") {
			if f, err := sub.Open(p); err == nil {
				f.Close()
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}

// uiCSPFromEmbed computes the CSP once from the embedded index.html.
func uiCSPFromEmbed() string {
	index, err := fs.ReadFile(web.FS, "build/index.html")
	if err != nil {
		return uiCSP(nil)
	}
	return uiCSP(index)
}
