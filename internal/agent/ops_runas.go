package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// RunAsUserRequest runs `monopanel fsop <args>` as a client through the
// privilege-dropping helper.
type RunAsUserRequest struct {
	Login          string   `json:"login"`
	Args           []string `json:"args"`
	StdinBase64    string   `json:"stdin_base64,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// RunAsUserResponse returns exit status and output.
type RunAsUserResponse struct {
	ExitCode     int    `json:"exit_code"`
	StdoutBase64 string `json:"stdout_base64,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
}

func (s *Server) runAsUser(ctx context.Context, req *RunAsUserRequest) (*RunAsUserResponse, error) {
	if !nameRe.MatchString(req.Login) || len(req.Args) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "login and args are required"}
	}
	u, err := lookupUser(req.Login)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "unknown user " + req.Login}
	}
	if u.uid == 0 || u.gid == 0 {
		return nil, &Error{Status: http.StatusForbidden, Message: "refusing to run as root"}
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	timeout := 10 * time.Minute
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append([]string{"helper", "--uid", strconv.Itoa(u.uid), "--gid", strconv.Itoa(u.gid), "--", self, "fsop"}, req.Args...)
	cmd := exec.CommandContext(rctx, self, argv...)
	cmd.WaitDelay = 5 * time.Second
	if req.StdinBase64 != "" {
		in, err := base64.StdEncoding.DecodeString(req.StdinBase64)
		if err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "stdin_base64: " + err.Error()}
		}
		cmd.Stdin = bytes.NewReader(in)
	}
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 64 * 1024}
	cmd.Stdout = &limitedWriter{w: &stdout, max: 512 << 20}
	cmd.Stderr = stderr
	err = cmd.Run()
	resp := &RunAsUserResponse{StdoutBase64: base64.StdEncoding.EncodeToString(stdout.Bytes()), Stderr: strings.TrimSpace(stderr.String())}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			resp.ExitCode = ee.ExitCode()
			return resp, nil
		}
		return nil, &Error{Message: "helper: " + err.Error(), Output: stderr.String()}
	}
	return resp, nil
}

type limitedWriter struct {
	w   *bytes.Buffer
	max int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.w.Len()+len(p) > l.max {
		return 0, errors.New("output too large")
	}
	return l.w.Write(p)
}
