package confcheck

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// nginx alone talks to an account's Apache. Any local process can connect to
// the Apache's address (127.100.x.y:81) and would skip what nginx enforces
// for a site — its allow list, its rate limit — and name the client address
// itself: mod_remoteip takes X-Real-IP from 127.0.0.1, where a local
// connection comes from too. So every request nginx sends an account's
// Apache carries a secret of that account, and the Apache refuses any other:
//
//	/etc/nginx/monopanel/apache/<login>.conf           proxy_set_header X-MonoPanel-Backend "<hex>";
//	/etc/apache2/monopanel/accounts/<login>/backend.conf the check (ApacheBackendConf)
//
// Both hold exactly that, with 64 hex digits, and are root's, 0600: root
// reads them (nginx's master, apache2's parent), and an account that could
// read one would send the header itself. A location that proxies to an
// account's Apache includes the account's file next to the other headers it
// sets — a level that sets one proxy_set_header drops every inherited one —
// and nothing below it proxies elsewhere; a location that proxies there
// without it is refused (the Apache would answer it 403). The account's
// httpd.conf includes its backend.conf at the top level, and every virtual
// host switches mod_rewrite on, without which it would not inherit the
// check (apacheRewriteGuard). Nothing else names the header, in nginx or in
// Apache, and no file of an account's Apache switches the check's rewriting
// off: a template override or custom directives can neither send the
// secret elsewhere nor set, drop or test the header in an Apache.

const (
	// ApacheBackendHeader is the request header nginx proves itself with.
	ApacheBackendHeader = "X-MonoPanel-Backend"
	// NginxApacheBackendDir holds the nginx side, <login>.conf per account.
	NginxApacheBackendDir = "/etc/nginx/monopanel/apache"
	// apacheBackendName is the Apache side, in the account's directory.
	apacheBackendName = "backend.conf"
)

// ApacheBackendFile is the check of the account's Apache, which its
// httpd.conf includes.
func ApacheBackendFile(login string) string {
	return path.Join(ApacheInstanceDir(login), apacheBackendName)
}

// NginxApacheBackendFile is the header nginx sends the account's Apache.
func NginxApacheBackendFile(login string) string {
	return path.Join(NginxApacheBackendDir, login+".conf")
}

var apacheBackendSecretRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidApacheBackendSecret is a secret as the panel makes them: 32 random
// bytes, in lower-case hex.
func ValidApacheBackendSecret(s string) bool { return apacheBackendSecretRe.MatchString(s) }

