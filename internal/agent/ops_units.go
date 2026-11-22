package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"monopanel/internal/osprofile"
)

// The panel's systemd units. A unit runs whatever it says, as whoever it
// says — User=root, ExecStart=+…, an EnvironmentFile PID 1 reads as root, a
// bind mount over /etc/cron.d — so /etc/systemd/system is not on the write
// allow-list, and /v1/units/apply and /v1/units/remove write and remove the
// units the panel makes, and only those:
//
//	monopanel-app-<login>-<name>.service        an account's app (systemd/app.service.tmpl)
//	monopanel-valkey-<login>-<purpose>.service  an account's Valkey (systemd/valkey.service.tmpl)
//	monopanel-sphinx.service                    Sphinx 3 on EL (sphinx/monopanel-sphinx.service.tmpl)
//	monopanel-firewall.service                  the panel's nftables table at boot
//	<nginx unit>.d/monopanel.conf               restart-on-failure for nginx
//
// No mount unit: a shared folder is bound by the agent itself
// (sharebind.go). A unit names its folder by path, and systemd mounts it by
// that path at boot — through whatever symlink the folder's owner has put
// there by then. The mount units of the shared folders a panel up to 0.8.20
// wrote, <escaped path>.mount, are only removed (/v1/units/remove, and as
// the agent starts: retireUnsafeShareUnits).
//
// The content is read the way systemd reads unit files (systemd.syntax(7))
// and must hold exactly the sections and keys of its kind, with the values
// checked: an account's unit runs as a panel account (uid >= UID_MIN, its home
// below the web root, none of the privileged groups) and starts a command
// line that begins with an absolute path — none of the prefixes (+, !, !!)
// that lift User= and the sandbox; the units of root's and of system
// services carry their exact command lines. daemon-reload stays the service
// call's (/v1/service).

// systemUnitDir is where the units go.
const systemUnitDir = "/etc/systemd/system"

// maxUnitSize bounds a unit file; the panel's are a few hundred bytes.
const maxUnitSize = 64 << 10

// appEnvDir holds the apps' root-only environment files (the API's
// appEnvDir): the only EnvironmentFile= an app unit may name, the one of
// its own unit.
const appEnvDir = "/etc/monopanel/apps"

// UnitHost is what the unit checks need to know about the host.
type UnitHost struct {
	// WWWRoot is the web root: the accounts' homes and the shared folders.
	WWWRoot string
	// NginxService is nginx's unit, whose drop-in the panel keeps.
	NginxService string
	// Sphinx is the search daemon's layout; the panel writes its unit only
	// where UnitFile is set (EL).
	Sphinx osprofile.SphinxLayout
	// ValkeyBinaries are the servers an account's Valkey unit may start.
	ValkeyBinaries []string
	// Account looks up the panel account an account's unit runs as and says
	// why login is not one.
	Account func(login string) (*UnitAccount, error)
	// SystemUser checks the system account a system service of the panel's
	// runs as (the Sphinx user).
	SystemUser func(login string) error
}

// UnitAccount is a panel account a unit may run as.
type UnitAccount struct {
	Login string
	Home  string
	// Group is the name of its primary group.
	Group string
	// UID is its uid: a shared folder is a directory of its site owner's
	// (openShareSource).
	UID uint32
}

// unitEntry is one assignment of a unit file.
type unitEntry struct {
	section, key, value string
	line                int
}

var (
	unitSectionRe = regexp.MustCompile(`^\[[A-Za-z]+\]$`)
	unitKeyRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
)

// unitRepeatable are the keys a unit may set more than once.
var unitRepeatable = map[string]bool{"Environment": true}

func unitComment(trimmed string) bool {
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";")
}

// escapedEnd reports whether line ends in a backslash that escapes the line
// break — systemd's own test: a backslash escapes the character after it, so
// \\ at the end is a backslash and no continuation.
func escapedEnd(line string) bool {
	escaped := false
	for i := 0; i < len(line); i++ {
		switch {
		case escaped:
			escaped = false
		case line[i] == '\\':
			escaped = true
		}
	}
	return escaped
}

