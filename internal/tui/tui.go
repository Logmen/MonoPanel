// Package tui is the interactive menu shown by `mp` without arguments. It is
// a client of the same API as the Web UI and the CLI: lists read typed data
// through internal/client, and every action is an mp command run as a child
// process of this binary, with a form generated from the command's flags.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"monopanel/internal/apitypes"
	"monopanel/internal/client"
	"monopanel/internal/i18n"
	"monopanel/internal/store"
)

// T is the text in the language of the terminal (the CLI detects it before
// the menu starts), so texts live in functions, not in package-level vars.
func T(ru, en string) string { return i18n.T(ru, en) }

// Options carry how the TUI was started, so the commands it runs reach the
// same panel with the same credentials: locally over the socket, or remotely
// with --server and a token (passed to the child through MP_SERVER/MP_TOKEN,
// never on its command line).
type Options struct {
	Server   string
	Token    string
	Insecure bool
	Config   string
}

// Remote says the TUI talks to another machine: commands that act on the local
// host (config --local, update trust, setup) make no sense there.
func (o Options) Remote() bool { return o.Server != "" }

const (
	loadTimeout   = 30 * time.Second
	headerRefresh = 15 * time.Second
	flashFor      = 8 * time.Second
)

// Run starts the TUI. root is the mp command tree: every action of the menu is
// one of its commands, run as a child process of this binary.
func Run(ctx context.Context, cl *client.Client, root *cobra.Command, opts Options) error {
	m := newModel(cl, root, opts, newExecRunner())
	// A run killed while it saved an edited file leaves the copy behind:
	// old ones go, recent ones are named on the main menu.
	m.leftovers = sweepEdits(os.TempDir(), time.Now())
	m.refreshMenu()
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	m.stopAll()
	return err
}

type model struct {
	cl     *client.Client
	root   *cobra.Command
	opts   Options
	runner runner

	me        *apitypes.Principal
	meErr     error
	status    *apitypes.SystemStatus
	statusErr error
	php       []string // the installed PHP branches: the choices of --php in site forms
	leftovers []string // edited copies an earlier run left behind (it was killed while saving)

	stack   []screen
	overlay overlay
	width   int
	height  int
	seq     int
	running map[int]func()

	flashText string
	flashTone tone
	flashAt   time.Time

	tick    func() tea.Cmd       // the header's refresh timer (tests stop it)
	runTick func(id int) tea.Cmd // the output view's clock
}

func newModel(cl *client.Client, root *cobra.Command, opts Options, r runner) *model {
	m := &model{cl: cl, root: root, opts: opts, runner: r, running: map[int]func(){}, width: 80, height: 24, tick: tick, runTick: runTick}
	m.stack = []screen{m.mainMenu()}
	m.stack[0].base().id = m.nextID()
	return m
}

func (m *model) nextID() int {
	m.seq++
	return m.seq
}

// admin: an unknown role (the API could not say) gets the full menus —
// the API refuses what the caller may not do, and a root on a broken panel
// still reaches the local recovery commands.
func (m *model) admin() bool { return m.me == nil || m.me.Role == "admin" }

func (m *model) env() formEnv { return formEnv{remote: m.opts.Remote(), admin: m.admin(), php: m.php} }

// refreshMenu builds the main menu again (the role is known now) where the
// old one stood.
func (m *model) refreshMenu() {
	menu := m.mainMenu()
	if old, ok := m.stack[0].(*hubScreen); ok {
		menu.id, menu.cursor = old.id, old.cursor
	}
	m.stack[0] = menu
}

func (m *model) stopAll() {
	for _, cancel := range m.running {
		cancel()
	}
	// An edited copy kept for another try goes with the TUI.
	for _, s := range m.stack {
		if r, ok := s.(*runScreen); ok && !r.running {
			r.drop()
		}
	}
}

type statusMsg struct {
	s   *apitypes.SystemStatus
	err error
}

type meMsg struct {
	p   *apitypes.Principal
	err error
}

type tickMsg struct{}

type phpMsg struct {
	versions []string
	err      error
}

