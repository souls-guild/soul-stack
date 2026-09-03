package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.profile.<verb> — parity with REST POST/GET/DELETE /v1/profiles*
// (ProfileHandler, ADR-017, Cloud CRUD). A thin MCP wrapper over the same
// profile.Service as REST. Permission mapping is 1:1, selector is
// NoSelector. NO update (Profile is immutable).
//
// Secret hygiene: VALUE params are NOT put into audit (keys only).

// profileViewOut — JSON form of the output (same as the HTTP handler).
type profileViewOut struct {
	ID string `json:"id"`
	// Label — display caption (ADR-0085); absent when the row carries none, and
	// a consumer then shows `name`.
	Label        *string        `json:"label,omitempty"`
	Provider     string         `json:"provider"`
	Params       map[string]any `json:"params"`
	CloudInit    *string        `json:"cloud_init,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
}

func toProfileViewOut(p *profile.Profile) profileViewOut {
	params := p.Params
	if params == nil {
		params = map[string]any{}
	}
	return profileViewOut{
		ID:           p.ID,
		Label:        p.Label,
		Provider:     p.Provider,
		Params:       params,
		CloudInit:    p.CloudInit,
		CreatedAt:    p.CreatedAt.UTC(),
		CreatedByAID: p.CreatedByAID,
	}
}

type profileCreateArgs struct {
	ID string `json:"id"`
	// Label — optional display caption (ADR-0085), free text; changed afterwards
	// by keeper.profile.label-set.
	Label     *string        `json:"label"`
	Provider  string         `json:"provider"`
	Params    map[string]any `json:"params"`
	CloudInit *string        `json:"cloud_init"`
}

func (h *Handler) callProfileCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.profile.create"
	if h.deps.ProfileSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "profile registry is not configured")
	}
	var a profileCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !profile.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+profile.IDPattern)
	}
	if a.Provider == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'provider' is required")
	}
	if !profile.ValidID(a.Provider) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'provider' must match "+profile.IDPattern)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "profile", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission profile.create")
	}

	p, err := h.deps.ProfileSvc.Create(ctx, profile.CreateInput{
		ID:        a.ID,
		Label:     a.Label,
		Provider:  a.Provider,
		Params:    a.Params,
		CloudInit: a.CloudInit,
		CallerAID: claims.Subject,
	})
	if err != nil {
		switch {
		case errors.Is(err, profile.ErrProfileAlreadyExists):
			return h.toolError(req.ID, toolName, mcpCodeProfileExists, "profile "+a.ID+" already exists")
		case errors.Is(err, profile.ErrProviderNotFound):
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"referenced provider "+a.Provider+" does not exist")
		}
		h.deps.Logger.Error("mcp: profile.create failed", slog.String("id", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "create profile failed")
	}

	h.writeAudit(audit.EventProfileCreated, claims.Subject, map[string]any{
		"id":          p.ID,
		"label":       p.Label,
		"provider":    p.Provider,
		"params_keys": paramKeysSortedMCP(p.Params),
	})
	return h.toolResult(req.ID, toProfileViewOut(p))
}

type profileByIDArgs struct {
	ID string `json:"id"`
}

// callProfileSetLabel — keeper.profile.label-set, the MCP mirror of
// PUT /v1/profiles/{id}/label (ADR-0085). The registry's only mutation.
func (h *Handler) callProfileSetLabel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	return callLabelSet(h, ctx, claims, req, args, labelSetSpec[profileViewOut]{
		tool:          "keeper.profile.label-set",
		resource:      "profile",
		configured:    h.deps.ProfileSvc != nil,
		notConfigured: "profile registry is not configured",
		validID:       profile.ValidID,
		idPattern:     profile.IDPattern,
		set: func(ctx context.Context, id string, label *string) (profileViewOut, *string, error) {
			p, previous, err := h.deps.ProfileSvc.SetLabel(ctx, id, label)
			if err != nil {
				return profileViewOut{}, nil, err
			}
			return toProfileViewOut(p), previous, nil
		},
		isNotFound: func(err error) bool { return errors.Is(err, profile.ErrProfileNotFound) },
		notFoundf:  func(id string) string { return "profile " + id + " not found" },
		failMsg:    "set profile label failed",
		event:      audit.EventProfileLabelChanged,
	})
}

func (h *Handler) callProfileRead(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.profile.read"
	if h.deps.ProfileSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "profile registry is not configured")
	}
	var a profileByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if err := h.deps.RBAC.Check(claims.Subject, "profile", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission profile.read")
	}
	p, err := h.deps.ProfileSvc.Get(ctx, a.ID)
	if err != nil {
		if errors.Is(err, profile.ErrProfileNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "profile "+a.ID+" not found")
		}
		h.deps.Logger.Error("mcp: profile.read failed", slog.String("id", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "read profile failed")
	}
	return h.toolResult(req.ID, toProfileViewOut(p))
}

func (h *Handler) callProfileDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.profile.delete"
	if h.deps.ProfileSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "profile registry is not configured")
	}
	var a profileByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !profile.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+profile.IDPattern)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "profile", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission profile.delete")
	}
	err := h.deps.ProfileSvc.Delete(ctx, a.ID)
	if err != nil {
		if errors.Is(err, profile.ErrProfileNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "profile "+a.ID+" not found")
		}
		h.deps.Logger.Error("mcp: profile.delete failed", slog.String("id", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "delete profile failed")
	}
	h.writeAudit(audit.EventProfileDeleted, claims.Subject, map[string]any{"id": a.ID})
	return h.toolResult(req.ID, struct{}{})
}

type profileListArgs struct {
	Provider string `json:"provider"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

type profileListOut struct {
	Items  []profileViewOut `json:"items"`
	Offset int              `json:"offset"`
	Limit  int              `json:"limit"`
	Total  int              `json:"total"`
}

func (h *Handler) callProfileList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.profile.list"
	if h.deps.ProfileSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "profile registry is not configured")
	}
	var a profileListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if err := h.deps.RBAC.Check(claims.Subject, "profile", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission profile.read")
	}
	if a.Limit <= 0 {
		a.Limit = 100
	}
	items, total, err := h.deps.ProfileSvc.List(ctx, a.Provider, a.Offset, a.Limit)
	if err != nil {
		h.deps.Logger.Error("mcp: profile.list failed", slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "list profiles failed")
	}
	out := make([]profileViewOut, 0, len(items))
	for _, p := range items {
		out = append(out, toProfileViewOut(p))
	}
	return h.toolResult(req.ID, profileListOut{Items: out, Offset: a.Offset, Limit: a.Limit, Total: total})
}
