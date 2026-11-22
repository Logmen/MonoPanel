package confcheck

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// dovecot: the master runs as root and starts every service as the user
// its block names, from the executable it names; the log process writes
// log_path as root; passdb and userdb decide which account a login's mail
// process runs as. The panel's dovecot.conf is complete (the distribution's
// conf.d is not included): passwd-file users, all of them the mail user,
// whose mail lives in /var/mail/monopanel. The check keeps it that way:
// first_valid_uid and last_valid_uid pin every mail process to the mail
// user, whatever a userdb answers, and nothing may run another program or
// write a file as root.

const (
	dovecotDir   = "/etc/dovecot"
	dovecotMain  = dovecotDir + "/dovecot.conf"
	dovecotUsers = dovecotDir + "/monopanel/users"
)

func isDovecot(p string) bool { return under(p, dovecotDir) }

func checkDovecot(c *checker, p string, content []byte) error {
	switch {
	case p == dovecotUsers:
		return checkDovecotUsers(content)
	case p == dovecotMain || strings.HasSuffix(p, ".conf") || strings.HasSuffix(p, ".ext"):
		return c.checkDovecotConf(p, content)
	}
	return notAllowed("writing "+p, "the panel writes dovecot.conf and the users file there")
}

const (
	dcKeyValue = iota
	dcSectionBegin
	dcSectionEnd
	dcInclude
)

type dcLine struct {
	n       int
	kind    int
	key     string
	value   string
	keyfile bool
}

// parseDovecot reads lines the way dovecot's config_parse_line does: "#"
// outside quotes starts a comment, a trailing backslash joins the next
// line with a blank, and a line is "key = value", "type [name] {", "}" or
// an !include.
func parseDovecot(content []byte) ([]dcLine, error) {
	lines, err := textLines(content)
	if err != nil {
		return nil, err
	}
	var out []dcLine
	var full strings.Builder
	fullStart := 0
	for i, raw := range lines {
		n := i + 1
		line := strings.TrimLeft(raw, " \t")
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimRight(dcStripComment(line), " \t")
		if strings.HasSuffix(line, "\\") {
			if l := strings.TrimRight(line[:len(line)-1], " \t"); l != "" {
				if full.Len() == 0 {
					fullStart = n
				}
				full.WriteString(l)
				full.WriteByte(' ')
			}
			continue
		}
		if full.Len() > 0 {
			full.WriteString(line)
			line, n = full.String(), fullStart
			full.Reset()
		}
		if line == "" {
			continue
		}
		l, err := dcClassify(n, line)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if full.Len() > 0 {
		return nil, unreadable(fullStart, "the continued line never ends")
	}
	return out, nil
}

func dcStripComment(line string) string {
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'', '"':
			q := line[i]
			for i++; i < len(line) && line[i] != q; i++ {
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
			}
			if i >= len(line) {
				return line
			}
		case '#':
			return line[:i]
		}
	}
	return line
}

func dcClassify(n int, line string) (dcLine, error) {
	j := strings.IndexAny(line, " \t=")
	if j < 0 {
		j = len(line)
	}
	key, rest := line[:j], strings.TrimLeft(line[j:], " \t")
	l := dcLine{n: n, key: key}
	switch {
	case key == "!include" || key == "!include_try":
		l.kind, l.value = dcInclude, dcUnquote(rest)
		return l, nil
	case strings.HasPrefix(rest, "="):
		v := strings.TrimLeft(rest[1:], " \t")
		if strings.HasPrefix(v, "<") {
			l.kind, l.keyfile, l.value = dcKeyValue, true, strings.TrimLeft(v[1:], " \t")
			return l, nil
		}
		l.kind, l.value = dcKeyValue, dcUnquote(v)
		return l, nil
	case key == "}" && rest == "":
		l.kind = dcSectionEnd
		return l, nil
	}
	l.kind = dcSectionBegin
	if rest == "{" {
		return l, nil
	}
	var name string
	if strings.HasPrefix(rest, "\"") {
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return l, unreadable(n, "unterminated section name")
		}
		name, rest = rest[1:1+end], strings.TrimLeft(rest[2+end:], " \t")
	} else {
		k := strings.IndexAny(rest, " \t")
		if k < 0 {
			return l, unreadable(n, "expecting \"{\" after %q", line)
		}
		name, rest = rest[:k], strings.TrimLeft(rest[k:], " \t")
	}
	if rest != "{" {
		return l, unreadable(n, "expecting \"{\" after %q", line)
	}
	l.value = name
	return l, nil
}

