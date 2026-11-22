package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// removeUnixUser deletes a client account created by the panel
// (internal/api/ops_users_delete.go). Only the panel's accounts — never
// root, a system account, the panel's service user or one in a privileged
// group — and the home only as a directory of the web root.
func (s *Server) removeUnixUser(ctx context.Context, req *RemoveUnixUserRequest) (*RemoveUnixUserResponse, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" || req.Login == s.cfg.ServiceUser {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	root := filepath.Clean(s.cfg.WWWRoot)
	if req.RemoveHome && (req.HomeUnder == "" || filepath.Clean(req.HomeUnder) != root) {
		return nil, notAllowed("removing a home under "+req.HomeUnder, "homes are directories of "+root)
	}
	resp := &RemoveUnixUserResponse{}
	home := filepath.Clean(req.Home)
	u, err := s.panelAccount(req.Login)
	switch {
	case errors.Is(err, errNoAccount):
		// already gone: only its home may be left, <www_root>/<login>
		if req.RemoveHome && req.Home != "" && home != filepath.Join(root, req.Login) {
			return nil, notAllowed("removing "+req.Home+" for "+req.Login, "the home of an account that is gone is "+filepath.Join(root, req.Login))
		}
	case err != nil:
		return nil, err
	default:
		if req.Home == "" {
			home = filepath.Clean(u.HomeDir)
		} else if req.RemoveHome && home != filepath.Clean(u.HomeDir) {
			return nil, notAllowed("removing "+req.Home+" for "+req.Login, "it is not the account's home ("+u.HomeDir+")")
		}
	}
	if req.RemoveHome && home != "" && home != "." {
		// os.RemoveAll would go into a shared folder mounted in the home and
		// take the site's files along: the folders go first, unit and mount
		// alike (sharemount.go).
		g, release, err := s.holdShareGuard()
		if err != nil {
			return nil, err
		}
		defer release()
		if m, ok := g.holding(home); ok {
			return nil, g.refuse("removing "+home, m)
		}
		if m, ok := mountedBelow(home); ok {
			return nil, &Error{Status: http.StatusConflict, Message: "removing " + home + " is not allowed: " + m + " is mounted there; remove the shared folders of " + req.Login + " first (or unmount it)"}
		}
	}
	if u != nil {
		uid, _ := strconv.Atoi(u.Uid)
		resp.Killed = killProcessesOf(uid)
		// userdel without -r: the home is removed below, because userdel
		// refuses directories not owned by the user (SFTP chroot homes are
		// root-owned) and exits non-zero for a missing mail spool too.
		if out, err := runArgv(ctx, nil, "userdel", req.Login); err != nil {
			if _, again := user.Lookup(req.Login); again == nil {
				return nil, &Error{Message: "userdel failed", Output: out}
			}
		}
		runArgv(ctx, nil, "groupdel", req.Login) //nolint:errcheck // usually gone with the user
		resp.Removed = true
	}
	if req.RemoveHome && home != "" && home != "." {
		// A directory of the web root itself: its entry is in a root-owned
		// directory, so no account can swap it for a symlink meanwhile, and
		// os.RemoveAll follows none below it.
		if !filepath.IsAbs(home) || filepath.Dir(home) != root {
			return resp, &Error{Status: http.StatusForbidden, Message: "refusing to remove " + home + ": not a directory of " + root}
		}
		if st, err := os.Lstat(home); err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return resp, &Error{Status: http.StatusForbidden, Message: home + " is a symlink"}
			}
			if err := os.RemoveAll(home); err != nil {
				return resp, &Error{Message: "removing " + home, Output: err.Error()}
			}
			resp.HomeRemoved = true
		}
	}
	return resp, nil
}

// killProcessesOf sends SIGTERM to every process of uid, waits briefly and
// SIGKILLs what is left. Returns the number of processes signalled.
func killProcessesOf(uid int) int {
	pids := processesOf(uid)
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM) //nolint:errcheck
	}
	if len(pids) == 0 {
		return 0
	}
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if len(processesOf(uid)) == 0 {
			return len(pids)
		}
	}
	for _, pid := range processesOf(uid) {
		syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
	}
	time.Sleep(200 * time.Millisecond)
	return len(pids)
}

func processesOf(uid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				f := strings.Fields(line)
				if len(f) > 1 && f[1] == strconv.Itoa(uid) {
					out = append(out, pid)
				}
				break
			}
		}
	}
	return out
}
