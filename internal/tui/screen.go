package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/client"
)

// The screens: a list (a table of typed API data with actions bound to its
// rows), a hub (some facts and a menu of sub-screens and actions), the form
// and the output of a command.

type hint struct{ key, text string }

type screenBase struct {
	id    int
	title string
	area  *area
}

func (b *screenBase) base() *screenBase { return b }

type screen interface {
	base() *screenBase
	name() string
	typing() bool // keys go to a text input: no global shortcuts
	update(m *model, msg tea.Msg) tea.Cmd
	view(m *model, w, h int) string
	hints(m *model) []hint
}

// loader is a screen that reads data: on start and on r.
type loader interface {
	load(m *model) tea.Cmd
}

// actioner is a screen with actions: listed by the help with what is bound.
type actioner interface {
	actions(m *model) ([]*act, map[string]string)
}

type column struct {
	title string
	min   int
	max   int // 0: as wide as the content
	right bool
	flex  bool // gives up width first (the last column when none is marked)
}

type row struct {
	cells []string
	tones []tone
	vals  map[string]string
	info  []string
}

// result is what a screen loaded: rows for a list, facts and values for
// both kinds.
type result struct {
	rows []row
	info []string
	vals map[string]string
}

type loadedMsg struct {
	id, seq int
	res     *result
	err     error
}

// link opens a sub-screen from a list by a key.
type link struct {
	key   string
	label string
	admin bool
	open  func(m *model) screen
}

type listScreen struct {
	screenBase
	cols      []column
	fetch     func(ctx context.Context, cl *client.Client, q map[string]string) (*result, error)
	ctx       map[string]string
	acts      []*act
	links     []link
	open      func(m *model, r row) screen
	enter     func(m *model, s *listScreen, r row) tea.Cmd
	keyHook   func(m *model, s *listScreen, k string) (bool, tea.Cmd)
	titleFn   func(s *listScreen) string
	empty     string
	query     map[string]string
	filterCmd string // its flags filter the list: job list, audit, metrics
	res       *result
	err       error
	loading   bool
	seq       int
	cursor    int
	offset    int
	filter    string
	finput    textInput
	filtering bool
	focusOn   [2]string // after the next load, put the cursor on the row whose value [0] is [1]
}

func (s *listScreen) name() string {
	if s.titleFn != nil {
		return s.titleFn(s)
	}
	return s.title
}

func (s *listScreen) typing() bool { return s.filtering }

func (s *listScreen) load(m *model) tea.Cmd {
	if s.fetch == nil {
		return nil
	}
	s.seq++
	s.loading = true
	// The fetch runs off the event loop: it gets the client and its query,
	// never the model.
	id, seq, fetch, q, cl := s.id, s.seq, s.fetch, merge(s.query), m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		res, err := fetch(ctx, cl, q)
		return loadedMsg{id: id, seq: seq, res: res, err: err}
	}
}

func (s *listScreen) loaded(msg loadedMsg) {
	if msg.seq != s.seq {
		return
	}
	s.loading = false
	s.res, s.err = msg.res, msg.err
	if s.res == nil {
		s.res = &result{}
	}
	if s.focusOn[0] != "" {
		for i, r := range s.visible() {
			if r.vals[s.focusOn[0]] == s.focusOn[1] {
				s.cursor = i
				break
			}
		}
		s.focusOn = [2]string{}
	}
	if n := len(s.visible()); s.cursor >= n {
		s.cursor = max(0, n-1)
	}
}

// visible are the rows that match the filter.
func (s *listScreen) visible() []row {
	if s.res == nil {
		return nil
	}
	if s.filter == "" {
		return s.res.rows
	}
	needle := strings.ToLower(s.filter)
	var out []row
	for _, r := range s.res.rows {
		if strings.Contains(strings.ToLower(strings.Join(r.cells, " ")), needle) {
			out = append(out, r)
		}
	}
	return out
}

func (s *listScreen) selected() (row, bool) {
	rows := s.visible()
	if s.cursor < 0 || s.cursor >= len(rows) {
		return row{}, false
	}
	return rows[s.cursor], true
}

