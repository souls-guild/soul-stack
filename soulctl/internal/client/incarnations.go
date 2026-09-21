package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// IncarnationsAPI holds typed methods for /v1/incarnations/*. Exposed as the
// Client.Incarnations field.
//
// Every body here is a wire.* type — the same declaration keeper's handler
// returns. The copies this file used to carry had drifted: they still had a
// `spec` field the server stopped sending, and none of `label`, `traits` or
// `applying_apply_id` that it started sending (NIM-776).
type IncarnationsAPI struct {
	c *Client
}

// IncarnationListOptions holds list filters. Not a wire type: service/status
// are query parameters and coven is filtered client-side (see List).
type IncarnationListOptions struct {
	Service string
	Status  string
	Coven   string
	Limit   int
	Offset  int
}

// List is GET /v1/incarnations. `coven` isn't defined as a filter on this
// endpoint by the openapi schema (the coven filter only exists on
// /v1/souls), so the filter is applied client-side after fetching the page.
// The server returns offset/limit/total for service/status; for coven the
// values won't be consistent with total — this is a known limitation,
// documented in the README.
func (a *IncarnationsAPI) List(ctx context.Context, opts IncarnationListOptions) (*wire.IncarnationListReply, error) {
	q := url.Values{}
	if opts.Service != "" {
		q.Set("service", opts.Service)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/v1/incarnations"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply wire.IncarnationListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	if opts.Coven != "" {
		filtered := reply.Items[:0]
		for _, it := range reply.Items {
			for _, c := range it.Covens {
				if c == opts.Coven {
					filtered = append(filtered, it)
					break
				}
			}
		}
		reply.Items = filtered
	}
	return &reply, nil
}

// Get is GET /v1/incarnations/{id}.
func (a *IncarnationsAPI) Get(ctx context.Context, id string) (*wire.IncarnationGetReply, error) {
	if id == "" {
		return nil, fmt.Errorf("incarnation id is empty")
	}
	var item wire.IncarnationGetReply
	if err := a.c.Do(ctx, "GET", "/v1/incarnations/"+url.PathEscape(id), nil, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// Run is POST /v1/incarnations/{id}/scenarios/{scenario}.
//
// It used to append ?dry_run=true for a --dry-run flag, on the reasoning that
// "the server will either honor it or ignore it, which is safe either way".
// That reasoning was wrong in the only direction that mattered: the operation
// has never declared the parameter, so the server ignored it and applied for
// real while the operator was told it was a rehearsal. Flag and parameter both
// removed in NIM-446 — a read-only check is an Errand dry-run, which is wired.
func (a *IncarnationsAPI) Run(ctx context.Context, id, scenario string, input map[string]any) (*wire.IncarnationRunReply, error) {
	if id == "" || scenario == "" {
		return nil, fmt.Errorf("incarnation/scenario are empty")
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", url.PathEscape(id), url.PathEscape(scenario))
	body := wire.IncarnationRunRequest{Input: input}
	var reply wire.IncarnationRunReply
	if err := a.c.Do(ctx, "POST", path, body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// History is GET /v1/incarnations/{id}/history.
func (a *IncarnationsAPI) History(ctx context.Context, id string, limit, offset int) (*wire.IncarnationHistoryReply, error) {
	if id == "" {
		return nil, fmt.Errorf("incarnation id is empty")
	}
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/incarnations/" + url.PathEscape(id) + "/history"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply wire.IncarnationHistoryReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Runs is GET /v1/incarnations/{id}/runs.
func (a *IncarnationsAPI) Runs(ctx context.Context, id string, limit, offset int) (*wire.IncarnationRunsReply, error) {
	if id == "" {
		return nil, fmt.Errorf("incarnation id is empty")
	}
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/incarnations/" + url.PathEscape(id) + "/runs"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply wire.IncarnationRunsReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// RunDetail is GET /v1/incarnations/{id}/runs/{apply_id}.
func (a *IncarnationsAPI) RunDetail(ctx context.Context, id, applyID string) (*wire.RunDetailReply, error) {
	if id == "" {
		return nil, fmt.Errorf("incarnation id is empty")
	}
	if applyID == "" {
		return nil, fmt.Errorf("apply_id is empty")
	}
	var detail wire.RunDetailReply
	path := "/v1/incarnations/" + url.PathEscape(id) + "/runs/" + url.PathEscape(applyID)
	if err := a.c.Do(ctx, "GET", path, nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}
