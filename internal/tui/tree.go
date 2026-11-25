package tui

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// The TUI offers the commands of the mp tree. These helpers find them by
// path ("site add"), decide which ones a menu may show, and read what a
// command expects from its Use string.

// cmdPath is the command path without the root name: "site add".
func cmdPath(c *cobra.Command) string {
	var parts []string
	for x := c; x != nil && x.HasParent(); x = x.Parent() {
		parts = append(parts, x.Name())
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, " ")
}

// cliOnly are the top-level commands that are not panel features: the
// installer, shell conveniences and the version (the TUI shows it itself).
// Hidden commands (api, agent, helper, fsop, update-run, demo) go too.
func cliOnly(name string) bool {
	switch name {
	case "setup", "version", "completion", "help", "api", "agent", "helper", "fsop", "update-run", "demo":
		return true
	}
	return false
}

// localOnly are the commands that change this machine directly instead of
// going through the API: they make no sense when the TUI talks to another
// server.
func localOnly(path string) bool {
	return path == "update trust" || path == "setup"
}

// localFlag is a flag that bypasses the API (config set --local): hidden in
// remote mode.
func localFlag(name string) bool { return name == "local" }

// offered says whether a menu may show the command at all.
func offered(c *cobra.Command, opts Options) bool {
	if c == nil || !c.HasParent() {
		return false
	}
	for x := c; x.HasParent(); x = x.Parent() {
		if x.Hidden || x.Deprecated != "" {
			return false
		}
		if !x.Parent().HasParent() && cliOnly(x.Name()) {
			return false
		}
	}
	if opts.Remote() && localOnly(cmdPath(c)) {
		return false
	}
	return true
}

// adminOnly are the commands whose operations the API keeps for
// administrators: an ordinary account does not see them, as in the web UI.
// The API decides anyway; this only keeps the menus honest.
func adminOnly(path string) bool {
	switch path {
	case "user add", "user list", "user rm", "user totp-reset", "user share add", "user share entry", "user share rm",
		"site move-ip", "site suspend", "site unsuspend", "site tls",
		"mail install", "mail apply", "mail settings", "mail webmail",
		"db engine tune", "db config", "db config set", "db config unset", "db phpmyadmin install", "db phpmyadmin remove",
		"doctor", "selinux", "logs":
		return true
	}
	top, _, _ := strings.Cut(path, " ")
	switch top {
	case "php", "stack", "ssl", "web", "dns-provider", "firewall", "service", "backup", "migrate", "update", "config", "webhook":
		return true
	}
	return false
}

// adminFlags are flags of otherwise shared commands that only an
// administrator may use: hidden from ordinary accounts.
func adminFlag(path, flag string) bool {
	if flag == "user" {
		// --user always means "on behalf of another account".
		return true
	}
	switch path {
	case "site nginx":
		return flag == "set" || flag == "clear"
	case "site set":
		// A certificate given by hand: the API takes it from administrators.
		return flag == "cert"
	case "user set":
		return flag == "shell" || flag == "sftp-only" || flag == "status"
	case "audit":
		// The API shows an ordinary account its own entries only, whatever
		// it names; the web UI has no actor filter for it either.
		return flag == "actor"
	}
	return false
}

// findCmd walks the tree by command names; nil when the command does not
// exist (mappings to commands of a newer or older mp are skipped).
func findCmd(root *cobra.Command, path string) *cobra.Command {
	c := root
	for _, name := range strings.Fields(path) {
		var next *cobra.Command
		for _, sub := range c.Commands() {
			if sub.Name() == name {
				next = sub
				break
			}
		}
		if next == nil {
			return nil
		}
		c = next
	}
	if c == root {
		return nil
	}
	return c
}