// parseUnit reads a unit file as systemd does — lines ending in a backslash
// continue on the next one, the backslash read as a blank, "#" and ";" start
// comments, a key runs up to the first "=", key and value lose the blanks
// around them — and refuses what systemd would read differently from a
// plain look at the file: a carriage return or NUL (systemd ends a line
// there), a byte order mark, a comment line ending in a backslash (systemd
// would swallow the next line into it), a comment inside a continued line, a
// line that is no section, comment or assignment (.include and its kind), an
// assignment outside a section, a section or a key given twice, an empty
// value (it resets a setting) and control characters in values.
func parseUnit(content string) ([]unitEntry, error) {
	switch {
	case len(content) > maxUnitSize:
		return nil, fmt.Errorf("more than %d bytes", maxUnitSize)
	case !utf8.ValidString(content):
		return nil, errors.New("not UTF-8")
	case strings.ContainsAny(content, "\x00\r"):
		return nil, errors.New("a carriage return or NUL: systemd ends a line there")
	case strings.HasPrefix(content, "\uFEFF"):
		return nil, errors.New("a byte order mark")
	}
	var (
		entries  []unitEntry
		section  string
		sections = map[string]bool{}
		keys     = map[string]bool{}
		cont     strings.Builder
		inCont   bool
		first    int
	)
	for i, raw := range strings.Split(content, "\n") {
		n := i + 1
		trimmed := strings.TrimLeft(raw, " \t")
		if inCont {
			if unitComment(trimmed) {
				return nil, fmt.Errorf("line %d: a comment inside a continued line", n)
			}
			cont.WriteString(raw)
		} else {
			if trimmed == "" {
				continue
			}
			cont.Reset()
			cont.WriteString(raw)
			first = n
		}
		line := cont.String()
		if escapedEnd(line) {
			if unitComment(strings.TrimLeft(line, " \t")) {
				return nil, fmt.Errorf("line %d: a comment ending in a backslash: systemd would join the next line to it", n)
			}
			cont.Reset()
			cont.WriteString(line[:len(line)-1] + " ")
			inCont = true
			continue
		}
		inCont = false
		l := strings.Trim(line, " \t")
		switch {
		case l == "" || unitComment(l):
		case strings.HasPrefix(l, "["):
			if !unitSectionRe.MatchString(l) {
				return nil, fmt.Errorf("line %d: invalid section header %q", first, l)
			}
			section = l[1 : len(l)-1]
			if sections[section] {
				return nil, fmt.Errorf("line %d: the section [%s] twice", first, section)
			}
			sections[section] = true
		default:
			k, v, ok := strings.Cut(l, "=")
			if !ok {
				return nil, fmt.Errorf("line %d: %q is neither a section, a comment nor an assignment", first, l)
			}
			k, v = strings.Trim(k, " \t"), strings.Trim(v, " \t")
			switch {
			case section == "":
				return nil, fmt.Errorf("line %d: %s= outside a section", first, k)
			case !unitKeyRe.MatchString(k):
				return nil, fmt.Errorf("line %d: invalid key %q", first, k)
			case keys[section+"."+k] && !unitRepeatable[k]:
				return nil, fmt.Errorf("line %d: %s= twice in [%s]", first, k, section)
			case v == "":
				return nil, fmt.Errorf("line %d: an empty %s= (it resets the setting)", first, k)
			}
			for _, r := range v {
				if (r < 0x20 && r != '\t') || r == 0x7f {
					return nil, fmt.Errorf("line %d: a control character in %s=", first, k)
				}
			}
			keys[section+"."+k] = true
			entries = append(entries, unitEntry{section: section, key: k, value: v, line: first})
		}
	}
	if inCont {
		return nil, errors.New("the file ends in a continued line")
	}
	return entries, nil
}

// unitValues are a unit's assignments by "Section.Key" (the last one of a
// repeatable key; Environment= is checked line by line).
type unitValues map[string]string

// collectUnit checks that every assignment is one of allowed (section → keys)
// and returns them.
func collectUnit(entries []unitEntry, allowed map[string][]string) (unitValues, error) {
	out := unitValues{}
	for _, e := range entries {
		keys, ok := allowed[e.section]
		if !ok {
			return nil, fmt.Errorf("line %d: the section [%s] has no place in this unit", e.line, e.section)
		}
		if !slices.Contains(keys, e.key) {
			return nil, fmt.Errorf("line %d: %s= in [%s] is not one of the settings this unit may have", e.line, e.key, e.section)
		}
		out[e.section+"."+e.key] = e.value
	}
	return out, nil
}

// require names the first of keys the unit lacks.
func (v unitValues) require(keys ...string) error {
	for _, k := range keys {
		if _, ok := v[k]; !ok {
			return fmt.Errorf("%s= is missing", k[strings.Index(k, ".")+1:])
		}
	}
	return nil
}

// is checks a value when it is set: one of allowed, or the exact value.
func (v unitValues) is(key string, allowed ...string) error {
	if val, ok := v[key]; ok && !slices.Contains(allowed, val) {
		return fmt.Errorf("%s=%s: only %s", key[strings.Index(key, ".")+1:], val, strings.Join(allowed, ", "))
	}
	return nil
}

// match checks a value against re when it is set.
func (v unitValues) match(key string, re *regexp.Regexp) error {
	if val, ok := v[key]; ok && !re.MatchString(val) {
		return fmt.Errorf("%s=%s is not a value this unit may have", key[strings.Index(key, ".")+1:], val)
	}
	return nil
}

var (
	unitBoolRe   = regexp.MustCompile(`^(yes|no|true|false|on|off|1|0)$`)
	unitTimeRe   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(ms|s|sec|m|min|h)?$`)
	unitNumberRe = regexp.MustCompile(`^[0-9]{1,9}$`)
	unitUMaskRe  = regexp.MustCompile(`^0?[0-7]{3}$`)
	unitSizeRe   = regexp.MustCompile(`^([0-9]{1,12}[KMGT]?|infinity)$`)
	unitListRe   = regexp.MustCompile(`^[A-Za-z0-9@_.:-]+\.(target|service|socket|mount|timer|path)( +[A-Za-z0-9@_.:-]+\.(target|service|socket|mount|timer|path))*$`)
	// Wants= starts what it names with the unit (debug-shell.service is a
	// root shell): the panel's units only pull targets in.
	unitTargetsRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.target( +[A-Za-z0-9_.-]+\.target)*$`)
	unitRestarts   = []string{"no", "always", "on-success", "on-failure", "on-abnormal", "on-abort", "on-watchdog"}
	unitKillModes  = []string{"control-group", "mixed", "process"}
	unitProtectSys = []string{"yes", "no", "true", "false", "full", "strict"}
	unitProtectHom = []string{"yes", "no", "true", "false", "read-only", "tmpfs"}
	appUnitRe      = regexp.MustCompile(`^monopanel-app-([a-z_][a-z0-9_-]{0,31})-([a-z0-9][a-z0-9_-]{0,31})\.service$`)
	valkeyUnitRe   = regexp.MustCompile(`^monopanel-valkey-([a-z_][a-z0-9_-]{0,31})-(cache|sessions)\.service$`)
	mountUnitRe    = regexp.MustCompile(`^[A-Za-z0-9:_.\\-]{1,240}\.mount$`)
	unitAppNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	unitPathRe     = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)
)

