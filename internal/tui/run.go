package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/i18n"
)

// Every action runs this binary as a child process with the command's
// arguments. The panel address and the token travel in the environment
// (MP_SERVER, MP_TOKEN, MP_INSECURE), never on its command line.

// runJob is one command to run.
type runJob struct {
	argv  []string // after the binary: site add example.com --php=8.4
	shown string   // the same for people, secrets masked
	stdin string   // a secret for --password-stdin and the like
}

type runEvent struct {
	out  string
	done bool
	code int
	err  error
}

// runner starts the child processes; tests put a fake one in its place.
type runner interface {
	start(job runJob, env []string) (<-chan runEvent, func(), error)
	terminal(job runJob, env []string) *exec.Cmd
	editor(path string, env []string) *exec.Cmd
}

type execRunner struct{ exe string }

func newExecRunner() execRunner {
	exe, err := os.Executable()
	if err != nil {
		exe = "mp"
	}
	return execRunner{exe: exe}
}

func (r execRunner) start(job runJob, env []string) (<-chan runEvent, func(), error) {
	c := exec.Command(r.exe, job.argv...)
	c.Env = env
	if job.stdin != "" {
		c.Stdin = strings.NewReader(job.stdin + "\n")
	}
	// One pipe for stdout and stderr keeps their lines in order. Without a
	// stdin the child reads /dev/null: a prompt gets EOF instead of hanging.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	c.Stdout, c.Stderr = pw, pw
	if err := c.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, err
	}
	pw.Close()
	ch := make(chan runEvent, 64)
	exited := make(chan struct{})
	go func() {
		buf := make([]byte, 16<<10)
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				ch <- runEvent{out: string(buf[:n])}
			}
			if rerr != nil {
				break
			}
		}
		pr.Close()
		werr := c.Wait()
		close(exited)
		ev := runEvent{done: true}
		var ee *exec.ExitError
		switch {
		case errors.As(werr, &ee):
			ev.code = ee.ExitCode()
		case werr != nil:
			ev.code, ev.err = -1, werr
		}
		ch <- ev
		close(ch)
	}()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			// mp stops waiting on SIGINT (the job itself goes on on the
			// server); a child that ignores it is killed a little later.
			_ = c.Process.Signal(os.Interrupt)
			go func() {
				select {
				case <-exited:
				case <-time.After(3 * time.Second):
					_ = c.Process.Kill()
				}
			}()
		})
	}
	return ch, cancel, nil
}

// terminal runs the command in the terminal itself and waits for Enter
// before giving the screen back, so its output (a QR code) can be read.
func (r execRunner) terminal(job runJob, env []string) *exec.Cmd {
	script := `"$0" "$@"; s=$?; printf '\n%s ' "$MP_TUI_PAUSE"; read -r _; exit $s`
	c := exec.Command("/bin/sh", append([]string{"-c", script, r.exe}, job.argv...)...)
	c.Env = append(env, "MP_TUI_PAUSE="+T("Нажмите Enter, чтобы вернуться в меню", "Press Enter to return to the menu"))
	if job.stdin != "" {
		c.Stdin = strings.NewReader(job.stdin + "\n")
	}
	return c
}

// editor opens $VISUAL or $EDITOR (editor, nano or vi when neither is set).
func (r execRunner) editor(path string, env []string) *exec.Cmd {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		for _, name := range []string{"editor", "nano", "vi"} {
			if p, err := exec.LookPath(name); err == nil {
				ed = p
				break
			}
		}
	}
	if ed == "" {
		ed = "vi"
	}
	c := exec.Command("/bin/sh", "-c", ed+` "$1"`, "sh", path)
	c.Env = env
	return c
}