// runnables lists c and its descendants that can run, in tree order, that a
// menu may offer.
func runnables(c *cobra.Command, opts Options) []*cobra.Command {
	var out []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(x *cobra.Command) {
		if !offered(x, opts) {
			return
		}
		if x.Runnable() {
			out = append(out, x)
		}
		subs := append([]*cobra.Command{}, x.Commands()...)
		sort.SliceStable(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
		for _, s := range subs {
			walk(s)
		}
	}
	walk(c)
	return out
}

// argSpec is one positional argument read from a Use string.
type argSpec struct {
	name     string
	required bool
	variadic bool
}

// parseUse reads the positional arguments from a Use string such as
// "add <info@example.com> <dest> [dest...]", "set key=value ..." or
// "add <login> --site <domain>" (the part from the first flag on is not
// about positional arguments).
func parseUse(use string) []argSpec {
	words := strings.Fields(use)
	if len(words) < 2 {
		return nil
	}
	var out []argSpec
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "-") {
			break
		}
		if w == "..." || w == "…" {
			if len(out) > 0 {
				out[len(out)-1].variadic = true
			}
			continue
		}
		a := argSpec{required: true}
		if strings.HasSuffix(w, "...") {
			a.variadic = true
			w = strings.TrimSuffix(w, "...")
		}
		switch {
		case strings.HasPrefix(w, "[") && strings.HasSuffix(w, "]"):
			a.required = false
			w = w[1 : len(w)-1]
		case strings.HasPrefix(w, "<") && strings.HasSuffix(w, ">"):
			w = w[1 : len(w)-1]
		}
		if strings.HasSuffix(w, "...") {
			a.variadic = true
			w = strings.TrimSuffix(w, "...")
		}
		a.name = w
		out = append(out, a)
	}
	return out
}

// argChoices are the values a positional argument takes when they are a
// closed list: cobra's ValidArgs for the first argument, otherwise the ones
// known here. "<id|hostname>" in a Use string names kinds of values, not
// values, so the Use string alone is never trusted for this.
func argChoices(c *cobra.Command, i int) []string {
	if i == 0 && len(c.ValidArgs) > 0 {
		out := make([]string, 0, len(c.ValidArgs))
		for _, v := range c.ValidArgs {
			v, _, _ = strings.Cut(v, "\t")
			out = append(out, v)
		}
		return out
	}
	switch p := cmdPath(c); {
	case p == "selinux" && i == 0:
		return []string{"enforcing", "permissive"}
	case p == "config set" && i == 0:
		return []string{"web.hostname", "web.listen", "log.level", "log.format", "jobs.workers"}
	case p == "stack install" && i == 0:
		return []string{"nginx", "apache", "percona", "mysql", "fail2ban", "memcached", "jpegoptim", "git", "composer", "sphinx", "valkey"}
	case p == "stack remove" && i == 0:
		return []string{"memcached", "jpegoptim", "git", "composer", "sphinx", "valkey"}
	case p == "cms install" && i == 1:
		return []string{"wordpress", "joomla", "opencart", "bitrix"}
	}
	return nil
}

// flagChoices are the values of a string flag that takes one of a closed
// list: known here for the flags whose usage text does not spell the list
// out unambiguously, otherwise read from the usage text.
func flagChoices(path, name, usage, def string) []string {
	switch path + " --" + name {
	case "site set --ssl":
		return []string{"auto", "none"}
	case "user set --status":
		return []string{"active", "suspended"}
	case "update settings --channel":
		return []string{"stable", "beta"}
	case "job list --status":
		return []string{"queued", "running", "done", "failed"}
	case "backup target add --schedule":
		return []string{"daily"}
	}
	return choicesFromUsage(usage, def)
}

// choicesFromUsage reads a closed list of values from a usage text such as
// "fpm (nginx → php-fpm), apache (…) or proxy (…)", "true|false — …" or
// "the panel to move from: monopanel | fastpanel | bitrixvm". Every value
// must look like a value (lower-case word); a list of two joined only by
// "or" counts only after a "label:" or when the default is one of them, so
// "port or range" stays free text.
func choicesFromUsage(usage, def string) []string {
	s := stripParens(usage)
	if i := strings.Index(s, " — "); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	labelled := false
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s, labelled = s[i+2:], true
	}
	s = strings.TrimSpace(s)
	var parts []string
	separated := false
	if strings.Contains(s, "|") {
		parts, separated = strings.Split(s, "|"), true
	} else {
		separated = strings.Contains(s, ",")
		// The usage text is in the language of the terminal.
		s = strings.ReplaceAll(s, T(" или ", " or "), ",")
		s = strings.ReplaceAll(s, " or ", ",")
		parts = strings.Split(s, ",")
	}
	if len(parts) < 2 {
		return nil
	}
	out := make([]string, 0, len(parts))
	hasDef := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !valueLike(p) {
			return nil
		}
		if p == def {
			hasDef = true
		}
		out = append(out, p)
	}
	if !separated && !labelled && !hasDef {
		return nil
	}
	return out
}

func valueLike(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '-' || r == '_' || r == '.'):
		default:
			return false
		}
	}
	return true
}

func stripParens(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
