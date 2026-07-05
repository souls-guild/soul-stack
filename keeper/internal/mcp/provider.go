package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.provider.<verb> — паритет REST POST/GET/DELETE /v1/providers*
// (ProviderHandler, ADR-017, Cloud CRUD). Тонкая MCP-обёртка над тем же
// provider.Service, что REST. Permission-маппинг 1:1 (keeper.provider.<verb> ↔
// provider.<verb>), селектор — NoSelector. БЕЗ update (Provider иммутабелен).
//
// Секрет-гигиена: credentials_ref отдаётся как ПУТЬ (vault:<path>), не резолвится.

// providerViewOut — JSON-форма output-а (та же, что HTTP-handler).
type providerViewOut struct {
	Name           string    `json:"name"`
	Type           string    `json:"type"`
	Region         string    `json:"region"`
	CredentialsRef string    `json:"credentials_ref"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedByAID   *string   `json:"created_by_aid,omitempty"`
}

func toProviderViewOut(p *provider.Provider) providerViewOut {
	return providerViewOut{
		Name:           p.Name,
		Type:           p.Type,
		Region:         p.Region,
		CredentialsRef: p.CredentialsRef,
		CreatedAt:      p.CreatedAt.UTC(),
		CreatedByAID:   p.CreatedByAID,
	}
}

type providerCreateArgs struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Region         string `json:"region"`
	CredentialsRef string `json:"credentials_ref"`
	// Credentials — опц. plaintext cloud-credentials (dual-mode, ADR-064); XOR с
	// credentials_ref. keeper пишет их в Vault, plaintext не персистится.
	Credentials map[string]any `json:"credentials"`
}

func (h *Handler) callProviderCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.provider.create"
	if h.deps.ProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "provider registry is not configured")
	}
	var a providerCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !provider.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' must match "+provider.NamePattern)
	}
	if !provider.ValidName(a.Type) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'type' must match "+provider.NamePattern)
	}
	if a.Region == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'region' is required")
	}
	// credentials_ref / credentials — dual-mode XOR (ADR-064); формат/XOR/plaintext-
	// disabled валидирует сервис (provider.IsValidationError → validation-failed).
	if err := h.deps.RBAC.Check(claims.Subject, "provider", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission provider.create")
	}

	p, err := h.deps.ProviderSvc.Create(ctx, provider.CreateInput{
		Name:           a.Name,
		Type:           a.Type,
		Region:         a.Region,
		CredentialsRef: a.CredentialsRef,
		Credentials:    a.Credentials,
		CallerAID:      claims.Subject,
	})
	if err != nil {
		if errors.Is(err, provider.ErrProviderAlreadyExists) {
			return h.toolError(req.ID, toolName, mcpCodeProviderExists, "provider "+a.Name+" already exists")
		}
		if provider.IsValidationError(err) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, provider.PublicMessage(err))
		}
		h.deps.Logger.Error("mcp: provider.create failed", slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "create provider failed")
	}

	// Audit: credentials_ref пишется как ПУТЬ (не секрет); plaintext_ingested —
	// маркер записи credentials keeper-ом (ADR-064), без plaintext.
	auditPayload := map[string]any{
		"name":            p.Name,
		"type":            p.Type,
		"region":          p.Region,
		"credentials_ref": p.CredentialsRef,
	}
	if p.SecretWritten {
		auditPayload["plaintext_ingested"] = true
	}
	h.writeAudit(audit.EventProviderCreated, claims.Subject, auditPayload)
	return h.toolResult(req.ID, toProviderViewOut(p))
}

type providerByNameArgs struct {
	Name string `json:"name"`
}

func (h *Handler) callProviderRead(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.provider.read"
	if h.deps.ProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "provider registry is not configured")
	}
	var a providerByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if err := h.deps.RBAC.Check(claims.Subject, "provider", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission provider.read")
	}
	p, err := h.deps.ProviderSvc.Get(ctx, a.Name)
	if err != nil {
		if errors.Is(err, provider.ErrProviderNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "provider "+a.Name+" not found")
		}
		h.deps.Logger.Error("mcp: provider.read failed", slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "read provider failed")
	}
	return h.toolResult(req.ID, toProviderViewOut(p))
}

func (h *Handler) callProviderDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.provider.delete"
	if h.deps.ProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "provider registry is not configured")
	}
	var a providerByNameArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !provider.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' must match "+provider.NamePattern)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "provider", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission provider.delete")
	}
	err := h.deps.ProviderSvc.Delete(ctx, a.Name)
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrProviderNotFound):
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "provider "+a.Name+" not found")
		case errors.Is(err, provider.ErrProviderHasProfiles):
			return h.toolError(req.ID, toolName, mcpCodeProviderHasProfiles,
				"provider "+a.Name+" has dependent profiles; delete them first")
		}
		h.deps.Logger.Error("mcp: provider.delete failed", slog.String("name", a.Name), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "delete provider failed")
	}
	h.writeAudit(audit.EventProviderDeleted, claims.Subject, map[string]any{"name": a.Name})
	return h.toolResult(req.ID, struct{}{})
}

type providerListArgs struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

type providerListOut struct {
	Items  []providerViewOut `json:"items"`
	Offset int               `json:"offset"`
	Limit  int               `json:"limit"`
	Total  int               `json:"total"`
}

func (h *Handler) callProviderList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.provider.list"
	if h.deps.ProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "provider registry is not configured")
	}
	var a providerListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if err := h.deps.RBAC.Check(claims.Subject, "provider", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission provider.read")
	}
	if a.Limit <= 0 {
		a.Limit = 100
	}
	items, total, err := h.deps.ProviderSvc.List(ctx, a.Offset, a.Limit)
	if err != nil {
		h.deps.Logger.Error("mcp: provider.list failed", slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "list providers failed")
	}
	out := make([]providerViewOut, 0, len(items))
	for _, p := range items {
		out = append(out, toProviderViewOut(p))
	}
	return h.toolResult(req.ID, providerListOut{Items: out, Offset: a.Offset, Limit: a.Limit, Total: total})
}
