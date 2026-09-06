package osprofile

import (
	"strings"
	"testing"
)

func TestPHPLayouts(t *testing.T) {
	rel, _ := ParseOSRelease(strings.NewReader(ubuntuRelease))
	deb, _ := FromRelease(rel)
	l := PHP(deb, "8.4")
	if l.FPMService != "php8.4-fpm.service" || l.PoolDir != "/etc/php/8.4/fpm/pool.d" || l.CLIBinary != "/usr/bin/php8.4" {
		t.Fatalf("sury layout: %+v", l)
	}
	if !contains(l.CorePackages, "php8.4-fpm") || !contains(l.CorePackages, "php8.4-mysql") || contains(l.ExtraPackages, "php8.4-imap") || contains(l.ExtraPackages, "php8.4-mcrypt") {
		t.Fatalf("sury packages 8.4: core=%v extra=%v", l.CorePackages, l.ExtraPackages)
	}
	old := PHP(deb, "5.6")
	if contains(old.CorePackages, "php5.6-opcache") || !contains(old.ExtraPackages, "php5.6-mcrypt") || contains(old.ExtraPackages, "php5.6-sodium") {
		t.Fatalf("sury packages 5.6: core=%v extra=%v", old.CorePackages, old.ExtraPackages)
	}
	rel, _ = ParseOSRelease(strings.NewReader(almaRelease))
	el, _ := FromRelease(rel)
	r := PHP(el, "8.4")
	if r.FPMService != "php84-php-fpm.service" || r.PoolDir != "/etc/opt/remi/php84/php-fpm.d" || !contains(r.CorePackages, "php84-php-mysqlnd") {
		t.Fatalf("remi layout: %+v", r)
	}
	m := PHPVersions(el)
	if len(m) != 12 || m[0].Version != "5.6" || m[0].Available || !m[11].Available || m[11].Support != "active" {
		t.Fatalf("matrix EL10: %+v", m)
	}
	if !ValidPHPVersion("8.3") || ValidPHPVersion("9.9") {
		t.Fatal("version validation")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
