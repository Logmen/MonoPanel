package agent

import (
	"context"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// removeUnixUser deletes a client account created by the panel. System
// accounts (uid < 1000), root and the panel's own service user are refused.
func (s *Server) removeUnixUser(ctx context.Context, req *RemoveUnixUserRequest) (*RemoveUnixUserResponse, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" || req.Login == s.cfg.ServiceUser {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	resp := &RemoveUnixUserResponse{}
	home := filepath.Clean(req.Home)
	if u, err := user.Lookup(req.Login); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		if uid < 1000 {
			return nil, &Error{Status: http.StatusForbidden, Message: "refusing to remove a system account (uid " + u.Uid + ")"}
		}
		if req.Home == "" {
			home = filepath.Clean(u.HomeDir)
		}
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
		root := filepath.Clean(req.HomeUnder)
		if req.HomeUnder == "" || home == root || !strings.HasPrefix(home, root+"/") {
			return resp, &Error{Status: http.StatusForbidden, Message: "refusing to remove " + home + ": outside " + root}
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
