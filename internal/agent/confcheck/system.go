package confcheck

import (
	"fmt"
	"regexp"
	"strings"
)

// systemd-tmpfiles, systemd-sysusers and systemd-sysctl apply their files as
// root at boot: tmpfiles creates, writes, chowns and links any path
// (C, L, w, f, x, z/Z), sysusers creates accounts and adds them to groups,
// sysctl sets kernel parameters (kernel.core_pattern=| runs a program as
// root on every crash). The panel writes one line of each kind it needs.

func isTmpfiles(p string) bool { return under(p, "/etc/tmpfiles.d") }

var (
	// tmpfilesModeRe is a mode without special bits in which only the owner
	// (root) writes: in a directory others may write, they could put a
	// symlink where the root daemon creates its socket or pid file.
	tmpfilesModeRe = regexp.MustCompile(`^0?[0-7][0145][0145]$`)
	// plainPathRe is a path without quotes, C escapes (systemd unescapes
	// \x2e\x2e to ".."), specifiers or globs.
	plainPathRe = regexp.MustCompile(`^/[A-Za-z0-9._@+/-]*$`)
)

// checkTmpfiles allows "d <dir below RunDir> <mode> root root -": the
// php-fpm socket directory and others like it.
func checkTmpfiles(c *checker, _ string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' {
			continue
		}
		f := blankFields(s)
		what := fmt.Sprintf("tmpfiles line %q", s)
		var err error
		switch {
		case f[0] != "d" && f[0] != "D":
			err = notAllowed(what, "the panel makes directories (d); the other types write, copy, link, chown or remove paths as root at every boot")
		case len(f) < 2 || len(f) > 6:
			err = notAllowed(what, "d <path> <mode> <user> <group> [age]")
		default:
			p, ok := absPath(f[1])
			switch {
			case !ok || !plainPathRe.MatchString(f[1]) || !under(p, c.l.RunDir):
				err = notAllowed(what, "the panel's directories are below %s", c.l.RunDir)
			case len(f) > 2 && f[2] != "-" && !tmpfilesModeRe.MatchString(f[2]):
				err = notAllowed(what, "the mode has no special bits, and only root writes to the directory")
			case len(f) > 3 && f[3] != "root" && f[3] != "-":
				err = notAllowed(what, "the directory is root's: a root process makes its files there")
			case len(f) > 4 && f[4] != "root" && f[4] != "-":
				err = notAllowed(what, "the directory is root's: a root process makes its files there")
			case len(f) > 5 && f[5] != "-":
				err = notAllowed(what, "the panel's directories are not cleaned by age")
			}
		}
		if err := atLine(i+1, err); err != nil {
			return err
		}
	}
	return nil
}

func isSysusers(p string) bool { return under(p, "/etc/sysusers.d") }

var sysusersLineRe = regexp.MustCompile(`^(u|g|m)\s+(\S+)\s+(\S+)(?:\s+"([^"]*)"|\s+(-))?(?:\s+(\S+))?(?:\s+(\S+))?$`)

// checkSysusers allows the panel's own service user and groups with ids
// systemd picks: u and g with "-", and m into the panel's groups.
func checkSysusers(c *checker, _ string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	names := map[string]bool{c.l.ServiceUser: true, c.l.ServiceGroup: true, c.l.WebGroup: true, c.l.CLIGroup: true, sftpGroup: true}
	groups := map[string]bool{c.l.ServiceGroup: true, c.l.WebGroup: true, c.l.CLIGroup: true, sftpGroup: true}
	for i, raw := range lines {
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' {
			continue
		}
		m := sysusersLineRe.FindStringSubmatch(s)
		what := fmt.Sprintf("sysusers line %q", s)
		var err error
		switch {
		case m == nil:
			err = notAllowed(what, "the panel writes u, g and m lines for its own accounts")
		case m[1] == "m":
			if !names[m[2]] || !groups[m[3]] {
				err = notAllowed(what, "the panel adds its own accounts to its own groups only")
			}
		case !names[m[2]]:
			err = notAllowed(what, "the panel creates its own accounts only (%s)", c.l.ServiceUser)
		case m[3] != "-":
			err = notAllowed(what, "the id is systemd's choice (\"-\"): a given one could be root's or a system account's")
		case m[1] == "u":
			if home := m[6]; home != "" && home != "-" {
				if p, ok := absPath(home); !ok || !plainPathRe.MatchString(home) || !under(p, "/var/lib") {
					err = notAllowed(what, "the service user's home is in /var/lib")
				}
			}
			if sh := m[7]; sh != "" && sh != "-" && sh != "/usr/sbin/nologin" && sh != "/sbin/nologin" && sh != "/bin/false" && sh != "/usr/bin/false" {
				err = notAllowed(what, "the service user has no login shell")
			}
		case m[1] == "g":
			if m[6] != "" || m[7] != "" {
				err = notAllowed(what, "a group line takes a name and an id")
			}
		}
		if err := atLine(i+1, err); err != nil {
			return err
		}
	}
	return nil
}

