package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Overlays sit on top of the current screen: the help with every action,
// the command palette and the confirmation dialog.

type overlay interface {
	update(m *model, msg tea.Msg) tea.Cmd
	view(m *model, w, h int) string
	hints(m *model) []hint
	name() string
	typing() bool
}

type helpItem struct {
	head  string
	text  string
	act   *act
	vals  map[string]string
	key   string
	label string
	cmd   string
}

// helpOverlay lists the screen's actions with their mp commands, every
// command of the area, and the keys. Enter runs the chosen one.
type helpOverlay struct {
	items  []helpItem
	cursor int
	offset int
}

func newHelp(m *model) *helpOverlay {
	h := &helpOverlay{}
	top := m.top()
	if ac, ok := top.(actioner); ok {
		acts, v := ac.actions(m)
		var sel []helpItem
		for _, a := range acts {
			c := findCmd(m.root, a.path)
			if c == nil || !offered(c, m.opts) || !a.shownFor(v, m.admin()) {
				continue
			}
			sel = append(sel, helpItem{act: a, vals: v, key: a.key, label: a.labelFor(c), cmd: a.template(c, v)})
		}
		if len(sel) > 0 {
			h.items = append(h.items, helpItem{head: T("Действия этого экрана", "This screen's actions")})
			h.items = append(h.items, sel...)
		}
	}
	if ar := top.base().area; ar != nil {
		var all []helpItem
		for _, c := range m.areaCommands(ar) {
			all = append(all, helpItem{act: &act{path: cmdPath(c), quick: readOnly(cmdPath(c))}, vals: map[string]string{}, label: c.Short, cmd: "mp " + useLine(c)})
		}
		if len(all) > 0 {
			h.items = append(h.items, helpItem{head: fmt.Sprintf(T("Все команды раздела «%s»", "All commands of “%s”"), ar.title)})
			h.items = append(h.items, all...)
		}
	}
	h.items = append(h.items, helpItem{head: T("Клавиши", "Keys")})
	for _, l := range []string{
		T("↑/↓ j/k — выбор, enter — открыть, esc/q — назад, ctrl+c — выход", "↑/↓ j/k — move, enter — open, esc/q — back, ctrl+c — quit"),
		T("/ — поиск по списку, f — фильтр (задачи, журнал, метрики), r — обновить", "/ — search the list, f — filter (jobs, audit, metrics), r — refresh"),
		T("? или F1 — эта справка, : — выполнить любую команду mp", "? or F1 — this help, : — run any mp command"),
		T("в форме: tab/↑/↓ — поля, enter — далее, ctrl+s — выполнить, ctrl+p — передать флаг как есть, ctrl+r — показать секрет", "in a form: tab/↑/↓ — fields, enter — next, ctrl+s — run, ctrl+p — pass a flag as it is, ctrl+r — reveal a secret"),
		T("● у поля — оно попадёт в командную строку", "● next to a field — it goes on the command line"),
	} {
		h.items = append(h.items, helpItem{text: l})
	}
	h.cursor = h.next(-1, 1)
	return h
}

func (h *helpOverlay) name() string { return T("Справка", "Help") }
func (h *helpOverlay) typing() bool { return false }

func (h *helpOverlay) selectable(i int) bool {
	return i >= 0 && i < len(h.items) && h.items[i].act != nil
}

// next is the next selectable item from i in direction d (i itself when
// there is none).
func (h *helpOverlay) next(i, d int) int {
	for j := i + d; j >= 0 && j < len(h.items); j += d {
		if h.selectable(j) {
			return j
		}
	}
	if h.selectable(i) {
		return i
	}
	return -1
}

func (h *helpOverlay) update(m *model, msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch k.String() {
	case "esc", "q", "?", "f1":
		m.overlay = nil
	case "up", "k":
		if n := h.next(h.cursor, -1); n >= 0 {
			h.cursor = n
		}
	case "down", "j":
		if n := h.next(h.cursor, 1); n >= 0 {
			h.cursor = n
		}
	case "pgdown":
		for i := 0; i < 10; i++ {
			if n := h.next(h.cursor, 1); n >= 0 {
				h.cursor = n
			}
		}
	case "pgup":
		for i := 0; i < 10; i++ {
			if n := h.next(h.cursor, -1); n >= 0 {
				h.cursor = n
			}
		}
	case "enter":
		if h.selectable(h.cursor) {
			it := h.items[h.cursor]
			m.overlay = nil
			return m.trigger(it.act, it.vals)
		}
	}
	return nil
}

