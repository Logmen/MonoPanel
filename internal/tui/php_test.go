package tui

import (
	"slices"
	"testing"
)

// The site forms offer the installed PHP branches as the web UI's select
// does — to ordinary accounts too (GET /php/versions is open to them).

func phpRoutes() map[string]any {
	routes := apiRoutes()
	routes["/api/v1/php/versions"] = map[string]any{
		"installed": []map[string]any{
			{"version": "7.4", "status": "installing"},
			{"version": "8.3", "status": "installed"},
			{"version": "8.4", "status": "installed"},
		},
		"available": []any{},
	}
	return routes
}

func TestPHPChoicesForEveryone(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		m, fr := newTestModel(t, role, phpRoutes())
		openArea(t, m, "Sites")
		press(m, "a") // site add
		f, ok := m.top().(*form)
		if !ok {
			t.Fatalf("%s: site add opens a form: %T", role, m.top())
		}
		php := fieldByName(f, "php")
		if php.kind != kindEnum || !slices.Equal(php.choices, []string{"", "8.3", "8.4"}) || php.value() != "" {
			t.Errorf("%s: --php offers the installed branches (not the one still installing): kind %d %q %q", role, php.kind, php.choices, php.value())
		}
		// The default stays off the command line; a choice goes on it.
		f.fields[0].setValue("new.test")
		if role == "admin" {
			fieldByName(f, "user").setValue("bob")
		}
		php.cycle(1)
		press(m, "ctrl+s")
		if jobs := fr.started(); len(jobs) != 1 || !slices.Contains(jobs[0].argv, "--php=8.3") {
			t.Errorf("%s: the chosen branch is passed: %+v", role, jobs)
		}
	}
}

func TestPHPChoicesKeepTheSitesBranch(t *testing.T) {
	routes := phpRoutes()
	old := map[string]any{"domain": "example.com", "login": "bob", "php_version": "8.1", "mode": "fpm", "status": "active"}
	routes["/api/v1/sites"] = []map[string]any{old}
	routes["/api/v1/sites/example.com"] = old
	m, fr := newTestModel(t, "admin", routes)
	openArea(t, m, "Sites")
	press(m, "s") // site set, from the site's settings
	f := m.top().(*form)
	php := fieldByName(f, "php")
	if php.kind != kindEnum || php.value() != "8.1" || !slices.Contains(php.choices, "8.1") || !slices.Contains(php.choices, "8.4") {
		t.Fatalf("the site's own branch stays a choice: %q %q", php.choices, php.value())
	}
	if php.passes() {
		t.Error("unchanged, it is not passed")
	}
	// A site that has a branch is not offered "none": --php='' would not
	// change it (the web UI's select has no such option for it either).
	if slices.Contains(php.choices, "") {
		t.Errorf("no empty choice for a site with a branch: %q", php.choices)
	}
	php.setValue("8.4")
	press(m, "ctrl+s")
	if jobs := fr.started(); len(jobs) != 1 || !slices.Equal(jobs[0].argv, []string{"site", "set", "example.com", "--php=8.4"}) {
		t.Errorf("jobs %+v", jobs)
	}
}

func TestPHPFreeTextWithoutTheList(t *testing.T) {
	// No /php/versions: the field stays free text, as before.
	m, _ := newTestModel(t, "user", apiRoutes())
	openArea(t, m, "Sites")
	press(m, "a")
	if php := fieldByName(m.top().(*form), "php"); php.kind != kindText {
		t.Errorf("--php without the list: kind %d", php.kind)
	}
	// A field typed into before the list came stays as typed.
	f := newForm(findCmd(m.root, "site add"), formEnv{}, nil, nil)
	fieldByName(f, "php").setValue("8.")
	f.refreshPHP([]string{"8.3"})
	if php := fieldByName(f, "php"); php.kind != kindText || php.value() != "8." {
		t.Errorf("a typed value is not taken away: %d %q", php.kind, php.value())
	}
	f = newForm(findCmd(m.root, "site add"), formEnv{}, nil, nil)
	f.refreshPHP([]string{"8.3"})
	if php := fieldByName(f, "php"); php.kind != kindEnum {
		t.Errorf("an untouched field takes the list that came late: %d", php.kind)
	}
}

// TestPHPChoicesFromTheKnownList: a form opened when the list is known
// already offers it at once, from the row's value on.
func TestPHPChoicesFromTheKnownList(t *testing.T) {
	root := testTree()
	env := formEnv{admin: true, php: []string{"8.3", "8.4"}}
	f := newForm(findCmd(root, "site add"), env, nil, nil)
	if php := fieldByName(f, "php"); php.kind != kindEnum || !slices.Equal(php.choices, []string{"", "8.3", "8.4"}) {
		t.Errorf("site add: %q", php.choices)
	}
	a := &act{path: "site set", args: []string{"domain"}, prefill: same("php")}
	f = newForm(findCmd(root, "site set"), env, a, map[string]string{"domain": "example.com", "php": "8.1"})
	php := fieldByName(f, "php")
	if php.kind != kindEnum || php.value() != "8.1" || !slices.Equal(php.choices, []string{"8.3", "8.4", "8.1"}) || php.passes() {
		t.Errorf("site set on 8.1: %q %q passes %v", php.choices, php.value(), php.passes())
	}
}
