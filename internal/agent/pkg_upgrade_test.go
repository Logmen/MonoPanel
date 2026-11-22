package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"monopanel/internal/osprofile"
)

// stubInstalled makes the installed packages those of the map.
func stubInstalled(t *testing.T, installed map[string]string) *[][]string {
	t.Helper()
	var asked [][]string
	old := queryInstalled
	queryInstalled = func(_ context.Context, _ osprofile.PackageManager, pkgs []string) map[string]string {
		asked = append(asked, append([]string{}, pkgs...))
		out := map[string]string{}
		for _, p := range pkgs {
			if v, ok := installed[p]; ok {
				out[p] = v
			}
		}
		return out
	}
	t.Cleanup(func() { queryInstalled = old })
	return &asked
}

// An upgrade takes the panel's own vendor packages by name, and of them only
// installed ones: a compromised API can neither install a package through it
// nor move one of the distribution's packages the panel does not manage.
func TestUpgradePolicy(t *testing.T) {
	ctx := context.Background()
	deb := withTrees(t, policyServer(t, debian13))
	el := withTrees(t, policyServer(t, alma9))
	stubInstalled(t, map[string]string{
		"nginx": "1.30.5-1~trixie", "php8.4-fpm": "8.4.27-1", "php8.4-common": "8.4.27-1", "php8.4-redis": "6.3.0-2", "php-common": "2:96",
		"apache2": "2.4.65-2", "apache2-bin": "2.4.65-2", "percona-server-server": "8.4.6-6-1.trixie", "openssh-server": "1:10.0p1-7", "php8.4-pgsql": "8.4.27-1",
		"php84-php-fpm": "8.4.27-1.el9.remi", "php84-php-common": "8.4.27-1.el9.remi", "php84-runtime": "8.4-1.el9.remi", "mysql-community-server": "8.4.7-1.el9", "httpd": "2.4.62-4.el9",
	})
	for _, c := range []struct {
		s    *Server
		pkgs []string
	}{
		{deb, []string{"nginx"}},
		{deb, []string{"php8.4-fpm", "php8.4-common", "php8.4-redis", "php-common"}},
		{deb, []string{"apache2", "apache2-bin"}},
		{deb, []string{"percona-server-server"}},
		{el, []string{"nginx", "php84-php-fpm", "php84-php-common", "php84-runtime"}},
		{el, []string{"mysql-community-server"}},
	} {
		if err := c.s.upgradable(ctx, c.s.profile.Packages(), c.pkgs); err != nil {
			t.Errorf("%s %v: %v", c.s.profile.Family(), c.pkgs, err)
		}
	}
	for _, c := range []struct {
		s    *Server
		pkgs []string
		want string
	}{
		// installed, but none of the panel's
		{deb, []string{"nginx", "openssh-server"}, "own packages only"},
		{deb, []string{"php8.4-pgsql"}, "own packages only"},
		{el, []string{"httpd"}, "own packages only"},
		{el, []string{"apache2"}, "own packages only"},
		{deb, []string{"php84-php-fpm"}, "own packages only"},
		// the panel's, but not installed: an upgrade installs nothing new
		{deb, []string{"nginx", "php8.3-fpm"}, "php8.3-fpm"},
		{deb, []string{"mysql-community-client"}, "not installed"},
		{el, []string{"percona-server-client"}, "not installed"},
	} {
		err := c.s.upgradable(ctx, c.s.profile.Packages(), c.pkgs)
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || !strings.Contains(ae.Message, c.want) {
			t.Errorf("%s %v: want a refusal with %q, got %v", c.s.profile.Family(), c.pkgs, c.want, err)
		}
	}

	// Through /v1/pkg: names only, as for install; the release packages the
	// panel installs by URL are no upgrade.
	for _, c := range []struct {
		s      *Server
		pkg    string
		status int
	}{
		{deb, "/var/cache/apt/archives/nginx_1.30.7-1~trixie_amd64.deb", http.StatusForbidden},
		{deb, "./nginx.deb", http.StatusForbidden},
		{deb, "https://nginx.org/packages/debian/pool/nginx/n/nginx/nginx_1.30.7-1~trixie_amd64.deb", http.StatusForbidden},
		{deb, PerconaReleaseDebURL, http.StatusForbidden},
		{el, "https://rpms.remirepo.net/enterprise/remi-release-9.rpm", http.StatusForbidden},
		{el, "/tmp/nginx.rpm", http.StatusForbidden},
		{deb, "-o", http.StatusBadRequest},
		{deb, "--allow-downgrades", http.StatusBadRequest},
		{el, "--setopt=gpgcheck=0", http.StatusBadRequest},
		{deb, "nginx=1.24.0-2", http.StatusBadRequest},
		{deb, "nginx/unstable", http.StatusBadRequest},
		{el, "nginx*", http.StatusBadRequest},
		{deb, "openssh-server", http.StatusForbidden},
		{deb, "php8.3-fpm", http.StatusForbidden},
	} {
		_, err := c.s.pkg(ctx, &PkgRequest{Action: "upgrade", Packages: []string{c.pkg}})
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != c.status {
			t.Errorf("upgrade %q: want %d, got %v", c.pkg, c.status, err)
		}
	}
	if _, err := deb.pkg(ctx, &PkgRequest{Action: "upgrade"}); err == nil {
		t.Error("an upgrade of nothing")
	}
}