// firewallUnit, sphinx and the nginx drop-in run as root or as a system
// user: their command lines are the panel's, word for word.
const (
	firewallUnitName  = "monopanel-firewall.service"
	firewallExecStart = "/usr/sbin/nft -f /etc/nftables.d/monopanel.nft"
	firewallExecStop  = "-/usr/sbin/nft delete table inet monopanel"
	nginxDropInName   = ".d/monopanel.conf"
	nginxExecStartPre = "-/usr/bin/rm -f /run/nginx.pid"
)

// unitTimes are the settings a unit gives in time spans.
func (v unitValues) times(keys ...string) error {
	var errs []error
	for _, k := range keys {
		errs = append(errs, v.match(k, unitTimeRe))
	}
	return errors.Join(errs...)
}

// singleCommand checks an account's command line: one command (no ";" word,
// which systemd reads as a second one), and a first word that is an
// absolute path as written — systemd unquotes and unescapes the first word
// before it reads the prefixes, so anything but a plain "/" first could be a
// "+" (no User=, no sandbox) or a "!" in disguise.
func singleCommand(key, value string) error {
	if !strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s= must start with an absolute path (no prefix such as +, ! or -, no quotes)", key)
	}
	// Read both ways systemd versions have read quotes (anywhere in a word,
	// or only where one begins): a separator either way is refused.
	if commandSeparator(value, true) || commandSeparator(value, false) {
		return fmt.Errorf("%s= holds a \";\" word: systemd would read a second command line", key)
	}
	return nil
}

// commandSeparator reports whether the command line v holds systemd's
// separator of command lines: a word that begins with an unquoted ";"
// followed by a blank or the end (config_parse_exec). Words are split the
// way systemd splits them — blanks between them, a backslash escaping the
// next character, quotes holding blanks — so that a quoted or an escaped
// semicolon, an argument like any other (sh -c "a ; b"), is not one.
// midWord takes a quote in the middle of a word for one (systemd since the
// extract_first_word rewrite); without it only a quote that begins a word
// is.
func commandSeparator(v string, midWord bool) bool {
	blank := func(c byte) bool { return c == ' ' || c == '\t' }
	for i, n := 0, len(v); i < n; {
		for i < n && blank(v[i]) {
			i++
		}
		if i >= n {
			break
		}
		if i > 0 && v[i] == ';' && (i+1 == n || blank(v[i+1])) {
			return true
		}
		var quote byte
	word:
		for start := i; i < n; {
			c := v[i]
			switch {
			case quote != 0 && c == '\\':
				i += 2
			case quote != 0:
				if c == quote {
					quote = 0
				}
				i++
			case c == '\\':
				i += 2
			case (c == '"' || c == '\'') && (midWord || i == start):
				quote = c
				i++
			case blank(c):
				break word
			default:
				i++
			}
		}
	}
	return false
}

// pathBelow reports whether p is dir or lies below it.
func pathBelow(dir, p string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// unitPath checks a path a unit names: absolute, clean, plain characters (no
// blank, no %, which systemd expands, no quotes).
func unitPath(key, p string) error {
	if !unitPathRe.MatchString(p) || filepath.Clean(p) != p || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		return fmt.Errorf("%s=%s: an absolute, clean path of letters, digits, '.', '_' and '-'", key, p)
	}
	return nil
}

// mountUnitName is the name systemd gives the mount unit of where (the
// API's store.MountUnit): the units of the shared folders the older panels
// wrote are named so, and the agent's records of the folders it binds.
func mountUnitName(where string) string {
	var b strings.Builder
	for _, r := range strings.Trim(where, "/") {
		switch {
		case r == '/':
			b.WriteByte('-')
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ':', r == '_', r == '.':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, `\x%02x`, r)
		}
	}
	return b.String() + ".mount"
}

// unitKind is what a unit name is.
type unitKind int

const (
	unitApp unitKind = iota + 1
	unitValkey
	unitMount
	unitSphinx
	unitFirewall
	unitNginxDropIn
)

