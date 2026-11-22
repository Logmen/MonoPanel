package agent

import (
	"strings"
	"testing"
)

func TestServicePolicy(t *testing.T) {
	for _, c := range []struct {
		unit, action string
		ok           bool
	}{
		// the panel's stack and any ordinary unit by name
		{"nginx.service", "reload-or-restart", true},
		{"php8.4-fpm.service", "restart", true},
		{"monopanel-app-alice-web.service", "start", true},
		{"monopanel-firewall.service", "enable", true},
		{"systemd-sysctl.service", "restart", true},
		{"redis-server.service", "stop", true},
		{"var-www-alice-data-shared.mount", "start", true},
		{"certbot.timer", "disable", true},
		{"", "daemon-reload", true},
		// state is always there to read
		{"debug-shell.service", "status", true},
		{"modprobe@dccp.service", "status", true},
		{"rescue.target", "status", true},
		// the panel's own runs, which a stop would break off halfway
		{"monopanel-pkg-1c6d5f656140e67b.service", "stop", false},
		{"monopanel-pkg-1c6d5f656140e67b.service", "restart", false},
		{"monopanel-pkg-1c6d5f656140e67b.service", "status", true},
		{"monopanel-update.service", "stop", false},
		{"monopanel-update.service", "start", false},
		{"monopanel-update.service", "status", true},
		{"monopanel-api.service", "restart", true},
		// root shells and power
		{"debug-shell.service", "start", false},
		{"debug-shell.service", "enable", false},
		{"emergency.service", "start", false},
		{"systemd-reboot.service", "start", false},
		{"systemd-confext.service", "restart", false},
		{"systemd-exit.service", "start", false},
		{"systemd-suspend.service", "start", false},
		{"systemd-hibernate.service", "start", false},
		{"systemd-hybrid-sleep.service", "start", false},
		{"systemd-suspend-then-hibernate.service", "start", false},
		{"systemd-suspend.service", "status", true},
		// systemd-tmpfiles over every file follows what others made below
		// the panel's run directory
		{"systemd-tmpfiles-setup.service", "restart", false},
		{"systemd-tmpfiles-setup.service", "start", false},
		{"systemd-tmpfiles-setup-dev.service", "start", false},
		{"systemd-tmpfiles-setup-dev-early.service", "restart", false},
		{"systemd-tmpfiles-clean.service", "start", false},
		{"systemd-tmpfiles-clean.timer", "restart", false},
		{"systemd-tmpfiles-setup.service", "status", true},
		// an account's Apache: the panel's own template, its instance a login
		{"monopanel-apache@alex.service", "start", true},
		{"monopanel-apache@alex.service", "reload-or-restart", true},
		{"monopanel-apache@shop-2.service", "enable", true},
		{"monopanel-apache@alex.service", "disable", true},
		{"monopanel-apache@alex.service", "stop", true},
		{"monopanel-apache@root.service", "start", false},
		{"monopanel-apache@.service", "start", false},
		{"monopanel-apache@Alex.service", "start", false},
		{"monopanel-apache@a\\x2fb.service", "start", false},
		{"monopanel-apache@alex.socket", "start", false},
		{"monopanel-apache@alex.timer", "start", false},
		{"apache2@alex.service", "start", false},
		{"monopanel-valkey@alex.service", "start", false},
		{"monopanel-apache@root.service", "status", true},
		// arguments of the caller's choosing
		{"modprobe@dccp.service", "start", false},
		{"systemd-nspawn@box.service", "start", false},
		{"user@0.service", "restart", false},
		{"getty@tty1.service", "enable", false},
		// targets, slices, devices
		{"rescue.target", "start", false},
		{"poweroff.target", "start", false},
		{"multi-user.target", "restart", false},
		{"user.slice", "stop", false},
		{"nginx", "restart", false}, // the RPC completes the name before
	} {
		err := serviceAllowed(c.unit, c.action)
		if (err == nil) != c.ok {
			t.Errorf("%s %s: %v, want allowed %v", c.action, c.unit, err, c.ok)
		}
	}
	if err := CheckService("nginx", "restart"); err != nil {
		t.Errorf("a name without its type is a service: %v", err)
	}
	if err := CheckService("monopanel-apache@alex", "restart"); err != nil {
		t.Errorf("the Apache of alex without its type: %v", err)
	}
	if err := CheckReloads([]string{"monopanel-apache@alex.service"}); err != nil {
		t.Errorf("config/apply reloads the Apache of alex: %v", err)
	}
	if err := CheckReloads([]string{"monopanel-apache@root.service"}); err == nil {
		t.Error("config/apply must not start an Apache of root")
	}
}

// Units config/apply and paths/remove reload or restart pass the same
// check, before anything is written.
func TestConfigApplyVetsItsUnits(t *testing.T) {
	s := policyServer(t, debian13)
	for _, req := range []*ApplyConfigSetRequest{
		{Reload: []string{"modprobe@dccp"}},
		{Restart: []string{"debug-shell.service"}},
		{Restart: []string{"rescue.target"}},
		{Reload: []string{"nginx; reboot"}},
	} {
		if _, err := s.applyConfigSet(t.Context(), req); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%+v: %v", req, err)
		}
	}
	if _, err := s.removePaths(t.Context(), &RemovePathsRequest{Paths: []string{"/nonexistent"}, Reload: []string{"debug-shell"}}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("paths/remove: %v", err)
	}
}
