package tui

import "github.com/spf13/cobra"

// BoundFlags lists the flags of the form the menus open for a command (its
// path as BoundCommands gives it) for the role: what the area's "all
// commands" list shows. A secret piped to stdin is named by its -stdin
// flag. The parity check (internal/parity) compares the list with the flags
// only administrators may use.
func BoundFlags(root *cobra.Command, opts Options, role, path string) []string {
	c := findCmd(root, path)
	if c == nil || !offered(c, opts) {
		return nil
	}
	f := newForm(c, formEnv{remote: opts.Remote(), admin: role == "admin"}, &act{path: path}, nil)
	var out []string
	for _, fd := range f.fields {
		switch {
		case fd.arg:
		case fd.stdin != "":
			out = append(out, fd.stdin)
		default:
			out = append(out, fd.name)
		}
	}
	return out
}
