package confcheck

import (
	"fmt"
	"regexp"
	"strings"
)

// opendkim starts as root and switches to UserID; without one it stays
// root. Debian's /etc/default/opendkim is a shell fragment the package's
// unit generator sources as root. The panel writes opendkim.conf with a
// fixed set of keys, the defaults file with five variables, and the key
// tables and keys in /etc/opendkim (data opendkim reads as its own user).

const (
	dkimConf     = "/etc/opendkim.conf"
	dkimDir      = "/etc/opendkim"
	dkimDefaults = "/etc/default/opendkim"
)

func isOpenDKIM(p string) bool { return p == dkimConf || p == dkimDefaults || under(p, dkimDir) }

func checkOpenDKIM(c *checker, p string, content []byte) error {
	switch {
	case p == dkimConf:
		return checkDKIMConf(content)
	case p == dkimDefaults:
		return checkDKIMDefaults(content)
	}
	// KeyTable, SigningTable, TrustedHosts, keys/<domain>/<selector>.private.
	_, err := textLines(content)
	return err
}

// dkimKeys are the opendkim.conf keys the panel writes and a few that only
// tune signing; scripts (the *PolicyScript keys, lua: datasets), report
// commands and anything that moves what root does before the switch to
// UserID are not among them.
var dkimKeys = map[string]bool{
	"syslog": true, "syslogsuccess": true, "logwhy": true, "umask": true, "socket": true, "pidfile": true, "userid": true,
	"oversignheaders": true, "canonicalization": true, "mode": true, "subdomains": true, "autorestart": true,
	"autorestartrate": true, "minimumkeybits": true, "keytable": true, "signingtable": true, "externalignorelist": true,
	"internalhosts": true, "domain": true, "selector": true, "keyfile": true, "signaturealgorithm": true,
	"requiresafekeys": true, "trustanchorfile": true, "softwareheader": true, "removeoldsignatures": true, "logresults": true,
}

var (
	dkimSocketRe = regexp.MustCompile(`^(local:(/run/opendkim|/var/run/opendkim|/var/spool/postfix/opendkim)/[A-Za-z0-9_.-]+|inet6?:[0-9]{1,5}(@(127\.0\.0\.1|localhost|\[?::1\]?))?|[0-9]{1,5}@(127\.0\.0\.1|localhost))$`)
	dkimPIDRe    = regexp.MustCompile(`^(/run/opendkim|/var/run/opendkim)/[A-Za-z0-9_.-]+$`)
)

// dkimLineMax is safely below opendkim's line buffer (BUFRSZ, 1024 bytes).
const dkimLineMax = 512

func checkDKIMConf(content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	user := false
	for i, raw := range lines {
		n := i + 1
		// config_load reads with fgets into a buffer of about a kilobyte:
		// the rest of a longer line is a line of its own ("UserID root"
		// after a long Syslog value). It cuts every line at its first "#".
		if len(raw) >= dkimLineMax {
			return unreadable(n, "a line of %d bytes; opendkim reads lines of fewer than %d bytes and starts a new one after that", len(raw), dkimLineMax)
		}
		if j := strings.IndexByte(raw, '#'); j >= 0 {
			raw = raw[:j]
		}
		s := trimBlanks(raw)
		if s == "" {
			continue
		}
		f := blankFields(s)
		key, val := strings.ToLower(f[0]), strings.Join(f[1:], " ")
		what := fmt.Sprintf("opendkim %s", f[0])
		var err error
		switch {
		case !dkimKeys[key]:
			err = notAllowed(what, "it is not one of the keys the panel writes; scripts, report commands and the like run in opendkim")
		case strings.Contains(strings.ToLower(val), "lua:"):
			err = notAllowed(what+" "+val, "a lua: dataset is a script opendkim runs")
		case key == "userid":
			user = true
			if val != "opendkim" && val != "opendkim:opendkim" {
				err = notAllowed(what+" "+val, "opendkim starts as root and switches to this account; it is opendkim")
			}
		case key == "socket":
			if !dkimSocketRe.MatchString(val) {
				err = notAllowed(what+" "+val, "root removes and makes the socket; it is local in /run/opendkim or inet on the loopback")
			}
		case key == "pidfile":
			if !dkimPIDRe.MatchString(val) {
				err = notAllowed(what+" "+val, "root writes the pid file; it belongs in /run/opendkim")
			}
		}
		if err := atLine(n, err); err != nil {
			return err
		}
	}
	if !user {
		return notAllowed("an opendkim.conf without UserID", "opendkim would keep root's privileges")
	}
	return nil
}

var (
	dkimVarRe   = regexp.MustCompile(`^([A-Z_]+)=(.*)$`)
	dkimValueRe = regexp.MustCompile(`^"?([A-Za-z0-9_./:@-]|\$RUNDIR|\$NAME)*"?$`)
)

// checkDKIMDefaults allows the variables the panel sets, with values that
// are paths, names and the two references it uses: the file is sourced as a
// shell script by root.
func checkDKIMDefaults(content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		n := i + 1
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' {
			continue
		}
		m := dkimVarRe.FindStringSubmatch(s)
		if m == nil {
			return atLine(n, notAllowed(fmt.Sprintf("the line %q", s), "the file is shell root sources; it holds plain VARIABLE=value lines"))
		}
		k, v := m[1], m[2]
		what := fmt.Sprintf("%s=%s", k, v)
		var err error
		switch {
		case !dkimValueRe.MatchString(v) || strings.Count(v, `"`)%2 != 0:
			err = notAllowed(what, "the file is shell root sources; values are paths and names")
		case k == "RUNDIR":
			if v != "/run/opendkim" && v != "/var/run/opendkim" {
				err = notAllowed(what, "opendkim's run directory is /run/opendkim")
			}
		case k == "SOCKET":
			sock := strings.ReplaceAll(strings.Trim(v, `"`), "$RUNDIR", "/run/opendkim")
			if !dkimSocketRe.MatchString(sock) {
				err = notAllowed(what, "root removes and makes the socket; it is local in /run/opendkim or inet on the loopback")
			}
		case k == "USER" || k == "GROUP":
			if strings.Trim(v, `"`) != "opendkim" {
				err = notAllowed(what, "opendkim runs as opendkim")
			}
		case k == "PIDFILE":
			pid := strings.ReplaceAll(strings.ReplaceAll(strings.Trim(v, `"`), "$RUNDIR", "/run/opendkim"), "$NAME", "opendkim")
			if !dkimPIDRe.MatchString(pid) {
				err = notAllowed(what, "root writes the pid file; it belongs in /run/opendkim")
			}
		default:
			err = notAllowed(what, "the panel sets RUNDIR, SOCKET, USER, GROUP and PIDFILE; others (DAEMON_OPTS) reach opendkim's command line")
		}
		if err := atLine(n, err); err != nil {
			return err
		}
	}
	return nil
}
