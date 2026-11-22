package confcheck

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Apache: the parent process runs as root. It loads modules and libraries,
// starts piped loggers and RewriteMap programs, opens the logs, writes the
// pid file, the scoreboard and the mutexes, and reads every Include. The
// panel writes the configuration of each account's Apache
// (apache_instance.go): its httpd.conf, the virtual hosts and the custom
// directives; administrators extend them through template overrides. Up to
// 0.8.19 it wrote a shared httpd.conf, ports.conf and the include of
// conf-available for the distribution's apache2.service, and the rest of
// /etc/apache2 is still checked the way it was then. Directives are
// case-insensitive, ${VAR} is expanded in the whole line before it is read,
// and a backslash at the end of a line joins the next one: the check reads
// the lines the same way.

var apacheDirs = []string{"/etc/apache2", "/etc/httpd"}

func isApache(p string) bool { return underAny(p, apacheDirs...) }

// apacheRoot is the ServerRoot a relative path of the file resolves against.
func apacheRoot(p string) string {
	for _, d := range apacheDirs {
		if under(p, d) {
			return d
		}
	}
	return apacheDirs[0]
}

// apacheVars are what Debian's envvars (root's, sourced by apache2ctl)
// defines: their values are the distribution's, so lines may use them.
var apacheVars = map[string]string{
	"APACHE_LOG_DIR": "/var/log/apache2", "APACHE_RUN_DIR": "/var/run/apache2", "APACHE_LOCK_DIR": "/var/lock/apache2",
	"APACHE_PID_FILE": "/var/run/apache2/apache2.pid", "APACHE_RUN_USER": "www-data", "APACHE_RUN_GROUP": "www-data",
}

var apacheVarRe = regexp.MustCompile(`\$\{([^}]*)\}`)

// apacheUsers are the accounts the children run as.
var apacheUsers = map[string]bool{"www-data": true, "apache": true, "${APACHE_RUN_USER}": true, "${APACHE_RUN_GROUP}": true}

// apacheDenied are directives that act as root in the parent, or make the
// children run as another account, and that nothing the panel writes uses.
var apacheDenied = map[string]string{
	"loadmodule":         "the root parent loads the module",
	"loadfile":           "the root parent loads the library",
	"serverroot":         "it moves every relative path, includes and logs included",
	"pidfile":            "root writes the pid file wherever it says",
	"scoreboardfile":     "root creates the scoreboard file wherever it says",
	"mutex":              "root creates the lock files wherever it says",
	"coredumpdirectory":  "the panel never sets it",
	"defaultruntimedir":  "root creates its files there",
	"chrootdir":          "the panel never sets it",
	"scriptsock":         "root removes and creates the socket wherever it says",
	"suexec":             "it switches users for CGI",
	"suexecusergroup":    "it runs CGI as the account it names",
	"assignuserid":       "mpm-itk serves requests as the account it names",
	"assignuseridexpr":   "mpm-itk serves requests as the account it names",
	"assigngroupidexpr":  "mpm-itk serves requests as the group it names",
	"limituidrange":      "mpm-itk lets requests run as other accounts",
	"limitgidrange":      "mpm-itk lets requests run as other groups",
	"enablecapabilities": "mpm-itk keeps root's capabilities",
	"ruidgid":            "mod_ruid2 serves requests as the account it names",
	"rgroups":            "mod_ruid2 adds groups to requests",
	"rdocumentchroot":    "mod_ruid2 chroots as root",
	"rmode":              "mod_ruid2 switches users",
	"rminuidgid":         "mod_ruid2 lowers the lowest account",
	"rdefaultuidgid":     "mod_ruid2 serves requests as the account it names",
	"sslcryptodevice":    "it loads an OpenSSL engine into the root parent",
	"use":                "mod_macro builds directives from arguments, a LoadModule included",
	// mod_ldap's shared cache: the root parent removes what is at the path
	// and creates its segment file there.
	"ldapsharedcachefile": "the root parent removes and creates the file it names",
}

// apacheDeniedPrefixes are modules that run embedded code or programs as
// root or as accounts of their choosing (mod_md runs its notify commands and
// keeps its store where it says).
var apacheDeniedPrefixes = []string{"perl", "lua", "python", "wsgi", "passenger", "fcgid", "jk", "md"}

