package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"monopanel/internal/agent/confcheck"
	"monopanel/internal/config"
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
func TestInstallUnitsWritesTheSocket(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "system")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "[Service]\nExecStart=/opt/monopanel api\n"
	if err := os.WriteFile(filepath.Join(dir, apiService), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	reload := func(context.Context) error { reloads++; return nil }
	cfg := config.Default()
	cfg.Web.Listen = "203.0.113.5:9443"

	wrote, moved, socketErr, err := installUnits(ctx, dir, cfg, "/usr/bin/monopanel", false, reload)
	if err != nil || socketErr != nil || !slices.Equal(wrote, []string{agentService, apiSocket, apacheUnitName}) || !moved || reloads != 2 {
		t.Fatalf("first: %v %v %v %v, %d reloads", wrote, moved, socketErr, err, reloads)
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

	wrote, moved, socketErr, err = installUnits(ctx, dir, cfg, "/usr/bin/monopanel", false, reload)
	if err != nil || socketErr != nil || len(wrote) != 0 || moved || reloads != 2 {
		t.Fatalf("again: %v %v %v %v, %d reloads", wrote, moved, socketErr, err, reloads)
	}
	wrote, _, socketErr, err = installUnits(ctx, dir, cfg, "/usr/bin/monopanel", true, reload)
	if err != nil || socketErr != nil || !slices.Equal(wrote, []string{agentService, apiSocket, apiService, apacheUnitName}) {
		t.Fatalf("--install-units: %v %v %v", wrote, socketErr, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, apiService)); string(b) != fmt.Sprintf(apiUnit, "/usr/bin/monopanel") {
		t.Fatalf("--install-units kept the old unit:\n%s", b)
	}
	// A web.listen the socket cannot take is no failure of mp setup — the
	// API listens on it itself then — and leaves the drop-in as it was.
	cfg.Web.Listen = "8443"
	if _, moved, socketErr, err := installUnits(ctx, dir, cfg, "/usr/bin/monopanel", false, reload); err != nil || moved || socketErr == nil || !strings.Contains(socketErr.Error(), apiSocket) {
		t.Fatalf("a web.listen the socket cannot take: moved=%v %v %v", moved, socketErr, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "monopanel-api.socket.d", "listen.conf")); !strings.HasSuffix(string(b), "\nListenStream=203.0.113.5:9443\n") {
		t.Fatalf("the drop-in after a refused web.listen:\n%s", b)
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

// The package's scripts as text: the socket is enabled with the services
// and follows web.listen, never failing the install. On an upgrade a socket
// that listens keeps the port while the services restart; one that does not
// (a panel of 0.8.19 or older listens on the port itself) or whose address
// moved restarts in the same systemctl call as a running API — systemd
// orders it between the old API's exit and the new one's start — and never
// after a stop of the API of its own, which would leave the port free for as
// long as two systemctl calls take. Removal takes the socket too.
func TestPackageScriptsOrderTheSocket(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "scripts", "postinstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	post := string(b)
	at := func(s string) int {
		t.Helper()
		i := strings.Index(post, s)
		if i < 0 {
			t.Fatalf("postinstall.sh lacks %q", s)
		}
		return i
	}
	enable := at("systemctl enable monopanel-agent.service monopanel-api.socket monopanel-api.service")
	before := at(`before=$(cat "$dropin" 2>/dev/null || true)`)
	sync := at("/usr/bin/monopanel api-socket-sync ||")
	moved := at(`[ "$(cat "$dropin" 2>/dev/null || true)" = "$before" ] || moved=1`)
	upgrade := at(`if [ "$upgrade" = "1" ]; then`)
	keep := at(`if [ "$moved" = "0" ] && systemctl -q is-active monopanel-api.socket; then`)
	tryRestart := at("systemctl try-restart monopanel-agent.service monopanel-api.service")
	running := at("if systemctl -q is-active monopanel-api.service; then")
	together := at("systemctl restart monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true")
	alone := at("systemctl restart monopanel-api.socket >/dev/null 2>&1 || true")
	agent := at("systemctl try-restart monopanel-agent.service >/dev/null 2>&1 || true")
	if !(enable < before && before < sync && sync < moved && moved < upgrade && upgrade < keep && keep < tryRestart && tryRestart < running && running < together && together < alone && alone < agent) {
		t.Errorf("postinstall.sh: enable %d, drop-in before %d, sync %d, moved %d, upgrade %d, keep %d, try-restart %d, API running %d, socket with the API %d, socket alone %d, agent %d",
			enable, before, sync, moved, upgrade, keep, tryRestart, running, together, alone, agent)
	}
	var acts []string
	for _, line := range strings.Split(post, "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "systemctl") && strings.Contains(line, "monopanel-api.socket") {
			acts = append(acts, line)
		}
	}
	if want := []string{
		"systemctl enable monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true",
		`if [ "$moved" = "0" ] && systemctl -q is-active monopanel-api.socket; then`,
		"systemctl restart monopanel-agent.service monopanel-api.socket monopanel-api.service >/dev/null 2>&1 || true",
		"systemctl restart monopanel-api.socket >/dev/null 2>&1 || true",
	}; !slices.Equal(acts, want) {
		t.Errorf("postinstall.sh acts on the socket outside the branch that restarts it with the API:\n%s", strings.Join(acts, "\n"))
	}
	for _, gap := range []string{"systemctl stop monopanel-api.service", "systemctl start monopanel-api.socket", "systemctl stop monopanel-api.socket"} {
		if strings.Contains(post, gap) {
			t.Errorf("postinstall.sh runs %q: the port is free between that and the next call", gap)
		}
	}
	b, err = os.ReadFile(filepath.Join("..", "..", "packaging", "scripts", "preremove.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "systemctl disable --now monopanel-api.socket monopanel-api.service monopanel-agent.service") {
		t.Error("preremove.sh leaves the socket listening")
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
