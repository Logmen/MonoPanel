package confcheck

import (
	"bytes"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Package repositories: apt and dnf install packages as root, and a package
// runs its maintainer scripts as root. A source on a host of the API's
// choosing, trusted=yes, a file: or mirror+file: URI, or gpgcheck=0 is a
// root shell at the next install. The panel adds the vendors' repositories
// for nginx, PHP and MySQL, each signed by the vendor's key in a keyring of
// its own: hosts, paths and keyrings are those.

type aptSource struct {
	host, path, keyring string
}

// aptSources are the panel's apt repositories. PHP comes from
// packages.sury.org on Ubuntu as on Debian: ppa:ondrej/php, which the panel
// set up on Ubuntu up to 0.8.19 (ondrej-php.list, ondrej-php.asc), is not
// written any more. The API moves such a host to Sury and removes the PPA's
// two files (paths/remove, which the write allow-list decides).
var aptSources = []aptSource{
	{"nginx.org", "/packages/", "/etc/apt/keyrings/nginx.asc"},
	{"packages.sury.org", "/php/", "/etc/apt/keyrings/sury-php.gpg"},
	{"repo.mysql.com", "/apt/", "/etc/apt/keyrings/mysql.asc"},
}

// aptKeyrings are the keyrings the panel downloads: armored (.asc) or
// binary (.gpg) public keys.
var aptKeyrings = map[string]bool{"nginx.asc": true, "sury-php.gpg": true, "mysql.asc": true}

func isApt(p string) bool { return under(p, "/etc/apt") }

func checkApt(_ *checker, p string, content []byte) error {
	switch {
	case directChild(p, "/etc/apt/sources.list.d"):
		if !strings.HasSuffix(p, ".list") {
			return notAllowed("writing "+p, "the panel writes one-line *.list sources")
		}
		return checkAptList(content)
	case directChild(p, "/etc/apt/preferences.d"):
		return checkAptPreferences(content)
	case directChild(p, "/etc/apt/keyrings"):
		return checkKeyring(p, content)
	}
	return notAllowed("writing "+p, "the panel writes sources, preferences and keyrings; apt.conf.d and the rest run commands and decide what apt trusts")
}

var (
	aptTokenRe  = regexp.MustCompile(`^[A-Za-z0-9._/~+-]+$`)
	aptArchRe   = regexp.MustCompile(`^[a-z0-9-]+(,[a-z0-9-]+)*$`)
	aptOptionRe = regexp.MustCompile(`^\[([^\]]*)\]$`)
)

func checkAptList(content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' {
			continue
		}
		if err := atLine(i+1, aptLine(s)); err != nil {
			return err
		}
	}
	return nil
}

func aptLine(s string) error {
	what := fmt.Sprintf("apt source %q", s)
	f := blankFields(s)
	if len(f) < 3 || (f[0] != "deb" && f[0] != "deb-src") {
		return notAllowed(what, "a source is \"deb [signed-by=…] URI suite components\"")
	}
	f = f[1:]
	keyring := ""
	if strings.HasPrefix(f[0], "[") {
		// The options are one bracketed word list, possibly with blanks.
		end := 0
		for end < len(f) && !strings.HasSuffix(f[end], "]") {
			end++
		}
		if end == len(f) {
			return notAllowed(what, "the options are not closed")
		}
		m := aptOptionRe.FindStringSubmatch(strings.Join(f[:end+1], " "))
		if m == nil {
			return notAllowed(what, "bad options")
		}
		for _, o := range blankFields(m[1]) {
			k, v, _ := strings.Cut(o, "=")
			switch k {
			case "signed-by":
				keyring = v
			case "arch":
				if !aptArchRe.MatchString(v) {
					return notAllowed(what, "bad architecture list")
				}
			default:
				return notAllowed(fmt.Sprintf("apt option %q", o), "only signed-by and arch are set; trusted=yes and the allow-* options let apt install unsigned packages")
			}
		}
		f = f[end+1:]
	}
	if len(f) < 2 {
		return notAllowed(what, "a source names a URI and a suite")
	}
	src, err := aptRepository(f[0])
	if err != nil {
		return notAllowed(fmt.Sprintf("apt URI %q", f[0]), "%v", err)
	}
	if keyring != src.keyring {
		return notAllowed(fmt.Sprintf("apt source %q signed by %q", f[0], keyring), "the repository is signed by the vendor's key, %s", src.keyring)
	}
	for _, t := range f[1:] {
		if !aptTokenRe.MatchString(t) || strings.Contains(t, "..") {
			return notAllowed(fmt.Sprintf("apt suite or component %q", t), "not a plain name")
		}
	}
	return nil
}

// aptRepository matches a URI against the panel's repositories.
func aptRepository(raw string) (aptSource, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Path, "..") {
		// Over plain http the keyring alone vouches for the packages, and
		// the keyring is a file the API writes: TLS keeps the vendor's
		// server the one that serves them.
		return aptSource{}, fmt.Errorf("the panel's repositories are https URIs of their vendors")
	}
	p := path.Clean(u.Path) + "/"
	for _, s := range aptSources {
		if u.Hostname() == s.host && strings.HasPrefix(p, s.path) {
			return s, nil
		}
	}
	return aptSource{}, fmt.Errorf("the panel adds the repositories of nginx.org, packages.sury.org/php and repo.mysql.com; a package from anywhere else runs its scripts as root")
}

