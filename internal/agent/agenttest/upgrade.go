package agenttest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
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

// failedRestart is the output of a config/apply body whose reload or
// restart of a unit RestartFails names, which is down from then on; "" for
// none. The caller holds mu.
func (a *Agent) failedRestart(body []byte) string {
	var req agent.ApplyConfigSetRequest
	if len(a.RestartFails) == 0 || json.Unmarshal(body, &req) != nil {
		return ""
	}
	for _, u := range append(append([]string{}, req.Reload...), req.Restart...) {
		if !strings.Contains(u, ".") {
			u += ".service"
		}
		out, ok := a.RestartFails[u]
		if !ok {
			continue
		}
		a.units[u] = "restart"
		if strings.Contains(u, "@") {
			a.instance(u).active = false
		} else {
			if a.ActiveState == nil {
				a.ActiveState = map[string]string{}
			}
			a.ActiveState[u] = "failed"
		}
		delete(a.BinaryReplaced, u)
		return out
	}
	return ""
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

// aptPolicy is what apt-cache policy prints of the packages the repositories
// have (versions), the answer of "available" unless PkgOutput gives one:
// each one's candidate, offered by every source under /etc/apt/sources.list.d
// that an index update read (aptUpdated), in lines like
// "500 https://packages.sury.org/php trixie/main amd64 Packages". The API
// reads in them whether apt has a repository's index. The fake does not
// know which source carries what: every one offers every package. The
// caller holds mu.
func (a *Agent) aptPolicy(pkgs []string, versions map[string]string) string {
	var origins []string
	for _, src := range a.aptSources() {
		for _, o := range src.origins() {
			if a.aptRead[o] {
				origins = append(origins, "500 "+o+" amd64 Packages")
			}
		}
	}
	var b strings.Builder
	for _, p := range pkgs {
		v, ok := versions[p]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "%s:\n  Installed: (none)\n  Candidate: %s\n  Version table:\n     %s 500\n", p, v, v)
		for _, o := range origins {
			b.WriteString("        " + o + "\n")
		}
	}
	return b.String()
}

// aptUpdated is what an index update that printed out did to the lists of
// the sources: apt read every source it said nothing against (no "Err:",
// "W:" or "E:" line names it) and kept the lists it had of the others; an
// update that failed nowhere (clean, and no such line at all) drops the
// lists of the sources gone since, as apt cleans them up then. An update
// that stopped before it read anything — a list of sources apt could not
// read, the lock of the lists another apt-get update holds — changes
// nothing. The caller holds mu.
func (a *Agent) aptUpdated(out string, clean bool) {
	if strings.Contains(out, "The list of sources could not be read") || strings.Contains(out, "Could not get lock /var/lib/apt/lists/lock") {
		return
	}
	var said []string
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "Err:") || strings.HasPrefix(l, "W:") || strings.HasPrefix(l, "E:") {
			said = append(said, l)
		}
	}
	current := map[string]bool{}
	for _, src := range a.aptSources() {
		against := slices.ContainsFunc(said, func(l string) bool { return strings.Contains(l, src.uri) })
		for _, o := range src.origins() {
			current[o] = true
			if !against {
				a.aptRead[o] = true
			}
		}
	}
	if clean && len(said) == 0 {
		for o := range a.aptRead {
			if !current[o] {
				delete(a.aptRead, o)
			}
		}
	}
}

// aptConflict is what apt says of two sources of one repository (URI and
// suite) whose Signed-By differ, as apt 2 has it: the first of them that
// names a keyring sets it, and a later one that names another, or none,
// conflicts. apt reads no source at all then, and apt-get update and
// apt-cache policy fail alike. "" when apt takes the sources. The caller
// holds mu.
func (a *Agent) aptConflict() string {
	signedBy := map[string]string{}
	for _, src := range a.aptSources() {
		for _, suite := range src.suites {
			repo := src.uri + "/ " + suite
			prev, seen := signedBy[repo]
			switch {
			case !seen || prev == "":
				signedBy[repo] = src.signedBy
			case prev != src.signedBy:
				return fmt.Sprintf("E: Conflicting values set for option Signed-By regarding source %s: %s != %s\nE: The list of sources could not be read.", repo, prev, src.signedBy)
			}
		}
	}
	return ""
}

