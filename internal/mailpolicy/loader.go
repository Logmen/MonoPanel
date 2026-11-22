package mailpolicy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// stamp tells a file's version apart: a rename puts a new inode in place,
// an edit in place changes size, mtime or ctime.
type stamp struct {
	ino, size    int64
	mtime, ctime time.Time
}

func stampOf(fi os.FileInfo) stamp {
	s := stamp{size: fi.Size(), mtime: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.ino = int64(st.Ino) //nolint:gosec // inode numbers fit
		s.ctime = time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
	}
	return s
}

// Loader keeps the policy and the trust in pickup's header copy current.
// Current stats both files on every call — a stat is a microsecond, and the
// milter calls it once per message: a new account, mailbox or domain counts
// from the next message on, with no reload signal and no timer — and reads
// a file again only when it changed. A file that cannot be read or parsed
// leaves the last good policy in place, and says so in the log once.
type Loader struct {
	PolicyPath, ChecksPath string
	Log                    *slog.Logger

	mu        sync.Mutex
	index     *Index
	polStamp  stamp
	polSeen   bool
	polErr    string
	checksOK  bool
	chkStamp  stamp
	chkSeen   bool
	chkErr    string
	readLimit int64
}

// NewLoader reads the files at path and checks.
func NewLoader(path, checks string, log *slog.Logger) *Loader {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Loader{PolicyPath: path, ChecksPath: checks, Log: log, readLimit: MaxPolicySize}
}

// Current is the policy (nil while no good one was ever read) and whether
// the first header of a local submission is pickup's copy (HeaderChecksFile
// is exactly HeaderChecksContent).
func (l *Loader) Current() (*Index, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshPolicy()
	l.refreshChecks()
	return l.index, l.checksOK
}

func (l *Loader) refreshPolicy() {
	fi, err := os.Stat(l.PolicyPath)
	if err != nil {
		l.policyFailed(fmt.Sprintf("%v", err))
		return
	}
	st := stampOf(fi)
	if l.polSeen && st == l.polStamp {
		return
	}
	b, err := readLimited(l.PolicyPath, l.readLimit)
	if err != nil {
		l.policyFailed(err.Error())
		return
	}
	p, err := Parse(b)
	if err != nil {
		l.polSeen, l.polStamp = true, st
		l.policyFailed(err.Error())
		return
	}
	l.index, l.polSeen, l.polStamp, l.polErr = NewIndex(p), true, st, ""
	l.Log.Info("mail policy loaded", "file", l.PolicyPath, "accounts", len(p.Accounts), "mailboxes", len(p.Mailboxes))
}

func (l *Loader) policyFailed(msg string) {
	if msg == l.polErr {
		return
	}
	l.polErr = msg
	if l.index != nil {
		l.Log.Error("mail policy: the file cannot be used; the last good copy stays", "file", l.PolicyPath, "err", msg)
	} else {
		l.Log.Error("mail policy: no policy yet; the mail of accounts waits (tempfail) until there is one", "file", l.PolicyPath, "err", msg)
	}
}

func (l *Loader) refreshChecks() {
	fi, err := os.Stat(l.ChecksPath)
	if err != nil {
		l.checksFailed(err.Error())
		return
	}
	st := stampOf(fi)
	if l.chkSeen && st == l.chkStamp {
		return
	}
	l.chkSeen, l.chkStamp = true, st
	b, err := readLimited(l.ChecksPath, 64<<10)
	if err != nil {
		l.checksFailed(err.Error())
		return
	}
	if !checksMatch(string(b)) {
		l.checksFailed("it is not the panel's table: its rule must be " + HeaderChecksRule)
		return
	}
	if !l.checksOK {
		l.Log.Info("mail policy: local submissions are told by pickup's header", "file", l.ChecksPath)
	}
	l.checksOK, l.chkErr = true, ""
}

func (l *Loader) checksFailed(msg string) {
	l.checksOK = false
	if msg == l.chkErr {
		return
	}
	l.chkErr = msg
	l.Log.Error("mail policy: the sender of local submissions cannot be told, they may use no domain of the panel", "file", l.ChecksPath, "err", msg)
}

// checksMatch tells whether a header_checks table holds the panel's rule
// and nothing else that acts on a header: blank lines and comments aside,
// exactly HeaderChecksRule.
func checksMatch(content string) bool {
	var rules []string
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		rules = append(rules, line)
	}
	return len(rules) == 1 && rules[0] == HeaderChecksRule
}

// readLimited reads a regular file of at most limit bytes.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read only
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}
