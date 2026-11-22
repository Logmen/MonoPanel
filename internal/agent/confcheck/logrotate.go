package confcheck

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// logrotate runs as root: its scripts run through the shell, it renames,
// creates (with the owner create names) and removes files, runs
// compresscmd, mails logs and follows include. The panel writes one file,
// monopanel-sites: two stanzas per account over the account's directory of
// the site log tree, a USR1 to nginx and to the account's own Apache through
// their pid files. Global options in a logrotate.d file would reach every
// file read after it, so there are none.

const logrotateDir = "/etc/logrotate.d"

func isLogrotate(p string) bool { return under(p, logrotateDir) || p == "/etc/logrotate.conf" }

// logrotateFlags are the options without arguments the panel's stanzas use
// and their harmless opposites.
var logrotateFlags = map[string]bool{
	"daily": true, "weekly": true, "monthly": true, "yearly": true, "hourly": true, "missingok": true, "nomissingok": true,
	"notifempty": true, "ifempty": true, "compress": true, "nocompress": true, "delaycompress": true, "nodelaycompress": true,
	"dateext": true, "nodateext": true, "nocreate": true, "copytruncate": true, "nocopytruncate": true, "sharedscripts": true,
	"nosharedscripts": true, "nomail": true, "noolddir": true,
}

// logrotateNumeric take a count or a size.
var logrotateNumeric = map[string]*regexp.Regexp{
	"rotate":  regexp.MustCompile(`^-?[0-9]+$`),
	"maxage":  regexp.MustCompile(`^[0-9]+$`),
	"maxsize": regexp.MustCompile(`^[0-9]+[kMG]?$`),
	"minsize": regexp.MustCompile(`^[0-9]+[kMG]?$`),
	"size":    regexp.MustCompile(`^[0-9]+[kMG]?$`),
	"start":   regexp.MustCompile(`^[0-9]+$`),
}

// logrotatePIDs are the pid files the panel may signal in every stanza:
// nginx's (its nginx.conf) and that of the distribution's Apache, which
// served the sites in Apache mode up to 0.8.19 and does so until they have
// moved to their accounts' own (Debian's; EL's never served any).
var logrotatePIDs = []string{"/run/nginx.pid", "/run/apache2/apache2.pid", "/run/httpd/httpd.pid"}

var logrotateLoginRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// logrotateUSR1 is the line that has a server reopen its logs: USR1 through
// its pid file, nothing when it does not run.
func logrotateUSR1(pid string) string {
	return fmt.Sprintf(`[ ! -s %s ] || kill -USR1 "$(cat %s)" 2>/dev/null || true`, pid, pid)
}

