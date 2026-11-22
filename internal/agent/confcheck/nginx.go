package confcheck

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// nginx: the master process runs as root. It loads modules, opens the log
// files (appending as root, and handing them to the worker user on every
// USR1), writes the pid file, creates and chowns the temp and cache
// directories, reads every include, and re-execs itself with the env it is
// told to keep on a binary upgrade. A site's custom directives (mp site
// nginx --set) land in the same tree and go through the same rules, so the
// check refuses only what reaches root and leaves ordinary directives alone.
// One more thing reaches it through another account: the FastCGI servers
// the workers connect to run code as theirs. The writer of a configuration
// may hand the panel's pools a request — they run the sites' PHP as the
// sites' accounts, which the API acts for anyway — and nothing else: no
// distribution's pool (www-data's, apache's), no FastCGI server of another
// daemon, no ini lines of its own for a pool to load an extension from, no
// distribution's location that hands PHP to its own pool (EL's default.d),
// and no client's bytes at the head of an upstream connection, which would
// speak FastCGI to any socket nginx's user may open: a FastCGI record opens
// with its version, 1, and a type, 1 or 5, so the first bytes nginx sends
// decide it. HTTP's are the method (proxy_method a word, never a variable
// or empty), uwsgi's its modifier1 (0, never FastCGI's version), and the
// stream module relays the client's own (no unix socket behind it, by
// proxy_pass or an upstream's server). SCGI's netstring, gRPC's preface
// and memcached's command open with bytes no FastCGI record has.

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
	// The backend secrets of the accounts' Apaches, one form each
	// (apache_backend.go); nothing else, a README neither, lies there.
	if under(p, NginxApacheBackendDir) {
		return c.checkNginxApacheBackend(p, content)
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
	return c.walkNginx(p, stmts, "", false)
}

// walkNginx checks stmts, the directives of a block named parent; stream
// tells that they lie inside a stream block, which the check sees whole:
// nothing is included there (nginxStream).
func (c *checker) walkNginx(p string, stmts []ngxStmt, parent string, stream bool) error {
	for _, s := range stmts {
		if nginxDataBlocks[parent] && s.name != "include" {
			continue
		}
		var err error
		if _, secret := nginxBackendTarget(s); secret {
			// An account's backend secret: allowed by the block it stands in.
			err = c.nginxBackendInclude(stmts, parent, s)
		} else {
			err = c.nginxDirective(p, s)
		}
		if err == nil && !stream && strings.EqualFold(s.name, "proxy_pass") {
			// To an account's Apache with the account's secret alone.
			err = c.nginxApacheProxy(stmts, s)
		}
		if err == nil && stream {
			err = nginxStream(s, parent)
		}
		if err := atLine(s.line, err); err != nil {
			return err
		}
		if s.isBlk {
			// nginx's names are case-sensitive; taking STREAM for stream
			// refuses more, never less.
			if err := c.walkNginx(p, s.block, s.name, stream || strings.EqualFold(s.name, "stream")); err != nil {
				return err
			}
		}
	}
	return nil
}

// nginxStream refuses, inside a stream block, what would relay a client's
// bytes to a unix socket: the stream module copies them as they come, and a
// socket nginx's user may reach where others may not (a php-fpm pool with
// nginx in its listen.acl_users) would read them as FastCGI requests of
// that user's — from any local process that connects to the stream server.
// proxy_pass to unix: and to a variable are refused wherever they stand
// (nginxDirective); here an upstream's server, which stands in for them,
// and include: the check reads a file on its own, so a stream server or
// upstream from an included file would be checked as an HTTP one.
func nginxStream(s ngxStmt, parent string) error {
	name := strings.ToLower(s.name)
	what := fmt.Sprintf("nginx directive %q in a stream block", s.name)
	switch {
	case name == "include":
		return notAllowed(what, "a stream block holds its servers and upstreams itself: the check reads an included file on its own, as HTTP configuration, and the stream module relays raw bytes")
	case name == "server" && strings.EqualFold(parent, "upstream"):
		if len(s.args) > 0 && strings.HasPrefix(strings.ToLower(s.args[0]), "unix:") {
			return notAllowed(fmt.Sprintf("%s %s", what, s.args[0]), "the stream module relays a client's raw bytes to the upstream's servers: behind a unix socket that nginx's user may open and others may not, they would reach a server as that user; stream upstreams are TCP addresses")
		}
	}
	return nil
}