// childEnv is the environment of a command: the TUI's own, with the panel
// address, the token, the TLS switch, the config path and the language set
// from how the TUI was started.
func childEnv(base []string, opts Options) []string {
	out := make([]string, 0, len(base)+5)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "MP_SERVER", "MP_TOKEN", "MP_INSECURE", "MP_LANG":
			continue
		case "MONOPANEL_CONFIG":
			if opts.Config != "" {
				continue
			}
		}
		out = append(out, kv)
	}
	if opts.Server != "" {
		out = append(out, "MP_SERVER="+opts.Server)
		if opts.Token != "" {
			out = append(out, "MP_TOKEN="+opts.Token)
		}
	}
	if opts.Insecure {
		out = append(out, "MP_INSECURE=1")
	}
	if opts.Config != "" {
		out = append(out, "MONOPANEL_CONFIG="+opts.Config)
	}
	lang := "en"
	if i18n.RU() {
		lang = "ru"
	}
	return append(out, "MP_LANG="+lang)
}

type runEventMsg struct {
	id int
	ev runEvent
}

// waitRun delivers the next output of a child; what is already waiting is
// merged into one message so a chatty command does not redraw per line.
func waitRun(id int, ch <-chan runEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return runEventMsg{id: id, ev: runEvent{done: true}}
		}
		if ev.done {
			return runEventMsg{id: id, ev: ev}
		}
		var b strings.Builder
		b.WriteString(ev.out)
		for i := 0; i < 64; i++ {
			select {
			case next, ok := <-ch:
				if !ok {
					return runEventMsg{id: id, ev: runEvent{out: b.String(), done: true}}
				}
				b.WriteString(next.out)
				if next.done {
					next.out = b.String()
					return runEventMsg{id: id, ev: next}
				}
			default:
				return runEventMsg{id: id, ev: runEvent{out: b.String()}}
			}
		}
		return runEventMsg{id: id, ev: runEvent{out: b.String()}}
	}
}

const maxLines = 5000

// runScreen shows a running command: its command line on top, the output
// as it comes, the exit status at the end.
type runScreen struct {
	screenBase
	jobs     []runJob
	cur      int
	lines    []string
	partial  string
	running  bool
	code     int
	failed   int
	err      error
	started  time.Time
	took     time.Duration
	cancel   func()
	stopping bool
	offset   int
	follow   bool
	from     *form
	readOnly bool
	cleanup  func()
	edit     *editState // saving an edited file: kept for another try when it fails
	next     *act
	ch       <-chan runEvent
	gone     bool // the command deleted the object whose page is below
}

func newRunScreen(title string, jobs []runJob) *runScreen {
	return &runScreen{screenBase: screenBase{title: title}, jobs: jobs, follow: true}
}

func (r *runScreen) name() string   { return r.title }
func (r *runScreen) typing() bool   { return false }
func (r *runScreen) finished() bool { return !r.running && r.cur >= len(r.jobs) }

func (r *runScreen) start(m *model) tea.Cmd {
	if r.cur >= len(r.jobs) {
		return nil
	}
	job := r.jobs[r.cur]
	if len(r.jobs) > 1 {
		r.add(fmt.Sprintf("$ %s\n", job.shown))
	}
	ch, cancel, err := m.runner.start(job, childEnv(os.Environ(), m.opts))
	if err != nil {
		r.add(fmt.Sprintf(T("не удалось запустить: %v\n", "could not start: %v\n"), err))
		r.code, r.err = -1, err
		r.failed++
		r.cur = len(r.jobs)
		r.running = false
		r.done()
		return nil
	}
	r.running, r.cancel, r.started, r.ch = true, cancel, time.Now(), ch
	m.running[r.id] = cancel
	return tea.Batch(waitRun(r.id, ch), m.runTick(r.id))
}

type runTickMsg struct{ id int }

// runTick redraws a running command once a second: its timer moves even
// while it prints nothing.
func runTick(id int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return runTickMsg{id: id} })
}

// done runs when the last command has ended. An edited copy that could not
// be saved (nginx -t refused the directives, say) stays until the view is
// left, so that e can open it again instead of losing the edit.
func (r *runScreen) done() {
	if r.edit != nil && r.failed > 0 {
		return
	}
	r.drop()
}

// drop removes what the view kept: the edited copy.
func (r *runScreen) drop() {
	if r.cleanup != nil {
		r.cleanup()
		r.cleanup = nil
	}
}

