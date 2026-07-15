package client

import (
	"context"
	"fmt"
)

// PushAPI holds thin methods for the push flow (`POST /v1/push/apply`).
// Per-id read, list/cleanup — extend with a separate slice once the need
// arises (currently called from `soulctl run push`).
type PushAPI struct {
	c *Client
}

// PushApplyRequest is the body for POST /v1/push/apply (openapi PushApplyRequest).
type PushApplyRequest struct {
	Inventory            []string       `json:"inventory"`
	Destiny              string         `json:"destiny"`
	Input                map[string]any `json:"input,omitempty"`
	SSHProvider          string         `json:"ssh_provider,omitempty"`
	CleanupStaleVersions bool           `json:"cleanup_stale_versions,omitempty"`
}

// PushApplyReply is the 202 response.
type PushApplyReply struct {
	ApplyID string `json:"apply_id"`
}

// Apply is POST /v1/push/apply. Inventory/Destiny are required.
func (a *PushAPI) Apply(ctx context.Context, req PushApplyRequest) (*PushApplyReply, error) {
	if len(req.Inventory) == 0 {
		return nil, fmt.Errorf("inventory пуст: требуется хотя бы один SID")
	}
	if req.Destiny == "" {
		return nil, fmt.Errorf("destiny пуст: требуется ссылка <name>@<ref>")
	}
	var reply PushApplyReply
	if err := a.c.Do(ctx, "POST", "/v1/push/apply", req, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
