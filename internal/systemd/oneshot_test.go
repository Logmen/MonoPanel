package systemd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// A one-shot run is what systemd takes over D-Bus: each property with the
// signature systemd reads it with (a wrong one fails the start of every
// run), the command as an argv without a shell, a failure not ignored, the
// caller's descriptor for both output streams.
func TestOneshotProperties(t *testing.T) {
	o := Oneshot{
		Unit: "monopanel-pkg-0123456789abcdef.service", Description: "MonoPanel package manager: apt-get install nginx",
		Path: "/usr/bin/apt-get", Argv: []string{"apt-get", "-q", "-y", "install", "nginx"},
		Env: []string{"DEBIAN_FRONTEND=noninteractive"}, Output: 7, Limit: 2 * time.Hour,
		Settings: []Setting{{"ProtectHome", "read-only"}, {"PrivateTmp", true}, {"SystemCallArchitectures", []string{"native"}}},
	}
	props := map[string]dbus.Variant{}
	var order []string
	for _, p := range o.properties() {
		if _, dup := props[p.Name]; dup {
			t.Fatalf("%s twice", p.Name)
		}
		props[p.Name] = p.Value
		order = append(order, p.Name)
	}
	for name, sig := range map[string]string{
		"Description": "s", "Type": "s", "ExecStart": "a(sasb)", "StandardInput": "s",
		"StandardOutputFileDescriptor": "h", "StandardErrorFileDescriptor": "h", "TimeoutStartUSec": "t",
		"Environment": "as", "ProtectHome": "s", "PrivateTmp": "b", "SystemCallArchitectures": "as",
	} {
		v, ok := props[name]
		if !ok {
			t.Errorf("no %s in %v", name, order)
			continue
		}
		if got := v.Signature().String(); got != sig {
			t.Errorf("%s: signature %s, systemd reads %s", name, got, sig)
		}
	}
	if props["Type"].Value() != "oneshot" || props["StandardInput"].Value() != "null" {
		t.Errorf("type %v, stdin %v", props["Type"].Value(), props["StandardInput"].Value())
	}
	cmds, _ := props["ExecStart"].Value().([]execCommand)
	if len(cmds) != 1 || cmds[0].Path != "/usr/bin/apt-get" || !slices.Equal(cmds[0].Argv, o.Argv) || cmds[0].Ignore {
		t.Fatalf("ExecStart %+v", cmds)
	}
	for _, name := range []string{"StandardOutputFileDescriptor", "StandardErrorFileDescriptor"} {
		if fd, _ := props[name].Value().(dbus.UnixFD); fd != 7 {
			t.Errorf("%s: %v", name, props[name].Value())
		}
	}
	if us, _ := props["TimeoutStartUSec"].Value().(uint64); us != uint64((2 * time.Hour).Microseconds()) {
		t.Errorf("TimeoutStartUSec %d", us)
	}
	if env, _ := props["Environment"].Value().([]string); !slices.Equal(env, o.Env) {
		t.Errorf("Environment %v", env)
	}
	if _, ok := props["ExecStopPost"]; ok {
		t.Errorf("ExecStopPost for a run that has no command after it: %v", props["ExecStopPost"])
	}
	if _, ok := props["TimeoutStopUSec"]; ok {
		t.Errorf("TimeoutStopUSec for a run without a StopLimit: %v", props["TimeoutStopUSec"])
	}
}

// A StopLimit goes to systemd as TimeoutStopUSec, microseconds as a uint64
// ("t"): the unit's own, which systemd's DefaultTimeoutStopSec does not
// change.
func TestOneshotStopLimit(t *testing.T) {
	o := Oneshot{Unit: "monopanel-pkg-0123456789abcdef.service", Path: "/usr/bin/dnf", Argv: []string{"dnf", "-q", "-y", "makecache"},
		Output: 7, Limit: time.Hour, StopLimit: 90 * time.Second}
	n := 0
	for _, p := range o.properties() {
		if p.Name != "TimeoutStopUSec" {
			continue
		}
		n++
		if got := p.Value.Signature().String(); got != "t" {
			t.Errorf("TimeoutStopUSec: signature %s, systemd reads t", got)
		}
		if us, _ := p.Value.Value().(uint64); us != uint64((90 * time.Second).Microseconds()) {
			t.Errorf("TimeoutStopUSec %v", p.Value.Value())
		}
	}
	if n != 1 {
		t.Fatalf("TimeoutStopUSec %d times", n)
	}
}

