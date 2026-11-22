package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// readFile serves log tails: only regular files in the panel's log directory
// (the site logs in <log_dir>/sites/<login>/ among them), well-known service
// logs and the PHP configuration. Nothing in the accounts' homes: the site
// logs left them because root must not open files there.
func (s *Server) readFile(_ context.Context, req *ReadFileRequest) (*ReadFileResponse, error) {
	p := filepath.Clean(req.Path)
	logDir := filepath.Clean(s.cfg.LogDir)
	inLogDir := strings.HasPrefix(p, logDir+"/")
	allowed := inLogDir
	// /var/log/mysqld.log is the EL server log, where the package leaves
	// the temporary root password; the PHP configuration directories hold
	// the extension ini files the panel switches on and off.
	for _, pre := range []string{"/var/log/nginx/", "/var/log/apache2/", "/var/log/httpd/", "/var/log/mysql/", "/var/log/mysqld.log", "/var/log/php", "/etc/php/", "/etc/opt/remi/"} {
		if strings.HasPrefix(p, pre) {
			allowed = true
		}
	}
	if !allowed {
		return nil, &Error{Status: http.StatusForbidden, Message: "not a readable log path: " + p}
	}
	var f *os.File
	var err error
	if inLogDir {
		// The log directory is root:<service group> 1771 (ops_sitelogs.go):
		// the API's service user still makes entries of its own in it — a
		// symlink, or a second name of another file, for root to read —,
		// though it can rename or remove none of root's, sites/ above all,
		// which is root's alone. So the directories are opened one by one
		// without following a symlink, the file so too, and it must have
		// one name.
		f, err = openBelow(logDir, p)
		if errors.Is(err, errSymlinkInPath) {
			return nil, &Error{Status: http.StatusForbidden, Message: err.Error()}
		}
	} else {
		f, err = os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	}
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
	if inLogDir && hasOtherNames(st) {
		return nil, &Error{Status: http.StatusForbidden, Message: p + " has other names: refusing to read it"}
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

// setUnixPassword sets the SFTP/SSH password of one of the panel's clients
// (internal/api/ops_users.go) or carries its hash over in a migration
// (ops_migrate_import.go). Only the panel's accounts: chpasswd for root, an
// administrator's account or a system user would be a way in for the API.
func (s *Server) setUnixPassword(ctx context.Context, req *SetUnixPasswordRequest) (*struct{}, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	if len(req.Password) < 8 || strings.ContainsAny(req.Password, "\n\r:\x00") {
		return nil, &Error{Status: http.StatusBadRequest, Message: "password must be at least 8 characters without ':' or newlines"}
	}
	if _, err := s.panelAccount(req.Login); err != nil {
		if errors.Is(err, errNoAccount) {
			return nil, &Error{Status: http.StatusNotFound, Message: "no such user"}
		}
		return nil, err
	}
	bin := ""
	for _, c := range []string{"/usr/sbin/chpasswd", "/sbin/chpasswd"} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			bin = c
			break
		}
	}
	if bin == "" {
		return nil, &Error{Message: "chpasswd is not installed"}
	}
	args := []string{}
	if req.Encrypted {
		args = append(args, "-e")
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(rctx, bin, args...)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("%s:%s\n", req.Login, req.Password))
	out := &tailBuffer{max: 64 * 1024}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	code, err := exitCode(cmd.Run())
	if err != nil || code != 0 {
		return nil, &Error{Message: "chpasswd failed", Output: out.String()}
	}
	return &struct{}{}, nil
}

// unixShadow returns the password hash of a client account so a migration can
// carry it to another server. Only accounts the panel itself creates qualify:
// system users, root and the server's own users are never readable this way.
func (s *Server) unixShadow(_ context.Context, req *UnixShadowRequest) (*UnixShadowResponse, error) {
	if !nameRe.MatchString(req.Login) || req.Login == "root" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid login"}
	}
	if _, err := s.panelAccount(req.Login); err != nil {
		if errors.Is(err, errNoAccount) {
			return nil, &Error{Status: http.StatusNotFound, Message: "no such user"}
		}
		return nil, err
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
	// a branch's pools, which the doctor reads for catch_workers_output (the
	// way of CVE-2026-129291 from a pool's worker to the root master)
	regexp.MustCompile(`^/etc/php/[0-9]+\.[0-9]+/fpm/pool\.d$`),
	regexp.MustCompile(`^/etc/opt/remi/php[0-9]+/php-fpm\.d$`),
	// the default servers, one per local address: those of addresses the
	// host lost are removed, nginx cannot bind them
	regexp.MustCompile(`^/etc/nginx/monopanel/http\.d$`),
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
