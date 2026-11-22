package confcheck

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// memcached: Debian's unit runs a wrapper as root that reads
// /etc/memcached.conf, opens its logfile as root and execs memcached with
// the options, which then switches to -u (and stays root with -u root). EL's
// unit reads /etc/sysconfig/memcached as an EnvironmentFile and puts USER
// and OPTIONS on the command line. The panel writes the memory, the port,
// the address, the connections and the distribution's user.

const (
	memcachedConf      = "/etc/memcached.conf"
	memcachedSysconfig = "/etc/sysconfig/memcached"
)

func isMemcached(p string) bool { return p == memcachedConf || p == memcachedSysconfig }

var memcachedUsers = map[string]bool{"memcache": true, "memcached": true, "nobody": true}

var (
	mcNumRe  = regexp.MustCompile(`^[0-9]+[kmgKMG]?$`)
	mcAddrRe = regexp.MustCompile(`^[0-9A-Fa-f:.,\[\]]+$`)
)

// memcachedOption checks one option of the command line; value is "" for
// a flag.
func memcachedOption(flag, value string) error {
	what := fmt.Sprintf("memcached option %s %s", flag, value)
	switch flag {
	case "-m", "-p", "-U", "-c", "-t", "-I", "-R", "-B":
		if !mcNumRe.MatchString(value) && !(flag == "-B" && (value == "auto" || value == "ascii" || value == "binary")) {
			return notAllowed(what, "it takes a number")
		}
	case "-l":
		if !mcAddrRe.MatchString(value) {
			return notAllowed(what, "it takes addresses")
		}
	case "-u":
		if !memcachedUsers[value] {
			return notAllowed(what, "memcached switches to this account and keeps root with -u root; it runs as the distribution's memcache user")
		}
	case "-P":
		// Debian's wrapper joins the lines and runs exec "memcached $params"
		// with one string, which Perl hands to /bin/sh: a ";" or "$(" in a
		// path would be a root command.
		if p, ok := absPath(value); !ok || !plainPathRe.MatchString(value) || !underAny(p, "/run/memcached", "/var/run/memcached") {
			return notAllowed(what, "the pid file is a plain path in /run/memcached (the wrapper runs the options through the shell as root)")
		}
	case "-v", "-vv", "-vvv", "-M", "-k", "-d":
		if value != "" {
			return notAllowed(what, "a flag takes no value")
		}
	default:
		return notAllowed(what, "it is not one of the options the panel writes; -e, -o ext_path and the like make memcached write files")
	}
	return nil
}

// memcachedArgs checks an option list such as "-l 127.0.0.1 -U 0".
func memcachedArgs(f []string) error {
	for i := 0; i < len(f); i++ {
		flag, value := f[i], ""
		switch flag {
		case "-v", "-vv", "-vvv", "-M", "-k", "-d":
		default:
			if i+1 >= len(f) {
				return notAllowed("memcached option "+flag, "it needs a value")
			}
			i++
			value = f[i]
		}
		if err := memcachedOption(flag, value); err != nil {
			return err
		}
	}
	return nil
}

var sysconfigLineRe = regexp.MustCompile(`^([A-Z_]+)="?([^"$\x60\\;|&<>()]*)"?$`)

func checkMemcached(_ *checker, p string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' {
			continue
		}
		var err error
		if p == memcachedConf {
			f := blankFields(s)
			if f[0] == "logfile" {
				if lp, ok := absPath(strings.Join(f[1:], " ")); !ok || len(f) != 2 || !plainPathRe.MatchString(f[1]) || !directChild(lp, "/var/log") || !strings.HasPrefix(path.Base(lp), "memcached") {
					err = notAllowed("memcached logfile "+strings.Join(f[1:], " "), "the wrapper opens the log as root; it is /var/log/memcached.log")
				}
			} else {
				err = memcachedArgs(f)
			}
		} else {
			m := sysconfigLineRe.FindStringSubmatch(s)
			switch {
			case m == nil:
				err = notAllowed(fmt.Sprintf("memcached setting %q", s), "the file is the environment of a root service: plain VARIABLE=\"value\" lines")
			case m[1] == "PORT" || m[1] == "MAXCONN" || m[1] == "CACHESIZE":
				if !mcNumRe.MatchString(m[2]) {
					err = notAllowed(fmt.Sprintf("memcached %s=%s", m[1], m[2]), "it takes a number")
				}
			case m[1] == "USER":
				if !memcachedUsers[m[2]] {
					err = notAllowed(fmt.Sprintf("memcached USER=%s", m[2]), "memcached switches to this account; it runs as the distribution's memcached user")
				}
			case m[1] == "OPTIONS":
				err = memcachedArgs(blankFields(m[2]))
			default:
				err = notAllowed(fmt.Sprintf("memcached variable %s", m[1]), "the panel sets PORT, USER, MAXCONN, CACHESIZE and OPTIONS; other variables reach the root service's environment (LD_PRELOAD)")
			}
		}
		if err := atLine(i+1, err); err != nil {
			return err
		}
	}
	return nil
}

