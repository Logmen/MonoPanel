package tui

import (
	"sort"

	"github.com/spf13/cobra"
)

// BoundCommands lists the command paths (space-joined, e.g. "site add") that
// the menus reach for the role, not counting the command palette. The parity
// check (internal/parity) compares it with the whole command tree.
//
// Every area offers all commands of its groups (its "all commands" list,
// also in the help of each of its screens), and the commands of groups no
// area owns are in "Other": so this is the union of those lists for the
// areas the role sees.
func BoundCommands(root *cobra.Command, opts Options, role string) []string {
	admin := role == "admin"
	seen := map[string]bool{}
	areas := append(areaDefs(admin), otherArea())
	for _, a := range areas {
		if !a.visible(admin) {
			continue
		}
		for _, c := range commandsOf(root, opts, a, admin) {
			seen[cmdPath(c)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
