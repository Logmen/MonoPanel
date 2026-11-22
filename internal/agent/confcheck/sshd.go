package confcheck

import (
	"fmt"
	"regexp"
	"strings"
)

// sshd decides who logs in as whom: AuthorizedKeysFile, TrustedUserCAKeys,
// AuthorizedKeysCommand or PermitRootLogin in a drop-in would let the API
// log in as root, Banner shows any file to anyone who connects, Include
// pulls in files the check has not seen. The panel's drop-in chroots the
// SFTP-only accounts: Match blocks with ChrootDirectory %h, ForceCommand
// internal-sftp and the forwarding switched off. That is the allow-list.

func isSSH(p string) bool { return under(p, "/etc/ssh") }

var sshYesNo = regexp.MustCompile(`^(?i:yes|no)$`)

var sshKeywords = map[string]*regexp.Regexp{
	"passwordauthentication":       sshYesNo,
	"x11forwarding":                sshYesNo,
	"allowagentforwarding":         sshYesNo,
	"permittty":                    sshYesNo,
	"pubkeyauthentication":         sshYesNo,
	"kbdinteractiveauthentication": sshYesNo,
	"allowtcpforwarding":           regexp.MustCompile(`^(?i:yes|no|local|remote|all)$`),
	"allowstreamlocalforwarding":   regexp.MustCompile(`^(?i:yes|no|local|remote|all)$`),
	"permittunnel":                 regexp.MustCompile(`^(?i:yes|no|point-to-point|ethernet)$`),
	"chrootdirectory":              regexp.MustCompile(`^%h$`),
	"forcecommand":                 regexp.MustCompile(`^internal-sftp(?: -(?:u [0-7]{3,4}|d /[A-Za-z0-9._@/-]*|l [A-Z0-9]+|f [A-Z0-9]+|R|e))*$`),
}

var sshMatchName = regexp.MustCompile(`^[a-z_][a-z0-9_.-]*(,[a-z_][a-z0-9_.-]*)*$`)

func checkSSH(_ *checker, p string, content []byte) error {
	if !(under(p, "/etc/ssh/sshd_config.d") && strings.HasSuffix(p, ".conf")) {
		return notAllowed("writing "+p, "the panel writes its drop-in in /etc/ssh/sshd_config.d; host keys and the client's configuration are root's")
	}
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' {
			continue
		}
		kw, rest := s, ""
		if j := strings.IndexAny(s, " \t="); j >= 0 {
			kw, rest = s[:j], strings.TrimLeft(s[j:], " \t")
			rest = trimBlanks(strings.TrimPrefix(rest, "="))
		}
		key := strings.ToLower(kw)
		what := fmt.Sprintf("sshd %s %s", kw, rest)
		var err error
		if key == "match" {
			err = sshMatch(what, rest)
		} else if re, ok := sshKeywords[key]; !ok {
			err = notAllowed(fmt.Sprintf("sshd keyword %q", kw), "the panel's drop-in chroots SFTP accounts; keywords such as AuthorizedKeysFile, TrustedUserCAKeys, PermitRootLogin, Include or Banner decide who logs in as root or what root reads")
		} else if strings.ContainsAny(rest, "\"'") || !re.MatchString(rest) {
			err = notAllowed(what, "not one of the values the panel writes")
		}
		if err := atLine(i+1, err); err != nil {
			return err
		}
	}
	return nil
}

// sshMatch allows User and Group criteria.
func sshMatch(what, rest string) error {
	f := blankFields(rest)
	if len(f) == 0 || len(f)%2 != 0 {
		return notAllowed(what, "a Match names User and Group criteria")
	}
	for i := 0; i < len(f); i += 2 {
		switch strings.ToLower(f[i]) {
		case "user", "group":
			if !sshMatchName.MatchString(f[i+1]) {
				return notAllowed(what, "criteria are plain names")
			}
		default:
			return notAllowed(what, "the panel matches by User and Group")
		}
	}
	return nil
}
