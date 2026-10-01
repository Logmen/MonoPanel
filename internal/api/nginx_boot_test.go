package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// At startup the panel writes what keeps nginx up across a reboot, and right
// after boot it does not take a missing IPv6 address for a removed one: the
// address from router advertisements arrives seconds after the services.
func TestNginxSurvivesBootWithLateIPv6(t *testing.T) {
	f := newSiteFixture(t)
	f.s.SetHostIPs(func() []string { return []string{"10.0.0.1"} })
	f.s.SetHostIPv6s(func() []string { return nil }) // not on the interface yet
	dir := f.s.defaultServersDir()
	f.agent.DirEntries = map[string][]string{dir: {"ip-10.0.0.1.conf", "ip-10.0.0.9.conf", "ip-2001:db8::10.conf", "README"}}

	uptime := filepath.Join(t.TempDir(), "uptime")
	prev := uptimePath
	uptimePath = uptime
	t.Cleanup(func() { uptimePath = prev })
	if err := os.WriteFile(uptime, []byte("12.50 20.00\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.s.refreshDefaultServers(f.ctx)
	sysctl, ok := f.agent.File(nginxSysctlPath)
	if !ok || !strings.Contains(sysctl, "net.ipv6.ip_nonlocal_bind = 1") {
		t.Errorf("sysctl: %v %q", ok, sysctl)
	}
	dropIn, ok := f.agent.File("/etc/systemd/system/nginx.service.d/monopanel.conf")
	if !ok || !strings.Contains(dropIn, "Restart=on-failure") || !strings.Contains(dropIn, "ExecStartPre=-/usr/bin/rm -f /run/nginx.pid") {
		t.Errorf("drop-in: %v %q", ok, dropIn)
	}
	if f.agent.UnitAction("systemd-sysctl.service") != "restart" {
		t.Error("systemd-sysctl not restarted after the sysctl file was written")
	}
	removed := strings.Join(f.agent.Removed(), " ")
	if !strings.Contains(removed, "ip-10.0.0.9.conf") {
		t.Errorf("the IPv4 address that is gone keeps its default server: %q", removed)
	}
	if strings.Contains(removed, "2001:db8::10") {
		t.Errorf("the default server of an IPv6 address was removed 12 seconds after boot: %q", removed)
	}

	// An hour later the same absence means the address is gone.
	if err := os.WriteFile(uptime, []byte("3600.00 7000.00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.agent.DirEntries = map[string][]string{dir: {"ip-10.0.0.1.conf", "ip-2001:db8::10.conf", "README"}}
	f.s.refreshDefaultServers(f.ctx)
	if removed := strings.Join(f.agent.Removed(), " "); !strings.Contains(removed, "2001:db8::10") {
		t.Errorf("a stale IPv6 default server stays an hour after boot: %q", removed)
	}
}
