// Package buildinfo carries version information injected at build time via -ldflags.
package buildinfo

import "runtime"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
	// Repo is the GitHub repository (owner/name) whose releases this build
	// updates from, set by the packaging; a hand-built binary has none.
	Repo = ""
)

// String returns a human-readable version line.
func String() string {
	return Version + " (" + Commit + ", " + Date + ", " + runtime.Version() + ")"
}