// ApacheBackendConf is backend.conf with the secret, the one content the
// check accepts there; httpd.conf includes it at the top level.
//
// The refusal is mod_rewrite's, at the server level: it runs when the URL is
// mapped (translate_name), before any .htaccess is read, and the virtual
// hosts inherit it ahead of their own rules (InheritDownBefore; a virtual
// host inherits only with a rewrite directive of its own, hence its
// RewriteEngine On). Authorization alone does not hold against a site's
// .htaccess, which merges into the very sections authorization reads: with
// mod_access_compat, "Satisfy Any" there lets a request in past every
// Require once Allow passes (by default, or by a forged X-Real-IP), and an
// <If> nested in an <If> of .htaccess merges after any <If> of
// httpd.conf's — Apache merges the nested ones last.
//
// The <If> in <Location "/"> is the second line, for a request the rule does
// not stop: one a module would map before mod_rewrite, or one of a virtual
// host that inherits no rule (apacheRewriteGuard refuses those it sees).
// Satisfy All there keeps a Satisfy Any from granting what the <If> denies;
// against a site's .htaccess the line does not hold on its own (a nested
// <If> merges after it). [NV] and req_novary: a test of the header
// would add "Vary: X-MonoPanel-Backend" to every answer. ProxyFCGISetEnvIf
// takes the header out of what PHP gets, and with it SCRIPT_URI and
// SCRIPT_URL (and their REDIRECT_ copies), which mod_rewrite adds once a
// virtual host turns its engine on: up to 0.8.19 PHP never got them, and
// SCRIPT_URI would say http on an https site (nginx speaks http to the
// Apache). RequestHeader unset would take it
// out of the request itself, in the fixups, before an internal redirect (a
// front controller's RewriteRule in .htaccess, a local ErrorDocument) runs
// the check again on the same headers — and every such request would be
// denied.
func ApacheBackendConf(secret string) string {
	return "RewriteEngine On\n" +
		"RewriteOptions InheritDownBefore\n" +
		"RewriteCond %{HTTP:" + ApacheBackendHeader + "} !=" + secret + " [NV]\n" +
		"RewriteRule ^ - [F]\n" +
		"<Location \"/\">\n" +
		"    <If \"req_novary('" + ApacheBackendHeader + "') != '" + secret + "'\">\n" +
		"        Require all denied\n" +
		"        <IfModule access_compat_module>\n" +
		"            Satisfy All\n" +
		"        </IfModule>\n" +
		"    </If>\n" +
		"    ProxyFCGISetEnvIf \"true\" !HTTP_X_MONOPANEL_BACKEND\n" +
		"    ProxyFCGISetEnvIf \"true\" !SCRIPT_URI\n" +
		"    ProxyFCGISetEnvIf \"true\" !SCRIPT_URL\n" +
		"    ProxyFCGISetEnvIf \"true\" !REDIRECT_SCRIPT_URI\n" +
		"    ProxyFCGISetEnvIf \"true\" !REDIRECT_SCRIPT_URL\n" +
		"</Location>\n"
}

// NginxApacheBackendConf is the account's file of nginx with the secret, the
// one content the check accepts there.
func NginxApacheBackendConf(secret string) string {
	return "proxy_set_header " + ApacheBackendHeader + " \"" + secret + "\";\n"
}

var (
	apacheBackendConfRe = regexp.MustCompile(`'([0-9a-f]{64})'`)
	nginxBackendConfRe  = regexp.MustCompile(`"([0-9a-f]{64})"`)
)

// exactBackend tells whether content is form(secret) for the secret it
// holds (found by re).
func exactBackend(content string, re *regexp.Regexp, form func(string) string) bool {
	m := re.FindStringSubmatch(content)
	return m != nil && content == form(m[1])
}

// checkApacheBackend is the check of an account's backend.conf.
func checkApacheBackend(p string, content []byte) error {
	if !exactBackend(string(content), apacheBackendConfRe, ApacheBackendConf) {
		return notAllowed("writing "+p, "the check of an account's Apache holds exactly the panel's rewrite rule and <If> on %s with a secret of 64 hex digits and ProxyFCGISetEnvIf; httpd.conf includes it, so anything else would be read as part of the account's Apache", ApacheBackendHeader)
	}
	return nil
}

// checkNginxApacheBackend is the check of a file in NginxApacheBackendDir.
func (c *checker) checkNginxApacheBackend(p string, content []byte) error {
	login, ok := strings.CutSuffix(path.Base(p), ".conf")
	if path.Dir(p) != NginxApacheBackendDir || !ok || !apacheInstanceLoginRe.MatchString(login) || login == "root" {
		return notAllowed("writing "+p, "%s holds <login>.conf alone, the header nginx sends the account's Apache", NginxApacheBackendDir)
	}
	if err := c.panelAccount(login); err != nil {
		return notAllowed("writing "+p, "it is the secret of the Apache of %s, and %v; only a hosting account has an Apache of its own", login, err)
	}
	if !exactBackend(string(content), nginxBackendConfRe, NginxApacheBackendConf) {
		return notAllowed("writing "+p, "it holds exactly proxy_set_header %s \"<64 hex digits>\";, which a location that proxies to the account's Apache includes", ApacheBackendHeader)
	}
	return nil
}

