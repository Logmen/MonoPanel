package osprofile

import (
	"strings"
	"testing"
)

const ubuntuRelease = `PRETTY_NAME="Ubuntu 24.04.4 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.4 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
`

const almaRelease = `NAME="AlmaLinux"
VERSION="10.0 (Purple Lion)"
ID="almalinux"
ID_LIKE="rhel centos fedora"
VERSION_ID="10.0"
PRETTY_NAME="AlmaLinux 10.0 (Purple Lion)"
`

const debianRelease = `PRETTY_NAME="Debian GNU/Linux 13 (trixie)"
NAME="Debian GNU/Linux"
VERSION_ID="13"
VERSION="13 (trixie)"
VERSION_CODENAME=trixie
ID=debian
`

func TestParseAndDetect(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		family Family
		major  string
		code   string
		apache string
	}{
		{"ubuntu", ubuntuRelease, FamilyDebian, "24", "noble", "www-data"},
		{"alma", almaRelease, FamilyRHEL, "10", "", "apache"},
		{"debian", debianRelease, FamilyDebian, "13", "trixie", "www-data"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rel, err := ParseOSRelease(strings.NewReader(c.src))
			if err != nil {
				t.Fatal(err)
			}
			rel.Arch = "amd64"
			if rel.Family() != c.family || rel.MajorVersion() != c.major || rel.Codename != c.code {
				t.Fatalf("parsed %+v", rel)
			}
			p, err := FromRelease(rel)
			if err != nil {
				t.Fatal(err)
			}
			if p.Web().ApacheUser != c.apache {
				t.Fatalf("apache user %q", p.Web().ApacheUser)
			}
			if p.Web().NginxUser != "nginx" {
				t.Fatalf("nginx user must be nginx (nginx.org packages) on every OS, got %q", p.Web().NginxUser)
			}
		})
	}
}

func TestUnsupported(t *testing.T) {
	rel, _ := ParseOSRelease(strings.NewReader("ID=arch\nPRETTY_NAME=Arch\n"))
	if _, err := FromRelease(rel); err == nil {
		t.Fatal("expected error for unsupported distro")
	}
}

func TestPackageQueryParsing(t *testing.T) {
	apt := aptManager{}
	got := apt.ParseQuery("nginx 1.28.0-1~noble installed\napache2 2.4.58 not-installed\n")
	if got["nginx"] != "1.28.0-1~noble" || len(got) != 1 {
		t.Fatalf("apt parse: %v", got)
	}
	dnf := dnfManager{}
	got = dnf.ParseQuery("nginx 1.28.0-1.el10.ngx\npackage httpd is not installed\n")
	if got["nginx"] != "1.28.0-1.el10.ngx" || len(got) != 1 {
		t.Fatalf("dnf parse: %v", got)
	}
	if strings.Join(apt.InstallArgv([]string{"nginx"}), " ") != "apt-get -q -y -o Dpkg::Options::=--force-confold --no-install-recommends install nginx" {
		t.Fatalf("apt install argv: %v", apt.InstallArgv([]string{"nginx"}))
	}
}