// pkgFailed is the agent's message for a run of the package manager that
// failed: how it ended and its unit (internal/agent pkg); the package
// manager's own text is the error's output.
func pkgFailed(action string) string {
	return "apt " + action + " failed: exit status 100 (monopanel-pkg-test.service)"
}

// aptSource is a source of apt's: a line of a .list file, or a stanza of a
// .sources file, of type deb.
type aptSource struct {
	uri        string // without its trailing slash, as apt prints it
	suites     []string
	components []string
	signedBy   string // "" when it names no keyring
}

// origins are the lines apt-cache policy prints for the source's indexes:
// "<uri> <suite>/<component>".
func (s aptSource) origins() []string {
	var out []string
	for _, suite := range s.suites {
		for _, c := range s.components {
			out = append(out, s.uri+" "+suite+"/"+c)
		}
	}
	return out
}

// aptSources are the sources of the files config/apply wrote under
// /etc/apt/sources.list.d, in the order apt reads them: by file name, .list
// and .sources alike. A line without a suite and a component is left out,
// as apt refuses it. The caller holds mu.
func (a *Agent) aptSources() []aptSource {
	var files []string
	for p := range a.files {
		if path.Dir(p) == "/etc/apt/sources.list.d" && (strings.HasSuffix(p, ".list") || strings.HasSuffix(p, ".sources")) {
			files = append(files, p)
		}
	}
	slices.Sort(files)
	var out []aptSource
	for _, p := range files {
		if strings.HasSuffix(p, ".sources") {
			out = append(out, deb822Sources(a.files[p].Content)...)
			continue
		}
		for _, line := range strings.Split(a.files[p].Content, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 || f[0] != "deb" {
				continue
			}
			f = f[1:]
			src := aptSource{}
			if len(f) > 0 && strings.HasPrefix(f[0], "[") {
				// The options, up to the word that closes them.
				for ; len(f) > 0; f = f[1:] {
					opt := strings.Trim(f[0], "[]")
					if v, ok := strings.CutPrefix(opt, "signed-by="); ok {
						src.signedBy = v
					}
					if strings.HasSuffix(f[0], "]") {
						f = f[1:]
						break
					}
				}
			}
			if len(f) < 3 {
				continue
			}
			src.uri, src.suites, src.components = strings.TrimSuffix(f[0], "/"), f[1:2], f[2:]
			out = append(out, src)
		}
	}
	return out
}

// deb822Sources are the deb sources of a .sources file: stanzas of fields
// apart by empty lines, a value going on in the lines that start with a
// space (an inline key in Signed-By).
func deb822Sources(content string) []aptSource {
	var out []aptSource
	for _, stanza := range strings.Split(content, "\n\n") {
		fields := map[string]string{}
		key := ""
		for _, line := range strings.Split(stanza, "\n") {
			switch {
			case strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#"):
			case line[0] == ' ' || line[0] == '\t':
				if key != "" {
					fields[key] += "\n   " + strings.TrimSpace(line)
				}
			default:
				k, v, _ := strings.Cut(line, ":")
				key = strings.ToLower(strings.TrimSpace(k))
				fields[key] = strings.TrimSpace(v)
			}
		}
		if !slices.Contains(strings.Fields(fields["types"]), "deb") || strings.EqualFold(fields["enabled"], "no") {
			continue
		}
		suites, components := strings.Fields(fields["suites"]), strings.Fields(fields["components"])
		if len(suites) == 0 || len(components) == 0 {
			continue
		}
		for _, uri := range strings.Fields(fields["uris"]) {
			out = append(out, aptSource{uri: strings.TrimSuffix(uri, "/"), suites: suites, components: components, signedBy: strings.TrimPrefix(fields["signed-by"], "\n   ")})
		}
	}
	return out
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
