// Command monopanel is the single MonoPanel binary. It runs as the API server
// (monopanel api), the privileged agent (monopanel agent), the unprivileged
// file helper (monopanel helper) and as the CLI/TUI client (mp ...).
package main

import (
	"os"

	"monopanel/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