// values are the bindings for an action: the screen's context, what the
// screen loaded, and the selected row.
func (s *listScreen) values(withRow bool) map[string]string {
	v := merge(s.ctx, s.query)
	if s.res != nil {
		v = merge(v, s.res.vals)
	}
	if withRow {
		if r, ok := s.selected(); ok {
			v = merge(v, r.vals)
		}
	}
	return v
}

func (s *listScreen) actions(m *model) ([]*act, map[string]string) {
	return s.acts, s.values(true)
}

func (s *listScreen) update(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case loadedMsg:
		s.loaded(msg)
	case tea.PasteMsg:
		if s.filtering {
			s.finput.insert(msg.Content)
			s.filter = s.finput.value()
			s.cursor = 0
		}
	case tea.KeyPressMsg:
		k := msg.String()
		if s.filtering {
			switch k {
			case "esc":
				s.filtering, s.filter = false, ""
				s.finput.set("")
			case "enter", "down", "up":
				s.filtering = false
			default:
				s.finput.handle(msg)
				s.filter = s.finput.value()
				s.cursor = 0
			}
			return nil
		}
		if s.keyHook != nil {
			if ok, cmd := s.keyHook(m, s, k); ok {
				return cmd
			}
		}
		n := len(s.visible())
		switch k {
		case "up", "k":
			s.cursor = max(0, s.cursor-1)
			return nil
		case "down", "j":
			s.cursor = min(max(0, n-1), s.cursor+1)
			return nil
		case "pgup":
			s.cursor = max(0, s.cursor-m.bodyHeight()/2)
			return nil
		case "pgdown":
			s.cursor = min(max(0, n-1), s.cursor+m.bodyHeight()/2)
			return nil
		case "home":
			s.cursor = 0
			return nil
		case "end":
			s.cursor = max(0, n-1)
			return nil
		case "/":
			s.filtering = true
			s.finput.set(s.filter)
			return nil
		case "esc":
			if s.filter != "" {
				s.filter = ""
				s.finput.set("")
				return nil
			}
			m.pop()
			return nil
		case "f":
			if s.filterCmd != "" {
				return m.openFilter(s)
			}
		case "enter":
			r, ok := s.selected()
			if !ok {
				return nil
			}
			if s.enter != nil {
				return s.enter(m, s, r)
			}
			if s.open != nil {
				if sc := s.open(m, r); sc != nil {
					return m.push(sc)
				}
				return nil
			}
			return m.push(rowPage(m, s, r))
		}
		for _, l := range s.links {
			if l.key == k && (!l.admin || m.admin()) {
				if sc := l.open(m); sc != nil {
					return m.push(sc)
				}
				return nil
			}
		}
		for _, a := range s.acts {
			if a.key != k {
				continue
			}
			if c := findCmd(m.root, a.path); c == nil || !offered(c, m.opts) {
				// Not in this mp (an older or a newer one): the key is not
				// shown either, and another action may own it.
				continue
			}
			rowBound := a.rowBound(s.values(false))
			if rowBound {
				if _, ok := s.selected(); !ok {
					m.flash(T("сначала выберите строку", "select a row first"), toneWarn)
					return nil
				}
			}
			v := s.values(true)
			if !a.shownFor(v, m.admin()) {
				continue
			}
			return m.trigger(a, v)
		}
	}
	return nil
}

func (s *listScreen) hints(m *model) []hint {
	if s.filtering {
		return []hint{{"enter", T("готово", "done")}, {"esc", T("сбросить", "clear")}}
	}
	var h []hint
	v := s.values(true)
	_, hasRow := s.selected()
	if hasRow && (s.open != nil || s.enter != nil || len(s.acts) > 0) {
		h = append(h, hint{"enter", T("открыть", "open")})
	}
	for _, a := range s.acts {
		if a.key == "" || !a.shownFor(v, m.admin()) || (a.rowBound(s.values(false)) && !hasRow) {
			continue
		}
		if c := findCmd(m.root, a.path); c != nil && offered(c, m.opts) {
			h = append(h, hint{a.key, shortLabel(a, c)})
		}
	}
	for _, l := range s.links {
		if !l.admin || m.admin() {
			h = append(h, hint{l.key, l.label})
		}
	}
	if s.filterCmd != "" {
		h = append(h, hint{"f", T("фильтр", "filter")})
	}
	return append(h, hint{"/", T("поиск", "search")})
}

