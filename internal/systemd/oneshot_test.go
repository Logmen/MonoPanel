package systemd

import (
	"slices"
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
}
