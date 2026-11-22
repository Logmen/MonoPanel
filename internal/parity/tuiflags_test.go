package parity

import "monopanel/internal/tui"

// tuiBoundFlags is the TUI's form of a command for a role: the flags it
// offers there, a secret piped to stdin named by its -stdin flag. The
// tui-flags check compares it with the flags only administrators may use.
var tuiBoundFlags = tui.BoundFlags

// tuiFlagsMissing is why the tui-flags check is skipped while tuiBoundFlags
// is not wired.
const tuiFlagsMissing = "internal/tui does not export BoundFlags (the flags of a command's form for a role) yet: wire it in tuiflags_test.go"
