// Package confcheck refuses configuration content that would hand root to
// whoever wrote it. The API process composes the configurations of daemons
// that run as root — nginx, Apache, php-fpm, MySQL, postfix, dovecot, sshd,
// logrotate, fail2ban, systemd-tmpfiles, sysctl, apt and yum — and the agent
// writes them as root. A directive such as nginx's load_module, Apache's
// piped logs, postfix's daemon_directory, a logrotate script or a tmpfiles
// line would let a compromised API process run code or write files as root;
// Check keeps such content out, whatever the API sends.
//
// Every family is read the way its daemon reads it: nginx's tokenizer,
// Apache's logical lines, php.ini, my.cnf, main.cf and master.cf, dovecot's
// sections and so on. What the panel itself writes (the templates under
// templates/ and the text built in internal/api) passes; a directive that
// runs a program, loads a library, writes a file outside the panel's log
// trees, or changes whose privileges a daemon runs with does not. Where the
// panel writes a fixed shape (sshd, sysctl, tmpfiles, logrotate, fail2ban,
// repositories, opendkim) the check is an allow-list; where an administrator
// adds directives of his own (custom nginx directives, template overrides)
// it refuses what is known to reach root and checks every path a root
// process opens for writing. Content the check cannot read the way the
// daemon does is refused.
package confcheck

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"sync/atomic"
)

// Layout is what the checks need to know about the panel's own directories
// and accounts: config.yaml may move them, and the agent hands its
// configuration over with Configure. An empty field means the built-in
// default (the one of config.Default()).
type Layout struct {
	// LogDir holds the site log tree LogDir/sites, which only root writes.
	LogDir string
	// RunDir holds the php-fpm sockets of the sites (RunDir/php).
	RunDir string
	// WWWRoot holds the homes of the panel's accounts.
	WWWRoot string
	// ServiceUser and ServiceGroup run the API process: nothing root starts
	// may run as them or hand them its sockets.
	ServiceUser  string
	ServiceGroup string
	// WebGroup and CLIGroup are the panel's own groups (sysusers.d).
	WebGroup string
	CLIGroup string
}

// DefaultLayout is the layout of config.Default().
func DefaultLayout() Layout {
	return Layout{
		LogDir: "/var/log/monopanel", RunDir: "/run/monopanel", WWWRoot: "/var/www",
		ServiceUser: "monopanel", ServiceGroup: "monopanel", WebGroup: "monopanel-web", CLIGroup: "monopanel-cli",
	}
}

// Fixed names of the panel (the constants of internal/api).
const (
	mailBase  = "/var/mail/monopanel"
	vmailUser = "vmail"
	sftpGroup = "monopanel-sftp"
)

var current atomic.Pointer[Layout]

// Configure sets the layout the checks use: the agent calls it once at
// start with the values of its configuration. Empty fields keep the default.
func Configure(l Layout) {
	d := DefaultLayout()
	dir := func(v *string, def string) {
		if *v == "" {
			*v = def
		} else {
			*v = path.Clean(*v)
		}
	}
	name := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}
	dir(&l.LogDir, d.LogDir)
	dir(&l.RunDir, d.RunDir)
	dir(&l.WWWRoot, d.WWWRoot)
	name(&l.ServiceUser, d.ServiceUser)
	name(&l.ServiceGroup, d.ServiceGroup)
	name(&l.WebGroup, d.WebGroup)
	name(&l.CLIGroup, d.CLIGroup)
	current.Store(&l)
}

func layout() Layout {
	if l := current.Load(); l != nil {
		return *l
	}
	return DefaultLayout()
}

// siteLogRoot is the root-owned tree of the site logs, LogDir/sites.
func (l Layout) siteLogRoot() string { return path.Join(l.LogDir, "sites") }

// phpRunDir is where the panel's php-fpm pools listen.
func (l Layout) phpRunDir() string { return path.Join(l.RunDir, "php") }

// checker carries what one Check needs: the layout and, read on first use,
// the system's accounts.
type checker struct {
	l   Layout
	db  *accountDB
	err error
}

func (c *checker) accounts() (*accountDB, error) {
	if c.db == nil && c.err == nil {
		c.db, c.err = readAccounts()
	}
	return c.db, c.err
}

type family struct {
	match func(p string) bool
	check func(c *checker, p string, content []byte) error
}