type apLine struct {
	n    int
	text string
}

func checkApache(c *checker, p string, content []byte) error {
	root := apacheRoot(p)
	if confDirBelow(p, root) {
		return notAllowed("writing "+p, "a directory named *.conf matches the panel's IncludeOptional patterns, and Apache then reads every file in it as configuration")
	}
	// The READMEs the panel leaves next to the virtual hosts are read like
	// configuration too: an Include of a directory, or of a pattern that
	// matches one, makes Apache read every file in it, a README included.
	if !strings.HasSuffix(p, ".conf") && path.Base(p) != "README" {
		return notAllowed("writing "+p, "the panel writes *.conf files to Apache's directories; other files there (envvars, magic, *.load) are read or sourced as root")
	}
	inst, err := c.apacheInstance(p)
	if err != nil {
		return err
	}
	// A README of an account's Apache is included by nothing; were it, it
	// would be read as custom directives, and is checked as those.
	if inst != nil && inst.kind == instReadme {
		inst.kind = instCustom
	}
	lines, err := apacheLines(content)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if err := atLine(l.n, c.apacheLine(root, inst, l.text)); err != nil {
			return err
		}
	}
	return nil
}

// apacheLines joins continuation lines the way ap_cfg_getline does (a
// backslash right before the line feed is dropped together with it) and
// drops blank lines and comments.
func apacheLines(content []byte) ([]apLine, error) {
	raw, err := textLines(content)
	if err != nil {
		return nil, err
	}
	var out []apLine
	for i := 0; i < len(raw); i++ {
		n := i + 1
		text := raw[i]
		// A line ends with a line feed unless it is the last one.
		for strings.HasSuffix(text, "\\") && i+1 < len(raw) {
			i++
			text = text[:len(text)-1] + raw[i]
		}
		// cfg_trim_line trims apr_isspace; textLines has refused every such
		// character but the space and the tab.
		text = trimBlanks(text)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		out = append(out, apLine{n: n, text: text})
	}
	return out, nil
}

