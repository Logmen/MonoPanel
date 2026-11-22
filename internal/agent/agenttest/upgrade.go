package agenttest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/osprofile"
)

// Refusal is a check NewBinaryRefuses fails with Output once "upgrade"
// upgraded Package.
type Refusal struct{ Package, Output string }

// refusedCheck is the output of a check of a config/apply body that
// CheckFails refuses, or NewBinaryRefuses once its package was upgraded.
// The caller holds mu.
func (a *Agent) refusedCheck(body []byte) (string, bool) {
	var req agent.ApplyConfigSetRequest
	if json.Unmarshal(body, &req) != nil {
		return "", false
	}
	for _, v := range req.Validate {
		if len(v) == 0 {
			continue
		}
		if out, ok := a.CheckFails[v[0]]; ok {
			return out, true
		}
		if r, ok := a.NewBinaryRefuses[v[0]]; ok {
			for _, pkgs := range a.upgraded {
				if slices.Contains(pkgs, r.Package) {
					return r.Output, true
				}
			}
		}
	}
	return "", false
}

// SetActiveState has status report the unit in the state ("" forgets it):
// what a package's own restart left behind, set from a SetBefore hook in the
// middle of a job.
func (a *Agent) SetActiveState(unit, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ActiveState == nil {
		a.ActiveState = map[string]string{}
	}
	if state == "" {
		delete(a.ActiveState, unit)
		return
	}
	a.ActiveState[unit] = state
}

// upgradeProfiles are the families whose package sets the fake's "upgrade"
// takes: it does not know which one the panel under test believes it runs on.
var upgradeProfiles = func() []osprofile.Profile {
	var out []osprofile.Profile
	for _, r := range []string{"ID=debian\nVERSION_ID=13\n", "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.6\n"} {
		rel, _ := osprofile.ParseOSRelease(strings.NewReader(r))
		if p, err := osprofile.FromRelease(rel); err == nil {
			out = append(out, p)
		}
	}
	return out
}()

// versionOf is the installed version of a package. The caller holds mu.
func (a *Agent) versionOf(p string) string {
	if v, ok := a.PackageVersions[p]; ok {
		return v
	}
	return "1.0-test"
}

// candidates answers "candidates" as the agent does: what of the packages is
// installed, and the newer version Upgrades offers of each installed one.
// The caller holds mu.
func (a *Agent) candidates(pkgs []string) any {
	resp := agent.PkgResponse{Installed: map[string]string{}, Upgrades: map[string]osprofile.Candidate{}, Output: "ok"}
	for _, p := range pkgs {
		if a.MissingPackages[p] {
			continue
		}
		v := a.versionOf(p)
		resp.Installed[p] = v
		c, ok := a.Upgrades[p]
		switch {
		case !ok || c.Version == "" || c.Version == v:
		case a.HeldPackages[p]:
			if resp.Held == nil {
				resp.Held = map[string]osprofile.Candidate{}
			}
			resp.Held[p] = c
		default:
			resp.Upgrades[p] = c
		}
	}
	return resp
}

// upgrade installs what Upgrades offers. Like the agent it takes the panel's
// own packages only and installed ones only. The caller holds mu.
func (a *Agent) upgrade(pkgs []string) any {
	if len(pkgs) == 0 {
		return &agent.Error{Status: http.StatusBadRequest, Message: "no packages"}
	}
	for _, p := range pkgs {
		known := false
		for _, prof := range upgradeProfiles {
			if osprofile.UpgradeComponent(prof, p) != "" {
				known = true
			}
		}
		if !known {
			return &agent.Error{Status: http.StatusForbidden, Message: "upgrading " + p + " is not allowed: the panel upgrades its own packages only"}
		}
		if a.MissingPackages[p] {
			return &agent.Error{Status: http.StatusForbidden, Message: "upgrading " + p + " is not allowed: not installed"}
		}
	}
	var kept []string
	for _, p := range pkgs {
		if !a.HeldPackages[p] {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return &agent.Error{Status: http.StatusConflict, Message: "the administrator holds " + strings.Join(pkgs, ", ") + " (apt-mark hold): nothing to upgrade"}
	}
	pkgs = kept
	a.upgraded = append(a.upgraded, append([]string{}, pkgs...))
	var out strings.Builder
	resp := agent.PkgResponse{Installed: map[string]string{}}
	for _, p := range pkgs {
		if c, ok := a.Upgrades[p]; ok {
			if a.PackageVersions == nil {
				a.PackageVersions = map[string]string{}
			}
			a.PackageVersions[p] = c.Version
			delete(a.Upgrades, p)
			fmt.Fprintf(&out, "Setting up %s (%s) ...\n", p, c.Version)
		}
		resp.Installed[p] = a.versionOf(p)
	}
	resp.Output = out.String()
	return resp
}

// restarted forgets that a unit runs a replaced binary once a restart (or a
// stop) ended its process; a reload keeps the old binary. The caller holds mu.
func (a *Agent) restarted(unit, action string) {
	if !strings.Contains(unit, ".") {
		unit += ".service"
	}
	switch action {
	case "restart", "stop":
		delete(a.BinaryReplaced, unit)
	}
}

// Upgraded returns the package lists "upgrade" was called with, in order.
func (a *Agent) Upgraded() [][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]string{}, a.upgraded...)
}

// InstalledVersion is the version "query" reports for a package now.
func (a *Agent) InstalledVersion(p string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.versionOf(p)
}

// SetUpgrades replaces what the repositories offer; SetBinaryReplaced
// marks units that run a replaced binary.
func (a *Agent) SetUpgrades(up map[string]osprofile.Candidate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Upgrades = up
}

// SetBinaryReplaced marks the units as running a binary an upgrade replaced.
func (a *Agent) SetBinaryReplaced(units ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.BinaryReplaced == nil {
		a.BinaryReplaced = map[string]bool{}
	}
	for _, u := range units {
		a.BinaryReplaced[u] = true
	}
}

// RunsReplacedBinary tells whether the fake still has the unit on its old binary.
func (a *Agent) RunsReplacedBinary(unit string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.BinaryReplaced[unit]
}
