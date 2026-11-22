package demo

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"monopanel/internal/agent"
	"monopanel/internal/api"
	"monopanel/internal/osprofile"
)

// pkgUpgrade answers "candidates" and "upgrade" (mp stack upgrade) the way
// the agent does: the candidate is the version the demo's repository has
// (candidate), an upgrade takes only the panel's packages that are
// installed. The caller holds mu.
func (a *Agent) pkgUpgrade(req *agent.PkgRequest) (*agent.PkgResponse, error) {
	if req.Action == "candidates" {
		out := &agent.PkgResponse{Installed: map[string]string{}, Upgrades: map[string]osprofile.Candidate{}}
		for _, p := range req.Packages {
			v, ok := a.st.Packages[p]
			if !ok {
				continue
			}
			out.Installed[p] = v
			if c, ok := candidate(p); ok && c != v {
				out.Upgrades[p] = osprofile.Candidate{Version: c}
			}
		}
		return out, nil
	}
	if len(req.Packages) == 0 {
		return nil, &agent.Error{Status: http.StatusBadRequest, Message: "no packages"}
	}
	for _, p := range req.Packages {
		if osprofile.UpgradeComponent(Profile(), p) == "" {
			return nil, &agent.Error{Status: http.StatusForbidden, Message: "upgrading " + p + " is not allowed: the panel upgrades its own packages only"}
		}
		if _, ok := a.st.Packages[p]; !ok {
			return nil, &agent.Error{Status: http.StatusForbidden, Message: "upgrading " + p + " is not allowed: not installed"}
		}
	}
	var b strings.Builder
	out := &agent.PkgResponse{Installed: map[string]string{}}
	for _, p := range req.Packages {
		if c, ok := candidate(p); ok && c != a.st.Packages[p] {
			a.st.Packages[p] = c
			fmt.Fprintf(&b, "Setting up %s (%s) ...\n", p, c)
		}
		out.Installed[p] = a.st.Packages[p]
	}
	out.Output = b.String()
	return out, nil
}

// kernelOf has the doctor's kernel modules check look at the pretend server:
// the package's modprobe rule and no module of it loaded, as on a server the
// panel was installed on, instead of the container's own kernel.
func kernelOf(srv *api.Server, dir string) error {
	rule, mods := filepath.Join(dir, "demo-modprobe.conf"), filepath.Join(dir, "demo-modules")
	if err := os.WriteFile(rule, []byte(osprofile.ModprobeConf), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(mods, []byte("nf_tables 376832 1 - Live 0x0000000000000000\next4 1060864 1 - Live 0x0000000000000000\n"), 0o644); err != nil {
		return err
	}
	srv.SetKernelSources([]string{rule}, mods, "")
	return nil
}
