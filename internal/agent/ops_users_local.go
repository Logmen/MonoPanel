package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"

	"monopanel/internal/osprofile"
)

// EnsureGroup creates a group when missing. Exported so `mp setup` (which
// runs as root before the agent exists) can reuse the exact same logic.
func EnsureGroup(ctx context.Context, req *EnsureGroupRequest) (*EnsureGroupResponse, error) {
	if !nameRe.MatchString(req.Name) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group name"}
	}
	if gid, err := lookupGID(req.Name); err == nil {
		return &EnsureGroupResponse{GID: gid}, nil
	}
	argv := []string{"groupadd"}
	if req.System {
		argv = append(argv, "-r")
	}
	argv = append(argv, req.Name)
	if out, err := runArgv(ctx, nil, argv...); err != nil {
		return nil, &Error{Message: "groupadd failed", Output: out}
	}
	gid, err := lookupGID(req.Name)
	if err != nil {
		return nil, err
	}
	return &EnsureGroupResponse{GID: gid, Created: true}, nil
}

// EnsureUnixUser creates a unix user when missing and adds supplementary groups.
func EnsureUnixUser(ctx context.Context, profile osprofile.Profile, req *EnsureUnixUserRequest) (*EnsureUnixUserResponse, error) {
	if !nameRe.MatchString(req.Login) {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	for _, g := range req.Groups {
		if !nameRe.MatchString(g) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group name: " + g}
		}
	}
	// Whoever calls, no account joins a group that is root by another name
	// (the agent's RPC checks the full shape first, see checkEnsureUser).
	for _, g := range append(append([]string{}, req.Groups...), req.PrimaryGroup) {
		if privilegedGroups[g] {
			return nil, &Error{Status: http.StatusForbidden, Message: "the group " + g + " is not allowed: it is a privileged group"}
		}
	}
	created := false
	u, err := user.Lookup(req.Login)
	if err == nil && !req.System {
		// A panel account must never take over one of the system's own
		// (root, www-data, mysql…): the calls below would change its shell,
		// home and groups.
		if uid, cerr := strconv.Atoi(u.Uid); cerr == nil && uid < uidMin() {
			return nil, &Error{Status: http.StatusForbidden, Message: fmt.Sprintf("refusing to take over the system account %s (uid %d)", req.Login, uid)}
		}
	}
	if err != nil {
		shell := req.Shell
		if shell == "" {
			shell = profile.NologinShell()
		}
		argv := []string{"useradd"}
		if req.System {
			argv = append(argv, "-r")
		}
		if req.Home != "" {
			argv = append(argv, "-d", req.Home)
		}
		if req.CreateHome {
			argv = append(argv, "-m")
		} else {
			argv = append(argv, "-M")
		}
		if req.Comment != "" {
			argv = append(argv, "-c", req.Comment)
		}
		if req.PrimaryGroup != "" {
			if !nameRe.MatchString(req.PrimaryGroup) {
				return nil, &Error{Status: http.StatusBadRequest, Message: "invalid primary group"}
			}
			argv = append(argv, "-g", req.PrimaryGroup)
		} else {
			argv = append(argv, "-U")
		}
		argv = append(argv, "-s", shell, req.Login)
		if out, err := runArgv(ctx, nil, argv...); err != nil {
			return nil, &Error{Message: "useradd failed", Output: out}
		}
		created = true
		if u, err = user.Lookup(req.Login); err != nil {
			return nil, err
		}
	}
	if !created && req.UpdateShell && req.Shell != "" {
		if out, err := runArgv(ctx, nil, "usermod", "-s", req.Shell, req.Login); err != nil {
			return nil, &Error{Message: "usermod -s failed", Output: out}
		}
	}
	for _, g := range req.Groups {
		if out, err := runArgv(ctx, nil, "usermod", "-aG", g, req.Login); err != nil {
			return nil, &Error{Message: "usermod failed for group " + g, Output: out}
		}
	}
	for _, g := range req.RemoveGroups {
		if !nameRe.MatchString(g) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid group name: " + g}
		}
		runArgv(ctx, nil, "gpasswd", "-d", req.Login, g) //nolint:errcheck // not a member of the group: nothing to do
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &EnsureUnixUserResponse{UID: uid, GID: gid, Home: u.HomeDir, Created: created}, nil
}

// uidMin is the first uid of ordinary accounts: UID_MIN of /etc/login.defs,
// 1000 by default.
func uidMin() int {
	b, err := os.ReadFile("/etc/login.defs")
	if err != nil {
		return 1000
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "UID_MIN" {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1000
}
