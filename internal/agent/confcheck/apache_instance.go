package confcheck

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Apache mode runs one Apache per account, monopanel-apache@<login>.service:
// its parent runs as root — it binds the account's address and opens the
// logs — and its children serve the requests as the account. A shared Apache
// whose children run as one account for every site cannot keep the sites
// apart: a site's .htaccess merges last and may point PHP at another site's
// pool, follow a symlink into another home or show the server's status page.
//
// The files of an account's Apache lie in ApacheInstanceRoot/<login>/:
//
//	httpd.conf              the whole configuration apache2 starts with (no
//	                        apache2.conf, no envvars): who the children run as,
//	                        what the parent binds, loads and writes, its sites
//	sites/<domain>.conf     a site's virtual host
//	sites/<domain>.d/*.conf the site's custom directives, inside the virtual host
//	sites/README, sites/<domain>.d/README  notes the panel leaves, included by nothing
//
// So httpd.conf alone may set User and Group (the login, nothing else), Listen
// (the login's own address), the pid file, the lock files and the runtime
// files (the unit's runtime directory), ServerRoot (/etc/apache2), the
// modules (an allow-list, from Debian's module directory) and what it
// includes (its own sites/*.conf). A virtual host includes its own custom
// directives and nothing else, they include nothing, and neither may set the
// directives above. The directory is a hosting account's: an Apache whose
// children run as root, as a system account or as the API's own user is
// never written. Everything else is checked as in the rest of /etc/apache2,
// and the logs go to the account's own directory of the site log tree.

const (
	// ApacheInstanceRoot holds the accounts' Apache configurations.
	ApacheInstanceRoot = "/etc/apache2/monopanel/accounts"
	// ApacheInstanceRunRoot holds the runtime directories (RuntimeDirectory=
	// of monopanel-apache@.service, root's).
	ApacheInstanceRunRoot = "/run/monopanel-apache"
	// ApacheInstancePort is the port every account's Apache listens on: below
	// 1024, so that no process without CAP_NET_BIND_SERVICE can take it, also
	// not while the Apache restarts; nothing on a panel host listens on 81 on
	// a wildcard address, which would keep a specific address from binding
	// it (80 is nginx's).
	ApacheInstancePort = 81
	// ApacheBinary is Debian's Apache, which the unit starts with -f.
	ApacheBinary = "/usr/sbin/apache2"
	// apacheModuleDir is where Debian's apache2-bin keeps the modules.
	apacheModuleDir = "/usr/lib/apache2/modules"
	// apacheInstanceUnitPrefix names the template's instances.
	apacheInstanceUnitPrefix = "monopanel-apache@"
)

// ApacheInstanceModules are the modules an account's Apache loads, in the
// order httpd.conf loads them: what the panel's sites and their .htaccess
// files use (authentication, redirects and rewrites, headers, expiry,
// compression, the client's address from nginx, php-fpm through
// mod_proxy_fcgi, content negotiation, request timeouts, directory
// listings a site turns on itself). Not mod_cgi, mod_cgid, mod_include,
// mod_lua or mod_ext_filter, which run programs, nor mod_status and
// mod_info, which show the server's requests and configuration, nor
// mod_userdir.
var ApacheInstanceModules = []string{
	"mpm_event",
	"authz_core", "authz_host", "authz_user", "authn_core", "authn_file", "auth_basic", "access_compat",
	"alias", "dir", "mime", "env", "headers", "expires", "rewrite", "setenvif",
	"filter", "deflate", "remoteip", "proxy", "proxy_fcgi", "negotiation", "reqtimeout", "autoindex",
}

var apacheInstanceModules = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range ApacheInstanceModules {
		m[n] = true
	}
	return m
}()

// ApacheInstanceAddr is the address the Apache of the account with the uid
// listens on: 127.<100 + uid>>16>.<(uid>>8)&255>.<uid&255>:81. The whole of
// 127.0.0.0/8 is local on Linux, so every account has an address of its
// own, which no other account's Apache binds, and 127.0.0.1 — nginx's side
// of the connection, which mod_remoteip trusts — is none of them. A uid
// beyond 127.255.255.254 has no address.
func ApacheInstanceAddr(uid int) (string, error) {
	hi := 100 + uid>>16
	// 127.255.255.255 is the broadcast address of the loopback network:
	// nothing binds it.
	if uid < 0 || hi > 255 || (hi == 255 && uid&0xffff == 0xffff) {
		return "", fmt.Errorf("uid %d has no loopback address for its Apache: the addresses 127.100.0.0–127.255.255.254 map uids 0–%d", uid, 155<<16|0xfffe)
	}
	return fmt.Sprintf("127.%d.%d.%d:%d", hi, (uid>>8)&255, uid&255, ApacheInstancePort), nil
}

// ApacheInstanceDir is an account's directory of its Apache configuration.
func ApacheInstanceDir(login string) string { return path.Join(ApacheInstanceRoot, login) }

