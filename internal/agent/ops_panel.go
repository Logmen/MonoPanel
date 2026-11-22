package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"monopanel/internal/config"
)

// configPath is the configuration file this agent was started with.
func (s *Server) configPath() string {
	p := s.cfg.Path()
	if p == "" {
		p = config.DefaultPath
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// samePath compares two paths, symlinks resolved where they exist.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// setPanelConfig changes the panel's own settings in config.yaml for the API
// process, which cannot write /etc (ProtectSystem=strict) and must not: the
// same file holds the release key. Only the keys of config.PanelKeys pass, the
// rest of the file stays byte for byte (config.EditPanelKeys), the file keeps
// its mode and owner (root:monopanel 0640, so that the API can read it) and is
// replaced atomically; the previous version goes to the configuration history.
// A new web.listen also goes into the drop-in of the socket that holds the
// panel's port (apisocket.go), from the checked value and nothing else. The
// change takes effect when monopanel-api restarts, which is the caller's
// call.
func (s *Server) setPanelConfig(ctx context.Context, req *PanelConfigRequest) (*PanelConfigResponse, error) {
	path := s.configPath()
	if req.Path != "" && !samePath(req.Path, path) {
		return nil, &Error{Status: http.StatusConflict, Message: fmt.Sprintf("the agent runs on %s and the API on %s: start both with the same configuration", path, req.Path)}
	}
	if len(req.Set) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "nothing to change"}
	}
	keys := make([]string, 0, len(req.Set))
	for k := range req.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !config.IsPanelKey(k) {
			return nil, &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("%s cannot be changed through the agent; only %s can", k, strings.Join(config.PanelKeys(), ", "))}
		}
	}

	s.panelMu.Lock()
	defer s.panelMu.Unlock()
	// The file's directory is walked to like every root write: /etc/monopanel
	// is root's, and a symlink nobody but root made is not followed. Where
	// the file lives is the administrator's choice (--config), not the API's,
	// so root's symlinks are followed all the way.
	dir, err := walkDir("/", filepath.Dir(path), walkOpts{trust: s.trust(), follow: followAll, uid: -1, gid: -1})
	if err != nil {
		return nil, walkErr("open the directory of "+path, err)
	}
	defer dir.close()
	name := filepath.Base(path)
	mode, uid, gid := uint32(0o640), -1, -1
	old, st, existed, err := readFileAt(dir.fd, name)
	switch {
	case errors.Is(err, errNotPlainFile):
		return nil, &Error{Status: http.StatusConflict, Message: path + " is not a regular file (a symlink?): the agent does not replace it; change it by hand"}
	case err != nil:
		return nil, &Error{Message: "read " + path, Output: err.Error()}
	case existed:
		mode, uid, gid = st.Mode&0o7777, int(st.Uid), int(st.Gid)
	default:
		// mp setup makes the file; without it the panel runs on the defaults.
		// A new one gets what setup gives it, root:<service group> 0640 —
		// root-only, the API could not read its own configuration and would
		// not start again.
		g, gerr := lookupGID(s.cfg.ServiceGroup)
		if gerr != nil {
			return nil, &Error{Status: http.StatusConflict, Message: path + " does not exist and the group " + s.cfg.ServiceGroup + " is missing: run mp setup"}
		}
		gid = g
	}

	out, changed, err := config.EditPanelKeys(old, req.Set)
	if err != nil {
		return nil, &Error{Status: http.StatusUnprocessableEntity, Message: err.Error()}
	}
	resp := &PanelConfigResponse{Path: path, Changed: changed}
	if resp.Changed == nil {
		resp.Changed = []string{}
	}
	if len(changed) == 0 {
		return resp, nil
	}
	// A new web.listen moves the socket that holds the panel's port. Where
	// to is worked out before anything is written: a web.listen the socket
	// cannot take (a name this host cannot resolve) changes nothing.
	var socket *APISocketAddr
	if slices.Contains(changed, config.KeyListen) {
		listen, _ := config.NormalizePanelValue(config.KeyListen, req.Set[config.KeyListen])
		addr, err := ResolveAPISocket(ctx, listen, s.lookupIP)
		if err != nil {
			return nil, &Error{Status: http.StatusUnprocessableEntity, Message: err.Error()}
		}
		socket = &addr
	}
	if err := writeFileAt(dir.fd, dir.resolved, name, out, mode, uid, gid); err != nil {
		return nil, &Error{Message: "write " + path, Output: err.Error()}
	}
	if existed {
		if err := s.history().store(path, old); err != nil {
			s.log.Warn("configuration history", "path", path, "err", err)
		}
	}
	s.restorecon(ctx, path)
	s.log.Info("panel configuration changed", "file", path, "keys", changed)
	if socket != nil {
		// The socket takes the new address when the API restarts next
		// (stopped first, it comes back with the API). Should its drop-in
		// fail, config.yaml stands all the same: the API listens on the new
		// address itself then, only without PID 1 keeping the port while it
		// restarts.
		if _, err := s.apiSocket().write(ctx, s.cfg, *socket); err != nil {
			s.log.Warn("the panel's socket did not follow web.listen; monopanel-api will listen on it itself", "socket", s.cfg.APISocketUnit(), "address", socket.ListenStream(), "err", err)
			resp.SocketError = err.Error()
		} else {
			s.log.Info("the panel's socket follows web.listen", "socket", s.cfg.APISocketUnit(), "address", socket.ListenStream())
		}
	}
	return resp, nil
}