func (h *helpOverlay) hints(m *model) []hint {
	return []hint{{"enter", T("выполнить", "run")}, {"esc", T("закрыть", "close")}}
}

func (h *helpOverlay) view(m *model, w, ht int) string {
	var lines []string
	cur := 0
	for i, it := range h.items {
		switch {
		case it.head != "":
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, " "+styleTitle.Render(it.head))
		case it.text != "":
			for _, l := range wrap(it.text, w-4) {
				lines = append(lines, "   "+styleMuted.Render(l))
			}
		default:
			key := fit(it.key, 2)
			labelW := max(16, (w-8)/2)
			text := " " + key + " " + fit(it.label, labelW) + " "
			cmdW := max(0, w-textWidth(text)-1)
			if i == h.cursor {
				cur = len(lines)
				lines = append(lines, styleCursor.Render(pad(text+truncate(it.cmd, cmdW), w)))
			} else {
				lines = append(lines, " "+styleKey.Render(key)+" "+fit(it.label, labelW)+" "+styleMuted.Render(truncate(it.cmd, cmdW)))
			}
		}
	}
	if cur < h.offset {
		h.offset = max(0, cur-1)
	}
	if cur >= h.offset+ht {
		h.offset = cur - ht + 1
	}
	if h.offset > max(0, len(lines)-ht) {
		h.offset = max(0, len(lines)-ht)
	}
	return strings.Join(lines[h.offset:min(len(lines), h.offset+ht)], "\n")
}

// cmdHelp is the full help of the form's command: its long description and
// every field with its usage, scrolled with the arrows.
type cmdHelp struct {
	f      *form
	offset int
}

func (c *cmdHelp) name() string { return T("Справка по команде", "Command help") }
func (c *cmdHelp) typing() bool { return false }
func (c *cmdHelp) hints(m *model) []hint {
	return []hint{{"↑/↓", T("прокрутка", "scroll")}, {"esc", T("закрыть", "close")}}
}

func (c *cmdHelp) lines(w int) []string {
	cmd := c.f.cmd
	out := []string{" " + styleTitle.Render("mp "+useLine(cmd))}
	text := cmd.Long
	if text == "" {
		text = cmd.Short
	}
	// A long help keeps its layout: its examples are indented command lines.
	for _, l := range wrapKeep(text, w-2) {
		out = append(out, " "+l)
	}
	out = append(out, "")
	labelW := 8
	for _, fd := range c.f.fields {
		labelW = max(labelW, min(textWidth(fd.label), 24))
	}
	for _, fd := range c.f.fields {
		usage := fd.usage
		if fd.arg {
			usage = T("аргумент", "argument")
		}
		if len(fd.choices) > 0 {
			usage += " [" + strings.Join(fd.choices, "|") + "]"
		}
		switch {
		case fd.pipedList():
			usage += fmt.Sprintf(T(" — через stdin (--%s), по одному в строке; в поле — через запятую или «;»", " — through stdin (--%s), one per line; in the field, separated by commas or semicolons"), fd.stdin)
		case fd.stdin != "":
			usage += fmt.Sprintf(T(" — через stdin (--%s)", " — through stdin (--%s)"), fd.stdin)
		}
		parts := wrap(usage, max(10, w-labelW-4))
		for i, p := range parts {
			label := ""
			if i == 0 {
				label = fd.label
			}
			out = append(out, " "+styleKey.Render(fit(label, labelW))+"  "+p)
		}
	}
	return out
}

func (c *cmdHelp) update(m *model, msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc", "q", "f1", "enter":
			m.overlay = nil
		case "up", "k":
			c.offset = max(0, c.offset-1)
		case "down", "j":
			c.offset++
		case "pgup":
			c.offset = max(0, c.offset-10)
		case "pgdown", "space":
			c.offset += 10
		}
	}
	return nil
}

func (c *cmdHelp) view(m *model, w, h int) string {
	lines := c.lines(w)
	c.offset = min(c.offset, max(0, len(lines)-h))
	return strings.Join(lines[c.offset:min(len(lines), c.offset+h)], "\n")
}

// useLine is a command's usage for lists: its path and its Use arguments.
func useLine(c *cobra.Command) string {
	p := cmdPath(c)
	if _, rest, ok := strings.Cut(c.Use, " "); ok {
		return p + " " + rest
	}
	return p
}

