package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The real runner, with harmless binaries in place of mp.

func TestExecRunnerOutputAndExitCode(t *testing.T) {
	r := execRunner{exe: "/bin/sh"}
	ch, _, err := r.start(runJob{argv: []string{"-c", "echo out; echo err >&2; exit 3"}}, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	out, code, err := collect(ch)
	if err != nil || code != 3 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(out, "out\n") || !strings.Contains(out, "err\n") {
		t.Errorf("stdout and stderr in one stream: %q", out)
	}
}

func TestExecRunnerStdin(t *testing.T) {
	r := execRunner{exe: "/bin/cat"}
	ch, _, err := r.start(runJob{stdin: "s3cret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, code, _ := collect(ch); out != "s3cret\n" || code != 0 {
		t.Errorf("the secret reaches stdin: %q %d", out, code)
	}
	// Without a secret the child reads EOF at once instead of hanging.
	ch, _, err = r.start(runJob{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		collect(ch) //nolint:errcheck // only the end matters
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a child without stdin must get EOF")
	}
}

func TestExecRunnerCancel(t *testing.T) {
	// sleep itself, not through a shell: a shell that forks it and waits
	// takes the SIGINT alone, and the grandchild keeps the output pipe open
	// for its 30 seconds after the shell is killed (flaky on CI runners)
	r := execRunner{exe: "/bin/sleep"}
	ch, cancel, err := r.start(runJob{argv: []string{"30"}}, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan int, 1)
	go func() {
		_, code, _ := collect(ch)
		done <- code
	}()
	select {
	case code := <-done:
		if code == 0 {
			t.Error("an interrupted command does not exit 0")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel must stop the child")
	}
}

func TestTerminalRunWaitsForEnter(t *testing.T) {
	r := execRunner{exe: "/bin/echo"}
	c := r.terminal(runJob{argv: []string{"hello"}}, []string{"PATH=/usr/bin:/bin"})
	var out bytes.Buffer
	c.Stdin = strings.NewReader("\n")
	c.Stdout = &out
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello") || !strings.Contains(out.String(), "Press Enter") {
		t.Errorf("output %q", out.String())
	}
	// The exit status is the command's, not the pause's.
	r = execRunner{exe: "/bin/false"}
	c = r.terminal(runJob{}, []string{"PATH=/usr/bin:/bin"})
	c.Stdin = strings.NewReader("\n")
	if err := c.Run(); err == nil {
		t.Error("the command's failure must show")
	}
}

func TestRunScreenOutput(t *testing.T) {
	r := newRunScreen("x", []runJob{{shown: "mp x"}})
	r.add("one\ntwo\rTWO\nthree")
	if got := r.output(); strings.Join(got, "|") != "one|TWO|three" {
		t.Errorf("lines %q", got)
	}
	r.add(" more\n\x1b[31mred\x1b[0m\n")
	if got := r.output(); got[2] != "three more" || got[3] != "red" {
		t.Errorf("partial line and escapes: %q", got)
	}
	for i := 0; i < maxLines+10; i++ {
		r.add("line\n")
	}
	if len(r.lines) != maxLines {
		t.Errorf("the output is capped: %d", len(r.lines))
	}
	w := r.window(20, 5)
	if len(w) != 5 {
		t.Errorf("the window: %d lines", len(w))
	}
}

func TestWrapKeep(t *testing.T) {
	table := "DOMAIN       USER  PHP\nexample.com  bob   8.4"
	if got := wrapKeep(table, 40); strings.Join(got, "|") != "DOMAIN       USER  PHP|example.com  bob   8.4" {
		t.Errorf("a table that fits keeps its columns: %q", got)
	}
	got := wrapKeep("    job line with several words in it", 20)
	for i, l := range got {
		if textWidth(l) > 20 {
			t.Errorf("line %d is too wide: %q", i, l)
		}
		if !strings.HasPrefix(l, "    ") {
			t.Errorf("line %d lost the indentation: %q", i, l)
		}
	}
	if strings.Join(strings.Fields(strings.Join(got, " ")), " ") != "job line with several words in it" {
		t.Errorf("words lost: %q", got)
	}
	if got := wrapKeep(strings.Repeat("x", 25), 10); len(got) != 3 || got[0] != strings.Repeat("x", 10) {
		t.Errorf("a long word is cut: %q", got)
	}
	if got := wrapKeep("", 10); len(got) != 1 || got[0] != "" {
		t.Errorf("an empty line stays: %q", got)
	}
}

func TestSanitizeControls(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[31mred\x1b[0m":         "red",
		"\u009b31mred":               "red",
		"\x1b]0;title\x07text":       "text",
		"\u009d0;title\u009ctext":    "text",
		"a\u0085b\x07c":              "abc",
		"tab\there":                  "tab    here",
		"bad \xff byte":              "bad \uFFFD byte",
		"plain текст":                "plain текст",
		"\x1b]8;;http://x\x1b\\link": "link",
		"\x1bcreset":                 "reset",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Styled lines keep the interface's own colours and lose everything else a
// value from the server could carry.
func TestSafeStyled(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[1;38;2;63;201;184mMonoPanel\x1b[0m": "\x1b[1;38;2;63;201;184mMonoPanel\x1b[0m",
		"site \x1b]52;c;ZWNobyBoaQ==\x07evil":     "site evil",
		"a\x1b[2J\x1b[Hb":                         "ab",
		"x\u009b31my":                             "xy",
		"tab\tkept":                               "tab\tkept",
		"cr\rlf\nbell\x07":                        "crlfbell",
		"\x1b[?25lhide":                           "hide",
	} {
		if got := safeStyled(in); got != want {
			t.Errorf("safeStyled(%q) = %q, want %q", in, got, want)
		}
	}
}
