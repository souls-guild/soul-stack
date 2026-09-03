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

// keeper.provider.<verb> — parity with REST POST/GET/DELETE /v1/providers*
// (ProviderHandler, ADR-017, Cloud CRUD). A thin MCP wrapper over the same
// provider.Service as REST. Permission mapping is 1:1 (keeper.provider.<verb>
// ↔ provider.<verb>), selector — NoSelector. NO update (Provider is
// immutable).
//
// Secret hygiene: credentials_ref is returned as a PATH (vault:<path>), never
// resolved.

// providerViewOut — JSON shape of the output (same as the HTTP handler).
type providerViewOut struct {
	ID string `json:"id"`
	// Label — display caption (ADR-0085); absent when the row carries none, and
	// a consumer then shows `name`.
	Label          *string   `json:"label,omitempty"`
	Type           string    `json:"type"`
	Region         string    `json:"region"`
	CredentialsRef string    `json:"credentials_ref"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedByAID   *string   `json:"created_by_aid,omitempty"`
}

func toProviderViewOut(p *provider.Provider) providerViewOut {
	return providerViewOut{
		ID:             p.ID,
		Label:          p.Label,
		Type:           p.Type,
		Region:         p.Region,
		CredentialsRef: p.CredentialsRef,
		CreatedAt:      p.CreatedAt.UTC(),
		CreatedByAID:   p.CreatedByAID,
	}
}

type providerCreateArgs struct {
	ID string `json:"id"`
	// Label — optional display caption (ADR-0085), free text; changed afterwards
	// by keeper.provider.label-set.
	Label          *string `json:"label"`
	Type           string  `json:"type"`
	Region         string  `json:"region"`
	CredentialsRef string  `json:"credentials_ref"`
	// Credentials — optional plaintext cloud-credentials (dual-mode, ADR-064);
	// XOR with credentials_ref. keeper writes them to Vault, plaintext is never
	// persisted.
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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !provider.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+provider.IDPattern)
	}
	if !provider.ValidID(a.Type) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'type' must match "+provider.IDPattern)
	}
	if a.Region == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'region' is required")
	}
	// credentials_ref / credentials — dual-mode XOR (ADR-064); the service
	// validates format/XOR/plaintext-disabled (provider.IsValidationError →
	// validation-failed).
	if err := h.deps.RBAC.Check(claims.Subject, "provider", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission provider.create")
	}

	p, err := h.deps.ProviderSvc.Create(ctx, provider.CreateInput{
		ID:             a.ID,
		Label:          a.Label,
		Type:           a.Type,
		Region:         a.Region,
		CredentialsRef: a.CredentialsRef,
		Credentials:    a.Credentials,
		CallerAID:      claims.Subject,
	})
	if err != nil {
		if errors.Is(err, provider.ErrProviderAlreadyExists) {
			return h.toolError(req.ID, toolName, mcpCodeProviderExists, "provider "+a.ID+" already exists")
		}
		if provider.IsValidationError(err) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, provider.PublicMessage(err))
		}
		h.deps.Logger.Error("mcp: provider.create failed", slog.String("id", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "create provider failed")
	}

	// Audit: credentials_ref is written as a PATH (not secret); plaintext_ingested
	// marks that keeper wrote credentials (ADR-064), without the plaintext.
	auditPayload := map[string]any{
		"id":              p.ID,
		"type":            p.Type,
		"region":          p.Region,
		"credentials_ref": p.CredentialsRef,
		"label":           p.Label,
	}
	if p.SecretWritten {
		auditPayload["plaintext_ingested"] = true
	}
	h.writeAudit(audit.EventProviderCreated, claims.Subject, auditPayload)
	return h.toolResult(req.ID, toProviderViewOut(p))
}

type providerByIDArgs struct {
	ID string `json:"id"`
}

func (h *Handler) callProviderRead(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.provider.read"
	if h.deps.ProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "provider registry is not configured")
	}
	var a providerByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if err := h.deps.RBAC.Check(claims.Subject, "provider", "read", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission provider.read")
	}
	p, err := h.deps.ProviderSvc.Get(ctx, a.ID)
	if err != nil {
		if errors.Is(err, provider.ErrProviderNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "provider "+a.ID+" not found")
		}
		h.deps.Logger.Error("mcp: provider.read failed", slog.String("id", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "read provider failed")
	}
	return h.toolResult(req.ID, toProviderViewOut(p))
}

func (h *Handler) callProviderDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.provider.delete"
	if h.deps.ProviderSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "provider registry is not configured")
	}
	var a providerByIDArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !provider.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+provider.IDPattern)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "provider", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden, "operator lacks required permission provider.delete")
	}
	err := h.deps.ProviderSvc.Delete(ctx, a.ID)
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrProviderNotFound):
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "provider "+a.ID+" not found")
		case errors.Is(err, provider.ErrProviderHasProfiles):
			return h.toolError(req.ID, toolName, mcpCodeProviderHasProfiles,
				"provider "+a.ID+" has dependent profiles; delete them first")
		}
		h.deps.Logger.Error("mcp: provider.delete failed", slog.String("id", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "delete provider failed")
	}
	h.writeAudit(audit.EventProviderDeleted, claims.Subject, map[string]any{"id": a.ID})
	return h.toolResult(req.ID, struct{}{})
}

// callProviderSetLabel — keeper.provider.label-set, the MCP mirror of
// PUT /v1/providers/{id}/label (ADR-0085). The registry's only mutation.
func (h *Handler) callProviderSetLabel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	return callLabelSet(h, ctx, claims, req, args, labelSetSpec[providerViewOut]{
		tool:          "keeper.provider.label-set",
		resource:      "provider",
		configured:    h.deps.ProviderSvc != nil,
		notConfigured: "provider registry is not configured",
		validID:       provider.ValidID,
		idPattern:     provider.IDPattern,
		set: func(ctx context.Context, id string, label *string) (providerViewOut, *string, error) {
			p, previous, err := h.deps.ProviderSvc.SetLabel(ctx, id, label)
			if err != nil {
				return providerViewOut{}, nil, err
			}
			return toProviderViewOut(p), previous, nil
		},
		isNotFound: func(err error) bool { return errors.Is(err, provider.ErrProviderNotFound) },
		notFoundf:  func(id string) string { return "provider " + id + " not found" },
		failMsg:    "set provider label failed",
		event:      audit.EventProviderLabelChanged,
	})
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