// palette runs any mp command line: with Tab completing commands and flags.
type palette struct {
	in   textInput
	err  string
	sugg []suggestion
	pick int
}

type suggestion struct{ word, desc string }

func newPalette() *palette { return &palette{pick: -1} }

func (p *palette) name() string { return T("Команда", "Command") }
func (p *palette) typing() bool { return true }

func (p *palette) hints(m *model) []hint {
	return []hint{{"enter", T("выполнить", "run")}, {"tab", T("дополнить", "complete")}, {"esc", T("закрыть", "close")}}
}

func (p *palette) update(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		p.in.insert(msg.Content)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			m.overlay = nil
			return nil
		case "enter":
			if p.pick >= 0 && p.pick < len(p.sugg) {
				p.complete(p.sugg[p.pick].word)
				p.pick = -1
				break
			}
			return m.runLine(p, p.in.value())
		case "tab":
			if p.pick >= 0 && p.pick < len(p.sugg) {
				p.complete(p.sugg[p.pick].word)
				p.pick = -1
			} else if len(p.sugg) == 1 {
				p.complete(p.sugg[0].word)
			} else if pre := commonPrefix(p.sugg); pre != "" {
				p.replaceWord(pre)
			}
		case "up":
			if len(p.sugg) > 0 {
				p.pick = max(0, p.pick-1)
			}
		case "down":
			if len(p.sugg) > 0 {
				p.pick = min(len(p.sugg)-1, p.pick+1)
			}
		default:
			p.in.handle(msg)
			p.pick = -1
		}
	}
	p.err = ""
	p.suggest(m)
	return nil
}

// current splits the line into the finished words and the one being typed.
func (p *palette) current() ([]string, string) {
	line := p.in.value()
	words, err := splitWords(line)
	if err != nil {
		return nil, ""
	}
	if line == "" || strings.HasSuffix(line, " ") {
		return words, ""
	}
	if len(words) == 0 {
		return nil, ""
	}
	return words[:len(words)-1], words[len(words)-1]
}

func (p *palette) suggest(m *model) {
	done, cur := p.current()
	c := m.root
	for i := 0; i < len(done); i++ {
		w := done[i]
		if i == 0 && (w == "mp" || w == "monopanel") {
			continue
		}
		if strings.HasPrefix(w, "-") {
			continue
		}
		sub := findCmd(c, w)
		if sub == nil || !offered(sub, m.opts) {
			break
		}
		c = sub
	}
	p.sugg = nil
	if strings.HasPrefix(cur, "-") {
		c.InheritedFlags()
		root := c.Root()
		c.Flags().VisitAll(func(fl *pflag.Flag) {
			if fl.Hidden || fl.Name == "help" {
				return
			}
			if root.PersistentFlags().Lookup(fl.Name) == fl && fl.Name != "json" && fl.Name != "no-wait" {
				return
			}
			if strings.HasPrefix("--"+fl.Name, cur) {
				p.sugg = append(p.sugg, suggestion{"--" + fl.Name, fl.Usage})
			}
		})
	} else {
		for _, sub := range c.Commands() {
			if !offered(sub, m.opts) || !strings.HasPrefix(sub.Name(), cur) {
				continue
			}
			p.sugg = append(p.sugg, suggestion{sub.Name(), sub.Short})
		}
	}
	sort.SliceStable(p.sugg, func(i, j int) bool { return p.sugg[i].word < p.sugg[j].word })
	if len(p.sugg) == 1 && p.sugg[0].word == cur {
		p.sugg = nil
	}
}

func commonPrefix(s []suggestion) string {
	if len(s) == 0 {
		return ""
	}
	pre := s[0].word
	for _, x := range s[1:] {
		for !strings.HasPrefix(x.word, pre) {
			pre = pre[:len(pre)-1]
		}
	}
	return pre
}

func (p *palette) replaceWord(word string) {
	line := p.in.value()
	i := strings.LastIndexAny(line, " ") + 1
	p.in.set(line[:i] + word)
}

func (p *palette) complete(word string) { p.replaceWord(word + " ") }