// Sphinx: searchd reads its configuration, and the indexer runs the
// commands of xmlpipe and tsvpipe sources; a configuration that starts with
// #! is run as a script. The panel writes one real-time index and the
// searchd section, and Debian's switch to start the daemon.

const sphinxDefaults = "/etc/default/sphinxsearch"

func isSphinx(p string) bool {
	return under(p, "/etc/sphinxsearch") || under(p, "/etc/sphinx") || p == sphinxDefaults
}

var sphinxDataDirs = []string{"/var/lib/sphinxsearch", "/var/lib/sphinx"}
var sphinxLogDirs = []string{"/var/log/sphinxsearch", "/var/log/sphinx"}
var sphinxRunDirs = []string{"/run/sphinxsearch", "/run/sphinx", "/var/run/sphinxsearch", "/var/run/sphinx"}

func checkSphinx(_ *checker, p string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	if p == sphinxDefaults {
		for i, raw := range lines {
			s := trimBlanks(raw)
			if s == "" || s[0] == '#' {
				continue
			}
			if s != "START=yes" && s != "START=no" {
				return atLine(i+1, notAllowed(fmt.Sprintf("the line %q", s), "the init script sources the file as root; it holds START=yes"))
			}
		}
		return nil
	}
	if !strings.HasSuffix(p, ".conf") {
		return notAllowed("writing "+p, "the panel writes sphinx.conf")
	}
	if strings.HasPrefix(string(content), "#!") {
		return notAllowed("a sphinx.conf starting with #!", "Sphinx runs such a configuration as a script")
	}
	for i, raw := range lines {
		n := i + 1
		s := trimBlanks(raw)
		// Whether Sphinx strips a comment before it joins a continued line
		// decides what the next line is; the panel continues none.
		if strings.HasSuffix(s, "\\") {
			return atLine(n, notAllowed("a continued line in sphinx.conf", "Sphinx and this check could join it differently; the panel writes one setting per line"))
		}
		if s == "" || s[0] == '#' || s == "{" || s == "}" || !strings.Contains(s, "=") {
			continue
		}
		k, v, _ := strings.Cut(s, "=")
		// Sphinx reads the configuration as a stream of tokens: a section's
		// name and brace may share the line with its first key
		// ("index x { path = …"). The key is the word right before "=".
		if j := strings.LastIndexAny(k, "{}"); j >= 0 {
			k = k[j+1:]
		}
		if f := blankFields(k); len(f) > 0 {
			k = f[len(f)-1]
		}
		k, v = strings.ToLower(trimBlanks(k)), trimBlanks(v)
		what := fmt.Sprintf("Sphinx setting %q", k)
		var err error
		switch {
		case strings.HasSuffix(k, "_command") || strings.Contains(k, "pipe"):
			err = notAllowed(what, "the indexer runs the command")
		case k == "plugin_dir" || k == "sphinxql_state" || k == "snippets_file_prefix" || k == "lemmatizer_base":
			err = notAllowed(what, "the panel never sets it; it loads code or reads and writes files of its choosing")
		case k == "log" || k == "query_log":
			err = sphinxPath(what, v, sphinxLogDirs)
		case k == "pid_file":
			err = sphinxPath(what, v, sphinxRunDirs)
		case k == "path" || k == "binlog_path":
			err = sphinxPath(what, v, sphinxDataDirs)
		}
		if err := atLine(n, err); err != nil {
			return err
		}
	}
	return nil
}

func sphinxPath(what, v string, dirs []string) error {
	p, ok := absPath(v)
	if ok && (underAny(p, dirs...) || containsDir(p, dirs)) {
		return nil
	}
	return notAllowed(fmt.Sprintf("%s = %s", what, v), "searchd writes there; it belongs in %s", strings.Join(dirs, " or "))
}

func containsDir(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d {
			return true
		}
	}
	return false
}
