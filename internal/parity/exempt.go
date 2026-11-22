package parity

// The mp commands that reach no API operation, and why. Everything else mp
// offers is in the manifest, so the web UI and the TUI offer it as well.

// cliOnly are the top-level commands (with everything under them) that are no
// panel feature: installation, the daemon roles of the binary and shell
// conveniences. Neither the web UI nor the TUI offers them. The hidden ones
// are listed so that the list is complete; help and completion are the ones
// cobra adds when mp runs.
var cliOnly = map[string]string{
	"setup":      "installs the panel: runs once as root, before there is an API",
	"version":    "a shell convenience; the web UI and the TUI show the version themselves",
	"completion": "shell completion scripts for the local terminal (added by cobra, with one subcommand per shell)",
	"help":       "help texts (added by cobra); the TUI shows the help of every command it offers",
	"api":        "daemon role: the unprivileged API process (hidden)",
	"agent":      "daemon role: the privileged agent (hidden)",
	"helper":     "internal: runs a command as an account for the agent (hidden)",
	"fsop":       "internal: the file operations the agent runs as an account (hidden)",
	"update-run": "internal: the updater's restart step (hidden)",
	"demo":       "the live demo build only (hidden)",
}

// localOnly are commands that change this machine without the API, as root
// on the server. The TUI offers them when it runs there, never when it talks
// to another panel (--server).
var localOnly = map[string]string{
	"update trust": "the release key vouches for packages installed as root, so the panel process must not be able to change it; the web UI shows key_pinned and key_source read-only",
}

// localFlags are flags that bypass the API: the recovery path when the API
// cannot start (a bad web.listen, say). The normal path goes through the API
// and is in the web UI too. The TUI hides them when it talks to another panel.
var localFlags = map[string]string{
	"config set --local":  "writes config.yaml directly as root when the API cannot start",
	"config show --local": "reads config.yaml directly as root when the API cannot start",
}