// Every package the API asks about or upgrades is one the agent takes, on
// both families: the component sets and the agent's check are one list.
func TestUpgradeSetsAreTheAgents(t *testing.T) {
	for _, rel := range []string{debian13, alma9, oracle10} {
		s := policyServer(t, rel)
		p := s.profile
		check := func(component string, pkgs []string) {
			t.Helper()
			for _, pkg := range pkgs {
				if got := osprofile.UpgradeComponent(p, pkg); got != component {
					t.Errorf("%s: %s is %q, want %q", p.Release().ID, pkg, got, component)
				}
			}
		}
		check(osprofile.UpgradeNginx, osprofile.NginxPackages(p))
		check(osprofile.UpgradeApache, osprofile.ApachePackages(p))
		check(osprofile.UpgradePHP, osprofile.PHPSharedPackages(p))
		for _, v := range osprofile.PHPVersions(p) {
			pkgs := osprofile.PHPPackages(p, v.Version)
			if len(pkgs) == 0 {
				t.Fatalf("%s: no packages for PHP %s", p.Release().ID, v.Version)
			}
			check(osprofile.UpgradePHP, pkgs)
			if l := osprofile.PHP(p, v.Version); !strings.Contains(strings.Join(pkgs, " "), l.FPMPackage) {
				t.Errorf("PHP %s: the FPM package %s is not upgraded", v.Version, l.FPMPackage)
			}
		}
		for _, engine := range []string{"percona", "mysql"} {
			check(osprofile.UpgradeDB, osprofile.DBPackages(p, engine))
			if main := osprofile.DB(p, engine).Packages[0]; osprofile.UpgradeComponent(p, main) != osprofile.UpgradeDB {
				t.Errorf("%s: the server package %s is not upgraded", engine, main)
			}
		}
	}
	if osprofile.ApachePackages(policyServer(t, alma9).profile) != nil {
		t.Error("the panel installs no Apache on EL")
	}
}

// candidates reports the other version of installed packages only.
func TestUpgradesOf(t *testing.T) {
	got := upgradesOf(map[string]string{"nginx": "1.30.5-1~noble", "php8.4-fpm": "8.4.28-1", "apache2": "2.4.58-1"},
		map[string]osprofile.Candidate{
			"nginx":      {Version: "1.30.7-1~noble", Origin: "nginx.org"},
			"php8.4-fpm": {Version: "8.4.28-1", Origin: "packages.sury.org"}, // up to date
			"php8.3-fpm": {Version: "8.3.37-1"},                              // not installed
			"apache2":    {},
		})
	if len(got) != 1 || got["nginx"].Version != "1.30.7-1~noble" || got["nginx"].Origin != "nginx.org" {
		t.Fatalf("upgrades: %v", got)
	}
}