// fetchPHP reads the installed PHP branches, as the web UI's site forms do
// each time they open (ordinary accounts may read them too).
func (m *model) fetchPHP() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		v, err := cl.PHPVersions(ctx)
		if err != nil {
			return phpMsg{err: err}
		}
		out := []string{}
		for _, p := range v.Installed {
			if p.Status == store.PHPInstalled {
				out = append(out, p.Version)
			}
		}
		return phpMsg{versions: out}
	}
}

func (m *model) fetchStatus() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		s, err := cl.Status(ctx)
		return statusMsg{s, err}
	}
}

func (m *model) fetchMe() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		p, err := cl.Me(ctx)
		return meMsg{p, err}
	}
}

func tick() tea.Cmd { return tea.Tick(headerRefresh, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.fetchStatus(), m.fetchMe(), m.tick(), m.loadTop())
}

func (m *model) top() screen { return m.stack[len(m.stack)-1] }

func (m *model) loadTop() tea.Cmd {
	if l, ok := m.top().(loader); ok {
		return l.load(m)
	}
	return nil
}

// push opens a screen on top; it inherits the area of the one below.
func (m *model) push(s screen) tea.Cmd {
	b := s.base()
	b.id = m.nextID()
	if b.area == nil {
		b.area = m.top().base().area
	}
	m.stack = append(m.stack, s)
	switch x := s.(type) {
	case *runScreen:
		return x.start(m)
	case loader:
		return x.load(m)
	case *form:
		if x.phpField() != nil {
			// The branches installed right now; the form shows the last
			// list it knows until they come.
			return m.fetchPHP()
		}
	}
	return nil
}

// pop closes the top screen; the one below reads its data again, since the
// action just taken may have changed it.
func (m *model) pop() tea.Cmd {
	if len(m.stack) <= 1 {
		return nil
	}
	m.stack = m.stack[:len(m.stack)-1]
	return m.loadTop()
}

func (m *model) onStack(s screen) bool {
	for _, x := range m.stack {
		if x == s {
			return true
		}
	}
	return false
}

func (m *model) find(id int) screen {
	for _, s := range m.stack {
		if s.base().id == id {
			return s
		}
	}
	return nil
}

func (m *model) flash(text string, t tone) {
	m.flashText, m.flashTone, m.flashAt = text, t, time.Now()
}

// bodyHeight is what the screens get: the terminal minus the header,
// the breadcrumbs, the message line and the footer.
func (m *model) bodyHeight() int { return max(3, m.height-4) }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case statusMsg:
		m.status, m.statusErr = msg.s, msg.err
		if msg.err != nil {
			m.status = nil
		}
		return m, nil
	case meMsg:
		m.me, m.meErr = msg.p, msg.err
		if msg.err != nil {
			m.me = nil
		}
		m.refreshMenu()
		return m, nil
	case phpMsg:
		// A failed read keeps what was known: the field is free text then.
		if msg.err == nil {
			m.php = msg.versions
			for _, s := range m.stack {
				if f, ok := s.(*form); ok {
					f.refreshPHP(m.php)
				}
			}
		}
		return m, nil
	case tickMsg:
		if !m.flashAt.IsZero() && time.Since(m.flashAt) > flashFor {
			m.flashText = ""
		}
		return m, tea.Batch(m.fetchStatus(), m.tick())
	case loadedMsg:
		if s := m.find(msg.id); s != nil {
			return m, s.update(m, msg)
		}
		return m, nil
	case runEventMsg:
		if s := m.find(msg.id); s != nil {
			return m, s.update(m, msg)
		}
		return m, nil
	case runTickMsg:
		if s := m.find(msg.id); s != nil {
			return m, s.update(m, msg)
		}
		return m, nil
	case termDoneMsg:
		if msg.err != nil {
			m.flash(fmt.Sprintf(T("%s — ошибка: %v", "%s — error: %v"), msg.shown, msg.err), toneErr)
		} else {
			m.flash(fmt.Sprintf(T("%s — готово", "%s — done"), msg.shown), toneOK)
		}
		return m, m.loadTop()
	case runManyMsg:
		if msg.err != nil {
			m.flash(msg.err.Error(), toneErr)
			return m, nil
		}
		if msg.ask != nil && len(msg.jobs) > 0 {
			title, jobs := msg.title, msg.jobs
			m.overlay = &confirmOverlay{spec: msg.ask, ok: func(m *model) tea.Cmd { return m.runMany(title, jobs) }}
			return m, nil
		}
		return m, m.runMany(msg.title, msg.jobs)
	case editFetchedMsg:
		return m, m.editFetched(msg)
	case editedMsg:
		return m, m.edited(msg)
	case tea.PasteMsg:
		if m.overlay != nil {
			return m, m.overlay.update(m, msg)
		}
		return m, m.top().update(m, msg)
	case tea.KeyPressMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		if rs, ok := m.top().(*runScreen); ok && rs.running && m.overlay == nil {
			return rs.update(m, msg)
		}
		m.stopAll()
		return tea.Quit
	}
	if m.overlay != nil {
		if k == "f1" && !m.overlay.typing() {
			m.overlay = nil
			return nil
		}
		return m.overlay.update(m, msg)
	}
	top := m.top()
	if k == "f1" {
		if f, ok := top.(*form); ok {
			m.overlay = &cmdHelp{f: f}
			return nil
		}
		m.overlay = newHelp(m)
		return nil
	}
	if top.typing() {
		return top.update(m, msg)
	}
	_, isRun := top.(*runScreen)
	switch k {
	case "?":
		m.overlay = newHelp(m)
		return nil
	case ":":
		p := newPalette()
		p.suggest(m)
		m.overlay = p
		return nil
	case "r":
		if len(m.stack) == 1 {
			return tea.Batch(m.fetchStatus(), m.fetchMe())
		}
		if l, ok := top.(loader); ok {
			return tea.Batch(l.load(m), m.fetchStatus())
		}
	case "q", "esc":
		if len(m.stack) == 1 {
			m.stopAll()
			return tea.Quit
		}
		if !isRun && k == "q" {
			return m.pop()
		}
	}
	return top.update(m, msg)
}