// isApacheBackendFile is either side of an account's backend secret.
func isApacheBackendFile(p string) bool {
	if path.Dir(p) == NginxApacheBackendDir {
		return true
	}
	rel, ok := strings.CutPrefix(p, ApacheInstanceRoot+"/")
	parts := strings.Split(rel, "/")
	return ok && len(parts) == 2 && parts[1] == apacheBackendName
}

// namesBackendHeader tells whether a word names the header, as a header or
// as the variable nginx and the CGI environment make of it.
func namesBackendHeader(s string) bool {
	return strings.Contains(strings.ReplaceAll(strings.ToLower(s), "_", "-"), "x-monopanel-backend")
}

// nginxBackendTarget is the file of NginxApacheBackendDir an include names,
// when it names one there.
func nginxBackendTarget(s ngxStmt) (string, bool) {
	if !strings.EqualFold(s.name, "include") || len(s.args) != 1 {
		return "", false
	}
	full := s.args[0]
	if !strings.HasPrefix(full, "/") {
		full = nginxDir + "/" + full
	}
	full = path.Clean(full)
	return full, full == NginxApacheBackendDir || path.Dir(full) == NginxApacheBackendDir
}

// nginxBackendInclude allows an include of an account's backend secret only
// as nginx sends it to the account's Apache: in a location (stmts, the
// block named parent) whose one proxy_pass is http://<that Apache's address>,
// with nothing below it that proxies — a block inside the location inherits
// its headers. The site's owner is the account whose Apache the location
// proxies to: the agent knows the site only by its domain, and the address
// names the uid. An explicit upstream named like that address cannot stand
// in for it: nginx refuses an upstream block whose name a proxy_pass with a
// port names ("upstream may not have port").
func (c *checker) nginxBackendInclude(stmts []ngxStmt, parent string, s ngxStmt) error {
	full, _ := nginxBackendTarget(s)
	what := fmt.Sprintf("nginx include %q", s.args[0])
	login, ok := strings.CutSuffix(path.Base(full), ".conf")
	if !ok || path.Dir(full) != NginxApacheBackendDir || strings.ContainsAny(s.args[0], "*?[\\") || !apacheInstanceLoginRe.MatchString(login) || login == "root" {
		return notAllowed(what, "the backend secret of an account's Apache is included by its name alone, %s/<login>.conf", NginxApacheBackendDir)
	}
	if !strings.EqualFold(parent, "location") {
		return notAllowed(what, "the backend secret of %s goes into a location that proxies to its Apache: anywhere else nginx hands it to the locations below, whatever they proxy to", login)
	}
	db, err := c.accounts()
	if err != nil {
		return notAllowed(what, "the accounts cannot be read: %v", err)
	}
	a, known := db.users[login]
	if !known {
		return notAllowed(what, "%s is not a local account", login)
	}
	addr, err := ApacheInstanceAddr(a.uid)
	if err != nil {
		return notAllowed(what, "%v", err)
	}
	target, passes := "", 0
	for _, t := range stmts {
		if strings.EqualFold(t.name, "proxy_pass") {
			passes++
			if len(t.args) == 1 {
				target = t.args[0]
			}
		}
	}
	// http://<address>, or a path on it: nothing that a variable decides.
	want := "http://" + addr
	if passes != 1 || (target != want && !(strings.HasPrefix(target, want+"/") && !strings.Contains(target, "$"))) {
		return notAllowed(what, "the backend secret of %s goes with requests to its Apache alone: the location's proxy_pass is %s", login, want)
	}
	for _, t := range stmts {
		if t.isBlk && nginxProxies(t.block) {
			return notAllowed(what, "the %s block inside this location inherits the backend secret of %s and proxies itself: the secret goes to %s's Apache alone", t.name, login, login)
		}
	}
	return nil
}

// nginxProxies tells whether a block, or one inside it, proxies anywhere.
func nginxProxies(stmts []ngxStmt) bool {
	for _, t := range stmts {
		if strings.EqualFold(t.name, "proxy_pass") || (t.isBlk && nginxProxies(t.block)) {
			return true
		}
	}
	return false
}

