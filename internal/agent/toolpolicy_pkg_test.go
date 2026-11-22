package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Packages are names; the release packages the panel installs are taken by
// their exact URL, on the host they are for.
func TestPkgPolicy(t *testing.T) {
	ctx := context.Background()
	el := policyServer(t, alma9)
	ol := policyServer(t, oracle10)
	deb := policyServer(t, debian13)
	names := []string{"nginx", "php8.4-fpm", "php84-php-pecl-redis6", "percona-server-server", "policycoreutils-python-utils", "libapache2-mod-fcgid", "g++", "libstdc++6", "valkey-server", "epel-release", "oracle-epel-release-el9"}
	for _, s := range []*Server{el, deb} {
		for _, action := range []string{"install", "remove", "query", "available"} {
			got, cleanup, err := s.pkgArgs(ctx, action, names)
			if err != nil || strings.Join(got, " ") != strings.Join(names, " ") {
				t.Errorf("%s %v: %v %v", action, names, got, err)
			}
			if cleanup != nil {
				cleanup()
			}
		}
	}
	for _, c := range []struct {
		s   *Server
		url string
	}{
		{el, "https://rpms.remirepo.net/enterprise/remi-release-9.rpm"},
		{el, "https://repo.percona.com/yum/percona-release-latest.noarch.rpm"},
		{ol, "https://dl.fedoraproject.org/pub/epel/epel-release-latest-10.noarch.rpm"},
		{ol, "https://rpms.remirepo.net/enterprise/remi-release-10.rpm"},
	} {
		got, cleanup, err := c.s.pkgArgs(ctx, "install", []string{c.url})
		if err != nil || len(got) != 1 || got[0] != c.url {
			t.Errorf("%s: %v %v", c.url, got, err)
		}
		cleanup()
	}
	if ol.profile.EPELPackage() != "https://dl.fedoraproject.org/pub/epel/epel-release-latest-10.noarch.rpm" {
		t.Fatalf("osprofile changed the EPEL package of Oracle Linux 10: %s", ol.profile.EPELPackage())
	}
	// Debian: the agent downloads percona-release itself
	staged := filepath.Join(t.TempDir(), "percona-release_latest.generic_all.deb")
	old := fetchReleasePackage
	fetchReleasePackage = func(_ context.Context, url, want string) (string, string, error) {
		if url != PerconaReleaseDebURL || want != "percona-release" {
			t.Errorf("fetch %s as %s", url, want)
		}
		return staged, filepath.Dir(staged), os.WriteFile(staged, []byte("deb"), 0o600)
	}
	t.Cleanup(func() { fetchReleasePackage = old })
	got, cleanup, err := deb.pkgArgs(ctx, "install", []string{PerconaReleaseDebURL, "nginx"})
	if err != nil || len(got) != 2 || got[0] != staged || got[1] != "nginx" {
		t.Fatalf("percona-release: %v %v", got, err)
	}
	cleanup()
	if _, err := os.Stat(staged); err == nil {
		t.Fatal("the download is not cleaned up")
	}

	for _, c := range []struct {
		s      *Server
		action string
		pkg    string
		status int
	}{
		{deb, "install", "/var/lib/monopanel/downloads/percona-release_latest.generic_all.deb", http.StatusForbidden},
		{deb, "install", "./evil.deb", http.StatusForbidden},
		{deb, "install", "evil.deb", http.StatusForbidden},
		{el, "install", "/tmp/evil.rpm", http.StatusForbidden},
		{el, "install", "evil.rpm", http.StatusForbidden},
		{el, "install", "https://evil.example.com/remi-release-9.rpm", http.StatusForbidden},
		{el, "install", "https://rpms.remirepo.net/enterprise/remi-release-8.rpm", http.StatusForbidden}, // another EL
		{el, "install", "https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm", http.StatusForbidden},
		{el, "install", PerconaReleaseDebURL, http.StatusForbidden},
		{deb, "install", "https://repo.percona.com/yum/percona-release-latest.noarch.rpm", http.StatusForbidden},
		{el, "query", "https://repo.percona.com/yum/percona-release-latest.noarch.rpm", http.StatusForbidden},
		{deb, "install", "nginx; rm -rf /", http.StatusBadRequest},
		{deb, "install", "-o", http.StatusBadRequest},
		{deb, "install", "--allow-unauthenticated", http.StatusBadRequest},
		{el, "install", "--nogpgcheck", http.StatusBadRequest},
		{el, "install", "--setopt=gpgcheck=0", http.StatusBadRequest},
		{deb, "install", "nginx=1.2.3", http.StatusBadRequest},
		{deb, "install", "nginx/unstable", http.StatusBadRequest},
		{el, "install", "*", http.StatusBadRequest},
		{el, "install", "@development", http.StatusBadRequest},
		{deb, "install", "Nginx", http.StatusBadRequest},
		{deb, "install", "", http.StatusBadRequest},
	} {
		_, _, err := c.s.pkgArgs(ctx, c.action, []string{c.pkg})
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != c.status {
			t.Errorf("%s %q: want %d, got %v", c.action, c.pkg, c.status, err)
		}
	}
}
