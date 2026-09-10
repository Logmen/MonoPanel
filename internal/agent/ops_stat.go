package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
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
	// /var/log/mysqld.log is the EL server log, where the package leaves
	// the temporary root password; the PHP configuration directories hold
	// the extension ini files the panel switches on and off.
	for _, pre := range []string{"/var/log/nginx/", "/var/log/apache2/", "/var/log/httpd/", "/var/log/mysql/", "/var/log/mysqld.log", "/var/log/php", "/etc/php/", "/etc/opt/remi/", s.cfg.LogDir + "/"} {
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
	args := []string{}
	if req.Encrypted {
		args = append(args, "-e")
	}
	res, err := s.tool(ctx, &ToolRequest{Name: "chpasswd", Args: args, Stdin: fmt.Sprintf("%s:%s\n", req.Login, req.Password), TimeoutSeconds: 30})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &Error{Message: "chpasswd failed", Output: res.Output}
	}
	return &struct{}{}, nil
}

// unixShadow returns the password hash of a client account so a migration can
// carry it to another server. Only accounts the panel itself creates qualify:
// system users and root are never readable this way.
func (s *Server) unixShadow(_ context.Context, req *UnixShadowRequest) (*UnixShadowResponse, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	u, err := user.Lookup(req.Login)
	if err != nil {
		return nil, &Error{Status: http.StatusNotFound, Message: "no such user"}
	}
	uid, _ := strconv.Atoi(u.Uid)
	root := filepath.Clean(s.cfg.WWWRoot)
	if uid < 1000 || !strings.HasPrefix(filepath.Clean(u.HomeDir), root+"/") {
		return nil, &Error{Status: http.StatusForbidden, Message: "only accounts of the panel can be exported"}
	}
	f, err := os.Open("/etc/shadow")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) > 1 && fields[0] == req.Login {
			hash := fields[1]
			// "!" и "*" — это «пароля нет», переносить нечего.
			if hash == "" || strings.HasPrefix(hash, "!") || hash == "*" {
				return &UnixShadowResponse{}, nil
			}
			return &UnixShadowResponse{Hash: hash}, nil
		}
	}
	return &UnixShadowResponse{}, sc.Err()
}

// listDirAllowed limits directory listing to configuration the panel manages.
// A general listing operation would hand the API process a file browser over
// the whole disk; this stays inside what the panel needs to know.
var listDirAllowed = []*regexp.Regexp{
	regexp.MustCompile(`^/etc/php/[0-9]+\.[0-9]+/mods-available$`),
	regexp.MustCompile(`^/etc/php/[0-9]+\.[0-9]+/(fpm|cli)/conf\.d$`),
	regexp.MustCompile(`^/etc/opt/remi/php[0-9]+/php\.d$`),
	// the search index files of the Sphinx extension (recreated on a schema change)
	regexp.MustCompile(`^/var/lib/sphinx$`),
	regexp.MustCompile(`^/var/lib/sphinxsearch/data$`),
}

func (s *Server) listDir(_ context.Context, req *ListDirRequest) (*ListDirResponse, error) {
	p := filepath.Clean(req.Path)
	ok := false
	for _, re := range listDirAllowed {
		if re.MatchString(p) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, &Error{Status: http.StatusForbidden, Message: "directory not allowed: " + p}
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ListDirResponse{Entries: []ListDirEntry{}}, nil
		}
		return nil, err
	}
	out := &ListDirResponse{Entries: make([]ListDirEntry, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, ListDirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}