func isSysctl(p string) bool { return under(p, "/etc/sysctl.d") || p == "/etc/sysctl.conf" }

// sysctlKeys are the kernel parameters the panel sets: nginx binds the
// sites' addresses before the host has them.
var sysctlKeys = map[string]bool{"net.ipv6.ip_nonlocal_bind": true, "net.ipv4.ip_nonlocal_bind": true}

func checkSysctl(_ *checker, _ string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		key := strings.ReplaceAll(trimBlanks(k), "/", ".")
		v = trimBlanks(v)
		if !ok || !sysctlKeys[key] || (v != "0" && v != "1") {
			return atLine(i+1, notAllowed(fmt.Sprintf("sysctl %q", s), "the panel sets net.ipv6.ip_nonlocal_bind and net.ipv4.ip_nonlocal_bind; other parameters (kernel.core_pattern, kernel.modprobe) run programs as root"))
		}
	}
	return nil
}

// SELinux: the mode for the next boot. The panel switches between
// enforcing and permissive (mp selinux) and keeps the rest of the file.

const selinuxConfig = "/etc/selinux/config"

func isSELinux(p string) bool { return p == selinuxConfig || under(p, "/etc/selinux") }

var selinuxValues = map[string]map[string]bool{
	"SELINUX":        {"enforcing": true, "permissive": true},
	"SELINUXTYPE":    {"targeted": true, "minimum": true, "mls": true},
	"SETLOCALDEFS":   {"0": true, "1": true},
	"REQUIRESEUSERS": {"0": true, "1": true},
}

func checkSELinux(_ *checker, p string, content []byte) error {
	if p != selinuxConfig {
		return notAllowed("writing "+p, "the panel writes %s alone; the policy files decide what every process may do", selinuxConfig)
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
		k, v, _ := strings.Cut(s, "=")
		if vals, ok := selinuxValues[k]; !ok || !vals[v] {
			return atLine(i+1, notAllowed(fmt.Sprintf("SELinux setting %q", s), "the panel sets SELINUX=enforcing|permissive and keeps SELINUXTYPE=targeted"))
		}
	}
	return nil
}

// nftables: rules are loaded by root with nft -f. They cannot run
// programs; an include could make nft read any file and print it in its
// error, which the agent hands back to the API.

func isNftables(p string) bool {
	return under(p, "/etc/nftables.d") || p == "/etc/nftables.conf" || p == "/etc/sysconfig/nftables.conf"
}

func checkNftables(_ *checker, _ string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		if nftHasInclude(raw) {
			return atLine(i+1, notAllowed("nftables include", "nft reads the file as root and prints what it cannot parse; the panel's rules are one file"))
		}
		if m := nftRedirectRe.FindStringSubmatch(raw); m != nil {
			return atLine(i+1, notAllowed(fmt.Sprintf("nftables %q", m[2]), "the panel's rules filter what comes in (input and forward); NAT and redirection would send the host's own connections elsewhere, apt's to nginx.org (plain http, signed by a keyring the API writes) to a server of the API's"))
		}
	}
	return nil
}

// nftIncludeRe finds the word include anywhere, in strings and comments
// too: whether "#" or a backslash inside a string hides what follows is a
// question of nft's version, and the panel's rules never say include.
var nftIncludeRe = regexp.MustCompile(`(^|[^A-Za-z0-9_])include([^A-Za-z0-9_]|$)`)

func nftHasInclude(line string) bool { return nftIncludeRe.MatchString(line) }

