package confcheck

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fail2ban-server runs as root and runs its actions through the shell, as
// root, with the jail's values (port, protocol, chain…) put into the
// command lines. A jail file can override any part of an action
// (action = name[actionban=...]), pick another one, or set ignorecommand;
// bantime.formula is Python fail2ban evaluates. The panel writes one jail
// file and one filter: jails with a fixed set of keys and plain values, a
// filter with regular expressions.

const fail2banDir = "/etc/fail2ban"

func isFail2ban(p string) bool { return under(p, fail2banDir) }

func checkFail2ban(c *checker, p string, content []byte) error {
	switch {
	case under(p, fail2banDir+"/jail.d") || p == fail2banDir+"/jail.local":
		return c.checkJails(content)
	case under(p, fail2banDir+"/filter.d"):
		return checkFilter(content)
	}
	return notAllowed("writing "+p, "the panel writes jails in jail.d and filters in filter.d; actions and fail2ban.conf run as root")
}

type cpEntry struct {
	n            int
	section, key string
	value        string
}

var (
	cpSectionRe = regexp.MustCompile(`^\[([^\[\]]+)\]$`)
	cpKeyRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// parseConfigParser reads Python's configparser format the way fail2ban
// does: [section], "key = value" or "key: value", comment lines starting
// with # or ;, and lines indented deeper than their option continuing it.
func parseConfigParser(content []byte) ([]cpEntry, error) {
	lines, err := textLines(content)
	if err != nil {
		return nil, err
	}
	// Python counts a line's indentation, and strips it, with str.isspace:
	// U+00A0 or U+3000 in front of an option make it indented (or not) where
	// the count below says otherwise, and an option such as ignorecommand
	// hides inside the value of the one before. Python's blanks beyond the
	// space and the tab are the control characters textLines refuses and
	// those unicode.IsSpace knows; fail2ban decodes the files as UTF-8.
	for i, raw := range lines {
		if !utf8.ValidString(raw) {
			return nil, atLine(i+1, notAllowed("text that is not UTF-8 in a fail2ban file", "fail2ban reads its files as UTF-8"))
		}
		if j := strings.IndexFunc(raw, func(r rune) bool { return r != ' ' && r != '\t' && unicode.IsSpace(r) }); j >= 0 {
			r, _ := utf8.DecodeRuneInString(raw[j:])
			return nil, atLine(i+1, notAllowed(fmt.Sprintf("the blank %U in a fail2ban file", r), "Python takes it for indentation, which decides what line continues the option above"))
		}
	}
	var out []cpEntry
	section := ""
	indent := -1
	for i, raw := range lines {
		n := i + 1
		s := strings.TrimSpace(raw)
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		cur := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if len(out) > 0 && indent >= 0 && cur > indent {
			out[len(out)-1].value += "\n" + s
			continue
		}
		indent = cur
		if m := cpSectionRe.FindStringSubmatch(s); m != nil {
			section, indent = m[1], -1
			continue
		}
		if strings.HasPrefix(s, "[") {
			return nil, unreadable(n, "bad section %q", s)
		}
		k := strings.IndexAny(s, "=:")
		if k <= 0 {
			return nil, unreadable(n, "not a \"key = value\" line")
		}
		key := strings.ToLower(strings.TrimSpace(s[:k]))
		if !cpKeyRe.MatchString(key) {
			return nil, unreadable(n, "bad key %q", s[:k])
		}
		if section == "" {
			return nil, unreadable(n, "option outside a section")
		}
		out = append(out, cpEntry{n: n, section: section, key: key, value: strings.TrimSpace(s[k+1:])})
	}
	return out, nil
}

// fail2banActions are the ban actions fail2ban ships and the panel may
// name; their definitions are root's, in action.d.
var fail2banActions = map[string]bool{
	"nftables-multiport": true, "nftables-allports": true, "nftables": true, "iptables-multiport": true,
	"iptables-allports": true, "iptables": true, "iptables-new": true, "iptables-ipset": true, "iptables-ipset-proto4": true,
	"iptables-ipset-proto6": true, "iptables-ipset-proto6-allports": true, "firewallcmd-ipset": true,
	"firewallcmd-multiport": true, "firewallcmd-allports": true, "firewallcmd-new": true, "firewallcmd-rich-rules": true,
	"ufw": true,
}

var (
	f2bBoolRe    = regexp.MustCompile(`^(?i:true|false|yes|no|on|off|1|0)$`)
	f2bIntRe     = regexp.MustCompile(`^[0-9]+$`)
	f2bTimeRe    = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?[a-z]*( [0-9]+[a-z]*)*$`)
	f2bPortRe    = regexp.MustCompile(`^[A-Za-z0-9,:-]+( ?, ?[A-Za-z0-9:-]+)*$`)
	f2bNameRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	f2bFilterRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+(\[[A-Za-z0-9_.-]+=[A-Za-z0-9_.-]+(, ?[A-Za-z0-9_.-]+=[A-Za-z0-9_.-]+)*\])?$`)
	f2bMatchRe   = regexp.MustCompile(`^[A-Za-z0-9_=.@+ -]+$`)
	f2bAddrRe    = regexp.MustCompile(`^[0-9A-Fa-f:./a-z-]+$`)
	f2bLogpathRe = regexp.MustCompile(`^/[A-Za-z0-9_./*?\[\]!-]+$`)
)