func (c *checker) apacheLine(root string, inst *apacheInst, line string) error {
	if inst != nil && strings.Contains(line, "${") {
		// apache2 -f reads no envvars: ${APACHE_LOG_DIR} would stay as it is
		// written, and a variable that is defined could only be one a
		// Define made, spelling whatever it was made to.
		return notAllowed("an Apache variable in "+inst.path, "an account's Apache reads no envvars and defines nothing")
	}
	if root != "/etc/apache2" && strings.Contains(line, "${") {
		// EL's httpd has no envvars: an undefined variable stays in the
		// line as it is written, a relative path into the ServerRoot.
		return notAllowed("an Apache variable in "+root, "only Debian's envvars defines APACHE_* variables")
	}
	for _, m := range apacheVarRe.FindAllStringSubmatch(line, -1) {
		if _, ok := apacheVars[m[1]]; !ok {
			return notAllowed(fmt.Sprintf("Apache variable %q", m[0]), "${...} is expanded before the line is read, so it could spell any directive; only the distribution's APACHE_* variables may be used")
		}
	}
	if strings.Contains(line, "${") && len(apacheVarRe.FindAllString(line, -1)) != strings.Count(line, "${") {
		return notAllowed("an unterminated ${", "Apache expands variables before it reads the line")
	}
	rest := line
	name := apWord(&rest)
	// ap_build_config_sub drops a ">" that ends the first word of any line
	// but a closing tag before it looks the directive up: "LoadModule>" is
	// LoadModule, "<IfModule>" a section.
	if len(name) < 2 || name[1] != '/' {
		name = strings.TrimSuffix(name, ">")
	}
	lname := strings.ToLower(name)
	what := fmt.Sprintf("Apache directive %q", name)
	switch {
	case strings.HasPrefix(lname, "</"):
		return nil
	case strings.HasPrefix(lname, "<"):
		section := strings.TrimSuffix(strings.TrimPrefix(lname, "<"), ">")
		switch {
		case section == "macro":
			return notAllowed("Apache section <Macro>", "mod_macro builds directives from arguments, a LoadModule included")
		case section == "perl" || section == "lua":
			return notAllowed(fmt.Sprintf("Apache section <%s>", section), "it runs embedded code in the root parent")
		}
		return nil
	}
	args := apWords(rest)
	// An account's Apache: its httpd.conf sets, with the exact values, what
	// is refused everywhere else (LoadModule, PidFile, Mutex, ServerRoot …);
	// the logs go to the account's own directory.
	if inst != nil {
		if handled, err := c.apacheInstanceLine(inst, name, lname, args); handled {
			return err
		}
	}
	if why, ok := apacheDenied[lname]; ok {
		return notAllowed(what, "%s", why)
	}
	for _, pre := range apacheDeniedPrefixes {
		if strings.HasPrefix(lname, pre) {
			return notAllowed(what, "the module runs embedded code or programs as root or as accounts of its choosing")
		}
	}
	for _, a := range args {
		// Piped logs of every module (mod_log_config, mod_log_forensic,
		// mod_security's SecAuditLog2, mod_jk) and SSLPassPhraseDialog start
		// the program behind "|" from the root parent; exec: does the same
		// for SSLPassPhraseDialog and SSLRandomSeed.
		if strings.HasPrefix(a, "|") {
			return notAllowed(fmt.Sprintf("%s %s", name, a), "a piped argument is a program the root parent starts")
		}
		if strings.HasPrefix(strings.ToLower(a), "exec:") {
			return notAllowed(fmt.Sprintf("%s %s", name, a), "exec: runs a program as root at start")
		}
	}
	switch lname {
	case "include", "includeoptional":
		if len(args) == 0 {
			return nil
		}
		return apacheInclude(root, args[len(args)-1])
	case "user", "group":
		if len(args) != 1 || !apacheUsers[args[0]] {
			return notAllowed(fmt.Sprintf("Apache %s %s", name, strings.Join(args, " ")), "the children run as the web server's own account (www-data or apache), never as root or an account the API chooses")
		}
		return nil
	case "define", "undefine":
		if len(args) > 0 {
			if _, ok := apacheVars[args[0]]; ok {
				return notAllowed(fmt.Sprintf("Apache %s %s", name, args[0]), "it would change a path the distribution's variables stand for")
			}
		}
		return nil
	case "rewritemap":
		if len(args) >= 2 && strings.HasPrefix(strings.ToLower(args[1]), "prg:") {
			return notAllowed(fmt.Sprintf("RewriteMap %s %s", args[0], args[1]), "a prg: map is a program the root parent starts")
		}
		return nil
	case "sslpassphrasedialog":
		if len(args) == 1 && args[0] == "builtin" {
			return nil
		}
		return notAllowed(fmt.Sprintf("SSLPassPhraseDialog %s", strings.Join(args, " ")), "exec: and | run a program as root at start")
	case "sslrandomseed":
		for _, a := range args {
			if strings.HasPrefix(strings.ToLower(a), "exec:") {
				return notAllowed(fmt.Sprintf("SSLRandomSeed %s", a), "exec: runs a program as root at start")
			}
		}
		return nil
	case "typesconfig", "mimemagicfile", "remoteiptrustedproxylist", "remoteipinternalproxylist":
		// The root parent reads these files while it parses, and mod_remoteip
		// quotes the token it cannot read ("RemoteIP: Error parsing IP …")
		// into the error apachectl -t prints, which the agent hands back.
		if len(args) == 1 {
			v := expandApacheVars(args[0])
			if !strings.HasPrefix(v, "/") {
				v = root + "/" + v
			}
			if p, ok := absPath(v); ok && (p == "/etc/mime.types" || under(p, root)) {
				return nil
			}
		}
		return notAllowed(fmt.Sprintf("%s %s", name, strings.Join(args, " ")), "the root parent reads it and reports its lines")
	}
	// Shared-memory and DBM caches (SSL, sessions, authn, cache) are files
	// the root parent creates where they say.
	for _, a := range args {
		kind, rest, ok := strings.Cut(a, ":")
		switch strings.ToLower(kind) {
		case "shmcb", "dbm", "shm":
			if !ok {
				continue
			}
			p := rest
			if i := strings.IndexByte(p, '('); i >= 0 {
				p = p[:i]
			}
			p = expandApacheVars(p)
			if v, ok := absPath(p); !ok || !underAny(v, "/run/apache2", "/var/run/apache2", "/run/httpd", "/var/run/httpd") {
				return notAllowed(fmt.Sprintf("%s %s", name, a), "root creates the cache file; it belongs in the web server's run directory")
			}
		}
	}
	if isApacheLog(lname) && len(args) > 0 {
		return c.apacheLog(root, name, args[0])
	}
	return nil
}

