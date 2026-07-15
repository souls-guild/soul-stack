package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// augurNotConfigured — public detail of the augur-tools nil guard. AugurSvc
// is an optional HandlerDeps field (production wiring passes the same
// *augur.Service as REST): when nil, augur-tools still dispatch but return
// an internal-error "not configured" (same pattern as RBACRoles/SigilSvc/ServiceSvc).
const augurNotConfigured = "augur registry is not configured"

// omenView — output projection of an Omen for augur-tools (schemaOmenView).
// 1:1 with REST omenResponse / [augur.Omen]: name + source_type/endpoint/
// auth_ref + audit metadata. No master credential in the record (auth_ref
// is a vault-ref).
type omenView struct {
	Name         string  `json:"name"`
	SourceType   string  `json:"source_type"`
	Endpoint     string  `json:"endpoint"`
	AuthRef      string  `json:"auth_ref"`
	CreatedByAID *string `json:"created_by_aid,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

func toOmenView(o *augur.Omen) omenView {
	return omenView{
		Name:         o.Name,
		SourceType:   string(o.SourceType),
		Endpoint:     o.Endpoint,
		AuthRef:      o.AuthRef,
		CreatedByAID: o.CreatedByAID,
		CreatedAt:    o.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// omenCreateArgs — arguments for keeper.augur.omen.create.
type omenCreateArgs struct {
	Name       string `json:"name"`
	SourceType string `json:"source_type"`
	Endpoint   string `json:"endpoint"`
	AuthRef    string `json:"auth_ref"`
}

// callAugurOmenCreate — mutating tool keeper.augur.omen.create. Transport
// over [augur.Service.CreateOmen]: all validation (name / source_type /
// endpoint / auth_ref) lives in Service; tool maps sentinels to MCP codes
// and writes the omen.created audit event.
//
// RBAC — omen.create with no selector (rbac.md §Augur: NoSelector).
func (h *Handler) callAugurOmenCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.omen.create"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "omen", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission omen.create")
	}

	var a omenCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	callerAID := claims.Subject
	o, err := h.deps.AugurSvc.CreateOmen(ctx, augur.CreateOmenInput{
		Name:       a.Name,
		SourceType: a.SourceType,
		Endpoint:   a.Endpoint,
		AuthRef:    a.AuthRef,
		CallerAID:  &callerAID,
	})
	if err != nil {
		code, detail := mapAugurErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: augur.omen.create failed",
				slog.String("name", a.Name), slog.String("by_aid", callerAID), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallels the REST handler: payload {name, source_type,
	// endpoint, auth_ref, created_by_aid}. endpoint/auth_ref aren't secrets
	// (no master cred in the record, augur.md §8).
	h.writeAudit(audit.EventOmenCreated, callerAID, map[string]any{
		"name":           o.Name,
		"source_type":    string(o.SourceType),
		"endpoint":       o.Endpoint,
		"auth_ref":       o.AuthRef,
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toOmenView(o))
}

// omenListOutput — output of keeper.augur.omen.list: the Omen registry
// under `omens` (parity with REST GET /v1/augur/omens items).
type omenListOutput struct {
	Omens []omenView `json:"omens"`
	Total int        `json:"total"`
}

// omenListArgs — arguments for keeper.augur.omen.list (optional offset/limit).
type omenListArgs struct {
	Offset *int `json:"offset"`
	Limit  *int `json:"limit"`
}

// callAugurOmenList — read tool keeper.augur.omen.list (read-only, not
// audited). RBAC — omen.list with no selector.
func (h *Handler) callAugurOmenList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.omen.list"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "omen", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission omen.list")
	}

	var a omenListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	offset, limit := 0, 50
	if a.Offset != nil {
		offset = *a.Offset
	}
	if a.Limit != nil {
		limit = *a.Limit
	}
	if offset < 0 || limit < 1 || limit > listMaxLimit {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"offset must be >= 0 and limit must be between 1 and 1000")
	}

	omens, total, err := h.deps.AugurSvc.ListOmens(ctx, offset, limit)
	if err != nil {
		h.deps.Logger.Error("mcp: augur.omen.list failed",
			slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "internal error")
	}

	out := omenListOutput{Omens: make([]omenView, 0, len(omens)), Total: total}
	for _, o := range omens {
		out.Omens = append(out.Omens, toOmenView(o))
	}
	return h.toolResult(req.ID, out)
}

// omenDeleteArgs — arguments for keeper.augur.omen.delete.
type omenDeleteArgs struct {
	Name string `json:"name"`
}

// callAugurOmenDelete — mutating tool keeper.augur.omen.delete. Cascades to
// delete associated Rites. RBAC — omen.delete with no selector.
func (h *Handler) callAugurOmenDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.omen.delete"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "omen", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission omen.delete")
	}

	var a omenDeleteArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	if err := h.deps.AugurSvc.DeleteOmen(ctx, a.Name); err != nil {
		code, detail := mapAugurErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: augur.omen.delete failed",
				slog.String("name", a.Name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallels the REST handler: payload {name}.
	h.writeAudit(audit.EventOmenRevoked, claims.Subject, map[string]any{
		"name": a.Name,
	})

	// REST returns 204 No Content; the MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}