// editAgain says whether e reopens the editor on the copy that failed to save.
func (r *runScreen) editAgain() bool {
	return r.finished() && r.failed > 0 && r.edit != nil && r.cleanup != nil
}

// add appends output; a carriage return rewrites the current line, as on
// a terminal.
func (r *runScreen) add(s string) {
	s = r.partial + s
	parts := strings.Split(s, "\n")
	r.partial = parts[len(parts)-1]
	for _, p := range parts[:len(parts)-1] {
		p = strings.TrimSuffix(p, "\r")
		if i := strings.LastIndexByte(p, '\r'); i >= 0 {
			p = p[i+1:]
		}
		r.lines = append(r.lines, sanitize(p))
	}
	if over := len(r.lines) - maxLines; over > 0 {
		r.lines = append([]string{}, r.lines[over:]...)
		r.offset = max(0, r.offset-over)
	}
}

func (r *runScreen) output() []string {
	out := r.lines
	if r.partial != "" {
		p := r.partial
		if i := strings.LastIndexByte(p, '\r'); i >= 0 {
			p = p[i+1:]
		}
		out = append(append([]string{}, out...), sanitize(p))
	}
	return out
}

func (r *runScreen) update(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case runTickMsg:
		if r.running {
			return m.runTick(r.id)
		}
	case runEventMsg:
		if msg.ev.out != "" {
			r.add(msg.ev.out)
		}
		if !msg.ev.done {
			return waitRun(r.id, r.ch)
		}
		delete(m.running, r.id)
		r.took += time.Since(r.started)
		r.code, r.err = msg.ev.code, msg.ev.err
		if r.code != 0 {
			r.failed++
		}
		if r.partial != "" {
			r.add("\n")
		}
		r.cur++
		r.running = false
		if r.cur < len(r.jobs) && !r.stopping {
			return r.start(m)
		}
		r.cur = len(r.jobs)
		r.done()
		return nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			if r.running && r.cancel != nil {
				r.stopping = true
				r.cancel()
				r.add(T("\n^C — прерываю…\n", "\n^C — interrupting…\n"))
			}
			return nil
		case "esc", "q", "enter", "backspace":
			if r.running {
				m.flash(T("команда ещё выполняется: ctrl+c — прервать", "the command is still running: ctrl+c interrupts it"), toneWarn)
				return nil
			}
			return m.leaveRun(r)
		case "n":
			if r.finished() && r.failed == 0 && r.next != nil && r.from != nil {
				return m.openNext(r)
			}
		case "e":
			if r.editAgain() {
				return m.reedit(r)
			}
		case "r":
			if r.finished() && r.readOnly {
				return m.rerun(r)
			}
		case "up", "k":
			r.scroll(-1)
		case "down", "j":
			r.scroll(1)
		case "pgup", "ctrl+b":
			r.scroll(-r.page(m))
		case "pgdown", "ctrl+f", "space":
			r.scroll(r.page(m))
		case "home", "g":
			r.follow, r.offset = false, 0
		case "end", "G":
			r.follow = true
		}
	}
	return nil
}

func (r *runScreen) page(m *model) int { return max(1, m.bodyHeight()-6) }

// scroll moves the top line of the view; reaching the end follows the
// output again.
func (r *runScreen) scroll(d int) {
	n := len(r.output())
	if r.follow {
		r.offset = max(0, n-1)
		r.follow = false
		if d > 0 {
			r.follow = true
			return
		}
	}
	r.offset = min(max(0, r.offset+d), max(0, n-1))
	if d > 0 && r.offset >= n-1 {
		r.follow = true
	}
}

