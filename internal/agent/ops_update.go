package agent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"monopanel/internal/buildinfo"
	"monopanel/internal/systemd"
	"monopanel/internal/updater"
)

// updateUnit is the transient unit that installs a new panel version. It is
// started by systemd rather than forked here, so restarting the agent (which
// installing the package does) cannot kill the update halfway through.
const updateUnit = "monopanel-update.service"

var versionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+~-]{0,63}$`)

func (s *Server) installPanel(ctx context.Context, req *InstallPanelRequest) (*InstallPanelResponse, error) {
	if !versionRe.MatchString(req.Version) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid version"}
	}
	pkg := filepath.Clean(req.Package)
	if filepath.Dir(pkg) != s.cfg.DownloadsDir() || (!strings.HasSuffix(pkg, ".deb") && !strings.HasSuffix(pkg, ".rpm")) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "package must be a .deb or .rpm staged in " + s.cfg.DownloadsDir()}
	}
	if st, err := os.Lstat(pkg); err != nil || !st.Mode().IsRegular() {
		return nil, &Error{Status: http.StatusBadRequest, Message: "no such package: " + pkg}
	}
	sum, err := updater.FileSHA256(pkg)
	if err != nil {
		return nil, &Error{Message: "read package", Output: err.Error()}
	}
	if !strings.EqualFold(sum, req.SHA256) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "package digest does not match the request"}
	}

	signed := false
	if key := strings.TrimSpace(s.cfg.Update.PublicKey); key != "" {
		if req.Sums == "" || req.Sig == "" {
			return nil, &Error{Status: http.StatusBadRequest, Message: "release is unsigned but this host requires a signature"}
		}
		if err := updater.VerifySums(key, []byte(req.Sums), []byte(req.Sig)); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "release signature: " + err.Error()}
		}
		if want := updater.ParseSums([]byte(req.Sums))[filepath.Base(pkg)]; !strings.EqualFold(want, sum) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "package is not the one the signed checksums describe"}
		}
		signed = true
	} else {
		s.log.Warn("installing an unsigned panel package: no release key in config.yaml", "package", pkg)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, &Error{Message: "locate the panel binary", Output: err.Error()}
	}
	argv := []string{exe, "update-run", "--package", pkg, "--sha256", sum, "--version", req.Version, "--from", buildinfo.Version}
	if p := s.cfg.Path(); p != "" {
		argv = append(argv, "--config", p)
	}
	conn, err := systemd.Connect(ctx)
	if err != nil {
		return nil, &Error{Message: "systemd unreachable", Output: err.Error()}
	}
	defer conn.Close()
	if err := conn.RunDetached(ctx, updateUnit, "MonoPanel update to "+req.Version, argv); err != nil {
		return nil, &Error{Message: "start the update", Output: err.Error()}
	}
	s.log.Info("panel update started", "unit", updateUnit, "version", req.Version, "signed", signed)
	return &InstallPanelResponse{Unit: updateUnit, Started: true, Signed: signed}, nil
}
