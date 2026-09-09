package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Streaming tools. A migration moves whole home directories and database
// dumps — tens of gigabytes — so /v1/tool with its captured output is the
// wrong shape: here stdout (or stdin) is the HTTP body and nothing is
// buffered. The binaries are the same allow-listed ones.
var (
	streamOutTools = map[string]bool{"tar": true, "mysqldump": true}
	streamInTools  = map[string]bool{"tar": true, "mysql": true}
)

// StreamRequest names the tool and its argv. Exactly one direction is
// streamed per call: /v1/stream/out sends stdout, /v1/stream/in takes stdin.
type StreamRequest struct {
	Name           string   `json:"name"`
	Args           []string `json:"args,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// StreamResponse is the JSON answer of /v1/stream/in and the trailer of
// /v1/stream/out: a stream that failed halfway cannot use a status code.
type StreamResponse struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

func (s *Server) streamCmd(ctx context.Context, req *StreamRequest, allowed map[string]bool) (*exec.Cmd, context.CancelFunc, error) {
	if !allowed[req.Name] {
		return nil, nil, &Error{Status: http.StatusBadRequest, Message: "tool not allowed for streaming: " + req.Name}
	}
	bin, err := resolveTool(req.Name)
	if err != nil {
		return nil, nil, &Error{Status: http.StatusBadRequest, Message: err.Error()}
	}
	for _, a := range req.Args {
		if strings.ContainsRune(a, 0) {
			return nil, nil, &Error{Status: http.StatusBadRequest, Message: "invalid argument"}
		}
	}
	timeout := 6 * time.Hour
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	cmd := exec.CommandContext(rctx, bin, req.Args...)
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
	cmd.WaitDelay = 5 * time.Second
	return cmd, cancel, nil
}

// streamOut runs the tool and copies its stdout into the response body. The
// exit status arrives in a trailer, because by the time the tool fails the
// status line is long gone.
func (s *Server) streamOut(w http.ResponseWriter, r *http.Request) {
	var req StreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, &Error{Status: http.StatusBadRequest, Message: "bad request: " + err.Error()})
		return
	}
	cmd, cancel, err := s.streamCmd(r.Context(), &req, streamOutTools)
	if err != nil {
		var ae *Error
		if !errors.As(err, &ae) {
			ae = &Error{Status: http.StatusInternalServerError, Message: err.Error()}
		}
		writeErr(w, ae)
		return
	}
	defer cancel()
	stderr := &tailBuffer{max: 32 * 1024}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, &Error{Message: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		writeErr(w, &Error{Message: req.Name + ": " + err.Error()})
		return
	}
	w.Header().Set("Trailer", "X-Exit-Code, X-Error")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, out)
	waitErr := cmd.Wait()
	code := 0
	var ee *exec.ExitError
	switch {
	case copyErr != nil:
		code = -1
	case errors.As(waitErr, &ee):
		code = ee.ExitCode()
	case waitErr != nil:
		code = -1
	}
	w.Header().Set("X-Exit-Code", strconv.Itoa(code))
	if code != 0 {
		msg := strings.TrimSpace(stderr.String())
		if copyErr != nil {
			msg = copyErr.Error() + " " + msg
		}
		s.log.Warn("stream failed", "tool", req.Name, "code", code, "err", msg)
		w.Header().Set("X-Error", strings.ReplaceAll(msg, "\n", " "))
	}
}

// streamIn feeds the request body to the tool's stdin (tar -x, mysql) and
// answers with its exit status. The specification travels in a header so the
// body stays exactly what the tool reads.
func (s *Server) streamIn(w http.ResponseWriter, r *http.Request) {
	raw, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Stream-Spec"))
	if err != nil {
		writeErr(w, &Error{Status: http.StatusBadRequest, Message: "X-Stream-Spec: " + err.Error()})
		return
	}
	var req StreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, &Error{Status: http.StatusBadRequest, Message: "X-Stream-Spec: " + err.Error()})
		return
	}
	cmd, cancel, err := s.streamCmd(r.Context(), &req, streamInTools)
	if err != nil {
		var ae *Error
		if !errors.As(err, &ae) {
			ae = &Error{Status: http.StatusInternalServerError, Message: err.Error()}
		}
		writeErr(w, ae)
		return
	}
	defer cancel()
	buf := &tailBuffer{max: 32 * 1024}
	cmd.Stdout, cmd.Stderr = buf, buf
	cmd.Stdin = r.Body
	resp := &StreamResponse{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			resp.ExitCode = ee.ExitCode()
		} else {
			writeErr(w, &Error{Message: req.Name + ": " + err.Error(), Output: buf.String()})
			return
		}
	}
	resp.Output = buf.String()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ChownRequest fixes ownership of a client's tree. Extraction runs as root,
// so the files land root-owned; this puts them back to their owner.
type ChownRequest struct {
	Path      string `json:"path"`
	Owner     string `json:"owner"`
	Group     string `json:"group,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
}

// ChownResponse counts what was touched.
type ChownResponse struct {
	Changed int `json:"changed"`
}

func (s *Server) chown(_ context.Context, req *ChownRequest) (*ChownResponse, error) {
	p, err := s.safeHomePath(req.Path)
	if err != nil {
		return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
	}
	uid, err := lookupUID(req.Owner)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "owner: " + err.Error()}
	}
	gid := -1
	if req.Group != "" {
		if gid, err = lookupGID(req.Group); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "group: " + err.Error()}
		}
	}
	resp := &ChownResponse{}
	fix := func(path string) error {
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == uid && (gid < 0 || int(sys.Gid) == gid) {
			return nil
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return err
		}
		resp.Changed++
		return nil
	}
	if !req.Recursive {
		return resp, fix(p)
	}
	err = filepath.WalkDir(p, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return fix(path)
	})
	if err != nil {
		return resp, &Error{Message: "chown " + p, Output: err.Error()}
	}
	return resp, nil
}
