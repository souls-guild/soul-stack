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

// riteView — output-проекция Rite-а для augur-tools (schemaRiteView). 1:1 с REST
// riteResponse / [augur.Rite].
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

// riteCreateArgs — arguments tool-а keeper.augur.rite.create. subject — XOR
// coven/sid; allow — сырой JSONB (форма зависит от source_type Omen-а).
type riteCreateArgs struct {
	Omen         string          `json:"omen"`
	Coven        *string         `json:"coven"`
	SID          *string         `json:"sid"`
	Allow        json.RawMessage `json:"allow"`
	Delegate     bool            `json:"delegate"`
	TokenTTL     *string         `json:"token_ttl"`
	TokenNumUses *int            `json:"token_num_uses"`
}

// callAugurRiteCreate — mutating-tool keeper.augur.rite.create. Транспорт поверх
// [augur.Service.CreateRite]: вся валидация (XOR-субъект, allow-shape по
// source_type, token-поля) — в Service; tool маппит sentinel-ы в MCP-коды и
// пишет audit rite.created.
//
// RBAC — rite.create без селектора (rbac.md §Augur: NoSelector).
func (h *Handler) callAugurRiteCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.rite.create"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно REST-handler-у: payload {id, omen, subject, delegate,
	// created_by_aid}. allow-list НЕ кладётся (augur.md §8).
	h.writeAudit(audit.EventRiteCreated, callerAID, map[string]any{
		"id":             rite.ID,
		"omen":           rite.Omen,
		"subject":        riteSubjectView(rite),
		"delegate":       rite.Delegate,
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toRiteView(rite))
}

// riteListOutput — output keeper.augur.rite.list: Rite-ы одного Omen-а под
// `rites` (паритет REST GET /v1/augur/rites?omen= items).
type riteListOutput struct {
	Rites []riteView `json:"rites"`
}

// riteListArgs — arguments keeper.augur.rite.list. omen обязателен (фильтр
// by-omen, augur.md §6 — list-all без omen-скоупа отложен).
type riteListArgs struct {
	Omen string `json:"omen"`
}

// callAugurRiteList — read-tool keeper.augur.rite.list (read-only, не
// аудируется). RBAC — rite.list без селектора.
func (h *Handler) callAugurRiteList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.rite.list"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

// riteDeleteArgs — arguments keeper.augur.rite.delete.
type riteDeleteArgs struct {
	ID int64 `json:"id"`
}

// callAugurRiteDelete — mutating-tool keeper.augur.rite.delete. RBAC —
// rite.delete без селектора.
func (h *Handler) callAugurRiteDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.augur.rite.delete"

	if h.deps.AugurSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, augurNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно REST-handler-у: payload {id}.
	h.writeAudit(audit.EventRiteRevoked, claims.Subject, map[string]any{
		"id": a.ID,
	})

	return h.toolResult(req.ID, struct{}{})
}

// riteSubjectView — человекочитаемая форма субъекта Rite-а для audit-payload
// (`coven=<v>` / `sid=<v>`). XOR гарантирован валидацией.
func riteSubjectView(r *augur.Rite) string {
	if r.Coven != nil && *r.Coven != "" {
		return "coven=" + *r.Coven
	}
	if r.SID != nil && *r.SID != "" {
		return "sid=" + *r.SID
	}
	return ""
}