// trigger starts an action with the values bound from the screen and row:
// a form, or the command itself when it is quick and nothing is missing.
func (m *model) trigger(a *act, vals map[string]string) tea.Cmd {
	c := findCmd(m.root, a.path)
	if c == nil || !offered(c, m.opts) {
		m.flash(fmt.Sprintf(T("команда недоступна: mp %s", "command not available: mp %s"), a.path), toneWarn)
		return nil
	}
	if a.run != nil {
		return a.run(m, vals)
	}
	if a.edit != "" {
		return m.startEdit(a, vals)
	}
	f := newForm(c, m.env(), a, vals)
	f.area = m.top().base().area
	if a.quick && f.complete() {
		return m.submit(f)
	}
	return m.push(f)
}

// submit checks the form, asks when the command destroys something and runs
// it.
func (m *model) submit(f *form) tea.Cmd {
	if f.act.apply != nil {
		return f.act.apply(m, f)
	}
	argv, shown, stdin, err := f.build()
	if err == nil && f.act.change && !f.changed() {
		err = tuiError(T("ничего не изменено", "nothing changed"))
	}
	if err != nil {
		f.err = err.Error()
		if !m.onStack(f) {
			return m.push(f)
		}
		return nil
	}
	job := runJob{argv: argv, shown: "mp " + strings.Join(shown, " "), stdin: stdin}
	if spec := confirmFor(f); spec != nil {
		m.overlay = &confirmOverlay{spec: spec, ok: func(m *model) tea.Cmd { return m.launch(f, job) }}
		return nil
	}
	return m.launch(f, job)
}

func (m *model) launch(f *form, job runJob) tea.Cmd {
	if f.yes {
		// The TUI has asked already; the command must not ask again on a pipe.
		at := len(job.argv)
		for i, a := range job.argv {
			if a == "--" {
				at = i
				break
			}
		}
		job.argv = append(job.argv[:at], append([]string{"--yes"}, job.argv[at:]...)...)
		job.shown += " --yes"
	}
	if f.act.tty {
		c := m.runner.terminal(job, childEnv(os.Environ(), m.opts))
		shown := job.shown
		return tea.ExecProcess(c, func(err error) tea.Msg { return termDoneMsg{shown: shown, err: err} })
	}
	title := f.title
	if m.onStack(f) {
		title = T("вывод", "output")
	}
	rs := newRunScreen(title, []runJob{job})
	rs.area = f.area
	if m.onStack(f) {
		rs.from = f
	}
	rs.readOnly = readOnly(f.path)
	// Deleting the object of the page the action came from: going back
	// lands on the list, not on a page of something that is gone.
	below := m.top()
	if below == f && len(m.stack) > 1 {
		below = m.stack[len(m.stack)-2]
	}
	if h, ok := below.(*hubScreen); ok && len(h.ctx) > 0 && len(m.stack) > 1 {
		switch f.cmd.Name() {
		case "rm", "remove", "delete":
			rs.gone = true
		}
	}
	if f.act.next != "" {
		if c := findCmd(m.root, f.act.next); c != nil && offered(c, m.opts) {
			rs.next = &act{path: f.act.next}
			rs.from = f
		}
	}
	return m.push(rs)
}