// dcUnquote is the quote removal of config_parse_line: str_unescape drops
// the backslash before any character.
func dcUnquote(v string) string {
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		s := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				if i >= len(s) {
					break
				}
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	return v
}

// dcFilterPrefixes are dovecot 2.4's setting prefixes of its filters:
// driver inside "passdb x { }" is passdb_driver, either spelling works, and
// the prefixed one at the top sets the default of every passdb.
var dcFilterPrefixes = []string{"passdb_", "userdb_", "service_", "unix_listener_", "fifo_listener_", "inet_listener_"}

// dcKeyContext names the filter a setting belongs to, by its prefix or by
// the section it stands in, and the setting without the prefix.
func dcKeyContext(key, parent string) (ctx, name string) {
	for _, p := range dcFilterPrefixes {
		if strings.HasPrefix(key, p) {
			return strings.TrimSuffix(p, "_"), strings.TrimPrefix(key, p)
		}
	}
	return parent, key
}

// dovecotDenied run a program, load code, or move the master's own files.
var dovecotDenied = map[string]string{
	"executable":         "the master runs it, as the service's user (root by default for some)",
	"import_environment": "the variables reach every process the root master starts (LD_PRELOAD)",
	"mail_plugin_dir":    "plugins load from the directory it names",
	"base_dir":           "the root master makes its sockets and files there",
	"state_dir":          "the root master writes its state there",
	"dsync_remote_cmd":   "doveadm runs the command",
	"doveadm_password":   "it opens doveadm's server, root's, to whoever knows it",
	"doveadm_api_key":    "it opens doveadm's HTTP API, root's, to whoever knows it",
	"ssl_crypto_device":  "it loads an OpenSSL engine",
}

type dcSection struct {
	typ, name string
	driver    string
	line      int
}