// ApacheInstanceConf is the httpd.conf the account's Apache starts with.
func ApacheInstanceConf(login string) string {
	return path.Join(ApacheInstanceDir(login), "httpd.conf")
}

// ApacheInstanceSites holds the account's virtual hosts.
func ApacheInstanceSites(login string) string { return path.Join(ApacheInstanceDir(login), "sites") }

// ApacheInstanceRunDir is the runtime directory of the account's Apache:
// the pid file, the lock files and DefaultRuntimeDir.
func ApacheInstanceRunDir(login string) string { return path.Join(ApacheInstanceRunRoot, login) }

// ApacheInstanceUnit is the account's Apache, an instance of the panel's
// template monopanel-apache@.service.
func ApacheInstanceUnit(login string) string { return apacheInstanceUnitPrefix + login + ".service" }

// ApacheInstanceCheckArgv is the configuration check of the account's
// Apache: apache2 -t on its httpd.conf.
func ApacheInstanceCheckArgv(login string) []string {
	return []string{ApacheBinary, "-t", "-f", ApacheInstanceConf(login)}
}

// apacheInstanceLoginRe is a login as the panel makes them.
var apacheInstanceLoginRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ApacheInstanceLoginOf reads the login from the name of an account's
// Apache unit (monopanel-apache@<login>.service), or from its check
// (ApacheInstanceCheckArgv); ok is false for anything else, root included.
func ApacheInstanceLoginOf(unit string) (string, bool) {
	login, ok := strings.CutPrefix(unit, apacheInstanceUnitPrefix)
	if !ok {
		return "", false
	}
	login, ok = strings.CutSuffix(login, ".service")
	return login, ok && apacheInstanceLoginRe.MatchString(login) && login != "root"
}

// ApacheInstanceCheckLogin reads the login from the check of an account's
// Apache; ok is false for any other argv.
func ApacheInstanceCheckLogin(argv []string) (string, bool) {
	if len(argv) != 4 || argv[0] != ApacheBinary || argv[1] != "-t" || argv[2] != "-f" {
		return "", false
	}
	rel, ok := strings.CutPrefix(argv[3], ApacheInstanceRoot+"/")
	if !ok {
		return "", false
	}
	login, ok := strings.CutSuffix(rel, "/httpd.conf")
	return login, ok && apacheInstanceLoginRe.MatchString(login) && login != "root"
}

// apacheInstFile is what a file of an account's Apache directory is.
type apacheInstFile int

const (
	instMain   apacheInstFile = iota + 1 // httpd.conf
	instVhost                            // sites/<domain>.conf
	instCustom                           // sites/<domain>.d/*.conf
	instReadme                           // a README the panel leaves
)

// apacheInst is the account an Apache file belongs to.
type apacheInst struct {
	path   string
	login  string
	kind   apacheInstFile
	domain string // the site of a virtual host or of custom directives
	addr   string // Listen
	// groupErr says why Group <login> is not the account's primary group.
	groupErr error
}

var apacheSiteNameRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.-]{0,252}$`)

// apacheInstance tells whether p lies in the directory of an account's
// Apache and, if so, whose and what it is; a file there that the account's
// Apache would not read, or that of an account that is no hosting
// account, is refused.
func (c *checker) apacheInstance(p string) (*apacheInst, error) {
	rel, ok := strings.CutPrefix(p, ApacheInstanceRoot+"/")
	if !ok {
		return nil, nil
	}
	parts := strings.Split(rel, "/")
	inst := &apacheInst{path: p, login: parts[0]}
	shape := func() error {
		return notAllowed("writing "+p, "an account's Apache reads %s/<login>/httpd.conf, sites/<domain>.conf and sites/<domain>.d/*.conf; nothing else belongs there", ApacheInstanceRoot)
	}
	if len(parts) < 2 || !apacheInstanceLoginRe.MatchString(inst.login) {
		return nil, shape()
	}
	site := func(name, suffix string) bool {
		d, ok := strings.CutSuffix(name, suffix)
		inst.domain = d
		return ok && apacheSiteNameRe.MatchString(d) && !strings.Contains(d, "..")
	}
	switch {
	case len(parts) == 2 && parts[1] == "httpd.conf":
		inst.kind = instMain
	case len(parts) == 3 && parts[1] == "sites" && parts[2] == "README":
		inst.kind = instReadme
	case len(parts) == 3 && parts[1] == "sites" && site(parts[2], ".conf"):
		inst.kind = instVhost
	case len(parts) == 4 && parts[1] == "sites" && site(parts[2], ".d") && parts[3] == "README":
		inst.kind = instReadme
	case len(parts) == 4 && parts[1] == "sites" && site(parts[2], ".d") && strings.HasSuffix(parts[3], ".conf") && parts[3] != ".conf":
		inst.kind = instCustom
	default:
		return nil, shape()
	}
	if err := c.panelAccount(inst.login); err != nil {
		return nil, notAllowed("writing "+p, "the Apache of %s would serve as that account, and %v; only a hosting account gets an Apache of its own", inst.login, err)
	}
	db, err := c.accounts()
	if err != nil {
		return nil, notAllowed("writing "+p, "the accounts cannot be read: %v", err)
	}
	a := db.users[inst.login]
	if inst.addr, err = ApacheInstanceAddr(a.uid); err != nil {
		return nil, notAllowed("writing "+p, "%v", err)
	}
	if g, ok := db.byGID[a.gid]; !ok || g.name != inst.login {
		inst.groupErr = fmt.Errorf("the primary group of %s is not the group %s", inst.login, inst.login)
	} else if err := c.groupAllowed(g.name); err != nil {
		inst.groupErr = err
	}
	return inst, nil
}

// apacheInstanceLine checks the directives of an account's Apache that
// decide what its root parent and its children are; handled is false for
// the others, which take the checks of the rest of /etc/apache2.
func (c *checker) apacheInstanceLine(inst *apacheInst, name, lname string, args []string) (handled bool, err error) {
	what := fmt.Sprintf("Apache %s %s", name, strings.Join(args, " "))
	main := inst.kind == instMain
	runDir := ApacheInstanceRunDir(inst.login)
	exact := func(want ...string) error {
		if !main {
			return notAllowed(what, "only the httpd.conf of the Apache of %s sets it", inst.login)
		}
		if len(args) != len(want) || strings.Join(args, " ") != strings.Join(want, " ") {
			return notAllowed(what, "the Apache of %s takes only %s %s", inst.login, name, strings.Join(want, " "))
		}
		return nil
	}
	switch lname {
	case "user":
		return true, exact(inst.login)
	case "group":
		if err := exact(inst.login); err != nil {
			return true, err
		}
		if inst.groupErr != nil {
			return true, notAllowed(what, "%v", inst.groupErr)
		}
		return true, nil
	case "listen":
		// One address, the account's own: a port below 1024 on an address
		// no other Apache of the panel binds.
		return true, exact(inst.addr)
	case "pidfile":
		return true, exact(runDir + "/httpd.pid")
	case "defaultruntimedir":
		return true, exact(runDir)
	case "mutex":
		return true, exact("file:"+runDir, "default")
	case "serverroot":
		return true, exact("/etc/apache2")
	case "loadmodule":
		if !main {
			return true, notAllowed(what, "only the httpd.conf of the Apache of %s loads modules", inst.login)
		}
		if len(args) == 2 {
			if mod, ok := strings.CutSuffix(args[0], "_module"); ok && apacheInstanceModules[mod] && args[1] == apacheModuleDir+"/mod_"+mod+".so" {
				return true, nil
			}
		}
		return true, notAllowed(what, "the root parent loads the module; an account's Apache loads only %s, from %s", strings.Join(ApacheInstanceModules, ", "), apacheModuleDir)
	case "include", "includeoptional":
		want := ""
		switch inst.kind {
		case instMain:
			want = ApacheInstanceSites(inst.login) + "/*.conf"
		case instVhost:
			want = ApacheInstanceSites(inst.login) + "/" + inst.domain + ".d/*.conf"
		}
		if want == "" || len(args) != 1 || args[0] != want {
			if want == "" {
				return true, notAllowed(what, "custom directives of a site include nothing: what they would pull in has not been checked as the Apache of %s", inst.login)
			}
			return true, notAllowed(what, "this file of the Apache of %s includes only %s", inst.login, want)
		}
		return true, nil
	case "define", "undefine":
		// -DFOREGROUND keeps the parent under systemd; a Define read before
		// the MPM starts would change how it runs, and the panel defines
		// nothing.
		return true, notAllowed(what, "an account's Apache defines nothing: a Define is read before the MPM starts and could undo -DFOREGROUND")
	}
	if isApacheLog(lname) && len(args) > 0 {
		return true, c.apacheInstanceLog(inst, name, args[0])
	}
	return false, nil
}

// isApacheLog names the directives that open a log in the root parent.
func isApacheLog(lname string) bool {
	return strings.HasSuffix(lname, "log") || strings.HasSuffix(lname, "logfile") || lname == "secauditlog2"
}

// apacheInstanceLog allows the logs of an account's Apache in the account's
// own directory of the site log tree (or syslog): the root parent opens them
// and the children, the account's, inherit them, so a log in another
// account's directory would be that account's log written by this one.
func (c *checker) apacheInstanceLog(inst *apacheInst, name, v string) error {
	what := fmt.Sprintf("Apache %s %s", name, v)
	if strings.HasPrefix(v, "|") {
		return notAllowed(what, "a piped log is a program the root parent starts")
	}
	if v == "syslog" || strings.HasPrefix(v, "syslog:") || v == "/dev/null" || v == "/dev/stderr" {
		return nil
	}
	own := path.Join(c.l.siteLogRoot(), inst.login)
	if p, ok := absPath(v); ok && under(p, own) {
		return nil
	}
	return notAllowed(what, "the root parent opens the log and the children of %s write to it: the logs of this Apache go to %s or syslog", inst.login, own)
}
