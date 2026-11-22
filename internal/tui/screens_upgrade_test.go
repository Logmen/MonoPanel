package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/systemd"
)

// A stack row whose service (or one of whose Apaches) runs a replaced binary
// gets the row's upgrade, as on the web page: with nothing to install it is
// the restart after the check. A row mp stack upgrade does not restart gets
// none.
func TestUpgradeOfAStackRow(t *testing.T) {
	for _, c := range []struct {
		row      apitypes.StackComponent
		replaced bool
		want     string
	}{
		{apitypes.StackComponent{Name: "nginx", Kind: "web", Service: &systemd.Status{BinaryReplaced: true}}, true, "nginx"},
		{apitypes.StackComponent{Name: "apache", Kind: "web", Instances: &apitypes.StackInstances{Running: 2, Total: 2, Replaced: 1}}, true, "apache"},
		{apitypes.StackComponent{Name: "php-8.4", Kind: "php", Service: &systemd.Status{}}, false, "php"},
		{apitypes.StackComponent{Name: "percona", Kind: "db", Service: &systemd.Status{BinaryReplaced: true}}, true, "db"},
		{apitypes.StackComponent{Name: "fail2ban", Kind: "tool", Service: &systemd.Status{BinaryReplaced: true}}, true, ""},
		{apitypes.StackComponent{Name: "memcached", Kind: "tool", Upgrade: nil}, false, ""},
	} {
		if got := runsReplacedBinary(c.row); got != c.replaced {
			t.Errorf("%s: replaced %v, want %v", c.row.Name, got, c.replaced)
		}
		if got := upgradeComponentOf(c.row); got != c.want {
			t.Errorf("%s: upgrade %q, want %q", c.row.Name, got, c.want)
		}
	}
}

// A doctor's row that leads to the upgrade — an update waiting, a service
// on the binary an upgrade replaced — gets u, as the dashboard gets its
// button: mp stack upgrade of the row's component, after the question that
// says what it restarts. A row the upgrade does not settle gets none.
func TestDoctorRowLeadsToTheUpgrade(t *testing.T) {
	routes := apiRoutes()
	routes["/api/v1/system/doctor"] = map[string]any{"summary": "3 checks, 0 failed, 2 warnings", "checks": []map[string]any{
		{"name": "agent", "status": "ok", "detail": "version test"},
		{"name": "update nginx", "status": "warn", "detail": "nginx 1.30.5 → 1.30.7 from nginx.org: mp stack upgrade nginx", "action": "stack.upgrade", "target": "nginx"},
		{"name": "service fail2ban.service", "status": "warn", "detail": "running, but on the binary an upgrade replaced; systemctl restart fail2ban.service"},
	}}
	tree := testTree()
	stack := &cobra.Command{Use: "stack", Short: "stack group"}
	stack.AddCommand(&cobra.Command{Use: "upgrade [component]", Short: "upgrade the panel's packages", Args: cobra.MaximumNArgs(1), RunE: noop})
	tree.AddCommand(stack, &cobra.Command{Use: "doctor", Short: "diagnostics", RunE: noop})
	m, fr := newTestModelOf(t, tree, "admin", routes)
	drive(m, m.push(openDoctor(m)))
	s, ok := m.top().(*listScreen)
	if !ok || len(s.visible()) != 3 {
		t.Fatalf("the doctor: %T", m.top())
	}
	s.cursor = 2
	press(m, "u")
	if m.overlay != nil || len(fr.started()) != 0 {
		t.Fatalf("u on a row the upgrade does not settle: %T %+v", m.overlay, fr.started())
	}
	s.cursor = 1
	press(m, "u")
	if f, ok := m.top().(*form); !ok || f.path != "stack upgrade" || !strings.Contains(f.line(), "mp stack upgrade nginx") {
		t.Fatalf("u opens the upgrade of the row's component, as on the stack screen: %T", m.top())
	}
	press(m, "ctrl+s")
	ov, ok := m.overlay.(*confirmOverlay)
	if !ok || !strings.Contains(ov.spec.line, "mp stack upgrade nginx") || !strings.Contains(ov.spec.note, "nginx -t checks the configuration") {
		t.Fatalf("the upgrade asks first, saying what it restarts: %T", m.overlay)
	}
	press(m, "y")
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"stack", "upgrade", "nginx"}) {
		t.Fatalf("y upgrades the row's component: %+v", jobs)
	}
}
