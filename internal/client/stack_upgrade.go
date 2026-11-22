package client

import (
	"context"
	"net/http"

	"monopanel/internal/apitypes"
)

// StackUpgrade starts the upgrade of a component's packages (nginx, php,
// apache, db; "" or all: nginx, PHP and Apache).
func (c *Client) StackUpgrade(ctx context.Context, component string) (*apitypes.JobRef, error) {
	var out apitypes.JobRef
	return &out, c.do(ctx, http.MethodPost, "/stack/upgrade", apitypes.StackUpgradeRequest{Component: component}, &out)
}

// StackAutoUpgrade reads the daily upgrade and how the last upgrade went.
func (c *Client) StackAutoUpgrade(ctx context.Context) (*apitypes.StackAutoUpgrade, error) {
	var out apitypes.StackAutoUpgrade
	return &out, c.do(ctx, http.MethodGet, "/stack/auto-upgrade", nil, &out)
}

// SetStackAutoUpgrade switches the daily upgrade on or off or moves it.
func (c *Client) SetStackAutoUpgrade(ctx context.Context, req apitypes.StackAutoUpgradeUpdate) (*apitypes.StackAutoUpgrade, error) {
	var out apitypes.StackAutoUpgrade
	return &out, c.do(ctx, http.MethodPut, "/stack/auto-upgrade", req, &out)
}