// unitKindOf tells which of the panel's units name is, or why it is none.
func unitKindOf(name string, h *UnitHost) (unitKind, error) {
	root := filepath.Clean(h.WWWRoot)
	switch {
	case name == "" || strings.ContainsAny(name, "\x00\n") || strings.Contains(name, ".."):
	case appUnitRe.MatchString(name):
		return unitApp, nil
	case valkeyUnitRe.MatchString(name):
		return unitValkey, nil
	case name == firewallUnitName:
		return unitFirewall, nil
	case h.Sphinx.UnitFile != "" && name == path.Base(h.Sphinx.UnitFile):
		return unitSphinx, nil
	case h.NginxService != "" && name == h.NginxService+nginxDropInName:
		return unitNginxDropIn, nil
	case mountUnitRe.MatchString(name) && root != "/" && strings.HasPrefix(name, strings.TrimSuffix(mountUnitName(root), ".mount")+"-"):
		return unitMount, nil
	}
	return 0, fmt.Errorf("the panel writes monopanel-app-*, monopanel-valkey-*, monopanel-firewall, Sphinx's and nginx's drop-in only, and removes the mount units of shared folders below %s that older panels wrote", root)
}

// CheckUnitName tells why name is none of the panel's units (/v1/units/remove).
func CheckUnitName(name string, h *UnitHost) error {
	_, err := unitKindOf(name, h)
	return err
}

// CheckUnit tells why content must not be written as the unit name: nil when
// it is one of the panel's units in the form the panel writes it.
func CheckUnit(name string, content []byte, h *UnitHost) error {
	kind, err := unitKindOf(name, h)
	if err != nil {
		return err
	}
	entries, err := parseUnit(string(content))
	if err != nil {
		return err
	}
	switch kind {
	case unitApp:
		return checkAppUnit(name, entries, h)
	case unitValkey:
		return checkValkeyUnit(name, entries, h)
	case unitMount:
		return errNoMountUnits
	case unitSphinx:
		return checkSphinxUnit(entries, h)
	case unitFirewall:
		return checkFirewallUnit(entries)
	case unitNginxDropIn:
		return checkNginxDropIn(entries)
	}
	return errors.New("unknown unit")
}

// accountOf checks User= and Group= of an account's unit: a panel account,
// and its own primary group.
func accountOf(v unitValues, h *UnitHost) (*UnitAccount, error) {
	if err := v.require("Service.User", "Service.Group"); err != nil {
		return nil, err
	}
	login := v["Service.User"]
	if !nameRe.MatchString(login) || login == "root" {
		return nil, fmt.Errorf("User=%s is not a panel account", login)
	}
	if h.Account == nil {
		return nil, errors.New("no panel accounts on this host")
	}
	acct, err := h.Account(login)
	if err != nil {
		return nil, fmt.Errorf("User=%s: %w", login, err)
	}
	if g := v["Service.Group"]; g != acct.Group {
		return nil, fmt.Errorf("Group=%s: only %s, the primary group of %s", g, acct.Group, login)
	}
	return acct, nil
}

var unitInstall = map[string][]string{"Install": {"WantedBy"}}

func withInstall(m map[string][]string) map[string][]string {
	m["Install"] = unitInstall["Install"]
	return m
}

func checkAppUnit(name string, entries []unitEntry, h *UnitHost) error {
	v, err := collectUnit(entries, withInstall(map[string][]string{
		"Unit": {"Description", "After", "Wants"},
		"Service": {"Type", "User", "Group", "WorkingDirectory", "ExecStart", "EnvironmentFile", "Environment",
			"Restart", "RestartSec", "TimeoutStopSec", "KillMode", "UMask", "PrivateTmp", "ProtectSystem"},
	}))
	if err != nil {
		return err
	}
	acct, err := accountOf(v, h)
	if err != nil {
		return err
	}
	// The unit carries its account's login: alex-b + c and alex + b-c spell
	// the same name, so the name must begin with this account's.
	if rest, ok := strings.CutPrefix(name, "monopanel-app-"+acct.Login+"-"); !ok || !unitAppNameRe.MatchString(strings.TrimSuffix(rest, ".service")) {
		return fmt.Errorf("the unit %s does not belong to User=%s", name, acct.Login)
	}
	if err := v.require("Service.ExecStart"); err != nil {
		return err
	}
	errs := []error{
		v.is("Service.Type", "simple", "exec", "notify"),
		singleCommand("ExecStart", v["Service.ExecStart"]),
		v.is("Service.Restart", unitRestarts...),
		v.times("Service.RestartSec", "Service.TimeoutStopSec"),
		v.is("Service.KillMode", unitKillModes...),
		v.match("Service.UMask", unitUMaskRe),
		v.match("Service.PrivateTmp", unitBoolRe),
		v.is("Service.ProtectSystem", unitProtectSys...),
		v.match("Unit.After", unitListRe),
		v.match("Unit.Wants", unitTargetsRe),
		v.is("Install.WantedBy", "multi-user.target"),
	}
	// The account enters it itself; a path of its home, as the API checks,
	// written out (no %-specifier, which systemd would expand, no ~ or -).
	if wd, ok := v["Service.WorkingDirectory"]; ok {
		if !strings.HasPrefix(wd, "/") || filepath.Clean(wd) != wd || strings.Contains(wd, "%") {
			errs = append(errs, fmt.Errorf("WorkingDirectory=%s: an absolute, clean path without %%", wd))
		} else if !pathBelow(acct.Home, wd) {
			errs = append(errs, fmt.Errorf("WorkingDirectory=%s is outside the home of %s (%s)", wd, acct.Login, acct.Home))
		}
	}
	// PID 1 reads EnvironmentFile= as root: only the panel's own file of
	// this very unit, root's, in /etc/monopanel/apps.
	if ef, ok := v["Service.EnvironmentFile"]; ok {
		if want := appEnvDir + "/" + strings.TrimSuffix(name, ".service") + ".env"; ef != want {
			errs = append(errs, fmt.Errorf("EnvironmentFile=%s: only %s", ef, want))
		}
	}
	return errors.Join(errs...)
}