func (c *checker) checkDovecotConf(p string, content []byte) error {
	lines, err := parseDovecot(content)
	if err != nil {
		return err
	}
	main := p == dovecotMain
	var stack []dcSection
	pinned := map[string]bool{}
	for _, l := range lines {
		var err error
		switch l.kind {
		case dcInclude:
			err = dovecotInclude(path.Dir(p), l.key, l.value)
		case dcSectionBegin:
			s := dcSection{typ: strings.ToLower(l.key), name: l.value, line: l.n}
			if s.typ == "passdb" || s.typ == "userdb" {
				s.driver = l.value
			}
			switch {
			case (s.typ == "unix_listener" || s.typ == "fifo_listener") && !dovecotListenerName(s.name):
				err = notAllowed(fmt.Sprintf("dovecot %s %q", s.typ, s.name), "the root master removes what is at the path and makes the socket there; a listener is a name in dovecot's run directory or a socket in /var/spool/postfix/private")
			case s.typ == "group" || strings.ContainsAny(s.typ, "/@$") || strings.HasPrefix(s.name, "@"):
				// dovecot 2.4's groups of settings are applied where an
				// "@name" names them, out of this check's sight.
				err = notAllowed(fmt.Sprintf("dovecot section %s %s", l.key, s.name), "the panel defines no groups of settings")
			case dcNameDenied(s.typ) != "":
				err = notAllowed(fmt.Sprintf("dovecot section %s", l.key), "%s", dcNameDenied(s.typ))
			}
			stack = append(stack, s)
		case dcSectionEnd:
			if len(stack) == 0 {
				return unreadable(l.n, "\"}\" without a section")
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.typ == "passdb" || top.typ == "userdb" {
				if top.driver != "passwd-file" {
					err = notAllowed(fmt.Sprintf("dovecot %s driver %q", top.typ, top.driver), "the panel's users are in a passwd-file; checkpassword and lua run programs as root, others hand out accounts the panel does not know")
				}
			}
		case dcKeyValue:
			key := strings.ToLower(l.key)
			if len(stack) == 0 && dovecotPinned[key] {
				pinned[key] = true
			}
			if len(stack) > 0 {
				// driver and passdb_driver set the driver of the passdb or
				// userdb they stand in (dovecotKV refuses any but passwd-file).
				top := &stack[len(stack)-1]
				if _, name := dcKeyContext(key, top.typ); name == "driver" && (top.typ == "passdb" || top.typ == "userdb") {
					top.driver = l.value
				}
			}
			err = c.dovecotKV(main, stack, key, l)
		}
		if err := atLine(l.n, err); err != nil {
			return err
		}
	}
	if len(stack) > 0 {
		return unreadable(stack[len(stack)-1].line, "section %q is not closed", stack[len(stack)-1].typ)
	}
	if main {
		for _, k := range []string{"first_valid_uid", "last_valid_uid"} {
			if !pinned[k] {
				return notAllowed("a dovecot.conf without "+k, "first_valid_uid and last_valid_uid pin every mail process to the mail user %s; without them a userdb answer could make one run as any account", vmailUser)
			}
		}
	}
	return nil
}

// dovecotPinned are the ids of the mail processes: dovecot.conf sets them
// once, at the top, to the mail user.
var dovecotPinned = map[string]bool{
	"mail_uid": true, "mail_gid": true, "first_valid_uid": true, "last_valid_uid": true, "first_valid_gid": true, "last_valid_gid": true,
}

var (
	dovecotServiceUsers  = map[string]bool{"": true, "$default_internal_user": true, "$default_login_user": true, "$SET:default_internal_user": true, "$SET:default_login_user": true, "dovecot": true, "dovenull": true, vmailUser: true}
	dovecotListenerUsers = map[string]bool{"": true, "postfix": true, vmailUser: true, "dovecot": true, "dovenull": true, "$default_internal_user": true, "$SET:default_internal_user": true}
	dovecotModeRe        = regexp.MustCompile(`^0?[0-7]{3,4}$`)
	dovecotFieldRe       = regexp.MustCompile(`^(uid|gid|home|quota_rule|quota_storage_size|mail)=`)
)

func (c *checker) dovecotKV(main bool, stack []dcSection, key string, l dcLine) error {
	v := l.value
	what := fmt.Sprintf("dovecot setting %q", l.key)
	parent := ""
	if len(stack) > 0 {
		parent = stack[len(stack)-1].typ
	}
	// "service/auth/user" names a setting by its path, "@group = name"
	// pulls in a group of settings defined elsewhere: either would put a
	// setting where this check does not look for it.
	if strings.ContainsAny(key, "/@$") {
		return notAllowed(what, "a setting is named plainly, inside its section; the panel writes no paths or groups of settings")
	}
	ctx, name := dcKeyContext(key, parent)
	if l.keyfile && !strings.HasPrefix(key, "ssl_") {
		return notAllowed(what+" = <"+v, "\"<file\" takes the value from a file this check cannot see; only TLS keys and certificates are read that way")
	}
	if why := dcNameDenied(key); why != "" {
		return notAllowed(what, "%s", why)
	}
	if dcGroupLists[parent] {
		// A boolean list of groups (dovecot 2.4): the keys are the groups.
		if err := c.groupAllowed(key); err != nil {
			return notAllowed(fmt.Sprintf("dovecot %s %s", parent, l.key), "%v", err)
		}
	}
	if dovecotPinned[key] {
		if !main || len(stack) > 0 {
			return notAllowed(what, "the mail processes' ids are set once, at the top of %s", dovecotMain)
		}
		check := c.isVmailUID
		if strings.HasSuffix(key, "gid") {
			check = c.isVmailGID
		}
		if err := check(v); err != nil {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "mail processes run as the mail user: %v", err)
		}
		return nil
	}
	switch key {
	case "mail_privileged_group", "mail_access_groups":
		for _, g := range postfixList(v) {
			if err := c.groupAllowed(g); err != nil {
				return notAllowed(fmt.Sprintf("%s = %s", what, v), "%v", err)
			}
		}
	case "default_internal_user", "default_login_user", "default_internal_group":
		if v != "dovecot" && v != "dovenull" {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "dovecot's own processes run as dovecot or dovenull")
		}
	case "log_path", "info_log_path", "debug_log_path":
		if v == "syslog" || v == "/dev/stderr" || v == "/dev/null" {
			return nil
		}
		if p, ok := absPath(v); ok && directChild(p, "/var/log") && !strings.ContainsAny(p, "$%*") {
			return nil
		}
		return notAllowed(fmt.Sprintf("%s = %s", what, v), "the log process writes it as root; logs go to syslog or right in /var/log")
	case "mail_location":
		drv, rest, _ := strings.Cut(v, ":")
		loc, _, _ := strings.Cut(rest, ":")
		if !dovecotDriver(drv) || !dovecotMailPath(loc) {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "mail is maildir (or dbox) in %s", mailBase)
		}
	case "mail_driver":
		if !dovecotDriver(v) {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "mail is maildir (or dbox); mbox appends to a file")
		}
	case "mail_path", "mail_home":
		if !dovecotMailPath(v) {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "mail lives in %s", mailBase)
		}
	}
	userdb := ctx == "passdb" || ctx == "userdb"
	switch {
	case userdb && (name == "default_fields" || name == "override_fields"):
		for _, f := range blankFields(v) {
			if err := c.dovecotField(f); err != nil {
				return notAllowed(fmt.Sprintf("%s %s", what, f), "%v", err)
			}
		}
	case userdb && name == "args":
		// The passwd-file is the panel's users file and no other: the extra
		// fields of another file's lines (userdb_mail_plugin_dir,
		// userdb_mail_access_groups, userdb_system_groups_user) would load
		// plugins into and hand groups to the mail processes.
		for _, f := range blankFields(v) {
			if !strings.Contains(f, "=") && path.Clean(f) != dovecotUsers {
				return notAllowed(fmt.Sprintf("%s %s", what, f), "the passwd-file is the panel's, %s, whose lines this check reads", dovecotUsers)
			}
		}
	case name == "passwd_file_path":
		if path.Clean(v) != dovecotUsers {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "the passwd-file is the panel's, %s, whose lines this check reads", dovecotUsers)
		}
	case userdb && name == "driver":
		if v != "passwd-file" {
			return notAllowed(fmt.Sprintf("dovecot %s driver %q", ctx, v), "the panel's users are in a passwd-file; checkpassword and lua run programs as root, others hand out accounts the panel does not know")
		}
	case parent == "fields" && len(stack) > 1 && (stack[len(stack)-2].typ == "userdb" || stack[len(stack)-2].typ == "passdb"),
		parent == "userdb_fields" || parent == "passdb_fields":
		if err := c.dovecotField(key + "=" + v); err != nil {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "%v", err)
		}
	case ctx == "service" && name == "user":
		if !dovecotServiceUsers[v] {
			return notAllowed(fmt.Sprintf("service %s %s = %s", dcSectionName(stack), l.key, v), "the root master runs the service as that account; services run as dovecot's own users")
		}
	case ctx == "service" && (name == "group" || name == "extra_groups" || name == "privileged_group"):
		for _, g := range postfixList(v) {
			if err := c.groupAllowed(g); err != nil {
				return notAllowed(fmt.Sprintf("service %s %s = %s", dcSectionName(stack), l.key, v), "%v", err)
			}
		}
	case (ctx == "unix_listener" || ctx == "fifo_listener") && (name == "user" || name == "group"):
		if !dovecotListenerUsers[v] {
			return notAllowed(fmt.Sprintf("listener %s %s = %s", dcSectionName(stack), l.key, v), "the socket goes to postfix, the mail user or dovecot's own users; any other account could talk to a root service through it")
		}
	case (ctx == "unix_listener" || ctx == "fifo_listener") && name == "mode":
		m, err := strconv.ParseUint(v, 8, 32)
		if !dovecotModeRe.MatchString(v) || err != nil || m&0o007 != 0 {
			return notAllowed(fmt.Sprintf("listener %s mode = %s", dcSectionName(stack), v), "other users must not reach the socket")
		}
	}
	return nil
}