// checkAptPreferences allows pins with a priority from 1 to 999: from 1000
// on apt installs the pinned version even when that is a downgrade (to an
// old, vulnerable one: the vendors' repositories keep every release), and a
// negative one keeps a package from ever being installed or upgraded, its
// security updates included (apt_preferences(5); 0 is undefined there).
func checkAptPreferences(content []byte) error {
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	for i, raw := range lines {
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(s, ":")
		v = trimBlanks(v)
		var err error
		switch k {
		case "Package", "Pin", "Explanation":
			if !ok {
				err = notAllowed(fmt.Sprintf("apt preference %q", s), "bad line")
			}
		case "Pin-Priority":
			if n, perr := strconv.Atoi(v); perr != nil || n < 1 || n >= 1000 {
				err = notAllowed(fmt.Sprintf("apt preference %q", s), "a priority of 1000 or more downgrades packages, one below 1 keeps them from their updates; the panel pins from 1 to 999")
			}
		default:
			err = notAllowed(fmt.Sprintf("apt preference %q", s), "a preference has Package, Pin and Pin-Priority")
		}
		if err := atLine(i+1, err); err != nil {
			return err
		}
	}
	return nil
}

// checkKeyring allows the panel's keyrings by name. What a keyring holds
// decides nothing by itself: a key is trusted for the source that names it,
// and every source is one of the vendors' (checkAptList).
func checkKeyring(p string, content []byte) error {
	name := path.Base(p)
	if !aptKeyrings[name] {
		return notAllowed("the keyring "+p, "the panel's keyrings are %s; a source signed by another key is not one of its repositories", strings.Join(slices.Sorted(maps.Keys(aptKeyrings)), ", "))
	}
	if bytes.Contains(content, []byte("PRIVATE KEY")) {
		return notAllowed("the keyring "+p, "a keyring holds public keys")
	}
	return nil
}

// yum/dnf repositories: the same rule, and gpgcheck stays on.

func isYum(p string) bool { return under(p, "/etc/yum.repos.d") }

type yumRepo struct {
	host, path string
}

var yumBaseURLs = []yumRepo{{"nginx.org", "/packages/"}, {"repo.mysql.com", "/yum/"}}

var yumKeys = map[string]bool{
	"name": true, "baseurl": true, "enabled": true, "gpgcheck": true, "pkg_gpgcheck": true, "repo_gpgcheck": true,
	"gpgkey": true, "module_hotfixes": true, "priority": true, "skip_if_unavailable": true, "countme": true,
	"type": true, "metadata_expire": true, "cost": true, "exclude": true, "excludepkgs": true, "includepkgs": true,
	"enabled_metadata": true,
}

var (
	yumSectionRe = regexp.MustCompile(`^\[([A-Za-z0-9._:-]+)\]$`)
	yumTrueRe    = regexp.MustCompile(`^(?i:1|yes|true|on)$`)
)

func checkYum(_ *checker, p string, content []byte) error {
	if !strings.HasSuffix(p, ".repo") {
		return notAllowed("writing "+p, "the panel writes *.repo files")
	}
	lines, err := textLines(content)
	if err != nil {
		return err
	}
	section := ""
	gpg := map[string]bool{}
	var sections []string
	lastKey := ""
	for i, raw := range lines {
		n := i + 1
		s := trimBlanks(raw)
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		if raw[0] == ' ' || raw[0] == '\t' {
			// A continuation line: more URLs of baseurl or gpgkey.
			if lastKey != "baseurl" && lastKey != "gpgkey" {
				return unreadable(n, "a continuation line")
			}
			if err := atLine(n, yumURLs(lastKey, s)); err != nil {
				return err
			}
			continue
		}
		if m := yumSectionRe.FindStringSubmatch(s); m != nil {
			section = m[1]
			sections = append(sections, section)
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		k, v = strings.ToLower(trimBlanks(k)), trimBlanks(v)
		if !ok || section == "" {
			return unreadable(n, "not a \"key=value\" line in a repository")
		}
		lastKey = k
		var err error
		switch {
		case !yumKeys[k]:
			err = notAllowed(fmt.Sprintf("yum setting %q", k), "the panel's repositories set baseurl, gpgkey and their switches; mirrorlist, metalink, proxy and sslverify change where and how packages come from")
		case k == "gpgcheck" || k == "pkg_gpgcheck":
			if !yumTrueRe.MatchString(v) {
				err = notAllowed(fmt.Sprintf("yum %s=%s", k, v), "packages are installed as root only with their signature checked")
			}
			gpg[section] = true
		case k == "baseurl" || k == "gpgkey":
			err = yumURLs(k, v)
		}
		if err := atLine(n, err); err != nil {
			return err
		}
	}
	for _, s := range sections {
		if !gpg[s] {
			return notAllowed(fmt.Sprintf("the repository [%s] without gpgcheck=1", s), "packages are installed as root only with their signature checked")
		}
	}
	return nil
}

func yumURLs(key, v string) error {
	for _, raw := range strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		if key == "gpgkey" && strings.HasPrefix(raw, "file:///etc/pki/rpm-gpg/") && !strings.Contains(raw, "..") {
			continue
		}
		u, err := url.Parse(raw)
		bad := err != nil || u.User != nil || u.Port() != "" || u.RawQuery != "" || strings.Contains(u.Path, "..") || strings.Contains(u.Host, "$")
		if !bad && key == "gpgkey" {
			// Keys come over TLS: dnf imports them without asking.
			bad = u.Scheme != "https"
		}
		if !bad && key == "baseurl" {
			// as for apt: the vendor's server, over TLS
			bad = u.Scheme != "https"
		}
		ok := false
		if !bad {
			p := path.Clean(u.Path) + "/"
			for _, r := range yumBaseURLs {
				if u.Hostname() == r.host && (key == "gpgkey" || strings.HasPrefix(p, r.path)) {
					ok = true
				}
			}
		}
		if !ok {
			return notAllowed(fmt.Sprintf("yum %s %s", key, raw), "the panel's repositories are nginx.org and repo.mysql.com over https")
		}
	}
	return nil
}
