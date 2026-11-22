package mailpolicy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func policyJSON(t *testing.T, p *Policy) string {
	t.Helper()
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoaderReloadsAndKeepsTheLastGood(t *testing.T) {
	dir := t.TempDir()
	path, checks := filepath.Join(dir, "policy.json"), filepath.Join(dir, "checks")
	l := NewLoader(path, checks, nil)
	if ix, ok := l.Current(); ix != nil || ok {
		t.Fatalf("no files: %v %v", ix, ok)
	}
	writeFile(t, checks, HeaderChecksContent)
	writeFile(t, path, policyJSON(t, testPolicy()))
	ix, ok := l.Current()
	if ix == nil || !ok || !ix.IsAccount("tenanta") || ix.IsAccount("tenantd") {
		t.Fatalf("first policy: %v %v", ix, ok)
	}
	// A new account counts from the next message on.
	p := testPolicy()
	p.Accounts["tenantd"] = Account{Domains: []string{"d.test"}}
	writeFile(t, path, policyJSON(t, p))
	if ix, _ := l.Current(); ix == nil || !ix.IsAccount("tenantd") {
		t.Fatal("the changed policy was not read")
	}
	// A broken file leaves the last good one.
	writeFile(t, path, `{"version":1,"accounts":{"Bad Login":{"domains":[]}},"mailboxes":{}}`)
	if ix, _ := l.Current(); ix == nil || !ix.IsAccount("tenantd") {
		t.Fatal("a bad policy replaced the last good one")
	}
	_ = os.Remove(path)
	if ix, _ := l.Current(); ix == nil || !ix.IsAccount("tenantd") {
		t.Fatal("a missing policy dropped the last good one")
	}
	// An edit in place (same inode) is seen too.
	writeFile(t, path, policyJSON(t, testPolicy()))
	l.Current()
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(policyJSON(t, p)), 0o640); err != nil {
		t.Fatal(err)
	}
	if ix, _ := l.Current(); ix == nil || !ix.IsAccount("tenantd") {
		t.Fatal("an edit in place was not read")
	}
}

func TestLoaderHeaderChecks(t *testing.T) {
	dir := t.TempDir()
	path, checks := filepath.Join(dir, "policy.json"), filepath.Join(dir, "checks")
	writeFile(t, path, policyJSON(t, testPolicy()))
	l := NewLoader(path, checks, nil)
	for content, want := range map[string]bool{
		HeaderChecksContent:     true,
		HeaderChecksRule + "\n": true,
		"":                      false,
		"# nothing\n":           false,
		"/^Received:/ IGNORE\n" + HeaderChecksRule + "\n":                           false,
		HeaderChecksRule + "\n/^X-Foo:/ IGNORE\n":                                   false,
		`/^(Received: by [^ ]+ \(Postfix, from userid [0-9]+\))/ PREPEND $1` + "\n": false,
	} {
		writeFile(t, checks, content)
		if _, ok := l.Current(); ok != want {
			t.Errorf("header checks %q: trusted %v, want %v", content, ok, want)
		}
	}
}
