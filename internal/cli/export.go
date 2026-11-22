package cli

import "github.com/spf13/cobra"

// NewRoot builds the whole command tree without running it. The TUI walks it
// to offer every command as a form, and the parity check (internal/parity)
// compares it with the API and the web UI.
func NewRoot() *cobra.Command { return newRoot() }
