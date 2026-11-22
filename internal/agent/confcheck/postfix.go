package confcheck

import (
	"fmt"
	"regexp"
	"strings"
)

// postfix: the master runs as root and starts every daemon from
// daemon_directory; local, virtual, pipe and spawn run as root until they
// switch to the account a setting names; postlogd opens maillog_file as
// root; postfix-script, post-install and newaliases (root, also from the
// package's maintainer scripts) take programs and directories from
// main.cf. The panel writes main.cf and master.cf (and a stub of both while
// the package installs) and the lookup tables in /etc/postfix/monopanel.

const postfixDir = "/etc/postfix"

func isPostfix(p string) bool { return under(p, postfixDir) }

func checkPostfix(c *checker, p string, content []byte) error {
	switch {
	case p == postfixDir+"/main.cf":
		return c.checkMainCf(content)
	case p == postfixDir+"/master.cf":
		return c.checkMasterCf(content)
	case directChild(p, postfixDir+"/monopanel"):
		// Lookup tables (domains, mailboxes, aliases, senders, lenient,
		// header checks): data that postmap compiles, nothing in them runs.
		_, err := textLines(content)
		return err
	}
	return notAllowed("writing "+p, "the panel writes main.cf, master.cf and the tables in %s/monopanel; postfix reads the other files there as root (dynamicmaps.cf loads libraries, postfix-files sets owners)", postfixDir)
}

// postfixDistro are the values the distributions give the path and identity
// settings (Debian, EL): main.cf may spell them out, nothing else.
var postfixDistro = map[string][]string{
	"daemon_directory":  {"/usr/lib/postfix/sbin", "/usr/libexec/postfix"},
	"command_directory": {"/usr/sbin"},
	"shlib_directory":   {"/usr/lib/postfix", "/usr/lib64/postfix", "no"},
	"meta_directory":    {"/etc/postfix"},
	"queue_directory":   {"/var/spool/postfix"},
	"data_directory":    {"/var/lib/postfix"},
	"sample_directory":  {"/etc/postfix", "no"},
	"html_directory":    {"no", "/usr/share/doc/postfix/html"},
	"manpage_directory": {"/usr/share/man"},
	"readme_directory":  {"no", "/usr/share/doc/postfix", "/usr/share/doc/postfix/README_FILES"},
	"sendmail_path":     {"/usr/sbin/sendmail", "/usr/sbin/sendmail.postfix"},
	"newaliases_path":   {"/usr/bin/newaliases", "/usr/bin/newaliases.postfix"},
	"mailq_path":        {"/usr/bin/mailq", "/usr/bin/mailq.postfix"},
	"mail_owner":        {"postfix"},
	"setgid_group":      {"postdrop"},
	"default_privs":     {"nobody"},
}

// postfixDenied run a program, as root or as a recipient (a local account
// such as the cloud image's sudoer), or let one be written: the panel's mail
// is virtual and delivered by dovecot, and it sets none of them.
var postfixDenied = map[string]string{
	"import_environment":           "the variables reach every daemon root starts (LD_PRELOAD)",
	"export_environment":           "the variables reach every command postfix runs",
	"local_command_shell":          "local(8) runs commands with it",
	"debugger_command":             "the daemons run it, as root, under -D",
	"maillog_file_compressor":      "postfix logrotate runs it as root",
	"alternate_config_directories": "the set-gid programs then read configurations from there",
	"multi_instance_directories":   "postmulti starts the instances configured there as root",
	"multi_instance_wrapper":       "postmulti runs it as root",
	"multi_instance_enable":        "postmulti starts more instances as root",
	"process_id_directory":         "the master writes its pid file there",
	"config_directory":             "it moves every configuration file",
	"mailbox_command":              "local(8) runs it as the recipient, a system account included",
	"mailbox_command_maps":         "local(8) runs the commands as the recipients",
	"forward_path":                 "local(8) reads .forward files, commands included, from there",
	"home_mailbox":                 "local(8) appends to the file in the recipient's home as the recipient (~/.ssh/authorized_keys)",
	"mail_spool_directory":         "local(8) writes the mailboxes there as the recipients",
	"allow_mail_to_commands":       "it lets local(8) run commands",
	"allow_mail_to_files":          "it lets local(8) write files",
	"command_execution_directory":  "local(8) runs commands there",
	"virtual_minimum_uid":          "it lowers the lowest account virtual(8) delivers as",
	"mailbox_transport":            "it hands local mail to a transport of its choosing",
	"mailbox_transport_maps":       "it hands local mail to transports of its choosing",
	"fallback_transport":           "it hands local mail to a transport of its choosing",
	"fallback_transport_maps":      "it hands local mail to transports of its choosing",
	"luser_relay":                  "the panel never sets it",
	// postfix-tls-script, which root runs for "postfix tls …", calls it.
	"openssl_path": "root runs it for postfix tls",
	// Cyrus SASL reads smtpd.conf (and with it the plugins it loads) from
	// these directories in smtpd's start-up, as root.
	"cyrus_sasl_config_path": "smtpd reads the SASL configuration from there while it is root",
}

var postfixKeyRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

type cfLine struct {
	n    int
	text string
}

// postfixLogical joins postfix's logical lines: a line that starts with
// blanks continues the one before, and a line whose first non-blank is "#"
// is a comment wherever it is.
func postfixLogical(content []byte) ([]cfLine, error) {
	lines, err := textLines(content)
	if err != nil {
		return nil, err
	}
	var out []cfLine
	for i, l := range lines {
		t := trimBlanks(l)
		if t == "" || t[0] == '#' {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			if len(out) == 0 {
				return nil, unreadable(i+1, "a continuation line with nothing to continue")
			}
			out[len(out)-1].text += " " + t
			continue
		}
		out = append(out, cfLine{n: i + 1, text: t})
	}
	return out, nil
}

func (c *checker) checkMainCf(content []byte) error {
	lines, err := postfixLogical(content)
	if err != nil {
		return err
	}
	for _, l := range lines {
		k, v, ok := strings.Cut(l.text, "=")
		k, v = trimBlanks(k), trimBlanks(v)
		if !ok || !postfixKeyRe.MatchString(k) {
			return unreadable(l.n, "not a \"name = value\" line")
		}
		if err := atLine(l.n, c.postfixParam(k, v)); err != nil {
			return err
		}
	}
	return nil
}

var (
	postfixMapRe     = regexp.MustCompile(`^(hash|lmdb|btree|cdb|dbm):/etc/aliases$`)
	postfixStaticRe  = regexp.MustCompile(`^static:([0-9]+)$`)
	postfixCacheDBRe = regexp.MustCompile(`^(btree|lmdb|hash|cdb):(\$\{data_directory\}|\$data_directory|/var/lib/postfix)/[A-Za-z0-9_.-]+$`)
)

