package tui

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"monopanel/internal/client"
)

// The file browser of an account: what the web file manager does, as mp
// files commands.

func openFiles(m *model, a *area) screen {
	if !m.admin() {
		return filesScreen(m, "", "/")
	}
	s := usersPicker(m, a.title, func(m *model, login string) tea.Cmd { return m.push(filesScreen(m, login, "/")) })
	s.(*listScreen).links = []link{allLink(a)}
	return s
}

// filesScreen browses an account's home: enter goes into a directory,
// backspace back up; every mp files command is a key away.
func filesScreen(m *model, user, dir string) screen {
	if dir == "" {
		dir = "/"
	}
	s := &listScreen{screenBase: screenBase{title: T("Файлы", "Files"), area: areaByID(m, "files")}, query: map[string]string{"dir": dir, "user": user},
		cols: []column{{title: T("Имя", "Name"), min: 20}, {title: T("Размер", "Size"), right: true}, {title: T("Изменён", "Modified")}, {title: T("Права", "Mode")}}}
	s.ctx = map[string]string{"user": user}
	s.titleFn = func(s *listScreen) string {
		who := s.query["user"]
		if who == "" {
			who = "~"
		}
		return T("Файлы", "Files") + " " + who + ":" + s.query["dir"]
	}
	s.fetch = func(ctx context.Context, cl *client.Client, q map[string]string) (*result, error) {
		l, err := cl.FilesList(ctx, q["user"], q["dir"])
		if err != nil {
			return nil, err
		}
		cur := l.Path
		if cur == "" {
			cur = q["dir"]
		}
		res := &result{vals: map[string]string{"dir": cur, "dirslash": strings.TrimSuffix(cur, "/") + "/"}}
		res.info = []string{styleMuted.Render(fmt.Sprintf(T("%s · enter — открыть, backspace — выше, e — править в $EDITOR", "%s · enter — open, backspace — up, e — edit in $EDITOR"), l.Home+cur))}
		if cur != "/" && cur != "" {
			res.rows = append(res.rows, row{cells: []string{"../", "", "", ""}, vals: map[string]string{"type": "up", "path": path.Dir(strings.TrimSuffix(cur, "/"))}})
		}
		for _, e := range l.Entries {
			name := e.Name
			switch e.Type {
			case "dir":
				name += "/"
			case "symlink":
				name += " → " + e.Target
			}
			p := path.Join(cur, e.Name)
			mtime := e.ModTime
			if len(mtime) >= 16 {
				mtime = strings.Replace(mtime[:16], "T", " ", 1)
			}
			size := humanBytes(uint64(e.Size))
			if e.Type == "dir" {
				size = ""
			}
			tn := toneNone
			if e.Type == "dir" {
				tn = toneOK
			}
			res.rows = append(res.rows, row{cells: []string{name, size, mtime, e.Mode}, tones: []tone{tn, toneMuted, toneMuted, toneMuted},
				vals: map[string]string{"path": p, "name": e.Name, "type": e.Type, "mode": octalMode(e.Mode)}})
		}
		return res, nil
	}
	// Going down starts on the first entry; going up, on the directory
	// that was just left.
	cd := func(m *model, s *listScreen, to string) tea.Cmd {
		from := s.query["dir"]
		s.query["dir"] = to
		s.cursor, s.offset, s.filter = 1, 0, ""
		if strings.HasPrefix(from, strings.TrimSuffix(to, "/")+"/") || to == "/" {
			s.cursor = 0
			s.focusOn = [2]string{"path", from}
		}
		return s.load(m)
	}
	s.enter = func(m *model, s *listScreen, r row) tea.Cmd {
		switch r.vals["type"] {
		case "up", "dir":
			return cd(m, s, r.vals["path"])
		}
		return m.push(rowPage(m, s, r))
	}
	s.keyHook = func(m *model, s *listScreen, k string) (bool, tea.Cmd) {
		if k == "backspace" || k == "left" || k == "h" {
			cur := s.query["dir"]
			if cur == "/" || cur == "" {
				return true, nil
			}
			return true, cd(m, s, path.Dir(strings.TrimSuffix(cur, "/")))
		}
		return false, nil
	}
	u := map[string]string{"user": "user"}
	p := []string{"path"}
	isFile := func(v map[string]string) bool { return v["type"] == "file" }
	real := func(v map[string]string) bool { return v["type"] != "up" && v["type"] != "" }
	archive := func(v map[string]string) bool {
		n := strings.ToLower(v["name"])
		return isFile(v) && (strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".tar"))
	}
	s.acts = []*act{
		{path: "files get", key: "e", args: p, flags: u, edit: "file", label: T("Править в $EDITOR", "Edit in $EDITOR"), when: isFile},
		{path: "files get", key: "g", args: []string{"path", "name"}, flags: u, when: isFile},
		{path: "files mv", key: "m", args: []string{"path", "path"}, flags: u, when: real},
		{path: "files chmod", key: "c", args: []string{"mode", "path"}, flags: u, when: real},
		{path: "files extract", key: "x", args: []string{"path", "dir"}, flags: u, when: archive},
		{path: "files size", key: "z", args: p, flags: u, quick: true, when: real},
		{path: "files rm", key: "d", args: p, flags: u, when: real},
		{path: "files mkdir", key: "N", args: []string{"dirslash"}, flags: u},
		{path: "files touch", key: "n", args: []string{"dirslash"}, flags: u},
		{path: "files put", key: "u", args: []string{"", "dirslash"}, flags: u},
		{path: "files ls", args: []string{"dir"}, flags: u, quick: true},
	}
	// For an ordinary account the browser is the area's first page.
	s.links = []link{{key: "*", label: T("все команды", "all commands"), open: areaAll}}
	s.empty = T("каталог пуст", "the directory is empty")
	return s
}

// octalMode turns "-rw-r--r--" into "644"; "0644" stays as it is.
func octalMode(mode string) string {
	if len(mode) == 4 && strings.Trim(mode, "01234567") == "" {
		return mode
	}
	if len(mode) < 9 {
		return mode
	}
	bits := mode[len(mode)-9:]
	n := 0
	for _, c := range bits {
		n <<= 1
		if c != '-' {
			n |= 1
		}
	}
	return strconv.FormatInt(int64(n), 8)
}
