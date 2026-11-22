package confcheck

import (
	"fmt"
	"path"
	"strings"
)

// MySQL and Percona. mysqld runs as mysql, but root starts it: the
// distribution's pre-start scripts read the configuration and create,
// chown and initialise what it names (EL's mysqld_pre_systemd chowns a file
// in the data directory as root, following symlinks), mysqld_safe preloads
// malloc-lib and runs ledir's binary as root, the agent runs mysqld
// --validate-config as root, and the client tools the agent runs as root
// read [client] and [mysql]. The panel writes zz-monopanel.cnf with a
// [mysqld] section and, on EL with Percona, a my.cnf that includes the
// directory of it.

var mysqlDirs = []string{"/etc/mysql", "/etc/my.cnf.d"}

func isMySQL(p string) bool { return underAny(p, mysqlDirs...) || p == "/etc/my.cnf" }

// mysqlDenied are server options that load code, run SQL or a program at
// start, or move what root's start scripts create and chown.
var mysqlDenied = map[string]string{
	"plugin_load":        "mysqld loads the plugin's shared object",
	"plugin_load_add":    "mysqld loads the plugin's shared object",
	"early_plugin_load":  "mysqld loads the plugin's shared object",
	"plugin_dir":         "plugins load from the directory it names",
	"init_file":          "mysqld runs the file's SQL at start",
	"init_connect":       "mysqld runs the SQL for every connection",
	"init_replica":       "mysqld runs the SQL",
	"init_slave":         "mysqld runs the SQL",
	"malloc_lib":         "mysqld_safe preloads the library as root",
	"ledir":              "mysqld_safe runs the server binary from there as root",
	"mysqld":             "mysqld_safe runs the binary it names as root",
	"basedir":            "the server's programs and plugins come from there",
	"lc_messages_dir":    "the panel never sets it",
	"character_sets_dir": "the panel never sets it",
	"datadir":            "root's start scripts create and chown what is in the data directory",
	"socket":             "root's start scripts create its directory",
	"pid_file":           "root's start scripts create its directory",
	"tmpdir":             "the panel never sets it",
	"secure_file_priv":   "it opens the server's file reads and writes",
	"grant_tables":       "skip-grant-tables lets anybody in as MySQL's root",
	"log_error":          "root's start scripts create the log",
	"general_log_file":   "the panel never sets it",
	"keyring_file_data":  "the panel never sets it",
	"user":               "",
}

// mysqlPrefixes are the prefixes my_getopt takes off an option name.
var mysqlPrefixes = []string{"loose_", "skip_", "enable_", "disable_", "maximum_"}

// mysqlSections are the groups the server reads; the client tools the
// agent runs as root read the others ([client], [mysql], [mysqldump] …) and
// mysqld_safe and mysqld_multi run programs from theirs.
func mysqlSection(name string) bool {
	n := strings.ToLower(name)
	return n == "mysqld" || n == "server" || strings.HasPrefix(n, "mysqld-")
}

func checkMySQL(c *checker, p string, content []byte) error {
	if !strings.HasSuffix(p, ".cnf") {
		return notAllowed("writing "+p, "the panel writes *.cnf files there; others in /etc/mysql are scripts root runs (debian-start)")
	}
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	section := ""
	for i, raw := range lines {
		n := i + 1
		// my_load_defaults reads a file with fgets into 4096 bytes: the rest
		// of a longer line is read as a line of its own, an option or a
		// [group] this check would take for part of a value or a comment.
		if len(raw) >= mysqlLineMax {
			return unreadable(n, "a line of %d bytes; MySQL reads a line in pieces of %d bytes, each a line of its own", len(raw), mysqlLineMax)
		}
		s := myTrim(raw)
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		if s[0] == '!' {
			if err := atLine(n, mysqlInclude(s)); err != nil {
				return err
			}
			continue
		}
		if s[0] == '[' {
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return unreadable(n, "unterminated group %q", s)
			}
			// MySQL trims the blanks before "]" only: "[ client]" is a group
			// no program reads, which is refused here all the same.
			section = myTrim(s[1:end])
			if !mysqlSection(section) {
				return atLine(n, notAllowed(fmt.Sprintf("MySQL group [%s]", section), "the panel writes [mysqld] only; client tools the agent runs as root read the client groups, and mysqld_safe and mysqld_multi run programs named in theirs"))
			}
			continue
		}
		if section == "" {
			return unreadable(n, "option outside a group")
		}
		// remove_end_comment cuts the line at a "#" outside quotes before
		// the name is looked for.
		key, val, _ := strings.Cut(myEndComment(s), "=")
		if err := atLine(n, mysqlOption(myTrim(key), myTrim(val))); err != nil {
			return err
		}
	}
	return nil
}