// shortLabel is the action's name for the footer: a word for the verb, or
// the first words of its own label.
func shortLabel(a *act, c interface{ Name() string }) string {
	if a.label != "" {
		return firstWords(a.label, 2)
	}
	return verbLabel(c.Name())
}

func verbLabel(name string) string {
	switch name {
	case "add", "create":
		return T("создать", "add")
	case "set":
		return T("изменить", "change")
	case "rm", "remove", "delete":
		return T("удалить", "delete")
	case "show":
		return T("подробно", "details")
	case "list", "ls":
		return T("список", "list")
	case "logs":
		return T("журнал", "log")
	case "apply":
		return T("применить", "apply")
	case "fix":
		return T("починить", "fix")
	case "suspend":
		return T("приостановить", "suspend")
	case "unsuspend":
		return T("возобновить", "resume")
	case "start":
		return T("запустить", "start")
	case "stop":
		return T("остановить", "stop")
	case "restart":
		return T("перезапустить", "restart")
	case "reload":
		return T("перечитать", "reload")
	case "reload-or-restart":
		return T("перечитать или перезапустить", "reload or restart")
	case "enable":
		return T("включить", "enable")
	case "disable":
		return T("выключить", "disable")
	case "install":
		return T("установить", "install")
	case "move-ip":
		return T("сменить IP", "move IP")
	case "passwd":
		return T("пароль", "password")
	case "revoke":
		return T("отозвать", "revoke")
	case "renew":
		return T("продлить", "renew")
	case "test":
		return T("проверить", "test")
	case "wait":
		return T("следить", "follow")
	case "status":
		return T("состояние", "status")
	case "restore":
		return T("восстановить", "restore")
	case "run":
		return T("запустить", "run")
	case "allow":
		return T("разрешить", "allow")
	case "deny":
		return T("запретить", "deny")
	case "ban":
		return T("забанить", "ban")
	case "unban":
		return T("разбанить", "unban")
	case "trust":
		return T("доверять", "trust")
	case "untrust":
		return T("не доверять", "untrust")
	case "dkim":
		return T("новый DKIM", "new DKIM")
	case "dns":
		return "DNS"
	case "tls":
		return "TLS"
	case "get":
		return T("скачать", "download")
	case "put":
		return T("загрузить", "upload")
	case "mv":
		return T("переименовать", "rename")
	case "chmod":
		return T("права", "mode")
	case "extract":
		return T("распаковать", "extract")
	case "size":
		return T("размер", "size")
	case "mkdir":
		return T("каталог", "directory")
	case "touch":
		return T("новый файл", "new file")
	case "tune":
		return T("пересчитать", "retune")
	}
	return name
}

// firstWords keeps the first n words of a label for the footer; an English
// article does not count as one ("Change the password", not "Change the").
func firstWords(s string, n int) string {
	var out []string
	counted := 0
	for _, w := range strings.Fields(s) {
		if counted == n {
			break
		}
		out = append(out, w)
		switch strings.ToLower(w) {
		case "the", "a", "an":
		default:
			counted++
		}
	}
	return strings.TrimRight(strings.Join(out, " "), ",:;")
}

