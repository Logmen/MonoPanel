package tui

import (
	"slices"
	"testing"
)

func TestBoundFlags(t *testing.T) {
	root := testTree()
	user := BoundFlags(root, Options{}, "user", "site add")
	admin := BoundFlags(root, Options{}, "admin", "site add")
	if slices.Contains(user, "user") || !slices.Contains(admin, "user") {
		t.Errorf("--user of site add is the administrators' only: user %q, admin %q", user, admin)
	}
	if !slices.Contains(user, "php") {
		t.Errorf("an ordinary account's site add keeps --php: %q", user)
	}
	if nginx := BoundFlags(root, Options{}, "user", "site nginx"); slices.Contains(nginx, "set") || slices.Contains(nginx, "clear") {
		t.Errorf("site nginx --set and --clear are the administrators': %q", nginx)
	}
	if pw := BoundFlags(root, Options{}, "admin", "user add"); !slices.Contains(pw, "password-stdin") || slices.Contains(pw, "password") {
		t.Errorf("the password goes to stdin, named by --password-stdin: %q", pw)
	}
	if got := BoundFlags(root, Options{Server: "https://x"}, "admin", "config set"); slices.Contains(got, "local") {
		t.Errorf("with --server, --local (which bypasses the API) is no field: %q", got)
	}
	if got := BoundFlags(root, Options{}, "admin", "config set"); !slices.Contains(got, "local") {
		t.Errorf("on the server, config set --local is offered: %q", got)
	}
	if BoundFlags(root, Options{}, "admin", "no such") != nil {
		t.Error("a command mp does not have has no form")
	}
}