// families are tried in order; the first that matches decides.
var families = []family{
	{never, refuseAlways},
	{isNginx, checkNginx},
	{isApache, checkApache},
	{isPHP, checkPHP},
	{isMySQL, checkMySQL},
	{isPostfix, checkPostfix},
	{isDovecot, checkDovecot},
	{isOpenDKIM, checkOpenDKIM},
	{isLogrotate, checkLogrotate},
	{isFail2ban, checkFail2ban},
	{isTmpfiles, checkTmpfiles},
	{isSysusers, checkSysusers},
	{isSysctl, checkSysctl},
	{isSSH, checkSSH},
	{isApt, checkApt},
	{isYum, checkYum},
	{isSELinux, checkSELinux},
	{isMemcached, checkMemcached},
	{isSphinx, checkSphinx},
	{isNftables, checkNftables},
	{isMonopanelEtc, checkMonopanelEtc},
	{isComposerWrapper, checkComposerWrapper},
	{isPanelLib, checkPanelLib},
	{isEnvDir, checkEnvDir},
}

// Check returns an error when content written to path would let its author
// run code, or write or change files, as root. Paths without rules pass.
func Check(p string, content []byte) error {
	if !strings.HasPrefix(p, "/") {
		return nil
	}
	p = path.Clean(p)
	c := &checker{l: layout()}
	for _, f := range families {
		if f.match(p) {
			return f.check(c, p, content)
		}
	}
	return nil
}

// notAllowed is the refusal: what, and why it would reach root.
func notAllowed(what, why string, args ...any) error {
	return fmt.Errorf("%s is not allowed: %s", printable(what), printable(fmt.Sprintf(why, args...)))
}