func (s *listScreen) view(m *model, w, h int) string {
	var out []string
	if s.res != nil {
		for _, l := range s.res.info {
			for _, wl := range wrap(l, w-2) {
				out = append(out, " "+wl)
			}
		}
		if len(s.res.info) > 0 {
			out = append(out, "")
		}
	}
	if s.query != nil {
		if q := describeQuery(s.query); q != "" {
			out = append(out, " "+styleMuted.Render(truncate(T("фильтр: ", "filter: ")+q, w-2)))
		}
	}
	switch {
	case s.err != nil:
		for _, l := range wrap(T("ошибка: ", "error: ")+s.err.Error(), w-2) {
			out = append(out, " "+styleErr.Render(l))
		}
	case s.res == nil && s.loading:
		out = append(out, " "+styleMuted.Render(T("загрузка…", "loading…")))
	}
	rows := s.visible()
	filterLine := ""
	if s.filtering || s.filter != "" {
		filterLine = " / " + s.finput.view(w-5, s.filtering, false)
	}
	avail := h - len(out) - 1
	if filterLine != "" {
		avail--
	}
	if s.res != nil && len(rows) == 0 && s.err == nil {
		msg := s.empty
		if msg == "" {
			msg = T("пусто", "nothing here")
		}
		if s.filter != "" {
			msg = T("ничего не найдено", "nothing matches")
		}
		out = append(out, " "+styleMuted.Render(msg))
	} else if len(rows) > 0 {
		out = append(out, renderTable(s.cols, rows, w, max(1, avail), s.cursor, &s.offset)...)
	}
	if filterLine != "" {
		for len(out) < h-1 {
			out = append(out, "")
		}
		out = append(out, filterLine)
	}
	return strings.Join(out, "\n")
}

