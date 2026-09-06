// Package systemd wraps the systemd D-Bus API. The agent and `mp setup` use
// it instead of shelling out to systemctl.
package systemd

import (
	"context"
	"fmt"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// Conn is a connection to the system manager.
type Conn struct{ c *sd.Conn }

// Connect opens the system bus connection.
func Connect(ctx context.Context) (*Conn, error) {
	c, err := sd.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("systemd dbus: %w", err)
	}
	return &Conn{c: c}, nil
}

// Close releases the connection.
func (c *Conn) Close() { c.c.Close() }

type unitOp func(ctx context.Context, name, mode string, ch chan<- string) (int, error)

func (c *Conn) wait(ctx context.Context, op string, unit string, fn unitOp) error {
	ch := make(chan string, 1)
	if _, err := fn(ctx, unit, "replace", ch); err != nil {
		return fmt.Errorf("%s %s: %w", op, unit, err)
	}
	select {
	case res := <-ch:
		if res != "done" {
			return fmt.Errorf("%s %s: job result %q", op, unit, res)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start starts a unit and waits for the job to finish.
func (c *Conn) Start(ctx context.Context, unit string) error {
	return c.wait(ctx, "start", unit, c.c.StartUnitContext)
}

// Stop stops a unit.
func (c *Conn) Stop(ctx context.Context, unit string) error {
	return c.wait(ctx, "stop", unit, c.c.StopUnitContext)
}

// Reload reloads a unit (SIGHUP for nginx, USR2 for php-fpm, graceful for apache).
func (c *Conn) Reload(ctx context.Context, unit string) error {
	return c.wait(ctx, "reload", unit, c.c.ReloadUnitContext)
}

// Restart restarts a unit.
func (c *Conn) Restart(ctx context.Context, unit string) error {
	return c.wait(ctx, "restart", unit, c.c.RestartUnitContext)
}

// ReloadOrRestart reloads when the unit supports it, otherwise restarts.
func (c *Conn) ReloadOrRestart(ctx context.Context, unit string) error {
	return c.wait(ctx, "reload-or-restart", unit, c.c.ReloadOrRestartUnitContext)
}

// Enable enables a unit for boot.
func (c *Conn) Enable(ctx context.Context, unit string) error {
	if _, _, err := c.c.EnableUnitFilesContext(ctx, []string{unit}, false, true); err != nil {
		return fmt.Errorf("enable %s: %w", unit, err)
	}
	return nil
}

// Disable disables a unit.
func (c *Conn) Disable(ctx context.Context, unit string) error {
	if _, err := c.c.DisableUnitFilesContext(ctx, []string{unit}, false); err != nil {
		return fmt.Errorf("disable %s: %w", unit, err)
	}
	return nil
}

// DaemonReload re-reads unit files.
func (c *Conn) DaemonReload(ctx context.Context) error { return c.c.ReloadContext(ctx) }

// Status is a summary of a unit's state.
type Status struct {
	Unit          string    `json:"unit"`
	LoadState     string    `json:"load_state"`
	ActiveState   string    `json:"active_state"`
	SubState      string    `json:"sub_state"`
	UnitFileState string    `json:"unit_file_state"`
	ActiveSince   time.Time `json:"active_since,omitempty"`
}

// Status returns the state of a unit.
func (c *Conn) Status(ctx context.Context, unit string) (Status, error) {
	props, err := c.c.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return Status{}, fmt.Errorf("status %s: %w", unit, err)
	}
	s := Status{Unit: unit}
	str := func(k string) string {
		if v, ok := props[k].(string); ok {
			return v
		}
		return ""
	}
	s.LoadState = str("LoadState")
	s.ActiveState = str("ActiveState")
	s.SubState = str("SubState")
	s.UnitFileState = str("UnitFileState")
	if ts, ok := props["ActiveEnterTimestamp"].(uint64); ok && ts > 0 {
		s.ActiveSince = time.UnixMicro(int64(ts))
	}
	return s, nil
}

// RunDetached starts a transient one-shot unit and returns as soon as systemd
// has accepted the job. The command keeps running when the caller exits or is
// restarted, which is what makes a self-update possible.
func (c *Conn) RunDetached(ctx context.Context, unit, description string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("run %s: no command", unit)
	}
	// A finished transient unit lingers when it failed; clear it or systemd
	// refuses to reuse the name.
	_ = c.c.ResetFailedUnitContext(ctx, unit)
	props := []sd.Property{
		sd.PropDescription(description),
		sd.PropExecStart(argv, false),
		{Name: "Type", Value: dbus.MakeVariant("oneshot")},
		{Name: "CollectMode", Value: dbus.MakeVariant("inactive-or-failed")},
		{Name: "TimeoutStartUSec", Value: dbus.MakeVariant(uint64(30 * 60 * 1e6))},
		{Name: "StandardOutput", Value: dbus.MakeVariant("journal")},
		{Name: "StandardError", Value: dbus.MakeVariant("journal")},
	}
	// The start job of a one-shot unit only completes when the command exits;
	// the buffered channel takes that result long after this call returned.
	if _, err := c.c.StartTransientUnitContext(ctx, unit, "replace", props, make(chan string, 1)); err != nil {
		return fmt.Errorf("start %s: %w", unit, err)
	}
	return nil
}
