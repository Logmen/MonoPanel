package tui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// textInput is a one-line editor with the usual readline keys.
type textInput struct {
	runes []rune
	pos   int
}

func (t *textInput) set(s string) {
	t.runes = []rune(s)
	t.pos = len(t.runes)
}

func (t *textInput) value() string { return string(t.runes) }

func (t *textInput) insert(s string) {
	ins := []rune(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if len(ins) == 0 {
		return
	}
	t.runes = append(t.runes[:t.pos], append(ins, t.runes[t.pos:]...)...)
	t.pos += len(ins)
}

// handle applies a key; it reports whether the key belonged to the editor.
func (t *textInput) handle(k tea.KeyPressMsg) bool {
	switch k.String() {
	case "left", "ctrl+b":
		if t.pos > 0 {
			t.pos--
		}
	case "right", "ctrl+f":
		if t.pos < len(t.runes) {
			t.pos++
		}
	case "home", "ctrl+a":
		t.pos = 0
	case "end", "ctrl+e":
		t.pos = len(t.runes)
	case "backspace", "ctrl+h":
		if t.pos > 0 {
			t.runes = append(t.runes[:t.pos-1], t.runes[t.pos:]...)
			t.pos--
		}
	case "delete", "ctrl+d":
		if t.pos < len(t.runes) {
			t.runes = append(t.runes[:t.pos], t.runes[t.pos+1:]...)
		}
	case "ctrl+u":
		t.runes = append([]rune{}, t.runes[t.pos:]...)
		t.pos = 0
	case "ctrl+k":
		t.runes = t.runes[:t.pos]
	case "ctrl+w", "alt+backspace":
		start := t.wordLeft()
		t.runes = append(t.runes[:start], t.runes[t.pos:]...)
		t.pos = start
	case "ctrl+left", "alt+left", "alt+b":
		t.pos = t.wordLeft()
	case "ctrl+right", "alt+right", "alt+f":
		t.pos = t.wordRight()
	default:
		if k.Text == "" || k.Mod.Contains(tea.ModCtrl) || k.Mod.Contains(tea.ModAlt) {
			return false
		}
		t.insert(k.Text)
	}
	return true
}

func (t *textInput) wordLeft() int {
	i := t.pos
	for i > 0 && unicode.IsSpace(t.runes[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(t.runes[i-1]) {
		i--
	}
	return i
}

func (t *textInput) wordRight() int {
	i := t.pos
	for i < len(t.runes) && unicode.IsSpace(t.runes[i]) {
		i++
	}
	for i < len(t.runes) && !unicode.IsSpace(t.runes[i]) {
		i++
	}
	return i
}

// view renders the text in w cells. With focus the caret is drawn and the
// text scrolls to keep it visible; mask hides the characters.
func (t *textInput) view(w int, focus, mask bool) string {
	if w < 2 {
		w = 2
	}
	shown := t.runes
	if mask {
		shown = []rune(strings.Repeat("•", len(t.runes)))
	}
	if !focus {
		return truncate(string(shown), w)
	}
	// Window of runes around the caret: start so that the caret fits.
	start, used := t.pos, 1
	for start > 0 && used+runeWidth(shown[start-1]) <= w {
		start--
		used += runeWidth(shown[start])
	}
	var b strings.Builder
	cells := 0
	for i := start; i <= len(shown); i++ {
		if i == t.pos {
			ch := " "
			if i < len(shown) {
				ch = string(shown[i])
			}
			if cells+max(1, textWidth(ch)) > w {
				break
			}
			b.WriteString(styleCaret.Render(ch))
			cells += max(1, textWidth(ch))
			continue
		}
		if i == len(shown) {
			break
		}
		rw := runeWidth(shown[i])
		if cells+rw > w {
			break
		}
		b.WriteRune(shown[i])
		cells += rw
	}
	return b.String()
}