// dcNameDenied explains why a setting may not be set, by its name with or
// without a filter prefix. dovecot 2.4 also writes list settings as
// sections ("import_environment { LD_PRELOAD = … }"), so section types go
// through it too.
func dcNameDenied(key string) string {
	_, name := dcKeyContext(key, "")
	if why, ok := dovecotDenied[name]; ok {
		return why
	}
	switch {
	case strings.Contains(key, "executable"):
		return dovecotDenied["executable"]
	case strings.HasSuffix(key, "import_environment"):
		return dovecotDenied["import_environment"]
	case strings.HasSuffix(key, "plugin_dir"):
		return "plugins load from the directory it names"
	case strings.HasSuffix(key, "base_dir") || strings.HasSuffix(key, "state_dir"):
		return "the root master makes its sockets and writes its state there"
	case strings.HasPrefix(key, "lua") || strings.Contains(key, "_lua") || strings.Contains(key, "checkpassword"):
		return "it runs a script or a program as root"
	case strings.HasSuffix(key, "_bin_dir"):
		return "sieve scripts run the programs of the directory it names"
	}
	return ""
}

// dcGroupLists are the list settings whose entries are groups the processes
// get (dovecot 2.4 writes them as "extra_groups { name = yes }").
var dcGroupLists = map[string]bool{
	"extra_groups": true, "service_extra_groups": true, "mail_access_groups": true, "privileged_group": true,
	"service_privileged_group": true, "mail_privileged_group": true,
}

// dcSectionName is the name of the innermost section, for messages.
func dcSectionName(stack []dcSection) string {
	if len(stack) == 0 {
		return "(all)"
	}
	return stack[len(stack)-1].name
}

var dovecotListenerRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// dovecotListenerName allows a socket in dovecot's own run directory (a
// bare name) or one postfix connects to in its private directory.
func dovecotListenerName(v string) bool {
	if dovecotListenerRe.MatchString(v) && v != "." && v != ".." {
		return true
	}
	p, ok := absPath(v)
	return ok && directChild(p, "/var/spool/postfix/private") && dovecotListenerRe.MatchString(path.Base(p))
}

func dovecotDriver(v string) bool { return v == "maildir" || v == "sdbox" || v == "mdbox" }

// dovecotMailPath allows paths in the mail store, variables after it.
func dovecotMailPath(v string) bool {
	if strings.Contains(v, "~") || hasDotDot(v) {
		return false
	}
	lit := v
	if i := strings.IndexAny(lit, "%$"); i >= 0 {
		lit = lit[:i]
	}
	return lit == mailBase || strings.HasPrefix(lit, mailBase+"/")
}

// dovecotField checks a userdb field: the ids are the mail user's, the home
// is in the mail store.
func (c *checker) dovecotField(f string) error {
	if !dovecotFieldRe.MatchString(f) {
		return fmt.Errorf("only uid, gid, home and the quota are set from the userdb")
	}
	k, v, _ := strings.Cut(f, "=")
	switch k {
	case "uid":
		return c.isVmailUID(v)
	case "gid":
		return c.isVmailGID(v)
	case "home":
		if !dovecotMailPath(v) {
			return fmt.Errorf("the home %s is not in %s", v, mailBase)
		}
	case "mail":
		drv, rest, _ := strings.Cut(v, ":")
		loc, _, _ := strings.Cut(rest, ":")
		if !dovecotDriver(drv) || !dovecotMailPath(loc) {
			return fmt.Errorf("mail is maildir in %s", mailBase)
		}
	}
	return nil
}

// dovecotInclude allows the configuration files of /etc/dovecot; a
// relative path is relative to the including file. dovecot hands the path
// to glob(3) with GLOB_BRACE: braces, a backslash and a wildcard in a
// directory part would climb out of /etc/dovecot ("{..,x}", "\.\.", ".*").
func dovecotInclude(dir, directive, v string) error {
	what := fmt.Sprintf("dovecot %s %s", directive, v)
	if hasDotDot(v) {
		return notAllowed(what, "the path must not walk up with \"..\"")
	}
	if !strings.HasPrefix(v, "/") {
		v = dir + "/" + v
	}
	v = path.Clean(v)
	if !globPlain(v) {
		return notAllowed(what, "dovecot globs the path: braces, a backslash or a wildcard in a directory may match \"..\"; only the file name has a wildcard")
	}
	if !under(v, dovecotDir) {
		return notAllowed(what, "only files in %s are included: the root master reads them", dovecotDir)
	}
	if b := path.Base(v); !strings.HasSuffix(b, ".conf") && !strings.HasSuffix(b, ".ext") {
		return notAllowed(what, "an include names *.conf or *.ext files; the users file and others are not configuration")
	}
	return nil
}

var (
	dovecotUserRe  = regexp.MustCompile(`^[^\s:,/]+$`)
	dovecotExtraRe = regexp.MustCompile(`^(userdb_quota_rule=\*:storage=[0-9]+[KMGT]?|userdb_quota_storage_size=([0-9]+[KMGT]?|unlimited))$`)
)

// checkDovecotUsers allows the panel's passwd-file: address and password
// hash, no uid, gid, home or shell (dovecot takes them from the mail user)
// and a quota as the only extra field.
func checkDovecotUsers(content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, line := range lines {
		n := i + 1
		if trimBlanks(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 2 || !dovecotUserRe.MatchString(f[0]) {
			return unreadable(n, "not a user line")
		}
		for j := 2; j <= 6 && j < len(f); j++ {
			if f[j] != "" {
				return atLine(n, notAllowed(fmt.Sprintf("field %d of %s in the users file", j+1, f[0]), "uid, gid, home and shell come from the mail user; a user line must leave them empty"))
			}
		}
		if len(f) > 7 {
			for _, e := range blankFields(strings.Join(f[7:], ":")) {
				if !dovecotExtraRe.MatchString(e) {
					return atLine(n, notAllowed(fmt.Sprintf("extra field %q of %s in the users file", e, f[0]), "only the quota is set per user; other fields change the account, the home or the settings of the mail process"))
				}
			}
		}
	}
	return nil
}
