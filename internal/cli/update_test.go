package cli

import (
	"slices"
	"strings"
	"testing"

	"monopanel/internal/config"
	"monopanel/internal/osprofile"
)

// update-run installs with the host's package manager as the agent runs it:
// on Debian and Ubuntu with apt's environment, NEEDRESTART_MODE=l among it,
// so that needrestart lists the services on old libraries instead of
// restarting them all; the reinstall of a version the package database
// holds already comes from the same manager.
func TestUpdateRunInstallsWithThePackageManager(t *testing.T) {
	ubuntu, err := osprofile.FromRelease(osprofile.Release{ID: "ubuntu", IDLike: []string{"debian"}, VersionID: "24.04"})
	if err != nil {
		t.Fatal(err)
	}
	const pkg = "/var/lib/monopanel-agent/updates/monopanel_0.8.21_amd64.deb"
	inst := updateInstaller(config.Default(), ubuntu, pkg, strings.Repeat("a", 64), "0.8.21", "0.8.20")
	if inst.Packages != ubuntu.Packages() {
		t.Fatalf("the installer's package manager: %#v", inst.Packages)
	}
	if env := inst.Packages.Env(); !slices.Contains(env, "NEEDRESTART_MODE=l") || !slices.Contains(env, "DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("apt runs with %v", env)
	}
	if argv := strings.Join(inst.Packages.ReinstallArgv([]string{inst.Package}), " "); !strings.Contains(argv, "--reinstall install "+pkg) {
		t.Fatalf("the reinstall: %s", argv)
	}
	if inst.To != "0.8.21" || inst.From != "0.8.20" || inst.BackupDir != config.Default().AgentUpdatesDir() || !slices.Equal(inst.Units, []string{"monopanel-agent.service", "monopanel-api.service"}) {
		t.Fatalf("installer: %+v", inst)
	}
}

// After an install that rolled back, mp update says what a retry does: the
// package database lists the version, mp update apply installs it again,
// and the panel does not on its own.
func TestUpdateSaysWhatARetryDoes(t *testing.T) {
	status := func(reinstall bool) map[string]any {
		return map[string]any{"current": "0.8.20", "latest": "0.8.21", "available": true,
			"settings":     map[string]any{"repo": "acme/panel", "channel": "stable", "check_hours": 24, "auto_apply": true},
			"last_attempt": map[string]any{"status": "rolled-back", "from": "0.8.20", "to": "0.8.21", "error": "panel did not come up after the update", "reinstall": reinstall}}
	}
	newFakeAPI(t, map[string]fakeResp{"GET /system/update": {body: status(true)}})
	out, err := mp(t, "", "update")
	if err != nil || !strings.Contains(out, "Last install: 0.8.20 → 0.8.21, rolled-back: panel did not come up after the update\n") ||
		!strings.Contains(out, "the package database lists 0.8.21 already while the previous binary runs: mp update apply installs it once more, the panel does not retry it on its own\n") {
		t.Fatalf("%v\n%s", err, out)
	}
	newFakeAPI(t, map[string]fakeResp{"GET /system/update": {body: status(false)}})
	if out, err := mp(t, "", "update"); err != nil || strings.Contains(out, "once more") {
		t.Fatalf("without a reinstall to do: %v\n%s", err, out)
	}
}
