package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Streaming tools. A migration moves whole home directories and database
// dumps — tens of gigabytes — so /v1/tool with its captured output is the
// wrong shape: here stdout (or stdin) is the HTTP body and nothing is
// buffered. The tools and their shapes are vetted by the same policies as
// /v1/tool: tar --create and mysqldump stream out, tar -x (done by the agent
// itself) and mysql stream in.

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

// StreamLimit is how long a stream may run when its request names no limit
// (TimeoutSeconds): a migration's copy of a home, of a database, of a mail
// domain's Maildirs. The API's time limit of a migration counts in it.
const StreamLimit = 6 * time.Hour

// streamPlan vets a streaming request and prepares the command, unless the
// agent does the work itself (plan.native).
func (s *Server) streamPlan(ctx context.Context, req *StreamRequest, mode toolMode) (*toolPlan, *exec.Cmd, context.Context, context.CancelFunc, error) {
	plan, err := s.planTool(&toolCall{name: req.Name, args: req.Args, mode: mode})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	timeout := StreamLimit
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	if plan.native != nil {
		return plan, nil, rctx, cancel, nil
	}
	bin, err := resolveTool(req.Name)
	if err == nil && plan.prepare != nil {
		err = plan.prepare()
	}
	if err != nil {
		cancel()
		plan.close()
		return nil, nil, nil, nil, &Error{Status: http.StatusBadRequest, Message: err.Error()}
	}
	cmd := exec.CommandContext(rctx, bin, plan.args...)
	// English, whatever LANGUAGE the host gives its units (osprofile's
	// aptManager.Env).
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "LANGUAGE=")
	cmd.ExtraFiles = plan.files
	cmd.WaitDelay = 5 * time.Second
	return plan, cmd, rctx, cancel, nil
}

func writeStreamErr(w http.ResponseWriter, err error) {
	var ae *Error
	if !errors.As(err, &ae) {
		ae = &Error{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	if ae.Status == 0 {
		ae.Status = http.StatusInternalServerError
	}
	writeErr(w, ae)
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
	plan, cmd, _, cancel, err := s.streamPlan(r.Context(), &req, modeStreamOut)
	if err != nil {
		writeStreamErr(w, err)
		return
	}
	defer cancel()
	defer plan.close()
	if cmd == nil {
		writeErr(w, &Error{Status: http.StatusBadRequest, Message: req.Name + " does not stream out"})
		return
	}
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
	plan, cmd, rctx, cancel, err := s.streamPlan(r.Context(), &req, modeStreamIn)
	if err != nil {
		writeStreamErr(w, err)
		return
	}
	defer cancel()
	defer plan.close()
	buf := &tailBuffer{max: 32 * 1024}
	resp := &StreamResponse{}
	if cmd == nil {
		resp.ExitCode = plan.native(rctx, r.Body, buf)
	} else {
		cmd.Stdout, cmd.Stderr = buf, buf
		cmd.Stdin = r.Body
		if resp.ExitCode, err = exitCode(cmd.Run()); err != nil {
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
	// Mode, when set, also sets the permissions of regular files. Both go
	// through the opened file, not the name: a name swapped for a symlink
	// meanwhile changes nothing. Other file types keep their mode.
	Mode uint32 `json:"mode,omitempty"`
}

// ChownResponse counts what was touched.
type ChownResponse struct {
	Changed int `json:"changed"`
	// Skipped counts files with several names left as they were where the
	// kernel lets users link files they do not own (see chownWalk.file).
	Skipped int `json:"skipped,omitempty"`
}

// chown hands a tree in an account's home (or a domain of the mail store) to
// its owner after the agent unpacked it as root (internal/api/cms.go,
// ops_sites.go, ops_migrate_import.go). Every directory is opened without
// following a symlink and every change goes through a descriptor: an account
// that swaps a directory of its tree for a symlink meanwhile cannot turn this
// into a chown of /etc/shadow.
func (s *Server) chown(ctx context.Context, req *ChownRequest) (*ChownResponse, error) {
	p := filepath.Clean(req.Path)
	inMail := below(p, mailStore)
	if !filepath.IsAbs(req.Path) || !(below(p, s.cfg.WWWRoot) || inMail) || s.coversProtected(p) || s.inRestoreArea(p) {
		return nil, notAllowed("chown of "+req.Path, "only trees in the accounts' homes and the mail store change owner")
	}
	w, err := s.chownTarget(req, inMail)
	if err != nil {
		return nil, err
	}
	w.ctx = ctx
	root := s.cfg.WWWRoot
	if inMail {
		root = mailStore
	} else {
		// A home holding a shared folder's mount point, or a directory on the
		// way to one, stays root's; a tree with one below would also be
		// walked into the mounted folder (sharemount.go).
		g, release, err := s.holdShareGuard()
		if err != nil {
			return nil, err
		}
		defer release()
		if m, ok := g.holding(p); ok {
			return nil, g.refuse("handing "+p+" to "+req.Owner, m)
		}
	}
	parent, err := openDirBelow(root, filepath.Dir(p), false, 0)
	if err != nil {
		return nil, notAllowed("chown of "+p, err.Error())
	}
	defer unix.Close(parent) //nolint:errcheck // a directory opened for reading
	base := filepath.Base(p)
	var st unix.Stat_t
	if err := unix.Fstatat(parent, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &Error{Message: "chown " + p, Output: err.Error()}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return nil, notAllowed("chown of "+p, "refusing to follow the symlink "+p)
	case unix.S_IFDIR:
		fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, notAllowed("chown of "+p, err.Error())
		}
		defer unix.Close(fd) //nolint:errcheck // a directory opened for reading
		err = w.fd(fd, p)
		if err == nil && req.Recursive {
			err = w.dir(fd, p)
		}
		if err != nil {
			return &w.resp, &Error{Message: "chown " + p, Output: err.Error()}
		}
	default:
		if err := w.entry(parent, base, p); err != nil {
			return &w.resp, &Error{Message: "chown " + p, Output: err.Error()}
		}
	}
	return &w.resp, nil
}

// chownTarget resolves and vets the owner: one of the panel's accounts below
// the web root (an ordinary uid), vmail in the mail store; never root, never
// a privileged group.
func (s *Server) chownTarget(req *ChownRequest, inMail bool) (*chownWalk, error) {
	u, err := lookupAccount(req.Owner)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "owner: " + err.Error()}
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "owner: uid " + u.Uid}
	}
	switch {
	case uid == 0:
		return nil, notAllowed("chown to "+req.Owner, "it is root")
	case inMail && req.Owner != mailUser:
		return nil, notAllowed("chown to "+req.Owner+" in the mail store", "the mail store is "+mailUser+"'s")
	case !inMail && uid < loginUIDMin():
		return nil, notAllowed("chown to "+req.Owner, fmt.Sprintf("uid %d is a system account's", uid))
	case !inMail:
		// a tree of a home goes to one of the panel's clients, not to an
		// administrator's account that happens to have an ordinary uid
		if err := s.checkPanelAccount(u); err != nil {
			return nil, err
		}
	}
	gid := -1
	if req.Group != "" {
		g, err := user.LookupGroup(req.Group)
		if err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "group: " + err.Error()}
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil || gid == 0 || s.privilegedGroup(req.Group) {
			return nil, notAllowed("chown to the group "+req.Group, "it is a privileged group")
		}
		switch {
		case inMail && req.Group != mailUser:
			return nil, notAllowed("chown to the group "+req.Group+" in the mail store", "the mail store is "+mailUser+"'s")
		case !inMail && req.Group != req.Owner:
			// internal/api hands a tree to the account and its own group
			return nil, notAllowed("chown to the group "+req.Group, "a client's tree goes to its own group, "+req.Owner)
		}
	}
	return &chownWalk{uid: uid, gid: gid, mode: req.Mode & 0o777, linksSafe: hardlinksProtected()}, nil
}

