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
	// font-src с data: — шрифт иконок редактора вшит в его CSS как data-URI;
	// worker-src с blob: — Monaco запускает свой рабочий поток через Blob,
	// который importScripts-ом тянет хешированный чанк с нашего origin.
	return "default-src 'self'; script-src " + script + "; worker-src 'self' blob:; img-src 'self' data:; font-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'"
}

// uiHandler serves the embedded SPA with an index.html fallback; see
// staticServer for compression and caching.
func (s *Server) uiHandler() http.Handler {
	sub, err := fs.Sub(web.FS, "build")
	if err != nil {
		return http.NotFoundHandler()
	}
	return newStaticServer(sub)
}

// uiCSPFromEmbed computes the CSP once from the embedded index.html.
func uiCSPFromEmbed() string {
	index, err := fs.ReadFile(web.FS, "build/index.html")
	if err != nil {
		return uiCSP(nil)
	}
	return uiCSP(index)
}
