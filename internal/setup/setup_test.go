package setup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent"
	"monopanel/internal/agent/confcheck"
	"monopanel/internal/config"
	"monopanel/internal/systemd"
)

// mp setup makes the panel's certificate as root in the API's own data
// directory: a symlink the API left there in place of the key is replaced,
// never written through, and the pair is made again; plain files stay.
func TestSelfSignedReplacesAPlantedSymlink(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	tls := filepath.Join(cfg.DataDir, "tls")
	if err := os.Mkdir(tls, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(victim, []byte("root:x:0:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, key := cfg.TLSCertPath(), cfg.TLSKeyPath()
	if err := os.Symlink(victim, key); err != nil {
		t.Fatal(err)
	}
	created, err := ensureSelfSigned(cfg, cert, key, []string{"panel.example.com"})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "root:x:0:0\n" {
		t.Fatalf("written through the symlink: %q", b)
	}
	for _, p := range []string{cert, key} {
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Errorf("the key: mode %v", st.Mode())
	}
	before, _ := os.ReadFile(key)
	if created, err := ensureSelfSigned(cfg, cert, key, []string{"panel.example.com"}); err != nil || created {
		t.Fatalf("again: created=%v err=%v", created, err)
	}
	if after, _ := os.ReadFile(key); string(after) != string(before) {
		t.Error("an existing key was replaced")
	}
}

// The database and its journals are opened by name as root: anything but a
// plain file there stops mp setup.
func TestDBFilesMustBePlain(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "panel.db")
	if err := checkDBFiles(db); err != nil {
		t.Fatalf("no database yet: %v", err)
	}
	if err := os.WriteFile(db, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := checkDBFiles(db); err != nil {
		t.Fatalf("a plain database: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), db+"-wal"); err != nil {
		t.Fatal(err)
	}
	if err := checkDBFiles(db); err == nil {
		t.Fatal("a symlinked journal was accepted")
	}
}

// mp setup writes the units the package ships, word for word: an install
// from the binary alone and one from the package behave the same.
func TestUnitsAreThePackages(t *testing.T) {
	for file, want := range map[string]string{
		"monopanel-agent.service":   unitText(agentUnit, "/usr/bin/monopanel"),
		"monopanel-api.service":     fmt.Sprintf(apiUnit, "/usr/bin/monopanel"),
		"monopanel-api.socket":      apiSocketUnit,
		"monopanel-apache@.service": apacheUnit,
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", file))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("packaging/systemd/%s differs from mp setup's:\n%s\n---\n%s", file, b, want)
		}
	}
	nfpm, err := os.ReadFile(filepath.Join("..", "..", "packaging", "nfpm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nfpm), "src: ./packaging/systemd/monopanel-api.socket\n    dst: /usr/lib/systemd/system/monopanel-api.socket\n") {
		t.Error("the package does not ship monopanel-api.socket")
	}
	if !strings.Contains(string(nfpm), "src: ./packaging/systemd/monopanel-apache@.service\n    dst: /usr/lib/systemd/system/monopanel-apache@.service\n") {
		t.Error("the package does not ship monopanel-apache@.service")
	}
	// An account's Apache stops gracefully: SIGWINCH to the parent alone,
	// which lets the requests in flight finish, bounded by the timeout.
	for _, line := range []string{"\nKillMode=mixed\n", "\nKillSignal=SIGWINCH\n", "\nTimeoutStopSec=60\n", "\nExecReload=/bin/kill -USR1 $MAINPID\n"} {
		if !strings.Contains(apacheUnit, line) {
			t.Errorf("the Apache unit lacks %q", line)
		}
	}
	// The API wants its socket and does not require it; the socket listens
	// where :8443 would, on every address and both families.
	for _, line := range []string{"\nWants=monopanel-api.socket\n", "\nAfter=monopanel-api.socket\n", "\nStartLimitIntervalSec=0\n"} {
		if !strings.Contains(apiUnit, line) {
			t.Errorf("the API unit lacks %q", line)
		}
	}
	if strings.Contains(apiUnit, "Requires=monopanel-api.socket") || strings.Contains(apiUnit, "BindsTo=") {
		t.Error("the API unit must not depend on its socket hard")
	}
	for _, line := range []string{"\nListenStream=8443\n", "\nAccept=no\n", "\nBindIPv6Only=both\n", "\nFreeBind=yes\n", "\nWantedBy=sockets.target\n"} {
		if !strings.Contains(apiSocketUnit, line) {
			t.Errorf("the socket unit lacks %q", line)
		}
	}
}

// The units go in when they are missing, or all of them with
// --install-units; a unit the administrator has is left alone. The
// socket's drop-in follows web.listen and is not written again unchanged.
// This is an install without the package: packaged holds none of its units.
func TestInstallUnitsWritesTheSocket(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "system")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	packaged := t.TempDir()
	mine := "[Service]\nExecStart=/opt/monopanel api\n"
	if err := os.WriteFile(filepath.Join(dir, apiService), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	reload := func(context.Context) error { reloads++; return nil }
	cfg := config.Default()
	cfg.Web.Listen = "203.0.113.5:9443"
	install := func(force bool) unitsResult {
		t.Helper()
		res, err := installUnits(ctx, dir, packaged, cfg, "/usr/bin/monopanel", force, reload)
		if err != nil || len(res.Removed)+len(res.Shadowing) > 0 || res.PruneErr != nil {
			t.Fatalf("without the package nothing is a copy of its units: %+v %v", res, err)
		}
		return res
	}

	res := install(false)
	if res.SocketErr != nil || !slices.Equal(res.Wrote, []string{agentService, apiSocket, apacheUnitName}) || !res.Moved || reloads != 2 {
		t.Fatalf("first: %+v, %d reloads", res, reloads)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, agentService)); string(b) != unitText(agentUnit, "/usr/bin/monopanel") {
		t.Fatalf("the agent's unit:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, apiSocket)); string(b) != apiSocketUnit {
		t.Fatalf("the socket unit:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, apiService)); string(b) != mine {
		t.Fatalf("the administrator's unit was replaced:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "monopanel-api.socket.d", "listen.conf")); !strings.HasSuffix(string(b), "\nListenStream=\nListenStream=203.0.113.5:9443\n") {
		t.Fatalf("the drop-in:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "monopanel-api.service.d", "socket.conf")); !strings.Contains(string(b), "\nWants=monopanel-api.socket\n") {
		t.Fatalf("the API's drop-in:\n%s", b)
	}

	if res := install(false); res.SocketErr != nil || len(res.Wrote) != 0 || res.Moved || reloads != 2 {
		t.Fatalf("again: %+v, %d reloads", res, reloads)
	}
	if res := install(true); res.SocketErr != nil || !slices.Equal(res.Wrote, []string{agentService, apiSocket, apiService, apacheUnitName}) {
		t.Fatalf("--install-units: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, apiService)); string(b) != fmt.Sprintf(apiUnit, "/usr/bin/monopanel") {
		t.Fatalf("--install-units kept the old unit:\n%s", b)
	}
	// A web.listen the socket cannot take is no failure of mp setup — the
	// API listens on it itself then — and leaves the drop-in as it was.
	cfg.Web.Listen = "8443"
	if res := install(false); res.Moved || res.SocketErr == nil || !strings.Contains(res.SocketErr.Error(), apiSocket) {
		t.Fatalf("a web.listen the socket cannot take: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "monopanel-api.socket.d", "listen.conf")); !strings.HasSuffix(string(b), "\nListenStream=203.0.113.5:9443\n") {
		t.Fatalf("the drop-in after a refused web.listen:\n%s", b)
	}
}

// packageUnits puts the package's units into a directory of their own, as
// it installs them in /usr/lib/systemd/system, and returns it.
func packageUnits(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, u := range panelUnits() {
		b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", u.name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, u.name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeFiles writes name → content into dir; a content starting with "->"
// makes a symlink to the rest.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// On a package install mp setup writes no unit into /etc/systemd/system:
// the package's apply, and a copy there would override them for good. The
// copies earlier releases of mp setup left there go — the units enabled
// through them stay enabled, now through the package's — and the socket
// still gets its drop-ins. A setup for a binary of its own (--binary) needs
// the units that run it; the socket and Apache's template are the package's
// all the same.
func TestInstallUnitsOnAPackageInstall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packaged := packageUnits(t)
	const bin = "/usr/bin/monopanel"
	writeFiles(t, dir, map[string]string{
		// mp setup of 0.8.12 and of 0.8.19, enabled.
		agentService: unitText(agentUnit060, bin),
		apiService:   unitText(apiUnit060, bin),
		"multi-user.target.wants/" + agentService: "->" + filepath.Join(dir, agentService),
		"multi-user.target.wants/" + apiService:   "->" + filepath.Join(dir, apiService),
	})
	reloads := 0
	reload := func(context.Context) error { reloads++; return nil }
	cfg := config.Default()
	cfg.Web.Listen = "203.0.113.5:9443"

	res, err := installUnits(ctx, dir, packaged, cfg, bin, false, reload)
	if err != nil || res.PruneErr != nil || res.SocketErr != nil || len(res.Wrote) != 0 || len(res.Shadowing) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if want := []string{filepath.Join(dir, agentService), filepath.Join(dir, apiService)}; !slices.Equal(res.Removed, want) {
		t.Fatalf("removed %v, want %v", res.Removed, want)
	}
	for _, u := range panelUnits() {
		if _, err := os.Lstat(filepath.Join(dir, u.name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s in the unit directory: %v", u.name, err)
		}
	}
	for _, u := range []string{agentService, apiService} {
		if target, _ := os.Readlink(filepath.Join(dir, "multi-user.target.wants", u)); target != filepath.Join(packaged, u) {
			t.Errorf("%s is enabled through %q", u, target)
		}
	}
	if reloads != 2 || !res.Moved {
		t.Errorf("%d reloads, moved %v", reloads, res.Moved)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "monopanel-api.service.d", "socket.conf")); !strings.Contains(string(b), "\nWants=monopanel-api.socket\n") {
		t.Fatalf("the API's drop-in:\n%s", b)
	}
	if res, err := installUnits(ctx, dir, packaged, cfg, bin, true, reload); err != nil || len(res.Wrote)+len(res.Removed) != 0 || reloads != 2 {
		t.Fatalf("--install-units on a package install: %+v %v, %d reloads", res, err, reloads)
	}

	res, err = installUnits(ctx, dir, packaged, cfg, "/opt/monopanel/bin/monopanel", false, reload)
	if err != nil || !slices.Equal(res.Wrote, []string{agentService, apiService}) || len(res.Removed)+len(res.Shadowing) != 0 {
		t.Fatalf("--binary of its own: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, apiService)); string(b) != fmt.Sprintf(apiUnit, "/opt/monopanel/bin/monopanel") {
		t.Fatalf("the API's unit for its own binary:\n%s", b)
	}
	// Back on the package's binary, that unit runs another binary: it stays,
	// and so does the warning that it overrides the package's.
	if res, err := installUnits(ctx, dir, packaged, cfg, bin, false, reload); err != nil || len(res.Removed) != 0 || !slices.Equal(res.Shadowing, []string{filepath.Join(dir, agentService), filepath.Join(dir, apiService)}) {
		t.Fatalf("units of another binary: %+v %v", res, err)
	}
}

// A copy goes only when it is provably the panel's own — a text a release
// wrote, for the package's binary — and no newer than the package's unit;
// enabling symlinks follow it to the package's unit. The administrator's
// own unit, a mask, a copy an alias leads to, one newer than the package's
// or one of a unit the package lacks all stay.
func TestPruneUnitCopies(t *testing.T) {
	const bin = "/usr/bin/monopanel"
	t.Run("the panel's own copies", func(t *testing.T) {
		dir, packaged := t.TempDir(), packageUnits(t)
		writeFiles(t, dir, map[string]string{
			agentService:   unitText(agentUnit0813, bin),
			apiService:     unitText(apiUnit, bin),
			apiSocket:      apiSocketUnit,
			apacheUnitName: apacheUnit,
			// An instance enabled through the template's copy, relative and
			// absolute, and a link to something else.
			"multi-user.target.wants/monopanel-apache@alex.service": "->../" + apacheUnitName,
			"multi-user.target.wants/monopanel-apache@kate.service": "->" + filepath.Join(dir, apacheUnitName),
			"sockets.target.wants/" + apiSocket:                     "->" + filepath.Join(dir, apiSocket),
			"multi-user.target.wants/nginx.service":                 "->/usr/lib/systemd/system/nginx.service",
		})
		res, err := pruneUnitCopies(dir, packaged, bin)
		if err != nil || len(res.Removed) != 4 || len(res.Shadowing) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		for link, want := range map[string]string{
			"multi-user.target.wants/monopanel-apache@alex.service": filepath.Join(packaged, apacheUnitName),
			"multi-user.target.wants/monopanel-apache@kate.service": filepath.Join(packaged, apacheUnitName),
			"sockets.target.wants/" + apiSocket:                     filepath.Join(packaged, apiSocket),
			"multi-user.target.wants/nginx.service":                 "/usr/lib/systemd/system/nginx.service",
		} {
			if got, _ := os.Readlink(filepath.Join(dir, link)); got != want {
				t.Errorf("%s -> %q, want %q", link, got, want)
			}
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "*", ".*")); len(left) != 0 {
			t.Errorf("left behind: %v", left)
		}
	})
	t.Run("what stays", func(t *testing.T) {
		dir, packaged := t.TempDir(), packageUnits(t)
		// The package of 0.8.19 had no socket nor template, and its API unit
		// knew no socket: a copy of 0.9.0's is newer than that.
		if err := os.Remove(filepath.Join(packaged, apacheUnitName)); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, packaged, map[string]string{apiService: unitText(apiUnit060, bin)})
		if err := os.Remove(filepath.Join(packaged, apiSocket)); err != nil {
			t.Fatal(err)
		}
		edited := strings.Replace(unitText(agentUnit0813, bin), "RestartSec=2\n", "RestartSec=2\nLimitNOFILE=65536\n", 1)
		writeFiles(t, dir, map[string]string{
			agentService:   edited,
			apiService:     unitText(apiUnit, bin),
			apiSocket:      "->/dev/null",
			apacheUnitName: apacheUnit,
		})
		res, err := pruneUnitCopies(dir, packaged, bin)
		if err != nil || len(res.Removed) != 0 || !slices.Equal(res.Shadowing, []string{filepath.Join(dir, agentService), filepath.Join(dir, apiService)}) {
			t.Fatalf("%+v %v", res, err)
		}
		for _, u := range []string{agentService, apiService, apiSocket, apacheUnitName} {
			if _, err := os.Lstat(filepath.Join(dir, u)); err != nil {
				t.Errorf("%s: %v", u, err)
			}
		}
	})
	t.Run("the agent's unit across its generations", func(t *testing.T) {
		// The package's agent unit of 0.8.13 to 0.8.19 (also mp setup's of
		// 0.8.20's development builds) is the panel's own and older than
		// today's, which empties the agent's /tmp before systemd removes it:
		// a copy of it goes, as the upgrade's postinstall must remove it for
		// the package's unit to stop the old agent.
		dir, packaged := t.TempDir(), packageUnits(t)
		writeFiles(t, dir, map[string]string{agentService: unitText(agentUnit0813pkg, bin)})
		if res, err := pruneUnitCopies(dir, packaged, bin); err != nil || !slices.Equal(res.Removed, []string{filepath.Join(dir, agentService)}) || len(res.Shadowing) != 0 {
			t.Fatalf("the earlier package's text: %+v %v", res, err)
		}
		// Today's text over the package of 0.8.19 is newer than it: it stays.
		writeFiles(t, packaged, map[string]string{agentService: unitText(agentUnit0813pkg, bin)})
		writeFiles(t, dir, map[string]string{agentService: unitText(agentUnit, bin)})
		if res, err := pruneUnitCopies(dir, packaged, bin); err != nil || len(res.Removed) != 0 || !slices.Equal(res.Shadowing, []string{filepath.Join(dir, agentService)}) {
			t.Fatalf("today's text over the package of 0.8.19: %+v %v", res, err)
		}
	})
	t.Run("an alias", func(t *testing.T) {
		dir, packaged := t.TempDir(), packageUnits(t)
		writeFiles(t, dir, map[string]string{
			agentService:               unitText(agentUnit, bin),
			"monopanel-agent2.service": "->" + agentService,
		})
		if res, err := pruneUnitCopies(dir, packaged, bin); err != nil || len(res.Removed) != 0 || !slices.Equal(res.Shadowing, []string{filepath.Join(dir, agentService)}) {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("copies of another binary", func(t *testing.T) {
		dir, packaged := t.TempDir(), packageUnits(t)
		writeFiles(t, dir, map[string]string{
			agentService: unitText(agentUnit, "/usr/local/bin/monopanel"),
			apiSocket:    apiSocketUnit,
		})
		// mp setup for that binary keeps the units that run it.
		res, err := pruneUnitCopies(dir, packaged, "/usr/local/bin/monopanel")
		if err != nil || !slices.Equal(res.Removed, []string{filepath.Join(dir, apiSocket)}) || len(res.Shadowing) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		if res, err := pruneUnitCopies(dir, packaged, bin); err != nil || len(res.Removed) != 0 || !slices.Equal(res.Shadowing, []string{filepath.Join(dir, agentService)}) {
			t.Fatalf("for the package's binary: %+v %v", res, err)
		}
	})
}

// The texts of the panel's units a release once wrote must stay known, or
// their copies would override the package's units for good: each is pinned
// here, the released ones as their tags have them. A unit that changes gets
// a generation of its own in unitHistory, its former text and pin staying,
// and today's text a new pin.
func TestUnitHistoryKeepsEveryRelease(t *testing.T) {
	for text, want := range map[string]string{
		// mp setup's and the package's agent of v0.6.0–v0.8.12, mp setup's
		// agent of v0.8.13–v0.8.19, the API of v0.6.0–v0.8.19.
		agentUnit060:  "b02f29ffd2207e85276ec0938c4ad91ebf9363c0f05c5b02cfdd49a5c4a57932",
		agentUnit0813: "fa18d69b25dc64fbad5f2a0375872a93fff33685d60415a12c45668f7ec19f1b",
		apiUnit060:    "4b0ed539f9bc58d840a714a41fae8e9d4836efbdfcdc9d4ff9955a82f246c9fb",
		// The package's agent of v0.8.13–v0.8.19.
		agentUnit0813pkg: "aeb2f5ff2d581607817621619827db69378dd86b687601ebf61c1585f96370ee",
		// Apache's template of 0.9.0's development builds before the
		// graceful stop.
		apacheUnit090dev: "97c3eb3afd46cf9d19510bb23855109a5b39c55acb30ef07e6c54679d5d34c6a",
		// Today's.
		agentUnit:     "788de936ccdcc05b32e410ca360796f942d3c2b1cd0ed3d7399c477a4635e37e",
		apiUnit:       "5c8adbb9b753b48608dad24d0b3b9ed56e43eaef0147c9502be75e63728902a7",
		apiSocketUnit: "23112e254d6b8d747d276d2ed14cdb1e9c359203f27af9f1a1df793e1e4b5002",
		apacheUnit:    "ccb74e2eb45271adfa66b1db8adc7e0f25238f0d49cbbc0478f952e0999f2c00",
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(text))); got != want {
			t.Errorf("a unit's text changed (sha256 %s, pinned %s): keep the former one in unitHistory and pin the new one\n%s", got, want, text)
		}
	}
	for _, u := range panelUnits() {
		gens := unitHistory()[u.name]
		if len(gens) == 0 || !slices.Contains(gens[len(gens)-1], u.text) {
			t.Errorf("%s: today's text is not in the last generation of unitHistory", u.name)
		}
		seen := map[string]int{}
		for i, g := range gens {
			for _, text := range g {
				if j, ok := seen[text]; ok {
					t.Errorf("%s: one text in generations %d and %d", u.name, j, i)
				}
				seen[text] = i
			}
		}
	}
}

// startPanel's systemd, recorded.
type recordedUnits struct {
	calls      []string
	failSocket bool
}

func (r *recordedUnits) act(action, unit string) error {
	r.calls = append(r.calls, action+" "+unit)
	if r.failSocket && unit == apiSocket {
		return errors.New("Address already in use")
	}
	return nil
}

func (r *recordedUnits) Start(_ context.Context, unit string) error   { return r.act("start", unit) }
func (r *recordedUnits) Stop(_ context.Context, unit string) error    { return r.act("stop", unit) }
func (r *recordedUnits) Restart(_ context.Context, unit string) error { return r.act("restart", unit) }

// A socket that takes the port anew does so with no API process around —
// the one of 0.8.19 and older holds the port itself — and before the API
// starts on it; otherwise the socket keeps the port while the services
// restart. One that cannot bind is a warning: the API listens itself.
func TestStartPanelOrder(t *testing.T) {
	ctx := context.Background()
	var warned []error
	warn := func(err error) { warned = append(warned, err) }
	r := &recordedUnits{}
	if err := startPanel(ctx, r, false, warn); err != nil || !slices.Equal(r.calls, []string{"restart " + agentService, "restart " + apiService}) {
		t.Fatalf("socket kept: %v %v", r.calls, err)
	}
	r = &recordedUnits{}
	if err := startPanel(ctx, r, true, warn); err != nil || !slices.Equal(r.calls, []string{"stop " + apiService, "restart " + apiSocket, "restart " + agentService, "start " + apiService}) || len(warned) != 0 {
		t.Fatalf("socket afresh: %v %v", r.calls, err)
	}
	r = &recordedUnits{failSocket: true}
	if err := startPanel(ctx, r, true, warn); err != nil || r.calls[len(r.calls)-1] != "start "+apiService || len(warned) != 1 {
		t.Fatalf("socket failing: %v %v %v", r.calls, err, warned)
	}
}

// enablePanel's systemd: the units' states, and what was enabled.
type enabledUnits struct {
	states  map[string]systemd.Status
	enabled []string
}

func (e *enabledUnits) Enable(_ context.Context, unit string) error {
	if agent.APISocketMasked(e.states[unit]) {
		// as systemd answers EnableUnitFiles for a masked unit
		return fmt.Errorf("enable %s: unit file /etc/systemd/system/%s is masked", unit, unit)
	}
	e.enabled = append(e.enabled, unit)
	return nil
}

func (e *enabledUnits) Status(_ context.Context, unit string) (systemd.Status, error) {
	if st, ok := e.states[unit]; ok {
		return st, nil
	}
	return systemd.Status{Unit: unit, LoadState: "loaded", UnitFileState: "enabled"}, nil
}

// mp setup enables the panel's units, a disabled socket of the panel's port
// among them (the API's unit starts it all the same), but leaves one the
// administrator masked as it is — systemd would refuse it, and the setup
// fail before the API's unit — so that the API listens itself, on purpose.
func TestEnablePanelLeavesAMaskedSocket(t *testing.T) {
	ctx := context.Background()
	all := []string{agentService, apiSocket, apiService}
	for _, c := range []struct {
		name   string
		socket systemd.Status
		masked bool
		want   []string
	}{
		{"enabled", systemd.Status{LoadState: "loaded", UnitFileState: "enabled"}, false, all},
		{"disabled", systemd.Status{LoadState: "loaded", UnitFileState: "disabled"}, false, all},
		{"masked", systemd.Status{LoadState: "masked", UnitFileState: "masked"}, true, []string{agentService, apiService}},
		{"masked at runtime", systemd.Status{LoadState: "masked", UnitFileState: "masked-runtime"}, true, []string{agentService, apiService}},
	} {
		e := &enabledUnits{states: map[string]systemd.Status{apiSocket: c.socket}}
		masked, err := enablePanel(ctx, e)
		if err != nil || masked != c.masked || !slices.Equal(e.enabled, c.want) {
			t.Errorf("%s: masked %v, enabled %v, %v", c.name, masked, e.enabled, err)
		}
	}
}

// The template of the accounts' Apaches starts apache2 on the configuration
// the agent writes and checks for that account (confcheck), with the
// runtime directory the configuration names; its parent keeps only the
// capabilities it needs, and no instance of it is enabled by mp setup.
func TestApacheUnitIsTheAgents(t *testing.T) {
	for _, line := range []string{
		"\nConditionPathExists=" + confcheck.ApacheInstanceConf("%i") + "\n",
		"\nExecStart=" + confcheck.ApacheBinary + " -f " + confcheck.ApacheInstanceConf("%i") + " -DFOREGROUND\n",
		"\nExecReload=/bin/kill -USR1 $MAINPID\n",
		"\nRuntimeDirectory=" + strings.TrimPrefix(confcheck.ApacheInstanceRunDir("%i"), "/run/") + "\n",
		"\nType=simple\n", "\nRestart=on-failure\n", "\nNoNewPrivileges=yes\n", "\nProtectSystem=full\n", "\nProtectHome=yes\n", "\nPrivateTmp=yes\n",
		"\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_KILL CAP_CHOWN CAP_DAC_READ_SEARCH\n",
		"\nWantedBy=multi-user.target\n",
	} {
		if !strings.Contains(apacheUnit, line) {
			t.Errorf("the Apache unit lacks %q", line)
		}
	}
	for _, bad := range []string{"User=", "Group=", "%I", "apache2ctl", "EnvironmentFile", "CAP_SYS_ADMIN", "CAP_DAC_OVERRIDE"} {
		if strings.Contains(apacheUnit, bad) {
			t.Errorf("the Apache unit has %q", bad)
		}
	}
	if login, ok := confcheck.ApacheInstanceLoginOf(apacheUnitName); ok {
		t.Errorf("the template itself reads as the Apache of %q", login)
	}
}