// mysqlLineMax is the length from which my_load_defaults splits a line
// (fgets of sizeof(buff)-1 into buff[4096] reads 4094 bytes at most).
const mysqlLineMax = 4094

// mySpace is my_isspace of my_charset_latin1, the character set option
// files are read with: the C blanks and 0xA0, a no-break space to Latin-1
// (and the second byte of some UTF-8 letters). strings.TrimSpace leaves a
// lone 0xA0 byte alone, so "\xa0plugin_dir" would pass for another name.
func mySpace(b byte) bool { return b == ' ' || (b >= '\t' && b <= '\r') || b == 0xa0 }

// myTrim trims what mySpace calls blanks on both ends.
func myTrim(s string) string {
	for len(s) > 0 && mySpace(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && mySpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// myEndComment is remove_end_comment: the text before the first "#" that
// is not inside quotes.
func myEndComment(s string) string {
	var quote byte
	escape := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch == '\'' || ch == '"') && !escape {
			if quote == 0 {
				quote = ch
			} else if quote == ch {
				quote = 0
			}
		}
		if quote == 0 && ch == '#' {
			return s[:i]
		}
		escape = quote != 0 && ch == '\\' && !escape
	}
	return s
}

func mysqlOption(key, val string) error {
	k := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	what := fmt.Sprintf("MySQL option %q", key)
	if strings.ContainsAny(k, " \t") {
		return notAllowed(what, "not an option name")
	}
	names := []string{k}
	for changed := true; changed; {
		changed = false
		for _, pre := range mysqlPrefixes {
			if s := names[len(names)-1]; strings.HasPrefix(s, pre) {
				names = append(names, strings.TrimPrefix(s, pre))
				changed = true
			}
		}
	}
	for _, n := range names {
		// Galera: wsrep_provider is a library mysqld loads, wsrep_notify_cmd
		// and the SST methods are programs it runs; the panel runs no
		// cluster.
		if strings.HasPrefix(n, "wsrep") {
			return notAllowed(what, "Galera's settings load a library and run programs in mysqld")
		}
		for d, why := range mysqlDenied {
			// An unambiguous prefix of an option is the option to older
			// servers' option parsers.
			if n == d || (len(n) >= 2 && strings.HasPrefix(d, n)) {
				if d == "user" {
					if n == "user" && strings.Trim(val, `"'`) == "mysql" {
						continue
					}
					return notAllowed(fmt.Sprintf("%s = %s", what, val), "mysqld runs as mysql; root's start scripts and mysqld switch to the user it names")
				}
				return notAllowed(what, "%s", why)
			}
		}
	}
	if k == "slow_query_log_file" {
		v := strings.Trim(val, `"'`)
		if p, ok := absPath(v); !ok || !(under(p, "/var/log/mysql") || p == "/var/log/mysqld.log" || under(p, "/var/lib/mysql")) {
			return notAllowed(fmt.Sprintf("%s = %s", what, val), "the slow log goes to /var/log/mysql")
		}
	}
	return nil
}

// mysqlInclude allows !include and !includedir of MySQL's own directories.
// my_load_defaults skips blanks after "!", wants a blank after the keyword
// and takes the rest of the line, its trailing blanks cut, for the path.
func mysqlInclude(s string) error {
	rest := myTrim(s[1:])
	directive := ""
	for _, kw := range []string{"includedir", "include"} {
		if strings.HasPrefix(rest, kw) && len(rest) > len(kw) && mySpace(rest[len(kw)]) {
			directive, rest = "!"+kw, myTrim(rest[len(kw):])
			break
		}
	}
	if directive == "" {
		return notAllowed(fmt.Sprintf("MySQL %q", s), "only !include and !includedir exist")
	}
	arg := rest
	what := fmt.Sprintf("MySQL %s %s", directive, arg)
	p, ok := absPath(arg)
	if !ok {
		return notAllowed(what, "the path must be absolute, without \"..\"")
	}
	p = path.Clean(p)
	if p == "/etc/my.cnf" || p == "/etc/mysql" || p == "/etc/my.cnf.d" || underAny(p, mysqlDirs...) {
		return nil
	}
	return notAllowed(what, "only MySQL's directories are included: the files there go through this check, files elsewhere do not")
}
