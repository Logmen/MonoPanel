package osprofile

import (
	"fmt"
	"strings"
)

// PHPSource identifies where PHP packages come from.
const (
	PHPSourceSury = "sury"
	PHPSourceRemi = "remi"
)

// PHPLayout describes one installed PHP version on this OS: package names,
// binaries, config directories and the FPM unit.
type PHPLayout struct {
	Version      string   `json:"version"`
	Source       string   `json:"source"`
	FPMBinary    string   `json:"fpm_binary"`
	FPMService   string   `json:"fpm_service"`
	FPMConfDir   string   `json:"fpm_conf_dir"`
	FPMConf      string   `json:"fpm_conf"`
	PoolDir      string   `json:"pool_dir"`
	IniDirs      []string `json:"ini_dirs"`
	CLIBinary    string   `json:"cli_binary"`
	FPMCheckArgv []string `json:"fpm_check_argv"`
	// CorePackages must all install; ExtraPackages are best effort (old
	// branches lack some extensions).
	CorePackages  []string `json:"core_packages"`
	ExtraPackages []string `json:"extra_packages"`
	// RepoPackages are the packages of the repository itself (used to detect
	// whether the version is installed): the FPM package.
	FPMPackage string `json:"fpm_package"`
}

// PHPVersionInfo is one entry of the availability matrix.
type PHPVersionInfo struct {
	Version   string `json:"version"`
	Support   string `json:"support"` // active | security | eol
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

// phpSupport is the upstream status as of 2026-09 (see docs/02 §3.1).
var phpSupport = map[string]string{
	"5.6": "eol", "7.0": "eol", "7.1": "eol", "7.2": "eol", "7.3": "eol", "7.4": "eol", "8.0": "eol", "8.1": "eol",
	"8.2": "security", "8.3": "security", "8.4": "active", "8.5": "active",
}

var phpAllVersions = []string{"5.6", "7.0", "7.1", "7.2", "7.3", "7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"}

// PHPVersions returns the availability matrix for a profile.
func PHPVersions(p Profile) []PHPVersionInfo {
	out := make([]PHPVersionInfo, 0, len(phpAllVersions))
	for _, v := range phpAllVersions {
		info := PHPVersionInfo{Version: v, Support: phpSupport[v], Available: true}
		if p.Family() == FamilyRHEL && p.Release().MajorVersion() == "10" && versionLess(v, "7.4") {
			info.Available = false
			info.Note = "Remi для EL10 не собирает ветки старше 7.4"
		}
		if p.Family() == FamilyUnknown {
			info.Available = false
			info.Note = "нет источника пакетов для этой ОС"
		}
		out = append(out, info)
	}
	return out
}

func versionLess(a, b string) bool {
	var am, an, bm, bn int
	fmt.Sscanf(a, "%d.%d", &am, &an)
	fmt.Sscanf(b, "%d.%d", &bm, &bn)
	return am < bm || (am == bm && an < bn)
}

// ValidPHPVersion reports whether v is a known branch like "8.4".
func ValidPHPVersion(v string) bool {
	for _, k := range phpAllVersions {
		if k == v {
			return true
		}
	}
	return false
}

// PHPExtensions is the standard extension set (see docs/02 §3.2), in the
// vendor-neutral names used for package lists below.
var phpCoreExt = []string{"mysql", "mbstring", "intl", "gd", "curl", "zip", "xml", "soap", "bcmath", "opcache", "readline", "sqlite3"}
var phpExtraExt = []string{"gmp", "imagick", "redis", "apcu", "igbinary", "memcached", "memcache", "xsl", "ldap", "imap", "mcrypt", "sodium"}

func suryLayout(v string) PHPLayout {
	base := "/etc/php/" + v
	l := PHPLayout{
		Version: v, Source: PHPSourceSury,
		FPMBinary:  "/usr/sbin/php-fpm" + v,
		FPMService: "php" + v + "-fpm.service",
		FPMConfDir: base + "/fpm",
		FPMConf:    base + "/fpm/php-fpm.conf",
		PoolDir:    base + "/fpm/pool.d",
		IniDirs:    []string{base + "/fpm/conf.d", base + "/cli/conf.d"},
		CLIBinary:  "/usr/bin/php" + v,
		FPMPackage: "php" + v + "-fpm",
	}
	l.FPMCheckArgv = []string{l.FPMBinary, "-t", "-y", l.FPMConf}
	l.CorePackages = []string{"php" + v + "-fpm", "php" + v + "-cli"}
	for _, e := range phpCoreExt {
		// Отдельный пакет opcache существует только с 7.0 по 8.4: в 5.6 он
		// внутри ядра, а начиная с 8.5 Sury его не собирает — opcache встроен,
		// и php8.5-opcache в репозитории просто нет.
		if e == "opcache" && (versionLess(v, "7.0") || !versionLess(v, "8.5")) {
			continue
		}
		if e == "sqlite3" && versionLess(v, "7.0") {
			continue
		}
		l.CorePackages = append(l.CorePackages, "php"+v+"-"+e)
	}
	for _, e := range phpExtraExt {
		switch e {
		case "mcrypt":
			if !versionLess(v, "7.2") {
				continue
			}
		case "sodium":
			if versionLess(v, "7.2") {
				continue
			}
		case "imap":
			if !versionLess(v, "8.4") {
				continue
			}
		}
		l.ExtraPackages = append(l.ExtraPackages, "php"+v+"-"+e)
	}
	return l
}

func remiLayout(v string) PHPLayout {
	short := "php" + strings.ReplaceAll(v, ".", "")
	root := "/opt/remi/" + short + "/root"
	conf := "/etc/opt/remi/" + short
	l := PHPLayout{
		Version: v, Source: PHPSourceRemi,
		FPMBinary:  root + "/usr/sbin/php-fpm",
		FPMService: short + "-php-fpm.service",
		FPMConfDir: conf,
		FPMConf:    conf + "/php-fpm.conf",
		PoolDir:    conf + "/php-fpm.d",
		IniDirs:    []string{conf + "/php.d"},
		CLIBinary:  root + "/usr/bin/php",
		FPMPackage: short + "-php-fpm",
	}
	l.FPMCheckArgv = []string{l.FPMBinary, "-t", "-y", l.FPMConf}
	l.CorePackages = []string{short + "-php-fpm", short + "-php-cli", short + "-php-mysqlnd", short + "-php-mbstring", short + "-php-intl", short + "-php-gd", short + "-php-pecl-zip", short + "-php-xml", short + "-php-soap", short + "-php-bcmath", short + "-php-opcache", short + "-php-pdo", short + "-php-process"}
	l.ExtraPackages = []string{short + "-php-gmp", short + "-php-pecl-imagick", short + "-php-pecl-redis6", short + "-php-pecl-apcu", short + "-php-pecl-igbinary", short + "-php-pecl-memcached", short + "-php-pecl-memcache", short + "-php-ldap", short + "-php-sodium"}
	return l
}

// PHP returns the layout of a version for a profile (nil for unsupported OS).
func PHP(p Profile, version string) *PHPLayout {
	switch p.Family() {
	case FamilyDebian:
		l := suryLayout(version)
		return &l
	case FamilyRHEL:
		l := remiLayout(version)
		return &l
	}
	return nil
}