func checkValkeyUnit(name string, entries []unitEntry, h *UnitHost) error {
	v, err := collectUnit(entries, withInstall(map[string][]string{
		"Unit": {"Description", "After"},
		"Service": {"Type", "User", "Group", "ExecStart", "RuntimeDirectory", "RuntimeDirectoryMode", "StateDirectory", "StateDirectoryMode",
			"Restart", "RestartSec", "TimeoutStopSec", "UMask", "MemoryMax", "PrivateTmp", "ProtectSystem", "ProtectHome", "ProtectControlGroups"},
	}))
	if err != nil {
		return err
	}
	acct, err := accountOf(v, h)
	if err != nil {
		return err
	}
	m := valkeyUnitRe.FindStringSubmatch(name)
	if m == nil || m[1] != acct.Login {
		return fmt.Errorf("the unit %s does not belong to User=%s", name, acct.Login)
	}
	if err := v.require("Service.ExecStart", "Service.RuntimeDirectory", "Service.StateDirectory"); err != nil {
		return err
	}
	exec := v["Service.ExecStart"]
	known := false
	for _, b := range h.ValkeyBinaries {
		known = known || strings.HasPrefix(exec, b+" ") || strings.HasPrefix(exec, b+"\t") || exec == b
	}
	// systemd makes the directories for the unit and gives them to User= —
	// recursively: another name would hand over another directory.
	dir := "monopanel-valkey/" + acct.Login + "-" + m[2]
	errs := []error{
		v.is("Service.Type", "notify", "simple", "exec"),
		singleCommand("ExecStart", exec),
		v.is("Service.RuntimeDirectory", dir),
		v.is("Service.StateDirectory", dir),
		v.is("Service.RuntimeDirectoryMode", "0700"),
		v.is("Service.StateDirectoryMode", "0700"),
		v.is("Service.Restart", unitRestarts...),
		v.times("Service.RestartSec", "Service.TimeoutStopSec"),
		v.match("Service.UMask", unitUMaskRe),
		v.match("Service.MemoryMax", unitSizeRe),
		v.match("Service.PrivateTmp", unitBoolRe),
		v.is("Service.ProtectSystem", unitProtectSys...),
		v.is("Service.ProtectHome", unitProtectHom...),
		v.match("Service.ProtectControlGroups", unitBoolRe),
		v.match("Unit.After", unitListRe),
		v.is("Install.WantedBy", "multi-user.target"),
	}
	if !known {
		errs = append(errs, fmt.Errorf("ExecStart= must start one of %s", strings.Join(h.ValkeyBinaries, ", ")))
	}
	return errors.Join(errs...)
}

// mountPointOf checks the mount point of a shared folder: inside the home of
// a panel account (an SFTP-only guest's root-owned chroot), or
// <www_root>/.shares/<login>/<name> for a panel account with a shell, whose
// own home is no place for a mount point (sharemount.go). Whether the
// directories on the way are root's is the agent's check of the host
// (openMountPoint), not this one of the text.
func mountPointOf(p string, h *UnitHost) error {
	root := filepath.Clean(h.WWWRoot)
	rel, ok := strings.CutPrefix(p, root+"/"+sharesDirName+"/")
	if !ok {
		if err := homeOf(p, h); err != nil {
			return err
		}
		// One name in the home, as the panel writes it.
		if strings.Count(strings.TrimPrefix(p, root+"/"), "/") != 1 {
			return fmt.Errorf("%s is not <home>/<name>: a shared folder is mounted right in the home of an SFTP-only account", p)
		}
		return nil
	}
	login, name, ok := strings.Cut(rel, "/")
	if !ok || login == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("%s is not %s/%s/<login>/<name>", p, root, sharesDirName)
	}
	if h.Account == nil {
		return errors.New("no panel accounts on this host")
	}
	if _, err := h.Account(login); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

// homeOf checks that p lies inside the home of a panel account: the first
// name below the web root is the account, and its home holds p.
func homeOf(p string, h *UnitHost) error {
	root := filepath.Clean(h.WWWRoot)
	rel, ok := strings.CutPrefix(p, root+"/")
	if !ok || !strings.Contains(rel, "/") {
		return fmt.Errorf("%s is not inside an account's home below %s", p, root)
	}
	login, _, _ := strings.Cut(rel, "/")
	if h.Account == nil {
		return errors.New("no panel accounts on this host")
	}
	acct, err := h.Account(login)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if p == acct.Home || !pathBelow(acct.Home, p) {
		return fmt.Errorf("%s is not inside the home of %s (%s)", p, login, acct.Home)
	}
	return nil
}

// errNoMountUnits refuses a mount unit to /v1/units/apply.
var errNoMountUnits = errors.New("the panel writes no mount units: the agent binds a shared folder itself, from the folder it opened without following a symlink (/v1/shares/bind), and a unit would have systemd mount it at boot by its path, through whatever symlink the folder's owner put there")

