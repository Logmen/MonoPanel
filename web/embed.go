// Package web embeds the compiled Web UI (SvelteKit static build in build/).
// Until the SvelteKit app is built, build/index.html is a minimal placeholder
// that exercises the API (health, login, status).
package web

import "embed"

// FS contains build/.
//
//go:embed all:build
var FS embed.FS
