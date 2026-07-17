package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// PushProvidersAPI holds typed methods for /v1/push-providers/* (ADR-032
// amendment 2026-05-26, S7-2). A Push-Provider holds per-provider
// env-payload params for the push-flow SSH plugin (NOT a Cloud Provider —
// that's a different entity, with separate tables and permission scopes).
type PushProvidersAPI struct {
	c *Client
}

// PushProviderBody is the body for POST /v1/push-providers (create).
type PushProviderBody struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params,omitempty"`
}

// PushProviderUpdateBody is the body for PUT /v1/push-providers/{name} (replace).
type PushProviderUpdateBody struct {
	Params map[string]any `json:"params"`
}

// PushProviderEntry is the JSON shape of a Push-Provider in responses.
type PushProviderEntry struct {
	Name         string         `json:"name"`
	Params       map[string]any `json:"params"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	CreatedByAID string         `json:"created_by_aid"`
	UpdatedByAID *string        `json:"updated_by_aid,omitempty"`
}

// PushProviderListReply is a list page.
type PushProviderListReply struct {
	Items  []PushProviderEntry `json:"items"`
	Offset int                 `json:"offset"`
	Limit  int                 `json:"limit"`
	Total  int                 `json:"total"`
}

// PushProviderListOptions holds list filters.
type PushProviderListOptions struct {
	NamePattern string
	Limit       int
	Offset      int
}

// Create is POST /v1/push-providers. Permission: push-provider.create.
func (a *PushProvidersAPI) Create(ctx context.Context, body PushProviderBody) (*PushProviderEntry, error) {
	if body.Name == "" {
		return nil, fmt.Errorf("name is empty")
	}
	var reply PushProviderEntry
	if err := a.c.Do(ctx, "POST", "/v1/push-providers", body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Update is PUT /v1/push-providers/{name}. Permission: push-provider.update.
func (a *PushProvidersAPI) Update(ctx context.Context, name string, body PushProviderUpdateBody) (*PushProviderEntry, error) {
	if name == "" {
		return nil, fmt.Errorf("name is empty")
	}
	var reply PushProviderEntry
	if err := a.c.Do(ctx, "PUT", "/v1/push-providers/"+name, body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Delete is DELETE /v1/push-providers/{name}. Permission: push-provider.delete.
func (a *PushProvidersAPI) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	return a.c.Do(ctx, "DELETE", "/v1/push-providers/"+name, nil, nil)
}

// Get is GET /v1/push-providers/{name}. Permission: push-provider.read.
func (a *PushProvidersAPI) Get(ctx context.Context, name string) (*PushProviderEntry, error) {
	if name == "" {
		return nil, fmt.Errorf("name is empty")
	}
	var reply PushProviderEntry
	if err := a.c.Do(ctx, "GET", "/v1/push-providers/"+name, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// List is GET /v1/push-providers. Permission: push-provider.list.
func (a *PushProvidersAPI) List(ctx context.Context, opts PushProviderListOptions) (*PushProviderListReply, error) {
	q := url.Values{}
	if opts.NamePattern != "" {
		q.Set("name_pattern", opts.NamePattern)
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
	var reply PushProviderListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