// CheckShareBind tells why the agent must not bind the folder what at the
// mount point where, as far as the text goes (/v1/shares/bind; the agent's
// doubles): both plain clean paths, what a folder inside the home of a panel
// account — never in .shares or the restore area, which are no homes —
// and where a mount point of a shared folder (mountPointOf), neither one
// inside the other. Whether they are what the names say is the agent's look
// at the host (openShareSource, openMountPoint).
func CheckShareBind(what, where string, h *UnitHost) error {
	if err := shareBindShape(what, where, h.WWWRoot); err != nil {
		return err
	}
	return errors.Join(homeOf(what, h), mountPointOf(where, h))
}

// shareBindShape is CheckShareBind without a look at the accounts: what a
// folder inside a home below the web root, never in .shares or the restore
// area, and where <www_root>/<login>/<name> or
// <www_root>/.shares/<login>/<name>, neither one inside the other — by
// their text alone.
func shareBindShape(what, where, wwwRoot string) error {
	if err := errors.Join(unitPath("What", what), unitPath("Where", where)); err != nil {
		return err
	}
	root := filepath.Clean(wwwRoot)
	rel, ok := strings.CutPrefix(what, root+"/")
	switch {
	case !ok || !strings.Contains(rel, "/"):
		return fmt.Errorf("%s is not inside an account's home below %s", what, root)
	case strings.HasPrefix(rel, "."):
		return fmt.Errorf("%s lies in %s, the agent's own: no account's home", what, filepath.Join(root, strings.SplitN(rel, "/", 2)[0]))
	case pathBelow(what, where) || pathBelow(where, what):
		return fmt.Errorf("the folder %s and its mount point %s lie one inside the other", what, where)
	}
	return shareMountPointShape(where, wwwRoot)
}

// CheckShareUnbind tells why the agent must not unbind the mount point where
// (/v1/shares/unbind; the agent's doubles): a plain clean path of the shape
// of a shared folder's mount point (shareMountPointShape) and — unless the
// agent knows a shared folder at exactly that mount point (known: its
// record of one, or an older panel's unit in the panel's form) — one where
// a panel account has its shared folders (mountPointOf).
func CheckShareUnbind(where string, known bool, h *UnitHost) error {
	if err := errors.Join(unitPath("Where", where), shareMountPointShape(where, h.WWWRoot)); err != nil {
		return err
	}
	if known {
		return nil
	}
	return mountPointOf(where, h)
}

// shareMountPointShape tells why where, a clean path (unitPath), is no mount
// point of a shared folder by its text alone: <www_root>/<login>/<name> or
// <www_root>/.shares/<login>/<name>, the login without a leading dot.
func shareMountPointShape(where, wwwRoot string) error {
	root := filepath.Clean(wwwRoot)
	wrel, ok := strings.CutPrefix(where, root+"/")
	switch parts := strings.Split(wrel, "/"); {
	case ok && len(parts) == 2 && !strings.HasPrefix(parts[0], "."):
	case ok && len(parts) == 3 && parts[0] == sharesDirName && !strings.HasPrefix(parts[1], "."):
	default:
		return fmt.Errorf("%s is neither %s/<login>/<name> nor %s/<login>/<name>", where, root, filepath.Join(root, sharesDirName))
	}
	return nil
}

// shareUnitHeader begins every mount unit of a shared folder a panel wrote,
// from 0.8.13 on (internal/api/ops_shares.go).
const shareUnitHeader = "# Generated by MonoPanel: shared folder of "

// legacyShareUnit reads a shared folder's mount unit as the panels up to
// 0.8.20 wrote it — a bind of What=, a folder in an account's home, at the
// Where= of a shared folder, wanted by multi-user.target — and returns What=
// and Where=; an error says why the unit is not one of those (an
// administrator's own, which the agent leaves as it is).
//
// Up to 0.8.19 the panel wrote these units through config/apply, with no look
// at the accounts: a guest or an owner may be one the agent no longer counts
// as a panel account (a supplementary group of the system's, a privileged
// one). A unit whose header says it is the panel's is read by its text alone
// all the same — systemd would go on mounting it by path at every boot — and
// the accounts are judged where the folder is bound: by the agent's look at
// what the unit mounted (verifyShareMounts) and at the API's next bind.
func legacyShareUnit(name string, content []byte, h *UnitHost) (what, where string, err error) {
	if kind, err := unitKindOf(name, h); err != nil || kind != unitMount {
		return "", "", errors.New("not a mount unit of a shared folder")
	}
	entries, err := parseUnit(string(content))
	if err != nil {
		return "", "", err
	}
	v, err := collectUnit(entries, withInstall(map[string][]string{
		"Unit":  {"Description", "RequiresMountsFor"},
		"Mount": {"What", "Where", "Type", "Options"},
	}))
	if err != nil {
		return "", "", err
	}
	if err := v.require("Mount.What", "Mount.Where", "Mount.Type", "Mount.Options"); err != nil {
		return "", "", err
	}
	what, where = v["Mount.What"], v["Mount.Where"]
	if strings.HasPrefix(string(content), shareUnitHeader) {
		err = shareBindShape(what, where, h.WWWRoot)
	} else {
		err = CheckShareBind(what, where, h)
	}
	if err != nil {
		return "", "", err
	}
	errs := []error{
		// A bind mount and nothing else: no device, no other file system,
		// no option such as exec or suid.
		v.is("Mount.Type", "none"),
		v.is("Mount.Options", "bind"),
		v.is("Unit.RequiresMountsFor", path.Dir(what)),
		v.is("Install.WantedBy", "multi-user.target"),
	}
	if want := mountUnitName(where); name != want {
		errs = append(errs, fmt.Errorf("the unit of Where=%s is %s, not %s", where, want, name))
	}
	if err := errors.Join(errs...); err != nil {
		return "", "", err
	}
	return what, where, nil
}