// runMany runs several commands in one output view (fix every site).
func (m *model) runMany(title string, jobs []runJob) tea.Cmd {
	if len(jobs) == 0 {
		m.flash(T("нечего выполнять", "nothing to run"), toneMuted)
		return nil
	}
	return m.push(newRunScreen(title, jobs))
}

// leaveRun goes back from the output: past the form too when it worked, to
// the form when it did not (to fix a value and try again).
func (m *model) leaveRun(r *runScreen) tea.Cmd {
	if m.top() != r {
		return nil
	}
	r.drop()
	m.stack = m.stack[:len(m.stack)-1]
	if r.from != nil && r.failed == 0 && len(m.stack) > 1 && m.top() == r.from {
		m.stack = m.stack[:len(m.stack)-1]
	}
	if r.gone && r.failed == 0 && len(m.stack) > 1 {
		if h, ok := m.top().(*hubScreen); ok && len(h.ctx) > 0 {
			m.stack = m.stack[:len(m.stack)-1]
		}
	}
	return tea.Batch(m.loadTop(), m.fetchStatus())
}

// openNext offers the follow-up command (migrate plan → migrate run) with
// the values of the form that just ran.
func (m *model) openNext(r *runScreen) tea.Cmd {
	c := findCmd(m.root, r.next.path)
	if c == nil {
		return nil
	}
	f := newForm(c, m.env(), r.next, r.from.vals)
	f.carry(r.from)
	f.area = r.area
	m.stack = m.stack[:len(m.stack)-1]
	if m.top() == r.from && len(m.stack) > 1 {
		m.stack = m.stack[:len(m.stack)-1]
	}
	return m.push(f)
}

func (m *model) rerun(r *runScreen) tea.Cmd {
	nr := newRunScreen(r.title, r.jobs)
	nr.readOnly, nr.from, nr.area = r.readOnly, r.from, r.area
	m.stack = m.stack[:len(m.stack)-1]
	return m.push(nr)
}

// readOnly commands may be run again from their output with r.
func readOnly(path string) bool {
	words := strings.Fields(path)
	if len(words) == 0 {
		return false
	}
	switch words[len(words)-1] {
	case "list", "show", "status", "logs", "dns", "types", "presets", "ls", "size", "templates", "metrics", "doctor", "audit", "wait", "whoami":
		return true
	}
	switch path {
	case "db engine", "db config", "php ini", "ssl panel", "web tls", "update", "site php", "config show":
		return true
	}
	return false
}

// openFilter edits a list's query with the form of the command that lists
// the same thing (job list, audit, metrics): the values go to the list, no
// command runs.
func (m *model) openFilter(s *listScreen) tea.Cmd {
	c := findCmd(m.root, s.filterCmd)
	if c == nil {
		return nil
	}
	// The query is the list's state: shown as flags, so the line is the
	// mp command that lists the same.
	a := &act{path: s.filterCmd, flags: map[string]string{}, label: T("Фильтр", "Filter")}
	for k := range s.query {
		a.flags[k] = k
	}
	a.apply = func(m *model, f *form) tea.Cmd {
		for _, fd := range f.fields {
			if !fd.arg && fd.stdin == "" {
				s.query[fd.name] = strings.TrimSpace(fd.value())
			}
		}
		m.stack = m.stack[:len(m.stack)-1]
		return s.load(m)
	}
	f := newForm(c, m.env(), a, s.query)
	f.area = s.area
	return m.push(f)
}