// jailKeys are what a jail of the panel sets, with the shape of the value;
// logpath is checked against the log trees (logpathAllowed).
var jailKeys = map[string]func(v string) bool{
	"enabled":              f2bBoolRe.MatchString,
	"port":                 f2bPortRe.MatchString,
	"protocol":             regexp.MustCompile(`^(tcp|udp|all|sctp|udplite)$`).MatchString,
	"chain":                f2bNameRe.MatchString,
	"filter":               f2bFilterRe.MatchString,
	"backend":              regexp.MustCompile(`^(auto|systemd|polling|pyinotify|gamin)$`).MatchString,
	"journalmatch":         f2bMatchRe.MatchString,
	"maxretry":             f2bIntRe.MatchString,
	"maxmatches":           f2bIntRe.MatchString,
	"findtime":             f2bTimeRe.MatchString,
	"bantime":              f2bTimeRe.MatchString,
	"bantime.increment":    f2bBoolRe.MatchString,
	"bantime.factor":       regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`).MatchString,
	"bantime.maxtime":      f2bTimeRe.MatchString,
	"bantime.rndtime":      f2bTimeRe.MatchString,
	"bantime.overalljails": f2bBoolRe.MatchString,
	"mode":                 regexp.MustCompile(`^[a-z-]+$`).MatchString,
	// raw hands the actions the text a filter matched instead of an address
	// it resolved; the panel sets none of it.
	"usedns":             regexp.MustCompile(`^(yes|no|warn)$`).MatchString,
	"logencoding":        f2bNameRe.MatchString,
	"ignoreself":         f2bBoolRe.MatchString,
	"banaction":          func(v string) bool { return fail2banActions[v] },
	"banaction_allports": func(v string) bool { return fail2banActions[v] },
	"ignoreip": func(v string) bool {
		for _, a := range strings.Fields(strings.ReplaceAll(v, ",", " ")) {
			if !f2bAddrRe.MatchString(a) {
				return false
			}
		}
		return true
	},
}

// logpathAllowed allows logs in /var/log and the site log tree: fail2ban
// reads them as root, and a filter of the API's could pick any file apart.
func (c *checker) logpathAllowed(v string) bool {
	for _, l := range strings.Split(v, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || len(f) > 2 || !f2bLogpathRe.MatchString(f[0]) || hasDotDot(f[0]) || (len(f) == 2 && f[1] != "head" && f[1] != "tail") {
			return false
		}
		if !underAny(f[0], "/var/log", c.l.siteLogRoot()) {
			return false
		}
	}
	return true
}

func (c *checker) checkJails(content []byte) error {
	entries, err := parseConfigParser(content)
	if err != nil {
		return err
	}
	for _, e := range entries {
		what := fmt.Sprintf("fail2ban [%s] %s", e.section, e.key)
		var err error
		switch lower := strings.ToLower(e.section); {
		case e.section != "DEFAULT" && (!f2bNameRe.MatchString(e.section) || lower == "includes" || lower == "definition" || lower == "init" || lower == "known"):
			err = notAllowed(fmt.Sprintf("fail2ban section [%s]", e.section), "a jail file holds [DEFAULT] and jails")
		case strings.Contains(e.value, "%(") || strings.ContainsAny(e.value, "<>`$"):
			err = notAllowed(fmt.Sprintf("%s = %s", what, e.value), "interpolation and tags put other values into the actions root runs through the shell")
		case e.key == "logpath":
			if !c.logpathAllowed(e.value) {
				err = notAllowed(fmt.Sprintf("%s = %s", what, e.value), "fail2ban reads the logs as root; they are in /var/log and %s", c.l.siteLogRoot())
			}
		case jailKeys[e.key] == nil:
			err = notAllowed(what, "it is not one of the jail settings the panel writes: action, action* overrides, ignorecommand and bantime.formula run commands or code as root")
		case !jailKeys[e.key](e.value):
			err = notAllowed(fmt.Sprintf("%s = %s", what, e.value), "the value goes into commands root runs through the shell, so it must be a plain one")
		}
		if err := atLine(e.n, err); err != nil {
			return err
		}
	}
	return nil
}

var f2bIncludeRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.(conf|local)( +[A-Za-z0-9_.-]+\.(conf|local))*$`)

// filterKeys are what a filter defines; the helper variables of the
// distribution's filters start with an underscore.
var filterKeys = map[string]bool{
	"failregex": true, "ignoreregex": true, "prefregex": true, "datepattern": true, "journalmatch": true, "maxlines": true, "mode": true,
}

func checkFilter(content []byte) error {
	entries, err := parseConfigParser(content)
	if err != nil {
		return err
	}
	for _, e := range entries {
		what := fmt.Sprintf("fail2ban filter [%s] %s", e.section, e.key)
		var err error
		switch section := strings.ToLower(e.section); {
		case section == "includes":
			if (e.key != "before" && e.key != "after") || !f2bIncludeRe.MatchString(e.value) {
				err = notAllowed(fmt.Sprintf("%s = %s", what, e.value), "a filter includes other filters of filter.d by name")
			}
		case section == "definition" || section == "init" || strings.HasPrefix(section, "init?"):
			if !filterKeys[e.key] && !strings.HasPrefix(e.key, "_") {
				err = notAllowed(what, "a filter defines its regular expressions and journal match; commands and actions are not a filter's")
			}
		default:
			err = notAllowed(fmt.Sprintf("fail2ban filter section [%s]", e.section), "a filter holds [INCLUDES], [Definition] and [Init]")
		}
		if err := atLine(e.n, err); err != nil {
			return err
		}
	}
	return nil
}
