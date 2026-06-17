package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// PushProvidersAPI — типизированные методы /v1/push-providers/* (ADR-032
// amendment 2026-05-26, S7-2). Push-Provider — per-provider env-payload
// params SSH-плагина push-flow (НЕ Cloud Provider — это другая сущность,
// разные таблицы и permission-области).
type PushProvidersAPI struct {
	c *Client
}

// PushProviderBody — body POST /v1/push-providers (create).
type PushProviderBody struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params,omitempty"`
}

// PushProviderUpdateBody — body PUT /v1/push-providers/{name} (replace).
type PushProviderUpdateBody struct {
	Params map[string]any `json:"params"`
}

// PushProviderEntry — JSON-форма Push-Provider-а в ответах.
type PushProviderEntry struct {
	Name         string         `json:"name"`
	Params       map[string]any `json:"params"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	CreatedByAID string         `json:"created_by_aid"`
	UpdatedByAID *string        `json:"updated_by_aid,omitempty"`
}

// PushProviderListReply — страница списка.
type PushProviderListReply struct {
	Items  []PushProviderEntry `json:"items"`
	Offset int                 `json:"offset"`
	Limit  int                 `json:"limit"`
	Total  int                 `json:"total"`
}

// PushProviderListOptions — фильтры list.
type PushProviderListOptions struct {
	NamePattern string
	Limit       int
	Offset      int
}

// Create — POST /v1/push-providers. Permission: push-provider.create.
func (a *PushProvidersAPI) Create(ctx context.Context, body PushProviderBody) (*PushProviderEntry, error) {
	if body.Name == "" {
		return nil, fmt.Errorf("name пуст")
	}
	var reply PushProviderEntry
	if err := a.c.Do(ctx, "POST", "/v1/push-providers", body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Update — PUT /v1/push-providers/{name}. Permission: push-provider.update.
func (a *PushProvidersAPI) Update(ctx context.Context, name string, body PushProviderUpdateBody) (*PushProviderEntry, error) {
	if name == "" {
		return nil, fmt.Errorf("name пуст")
	}
	var reply PushProviderEntry
	if err := a.c.Do(ctx, "PUT", "/v1/push-providers/"+name, body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Delete — DELETE /v1/push-providers/{name}. Permission: push-provider.delete.
func (a *PushProvidersAPI) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("name пуст")
	}
	return a.c.Do(ctx, "DELETE", "/v1/push-providers/"+name, nil, nil)
}

// Get — GET /v1/push-providers/{name}. Permission: push-provider.read.
func (a *PushProvidersAPI) Get(ctx context.Context, name string) (*PushProviderEntry, error) {
	if name == "" {
		return nil, fmt.Errorf("name пуст")
	}
	var reply PushProviderEntry
	if err := a.c.Do(ctx, "GET", "/v1/push-providers/"+name, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// List — GET /v1/push-providers. Permission: push-provider.list.
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
