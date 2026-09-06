// Package buildinfo carries version information injected at build time via -ldflags.
package buildinfo

import "runtime"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable version line.
func String() string {
	return Version + " (" + Commit + ", " + Date + ", " + runtime.Version() + ")"
}
