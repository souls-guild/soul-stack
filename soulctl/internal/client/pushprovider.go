package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// PushProvidersAPI holds typed methods for /v1/push-providers/* (ADR-032
// amendment 2026-05-26, S7-2). A Push-Provider holds per-provider
// env-payload params for the push-flow SSH plugin (NOT a Cloud Provider —
// that's a different entity, with separate tables and permission scopes).
//
// The request and reply bodies are wire.* — the same declarations keeper's
// handlers use. This package used to restate them, which is how it kept
// sending `name` after NIM-729 renamed the field to `id`: the copy compiled
// perfectly and the server answered 400 (NIM-776).
type PushProvidersAPI struct {
	c *Client
}

// PushProviderListOptions holds list filters. Not a wire type: these become
// query parameters, which the server declares one at a time on the operation
// rather than as a body schema.
type PushProviderListOptions struct {
	IDPattern string
	Limit     int
	Offset    int
}

// Create is POST /v1/push-providers. Permission: push-provider.create.
func (a *PushProvidersAPI) Create(ctx context.Context, body wire.PushProviderCreateRequest) (*wire.PushProvider, error) {
	if body.ID == "" {
		return nil, fmt.Errorf("id is empty")
	}
	var reply wire.PushProvider
	if err := a.c.Do(ctx, "POST", "/v1/push-providers", body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Update is PUT /v1/push-providers/{id} (replace semantics). Permission: push-provider.update.
func (a *PushProvidersAPI) Update(ctx context.Context, id string, body wire.PushProviderUpdateRequest) (*wire.PushProvider, error) {
	if id == "" {
		return nil, fmt.Errorf("id is empty")
	}
	var reply wire.PushProvider
	if err := a.c.Do(ctx, "PUT", "/v1/push-providers/"+id, body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Delete is DELETE /v1/push-providers/{id}. Permission: push-provider.delete.
func (a *PushProvidersAPI) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is empty")
	}
	return a.c.Do(ctx, "DELETE", "/v1/push-providers/"+id, nil, nil)
}

// Get is GET /v1/push-providers/{id}. Permission: push-provider.read.
func (a *PushProvidersAPI) Get(ctx context.Context, id string) (*wire.PushProvider, error) {
	if id == "" {
		return nil, fmt.Errorf("id is empty")
	}
	var reply wire.PushProvider
	if err := a.c.Do(ctx, "GET", "/v1/push-providers/"+id, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// List is GET /v1/push-providers. Permission: push-provider.list.
func (a *PushProvidersAPI) List(ctx context.Context, opts PushProviderListOptions) (*wire.PushProviderListReply, error) {
	q := url.Values{}
	if opts.IDPattern != "" {
		q.Set("id_pattern", opts.IDPattern)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/v1/push-providers"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply wire.PushProviderListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
