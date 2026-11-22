package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

// List-valued secrets (--cred-stdin, --env-stdin) and the one secret plus
// one list a command reads from the same stdin.

func TestListSecretGoesThroughStdin(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "dns-provider add"), adminEnv(), nil, nil)
	for _, fd := range f.fields {
		if fd.label == "--cred" || fd.label == "--cred-stdin" {
			t.Errorf("%s must not be a field: the credentials go through stdin", fd.label)
		}
	}
	cred := fieldByName(f, "cred")
	if cred == nil || cred.stdin != "cred-stdin" || !cred.pipedList() || !cred.secret || !cred.pairs {
		t.Fatalf("the credentials are a masked list for stdin: %+v", cred)
	}
	if !strings.Contains(cred.usage, "CLOUDFLARE_DNS_API_TOKEN") {
		t.Errorf("the help says what the values are (the list flag's usage): %q", cred.usage)
	}
	f.fields[0].setValue("cf")
	// Commas and semicolons separate; a quoted value keeps both.
	cred.setValue(`CLOUDFLARE_DNS_API_TOKEN=abc=def; "EXTRA=a,b;c" , B=2,,`)
	argv, shown, stdin, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"dns-provider", "add", "cf", "--cred-stdin"}) {
		t.Errorf("argv %q", argv)
	}
	if want := "CLOUDFLARE_DNS_API_TOKEN=abc=def\nEXTRA=a,b;c\nB=2"; stdin != want {
		t.Errorf("stdin %q, want %q", stdin, want)
	}
	for _, s := range append(shown, f.line()) {
		if strings.Contains(s, "abc") || strings.Contains(s, "EXTRA") {
			t.Errorf("a credential is shown: %q", s)
		}
	}
	if !strings.Contains(f.line(), "--cred-stdin") {
		t.Errorf("line %q", f.line())
	}

	// Not a pair: refused before running, without echoing the value.
	cred.setValue("CLOUDFLARE_DNS_API_TOKEN=abc, just-the-token-xyz")
	if _, _, _, err := f.build(); err == nil || !strings.Contains(err.Error(), "item 2") || strings.Contains(err.Error(), "xyz") {
		t.Errorf("an item without KEY= must be refused by number: %v", err)
	}
	// Only separators: nothing to pipe, so no --cred-stdin either.
	cred.setValue(" ;, ")
	if argv, _, stdin, err := f.build(); err != nil || slices.Contains(argv, "--cred-stdin") || stdin != "" {
		t.Errorf("an empty list is not passed: %q %q %v", argv, stdin, err)
	}
}

func TestSecretAndListShareStdin(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "backup target add"), adminEnv(), nil, nil)
	for _, fd := range f.fields {
		if fd.label == "--password" || fd.label == "--env" {
			t.Errorf("%s must not be a field next to its stdin twin", fd.label)
		}
	}
	f.fields[0].setValue("s3")
	fieldByName(f, "password").setValue("repo pass")
	fieldByName(f, "env").setValue("AWS_ACCESS_KEY_ID=k; AWS_SECRET_ACCESS_KEY=s")
	argv, _, stdin, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "--password-stdin") || !slices.Contains(argv, "--env-stdin") {
		t.Errorf("both stdin flags: %q", argv)
	}
	// The order mp backup target add reads: the password line, then the pairs.
	if want := "repo pass\nAWS_ACCESS_KEY_ID=k\nAWS_SECRET_ACCESS_KEY=s"; stdin != want {
		t.Errorf("stdin %q, want %q", stdin, want)
	}
	if note := f.stdinNote(); !strings.Contains(note, "password") || !strings.Contains(note, "env") {
		t.Errorf("the note names both: %q", note)
	}
	// Either one alone works as before.
	fieldByName(f, "password").setValue("")
	if _, _, stdin, err := f.build(); err != nil || stdin != "AWS_ACCESS_KEY_ID=k\nAWS_SECRET_ACCESS_KEY=s" {
		t.Errorf("the list alone: %q %v", stdin, err)
	}

	// Two one-line secrets cannot share stdin: refused.
	f = newForm(findCmd(root, "migrate run"), adminEnv(), nil, nil)
	fieldByName(f, "source").setValue("root@old")
	fieldByName(f, "token").setValue("t")
	fieldByName(f, "password").setValue("p")
	if _, _, _, err := f.build(); err == nil || !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "password") {
		t.Errorf("two secrets on one stdin must be refused: %v", err)
	}
	fieldByName(f, "password").setValue("")
	if _, _, stdin, err := f.build(); err != nil || stdin != "t" {
		t.Errorf("one secret: %q %v", stdin, err)
	}

	// Two lists cannot either: where one ends and the other starts is lost.
	two := &cobra.Command{Use: "add <name>", Args: cobra.ExactArgs(1), RunE: noop}
	two.Flags().StringSlice("cred", nil, "KEY=VALUE credentials")
	two.Flags().Bool("cred-stdin", false, "read the KEY=VALUE credentials from stdin")
	two.Flags().StringSlice("env", nil, "KEY=VALUE variables")
	two.Flags().Bool("env-stdin", false, "read the KEY=VALUE variables from stdin")
	(&cobra.Command{Use: "both"}).AddCommand(two)
	f = newForm(two, adminEnv(), nil, nil)
	f.fields[0].setValue("x")
	fieldByName(f, "cred").setValue("A=1")
	fieldByName(f, "env").setValue("B=2")
	if _, _, _, err := f.build(); err == nil || !strings.Contains(err.Error(), "cred") || !strings.Contains(err.Error(), "env") {
		t.Errorf("two lists on one stdin must be refused: %v", err)
	}
	fieldByName(f, "env").setValue("")
	if _, _, stdin, err := f.build(); err != nil || stdin != "A=1" {
		t.Errorf("one list: %q %v", stdin, err)
	}
}