// MountUnitName is the name systemd gives the mount unit of where, for the
// agent's doubles, which keep the state of a shared folder by it as systemd
// shows a mount by it.
func MountUnitName(where string) string { return mountUnitName(where) }

// MountUnitWhere is the path below the web root wwwRoot the mount unit name
// stands for — MountUnitName the other way round — for the agent's doubles,
// which list the shared folders they keep by those names.
func MountUnitWhere(name, wwwRoot string) (string, bool) {
	where, _, ok := isShareMountUnit(name, filepath.Clean(wwwRoot))
	return where, ok
}

func checkSphinxUnit(entries []unitEntry, h *UnitHost) error {
	v, err := collectUnit(entries, withInstall(map[string][]string{
		"Unit":    {"Description", "After"},
		"Service": {"Type", "User", "Group", "ExecStart", "RuntimeDirectory", "RuntimeDirectoryMode", "Restart", "RestartSec"},
	}))
	if err != nil {
		return err
	}
	sx := h.Sphinx
	if err := v.require("Service.User", "Service.Group", "Service.ExecStart"); err != nil {
		return err
	}
	errs := []error{
		v.is("Service.User", sx.User),
		v.is("Service.Group", sx.User),
		v.is("Service.Type", "simple"),
		v.is("Service.ExecStart", sx.InstallDir+"/bin/searchd --config "+sx.ConfFile+" --nodetach"),
		v.is("Service.RuntimeDirectory", strings.TrimPrefix(path.Dir(sx.PidFile), "/run/")),
		v.is("Service.RuntimeDirectoryMode", "0755"),
		v.is("Service.Restart", unitRestarts...),
		v.times("Service.RestartSec"),
		v.match("Unit.After", unitListRe),
		v.is("Install.WantedBy", "multi-user.target"),
	}
	if h.SystemUser != nil {
		if err := h.SystemUser(sx.User); err != nil {
			errs = append(errs, fmt.Errorf("User=%s: %w", sx.User, err))
		}
	}
	return errors.Join(errs...)
}

func checkFirewallUnit(entries []unitEntry) error {
	v, err := collectUnit(entries, withInstall(map[string][]string{
		"Unit":    {"Description", "After", "Wants", "Before"},
		"Service": {"Type", "RemainAfterExit", "ExecStart", "ExecStop"},
	}))
	if err != nil {
		return err
	}
	if err := v.require("Service.ExecStart"); err != nil {
		return err
	}
	return errors.Join(
		v.is("Service.Type", "oneshot"),
		v.match("Service.RemainAfterExit", unitBoolRe),
		v.is("Service.ExecStart", firewallExecStart),
		v.is("Service.ExecStop", firewallExecStop),
		v.match("Unit.After", unitListRe),
		v.match("Unit.Wants", unitTargetsRe),
		v.match("Unit.Before", unitListRe),
		v.is("Install.WantedBy", "multi-user.target"),
	)
}

func checkNginxDropIn(entries []unitEntry) error {
	v, err := collectUnit(entries, map[string][]string{
		"Unit":    {"StartLimitIntervalSec", "StartLimitBurst"},
		"Service": {"ExecStartPre", "Restart", "RestartSec"},
	})
	if err != nil {
		return err
	}
	return errors.Join(
		v.is("Service.ExecStartPre", nginxExecStartPre),
		v.is("Service.Restart", unitRestarts...),
		v.times("Service.RestartSec", "Unit.StartLimitIntervalSec"),
		v.match("Unit.StartLimitBurst", unitNumberRe),
	)
}

// unitDeniedGroups are groups worth root (or the root of a container, the
// disks, the journal, the password hashes) even when an administrator made
// one with an ordinary gid.
var unitDeniedGroups = map[string]bool{
	"root": true, "sudo": true, "wheel": true, "admin": true, "adm": true, "staff": true,
	"docker": true, "lxd": true, "incus": true, "incus-admin": true, "libvirt": true, "kvm": true,
	"disk": true, "shadow": true, "systemd-journal": true,
}

