package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (s *Server) stat(_ context.Context, req *StatRequest) (*StatResponse, error) {
	resp := &StatResponse{Entries: make([]StatEntry, 0, len(req.Paths))}
	for _, raw := range req.Paths {
		p := filepath.Clean(raw)
		if !filepath.IsAbs(p) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "absolute path required"}
		}
		e := StatEntry{Path: p}
		st, err := os.Lstat(p)
		if err == nil {
			e.Exists, e.IsDir, e.Size, e.Mode = true, st.IsDir(), st.Size(), st.Mode().String()
			e.ModTime = st.ModTime().UTC().Format(time.RFC3339)
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				e.UID, e.GID = int(sys.Uid), int(sys.Gid)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

// readFile serves log tails: only regular files below <home>/data/logs/ or
// well-known service logs.
func (s *Server) readFile(_ context.Context, req *ReadFileRequest) (*ReadFileResponse, error) {
	p := filepath.Clean(req.Path)
	allowed := false
	if strings.HasPrefix(p, filepath.Clean(s.cfg.WWWRoot)+"/") {
		if _, err := s.safeHomePath(p); err != nil {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
		rel := strings.TrimPrefix(p, filepath.Clean(s.cfg.WWWRoot)+"/")
		parts := strings.Split(rel, "/")
		allowed = len(parts) == 4 && parts[1] == "data" && parts[2] == "logs" && strings.HasSuffix(parts[3], ".log")
	}
	for _, pre := range []string{"/var/log/nginx/", "/var/log/apache2/", "/var/log/httpd/", "/var/log/mysql/", "/var/log/php", s.cfg.LogDir + "/"} {
		if strings.HasPrefix(p, pre) {
			allowed = true
		}
	}
	if !allowed {
		return nil, &Error{Status: http.StatusForbidden, Message: "not a readable log path: " + p}
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ReadFileResponse{}, nil
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, &Error{Status: http.StatusBadRequest, Message: "not a regular file"}
	}
	tail := req.TailBytes
	if tail <= 0 || tail > 4<<20 {
		tail = 256 << 10
	}
	resp := &ReadFileResponse{Size: st.Size()}
	if st.Size() > tail {
		if _, err := f.Seek(st.Size()-tail, io.SeekStart); err != nil {
			return nil, err
		}
		resp.Truncated = true
	}
	b, err := io.ReadAll(io.LimitReader(f, tail))
	if err != nil {
		return nil, err
	}
	if resp.Truncated {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	resp.Content = string(b)
	return resp, nil
}

func (s *Server) setUnixPassword(ctx context.Context, req *SetUnixPasswordRequest) (*struct{}, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	if len(req.Password) < 8 || strings.ContainsAny(req.Password, "\n:") {
		return nil, &Error{Status: http.StatusBadRequest, Message: "password must be at least 8 characters without ':' or newlines"}
	}
	res, err := s.tool(ctx, &ToolRequest{Name: "chpasswd", Stdin: fmt.Sprintf("%s:%s\n", req.Login, req.Password), TimeoutSeconds: 30})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &Error{Message: "chpasswd failed", Output: res.Output}
	}
	return &struct{}{}, nil
}