func describeQuery(q map[string]string) string {
	var parts []string
	for _, k := range []string{"status", "limit", "actor", "action", "range"} {
		if v := q[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// renderTable lays the rows out in columns that fit w cells, with a header
// line; offset scrolls to keep the cursor in view.
func renderTable(cols []column, rows []row, w, h, cursor int, offset *int) []string {
	n := len(cols)
	widths := make([]int, n)
	for i, c := range cols {
		widths[i] = textWidth(c.title)
	}
	for _, r := range rows {
		for i := 0; i < n && i < len(r.cells); i++ {
			widths[i] = max(widths[i], textWidth(r.cells[i]))
		}
	}
	for i, c := range cols {
		if c.max > 0 {
			widths[i] = min(widths[i], c.max)
		}
		widths[i] = max(widths[i], min(c.min, widths[i]))
	}
	avail := w - 1 - 2*(n-1)
	total := func() int {
		t := 0
		for _, x := range widths {
			t += x
		}
		return t
	}
	// The flexible column (a message, a command) gives up width first, down
	// to a dozen cells; then the widest of the others, down to its minimum.
	flex := n - 1
	for i, c := range cols {
		if c.flex {
			flex = i
		}
	}
	for total() > avail && widths[flex] > max(8, cols[flex].min) {
		widths[flex]--
	}
	for total() > avail {
		widest := -1
		for i := range widths {
			floor := max(4, cols[i].min)
			if widths[i] > floor && (widest < 0 || widths[i] > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
	}
	// Still too wide: drop columns from the right.
	shown := n
	for shown > 1 && total() > avail {
		shown--
		avail += 2
		widths = widths[:shown]
	}
	line := func(cells []string, tones []tone, plain bool) string {
		parts := make([]string, 0, shown)
		for i := 0; i < shown; i++ {
			c := ""
			if i < len(cells) {
				// A line break in a value would start a row of its own.
				c = strings.ReplaceAll(sanitize(cells[i]), "\n", " ")
			}
			var txt string
			if cols[i].right {
				txt = truncate(c, widths[i])
				txt = strings.Repeat(" ", widths[i]-textWidth(txt)) + txt
			} else {
				txt = fit(c, widths[i])
			}
			if !plain && i < len(tones) {
				txt = tones[i].render(txt)
			}
			parts = append(parts, txt)
		}
		return strings.Join(parts, "  ")
	}
	titles := make([]string, n)
	for i, c := range cols {
		titles[i] = c.title
	}
	out := []string{" " + styleHead.Render(line(titles, nil, true))}
	body := h - 1
	if cursor < *offset {
		*offset = cursor
	}
	if cursor >= *offset+body {
		*offset = cursor - body + 1
	}
	if *offset > max(0, len(rows)-body) {
		*offset = max(0, len(rows)-body)
	}
	for i := *offset; i < len(rows) && i < *offset+body; i++ {
		if i == cursor {
			out = append(out, styleCursor.Render(pad(" "+line(rows[i].cells, nil, true), w)))
		} else {
			out = append(out, " "+line(rows[i].cells, rows[i].tones, false))
		}
	}
	if len(rows) > body {
		more := fmt.Sprintf(" %d/%d", cursor+1, len(rows))
		out[0] = pad(out[0], max(0, w-textWidth(more))) + styleMuted.Render(more)
	}
	return out
}

// rowPage is what Enter opens on a row that has no page of its own: the
// row's values and the actions that apply to it.
func rowPage(m *model, s *listScreen, r row) *hubScreen {
	h := &hubScreen{screenBase: screenBase{title: firstCell(r), area: s.area}, ctx: s.values(false)}
	h.ctx = merge(h.ctx, r.vals)
	for i, c := range s.cols {
		if i < len(r.cells) && r.cells[i] != "" {
			h.info = append(h.info, styleMuted.Render(c.title+": ")+r.cells[i])
		}
	}
	h.info = append(h.info, r.info...)
	for _, a := range s.acts {
		if a.rowBound(s.values(false)) {
			h.entries = append(h.entries, entry{act: a, key: a.key})
		}
	}
	return h
}

func firstCell(r row) string {
	for _, c := range r.cells {
		if c != "" {
			return c
		}
	}
	return "—"
}

// entry is one line of a hub: a sub-screen, an action or a heading.
type entry struct {
	label   string
	key     string
	act     *act
	open    func(m *model) screen
	when    func(v map[string]string) bool
	admin   bool
	section string
}

type hubScreen struct {
	screenBase
	fetch   func(ctx context.Context, cl *client.Client) (*result, error)
	info    []string
	ctx     map[string]string
	entries []entry
	res     *result
	err     error
	loading bool
	seq     int
	cursor  int
	offset  int
}

func (h *hubScreen) name() string { return h.title }
func (h *hubScreen) typing() bool { return false }

func (h *hubScreen) load(m *model) tea.Cmd {
	if h.fetch == nil {
		return nil
	}
	h.seq++
	h.loading = true
	id, seq, fetch, cl := h.id, h.seq, h.fetch, m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		res, err := fetch(ctx, cl)
		return loadedMsg{id: id, seq: seq, res: res, err: err}
	}
}

func (h *hubScreen) values() map[string]string {
	v := merge(h.ctx)
	if h.res != nil {
		v = merge(v, h.res.vals)
	}
	return v
}

// items are the entries that apply now, without the headings.
func (h *hubScreen) items(m *model) []entry {
	v := h.values()
	var out []entry
	for _, e := range h.entries {
		if e.section != "" {
			continue
		}
		if !h.applies(m, e, v) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (h *hubScreen) applies(m *model, e entry, v map[string]string) bool {
	if e.admin && !m.admin() {
		return false
	}
	if e.when != nil && !e.when(v) {
		return false
	}
	if e.act != nil {
		c := findCmd(m.root, e.act.path)
		if c == nil || !offered(c, m.opts) || !e.act.shownFor(v, m.admin()) {
			return false
		}
	}
	return true
}

func (h *hubScreen) actions(m *model) ([]*act, map[string]string) {
	var acts []*act
	for _, e := range h.items(m) {
		if e.act != nil {
			acts = append(acts, e.act)
		}
	}
	return acts, h.values()
}

func (h *hubScreen) label(m *model, e entry) string {
	if e.label != "" {
		return e.label
	}
	if e.act != nil {
		return e.act.labelFor(findCmd(m.root, e.act.path))
	}
	return "?"
}

func (h *hubScreen) update(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case loadedMsg:
		if msg.seq == h.seq {
			h.loading = false
			h.res, h.err = msg.res, msg.err
		}
	case tea.KeyPressMsg:
		items := h.items(m)
		k := msg.String()
		switch k {
		case "up", "k":
			h.cursor = max(0, h.cursor-1)
			return nil
		case "down", "j":
			h.cursor = min(max(0, len(items)-1), h.cursor+1)
			return nil
		case "home":
			h.cursor = 0
			return nil
		case "end":
			h.cursor = max(0, len(items)-1)
			return nil
		case "esc":
			m.pop()
			return nil
		case "enter":
			if h.cursor < len(items) {
				return h.activate(m, items[h.cursor])
			}
			return nil
		}
		for i, e := range items {
			if e.key != "" && e.key == k {
				h.cursor = i
				return h.activate(m, e)
			}
		}
	}
	return nil
}

func (h *hubScreen) activate(m *model, e entry) tea.Cmd {
	if e.open != nil {
		if sc := e.open(m); sc != nil {
			return m.push(sc)
		}
		return nil
	}
	if e.act != nil {
		return m.trigger(e.act, h.values())
	}
	return nil
}

func (h *hubScreen) hints(m *model) []hint {
	out := []hint{{"enter", T("выбрать", "select")}}
	for _, e := range h.items(m) {
		if e.key == "" || e.key == "*" {
			continue
		}
		label := firstWords(e.label, 2)
		if e.label == "" && e.act != nil {
			label = shortLabel(e.act, findCmd(m.root, e.act.path))
		}
		out = append(out, hint{e.key, label})
	}
	return out
}

func (h *hubScreen) view(m *model, w, ht int) string {
	var lines []string
	for _, l := range h.info {
		for _, wl := range wrap(l, w-2) {
			lines = append(lines, " "+wl)
		}
	}
	if h.res != nil {
		for _, l := range h.res.info {
			for _, wl := range wrap(l, w-2) {
				lines = append(lines, " "+wl)
			}
		}
	}
	if h.err != nil {
		for _, l := range wrap(T("ошибка: ", "error: ")+h.err.Error(), w-2) {
			lines = append(lines, " "+styleErr.Render(l))
		}
	} else if h.loading && h.res == nil {
		lines = append(lines, " "+styleMuted.Render(T("загрузка…", "loading…")))
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	head := len(lines)
	items := h.items(m)
	if h.cursor >= len(items) {
		h.cursor = max(0, len(items)-1)
	}
	v := h.values()
	cursorLine := head
	idx := 0
	for i, e := range h.entries {
		if e.section != "" {
			// A heading only over entries that are shown.
			for _, next := range h.entries[i+1:] {
				if next.section != "" {
					break
				}
				if h.applies(m, next, v) {
					lines = append(lines, " "+styleHead.Render(e.section))
					break
				}
			}
			continue
		}
		if !h.applies(m, e, v) {
			continue
		}
		label := h.label(m, e)
		hintText := ""
		if e.act != nil {
			hintText = "mp " + e.act.path
		}
		hintW := min(textWidth(hintText), w/3)
		labelW := max(10, w-8-hintW)
		lt := fit(label, labelW)
		key := fit(e.key, 2)
		if idx == h.cursor {
			cursorLine = len(lines)
			lines = append(lines, styleCursor.Render(" ▶ "+key+" "+lt)+" "+styleMuted.Render(truncate(hintText, hintW)))
		} else {
			lines = append(lines, "   "+styleKey.Render(key)+" "+lt+" "+styleMuted.Render(truncate(hintText, hintW)))
		}
		idx++
	}
	// Scroll: keep the facts on top while the cursor fits below them; when
	// the menu goes on below the screen, its last line says so.
	avail := ht
	if len(lines) > ht {
		avail = max(1, ht-1)
	}
	if cursorLine < h.offset {
		h.offset = cursorLine
	}
	if cursorLine >= h.offset+avail {
		h.offset = cursorLine - avail + 1
	}
	if idx > 0 && h.cursor == 0 && cursorLine < avail {
		h.offset = 0
	}
	if h.offset > max(0, len(lines)-avail) {
		h.offset = max(0, len(lines)-avail)
	}
	end := min(len(lines), h.offset+avail)
	out := append([]string{}, lines[h.offset:end]...)
	if end < len(lines) {
		out = append(out, styleMuted.Render(fmt.Sprintf(T("   ↓ ещё %d", "   ↓ %d more"), len(lines)-end)))
	}
	return strings.Join(out, "\n")
}
