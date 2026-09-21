package client

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// PushAPI holds thin methods for the push flow (`POST /v1/push/apply`).
// Per-id read, list/cleanup — extend with a separate slice once the need
// arises (currently called from `soulctl run push`).
type PushAPI struct {
	c *Client
}

// Apply is POST /v1/push/apply. Inventory/Destiny are required.
func (a *PushAPI) Apply(ctx context.Context, req wire.PushApplyRequest) (*wire.PushApplyReply, error) {
	if len(req.Inventory) == 0 {
		return nil, fmt.Errorf("inventory is empty: at least one SID is required")
	}
	if req.Destiny == "" {
		return nil, fmt.Errorf("destiny is empty: a <name>@<ref> reference is required")
	}
	var reply wire.PushApplyReply
	if err := a.c.Do(ctx, "POST", "/v1/push/apply", req, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
