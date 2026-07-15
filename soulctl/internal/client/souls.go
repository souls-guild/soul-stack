package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// SoulsAPI holds typed methods for /v1/souls/*.
type SoulsAPI struct {
	c *Client
}

// SoulListOptions holds list filters (coven is passed as a repeated query
// parameter per openapi: `style: form, explode: true`).
type SoulListOptions struct {
	Covens    []string
	Status    string
	Transport string
	Limit     int
	Offset    int
}

// SoulListEntry is a projection of the souls registry (SoulListEntry in openapi.yaml).
type SoulListEntry struct {
	SID          string   `json:"sid"`
	Transport    string   `json:"transport"`
	Status       string   `json:"status"`
	Covens       []string `json:"covens,omitempty"`
	LastSeenAt   string   `json:"last_seen_at,omitempty"`
	LastSeenByKI string   `json:"last_seen_by_kid,omitempty"`
	RegisteredAt string   `json:"registered_at"`
}

// SoulListReply is a list page.
type SoulListReply struct {
	Items  []SoulListEntry `json:"items"`
	Offset int32           `json:"offset"`
	Limit  int32           `json:"limit"`
	Total  int32           `json:"total"`
}

// List is GET /v1/souls. The coven filter in openapi is a repeated query param.
func (a *SoulsAPI) List(ctx context.Context, opts SoulListOptions) (*SoulListReply, error) {
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
	var reply SoulListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// SoulSshTargetBody is the body for PUT /v1/souls/{sid}/ssh-target (ADR-032
// amendment 2026-05-26, S7-1; extended 2026-05-27, P2 W-1). Fields
// `ssh_port`/`ssh_user`/`soul_path` are required; `ssh_provider` is an
// optional per-SID explicit SshProvider plugin choice (Level 1 in the
// 3-tier routing).
type SoulSshTargetBody struct {
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	SoulPath    string `json:"soul_path"`
	SSHProvider string `json:"ssh_provider,omitempty"`
}

// SoulSshTargetReply is the 200 body for PUT /v1/souls/{sid}/ssh-target.
type SoulSshTargetReply struct {
	SID       string            `json:"sid"`
	SSHTarget SoulSshTargetBody `json:"ssh_target"`
}

// SetSshTarget is PUT /v1/souls/{sid}/ssh-target. Permission: soul.ssh-target-update.
func (a *SoulsAPI) SetSshTarget(ctx context.Context, sid string, body SoulSshTargetBody) (*SoulSshTargetReply, error) {
	if sid == "" {
		return nil, fmt.Errorf("SID пуст")
	}
	var reply SoulSshTargetReply
	if err := a.c.Do(ctx, "PUT", "/v1/souls/"+sid+"/ssh-target", body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Get: GET /v1/souls/{sid} doesn't exist in the openapi MVP (no soul.get
// permission, see operator-api.md → ID in path). Fallback: fetch list with a
// large limit and filter client-side. This is a known limitation, see
// soulctl/README.md.
func (a *SoulsAPI) Get(ctx context.Context, sid string) (*SoulListEntry, error) {
	if sid == "" {
		return nil, fmt.Errorf("SID пуст")
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
		Detail: fmt.Sprintf("soul %s не найден в реестре", sid),
		Method: "GET",
		Path:   "/v1/souls (filtered by sid)",
	}
}