// postfixParam checks one parameter of main.cf or of a master.cf -o.
func (c *checker) postfixParam(k, v string) error {
	key := strings.ToLower(k)
	what := fmt.Sprintf("postfix parameter %q", k)
	if vals, ok := postfixDistro[key]; ok {
		for _, d := range vals {
			if v == d {
				return nil
			}
		}
		return notAllowed(fmt.Sprintf("%s = %s", what, v), "postfix runs programs from these directories and gives files to these accounts as root; only the distribution's value (%s) may be spelled out", strings.Join(vals, ", "))
	}
	if why, ok := postfixDenied[key]; ok {
		return notAllowed(what, "%s", why)
	}
	if err := postfixTables(what, v); err != nil {
		return err
	}
	switch key {
	case "tls_config_file":
		// Postfix 3.9 initialises OpenSSL with this file in the daemons'
		// start-up, as root; an OpenSSL configuration loads engines and
		// providers, shared objects of any path. default is the system's
		// openssl.cnf (root's), none no file at all.
		if v != "default" && v != "none" {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "the daemons load the OpenSSL configuration, engines and providers included, as root; it is default or none")
		}
	case "maillog_file":
		if v == "" || v == "/dev/stdout" {
			return nil
		}
		if p, ok := absPath(v); ok && directChild(p, "/var/log") && !strings.ContainsAny(p, "$*?[") {
			return nil
		}
		return notAllowed(fmt.Sprintf("%s = %s", what, v), "postlogd opens the log as root; it belongs right in /var/log")
	case "maillog_file_prefixes":
		for _, e := range postfixList(v) {
			if e != "/var/log" && e != "/var/log/" && e != "/dev/stdout" {
				return notAllowed(fmt.Sprintf("%s = %s", what, v), "it guards where postlogd writes as root: /var/log and /dev/stdout")
			}
		}
	case "alias_maps", "alias_database":
		for _, e := range postfixList(v) {
			if !postfixMapRe.MatchString(e) && !(key == "alias_maps" && e == "nis:mail.aliases") {
				return notAllowed(fmt.Sprintf("%s = %s", what, v), "aliases run commands as the owner of the table and newaliases writes it as root; the aliases are /etc/aliases")
			}
		}
	case "virtual_mailbox_base":
		if p, ok := absPath(v); !ok || strings.Contains(p, "$") || (p != mailBase && !under(p, mailBase)) {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "virtual(8) writes below it as the mail user; mail lives in %s", mailBase)
		}
	case "virtual_uid_maps", "virtual_gid_maps":
		m := postfixStaticRe.FindStringSubmatch(v)
		if m == nil {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "virtual(8) delivers as the panel's mail user, static:<id>")
		}
		check := c.isVmailUID
		if key == "virtual_gid_maps" {
			check = c.isVmailGID
		}
		if err := check(m[1]); err != nil {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "virtual(8) delivers as the panel's mail user: %v", err)
		}
	case "smtpd_tls_session_cache_database", "smtp_tls_session_cache_database", "lmtp_tls_session_cache_database":
		if !postfixCacheDBRe.MatchString(v) {
			return notAllowed(fmt.Sprintf("%s = %s", what, v), "the cache is a file in postfix's data directory")
		}
	}
	return nil
}

// postfixList splits a list value on blanks and commas.
func postfixList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == '\t' || r == ',' })
}

// postfixFileTables are the lookup table types that read a file of their
// own. Daemons open their tables in their start-up, as root, and some
// report the lines they cannot parse (cidr quotes them): a table is in
// /etc/postfix, /etc/aliases or postfix's data directory, like the panel's.
var postfixFileTables = map[string]bool{
	"btree": true, "cdb": true, "cidr": true, "dbm": true, "hash": true, "lmdb": true, "pcre": true, "regexp": true,
	"sdbm": true, "texthash": true,
}

// postfixClientTables read a configuration file of their own and hand it
// to a client library (MySQL's option files load plugins); the panel uses
// none of them.
var postfixClientTables = map[string]bool{
	"ldap": true, "memcache": true, "mongodb": true, "mysql": true, "pgsql": true, "sqlite": true,
}

var postfixTablePathRe = regexp.MustCompile(`^(/etc/postfix/[A-Za-z0-9_./-]+|/etc/aliases|(\$\{data_directory\}|\$data_directory|/var/lib/postfix)/[A-Za-z0-9_.-]+)$`)

