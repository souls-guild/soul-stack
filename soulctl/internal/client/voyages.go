package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// VoyagesAPI holds typed methods for /v1/voyages/* (ADR-043). A Voyage is a
// unified batch run (kind=scenario|command), async by default.
//
// Bodies are wire.* — the copy this file used to carry had six fewer create
// fields than the server accepts (batch_mode, batch_percent, the inter-batch
// intervals, require_alive, notify) and typed schedule_at as a string, so a
// soulctl user could not reach half the operation (NIM-776).
type VoyagesAPI struct {
	c *Client
}

// VoyageListOptions holds filters for GET /v1/voyages. Not a wire type: these
// are query parameters, not a body.
type VoyageListOptions struct {
	Kind   string
	Status []string
	Offset int
	Limit  int
}

// Create is POST /v1/voyages (ADR-043). Async by default: always 202.
func (a *VoyagesAPI) Create(ctx context.Context, req wire.VoyageCreateRequest) (*wire.VoyageCreateReply, error) {
	if req.Kind == "" {
		return nil, fmt.Errorf("kind is empty")
	}
	var reply wire.VoyageCreateReply
	if err := a.c.Do(ctx, "POST", "/v1/voyages", req, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Get is GET /v1/voyages/{id}.
func (a *VoyagesAPI) Get(ctx context.Context, voyageID string) (*wire.Voyage, error) {
	if voyageID == "" {
		return nil, fmt.Errorf("voyage_id is empty")
	}
	var reply wire.Voyage
	if err := a.c.Do(ctx, "GET", "/v1/voyages/"+url.PathEscape(voyageID), nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// List is GET /v1/voyages (multi-value status, OR semantics).
func (a *VoyagesAPI) List(ctx context.Context, opts VoyageListOptions) (*wire.VoyageListReply, error) {
	q := url.Values{}
	if opts.Kind != "" {
		q.Set("kind", opts.Kind)
	}
	for _, s := range opts.Status {
		q.Add("status", s)
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	path := "/v1/voyages"
	if enc := q.Encode(); enc != "" {
		path = path + "?" + enc
	}
	var reply wire.VoyageListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Cancel is DELETE /v1/voyages/{id} (ADR-043 S5): cancels pending/scheduled.
func (a *VoyagesAPI) Cancel(ctx context.Context, voyageID string) (*wire.VoyageCancelReply, error) {
	if voyageID == "" {
		return nil, fmt.Errorf("voyage_id is empty")
	}
	var reply wire.VoyageCancelReply
	if err := a.c.Do(ctx, "DELETE", "/v1/voyages/"+url.PathEscape(voyageID), nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