// A service whose binary an upgrade replaced still runs the old code: the
// kernel shows its program file as deleted.
func TestExeDeleted(t *testing.T) {
	dir := t.TempDir()
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })
	for pid, target := range map[string]string{"4242": "/usr/sbin/nginx (deleted)", "4243": "/usr/sbin/nginx"} {
		if err := os.Mkdir(filepath.Join(dir, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	if !exeDeleted(4242) {
		t.Error("a replaced binary is not seen")
	}
	if exeDeleted(4243) || exeDeleted(4244) {
		t.Error("a binary in place (or no process) counts as replaced")
	}
}

// nginx's own binary upgrade: the old master, still systemd's main
// process, finishes its requests on the replaced binary while the master
// the PID file names serves on the new one — that is no replaced binary,
// and a restart would cut those requests for nothing. A PID file that names
// the old master, nothing alive or another program is no upgrade.
func TestBinaryUpgraded(t *testing.T) {
	dir := t.TempDir()
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })
	for pid, target := range map[string]string{"100": "/usr/sbin/nginx (deleted)", "200": "/usr/sbin/nginx", "300": "/usr/bin/python3.12", "400": "/usr/sbin/nginx (deleted)"} {
		if err := os.Mkdir(filepath.Join(dir, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	pidFile := func(content string) string {
		p := filepath.Join(t.TempDir(), "nginx.pid")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := pidFile("200\n")
	if !binaryUpgraded(100, good) {
		t.Error("the new master on the new binary is not seen")
	}
	link := filepath.Join(t.TempDir(), "link.pid")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo.pid")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{
		"a link":                link,
		"a FIFO (no hang)":      fifo,
		"no PID file":           "",
		"a missing one":         filepath.Join(dir, "none.pid"),
		"the old master itself": pidFile("100\n"),
		"nothing alive":         pidFile("999\n"),
		"another program":       pidFile("300"),
		"another replaced one":  pidFile("400"),
		"garbage":               pidFile("nginx\n"),
	} {
		if binaryUpgraded(100, file) {
			t.Errorf("%s: counts as upgraded", name)
		}
	}
}

// A package manager run is not tied to the request: when the API goes away
// halfway (a restart of monopanel-api, a job's time limit) apt-get or dnf
// goes on to the end instead of being killed with dpkg in the middle of a
// transaction.
func TestPackageManagerOutlivesTheRequest(t *testing.T) {
	deb := withTrees(t, policyServer(t, debian13))
	stubInstalled(t, map[string]string{"nginx": "1.30.5-1~trixie"})
	oldHeld, oldRun := heldPackages, runPackageManager
	heldPackages = func(context.Context, osprofile.PackageManager) map[string]bool { return map[string]bool{} }
	t.Cleanup(func() { heldPackages, runPackageManager = oldHeld, oldRun })
	for _, req := range []*PkgRequest{{Action: "upgrade", Packages: []string{"nginx"}}, {Action: "install", Packages: []string{"nginx"}}, {Action: "update-index"}} {
		ctx, cancel := context.WithCancel(context.Background())
		var ran []string
		var during error
		runPackageManager = func(_ *Server, rctx context.Context, _ string, _ []string, argv ...string) (string, error) {
			ran = argv
			cancel() // the API is gone
			during = rctx.Err()
			return "", nil
		}
		if _, err := deb.pkg(ctx, req); err != nil {
			t.Fatalf("%s: %v", req.Action, err)
		}
		if len(ran) == 0 || ran[0] != "apt-get" {
			t.Fatalf("%s: ran %v", req.Action, ran)
		}
		if during != nil {
			t.Errorf("%s: the run ends with the request: %v", req.Action, during)
		}
		cancel()
	}

	// A request that ended while it waited for the lock starts nothing:
	// nobody would settle what it changed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	runPackageManager = func(*Server, context.Context, string, []string, ...string) (string, error) {
		ran = true
		return "", nil
	}
	if _, err := deb.pkg(ctx, &PkgRequest{Action: "upgrade", Packages: []string{"nginx"}}); err == nil || ran {
		t.Fatalf("an upgrade for a request that is gone: ran %v, %v", ran, err)
	}
}

// What the administrator holds (apt-mark hold) is left as it is: apt-get
// would fail the whole run on it, and the candidates report it apart.
func TestUpgradeLeavesHeldPackages(t *testing.T) {
	ctx := context.Background()
	deb := withTrees(t, policyServer(t, debian13))
	stubInstalled(t, map[string]string{"nginx": "1.30.5-1~trixie", "php8.4-fpm": "8.4.27-1"})
	old := heldPackages
	heldPackages = func(context.Context, osprofile.PackageManager) map[string]bool { return map[string]bool{"nginx": true} }
	t.Cleanup(func() { heldPackages = old })
	argv, err := deb.upgradeArgv(ctx, deb.profile.Packages(), []string{"nginx", "php8.4-fpm"})
	if err != nil || argv[len(argv)-1] != "php8.4-fpm" || slices.Contains(argv, "nginx") {
		t.Fatalf("a held package in the upgrade: %v %v", argv, err)
	}
	if !slices.Contains(argv, "--no-remove") || !slices.Contains(argv, "--only-upgrade") {
		t.Fatalf("the upgrade may remove or install: %v", argv)
	}
	var ae *Error
	if _, err := deb.upgradeArgv(ctx, deb.profile.Packages(), []string{"nginx"}); !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("only held packages: %v", err)
	}
	ups, held := splitHeld(map[string]osprofile.Candidate{"nginx": {Version: "1.30.7-1~trixie"}, "php8.4-fpm": {Version: "8.4.28-1"}}, map[string]bool{"nginx": true})
	if len(ups) != 1 || ups["php8.4-fpm"].Version == "" || len(held) != 1 || held["nginx"].Version != "1.30.7-1~trixie" {
		t.Fatalf("split: %v %v", ups, held)
	}
}

// withTrees gives s run and log directories of the test's own, made as the
// package makes them (the test's user stands in for root): the calls that
// run packages' scripts get past the check of the trees root's daemons act
// in (vetPackageScripts) to what the test is about, rather than being
// refused for a /run/monopanel the workstation does not have.
func withTrees(t *testing.T, s *Server) *Server {
	t.Helper()
	base := t.TempDir()
	s.cfg.RunDir, s.cfg.LogDir = filepath.Join(base, "run"), filepath.Join(base, "log")
	for _, d := range []string{s.cfg.RunDir, s.cfg.LogDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Chmod(d, 0o1771); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