func expandApacheVars(v string) string {
	return apacheVarRe.ReplaceAllStringFunc(v, func(m string) string {
		if val, ok := apacheVars[m[2:len(m)-1]]; ok {
			return val
		}
		return m
	})
}

// apacheLog allows syslog and files in Apache's log directories or the site
// log tree; a piped log is a program the root parent starts.
func (c *checker) apacheLog(root, name, v string) error {
	what := fmt.Sprintf("Apache %s %s", name, v)
	if strings.HasPrefix(v, "|") {
		return notAllowed(what, "a piped log is a program the root parent starts")
	}
	if v == "syslog" || strings.HasPrefix(v, "syslog:") || v == "/dev/null" || v == "/dev/stderr" {
		return nil
	}
	e := expandApacheVars(v)
	if !strings.HasPrefix(e, "/") {
		// Relative to ServerRoot: EL's logs is a symlink to /var/log/httpd.
		if strings.HasPrefix(e, "logs/") && !hasDotDot(e) {
			return nil
		}
		return notAllowed(what, "the root parent opens the log; a relative path lands in the configuration directory")
	}
	if p, ok := absPath(e); ok && underAny(p, "/var/log/apache2", "/var/log/httpd", c.l.siteLogRoot()) {
		return nil
	}
	return notAllowed(what, "the root parent opens the log and appends to it; logs go to /var/log/apache2, /var/log/httpd, %s or syslog", c.l.siteLogRoot())
}

// apacheInclude allows *.conf files in Apache's own directories, each of
// which goes through this check when the panel writes it.
func apacheInclude(root, v string) error {
	what := fmt.Sprintf("Apache Include %q", v)
	e := expandApacheVars(v)
	if hasDotDot(e) {
		return notAllowed(what, "the path must not walk up with \"..\"")
	}
	if !strings.HasPrefix(e, "/") {
		e = root + "/" + e
	}
	e = path.Clean(e)
	if !underAny(e, apacheDirs...) {
		return notAllowed(what, "only files in %s are included: what lies elsewhere has not been checked, and the root parent reads it", strings.Join(apacheDirs, " or "))
	}
	if !strings.HasSuffix(path.Base(e), ".conf") {
		return notAllowed(what, "an Include names *.conf files: a directory or another name pulls in files that are not configuration")
	}
	return nil
}

// apSpace is apr_isspace, which ap_getword_conf splits words at (textLines
// leaves only the space and the tab of it).
const apSpace = " \t\n\v\f\r"

// apWord is ap_getword_conf: a quoted or bare word, quotes and the
// backslashes before them removed.
func apWord(line *string) string {
	s := strings.TrimLeft(*line, apSpace)
	if s == "" {
		*line = ""
		return ""
	}
	var res string
	if q := s[0]; q == '"' || q == '\'' {
		end := 1
		for end < len(s) && s[end] != q {
			if s[end] == '\\' && end+1 < len(s) && (s[end+1] == q || s[end+1] == '\\') {
				end += 2
			} else {
				end++
			}
		}
		res = apSubstring(s[1:min(end, len(s))], q)
		if end < len(s) {
			end++
		}
		s = s[end:]
	} else {
		end := strings.IndexAny(s, apSpace)
		if end < 0 {
			end = len(s)
		}
		res = apSubstring(s[:end], 0)
		s = s[end:]
	}
	*line = strings.TrimLeft(s, apSpace)
	return res
}

// apSubstring is substring_conf: "\\" and, in quotes, the escaped quote
// lose their backslash.
func apSubstring(s string, q byte) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\\' || (q != 0 && s[i+1] == q)) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func apWords(s string) []string {
	var out []string
	for {
		w := apWord(&s)
		if w == "" && s == "" {
			return out
		}
		out = append(out, w)
	}
}
