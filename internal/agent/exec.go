package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"time"
)

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte("[...truncated...]\n"), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// runArgv executes argv without a shell and returns combined output.
func runArgv(ctx context.Context, env []string, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 5 * time.Second
	out := &tailBuffer{max: 256 * 1024}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err != nil && out.buf == nil {
		// no output at all (binary missing, permission denied): surface the exec error itself
		return err.Error() + "\n", err
	}
	return out.String(), err
}

var (
	nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	unitRe = regexp.MustCompile(`^[A-Za-z0-9_.@:\\-]+$`)
	pkgRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:=~-]*$`)
	rpmURL = regexp.MustCompile(`^https://[a-z0-9.-]+/[A-Za-z0-9._/-]+\.rpm$`)
)

func lookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(u.Uid)
}

func lookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(g.Gid)
}

type unixUser struct {
	uid int
	gid int
}

func lookupUser(name string) (unixUser, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return unixUser{}, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return unixUser{uid: uid, gid: gid}, nil
}
