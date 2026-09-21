package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// SoulsAPI holds typed methods for /v1/souls/*. Bodies are wire.* — the copy
// this file used to carry was missing `traits`, `created_by_aid` and
// `requested_at` entirely, and nothing said so (NIM-776).
type SoulsAPI struct {
	c *Client
}

// SoulListOptions holds list filters (coven is passed as a repeated query
// parameter per openapi: `style: form, explode: true`). Not a wire type:
// these are query parameters, not a body.
type SoulListOptions struct {
	Covens    []string
	Status    string
	Transport string
	Limit     int
	Offset    int
}

// List is GET /v1/souls. The coven filter in openapi is a repeated query param.
func (a *SoulsAPI) List(ctx context.Context, opts SoulListOptions) (*wire.SoulListReply, error) {
	q := url.Values{}
	for _, c := range opts.Covens {
		if c != "" {
			q.Add("coven", c)
		}
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Transport != "" {
		q.Set("transport", opts.Transport)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/v1/souls"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply wire.SoulListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// SetSshTarget is PUT /v1/souls/{sid}/ssh-target. Permission: soul.ssh-target-update.
func (a *SoulsAPI) SetSshTarget(ctx context.Context, sid string, body wire.SoulSshTarget) (*wire.SoulSshTargetReply, error) {
	if sid == "" {
		return nil, fmt.Errorf("SID is empty")
	}
	var reply wire.SoulSshTargetReply
	if err := a.c.Do(ctx, "PUT", "/v1/souls/"+sid+"/ssh-target", body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Get: GET /v1/souls/{sid} doesn't exist in the openapi MVP (no soul.get
// permission, see operator-api.md → ID in path). Fallback: fetch list with a
// large limit and filter client-side. This is a known limitation, see
// soulctl/README.md.
func (a *SoulsAPI) Get(ctx context.Context, sid string) (*wire.SoulListEntry, error) {
	if sid == "" {
		return nil, fmt.Errorf("SID is empty")
	}
	// Paginate in pages of 1000 (max per openapi PaginationRequest); if the
	// cluster has more, we'll extend this, but it's enough for the MVP CLI.
	const pageLimit = 1000
	offset := 0
	for {
		reply, err := a.List(ctx, SoulListOptions{Limit: pageLimit, Offset: offset})
		if err != nil {
			return nil, err
		}
		for i := range reply.Items {
			if reply.Items[i].SID == sid {
				return &reply.Items[i], nil
			}
		}
		offset += len(reply.Items)
		if len(reply.Items) < pageLimit || offset >= int(reply.Total) {
			break
		}
	}
	return nil, &APIError{
		Status: 404,
		Title:  "not-found",
		Detail: fmt.Sprintf("soul %s not found in registry", sid),
		Method: "GET",
		Path:   "/v1/souls (filtered by sid)",
	}
}