// unitAccount looks up the panel account an account's unit runs as: not
// root, not the service user, uid >= UID_MIN, its home below the web root, a
// primary group of its own and no supplementary group besides the panel's
// (CLI, web, SFTP) and other ordinary groups that grant nothing — a unit as
// an account in docker, sudo or disk would be root by another name.
func (s *Server) unitAccount(login string) (*UnitAccount, error) {
	if !nameRe.MatchString(login) || login == "root" || login == s.cfg.ServiceUser {
		return nil, errors.New("not a panel account")
	}
	u, err := user.Lookup(login)
	if err != nil {
		return nil, fmt.Errorf("no such account: %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if uid == 0 || uid < uidMin() {
		return nil, fmt.Errorf("a system account (uid %d), not one of the panel's", uid)
	}
	home, root := filepath.Clean(u.HomeDir), filepath.Clean(s.cfg.WWWRoot)
	if !strings.HasPrefix(home, root+"/") {
		return nil, fmt.Errorf("its home %s is not below %s", home, root)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		return nil, fmt.Errorf("primary group %s: %w", u.Gid, err)
	}
	if gid == 0 || gid < loginDefsGIDMin() || unitDeniedGroups[g.Name] {
		return nil, fmt.Errorf("its primary group %s (gid %d) is not one of its own", g.Name, gid)
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("groups: %w", err)
	}
	panel := map[string]bool{s.cfg.CLIGroup: true, s.cfg.WebGroup: true, unitSFTPGroup: true}
	for _, id := range ids {
		if id == u.Gid {
			continue
		}
		sg, err := user.LookupGroupId(id)
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", id, err)
		}
		n, _ := strconv.Atoi(id)
		if panel[sg.Name] || (n >= loginDefsGIDMin() && !unitDeniedGroups[sg.Name]) {
			continue
		}
		return nil, fmt.Errorf("a member of the group %s (gid %s): the panel runs services only for its own accounts", sg.Name, id)
	}
	return &UnitAccount{Login: login, Home: home, Group: g.Name, UID: uint32(uid)}, nil
}

// unitSystemUser checks the account a system service of the panel's runs as: a
// system account, not root and not the service user.
func (s *Server) unitSystemUser(login string) error {
	if !nameRe.MatchString(login) || login == "root" || login == s.cfg.ServiceUser {
		return errors.New("not a system account of its own")
	}
	uid, err := lookupUID(login)
	if err != nil {
		return fmt.Errorf("no such account: %w", err)
	}
	if uid == 0 || uid >= uidMin() {
		return fmt.Errorf("uid %d is not a system account's", uid)
	}
	return nil
}

// lookupUnitAccount finds the panel account a unit is for (unitAccount);
// tests, which cannot make unix accounts, replace it.
var lookupUnitAccount = (*Server).unitAccount

// unitHost is this host for the unit checks.
func (s *Server) unitHost() *UnitHost {
	account := func(login string) (*UnitAccount, error) { return lookupUnitAccount(s, login) }
	h := &UnitHost{WWWRoot: s.cfg.WWWRoot, Account: account, SystemUser: s.unitSystemUser}
	if s.profile != nil {
		h.NginxService = s.profile.Web().NginxService
		h.Sphinx = osprofile.Sphinx(s.profile)
		for _, l := range osprofile.ValkeyCandidates(s.profile) {
			h.ValkeyBinaries = append(h.ValkeyBinaries, l.Binary)
		}
	}
	return h
}

func (s *Server) unitPath(name string) string {
	return filepath.Join(s.unitDirectory(), name)
}

func (s *Server) unitDirectory() string {
	if s.unitDir == "" {
		return systemUnitDir
	}
	return filepath.Clean(s.unitDir)
}

// unitFollow lets the walk to a unit resolve root's symlinks on the way to
// /etc/systemd/system, and none in it (a drop-in directory).
func (s *Server) unitFollow() int { return len(pathParts(s.unitDirectory())) }

// applyUnit writes one of the panel's units (0644 root:root, atomically, the
// previous version to the history) after CheckUnit — never a mount unit: the
// shared folders are the agent's to bind (sharebind.go).
func (s *Server) applyUnit(ctx context.Context, req *ApplyUnitRequest) (*ApplyUnitResponse, error) {
	h := s.unitHost()
	if err := CheckUnit(req.Name, []byte(req.Content), h); err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: "unit " + req.Name + " is not allowed: " + err.Error()}
	}
	p := s.unitPath(req.Name)
	tx := s.newFileTx()
	defer tx.close()
	// The agent's own file: made by root, root's (no chown, so that a
	// test's unprivileged agent can write it too).
	changed, err := tx.write(FileSpec{Path: p, Mode: 0o644}, []byte(req.Content), s.unitFollow())
	if err != nil {
		tx.rollback()
		return nil, walkErr("write "+p, err)
	}
	tx.commitHistory()
	if changed {
		s.restorecon(ctx, p)
		s.log.Info("unit written", "unit", req.Name, "origin", req.Origin)
	}
	return &ApplyUnitResponse{Path: p, Changed: changed}, nil
}

// removeUnit deletes one of the panel's units; the caller stops and
// disables it first and reloads systemd afterwards. A shared folder's unit
// systemd forgets before the call returns (removeShareUnit).
func (s *Server) removeUnit(ctx context.Context, req *RemoveUnitRequest) (*RemoveUnitResponse, error) {
	h := s.unitHost()
	if err := CheckUnitName(req.Name, h); err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: "unit " + req.Name + " is not allowed: " + err.Error()}
	}
	p := s.unitPath(req.Name)
	if kind, _ := unitKindOf(req.Name, h); kind == unitMount {
		return s.removeShareUnit(ctx, req.Name, p)
	}
	removed, err := s.removeRootPath(p, false, s.unitFollow())
	if err != nil {
		return nil, err
	}
	if removed {
		s.log.Info("unit removed", "unit", req.Name)
	}
	return &RemoveUnitResponse{Path: p, Removed: removed}, nil
}
