package tui

import (
	"testing"

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