// window wraps only the lines on screen: from the top line down, or, when
// following, from the last line up — a long output is never wrapped whole.
// The output keeps its spaces: mp prints tables aligned with them.
func (r *runScreen) window(w, h int) []string {
	out := r.output()
	if len(out) == 0 {
		return nil
	}
	if r.follow {
		var rev []string
		for i := len(out) - 1; i >= 0 && len(rev) < h; i-- {
			wl := wrapKeep(out[i], w)
			for j := len(wl) - 1; j >= 0 && len(rev) < h; j-- {
				rev = append(rev, " "+wl[j])
			}
		}
		for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
			rev[i], rev[j] = rev[j], rev[i]
		}
		return rev
	}
	var body []string
	for i := min(r.offset, len(out)-1); i < len(out) && len(body) < h; i++ {
		for _, l := range wrapKeep(out[i], w) {
			if len(body) == h {
				break
			}
			body = append(body, " "+l)
		}
	}
	return body
}

func (r *runScreen) hints(m *model) []hint {
	if r.running {
		return []hint{{"ctrl+c", T("прервать", "interrupt")}, {"↑/↓", T("прокрутка", "scroll")}}
	}
	h := []hint{{"esc", T("назад", "back")}, {"↑/↓ pgup/pgdn", T("прокрутка", "scroll")}}
	if r.failed == 0 && r.next != nil && r.from != nil {
		h = append([]hint{{"n", r.next.labelFor(findCmd(m.root, r.next.path))}}, h...)
	}
	if r.editAgain() {
		h = append([]hint{{"e", T("править снова", "edit again")}}, h...)
	}
	if r.readOnly {
		h = append(h, hint{"r", T("ещё раз", "run again")})
	}
	return h
}

func (r *runScreen) view(m *model, w, h int) string {
	var head []string
	shown := ""
	if len(r.jobs) == 1 {
		shown = "$ " + r.jobs[0].shown
	} else {
		shown = fmt.Sprintf(T("$ %d команд(ы) подряд", "$ %d commands in a row"), len(r.jobs))
	}
	for i, l := range wrap(shown, w-2) {
		if i == 2 {
			break
		}
		head = append(head, " "+styleKey.Render(l))
	}
	head = append(head, styleMuted.Render(strings.Repeat("─", max(0, w))))

	var status string
	switch {
	case r.running:
		status = styleWarn.Render(fmt.Sprintf(T("⟳ выполняется… %d с", "⟳ running… %d s"), int(time.Since(r.started).Seconds())))
	case r.failed == 0:
		status = styleKey.Render(fmt.Sprintf(T("✓ готово за %.1f с (код 0)", "✓ done in %.1f s (exit 0)"), r.took.Seconds()))
	default:
		status = styleErr.Render(fmt.Sprintf(T("✗ код выхода %d", "✗ exit status %d"), r.code) + exitMeaning(r.code, r.err))
		if len(r.jobs) > 1 {
			status += styleErr.Render(fmt.Sprintf(T(" · с ошибкой: %d из %d", " · failed: %d of %d"), r.failed, len(r.jobs)))
		}
	}
	foot := []string{styleMuted.Render(strings.Repeat("─", max(0, w))), " " + status}

	avail := max(1, h-len(head)-len(foot))
	body := r.window(max(10, w-2), avail)
	if len(body) == 0 && !r.running {
		body = append(body, " "+styleMuted.Render(T("(команда ничего не вывела)", "(the command printed nothing)")))
	}
	for len(body) < avail {
		body = append(body, "")
	}
	return strings.Join(append(append(head, body...), foot...), "\n")
}

// exitMeaning explains mp's exit codes.
func exitMeaning(code int, err error) string {
	switch {
	case err != nil:
		return ": " + err.Error()
	case code == 2:
		return T(" — неверный запрос или аргументы", " — bad request or arguments")
	case code == 3:
		return T(" — задача или проверка завершилась с ошибкой", " — the job or a check failed")
	case code == 4:
		return T(" — нет доступа", " — access denied")
	}
	return ""
}

// collect waits for a child to finish and returns all its output.
func collect(ch <-chan runEvent) (string, int, error) {
	var b strings.Builder
	for ev := range ch {
		b.WriteString(ev.out)
		if ev.done {
			return b.String(), ev.code, ev.err
		}
	}
	return b.String(), -1, errors.New("no exit status")
}

// The edit flow: fetch the file (mp files get, or the site's nginx
// directives), open the editor in the terminal, and put it back with mp
// files put / mp site nginx --set when it changed.