func (p *palette) view(m *model, w, h int) string {
	var lines []string
	lines = append(lines, " "+styleTitle.Render(T("Любая команда mp", "Any mp command")))
	lines = append(lines, " "+styleMuted.Render(truncate(T("Например: site add example.com --php 8.4 · без аргументов откроется форма", "For example: site add example.com --php 8.4 · without arguments a form opens"), w-2)))
	lines = append(lines, "")
	lines = append(lines, " "+styleKey.Render("mp ")+p.in.view(w-6, true, false))
	if p.err != "" {
		for _, l := range wrap(p.err, w-2) {
			lines = append(lines, " "+styleErr.Render(l))
		}
	}
	lines = append(lines, "")
	for i, s := range p.sugg {
		if len(lines) >= h {
			break
		}
		text := " " + fit(s.word, 24) + " " + truncate(s.desc, max(0, w-28))
		if i == p.pick {
			lines = append(lines, styleCursor.Render(pad(text, w)))
		} else {
			lines = append(lines, " "+styleKey.Render(fit(s.word, 24))+" "+styleMuted.Render(truncate(s.desc, max(0, w-28))))
		}
	}
	return strings.Join(lines, "\n")
}

// confirmOverlay asks before a destructive command: y/N, or the name typed
// out for deleting an account or a site with its files.
type confirmOverlay struct {
	spec  *confirmSpec
	input textInput
	ok    func(m *model) tea.Cmd
}

func (c *confirmOverlay) name() string { return T("Подтверждение", "Confirmation") }
func (c *confirmOverlay) typing() bool { return c.spec.word != "" }

func (c *confirmOverlay) hints(m *model) []hint {
	if c.spec.word != "" {
		return []hint{{"enter", T("подтвердить", "confirm")}, {"esc", T("отмена", "cancel")}}
	}
	return []hint{{"y/enter", T("да", "yes")}, {"n/esc", T("нет", "no")}}
}

func (c *confirmOverlay) update(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		if c.spec.word != "" {
			c.input.insert(msg.Content)
		}
	case tea.KeyPressMsg:
		k := msg.String()
		if k == "esc" {
			m.overlay = nil
			m.flash(T("отменено", "cancelled"), toneMuted)
			return nil
		}
		if c.spec.word != "" {
			if k == "enter" {
				if strings.TrimSpace(c.input.value()) == c.spec.word {
					m.overlay = nil
					return c.ok(m)
				}
				m.flash(fmt.Sprintf(T("введите %s точно", "type %s exactly"), c.spec.word), toneWarn)
				return nil
			}
			c.input.handle(msg)
			return nil
		}
		switch k {
		case "y", "Y", "enter", T("д", "y"), T("Д", "Y"):
			m.overlay = nil
			return c.ok(m)
		case "n", "N", "q", T("н", "n"), T("Н", "N"):
			m.overlay = nil
			m.flash(T("отменено", "cancelled"), toneMuted)
		}
	}
	return nil
}

func (c *confirmOverlay) view(m *model, w, h int) string {
	bw := min(w-4, 76)
	var body []string
	body = append(body, styleBold.Render(truncate(c.spec.title, bw-4)))
	body = append(body, "")
	cl := wrap(c.spec.line, bw-4)
	if len(cl) > 3 {
		cl = append(cl[:2], truncate(strings.Join(cl[2:], " "), bw-4))
	}
	for _, l := range cl {
		body = append(body, styleKey.Render(l))
	}
	var ask []string
	ask = append(ask, "")
	if c.spec.word != "" {
		ask = append(ask, fmt.Sprintf(T("Введите %s для подтверждения:", "Type %s to confirm:"), c.spec.word))
		ask = append(ask, "> "+c.input.view(bw-8, true, false))
	} else {
		ask = append(ask, T("Выполнить? [y/N]", "Run it? [y/N]"))
	}
	if c.spec.note != "" {
		// The box must fit the screen with its question: on a small
		// terminal the explanation gives way (cut short with "…"), never
		// the command, the question or the frame.
		room := h - 2 - len(body) - len(ask) - 1
		if nl := wrap(c.spec.note, bw-4); room > 0 {
			if len(nl) > room {
				nl = append(nl[:room-1], truncate(nl[room-1]+" "+strings.Join(nl[room:], " "), bw-4))
			}
			body = append(body, "")
			for _, l := range nl {
				body = append(body, styleWarn.Render(l))
			}
		}
	}
	body = append(body, ask...)
	box := styleBox.Width(bw).Render(strings.Join(body, "\n"))
	lines := strings.Split(box, "\n")
	top := max(0, (h-len(lines))/2)
	left := strings.Repeat(" ", max(0, (w-bw)/2))
	out := make([]string, 0, h)
	for i := 0; i < top; i++ {
		out = append(out, "")
	}
	for _, l := range lines {
		out = append(out, left+l)
	}
	return strings.Join(out, "\n")
}