// StartOneshot refuses what check refuses before systemd sees it: a command
// after the run without an absolute program or arguments, which systemd
// would refuse (no arguments) or look up in its own search path (a bare
// name). The Conn is never used: no bus is reached.
func TestStartOneshotChecksTheCommandsAfterIt(t *testing.T) {
	o := Oneshot{Unit: "monopanel-pkg-0123456789abcdef.service", Path: "/usr/bin/dnf", Argv: []string{"dnf", "-q", "-y", "makecache"}, Output: 7, Limit: time.Hour}
	for _, bad := range []Command{{Path: "monopanel", Argv: []string{"monopanel", "clean-private-tmp", o.Unit}}, {Path: "/usr/bin/monopanel"}} {
		o.StopPost = []Command{bad}
		ch, err := (&Conn{}).StartOneshot(context.Background(), o)
		if err == nil || ch != nil || !strings.Contains(err.Error(), "a command after it needs an absolute program and its arguments") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// The commands after the run go to systemd as ExecStopPost, a(sasb) like
// ExecStart — a wrong signature fails the start of every run —, in their
// order, each with its own IgnoreFailure, the "-" that keeps its failure out
// of the unit's result; the main command's failure is never ignored. A
// command after the run without an absolute program or arguments is refused
// before systemd sees it.
func TestOneshotStopPost(t *testing.T) {
	clean := Command{Path: "/usr/bin/monopanel", Argv: []string{"/usr/bin/monopanel", "clean-private-tmp", "monopanel-pkg-0123456789abcdef.service"}, IgnoreFailure: true}
	note := Command{Path: "/usr/bin/logger", Argv: []string{"logger", "over"}}
	o := Oneshot{Unit: "monopanel-pkg-0123456789abcdef.service", Path: "/usr/bin/dnf", Argv: []string{"dnf", "-q", "-y", "install", "percona-server-server"},
		Output: 7, Limit: time.Hour, StopPost: []Command{clean, note}}
	var post, start dbus.Variant
	n := 0
	for _, p := range o.properties() {
		switch p.Name {
		case "ExecStopPost":
			post = p.Value
			n++
		case "ExecStart":
			start = p.Value
		}
	}
	if n != 1 {
		t.Fatalf("ExecStopPost %d times", n)
	}
	if got := post.Signature().String(); got != "a(sasb)" {
		t.Fatalf("ExecStopPost: signature %s, systemd reads a(sasb)", got)
	}
	cmds, _ := post.Value().([]execCommand)
	if len(cmds) != 2 || cmds[0].Path != clean.Path || !slices.Equal(cmds[0].Argv, clean.Argv) || !cmds[0].Ignore ||
		cmds[1].Path != note.Path || !slices.Equal(cmds[1].Argv, note.Argv) || cmds[1].Ignore {
		t.Fatalf("ExecStopPost %+v", cmds)
	}
	if main, _ := start.Value().([]execCommand); len(main) != 1 || main[0].Ignore {
		t.Fatalf("ExecStart %+v", main)
	}
	if err := o.check(); err != nil {
		t.Fatalf("a run with commands after it: %v", err)
	}
	for _, bad := range []Command{{Path: "monopanel", Argv: []string{"monopanel"}}, {Path: "/usr/bin/monopanel"}, {Argv: []string{"x"}}} {
		o.StopPost = []Command{clean, bad}
		if err := o.check(); err == nil {
			t.Errorf("a command after the run %+v was taken", bad)
		}
	}
	o.StopPost, o.Argv = nil, nil
	if err := o.check(); err == nil {
		t.Error("a run without arguments was taken")
	}
}