type chownWalk struct {
	ctx       context.Context
	uid, gid  int
	mode      uint32
	linksSafe bool
	resp      ChownResponse
}

func (w *chownWalk) owned(st *unix.Stat_t) bool {
	return int(st.Uid) == w.uid && (w.gid < 0 || int(st.Gid) == w.gid)
}

// fd re-owns an opened directory.
func (w *chownWalk) fd(fd int, p string) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if w.owned(&st) {
		return nil
	}
	if err := unix.Fchown(fd, w.uid, w.gid); err != nil {
		return &os.PathError{Op: "chown", Path: p, Err: err}
	}
	w.resp.Changed++
	return nil
}

// dir re-owns everything below the opened directory.
func (w *chownWalk) dir(fd int, p string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	names, err := readDirNames(fd)
	if err != nil {
		return &os.PathError{Op: "readdir", Path: p, Err: err}
	}
	for _, n := range names {
		child := filepath.Join(p, n)
		var st unix.Stat_t
		if err := unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // gone meanwhile
			}
			return &os.PathError{Op: "stat", Path: child, Err: err}
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := w.entry(fd, n, child); err != nil {
				return err
			}
			continue
		}
		cfd, err := unix.Openat(fd, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			// swapped for something else since the stat: take it as it is now
			if err := w.entry(fd, n, child); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return &os.PathError{Op: "open", Path: child, Err: err}
		}
		err = w.fd(cfd, child)
		if err == nil {
			err = w.dir(cfd, child)
		}
		unix.Close(cfd) //nolint:errcheck // a directory opened for reading
		if err != nil {
			return err
		}
	}
	return nil
}

// entry re-owns a non-directory: opened as a path (O_PATH opens no device
// and follows no symlink), checked on the descriptor, changed through it.
func (w *chownWalk) entry(dir int, name, p string) error {
	fd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: p, Err: err}
	}
	defer unix.Close(fd) //nolint:errcheck // a path descriptor
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: p, Err: err}
	}
	regular := st.Mode&unix.S_IFMT == unix.S_IFREG
	if regular && st.Nlink > 1 && !w.linksSafe {
		// Without fs.protected_hardlinks an account can give a file of
		// root's a second name in its tree: that one stays as it is.
		w.resp.Skipped++
		return nil
	}
	setMode := regular && w.mode != 0 && st.Mode&0o7777 != w.mode
	if w.owned(&st) && !setMode {
		return nil
	}
	if !w.owned(&st) {
		if err := unix.Fchownat(fd, "", w.uid, w.gid, unix.AT_EMPTY_PATH); err != nil {
			return &os.PathError{Op: "chown", Path: p, Err: err}
		}
	}
	if setMode {
		// fchmod does not take a path descriptor; its /proc name does, and
		// it names this very file.
		if err := unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), w.mode); err != nil {
			return &os.PathError{Op: "chmod", Path: p, Err: err}
		}
	}
	w.resp.Changed++
	return nil
}
