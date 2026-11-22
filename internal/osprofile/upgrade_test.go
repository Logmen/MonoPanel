package osprofile

import (
	"slices"
	"strings"
	"testing"
)

// apt-cache policy: the candidate and the host of the first repository that
// carries it; a package that is up to date has the installed version as its
// candidate (the agent drops it), one no repository has is absent.
func TestAptParseCandidates(t *testing.T) {
	out := `nginx:
  Installed: 1.30.5-1~noble
  Candidate: 1.30.7-1~noble
  Version table:
     1.30.7-1~noble 900
        900 https://nginx.org/packages/ubuntu noble/nginx amd64 Packages
 *** 1.30.5-1~noble 900
        900 https://nginx.org/packages/ubuntu noble/nginx amd64 Packages
        100 /var/lib/dpkg/status
     1.24.0-2ubuntu7.5 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
php8.5-fpm:
  Installed: 8.5.12-1+ubuntu24.04.1+deb.sury.org+1
  Candidate: 8.5.13-1+ubuntu24.04.1+deb.sury.org+1
  Version table:
     8.5.13-1+ubuntu24.04.1+deb.sury.org+1 500
        500 https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble/main amd64 Packages
 *** 8.5.12-1+ubuntu24.04.1+deb.sury.org+1 100
        100 /var/lib/dpkg/status
apache2:
  Installed: 2.4.58-1ubuntu8.8
  Candidate: 2.4.58-1ubuntu8.8
  Version table:
 *** 2.4.58-1ubuntu8.8 500
        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
        100 /var/lib/dpkg/status
ca-certificates:
  Installed: 20230311
  Candidate: 20240203
  Version table:
     20240203 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
 *** 20230311 100
        100 /var/lib/dpkg/status
php8.3-fpm:
  Installed: (none)
  Candidate: (none)
  Version table:
N: Unable to locate package php8.2-fpm
`
	got := aptManager{}.ParseCandidates(out)
	for name, want := range map[string]Candidate{
		"nginx":           {Version: "1.30.7-1~noble", Origin: "nginx.org"},
		"php8.5-fpm":      {Version: "8.5.13-1+ubuntu24.04.1+deb.sury.org+1", Origin: "ppa.launchpadcontent.net"},
		"apache2":         {Version: "2.4.58-1ubuntu8.8", Origin: "security.ubuntu.com"},
		"ca-certificates": {Version: "20240203", Origin: "archive.ubuntu.com"},
	} {
		if got[name] != want {
			t.Errorf("%s = %+v, want %+v", name, got[name], want)
		}
	}
	for _, name := range []string{"php8.3-fpm", "php8.2-fpm"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s has no candidate and must be absent", name)
		}
	}

	// A pin of 1000 or more on an older version (the administrator holding
	// nginx there) makes it the candidate, under the installed one in the
	// table: a downgrade apt-get -y refuses, no update. A library's header
	// may carry its architecture.
	got = aptManager{}.ParseCandidates(`nginx:
  Installed: 1.30.7-1~noble
  Candidate: 1.30.5-1~noble
  Version table:
 *** 1.30.7-1~noble 900
        900 https://nginx.org/packages/ubuntu noble/nginx amd64 Packages
        100 /var/lib/dpkg/status
     1.30.5-1~noble 1001
        900 https://nginx.org/packages/ubuntu noble/nginx amd64 Packages
libperconaserverclient21:amd64:
  Installed: 8.0.43-34-1.noble
  Candidate: 8.0.44-35-1.noble
  Version table:
     8.0.44-35-1.noble 500
        500 http://repo.percona.com/ps-80/apt noble/main amd64 Packages
 *** 8.0.43-34-1.noble 100
        100 /var/lib/dpkg/status
`)
	if c, ok := got["nginx"]; ok {
		t.Errorf("a downgrade counts as an update: %+v", c)
	}
	if c := got["libperconaserverclient21"]; c.Version != "8.0.44-35-1.noble" || c.Origin != "repo.percona.com" {
		t.Errorf("a header with its architecture: %+v (%v)", c, got)
	}
	if held := (aptManager{}).ParseHeld("nginx\nlibperconaserverclient21:amd64\n"); !held["nginx"] || !held["libperconaserverclient21"] {
		t.Errorf("held: %v", held)
	}
}

func TestDnfParseCandidates(t *testing.T) {
	got := dnfManager{}.ParseCandidates("nginx 1.30.7-1.el9.ngx nginx-stable\n\nphp84-php-fpm 8.4.28-1.el9.remi remi-safe\nWarning: something odd\n")
	if len(got) != 2 || got["nginx"] != (Candidate{Version: "1.30.7-1.el9.ngx", Origin: "nginx-stable"}) || got["php84-php-fpm"].Origin != "remi-safe" {
		t.Errorf("dnf candidates: %v", got)
	}
}

// An upgrade installs nothing new and keeps the configuration files; the
// listing asks dnf's cache only.
func TestUpgradeArgv(t *testing.T) {
	apt := strings.Join(aptManager{}.UpgradeArgv([]string{"nginx"}), " ")
	for _, want := range []string{"--only-upgrade install nginx", "Dpkg::Options::=--force-confold", aptLockWait, "-y", "--no-install-recommends"} {
		if !strings.Contains(apt, want) {
			t.Errorf("apt upgrade %q lacks %q", apt, want)
		}
	}
	if got := (dnfManager{}).UpgradeArgv([]string{"nginx"}); !slices.Equal(got, []string{"dnf", "-q", "-y", "upgrade", "nginx"}) {
		t.Errorf("dnf upgrade: %v", got)
	}
	dnf := strings.Join(dnfManager{}.CandidatesArgv([]string{"nginx"}), " ")
	for _, want := range []string{" -C ", "--upgrades", "--latest-limit 1", "%{repoid}"} {
		if !strings.Contains(dnf, want) {
			t.Errorf("dnf candidates %q lacks %q", dnf, want)
		}
	}
}

// apt's runs restart nothing behind the panel: Ubuntu's needrestart hook
// only lists what runs old code, it does not restart it (the database server
// among it) unasked and unchecked.
func TestAptEnvRestartsNothing(t *testing.T) {
	env := aptManager{}.Env()
	for _, want := range []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l"} {
		if !slices.Contains(env, want) {
			t.Errorf("apt's environment %v lacks %s", env, want)
		}
	}
}