// TestStdinOrderReachesTheCommand runs the form's command with the real
// runner in place of mp, as R-CLI's backup target add reads stdin: the
// password from the first line, then KEY=VALUE lines up to the end.
func TestStdinOrderReachesTheCommand(t *testing.T) {
	script := filepath.Join(t.TempDir(), "mp.sh")
	body := "echo \"args: $*\"\n" +
		"IFS= read -r pw; echo \"password=[$pw]\"\n" +
		"while IFS= read -r line && [ -n \"$line\" ]; do echo \"env=[$line]\"; done\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newForm(findCmd(testTree(), "backup target add"), adminEnv(), nil, nil)
	f.fields[0].setValue("s3")
	fieldByName(f, "password").setValue(" pass with spaces ")
	fieldByName(f, "env").setValue(`AWS_ACCESS_KEY_ID=k, "AWS_SECRET_ACCESS_KEY=a,b;c"`)
	argv, _, stdin, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	r := execRunner{exe: "/bin/sh"}
	ch, _, err := r.start(runJob{argv: append([]string{script}, argv...), stdin: stdin}, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	out, code, err := collect(ch)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v: %s", code, err, out)
	}
	want := "args: backup target add s3 --password-stdin --env-stdin\n" +
		"password=[ pass with spaces ]\n" +
		"env=[AWS_ACCESS_KEY_ID=k]\n" +
		"env=[AWS_SECRET_ACCESS_KEY=a,b;c]\n"
	if out != want {
		t.Errorf("the command read\n%s\nwant\n%s", out, want)
	}
}

func TestPastedLinesAreItems(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	f := newForm(findCmd(m.root, "dns-provider add"), adminEnv(), nil, nil)
	m.push(f)
	cred := fieldByName(f, "cred")
	for i, fd := range f.fields {
		if fd == cred {
			f.focus = i
		}
	}
	// The lines of a credentials file, one with a comma in its value.
	m.Update(tea.PasteMsg{Content: "RFC2136_NAMESERVER=ns1.example.com\r\nRFC2136_TSIG_SECRET=c2Vj,cmV0\n\n  RFC2136_TSIG_KEY=k  \n"})
	if got := cred.items(); !slices.Equal(got, []string{"RFC2136_NAMESERVER=ns1.example.com", "RFC2136_TSIG_SECRET=c2Vj,cmV0", "RFC2136_TSIG_KEY=k"}) {
		t.Errorf("pasted lines %q (field %q)", got, cred.value())
	}
	// A one-line paste is typed text: the user's own separators.
	cred.setValue("")
	m.Update(tea.PasteMsg{Content: "A=1, B=2"})
	if got := cred.items(); !slices.Equal(got, []string{"A=1", "B=2"}) {
		t.Errorf("a pasted line %q", got)
	}
	// Lines pasted after an item typed (or before one) stay items of their
	// own instead of running into it.
	cred.setValue("A=1")
	m.Update(tea.PasteMsg{Content: "B=2\nC=3\n"})
	if got := cred.items(); !slices.Equal(got, []string{"A=1", "B=2", "C=3"}) {
		t.Errorf("lines after an item %q (field %q)", got, cred.value())
	}
	cred.setValue("A=1,")
	cred.input.pos = 0
	m.Update(tea.PasteMsg{Content: "B=2\nC=3\n"})
	if got := cred.items(); !slices.Equal(got, []string{"B=2", "C=3", "A=1"}) {
		t.Errorf("lines before an item %q (field %q)", got, cred.value())
	}
	cred.setValue("A=1; ")
	m.Update(tea.PasteMsg{Content: "B=2\n"})
	if got := cred.items(); !slices.Equal(got, []string{"A=1", "B=2"}) || strings.Contains(cred.value(), ";,") {
		t.Errorf("after a separator %q (field %q)", got, cred.value())
	}
}

