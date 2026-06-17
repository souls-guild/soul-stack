package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.push-provider.<verb> — паритет REST POST/GET/PUT/DELETE /v1/push-providers*
// (PushProviderHandler, ADR-032 amendment 2026-05-26, S7-2). Тонкая
// MCP-обёртка над тем же pushprovider.Service, что REST. Permission-маппинг
// 1:1 (keeper.push-provider.<verb> ↔ push-provider.<verb>), селектор —
// NoSelector (паттерн service.* / role.*).

// pushProviderViewOut — JSON-форма output-а (та же, что HTTP-handler).
type pushProviderViewOut struct {
	Name         string         `json:"name"`
	Params       map[string]any `json:"params"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	CreatedByAID string         `json:"created_by_aid"`
	UpdatedByAID *string        `json:"updated_by_aid,omitempty"`
}

func toPushProviderViewOut(p *pushprovider.PushProvider) pushProviderViewOut {
	params := p.Params
	if params == nil {
		params = map[string]any{}
	}
	return pushProviderViewOut{
		Name:         p.Name,
		Params:       params,
		CreatedAt:    p.CreatedAt.UTC(),
		UpdatedAt:    p.UpdatedAt.UTC(),
		CreatedByAID: p.CreatedByAID,
		UpdatedByAID: p.UpdatedByAID,
	}
}

type pushProviderCreateArgs struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params"`
}

func (h *Handler) callPushProviderCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.push-provider.create"
	if h.deps.PushProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "push-provider registry is not configured")
	}
	var a pushProviderCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !pushprovider.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+pushprovider.NamePattern)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "push-provider", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission push-provider.create")
	}

	p, err := h.deps.PushProviderSvc.Create(ctx, pushprovider.CreateInput{
		Name:      a.Name,
		Params:    a.Params,
		CallerAID: claims.Subject,
	})
	if err != nil {
		switch {
		case errors.Is(err, pushprovider.ErrPushProviderAlreadyExists):
			return h.toolError(req.ID, toolName, mcpCodePushProviderExists,
				"push provider "+a.Name+" already exists")
		case errors.Is(err, pushprovider.ErrSensitiveNotVaultRef):
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
		}
		h.deps.Logger.Error("mcp: push-provider.create failed",
			slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "create push provider failed")
	}

	h.writeAudit(audit.EventPushProviderCreated, claims.Subject, map[string]any{
		"name":        p.Name,
		"params_keys": paramKeysSortedMCP(p.Params),
	})
	return h.toolResult(req.ID, toPushProviderViewOut(p))
}

type pushProviderUpdateArgs struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params"`
}

func (h *Handler) callPushProviderUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.push-provider.update"
	if h.deps.PushProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "push-provider registry is not configured")
	}
	var a pushProviderUpdateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !pushprovider.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+pushprovider.NamePattern)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "push-provider", "update", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission push-provider.update")
	}

	p, err := h.deps.PushProviderSvc.Update(ctx, pushprovider.UpdateInput{
		Name:      a.Name,
		Params:    a.Params,
		CallerAID: claims.Subject,
	})
	if err != nil {
		switch {
		case errors.Is(err, pushprovider.ErrPushProviderNotFound):
			return h.toolError(req.ID, toolName, mcpCodeNotFound,
				"push provider "+a.Name+" not found")
		case errors.Is(err, pushprovider.ErrSensitiveNotVaultRef):
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
		}
		h.deps.Logger.Error("mcp: push-provider.update failed",
			slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "update push provider failed")
	}

	h.writeAudit(audit.EventPushProviderUpdated, claims.Subject, map[string]any{
		"name":        p.Name,
		"params_keys": paramKeysSortedMCP(p.Params),
	})
	return h.toolResult(req.ID, toPushProviderViewOut(p))
}

type pushProviderByNameArgs struct {
	Name string `json:"name"`
}

func (h *Handler) callPushProviderDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.push-provider.delete"
	if h.deps.PushProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "push-provider registry is not configured")
	}
	var a pushProviderByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !pushprovider.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+pushprovider.NamePattern)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "push-provider", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission push-provider.delete")
	}

	err := h.deps.PushProviderSvc.Delete(ctx, a.Name)
	if err != nil {
		if errors.Is(err, pushprovider.ErrPushProviderNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound,
				"push provider "+a.Name+" not found")
		}
		h.deps.Logger.Error("mcp: push-provider.delete failed",
			slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "delete push provider failed")
	}

	h.writeAudit(audit.EventPushProviderDeleted, claims.Subject, map[string]any{"name": a.Name})
	return h.toolResult(req.ID, struct{}{})
}

func (h *Handler) callPushProviderRead(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.push-provider.read"
	if h.deps.PushProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "push-provider registry is not configured")
	}
	var a pushProviderByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if err := h.deps.RBAC.Check(claims.Subject, "push-provider", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission push-provider.read")
	}
	p, err := h.deps.PushProviderSvc.Get(ctx, a.Name)
	if err != nil {
		if errors.Is(err, pushprovider.ErrPushProviderNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound,
				"push provider "+a.Name+" not found")
		}
		h.deps.Logger.Error("mcp: push-provider.read failed",
			slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "read push provider failed")
	}
	return h.toolResult(req.ID, toPushProviderViewOut(p))
}

type pushProviderListArgs struct {
	NamePattern string `json:"name_pattern"`
	Offset      int    `json:"offset"`
	Limit       int    `json:"limit"`
}

type pushProviderListOut struct {
	Items  []pushProviderViewOut `json:"items"`
	Offset int                   `json:"offset"`
	Limit  int                   `json:"limit"`
	Total  int                   `json:"total"`
}

func (h *Handler) callPushProviderList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.push-provider.list"
	if h.deps.PushProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "push-provider registry is not configured")
	}
	var a pushProviderListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if err := h.deps.RBAC.Check(claims.Subject, "push-provider", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission push-provider.list")
	}
	if a.Limit <= 0 {
		a.Limit = 100
	}
	items, total, err := h.deps.PushProviderSvc.List(ctx, pushprovider.ListFilter{NamePattern: a.NamePattern}, a.Offset, a.Limit)
	if err != nil {
		h.deps.Logger.Error("mcp: push-provider.list failed", slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "list push providers failed")
	}
	out := make([]pushProviderViewOut, 0, len(items))
	for _, p := range items {
		out = append(out, toPushProviderViewOut(p))
	}
	return h.toolResult(req.ID, pushProviderListOut{
		Items:  out,
		Offset: a.Offset,
		Limit:  a.Limit,
		Total:  total,
	})
}

// paramKeysSortedMCP — копия handlers.paramKeysSorted (mcp не импортит handlers
// для audit-payload, чтобы не плодить обратные зависимости).
func paramKeysSortedMCP(params map[string]any) []string {
	if len(params) == 0 {
		return []string{}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