func (c *checker) nginxDirective(p string, s ngxStmt) error {
	name := strings.ToLower(s.name)
	what := fmt.Sprintf("nginx directive %q", s.name)
	// The header nginx proves itself to an account's Apache with is set by
	// the account's file alone (apache_backend.go): written anywhere else it
	// would be a secret in a world-readable file, or one sent where it does
	// not belong.
	for _, a := range append([]string{s.name}, s.args...) {
		if namesBackendHeader(a) {
			return notAllowed(fmt.Sprintf("%s %s", what, strings.Join(s.args, " ")), "only %s/<login>.conf names %s, the secret nginx sends the account's Apache", NginxApacheBackendDir, ApacheBackendHeader)
		}
	}
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
	case name == "fastcgi_pass":
		return c.nginxFastCGIPass(what, s.args)
	case name == "fastcgi_param":
		// php-fpm reads these two parameters of a request as ini lines and
		// applies them to the worker that serves it, PHP_ADMIN_VALUE with an
		// administrator's rights: extension= loads any shared object, and
		// open_basedir, disable_functions and the rest the pool sets give
		// way. No template sets them; a site's PHP settings are the pool's
		// php_value lines (mp site set --ini).
		if len(s.args) > 0 && (strings.EqualFold(s.args[0], "PHP_VALUE") || strings.EqualFold(s.args[0], "PHP_ADMIN_VALUE")) {
			return notAllowed(fmt.Sprintf("%s %s", what, s.args[0]), "php-fpm applies it to the pool's worker as ini lines (extension= loads any library); a site's PHP settings belong to its pool (mp site set --ini)")
		}
		return nil
	case name == "upstream":
		// fastcgi_pass unix:<path> looks the path up among the upstreams by
		// name: a block named like a path could stand in for a socket the
		// check allows with servers it never saw.
		if len(s.args) > 0 && strings.Contains(s.args[0], "/") {
			return notAllowed(fmt.Sprintf("%s %s", what, s.args[0]), "an upstream is named by a word, not a path: nginx would take it for the unix socket of that path")
		}
		return nil
	case name == "proxy_method":
		// The method opens the request nginx sends upstream, copied as it
		// is: from a variable ($request_body above all) the client picks the
		// first bytes on the connection, enough for a FastCGI request to a
		// php-fpm socket behind proxy_pass http://unix:…, as nginx's user.
		// An empty method, or one of control bytes, leaves them to what
		// follows it, the URI — a variable of proxy_pass's. A word of
		// letters opens no FastCGI record: its version would be a letter.
		if len(s.args) != 1 || !httpMethodRe.MatchString(s.args[0]) {
			return notAllowed(fmt.Sprintf("%s %q", what, strings.Join(s.args, " ")), "the method opens the upstream request byte for byte: from a variable the client writes what php-fpm reads as FastCGI, and an empty or odd one hands that to the URI; a method is a word of letters (GET, POST, PROPFIND)")
		}
		return nil
	case name == "uwsgi_modifier1":
		// The modifier is the first byte of every request nginx sends a
		// uwsgi server, and the version of a FastCGI record is its first:
		// from 1 up a FastCGI server at the other end (a distribution's
		// php-fpm, fcgiwrap) would take the request for records of its own
		// and run what the client's bytes in it ($request_body as a
		// parameter) say, as nginx's user. 0 is the WSGI applications' and
		// nginx's default; FastCGI knows no version 0.
		if len(s.args) != 1 || s.args[0] == "" || strings.Trim(s.args[0], "0") != "" {
			return notAllowed(fmt.Sprintf("%s %q", what, strings.Join(s.args, " ")), "the modifier opens every uwsgi request, and from 1 up it is the version byte of a FastCGI record: a FastCGI server behind uwsgi_pass would run what the client sends; it stays 0")
		}
		return nil
	case name == "proxy_pass":
		// The stream module relays a connection's bytes as they come: to a
		// unix socket, which nginx's user may reach where other processes
		// may not (a php-fpm pool), any local process would speak FastCGI
		// there. unix: without a scheme is the stream module's syntax alone
		// (HTTP's is http://unix:…), and a target from a variable without a
		// scheme may be one: neither depends on the block it stands in, so
		// an included file is checked the same way.
		if len(s.args) > 0 {
			a := strings.ToLower(s.args[0])
			if strings.HasPrefix(a, "unix:") || (strings.Contains(a, "$") && !strings.HasPrefix(a, "http://") && !strings.HasPrefix(a, "https://")) {
				return notAllowed(fmt.Sprintf("%s %s", what, s.args[0]), "a stream proxy relays raw bytes, and to a unix socket only nginx's user reaches it would hand them to its server as that user; stream proxies go to TCP addresses")
			}
		}
		return nil
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

// panelSocketRe is the name of a site's php-fpm socket, <domain>.sock: the
// panel names a pool's socket after its site (ops_sites.go layoutFor, the
// webmail's too), and a site's name is a host name in lower case.
var panelSocketRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+\.sock$`)

// httpMethodRe is a method proxy_method may set: a word that starts with a
// letter, so that neither of the first two bytes nginx sends upstream (the
// method's, or its first and a blank) can be FastCGI's version 1 or one of
// its request types (1, 5).
var httpMethodRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// nginxFastCGIPass allows the socket of a site's pool alone:
// unix:<RunDir>/php/<domain>.sock, written plainly. A pool the panel writes
// listens there as an account of the panel (fpmPool) — the distributions'
// idle pools too, on .idle-<branch>.sock, no domain's name and root's
// alone — and the directory is root's, so no account can put a socket of
// its own there. Anything else — a TCP address, another unix path, a
// variable, an upstream's name — may be a distribution's pool running as
// www-data or apache (Remi lets nginx connect), or a FastCGI server of
// another daemon: whoever writes the configuration would run code as that
// account.
func (c *checker) nginxFastCGIPass(what string, args []string) error {
	if len(args) == 1 {
		if v, ok := strings.CutPrefix(args[0], "unix:"); ok && plainPathRe.MatchString(v) {
			if p, ok := absPath(v); ok && p == v && directChild(p, c.l.phpRunDir()) && panelSocketRe.MatchString(path.Base(p)) {
				return nil
			}
		}
	}
	return notAllowed(fmt.Sprintf("%s %s", what, strings.Join(args, " ")), "nginx hands the requests to the panel's pools alone: unix:%s/<domain>.sock, where a site's PHP runs as the site's account", c.l.phpRunDir())
}

// nginxDefaultD is EL's directory of the distributions' locations for the
// default server: php-fpm's php.conf and Remi's php<NN>-php.conf hand PHP to
// the distribution's pools (fastcgi_pass php-fpm, an upstream of conf.d on
// /run/php-fpm/www.sock, which runs as apache with nginx in its ACL).
const nginxDefaultD = nginxDir + "/default.d"

// nginxInclude allows includes of the panel's own files in /etc/nginx (every
// one of them goes through this check when the panel writes it) and of the
// distribution's parameter files. A relative path is relative to /etc/nginx,
// the directory of nginx.conf. A pattern with a wildcard is glob(3)'s,
// which takes a backslash for an escape and lets a wildcard in a directory
// match "..": only the file name may have one (globPlain). EL's default.d
// is not included: the distributions' locations there pass PHP to their
// own pools, past the fastcgi_pass rule.
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
	if !globPlain(full) {
		return notAllowed(what, "glob(3) reads a backslash as an escape and a wildcard in a directory as any one, \"..\" included: only the file name of an include has a wildcard, and no backslash stands anywhere")
	}
	if !under(full, nginxDir) {
		return notAllowed(what, "only files in %s are included: what lies elsewhere has not been checked, and the master reads it as root", nginxDir)
	}
	if under(full, nginxDefaultD) {
		return notAllowed(what, "the distributions' locations in %s hand PHP to their own pools (php-fpm's www pool runs as apache): nginx passes requests to the panel's pools alone", nginxDefaultD)
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
	if ok && (under(p, "/var/log/nginx") || c.l.siteLog(p)) {
		return nil
	}
	return notAllowed(fmt.Sprintf("%s %s", what, v), "the root master opens the log, appends to it and gives it to the worker user; logs go to /var/log/nginx or an account's directory of %s, stderr, syslog or off", c.l.siteLogRoot())
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