// runLine runs a command line typed in the palette, or opens its form when
// something is missing.
func (m *model) runLine(p *palette, line string) tea.Cmd {
	words, err := splitWords(line)
	if err != nil {
		p.err = T("незакрытая кавычка", "unclosed quote")
		return nil
	}
	if len(words) > 0 && (words[0] == "mp" || words[0] == "monopanel") {
		words = words[1:]
	}
	if len(words) == 0 {
		m.overlay = nil
		return nil
	}
	c, rest, err := m.root.Find(words)
	if err != nil || c == m.root {
		p.err = fmt.Sprintf(T("нет такой команды: %s", "no such command: %s"), words[0])
		return nil
	}
	if !offered(c, m.opts) {
		p.err = fmt.Sprintf(T("mp %s не запускается из меню", "mp %s cannot be run from the menu"), cmdPath(c))
		if m.opts.Remote() && localOnly(cmdPath(c)) {
			p.err = fmt.Sprintf(T("mp %s меняет этот сервер напрямую — только локально, на самом сервере", "mp %s changes the server directly — only locally, on the server itself"), cmdPath(c))
		}
		return nil
	}
	if !c.Runnable() {
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			// cobra stops at the group: "site nosuch" is not "site".
			p.err = fmt.Sprintf(T("нет такой команды: %s", "no such command: %s"), cmdPath(c)+" "+rest[0])
			return nil
		}
		m.overlay = nil
		return m.push(commandsScreen(m, cmdPath(c), runnables(c, m.opts)))
	}
	f := newForm(c, m.env(), &act{path: cmdPath(c)}, nil)
	f.area = m.top().base().area
	ready, err := f.fill(rest)
	if err != nil {
		p.err = err.Error()
		return nil
	}
	m.overlay = nil
	if !ready {
		return m.push(f)
	}
	if _, _, _, err := f.build(); err != nil {
		f.err = err.Error()
		return m.push(f)
	}
	return m.submit(f)
}

func (m *model) View() tea.View {
	w, h := max(20, m.width), max(6, m.height)
	var lines []string
	lines = append(lines, m.header(w))
	lines = append(lines, m.crumbs(w))
	bh := m.bodyHeight()
	var body string
	if m.overlay != nil {
		body = m.overlay.view(m, w, bh)
	} else {
		body = m.top().view(m, w, bh)
	}
	bl := strings.Split(body, "\n")
	if len(bl) > bh {
		bl = bl[:bh]
	}
	for len(bl) < bh {
		bl = append(bl, "")
	}
	lines = append(lines, bl...)
	lines = append(lines, m.flashLine(w))
	lines = append(lines, m.footer(w))
	for i, l := range lines {
		// Text from the server (names, comments, job messages, a login a
		// stranger typed at the sign-in) must not drive the terminal: only
		// the colours the interface itself writes stay.
		lines[i] = clip(safeStyled(l), w)
	}
	v := tea.NewView(strings.Join(lines[:min(len(lines), h)], "\n"))
	v.AltScreen = true
	v.WindowTitle = "MonoPanel"
	if m.status != nil && m.status.Host != nil {
		// A BEL or ST in the host name would end the title sequence early.
		v.WindowTitle = "MonoPanel — " + strings.ReplaceAll(sanitize(m.status.Host.Hostname), "\n", " ")
	}
	return v
}

