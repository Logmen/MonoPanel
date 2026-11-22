package confcheck

import (
	"fmt"
	"path"
	"strings"
)

// nginx: the master process runs as root. It loads modules, opens the log
// files (appending as root, and handing them to the worker user on every
// USR1), writes the pid file, creates and chowns the temp and cache
// directories, reads every include, and re-execs itself with the env it is
// told to keep on a binary upgrade. A site's custom directives (mp site
// nginx --set) land in the same tree and go through the same rules, so the
// check refuses only what reaches root and leaves ordinary directives alone.

const nginxDir = "/etc/nginx"

func isNginx(p string) bool { return under(p, nginxDir) }

// nginxUsers are the web server's own users: nginx.org's packages (the
// panel's) and Debian's.
var nginxUsers = map[string]bool{"nginx": true, "www-data": true}

// nginxIncludeNames are the distribution's files an include may name
// besides *.conf.
var nginxIncludeNames = map[string]bool{
	"mime.types": true, "fastcgi_params": true, "scgi_params": true, "uwsgi_params": true, "proxy_params": true,
	"koi-utf": true, "koi-win": true, "win-utf": true,
}

// nginxDataBlocks hold data, not directives: their entries are map keys,
// MIME types, addresses. Only include is a directive inside them.
var nginxDataBlocks = map[string]bool{"map": true, "geo": true, "types": true, "split_clients": true, "charset_map": true}

type ngxStmt struct {
	name  string
	args  []string
	line  int
	block []ngxStmt
	isBlk bool
}

func checkNginx(c *checker, p string, content []byte) error {
	if confDirBelow(p, nginxDir) {
		return notAllowed("writing "+p, "a directory named *.conf matches the panel's include patterns; configurations are files")
	}
	if inertName(p) {
		return nil
	}
	if strings.IndexByte(string(content), 0) >= 0 {
		return notAllowed("a NUL byte", "configuration files are text")
	}
	stmts, err := parseNginx(string(content))
	if err != nil {
		return err
	}
	return c.walkNginx(p, stmts, "")
}

