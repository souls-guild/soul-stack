package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// SoulsAPI — типизированные методы /v1/souls/*.
type SoulsAPI struct {
	c *Client
}

// SoulListOptions — фильтры list (coven передаётся повторяющимся query-параметром
// по openapi: `style: form, explode: true`).
type SoulListOptions struct {
	Covens    []string
	Status    string
	Transport string
	Limit     int
	Offset    int
}

// SoulListEntry — проекция реестра souls (SoulListEntry в openapi.yaml).
type SoulListEntry struct {
	SID          string   `json:"sid"`
	Transport    string   `json:"transport"`
	Status       string   `json:"status"`
	Covens       []string `json:"covens,omitempty"`
	LastSeenAt   string   `json:"last_seen_at,omitempty"`
	LastSeenByKI string   `json:"last_seen_by_kid,omitempty"`
	RegisteredAt string   `json:"registered_at"`
}

// SoulListReply — страница списка.
type SoulListReply struct {
	Items  []SoulListEntry `json:"items"`
	Offset int32           `json:"offset"`
	Limit  int32           `json:"limit"`
	Total  int32           `json:"total"`
}

// List — GET /v1/souls. coven фильтр в openapi — repeated query-param.
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

// SoulSshTargetBody — body PUT /v1/souls/{sid}/ssh-target (ADR-032 amendment
// 2026-05-26, S7-1; расширено 2026-05-27, P2 W-1). Поля
// `ssh_port`/`ssh_user`/`soul_path` required; `ssh_provider` — optional
// per-SID explicit-выбор SshProvider-плагина (Level 1 в 3-tier routing).
type SoulSshTargetBody struct {
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	SoulPath    string `json:"soul_path"`
	SSHProvider string `json:"ssh_provider,omitempty"`
}

// SoulSshTargetReply — 200-body PUT /v1/souls/{sid}/ssh-target.
type SoulSshTargetReply struct {
	SID       string            `json:"sid"`
	SSHTarget SoulSshTargetBody `json:"ssh_target"`
}

// SetSshTarget — PUT /v1/souls/{sid}/ssh-target. Permission: soul.ssh-target-update.
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

// Get — GET /v1/souls/{sid} в openapi MVP отсутствует (нет permission soul.get,
// см. operator-api.md → ID в path). Фоллбэк: тянем list с большим лимитом и
// фильтруем client-side. Это known limitation, см. soulctl/README.md.
func (a *SoulsAPI) Get(ctx context.Context, sid string) (*SoulListEntry, error) {
	if sid == "" {
		return nil, fmt.Errorf("SID пуст")
	}
	// Пагинируем по 1000 (max по openapi PaginationRequest); если в кластере
	// больше — расширим, но для MVP-CLI достаточно.
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