// printable escapes control characters: the refusal travels into logs and
// the API's answers, where a value's line break would start a line of its
// own.
func printable(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// atLine names the line of a refusal.
func atLine(n int, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("line %d: %w", n, err)
}

// errUnreadable marks content the check cannot read the way the daemon
// does. It is refused: what cannot be read cannot be vouched for.
var errUnreadable = errors.New("content this check cannot read the way the daemon does is not allowed")

func unreadable(n int, format string, args ...any) error {
	return fmt.Errorf("line %d: %s: %w", n, printable(fmt.Sprintf(format, args...)), errUnreadable)
}

// under reports whether p lies strictly below dir.
func under(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return len(p) > len(dir)+1 && strings.HasPrefix(p, dir+"/")
}

// underAny reports whether p lies below one of dirs.
func underAny(p string, dirs ...string) bool {
	for _, d := range dirs {
		if under(p, d) {
			return true
		}
	}
	return false
}

// hasDotDot reports whether a path has a ".." element: a path a root
// process opens must say where it goes without walking back up, the more so
// as ".." behind a symlinked directory goes elsewhere than it reads.
func hasDotDot(p string) bool {
	for _, e := range strings.Split(p, "/") {
		if e == ".." {
			return true
		}
	}
	return false
}

// absPath checks that v is an absolute path without ".." and returns it
// cleaned.
func absPath(v string) (string, bool) {
	if !strings.HasPrefix(v, "/") || hasDotDot(v) || strings.ContainsAny(v, "\x00\n\r") {
		return "", false
	}
	return path.Clean(v), true
}

// directChild reports whether p is a file right in dir: dir is one only
// root adds entries to, so no symlink of somebody else's can stand in for
// the file.
func directChild(p, dir string) bool {
	b := path.Base(p)
	return path.Dir(p) == strings.TrimSuffix(dir, "/") && b != "." && b != ".." && b != "/"
}

// textLines splits a text configuration into lines. A NUL byte has no place
// in one, and a carriage return is a line break to some daemons and none to
// others. The other control characters are blanks to some parsers (Apache's
// apr_isspace, MySQL's and postfix's isspace, opendkim's, Python's
// str.strip) and part of a word to others: "LoadModule\vx" is LoadModule to
// Apache. The panel writes none of them, so all are refused rather than read
// one way while the daemon reads them another; tabs and line feeds are the
// only control characters a configuration keeps.
func textLines(content []byte) ([]string, error) {
	s := string(content)
	if strings.IndexByte(s, 0) >= 0 {
		return nil, notAllowed("a NUL byte", "configuration files are text")
	}
	if strings.IndexByte(s, '\r') >= 0 {
		return nil, notAllowed("a carriage return", "a daemon may take it for a line break where this check does not; the panel writes plain line feeds")
	}
	if i := strings.IndexFunc(s, isOddControl); i >= 0 {
		return nil, atLine(strings.Count(s[:i], "\n")+1, notAllowed(fmt.Sprintf("the control character %q", s[i]), "some daemons take it for a blank where others, and this check, do not; the panel writes tabs and line feeds only"))
	}
	return strings.Split(s, "\n"), nil
}

// isOddControl is a control character other than a tab or a line feed.
func isOddControl(r rune) bool { return (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f }

// isBlank is a blank the way the C daemons see one once textLines has
// refused the other control characters: a space or a tab. strings.TrimSpace
// and strings.Fields would also take U+00A0, U+0085 and the like for
// blanks, which no daemon here does.
func isBlank(r rune) bool { return r == ' ' || r == '\t' }

// trimBlanks trims spaces and tabs.
func trimBlanks(s string) string { return strings.Trim(s, " \t") }

// blankFields splits s at runs of spaces and tabs.
func blankFields(s string) []string { return strings.FieldsFunc(s, isBlank) }

// confDirBelow reports whether a directory between root and the file p is
// named *.conf. Apache's Include of a pattern takes a matching directory and
// reads every file in it, the README next to the configurations included:
// IncludeOptional sites/*.conf would read sites/x.conf/README. config/apply
// makes missing directories, so the name is refused outright.
func confDirBelow(p, root string) bool {
	rel := strings.TrimPrefix(path.Dir(p), strings.TrimSuffix(root, "/"))
	for _, e := range strings.Split(rel, "/") {
		if strings.HasSuffix(e, ".conf") {
			return true
		}
	}
	return false
}

// never are places where any content runs as root or decides who is root,
// and data directories the panel fills through other operations (tar, the
// daemons themselves): config/apply writes none of them, whatever
// extra_write_prefixes says.
func never(p string) bool {
	for _, d := range []string{
		"/etc/cron.d", "/etc/cron.hourly", "/etc/cron.daily", "/etc/cron.weekly", "/etc/cron.monthly", "/var/spool/cron",
		"/etc/sudoers.d", "/etc/pam.d", "/etc/security", "/etc/profile.d", "/etc/ld.so.conf.d", "/etc/init.d", "/etc/rc.d",
		"/etc/modprobe.d", "/etc/modules-load.d", "/etc/udev", "/etc/polkit-1", "/etc/apt/apt.conf.d", "/etc/apt/trusted.gpg.d",
		"/etc/dnf", "/etc/yum", "/etc/pki/rpm-gpg", "/usr/share/keyrings", "/opt/monopanel", "/var/log/mysql", "/var/lib/sphinxsearch",
		"/var/lib/sphinx", "/var/log/sphinx", "/var/log/sphinxsearch", "/var/mail/monopanel", "/var/lib/monopanel", "/var/lib/monopanel-valkey",
	} {
		if p == d || under(p, d) {
			return true
		}
	}
	switch p {
	case "/etc/crontab", "/etc/sudoers", "/etc/ld.so.preload", "/etc/ld.so.conf", "/etc/profile", "/etc/bash.bashrc",
		"/etc/environment", "/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/rc.local", "/etc/yum.conf",
		"/etc/apt/sources.list", "/etc/apt/trusted.gpg", "/etc/apt/apt.conf":
		return true
	}
	return false
}

func refuseAlways(_ *checker, p string, _ []byte) error {
	return notAllowed("writing "+p, "the panel applies nothing there, and what lies there runs or decides as root (daemon data arrives through the daemons' own operations)")
}

// inertName is a file next to configurations that no daemon reads: a
// README the panel leaves for the administrator, or the custom nginx
// directives of a migrated site kept for review (custom.conf.migrated).
// The include rules never let a daemon pull such a name in.
func inertName(p string) bool {
	b := path.Base(p)
	return b == "README" || strings.HasSuffix(b, ".migrated")
}

// CheckOwnership refuses an owner, a group or a mode that would make a file
// root reads someone else's to change: the content checks vouch for what is
// written now, and a file the API's account owns, or one anybody may write,
// would change afterwards without them. Root's files stay root's and are
// not writable by group or others; the panel gives away only what the
// daemon reading it must own (opendkim's keys, dovecot's group on the users
// file). config/apply calls it with FileSpec's fields; mode is the unix
// permission (0 means the default 0644).
func CheckOwnership(p, owner, group string, mode uint32) error {
	p = path.Clean(p)
	if mode&^0o777 != 0 || mode&0o022 != 0 {
		return notAllowed(fmt.Sprintf("mode %04o for %s", mode, p), "files root reads are writable by root alone, with no special bits")
	}
	ownerOK, groupOK := owner == "" || owner == "root", group == "" || group == "root"
	switch {
	case under(p, dkimDir+"/keys"):
		ownerOK = ownerOK || owner == "opendkim"
		groupOK = groupOK || group == "opendkim"
	case p == dovecotUsers:
		groupOK = groupOK || group == "dovecot"
	}
	if !ownerOK {
		return notAllowed(fmt.Sprintf("owner %q for %s", owner, p), "the file would be its owner's to rewrite after this check; root's files stay root's")
	}
	if !groupOK {
		return notAllowed(fmt.Sprintf("group %q for %s", group, p), "the group would read what only root and the daemon read")
	}
	return nil
}
