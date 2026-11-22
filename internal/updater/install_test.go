package updater

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"monopanel/internal/osprofile"
)

// fakePackages is a package manager of shell scripts: each logs what it was
// asked and the environment it ran in, the package file names the version
// file says, and the package database holds the one installed says ("" for
// a package that is not installed, which fails the query as dpkg-query and
// rpm -q do).
type fakePackages struct {
	dir, log string
	env      []string
}

func newFakePackages(t *testing.T, file, installed string, env []string) *fakePackages {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	f := &fakePackages{dir: t.TempDir(), env: env}
	f.log = filepath.Join(f.dir, "calls.log")
	logEnv := `env | grep -E '^(DEBIAN_FRONTEND|NEEDRESTART_MODE|LANG|LC_ALL)=' | sort | sed 's/^/  /' >> "` + f.log + `"`
	fileQuery := "echo 'warning: a signature key the host does not know'\necho '" + file + "'"
	if file == "" {
		fileQuery = "echo 'cannot read the package' >&2\nexit 2"
	}
	query := "echo '" + installed + "'"
	if installed == "" {
		query = "echo 'package monopanel is not installed'\nexit 1"
	}
	for name, body := range map[string]string{
		"install":    `echo "install $*" >> "` + f.log + `"` + "\n" + logEnv + "\necho 'Setting up monopanel'",
		"reinstall":  `echo "reinstall $*" >> "` + f.log + `"` + "\n" + logEnv + "\necho 'Setting up monopanel'",
		"file-query": `echo "file-query $*" >> "` + f.log + `"` + "\n" + fileQuery,
		"query":      `echo "query $*" >> "` + f.log + `"` + "\n" + query,
	} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fakePackages) Env() []string { return f.env }
func (f *fakePackages) InstallArgv(pkgs []string) []string {
	return append([]string{filepath.Join(f.dir, "install")}, pkgs...)
}
func (f *fakePackages) ReinstallArgv(pkgs []string) []string {
	return append([]string{filepath.Join(f.dir, "reinstall")}, pkgs...)
}
func (f *fakePackages) PackageFileArgv(file string) []string {
	return []string{filepath.Join(f.dir, "file-query"), file}
}
func (f *fakePackages) QueryInstalledArgv(pkgs []string) []string {
	return append([]string{filepath.Join(f.dir, "query")}, pkgs...)
}
func (f *fakePackages) ParseQuery(out string) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if fs := strings.Fields(line); len(fs) == 2 {
			res[fs[0]] = fs[1]
		}
	}
	return res
}

// calls is what the fake was asked, with the environment of the installs.
func (f *fakePackages) calls(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// The package goes in with the package manager's environment — on Ubuntu
// NEEDRESTART_MODE=l, so that needrestart lists the services on old
// libraries instead of restarting them all, the database server too — and
// with update-run's own when the manager gives none. A package whose version
// the package database holds already — an update that rolled back put the
// previous binary back, the package database kept the new version — is
// installed again, which a plain install would not do; any other, and one
// whose version cannot be told, is installed as before.
func TestInstallerInstallsWithThePackageManager(t *testing.T) {
	debian, err := osprofile.FromRelease(osprofile.Release{ID: "ubuntu", IDLike: []string{"debian"}, VersionID: "24.04"})
	if err != nil {
		t.Fatal(err)
	}
	aptEnv := debian.Packages().Env()
	if !slices.Contains(aptEnv, "NEEDRESTART_MODE=l") || !slices.Contains(aptEnv, "DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("apt's environment: %v", aptEnv)
	}
	const pkg = "/var/lib/monopanel-agent/updates/monopanel_0.9.1_amd64.deb"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, c := range []struct {
		what, file, installed string
		env                   []string
		reinstall             bool
		wantEnv               []string
	}{
		{"after a rollback", "monopanel 0.9.1", "monopanel 0.9.1", aptEnv, true, aptEnv},
		{"over the version before", "monopanel 0.9.1", "monopanel 0.9.0", aptEnv, false, aptEnv},
		{"over the binary alone", "monopanel 0.9.1", "", aptEnv, false, aptEnv},
		{"a file that cannot be read", "", "monopanel 0.9.1", aptEnv, false, aptEnv},
		{"a manager without an environment", "monopanel 0.9.1-1", "monopanel 0.9.1-1", nil, true, defaultEnv},
	} {
		t.Run(c.what, func(t *testing.T) {
			fake := newFakePackages(t, c.file, c.installed, c.env)
			i := &Installer{Package: pkg, To: "0.9.1", Packages: fake, Timeout: time.Minute, Log: quiet}
			out, err := i.install(context.Background())
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			calls := "\n" + fake.calls(t)
			ran, other := "\ninstall "+pkg+"\n", "\nreinstall "
			if c.reinstall {
				ran, other = "\nreinstall "+pkg+"\n", "\ninstall "
			}
			if !strings.Contains(calls, ran) || strings.Contains(calls, other) {
				t.Fatalf("want %q alone:%s", strings.TrimSpace(ran), calls)
			}
			if c.file != "" && !strings.Contains(calls, "query monopanel\n") {
				t.Fatalf("the package database was not asked:\n%s", calls)
			}
			var env []string
			for _, line := range strings.Split(calls, "\n") {
				if strings.HasPrefix(line, "  ") {
					env = append(env, strings.TrimSpace(line))
				}
			}
			wantEnv := slices.Clone(c.wantEnv)
			slices.Sort(wantEnv)
			for _, kv := range wantEnv {
				if !slices.Contains(env, kv) {
					t.Fatalf("the install ran without %s: %v", kv, env)
				}
			}
			if !strings.HasPrefix(out, "Setting up monopanel") {
				t.Fatalf("output:\n%s", out)
			}
			if c.reinstall {
				if !strings.HasSuffix(out, "\nthe package database held "+strings.Fields(c.installed)[1]+" already, so it was installed again") {
					t.Fatalf("the output does not say it installed the package again:\n%s", out)
				}
			} else if strings.Contains(out, "installed again") {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
	// Without a package manager there is nothing to install with.
	if _, err := (&Installer{Package: pkg, Timeout: time.Minute, Log: quiet}).install(context.Background()); err == nil || !strings.Contains(err.Error(), "no install command") {
		t.Fatalf("no package manager: %v", err)
	}
}

// A package file's query names the package and its version on one line; a
// warning printed on the way is no such line.
func TestNameVersion(t *testing.T) {
	for _, c := range []struct{ out, name, version string }{
		{"monopanel 0.9.1\n", "monopanel", "0.9.1"},
		{"warning: /x.rpm: Header V4 RSA/SHA256 Signature, key ID 1: NOKEY\nmonopanel 0.9.1-1\n", "monopanel", "0.9.1-1"},
		{"dpkg-deb: error: not a debian format archive\n", "", ""},
		{"", "", ""},
	} {
		name, version, ok := nameVersion(c.out)
		if name != c.name || version != c.version || ok != (c.name != "") {
			t.Errorf("%q: %q %q %v", c.out, name, version, ok)
		}
	}
}
