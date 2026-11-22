package systemd

import (
	"context"
	"fmt"
)

// MainPID is the main process of a service unit, 0 when it has none.
func (c *Conn) MainPID(ctx context.Context, unit string) (uint32, error) {
	p, err := c.c.GetServicePropertyContext(ctx, unit, "MainPID")
	if err != nil {
		return 0, fmt.Errorf("MainPID of %s: %w", unit, err)
	}
	pid, _ := p.Value.Value().(uint32)
	return pid, nil
}

// PIDFile is the PID file a service unit names (PIDFile=), "" for none.
func (c *Conn) PIDFile(ctx context.Context, unit string) (string, error) {
	p, err := c.c.GetServicePropertyContext(ctx, unit, "PIDFile")
	if err != nil {
		return "", fmt.Errorf("PIDFile of %s: %w", unit, err)
	}
	f, _ := p.Value.Value().(string)
	return f, nil
}