func checkLogrotate(c *checker, _ string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	allowedScript := map[string]bool{}
	for _, pid := range logrotatePIDs {
		allowedScript[logrotateUSR1(pid)] = true
	}
	// A stanza also reopens the logs of its own account's Apache, whose root
	// parent writes the pid file into the unit's runtime directory, root's
	// and removed with the unit's stop, so that no stale pid is signalled.
	ownApache := func(login, s string) bool {
		return login != "" && s == logrotateUSR1(ApacheInstanceRunDir(login)+"/httpd.pid")
	}
	inBlock, inScript := false, false
	login := ""
	for i, raw := range lines {
		n := i + 1
		s := trimBlanks(raw)
		if inScript {
			if f := blankFields(s); len(f) > 0 && f[0] == "endscript" {
				// logrotate goes on reading directives right after the
				// word: "endscript compresscmd /bin/sh" sets one.
				if len(f) != 1 {
					return atLine(n, notAllowed(fmt.Sprintf("text after endscript %q", strings.Join(f[1:], " ")), "logrotate reads it as directives of the stanza"))
				}
				inScript = false
				continue
			}
			if s != "" && !allowedScript[s] && !ownApache(login, s) {
				return atLine(n, notAllowed(fmt.Sprintf("logrotate script line %q", s), "root runs it through the shell; the panel's scripts only send USR1 to nginx and to the account's Apache through their pid files"))
			}
			continue
		}
		if s == "" || s[0] == '#' {
			continue
		}
		if !inBlock {
			if !strings.HasSuffix(s, "{") {
				return atLine(n, notAllowed(fmt.Sprintf("logrotate option %q outside a stanza", s), "a global option reaches every log rotated after this file (compresscmd, olddir, include)"))
			}
			l, err := c.logrotatePatterns(trimBlanks(strings.TrimSuffix(s, "{")))
			if err != nil {
				return atLine(n, err)
			}
			login, inBlock = l, true
			continue
		}
		if s == "}" {
			inBlock = false
			continue
		}
		f := blankFields(s)
		key := strings.ToLower(f[0])
		var err error
		switch {
		case key == "postrotate":
			if len(f) != 1 {
				err = notAllowed("text after postrotate", "the script starts on the next line")
			}
			inScript = true
		case key == "prerotate" || key == "firstaction" || key == "lastaction" || key == "preremove":
			err = notAllowed(fmt.Sprintf("logrotate %s script", key), "root runs it through the shell; the panel writes postrotate only")
		case logrotateFlags[key]:
			if len(f) != 1 {
				err = notAllowed(fmt.Sprintf("logrotate %q", s), "%s takes no argument", key)
			}
		case logrotateNumeric[key] != nil:
			if len(f) != 2 || !logrotateNumeric[key].MatchString(f[1]) {
				err = notAllowed(fmt.Sprintf("logrotate %q", s), "%s takes a number", key)
			}
		case key == "create":
			err = logrotateCreate(f[1:], login)
		default:
			err = notAllowed(fmt.Sprintf("logrotate option %q", f[0]), "it is not one of the options the panel writes; su, olddir, compresscmd, mail, include and the like act as root on other files or run programs")
		}
		if err := atLine(n, err); err != nil {
			return err
		}
	}
	if inBlock || inScript {
		return unreadable(len(lines), "a stanza or a script is not closed")
	}
	return nil
}

// logrotatePatterns allows the masks of one account's directory in the
// site log tree, which only root writes, and returns the login.
func (c *checker) logrotatePatterns(s string) (string, error) {
	pats := blankFields(s)
	if len(pats) == 0 {
		return "", notAllowed("a logrotate stanza without files", "it names the logs it rotates")
	}
	login := ""
	for _, p := range pats {
		p = strings.Trim(p, `"`)
		// logrotate ends the list of files at the first "{" and splits it
		// with popt, which honours quotes and backslashes: none of them may
		// stand inside a mask this check reads word by word.
		if strings.ContainsAny(p, "{}\"'\\") {
			return "", notAllowed(fmt.Sprintf("logrotate files %q", p), "a mask of the panel has no braces, quotes or backslashes")
		}
		ap, ok := absPath(p)
		dir := path.Dir(ap)
		l := path.Base(dir)
		if !ok || path.Dir(dir) != c.l.siteLogRoot() || !logrotateLoginRe.MatchString(l) || strings.ContainsAny(dir, "*?[") || strings.HasPrefix(path.Base(ap), ".") {
			return "", notAllowed(fmt.Sprintf("logrotate files %q", p), "root renames, creates and removes them; the panel rotates the logs of %s/<login>/", c.l.siteLogRoot())
		}
		if login != "" && l != login {
			return "", notAllowed(fmt.Sprintf("logrotate files %q", p), "one stanza rotates the logs of one account")
		}
		login = l
	}
	return login, nil
}

// logrotateCreate allows "create mode owner group" with the account's
// group and root or the account as the owner, and no special or
// world-writable bits.
func logrotateCreate(args []string, login string) error {
	what := fmt.Sprintf("logrotate create %s", strings.Join(args, " "))
	if len(args) != 3 {
		return notAllowed(what, "the panel names the mode, the owner and the group")
	}
	m, err := strconv.ParseUint(args[0], 8, 32)
	if err != nil || m&^0o777 != 0 || m&0o002 != 0 {
		return notAllowed(what, "a log is not world-writable and has no special bits")
	}
	if (args[1] != "root" && args[1] != login) || args[2] != login {
		return notAllowed(what, "the logs of %s belong to root or %s and to the group %s", login, login, login)
	}
	return nil
}