// TestPastedSecretHasNoLineBreak: a password copied with its line break is
// the password, not the password and a space.
func TestPastedSecretHasNoLineBreak(t *testing.T) {
	m, _ := newTestModel(t, "admin", apiRoutes())
	f := newForm(findCmd(m.root, "backup target add"), adminEnv(), nil, nil)
	m.push(f)
	f.fields[0].setValue("s3")
	pw := fieldByName(f, "password")
	for i, fd := range f.fields {
		if fd == pw {
			f.focus = i
		}
	}
	m.Update(tea.PasteMsg{Content: "c0rrect horse\r\n"})
	if pw.value() != "c0rrect horse" {
		t.Errorf("pasted password %q", pw.value())
	}
	if _, _, stdin, err := f.build(); err != nil || stdin != "c0rrect horse" {
		t.Errorf("piped %q %v", stdin, err)
	}
}

func TestPaletteListGoesToStdin(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "dns-provider add"), adminEnv(), nil, nil)
	// pflag would read each --cred as CSV: a semicolon is part of a value.
	ready, err := f.fill([]string{"cf", "--cred", "A=1,B=x;y", "--cred", `"C=p,q"`})
	if err != nil || !ready {
		t.Fatalf("fill: %v %v", ready, err)
	}
	argv, _, stdin, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "--cred=") }) || !slices.Contains(argv, "--cred-stdin") {
		t.Errorf("a typed --cred goes through stdin: %q", argv)
	}
	if stdin != "A=1\nB=x;y\nC=p,q" {
		t.Errorf("stdin %q", stdin)
	}
	// --cred-stdin typed: the form opens on the field for the values.
	f = newForm(findCmd(root, "dns-provider add"), adminEnv(), nil, nil)
	if ready, err := f.fill([]string{"cf", "--cred-stdin"}); err != nil || ready || f.fields[f.focus].name != "cred" {
		t.Errorf("--cred-stdin needs the values in the form: %v %v %d", ready, err, f.focus)
	}
}

func TestSplitPairs(t *testing.T) {
	for in, want := range map[string][]string{
		"A=1, B=2":                {"A=1", "B=2"},
		"A=1;B=2;":                {"A=1", "B=2"},
		`"A=x,y" ; B=" q "`:       {"A=x,y", `B=" q "`},
		`"A="" quoted """`:        {`A=" quoted "`},
		`A="x"`:                   {`A="x"`},
		`" A=spaced "`:            {" A=spaced "},
		"":                        nil,
		" , ;":                    nil,
		`"A=unclosed, still one`:  {"A=unclosed, still one"},
		"KEY=value with spaces  ": {"KEY=value with spaces"},
	} {
		if got := splitPairs(in); !slices.Equal(got, want) {
			t.Errorf("splitPairs(%q) = %q, want %q", in, got, want)
		}
	}
	items := []string{"A=1", "B=x,y", "C=p;q", `D="z"`, " E=edge "}
	if got := splitPairs(joinPairs(items)); !slices.Equal(got, items) {
		t.Errorf("joinPairs/splitPairs round trip: %q → %q", joinPairs(items), got)
	}
}

// A command that declares the order it reads its secrets in gets several on
// stdin, one per line in that order — mp user set: the current password,
// then the new one — whatever the order of the form's fields.
func TestSecretsInTheDeclaredOrder(t *testing.T) {
	root := testTree()
	f := newForm(findCmd(root, "user set"), adminEnv(), nil, nil)
	fieldByName(f, "login").setValue("alex")
	fieldByName(f, "password").setValue("new-made-up-pass")
	fieldByName(f, "current-password").setValue("old-made-up-pass")
	argv, shown, stdin, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if stdin != "old-made-up-pass\nnew-made-up-pass" {
		t.Errorf("stdin %q", stdin)
	}
	for _, a := range append(argv, shown...) {
		if strings.Contains(a, "made-up") {
			t.Errorf("a secret on the command line: %q", a)
		}
	}
}