func (c *checker) walkNginx(p string, stmts []ngxStmt, parent string) error {
	for _, s := range stmts {
		if nginxDataBlocks[parent] && s.name != "include" {
			continue
		}
		if err := atLine(s.line, c.nginxDirective(p, s)); err != nil {
			return err
		}
		if s.isBlk {
			if err := c.walkNginx(p, s.block, s.name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *checker) nginxDirective(p string, s ngxStmt) error {
	name := strings.ToLower(s.name)
	what := fmt.Sprintf("nginx directive %q", s.name)
	switch {
	case name == "load_module":
		return notAllowed(what, "the master process loads the module as root")
	case name == "env":
		return notAllowed(what, "the master passes the variable on to every process it starts, a re-executed master included (LD_PRELOAD)")
	case name == "ssl_engine":
		return notAllowed(what, "it loads an OpenSSL engine, a shared object, into the root master")
	case strings.HasPrefix(name, "perl"), strings.HasPrefix(name, "js_"), strings.HasPrefix(name, "lua"),
		strings.Contains(name, "_by_lua"), strings.HasPrefix(name, "mruby"), strings.HasPrefix(name, "php_"),
		strings.Contains(name, "_by_php"), strings.HasPrefix(name, "wasm"), strings.HasPrefix(name, "proxy_wasm"),
		strings.HasPrefix(name, "python"):
		return notAllowed(what, "it runs embedded code, some of it in the root master at configuration time")
	case name == "master_process":
		if len(s.args) == 1 && strings.EqualFold(s.args[0], "on") {
			return nil
		}
		return notAllowed(what+" "+strings.Join(s.args, " "), "without a master the workers keep root's privileges")
	case name == "user":
		return c.nginxUser(p, s)
	case name == "include":
		if len(s.args) != 1 {
			return notAllowed(what, "include takes one path")
		}
		return nginxInclude(s.args[0])
	case name == "error_log" || name == "access_log":
		if len(s.args) == 0 {
			return nil
		}
		if name == "access_log" && s.args[0] == "off" {
			return nil
		}
		return c.nginxLogPath(what, s.args[0])
	case strings.HasSuffix(name, "_log"):
		// rewrite_log on, ssl_key_log <file> and the like: a file a
		// *_log directive names is opened as root.
		for _, a := range s.args {
			if strings.HasPrefix(a, "/") || strings.Contains(a, "/") {
				if err := c.nginxLogPath(what, a); err != nil {
					return err
				}
			}
		}
		return nil
	case name == "pid" || name == "lock_file":
		if len(s.args) != 1 {
			return notAllowed(what, "it takes one path")
		}
		return runFile(what, s.args[0])
	case strings.HasSuffix(name, "_temp_path") || strings.HasSuffix(name, "_cache_path"):
		// ngx_create_paths runs mkdir and then chown(path, worker user) as
		// root, following symlinks. The directories below /var/cache/nginx
		// are the worker's own (nginx chowned them on an earlier start), so
		// whoever runs as the worker could put a symlink to /etc/passwd
		// there; /var/cache/nginx itself is root's (nginx.org's packages).
		if len(s.args) == 0 {
			return nil
		}
		v, ok := absPath(s.args[0])
		if !ok || !plainPathRe.MatchString(v) || !directChild(v, "/var/cache/nginx") {
			return notAllowed(fmt.Sprintf("%s %s", what, s.args[0]), "the root master creates the directory and gives it to the worker user, following symlinks; it is a directory right in /var/cache/nginx")
		}
		return nil
	case name == "listen":
		// A unix socket is bound and then chmod-ed 0666 by path as root: in
		// a directory anyone but root may change, the socket could be
		// swapped for a symlink between the two calls.
		if len(s.args) > 0 && strings.HasPrefix(s.args[0], "unix:") {
			v, ok := absPath(strings.TrimPrefix(s.args[0], "unix:"))
			if !ok || !plainPathRe.MatchString(v) || !(directChild(v, "/run") || directChild(v, "/var/run")) {
				return notAllowed(fmt.Sprintf("%s %s", what, s.args[0]), "the root master binds the socket and changes its mode by path; it belongs right in /run")
			}
		}
		return nil
	case name == "xslt_stylesheet" || name == "xml_entities":
		// libxml2 quotes the lines it cannot parse into the error nginx -t
		// prints, and the agent hands that output back to the API.
		if len(s.args) > 0 {
			if v, ok := absPath(s.args[0]); !ok || !under(v, nginxDir) {
				return notAllowed(fmt.Sprintf("%s %s", what, s.args[0]), "the root master parses the file and quotes what it cannot parse; stylesheets live in %s", nginxDir)
			}
		}
		return nil
	case strings.Contains(name, "certificate"):
		for _, a := range s.args {
			if strings.HasPrefix(strings.ToLower(a), "engine:") {
				return notAllowed(fmt.Sprintf("%s %s", what, a), "an engine: key loads an OpenSSL engine, a shared object of any path, into the root master")
			}
		}
		return nil
	}
	return nil
}

// nginxUser allows the worker user only in nginx.conf and only as the web
// server's own account.
func (c *checker) nginxUser(p string, s ngxStmt) error {
	what := fmt.Sprintf("nginx directive \"user %s\"", strings.Join(s.args, " "))
	if p != nginxDir+"/nginx.conf" {
		return notAllowed(what, "the worker user is set in %s/nginx.conf alone", nginxDir)
	}
	if len(s.args) < 1 || len(s.args) > 2 || !nginxUsers[s.args[0]] || (len(s.args) == 2 && !nginxUsers[s.args[1]]) {
		return notAllowed(what, "the workers run as the web server's own user (nginx or www-data), never as root or an account the API chooses")
	}
	return nil
}

// nginxInclude allows includes of the panel's own files in /etc/nginx (every
// one of them goes through this check when the panel writes it) and of the
// distribution's parameter files. A relative path is relative to /etc/nginx,
// the directory of nginx.conf.
func nginxInclude(v string) error {
	what := fmt.Sprintf("nginx include %q", v)
	if hasDotDot(v) || strings.ContainsAny(v, "\x00\n") {
		return notAllowed(what, "the path must not walk up with \"..\"")
	}
	full := v
	if !strings.HasPrefix(full, "/") {
		full = nginxDir + "/" + full
	}
	full = path.Clean(full)
	if !under(full, nginxDir) {
		return notAllowed(what, "only files in %s are included: what lies elsewhere has not been checked, and the master reads it as root", nginxDir)
	}
	if b := path.Base(full); !strings.HasSuffix(b, ".conf") && !nginxIncludeNames[b] {
		return notAllowed(what, "an include names *.conf files or the distribution's parameter files; anything else (READMEs, files kept for review) is not configuration")
	}
	return nil
}

// nginxLogPath allows the destinations a root master may write to: off,
// stderr, syslog, memory, /dev/null and files in /var/log/nginx or the site
// log tree (root's alone, so no account can swap a log for a symlink).
func (c *checker) nginxLogPath(what, v string) error {
	switch {
	case v == "stderr" || v == "/dev/null" || v == "/dev/stderr" || v == "/dev/stdout":
		return nil
	case strings.HasPrefix(v, "syslog:") || strings.HasPrefix(v, "memory:"):
		return nil
	}
	p, ok := absPath(v)
	if ok && underAny(p, "/var/log/nginx", c.l.siteLogRoot()) {
		return nil
	}
	return notAllowed(fmt.Sprintf("%s %s", what, v), "the root master opens the log, appends to it and gives it to the worker user; logs go to /var/log/nginx or %s, stderr, syslog or off", c.l.siteLogRoot())
}

// runFile allows a pid or lock file right in /run (or /var/run): only root
// adds entries there, and the file is written and removed as root.
func runFile(what, v string) error {
	p, ok := absPath(v)
	if ok && !strings.ContainsAny(p, "$*?[") && (directChild(p, "/run") || directChild(p, "/var/run")) {
		return nil
	}
	return notAllowed(fmt.Sprintf("%s %s", what, v), "root writes and removes the file; it belongs right in /run")
}

// parseNginx reads a configuration the way ngx_conf_parse does.
func parseNginx(s string) ([]ngxStmt, error) {
	t := &ngxTokenizer{buf: s, line: 1}
	var parse func(depth int) ([]ngxStmt, error)
	parse = func(depth int) ([]ngxStmt, error) {
		var out []ngxStmt
		for {
			words, kind, line, err := t.next()
			if err != nil {
				return nil, err
			}
			switch kind {
			case ngxEOF:
				if depth > 0 {
					return nil, unreadable(line, "unexpected end of file, expecting \"}\"")
				}
				return out, nil
			case ngxBlockEnd:
				if depth == 0 {
					return nil, unreadable(line, "unexpected \"}\"")
				}
				return out, nil
			case ngxDirective:
				out = append(out, ngxStmt{name: words[0], args: words[1:], line: line})
			case ngxBlockStart:
				body, err := parse(depth + 1)
				if err != nil {
					return nil, err
				}
				out = append(out, ngxStmt{name: words[0], args: words[1:], line: line, block: body, isBlk: true})
			}
		}
	}
	return parse(0)
}

const (
	ngxDirective = iota
	ngxBlockStart
	ngxBlockEnd
	ngxEOF
)

type ngxTokenizer struct {
	buf  string
	pos  int
	line int
}

func ngxSpace(ch byte) bool { return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' }

// next is ngx_conf_read_token: the words of one directive and what ended
// it. Quotes, backslashes, "#" comments and "${" behave as in nginx, so the
// check sees the directives nginx sees.
func (t *ngxTokenizer) next() (words []string, kind int, line int, err error) {
	var (
		found, needSpace, sharpComment, variable, quoted, sQuoted, dQuoted bool
		lastSpace                                                          = true
		start                                                              int
		startLine                                                          = t.line
	)
	for {
		if t.pos >= len(t.buf) {
			if len(words) > 0 || !lastSpace {
				return nil, 0, startLine, unreadable(startLine, "unexpected end of file, expecting \";\" or \"}\"")
			}
			return nil, ngxEOF, t.line, nil
		}
		ch := t.buf[t.pos]
		t.pos++
		if ch == '\n' {
			t.line++
			if sharpComment {
				sharpComment = false
			}
		}
		if sharpComment {
			continue
		}
		if quoted {
			quoted = false
			continue
		}
		if needSpace {
			if ngxSpace(ch) {
				lastSpace = true
				needSpace = false
				continue
			}
			switch ch {
			case ';':
				return words, ngxDirective, startLine, nil
			case '{':
				return words, ngxBlockStart, startLine, nil
			case ')':
				lastSpace = true
				needSpace = false
			default:
				return nil, 0, t.line, unreadable(t.line, "unexpected %q", ch)
			}
		}
		if lastSpace {
			start = t.pos - 1
			if len(words) == 0 {
				startLine = t.line
			}
			if ngxSpace(ch) {
				continue
			}
			switch ch {
			case ';', '{':
				if len(words) == 0 {
					return nil, 0, t.line, unreadable(t.line, "unexpected %q", ch)
				}
				if ch == '{' {
					return words, ngxBlockStart, startLine, nil
				}
				return words, ngxDirective, startLine, nil
			case '}':
				if len(words) != 0 {
					return nil, 0, t.line, unreadable(t.line, "unexpected \"}\"")
				}
				return nil, ngxBlockEnd, t.line, nil
			case '#':
				sharpComment = true
				continue
			case '\\':
				quoted = true
				lastSpace = false
				continue
			case '"':
				start++
				dQuoted = true
				lastSpace = false
				continue
			case '\'':
				start++
				sQuoted = true
				lastSpace = false
				continue
			case '$':
				variable = true
				lastSpace = false
				continue
			default:
				lastSpace = false
			}
		} else {
			if ch == '{' && variable {
				continue
			}
			variable = false
			if ch == '\\' {
				quoted = true
				continue
			}
			if ch == '$' {
				variable = true
				continue
			}
			if dQuoted {
				if ch == '"' {
					dQuoted = false
					needSpace = true
					found = true
				}
			} else if sQuoted {
				if ch == '\'' {
					sQuoted = false
					needSpace = true
					found = true
				}
			} else if ngxSpace(ch) || ch == ';' || ch == '{' {
				lastSpace = true
				found = true
			}
			if found {
				words = append(words, ngxUnescape(t.buf[start:t.pos-1]))
				if ch == ';' {
					return words, ngxDirective, startLine, nil
				}
				if ch == '{' {
					return words, ngxBlockStart, startLine, nil
				}
				found = false
			}
		}
	}
}

// ngxUnescape copies a word the way ngx_conf_read_token does.
func ngxUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\'', '\\':
				i++
			case 't':
				b.WriteByte('\t')
				i += 2
				continue
			case 'r':
				b.WriteByte('\r')
				i += 2
				continue
			case 'n':
				b.WriteByte('\n')
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