// removeTemp drops the editor's copy; a leftover in the temp directory is
// no reason to fail what the user did.
func removeTemp(dir string) { _ = os.RemoveAll(dir) }

// The editor's copy lives in a private (0700) directory named after the
// TUI that made it: mp-edit-<pid>-<random>. A TUI killed while the editor
// was open or the copy was being saved leaves it behind; the next start
// can tell such a leftover from the copy another running TUI edits now.
const editDirPrefix = "mp-edit-"

// staleEdit is how long a leftover copy is kept for the user to recover.
const staleEdit = 24 * time.Hour

func newEditDir() (string, error) {
	return os.MkdirTemp("", fmt.Sprintf("%s%d-", editDirPrefix, os.Getpid()))
}

// editDirOwner is the PID in an edit directory's name, 0 when it has none
// (made by an older mp).
func editDirOwner(name string) int {
	pid, _, ok := strings.Cut(strings.TrimPrefix(name, editDirPrefix), "-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// alive says whether the TUI that made an edit directory still runs: a
// process of this user with that PID, running this binary. A PID taken over
// by another program (or another user's) means the TUI is gone.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return true // no way to tell: better kept than lost
	}
	self, err := os.Executable()
	return err != nil || sameBinary(exe, self)
}

// sameBinary compares a process's /proc/<pid>/exe with this binary. A TUI
// started before an update runs the replaced file, which the kernel names
// "<path> (deleted)": it is still a TUI of this mp, and its copy is in use.
func sameBinary(exe, self string) bool {
	return strings.TrimSuffix(exe, " (deleted)") == self
}

// sweepEdits looks for edit directories that earlier runs of this user
// left in dir: it removes those untouched for a day (or empty) and returns
// the files of the younger ones, so the menu can say where an edit that may
// not have been saved is. Directories of a TUI still running are left
// alone, and so is anything not made by this user's TUI.
func sweepEdits(dir string, now time.Time) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var kept []string
	for _, e := range entries {
		// The type comes from the directory itself: a symlink is not a dir.
		if !e.IsDir() || !strings.HasPrefix(e.Name(), editDirPrefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
			continue
		}
		if pid := editDirOwner(e.Name()); pid > 0 && pid != os.Getpid() && alive(pid) {
			continue
		}
		newest := fi.ModTime()
		var files []string
		if inner, err := os.ReadDir(path); err == nil {
			for _, f := range inner {
				info, err := f.Info()
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				if info.ModTime().After(newest) {
					newest = info.ModTime()
				}
				if !editorJunk(f.Name()) {
					files = append(files, filepath.Join(path, f.Name()))
				}
			}
		}
		if len(files) == 0 || now.Sub(newest) > staleEdit {
			removeTemp(path)
			continue
		}
		kept = append(kept, files...)
	}
	return kept
}

// editorJunk is what an editor keeps next to the file it edits — a swap
// file, a backup, an autosave: not worth naming as a kept copy.
func editorJunk(name string) bool {
	switch {
	case strings.HasSuffix(name, "~"):
		return true
	case strings.HasPrefix(name, ".") && (strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, ".swo") || strings.HasSuffix(name, ".swx")):
		return true
	case len(name) > 1 && strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#"):
		return true
	}
	return false
}

type editState struct {
	kind   string // file or nginx
	dir    string
	local  string
	remote string // the file in the account's home, or the domain
	user   string
	before []byte
}

type editFetchedMsg struct {
	st  *editState
	id  int // the fetch in model.running
	out string
	err error
}

type editedMsg struct {
	st  *editState
	err error
}