// nginxApacheProxy wants a proxy_pass to the Apache of a hosting account
// (http://127.<100+uid>>16>.<…>:81, ApacheInstanceAddr) to stand next to the
// include of that account's secret, in the same block: without it the
// Apache answers every request of the location 403. Refused here, a
// template override or custom directives of the time before the secret
// fail when they are applied, naming the include, instead of a site that
// answers nothing but 403. Nothing else depends on it: what does not carry
// the secret is refused by the Apache all the same.
func (c *checker) nginxApacheProxy(stmts []ngxStmt, s ngxStmt) error {
	login, ok := c.apacheProxyAccount(s)
	if !ok {
		return nil
	}
	want := NginxApacheBackendFile(login)
	for _, t := range stmts {
		if full, secret := nginxBackendTarget(t); secret && full == want {
			return nil
		}
	}
	return notAllowed(fmt.Sprintf("nginx directive \"proxy_pass %s\"", strings.Join(s.args, " ")), "the Apache of %s answers only requests that carry its backend secret: the location that proxies there includes %s next to proxy_pass (include monopanel/apache/%s.conf;), or the Apache refuses every request of it", login, want, login)
}

// apacheProxyAccount is the hosting account whose Apache a proxy_pass names
// by its address, written plainly.
func (c *checker) apacheProxyAccount(s ngxStmt) (string, bool) {
	if len(s.args) == 0 || len(s.args[0]) < len("http://") || !strings.EqualFold(s.args[0][:len("http://")], "http://") {
		return "", false
	}
	host := s.args[0][len("http://"):]
	if i := strings.IndexAny(host, "/$?"); i >= 0 {
		host = host[:i]
	}
	h, port, err := net.SplitHostPort(host)
	if err != nil || port != strconv.Itoa(ApacheInstancePort) {
		return "", false
	}
	ip := net.ParseIP(h).To4()
	if ip == nil || ip[0] != 127 || ip[1] < 100 {
		return "", false
	}
	uid := int(ip[1]-100)<<16 | int(ip[2])<<8 | int(ip[3])
	db, err := c.accounts()
	if err != nil {
		return "", false
	}
	a, ok := db.byUID[uid]
	if !ok || c.panelAccount(a.name) != nil {
		return "", false
	}
	return a.name, true
}

// apacheWalk calls fn for each line of an Apache file with the sections open
// around it, outermost first, in lower case ("virtualhost", "directory",
// "if" …): a directive with its name, a section's opening line with "<" and
// the section's name; closing lines close the innermost section.
func apacheWalk(lines []apLine, fn func(l apLine, name string, open []string) error) error {
	var open []string
	for _, l := range lines {
		rest := l.text
		name := strings.ToLower(apWord(&rest))
		if strings.HasPrefix(name, "</") {
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
			continue
		}
		if strings.HasPrefix(name, "<") {
			section := strings.TrimSuffix(strings.TrimPrefix(name, "<"), ">")
			if err := fn(l, "<"+section, open); err != nil {
				return err
			}
			open = append(open, section)
			continue
		}
		if err := fn(l, name, open); err != nil {
			return err
		}
	}
	return nil
}