// postfixTokens splits a value at blanks, commas and the braces of
// inline:{…}, unionmap:{…} and pipemap:{…}; ${name} stays in its token.
func postfixTokens(v string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(v); i++ {
		switch ch := v[i]; {
		case ch == '$' && i+1 < len(v) && v[i+1] == '{':
			end := strings.IndexByte(v[i:], '}')
			if end < 0 {
				end = len(v) - i - 1
			}
			cur.WriteString(v[i : i+end+1])
			i += end
		case ch == ' ' || ch == '\t' || ch == ',' || ch == '{' || ch == '}':
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return out
}

// postfixTables checks every "type:name" a value names, inside the braces
// of inline:, unionmap: and pipemap: and behind proxy: too.
func postfixTables(what, v string) error {
	for _, t := range postfixTokens(v) {
		for strings.HasPrefix(t, "proxy:") {
			t = strings.TrimPrefix(t, "proxy:")
		}
		typ, name, ok := strings.Cut(t, ":")
		switch {
		case !ok:
		case postfixClientTables[typ]:
			return notAllowed(fmt.Sprintf("%s table %s", what, t), "the %s client reads the table's configuration file and the options it names (plugins included)", typ)
		case postfixFileTables[typ]:
			if hasDotDot(name) || !postfixTablePathRe.MatchString(name) {
				return notAllowed(fmt.Sprintf("%s table %s", what, t), "the daemons open their tables as root and report lines they cannot parse; the tables are in /etc/postfix, /etc/aliases or the data directory")
			}
		}
	}
	return nil
}

// postfixDaemons are the programs of postfix's daemon_directory a
// master.cf service may run. postlogd needs root to open the log, local,
// virtual, pipe and spawn to switch to the recipient: only they may run
// with unpriv "n".
var postfixDaemons = map[string]bool{
	"smtpd": true, "smtp": true, "lmtp": true, "pickup": true, "cleanup": true, "qmgr": true, "oqmgr": true, "tlsmgr": true,
	"trivial-rewrite": true, "bounce": true, "verify": true, "flush": true, "proxymap": true, "showq": true, "error": true,
	"discard": true, "local": true, "virtual": true, "anvil": true, "scache": true, "postlogd": true, "postscreen": true,
	"dnsblog": true, "tlsproxy": true, "pipe": true, "spawn": true, "qmqpd": true,
}

var postfixPrivileged = map[string]bool{"local": true, "virtual": true, "pipe": true, "spawn": true}

// pipeDeniedUsers are system accounts a root process trusts with files, so
// a program running as one of them is a step to root: the web servers own
// nginx's temp and cache directories and EL's php-fpm log directory, which
// root follows symlinks in; EL's mysqld_pre_systemd and mysql_ssl_rsa_setup
// create and chown files in mysql's data directory as root; dovecot's and
// opendkim's accounts hold the mail secrets. The panel's master.cf has no
// pipe or spawn service at all.
var pipeDeniedUsers = map[string]string{
	"www-data": "root follows symlinks in the web server's directories",
	"apache":   "root follows symlinks in the web server's directories",
	"nginx":    "root follows symlinks in the web server's directories",
	"mysql":    "root's MySQL start scripts follow symlinks in its data directory",
	"dovecot":  "it holds dovecot's secrets", "dovenull": "it is dovecot's login account",
	"opendkim": "it holds the DKIM keys",
}

var (
	masterTypes   = map[string]bool{"inet": true, "unix": true, "unix-dgram": true, "fifo": true, "pass": true}
	masterFlagRe  = regexp.MustCompile(`^[yn-]$`)
	masterWakeRe  = regexp.MustCompile(`^(-|[0-9]+\??)$`)
	masterProcRe  = regexp.MustCompile(`^(-|[0-9]+)$`)
	masterNameRe  = regexp.MustCompile(`^[A-Za-z0-9_.:\[\]-]+$`)
	pipeUserRe    = regexp.MustCompile(`^[a-z_][a-z0-9_.-]*(:[a-z_][a-z0-9_.-]*)?$`)
	pipeAttrKeyRe = regexp.MustCompile(`^(flags|user|argv|size|eol|null_sender|directory|transport|nexthop)=`)
)

func (c *checker) checkMasterCf(content []byte) error {
	lines, err := postfixLogical(content)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if err := atLine(l.n, c.masterService(l.text)); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) masterService(line string) error {
	f := blankFields(line)
	if len(f) < 8 {
		return fmt.Errorf("a master.cf service has eight columns: %w", errUnreadable)
	}
	service, typ, unpriv, command := f[0], f[1], f[3], f[7]
	what := fmt.Sprintf("postfix service %q", service)
	if !masterNameRe.MatchString(service) || !masterTypes[typ] || !masterFlagRe.MatchString(f[2]) || !masterFlagRe.MatchString(unpriv) ||
		!masterFlagRe.MatchString(f[4]) || !masterWakeRe.MatchString(f[5]) || !masterProcRe.MatchString(f[6]) {
		return fmt.Errorf("%s: bad columns: %w", what, errUnreadable)
	}
	if !postfixDaemons[command] {
		return notAllowed(fmt.Sprintf("%s command %q", what, command), "the master runs it as root from the daemon directory; only postfix's own daemons may be named")
	}
	if unpriv == "n" && !postfixPrivileged[command] {
		return notAllowed(fmt.Sprintf("%s with unpriv \"n\"", what), "%s would keep root's privileges", command)
	}
	args, err := masterArgs(f[8:])
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	pipe := command == "pipe" || command == "spawn"
	hasUser := false
	for _, a := range args {
		switch {
		case a.opt:
			if err := c.postfixParam(a.key, a.val); err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
		case a.flag == "-v":
		case pipe && a.flag == "":
			k, v, _ := strings.Cut(a.attr, "=")
			switch k {
			case "user":
				hasUser = true
				if !pipeUserRe.MatchString(v) {
					return notAllowed(fmt.Sprintf("%s user=%s", what, v), "not an account name")
				}
				u, g, _ := strings.Cut(v, ":")
				if err := c.runAsAllowed(u); err != nil {
					return notAllowed(fmt.Sprintf("%s user=%s", what, v), "%s runs the command as that account: %v", command, err)
				}
				if err := c.groupAllowed(g); err != nil {
					return notAllowed(fmt.Sprintf("%s user=%s", what, v), "%v", err)
				}
				if why := pipeDeniedUsers[u]; why != "" {
					return notAllowed(fmt.Sprintf("%s user=%s", what, v), "%s runs the command as %s, and %s", command, u, why)
				}
				if why := pipeDeniedUsers[g]; why != "" {
					return notAllowed(fmt.Sprintf("%s user=%s", what, v), "%s runs the command with the group %s, and %s", command, g, why)
				}
			case "argv":
				if !strings.HasPrefix(v, "/") {
					return notAllowed(fmt.Sprintf("%s argv=%s", what, v), "the command needs an absolute path")
				}
			}
		default:
			return notAllowed(fmt.Sprintf("%s argument %q", what, a.flag+a.attr), "a service takes -o name=value and -v; -D runs the debugger command as root")
		}
	}
	if pipe && !hasUser {
		return notAllowed(what, "%s needs user=: the command would run with root's choice of account", command)
	}
	return nil
}

type masterArg struct {
	opt      bool
	key, val string
	flag     string
	attr     string
}

// masterArgs reads the arguments of a service: -o name=value, the
// "-o { name = value }" form of postfix 3, flags, and the attributes of
// pipe and spawn (everything after argv= belongs to the command).
func masterArgs(f []string) ([]masterArg, error) {
	var out []masterArg
	for i := 0; i < len(f); i++ {
		a := f[i]
		switch {
		case a == "-o":
			if i+1 >= len(f) {
				return nil, fmt.Errorf("-o without a value: %w", errUnreadable)
			}
			i++
			kv := f[i]
			if strings.HasPrefix(kv, "{") {
				var b []string
				for ; i < len(f); i++ {
					b = append(b, f[i])
					if strings.HasSuffix(f[i], "}") {
						break
					}
				}
				kv = trimBlanks(strings.TrimSuffix(strings.TrimPrefix(strings.Join(b, " "), "{"), "}"))
			}
			k, v, ok := strings.Cut(kv, "=")
			k = trimBlanks(k)
			if !ok || !postfixKeyRe.MatchString(k) {
				return nil, fmt.Errorf("-o %s: %w", printable(kv), errUnreadable)
			}
			out = append(out, masterArg{opt: true, key: k, val: trimBlanks(v)})
		case strings.HasPrefix(a, "-o"):
			k, v, ok := strings.Cut(strings.TrimPrefix(a, "-o"), "=")
			if !ok || !postfixKeyRe.MatchString(k) {
				return nil, fmt.Errorf("%s: %w", printable(a), errUnreadable)
			}
			out = append(out, masterArg{opt: true, key: k, val: v})
		case strings.HasPrefix(a, "-"):
			out = append(out, masterArg{flag: a})
		case pipeAttrKeyRe.MatchString(a):
			if strings.HasPrefix(a, "argv=") {
				out = append(out, masterArg{attr: strings.Join(f[i:], " ")})
				return out, nil
			}
			out = append(out, masterArg{attr: a})
		default:
			return nil, fmt.Errorf("argument %q: %w", a, errUnreadable)
		}
	}
	return out, nil
}