// header: the version, the host and its load and memory, who is signed in;
// and the warnings that matter at a glance.
func (m *model) header(w int) string {
	left := "MonoPanel"
	if m.status != nil {
		left += " v" + m.status.Panel.Version
	}
	var mid string
	midTone := toneMuted
	switch {
	case m.statusErr != nil:
		// The client says "panel unreachable: …" itself, in English.
		mid, midTone = T("панель недоступна: ", "panel unreachable: ")+strings.TrimPrefix(firstLine(m.statusErr.Error(), ""), "panel unreachable: "), toneErr
	case m.status != nil && !m.status.Panel.AgentOK:
		// The server sends no host facts exactly when the agent is down, so
		// this must not depend on them.
		mid, midTone = T("агент недоступен", "agent unavailable"), toneWarn
		if e := m.status.Panel.AgentError; e != "" {
			mid += ": " + firstLine(e, "")
		}
	case m.status != nil && m.status.Host != nil:
		hst := m.status.Host
		used := float64(hst.MemTotalBytes-hst.MemAvailableBytes) / (1 << 30)
		total := float64(hst.MemTotalBytes) / (1 << 30)
		mid = fmt.Sprintf(T("%s · load %.2f %.2f %.2f · RAM %.1f/%.1f ГБ", "%s · load %.2f %.2f %.2f · RAM %.1f/%.1f GB"), hst.Hostname, hst.Load[0], hst.Load[1], hst.Load[2], used, total)
	}
	// Who and where, shortened until the host facts fit: login · role @
	// server, then login · role, then the login alone.
	var rights []string
	if m.me != nil {
		who := m.me.Login + " · " + m.me.Role
		if m.opts.Remote() {
			host := strings.TrimPrefix(strings.TrimPrefix(m.opts.Server, "https://"), "http://")
			rights = append(rights, who+" @ "+host)
		}
		rights = append(rights, who, m.me.Login)
	}
	rights = append(rights, "")
	right := ""
	for _, r := range rights {
		right = r
		if 1+textWidth(left)+2+textWidth(mid)+2+textWidth(r) <= w {
			break
		}
	}
	mid = truncate(mid, max(0, w-1-textWidth(left)-2-2-textWidth(right)))
	line := " " + styleTitle.Render(left)
	if mid != "" {
		line += "  " + midTone.render(mid)
	}
	gap := w - textWidth(line) - textWidth(right) - 1
	if right == "" || gap < 1 {
		return line
	}
	return line + strings.Repeat(" ", gap) + styleMuted.Render(right)
}

func (m *model) crumbs(w int) string {
	parts := make([]string, 0, len(m.stack)+1)
	for _, s := range m.stack {
		parts = append(parts, s.name())
	}
	if m.overlay != nil {
		parts = append(parts, m.overlay.name())
	}
	line := strings.Join(parts, " › ")
	if textWidth(line) > w-2 {
		r := []rune(line)
		for len(r) > 0 && textWidth(string(r)) > w-3 {
			r = r[1:]
		}
		line = "…" + string(r)
	}
	return " " + styleBold.Render(line)
}

func (m *model) flashLine(w int) string {
	if m.flashText == "" {
		return ""
	}
	return " " + m.flashTone.render(truncate(m.flashText, w-2))
}

func (m *model) footer(w int) string {
	var hs, keep, extra []hint
	if m.overlay != nil {
		hs = m.overlay.hints(m)
	} else {
		top := m.top()
		hs = top.hints(m)
		if !top.typing() {
			// "?" and the way back are always shown; the rest when it fits.
			keep = append(keep, hint{"?", T("все действия", "all actions")})
			if len(m.stack) == 1 {
				keep = append(keep, hint{"q", T("выход", "quit")})
			} else if _, ok := top.(*runScreen); !ok {
				keep = append(keep, hint{"esc", T("назад", "back")})
			}
			extra = append(extra, hint{":", T("команда", "command")})
			if _, ok := top.(loader); ok || len(m.stack) == 1 {
				extra = append(extra, hint{"r", T("обновить", "refresh")})
			}
		}
	}
	render := func(h hint) string { return styleKey.Render(h.key) + " " + styleMuted.Render(h.text) }
	sep := styleMuted.Render(" · ")
	width := func(list []hint) int {
		n := 0
		for i, h := range list {
			if i > 0 {
				n += textWidth(sep)
			}
			n += textWidth(render(h))
		}
		return n
	}
	reserved := width(keep)
	if reserved > 0 {
		reserved += textWidth(sep)
	}
	var out []hint
	used := 1
	for _, h := range hs {
		add := textWidth(render(h))
		if len(out) > 0 {
			add += textWidth(sep)
		}
		if used+add+reserved > w {
			continue
		}
		out = append(out, h)
		used += add
	}
	out = append(out, keep...)
	used = 1 + width(out)
	for _, h := range extra {
		add := textWidth(sep) + textWidth(render(h))
		if used+add > w {
			break
		}
		out = append(out, h)
		used += add
	}
	parts := make([]string, len(out))
	for i, h := range out {
		parts[i] = render(h)
	}
	return " " + strings.Join(parts, sep)
}