func (m *model) startEdit(a *act, v map[string]string) tea.Cmd {
	dir, err := newEditDir()
	if err != nil {
		m.flash(err.Error(), toneErr)
		return nil
	}
	st := &editState{kind: a.edit, dir: dir}
	switch a.edit {
	case "nginx":
		st.remote = v["domain"]
		st.local = filepath.Join(dir, "custom.conf")
		m.flash(fmt.Sprintf(T("читаю nginx-директивы %s…", "reading the nginx directives of %s…"), st.remote), toneMuted)
		cl := m.cl
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
			defer cancel()
			n, err := cl.SiteNginx(ctx, st.remote)
			if err == nil {
				err = os.WriteFile(st.local, []byte(n.Custom), 0o600)
			}
			return editFetchedMsg{st: st, err: err}
		}
	default:
		st.remote, st.user = v["path"], v["user"]
		st.local = filepath.Join(dir, filepath.Base(st.remote))
		job := m.filesJob("get", st.remote, st.local, st.user)
		ch, cancel, err := m.runner.start(job, childEnv(os.Environ(), m.opts))
		if err != nil {
			removeTemp(dir)
			m.flash(err.Error(), toneErr)
			return nil
		}
		// Quitting the TUI stops the download too.
		id := m.nextID()
		m.running[id] = cancel
		m.flash(fmt.Sprintf(T("скачиваю %s…", "downloading %s…"), st.remote), toneMuted)
		return func() tea.Msg {
			out, code, err := collect(ch)
			if err == nil && code != 0 {
				err = errors.New(strings.TrimSpace(out))
			}
			return editFetchedMsg{st: st, id: id, out: out, err: err}
		}
	}
}

// filesJob builds mp files get|put with the owner when it is not the caller.
func (m *model) filesJob(verb, a, b, user string) runJob {
	argv := []string{"files", verb, a, b}
	if user != "" && m.admin() {
		argv = append(argv, "--user="+user)
	}
	shown := "mp files " + verb + " " + shellQuote(a) + " " + shellQuote(b)
	if user != "" && m.admin() {
		shown += " --user " + shellQuote(user)
	}
	return runJob{argv: argv, shown: shown}
}

func (m *model) editFetched(msg editFetchedMsg) tea.Cmd {
	st := msg.st
	delete(m.running, msg.id)
	if msg.err != nil {
		removeTemp(st.dir)
		m.flash(T("не удалось получить файл: ", "could not fetch the file: ")+firstLine(msg.err.Error(), ""), toneErr)
		return nil
	}
	m.flashText = ""
	st.before, _ = os.ReadFile(st.local)
	c := m.runner.editor(st.local, os.Environ())
	return tea.ExecProcess(c, func(err error) tea.Msg { return editedMsg{st: st, err: err} })
}

func (m *model) edited(msg editedMsg) tea.Cmd {
	st := msg.st
	cleanup := func() { removeTemp(st.dir) }
	if msg.err != nil {
		cleanup()
		m.flash(T("редактор завершился с ошибкой: ", "the editor failed: ")+msg.err.Error(), toneErr)
		return nil
	}
	after, err := os.ReadFile(st.local)
	if err != nil {
		cleanup()
		m.flash(err.Error(), toneErr)
		return nil
	}
	if bytes.Equal(after, st.before) {
		cleanup()
		m.flash(T("файл не изменён — ничего не записано", "the file is unchanged — nothing written"), toneMuted)
		return nil
	}
	var job runJob
	if st.kind == "nginx" {
		job = runJob{argv: []string{"site", "nginx", st.remote, "--set=" + st.local}, shown: "mp site nginx " + shellQuote(st.remote) + " --set " + shellQuote(st.local)}
	} else {
		job = m.filesJob("put", st.local, st.remote, st.user)
	}
	rs := newRunScreen(T("Сохранение", "Saving"), []runJob{job})
	rs.cleanup, rs.edit = cleanup, st
	return m.push(rs)
}

// reedit opens the editor again on the copy that failed to save; leaving
// the editor saves it once more (or drops it when it is back to what the
// panel has).
func (m *model) reedit(r *runScreen) tea.Cmd {
	if m.top() != r {
		return nil
	}
	m.stack = m.stack[:len(m.stack)-1]
	st := r.edit
	c := m.runner.editor(st.local, os.Environ())
	return tea.ExecProcess(c, func(err error) tea.Msg { return editedMsg{st: st, err: err} })
}

type termDoneMsg struct {
	shown string
	err   error
}
