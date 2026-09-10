package osprofile

import (
	"strings"
	"testing"
)

// apt-cache policy: a package no repository carries says "(none)".
func TestAptParseAvailable(t *testing.T) {
	out := `php8.5-fpm:
  Installed: (none)
  Candidate: 8.5.4-0ubuntu1.2
  Version table:
     8.5.4-0ubuntu1.2 500
        500 http://archive.ubuntu.com/ubuntu resolute-updates/main amd64 Packages
php8.4-fpm:
  Installed: (none)
  Candidate: (none)
  Version table:
N: Unable to locate package php8.3-fpm
`
	got := aptManager{}.ParseAvailable(out)
	if got["php8.5-fpm"] != "8.5.4-0ubuntu1.2" {
		t.Errorf("php8.5-fpm = %q", got["php8.5-fpm"])
	}
	if _, ok := got["php8.4-fpm"]; ok {
		t.Error("php8.4-fpm has no candidate and must be absent")
	}
	if _, ok := got["php8.3-fpm"]; ok {
		t.Error("an unknown package must be absent")
	}
}

func TestDnfParseAvailable(t *testing.T) {
	got := dnfManager{}.ParseAvailable("php84-php-fpm 8.4.12-1.el9.remi\nnginx 1.30.4-1.el9.ngx\n")
	if got["php84-php-fpm"] != "8.4.12-1.el9.remi" || got["nginx"] != "1.30.4-1.el9.ngx" || len(got) != 2 {
		t.Errorf("dnf parse: %v", got)
	}
}

// Oracle Linux names its EPEL package after itself; the others use epel-release.
func TestEPELPackage(t *testing.T) {
	for _, tc := range []struct{ release, want string }{
		{"ID=ol\nVERSION_ID=9.8\nID_LIKE=fedora\n", "oracle-epel-release-el9"},
		{"ID=ol\nVERSION_ID=10.1\nID_LIKE=fedora\n", "https://dl.fedoraproject.org/pub/epel/epel-release-latest-10.noarch.rpm"},
		{"ID=rocky\nVERSION_ID=10.1\nID_LIKE=\"rhel centos fedora\"\n", "epel-release"},
		{"ID=debian\nVERSION_ID=13\n", ""},
	} {
		rel, _ := ParseOSRelease(strings.NewReader(tc.release))
		p, err := FromRelease(rel)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.EPELPackage(); got != tc.want {
			t.Errorf("%q: EPELPackage = %q, want %q", tc.release, got, tc.want)
		}
	}
}
