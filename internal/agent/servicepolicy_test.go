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
		// root shells and power
		{"debug-shell.service", "start", false},
		{"debug-shell.service", "enable", false},
		{"emergency.service", "start", false},
		{"systemd-reboot.service", "start", false},
		{"systemd-confext.service", "restart", false},
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
