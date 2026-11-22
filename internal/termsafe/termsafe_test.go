package termsafe

import "testing"

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[31mred\x1b[0m":         "red",
		"\u009b31mred":               "red",
		"\x1b]52;c;ZWNobw==\x07text": "text",
		"\u009d0;title\u009ctext":    "text",
		"a\u0085b\x07c":              "abc",
		"tab\there":                  "tab    here",
		"bad \xff byte":              "bad � byte",
		"plain текст\nnext":          "plain текст\nnext",
		"\x1b]8;;http://x\x1b\\link": "link",
		"\x1bcreset":                 "reset",
		"cr\rhide":                   "crhide",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Line("a\nb\x1b[2Jc"); got != "a bc" {
		t.Errorf("Line: %q", got)
	}
}