// apacheMainBackend wants the httpd.conf of an account's Apache to include
// its backend.conf with Include at the top level, as the template writes
// it: inside an <IfModule>, an <IfDefine> or a <VirtualHost> the check could
// be skipped or apply to one virtual host alone, inside a <Location> its
// rewrite rule would be a directory's, which runs after .htaccess, and an
// IncludeOptional skips a missing file.
func apacheMainBackend(inst *apacheInst, lines []apLine) error {
	want := ApacheBackendFile(inst.login)
	found := false
	err := apacheWalk(lines, func(l apLine, name string, open []string) error {
		if name != "include" && name != "includeoptional" {
			return nil
		}
		rest := l.text
		apWord(&rest)
		if args := apWords(rest); len(args) != 1 || args[0] != want {
			return nil
		}
		if name != "include" || len(open) > 0 {
			return atLine(l.n, notAllowed("Apache "+l.text, "the httpd.conf of the Apache of %s includes %s with Include, at the top level and in no section: elsewhere the check could be skipped, or come after a site's own .htaccess", inst.login, want))
		}
		found = true
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return notAllowed("the httpd.conf of the Apache of "+inst.login+" without "+want, "without its check the Apache answers every local process, past what nginx enforces for its sites: it includes it at the top level")
	}
	return nil
}

// apacheLateIf refuses, in a file of an account's Apache, an <If> (<ElseIf>,
// <Else>) inside a <Location> or <LocationMatch>, or inside another <If>:
// Apache merges the <If> sections last — those of the locations after those
// of the directories and files, and the nested ones after all the others —
// and such an <If> of a virtual host's, or of httpd.conf's after the check,
// would merge after the <If> of backend.conf, so that its Require replaced
// the denial of that second line (ApacheBackendConf). Everywhere else an
// <If> merges before it: at the top of a virtual host, in a <Directory>, a
// <Files>. The panel writes none of them.
func apacheLateIf(inst *apacheInst, lines []apLine) error {
	return apacheWalk(lines, func(l apLine, name string, open []string) error {
		if name != "<if" && name != "<elseif" && name != "<else" {
			return nil
		}
		for _, o := range open {
			switch o {
			case "location", "locationmatch", "if", "elseif", "else":
				return atLine(l.n, notAllowed("Apache "+name+"> in a <"+o+">", "in the Apache of %s it merges after the <If> of the check of nginx's secret and could let in what nginx did not send: an <If> goes into a <Directory>, a <Files> or the top of the virtual host, not into a location or another <If>", inst.login))
			}
		}
		return nil
	})
}

// apachePerDir are the sections whose directives apply to the requests in
// them (per directory), as opposed to the server or virtual host around
// them; apacheLateSections are those of them Apache merges only after it
// mapped the URL (translate_name), when the server-level rewrite rules of
// backend.conf have already run: the directories, the files, the proxied
// URLs. A location or an <If> outside them merges before (and again after).
var (
	apachePerDir       = map[string]bool{"directory": true, "directorymatch": true, "files": true, "filesmatch": true, "location": true, "locationmatch": true, "proxy": true, "proxymatch": true, "if": true, "elseif": true, "else": true, "limit": true, "limitexcept": true}
	apacheLateSections = map[string]bool{"directory": true, "directorymatch": true, "files": true, "filesmatch": true, "proxy": true, "proxymatch": true}
)

// apacheRewriteGuard keeps the files of an account's Apache from switching
// off the server-level rewriting through which backend.conf refuses what
// nginx did not send (ApacheBackendConf):
//
//   - at the server level of httpd.conf (and of a virtual host's file, outside
//     its <VirtualHost>) the rewrite directives are backend.conf's alone: a
//     rule ahead of its own could end the rewriting before the check, options
//     could keep the virtual hosts from inheriting it;
//   - every <VirtualHost> switches mod_rewrite on itself, at its own level: a
//     virtual host without a rewrite directive of its own does not inherit
//     the server's rules at all;
//   - RewriteEngine Off goes only into a section Apache merges after the URL
//     was mapped (a <Directory>, a <Files>): in a virtual host, a location
//     or an <If> outside those it switches the check off for the requests in
//     it;
//   - RewriteOptions Inherit (the virtual host's rules ahead of the server's)
//     or IgnoreInherit (none of the server's) at a virtual host's level would
//     let its own rules end the rewriting before the check, or drop it.
//
// .htaccess files cannot touch any of it: they are read after the URL was
// mapped, and set the rewriting of their directories alone.
func apacheRewriteGuard(inst *apacheInst, lines []apLine) error {
	// vhosts holds, for each <VirtualHost> open, whether it switched
	// mod_rewrite on at its own level.
	var vhosts []bool
	var open []string
	for _, l := range lines {
		rest := l.text
		name := strings.ToLower(apWord(&rest))
		if strings.HasPrefix(name, "</") {
			if len(open) == 0 {
				continue
			}
			if open[len(open)-1] == "virtualhost" && len(vhosts) > 0 {
				on := vhosts[len(vhosts)-1]
				vhosts = vhosts[:len(vhosts)-1]
				if !on {
					return atLine(l.n, notAllowed("a <VirtualHost> of the Apache of "+inst.login+" without RewriteEngine On", "a virtual host without a rewrite directive of its own does not inherit the rewrite rule of backend.conf that refuses what nginx did not send: RewriteEngine On goes right into the <VirtualHost>"))
				}
			}
			open = open[:len(open)-1]
			continue
		}
		if strings.HasPrefix(name, "<") {
			section := strings.TrimSuffix(strings.TrimPrefix(name, "<"), ">")
			if section == "virtualhost" {
				vhosts = append(vhosts, false)
			}
			open = append(open, section)
			continue
		}
		if !strings.HasPrefix(name, "rewrite") {
			continue
		}
		args := apWords(rest)
		inVhost := inst.kind == instCustom || slices.Contains(open, "virtualhost")
		perDir := ""
		for _, o := range open {
			if apachePerDir[o] {
				perDir = o
				break
			}
		}
		what := fmt.Sprintf("Apache %s", l.text)
		switch {
		case !inVhost && perDir == "":
			return atLine(l.n, notAllowed(what, "the server-level rewriting of the Apache of %s is backend.conf's alone, which refuses what nginx did not send: a rule ahead of its own could end the rewriting before the check, options could keep the virtual hosts from inheriting it", inst.login))
		case name == "rewriteengine" && len(args) == 1 && strings.EqualFold(args[0], "off") && !apacheLateSections[perDir]:
			return atLine(l.n, notAllowed(what, "the Apache of %s refuses what nginx did not send with a rewrite rule when it maps the URL: switched off in a virtual host, a location or an <If> it would let such requests in; RewriteEngine Off goes into a <Directory> or a <Files>", inst.login))
		case name == "rewriteengine" && len(args) == 1 && strings.EqualFold(args[0], "on") && perDir == "" && len(open) > 0 && open[len(open)-1] == "virtualhost" && len(vhosts) > 0:
			vhosts[len(vhosts)-1] = true
		case name == "rewriteoptions" && perDir == "":
			for _, a := range args {
				if strings.EqualFold(a, "inherit") || strings.EqualFold(a, "ignoreinherit") {
					return atLine(l.n, notAllowed(what, "a virtual host of the Apache of %s inherits the server's rewrite rule that refuses what nginx did not send, ahead of its own: with %s its own rules could end the rewriting before it, or drop it (InheritBefore keeps them after it)", inst.login, a))
				}
			}
		}
	}
	if len(vhosts) > 0 && !vhosts[len(vhosts)-1] {
		return notAllowed("a <VirtualHost> of the Apache of "+inst.login+" without RewriteEngine On", "a virtual host without a rewrite directive of its own does not inherit the rewrite rule of backend.conf that refuses what nginx did not send: RewriteEngine On goes right into the <VirtualHost>")
	}
	return nil
}

// apacheIncludeReaches tells whether an Include pattern can match a file in
// dir or below it: Apache expands wildcards in every component of the path.
func apacheIncludeReaches(pattern, dir string) bool {
	pp := strings.Split(strings.Trim(pattern, "/"), "/")
	dp := strings.Split(strings.Trim(dir, "/"), "/")
	if len(pp) <= len(dp) {
		return false
	}
	for i, d := range dp {
		if ok, err := path.Match(pp[i], d); err != nil || !ok {
			return err != nil
		}
	}
	return true
}
