package systemd

import (
	"context"
	"fmt"
	"strings"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// Oneshot is a command systemd runs for the caller as a transient one-shot
// service (StartOneshot). The command is systemd's child in a cgroup of its
// own, not the caller's: stopping or restarting the caller's unit, whose
// KillMode=control-group kills all of its cgroup, leaves it running. Nor does
// it share the caller's namespaces: the caller's PrivateTmp, which systemd
// removes when the caller stops, is not its /tmp.
type Oneshot struct {
	Unit        string
	Description string
	// Path is the program, absolute; Argv its arguments, Argv[0] included.
	Path string
	Argv []string
	// Env is set on top of what systemd gives a service.
	Env []string
	// Output is the descriptor the command's standard output and error go
	// to: a file of the caller's, of which systemd gets a copy. Nothing is
	// written into a pipe the caller holds, whose reader could die halfway.
	Output int
	// Limit is how long the command may run (TimeoutStartSec): then systemd
	// stops it, whether the caller is still there or not.
	Limit time.Duration
	// StopLimit is how long each step of the unit's end may take
	// (TimeoutStopSec): the command's exit after SIGTERM before SIGKILL,
	// each command of StopPost. Zero leaves it to systemd's default
	// (DefaultTimeoutStopSec, which an administrator may have lowered).
	StopLimit time.Duration
	// StopPost are the commands systemd runs once the command is over,
	// however it ended — exited, killed, stopped at the limit or by the
	// caller (ExecStopPost) —, one after the other, with the unit's settings
	// and in its namespaces (its PrivateTmp among them), each within
	// StopLimit rather than Limit; their output goes to Output too. The start
	// job's result comes once they are over, and so does that of a stop. A
	// failure of one with IgnoreFailure leaves the unit's result and the main
	// process's exit as the command left them (MainExit) — but not one still
	// going at StopLimit: systemd kills it and fails the unit
	// (Result=timeout) whatever IgnoreFailure says, so each must end well
	// within StopLimit by itself.
	StopPost []Command
	// Settings are further properties of the unit by their D-Bus names, with
	// values of the D-Bus types systemd takes for them.
	Settings []Setting
}

// Command is a command of a unit's beside the main one: the program,
// absolute, and its arguments, Argv[0] included, as systemd runs them (no
// shell; systemd expands $NAME in them, nothing else).
type Command struct {
	Path string
	Argv []string
	// IgnoreFailure keeps a failure of the command — an exit status other
	// than 0, a signal, a program that does not start — out of the unit's
	// result: the "-" before a command line of a unit file. A command still
	// going at the unit's TimeoutStopSec fails the unit all the same
	// (Oneshot.StopPost).
	IgnoreFailure bool
}

// Setting is a property of a transient unit (ProtectHome, PrivateTmp …).
type Setting struct {
	Name  string
	Value any
}

// execCommand is an entry of ExecStart or ExecStopPost, a(sasb): the
// program, its argv, and whether a failure is ignored — never for the main
// command, whose failure is the run's.
type execCommand struct {
	Path   string
	Argv   []string
	Ignore bool
}

// properties are the D-Bus properties of o's unit. Type=oneshot keeps the
// start job open until the command exits, so the job's result is the
// command's; stdin is /dev/null.
func (o Oneshot) properties() []sd.Property {
	props := []sd.Property{
		sd.PropDescription(o.Description),
		{Name: "Type", Value: dbus.MakeVariant("oneshot")},
		{Name: "ExecStart", Value: dbus.MakeVariant([]execCommand{{Path: o.Path, Argv: o.Argv}})},
		{Name: "StandardInput", Value: dbus.MakeVariant("null")},
		{Name: "StandardOutputFileDescriptor", Value: dbus.MakeVariant(dbus.UnixFD(o.Output))},
		{Name: "StandardErrorFileDescriptor", Value: dbus.MakeVariant(dbus.UnixFD(o.Output))},
		{Name: "TimeoutStartUSec", Value: dbus.MakeVariant(uint64(o.Limit.Microseconds()))},
	}
	if o.StopLimit > 0 {
		props = append(props, sd.Property{Name: "TimeoutStopUSec", Value: dbus.MakeVariant(uint64(o.StopLimit.Microseconds()))})
	}
	if len(o.StopPost) > 0 {
		cmds := make([]execCommand, 0, len(o.StopPost))
		for _, c := range o.StopPost {
			cmds = append(cmds, execCommand{Path: c.Path, Argv: c.Argv, Ignore: c.IgnoreFailure})
		}
		props = append(props, sd.Property{Name: "ExecStopPost", Value: dbus.MakeVariant(cmds)})
	}
	if len(o.Env) > 0 {
		props = append(props, sd.Property{Name: "Environment", Value: dbus.MakeVariant(o.Env)})
	}
	for _, s := range o.Settings {
		props = append(props, sd.Property{Name: s.Name, Value: dbus.MakeVariant(s.Value)})
	}
	return props
}

// check tells a unit systemd would refuse, or run otherwise than meant: a
// command without a program or arguments, one after it whose program is not
// given by its absolute path.
func (o Oneshot) check() error {
	if o.Path == "" || len(o.Argv) == 0 {
		return fmt.Errorf("run %s: no command", o.Unit)
	}
	for _, c := range o.StopPost {
		if !strings.HasPrefix(c.Path, "/") || len(c.Argv) == 0 {
			return fmt.Errorf("run %s: a command after it needs an absolute program and its arguments: %q %q", o.Unit, c.Path, c.Argv)
		}
	}
	return nil
}

// StartOneshot starts o and returns once systemd has taken it: the channel
// receives the result of its start job once the command has exited and the
// commands after it (StopPost) are over ("done" when it exited with 0). No
// reference of the caller's holds the unit, so the caller going away
// changes nothing for it; a unit of that name that exists already makes the
// start fail.
func (c *Conn) StartOneshot(ctx context.Context, o Oneshot) (<-chan string, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	ch := make(chan string, 1)
	if _, err := c.c.StartTransientUnitContext(ctx, o.Unit, "fail", o.properties(), ch); err != nil {
		return nil, fmt.Errorf("start %s: %w", o.Unit, err)
	}
	return ch, nil
}

// Exit is how the main process of a service ended.
type Exit struct {
	// Code is CLD_EXITED (1), CLD_KILLED (2) or CLD_DUMPED (3), 0 when the
	// process never ran; Status is the exit status or the signal.
	Code, Status int32
	// Result is the unit's: success, exit-code, signal, timeout, oom-kill …
	Result string
}

// MainExit is how a finished service's main process ended. A failed unit
// keeps it until ResetFailed; systemd collects a successful one at once.
func (c *Conn) MainExit(ctx context.Context, unit string) (Exit, error) {
	var e Exit
	for _, p := range []struct {
		name string
		to   any
	}{{"ExecMainCode", &e.Code}, {"ExecMainStatus", &e.Status}, {"Result", &e.Result}} {
		v, err := c.c.GetServicePropertyContext(ctx, unit, p.name)
		if err != nil {
			return e, fmt.Errorf("%s of %s: %w", p.name, unit, err)
		}
		if err := v.Value.Store(p.to); err != nil {
			return e, fmt.Errorf("%s of %s: %w", p.name, unit, err)
		}
	}
	return e, nil
}

// Running lists the units matching the patterns that run or are about to:
// activating, active, deactivating or reloading, or with a job queued.
func (c *Conn) Running(ctx context.Context, patterns ...string) ([]string, error) {
	units, err := c.c.ListUnitsByPatternsContext(ctx, nil, patterns)
	if err != nil {
		return nil, fmt.Errorf("list %v: %w", patterns, err)
	}
	var out []string
	for _, u := range units {
		switch {
		case u.JobId != 0, u.ActiveState == "activating", u.ActiveState == "active", u.ActiveState == "deactivating", u.ActiveState == "reloading", u.ActiveState == "refreshing":
			out = append(out, u.Name)
		}
	}
	return out, nil
}
