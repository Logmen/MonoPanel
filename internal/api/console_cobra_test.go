package api_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"monopanel/internal/api"
	"monopanel/internal/cli"
)

// The console refuses the daemon roles and lets a command have its own
// --token and --insecure by the command the arguments name. It must name the
// one mp then runs: cobra's Find on mp's own tree, set up as Execute sets it up
// before the search. Besides the lines below, every flag cobra can meet before
// the command words — mp's global flags and those of its command groups, long
// and short — is tried in front of a command, so a new switch among them
// shows up here first. (The console does not know the tree, so after a
// command without subcommands its second word is an argument: "helper id"
// for mp helper.)
func TestConsoleCommandIsCobras(t *testing.T) {
	lines := [][]string{
		{"migrate", "plan", "--token", "x"}, {"--json", "migrate", "run"}, {"update", "--no-wait", "settings"},
		{"--", "migrate", "plan"}, {"migrate", "--", "plan"}, {"migrate", "--token", "x", "plan"},
		{"--token", "x", "--insecure", "migrate", "plan"}, {"migrate", "--token", "plan", "grant"},
		{"--json=true", "migrate", "--insecure=false", "run"}, {"--no-wait", "--json", "update", "settings", "--token=x"},
		{"--cwd", ".", "fsop", "run", "--", "php"}, {"--strip", "1", "fsop", "extract", "a.zip", "x"},
		{"--uid", "0", "--gid", "0", "helper", "id"}, {"--hostname", "x", "setup"}, {"--json", "", "fsop", "read"},
		{"-", "agent"}, {"-v", "agent"}, {"--config", "/x", "api"}, {"files", "--user", "bob", "ls"},
		{"cron", "--user", "bob", "list"}, {"mail", "box", "--domain", "example.com", "list"}, {"user", "share", "entry", "bob", "x"},
		{"site", "list"}, {"status"}, {"--server", "https://x", "migrate", "run"}, {"update", "--json", "trust"},
	}
	forms := func(f *pflag.Flag) []string {
		out := []string{"--" + f.Name}
		if f.Shorthand != "" {
			out = append(out, "-"+f.Shorthand)
		}
		return out
	}
	root := cli.NewRoot()
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		for _, flag := range forms(f) {
			lines = append(lines, []string{flag, "fsop", "run"}, []string{flag, "x", "fsop", "run"})
		}
	})
	for _, group := range root.Commands() {
		if !group.HasSubCommands() {
			continue
		}
		sub := group.Commands()[0].Name()
		group.LocalFlags().VisitAll(func(f *pflag.Flag) {
			for _, flag := range forms(f) {
				lines = append(lines, []string{group.Name(), flag, sub, "x"}, []string{group.Name(), flag, "x", sub})
			}
		})
	}
	for _, args := range lines {
		root := cli.NewRoot()
		root.InitDefaultHelpCmd()
		root.InitDefaultCompletionCmd()
		c, _, err := root.Find(args)
		if err != nil || !c.Runnable() {
			continue // cobra runs nothing (a group only prints its help)
		}
		want := strings.Fields(strings.TrimPrefix(c.CommandPath(), root.Name()))
		if len(want) > 2 {
			want = want[:2]
		}
		got := strings.Fields(api.ConsoleCommand(args))
		if len(got) > len(want) && len(c.Commands()) == 0 {
			got = got[:len(want)]
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%q: the console reads %q, mp runs %q", args, api.ConsoleCommand(args), strings.Join(want, " "))
		}
	}
}

// The commands and flags the console singles out are mp's own: a renamed
// command or flag would slip past a refusal, and a command's own --token or
// --insecure must really be the command's, or a typed one would replace the
// console's credentials.
func TestConsoleListsNameMPsCommands(t *testing.T) {
	root := cli.NewRoot()
	find := func(path string) *cobra.Command {
		t.Helper()
		c, _, err := root.Find(strings.Fields(path))
		if err != nil || c.CommandPath() != root.Name()+" "+path {
			t.Errorf("the console names mp %s, which mp does not have", path)
			return nil
		}
		return c
	}

	// The commands' own flags are exactly their local flags that hide mp's
	// global ones.
	shadows := map[string][]string{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if root.PersistentFlags().Lookup(f.Name) != nil {
				path := strings.TrimPrefix(c.CommandPath(), root.Name()+" ")
				shadows[path] = append(shadows[path], "--"+f.Name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	own := map[string][]string{}
	for path, flags := range api.ConsoleOwnFlags {
		own[path] = slices.Sorted(slices.Values(flags))
	}
	for path, flags := range shadows {
		slices.Sort(flags)
		shadows[path] = flags
	}
	if !reflect.DeepEqual(own, shadows) {
		t.Errorf("the console lets these commands have their own client flags: %v; mp's commands define these: %v", own, shadows)
	}

	// Every command and flag that works with local files is there to refuse.
	for path, flags := range api.ConsoleLocalFiles {
		c := find(path)
		if c == nil {
			continue
		}
		if len(flags) == 0 && !c.Runnable() {
			t.Errorf("the console refuses mp %s as a whole, which runs nothing", path)
		}
		for _, flag := range flags {
			found := false
			var look func(c *cobra.Command)
			look = func(c *cobra.Command) {
				found = found || c.LocalNonPersistentFlags().Lookup(strings.TrimPrefix(flag, "--")) != nil
				for _, sub := range c.Commands() {
					look(sub)
				}
			}
			if look(c); !found {
				t.Errorf("the console refuses mp %s %s, which mp does not have", path, flag)
			}
		}
	}
	for path := range api.ConsoleRootOnly {
		find(path)
	}
}

// Every secret flag the console moves to stdin is a flag of that command,
// and so is the -stdin twin it puts in its place.
func TestConsoleStdinTwinsAreMPsFlags(t *testing.T) {
	root := cli.NewRoot()
	for path, twins := range api.ConsoleStdinTwins {
		c, _, err := root.Find(strings.Fields(path))
		if err != nil || c.CommandPath() != root.Name()+" "+path {
			t.Errorf("the console moves secrets of mp %s, which mp does not have", path)
			continue
		}
		for plain, twin := range twins {
			if c.Flags().Lookup(strings.TrimPrefix(plain, "--")) == nil {
				t.Errorf("mp %s has no %s", path, plain)
			}
			if f := c.Flags().Lookup(strings.TrimPrefix(twin, "--")); f == nil || f.Value.Type() != "bool" {
				t.Errorf("mp %s has no switch %s for %s", path, twin, plain)
			}
		}
	}
	// And every secret flag with a -stdin twin in mp is moved by the console.
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := strings.TrimPrefix(c.CommandPath(), root.Name()+" ")
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if !strings.HasSuffix(f.Name, "-stdin") || path == "setup" || path == "totp disable" || path == "migrate plan" && f.Name == "password-stdin" || path == "migrate run" && f.Name == "password-stdin" {
				return
			}
			found := false
			for _, twin := range api.ConsoleStdinTwins[path] {
				found = found || twin == "--"+f.Name
			}
			if !found {
				t.Errorf("mp %s --%s: the console does not move the secret typed for it to stdin", path, f.Name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
