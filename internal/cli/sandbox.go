package cli

import (
	"github.com/spf13/cobra"

	"monopanel/internal/sandbox"
)

// sandboxExecCmd is how the agent runs restic in a Landlock sandbox
// (internal/sandbox): its own arguments are the trees the program may read,
// write and execute, and after "--" the program and its arguments, which
// are passed on as they are — so cobra parses none of them.
func sandboxExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "sandbox-exec [--ro|--rw|--rx <path>]… -- <program> [args…]",
		Short:              T("служебная: запуск программы в песочнице Landlock (вызывается агентом)", "internal: run a program in a Landlock sandbox (called by the agent)"),
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			// Main returns only when the program did not start; it has said why.
			return &exitError{code: sandbox.Main(args)}
		},
	}
}