// nftRedirectRe finds, the same way, the words of NAT and of rerouting:
// nat and route chains, the statements that rewrite or move packets, and
// the hooks the panel's input and forward filters do not use.
var nftRedirectRe = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])(nat|route|dnat|snat|masquerade|redirect|tproxy|dup|fwd|queue|output|prerouting|postrouting|ingress|egress)([^A-Za-z0-9_]|$)`)

// The panel's own tree in /etc/monopanel: template overrides (read by the
// API, which renders them, and checked when what it renders is applied),
// the root-only EnvironmentFiles of the app units (their variables reach
// the account's own process) and the SELinux module semodule loads.

const monopanelEtc = "/etc/monopanel"

func isMonopanelEtc(p string) bool { return under(p, monopanelEtc) }

var selinuxModuleRule = regexp.MustCompile(`^\(allow httpd_t self \((capability \(sys_ptrace\)|process \(ptrace\))\)\)$`)

func checkMonopanelEtc(_ *checker, p string, content []byte) error {
	switch {
	case under(p, monopanelEtc+"/templates"):
		return nil
	case directChild(p, monopanelEtc+"/apps") && strings.HasSuffix(p, ".env"):
		_, err := textLines(content)
		return err
	case directChild(p, monopanelEtc+"/selinux") && strings.HasSuffix(p, ".cil"):
		lines, err := textLines(content)
		if err != nil {
			return err
		}
		for i, raw := range lines {
			s := raw
			if j := strings.IndexByte(s, ';'); j >= 0 {
				s = s[:j]
			}
			s = strings.Join(blankFields(s), " ")
			if s != "" && !selinuxModuleRule.MatchString(s) {
				return atLine(i+1, notAllowed(fmt.Sprintf("SELinux rule %q", s), "the panel's module lets php-fpm (httpd_t) trace its own workers; semodule installs anything else into the policy as root"))
			}
		}
		return nil
	}
	return notAllowed("writing "+p, "the panel writes templates/, apps/*.env and selinux/*.cil there")
}

// composer: /usr/local/bin is on root's PATH, so the wrapper is root's
// whenever an administrator runs composer. It may do nothing but exec the
// panel's composer.phar with one of the panel's PHP binaries.

const (
	composerBin  = "/usr/local/bin/composer"
	panelLib     = "/usr/local/lib/monopanel"
	composerPhar = panelLib + "/composer.phar"
	wpCLIPhar    = panelLib + "/wp-cli.phar"
)

func isComposerWrapper(p string) bool { return p == composerBin }

var composerExecRe = regexp.MustCompile(`^exec (?:(/usr/bin/php[0-9]\.[0-9]+|/usr/bin/php|/opt/remi/php[0-9]+/root/usr/bin/php) )? ?` + regexp.QuoteMeta(composerPhar) + ` "\$@"$`)

func checkComposerWrapper(_ *checker, _ string, content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	if len(lines) == 0 || lines[0] != "#!/bin/sh" {
		return notAllowed("the composer wrapper", "it is the panel's #!/bin/sh wrapper")
	}
	execs := 0
	for i, l := range lines[1:] {
		switch {
		case l == "" || strings.HasPrefix(l, "#"):
		case composerExecRe.MatchString(l):
			execs++
		default:
			return atLine(i+2, notAllowed(fmt.Sprintf("the composer wrapper line %q", l), "root runs the wrapper when an administrator types composer; it only execs %s with a PHP binary of the panel", composerPhar))
		}
	}
	if execs != 1 {
		return notAllowed("the composer wrapper", "it execs %s once", composerPhar)
	}
	return nil
}

func isPanelLib(p string) bool { return under(p, panelLib) }

// checkPanelLib allows the two PHP archives the panel downloads: composer
// (its checksum checked against getcomposer.org) and wp-cli. Their content
// is PHP code that the accounts run, and root when an administrator runs
// composer (see the report: the archive itself is not verified here).
func checkPanelLib(_ *checker, p string, _ []byte) error {
	if p == composerPhar || p == wpCLIPhar {
		return nil
	}
	return notAllowed("writing "+p, "the panel keeps composer.phar and wp-cli.phar there")
}

// /etc/default and /etc/sysconfig hold shell fragments and environment
// files of root's services; the panel writes two of them (handled by
// their families) and the Sphinx switch.

func isEnvDir(p string) bool { return under(p, "/etc/default") || under(p, "/etc/sysconfig") }

func checkEnvDir(_ *checker, p string, _ []byte) error {
	return notAllowed("writing "+p, "the files there are sourced by root's init scripts or are the environment of root's services; the panel writes %s, /etc/default/sphinxsearch and /etc/sysconfig/memcached alone", dkimDefaults)
}
