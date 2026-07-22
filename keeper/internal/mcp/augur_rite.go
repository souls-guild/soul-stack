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

// riteView — output projection of a Rite for augur-tools (schemaRiteView).
// 1:1 with REST riteResponse / [augur.Rite].
type riteView struct {
	ID           int64           `json:"id"`
	Omen         string          `json:"omen"`
	Coven        *string         `json:"coven,omitempty"`
	SID          *string         `json:"sid,omitempty"`
	Allow        json.RawMessage `json:"allow"`
	Delegate     bool            `json:"delegate"`
	TokenTTL     *string         `json:"token_ttl,omitempty"`
	TokenNumUses *int            `json:"token_num_uses,omitempty"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	CreatedAt    string          `json:"created_at"`
}

func toRiteView(r *augur.Rite) riteView {
	return riteView{
		ID:           r.ID,
		Omen:         r.Omen,
		Coven:        r.Coven,
		SID:          r.SID,
		Allow:        r.Allow,
		Delegate:     r.Delegate,
		TokenTTL:     r.TokenTTL,
		TokenNumUses: r.TokenNumUses,
		CreatedByAID: r.CreatedByAID,
		CreatedAt:    r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// riteCreateArgs — arguments for keeper.augur.rite.create. subject is XOR
// coven/sid; allow is raw JSONB (shape depends on the Omen's source_type).
type riteCreateArgs struct {
	Omen         string          `json:"omen"`
	Coven        *string         `json:"coven"`
	SID          *string         `json:"sid"`
	Allow        json.RawMessage `json:"allow"`
	Delegate     bool            `json:"delegate"`
	TokenTTL     *string         `json:"token_ttl"`
	TokenNumUses *int            `json:"token_num_uses"`
}

// callAugurRiteCreate — mutating tool keeper.augur.rite.create. A transport
// over [augur.Service.CreateRite]: all validation (XOR subject, allow shape
// by source_type, token fields) lives in Service; the tool maps sentinels
// to MCP codes and writes the rite.created audit event.
//
// RBAC — rite.create with no selector (rbac.md §Augur: NoSelector).
func (h *Handler) callAugurRiteCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.rite.create"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "rite", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission rite.create")
	}

	var a riteCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Omen == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'omen' is required")
	}

	callerAID := claims.Subject
	rite, err := h.deps.AugurSvc.CreateRite(ctx, augur.CreateRiteInput{
		Omen:         a.Omen,
		Coven:        a.Coven,
		SID:          a.SID,
		Allow:        a.Allow,
		Delegate:     a.Delegate,
		TokenTTL:     a.TokenTTL,
		TokenNumUses: a.TokenNumUses,
		CallerAID:    &callerAID,
	})
	if err != nil {
		code, detail := mapAugurErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: augur.rite.create failed",
				slog.String("omen", a.Omen), slog.String("by_aid", callerAID), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — mirrors the REST handler: payload {id, omen, subject, delegate,
	// created_by_aid}. The allow-list is NOT included (augur.md §8).
	h.writeAudit(audit.EventRiteCreated, callerAID, map[string]any{
		"id":             rite.ID,
		"omen":           rite.Omen,
		"subject":        riteSubjectView(rite),
		"delegate":       rite.Delegate,
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toRiteView(rite))
}

// riteListOutput — output of keeper.augur.rite.list: one Omen's Rites under
// `rites` (parity with REST GET /v1/augur/rites?omen= items).
type riteListOutput struct {
	Rites []riteView `json:"rites"`
}

// riteListArgs — arguments for keeper.augur.rite.list. omen is required
// (by-omen filter; augur.md §6 — list-all without an omen scope is deferred).
type riteListArgs struct {
	Omen string `json:"omen"`
}

// callAugurRiteList — read-only tool keeper.augur.rite.list (not audited).
// RBAC — rite.list with no selector.
func (h *Handler) callAugurRiteList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.rite.list"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "rite", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission rite.list")
	}

	var a riteListArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Omen == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'omen' is required")
	}

	rites, err := h.deps.AugurSvc.ListRitesByOmen(ctx, a.Omen)
	if err != nil {
		h.deps.Logger.Error("mcp: augur.rite.list failed",
			slog.String("omen", a.Omen), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "internal error")
	}

	out := riteListOutput{Rites: make([]riteView, 0, len(rites))}
	for _, rt := range rites {
		out.Rites = append(out.Rites, toRiteView(rt))
	}
	return h.toolResult(req.ID, out)
}

// riteDeleteArgs — arguments for keeper.augur.rite.delete.
type riteDeleteArgs struct {
	ID int64 `json:"id"`
}

// callAugurRiteDelete — mutating tool keeper.augur.rite.delete. RBAC —
// rite.delete with no selector.
func (h *Handler) callAugurRiteDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.rite.delete"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "rite", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission rite.delete")
	}

	var a riteDeleteArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID <= 0 {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must be a positive integer")
	}

	if err := h.deps.AugurSvc.DeleteRite(ctx, a.ID); err != nil {
		code, detail := mapAugurErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: augur.rite.delete failed",
				slog.Int64("id", a.ID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — mirrors the REST handler: payload {id}.
	h.writeAudit(audit.EventRiteRevoked, claims.Subject, map[string]any{
		"id": a.ID,
	})

	return h.toolResult(req.ID, struct{}{})
}

// riteSubjectView — human-readable form of a Rite's subject for the audit
// payload (`coven=<v>` / `sid=<v>`). XOR is guaranteed by validation.
func riteSubjectView(r *augur.Rite) string {
	if r.Coven != nil && *r.Coven != "" {
		return "coven=" + *r.Coven
	}
	if r.SID != nil && *r.SID != "" {
		return "sid=" + *r.SID
	}
	return ""
}
