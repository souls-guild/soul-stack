package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.incarnation.traits-set — parity with REST PUT
// /v1/incarnations/{name}/traits (IncarnationHandler.SetTraitsTyped, ADR-060).
// Wholesale REPLACES the incarnation's operator-set trait labels in one FOR
// UPDATE tx. The write ends there, in both senses: nothing is projected onto a
// host row, and no member host reads these labels either (NIM-281). A host
// carries only what keeper.soul.traits-assign put on it.
//
// SECURITY. RBAC — body-scoped OR-Check over the incarnation's coven/service
// scope (its declared covens, with the name as the `incarnation=` dimension —
// mirrors REST IncarnationScopeSelector + permission
// incarnation.traits-set). Without it MCP would bypass REST protection (MCP
// has no chi middleware). scope is resolved via a separate probe-SelectByName
// (same cold RBAC round-trip as REST). No gate on trait keys here — this route
// labels the incarnation, and the operator already holds it by scope; the
// per-pair gate (b) belongs to the per-HOST write, where a pair can be attached
// to a machine outside the label's own scope.

type incarnationTraitsSetArgs struct {
	Name   string         `json:"name"`
	Traits map[string]any `json:"traits,omitempty"`
}

// incarnationTraitsSetOutput — output of keeper.incarnation.traits-set.
// trait VALUES are never echoed in output (secret hygiene, mirrors audit):
// we record only that the replacement happened and which keys. Full state
// via keeper.incarnation.get.
type incarnationTraitsSetOutput struct {
	Incarnation string   `json:"incarnation"`
	Keys        []string `json:"keys"`
}

func (h *Handler) callIncarnationTraitsSet(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.traits-set"

	var a incarnationTraitsSetArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if !incarnation.ValidName(a.Name) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'name' must match "+incarnation.NamePattern)
	}
	// trait format/value (no nesting) — parity with REST SetTraitsTyped.
	if err := soul.ValidateTraitDelta(a.Traits); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}

	// RBAC OR-Check over the incarnation's coven/service scope (covens ∪
	// {name}) — mirrors REST middleware. UpdateTraits does its own FOR
	// UPDATE select internally, so scope is resolved via a separate
	// probe-SelectByName (unlock/destroy pattern). A failed probe →
	// fail-closed (scoped deny, bare/`*` pass through → UpdateTraits returns
	// 404/500).
	inc, probeErr := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "traits-set", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.traits-set")
		}
	} else if scopeErr := h.checkIncarnationScope(claims, "traits-set", inc.Name, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.traits-set")
	}

	res, err := incarnation.UpdateTraits(ctx, h.deps.IncarnationDB, a.Name, a.Traits)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound,
				"incarnation "+a.Name+" not found")
		}
		h.deps.Logger.Error("mcp: incarnation.traits-set failed",
			slog.String("name", a.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "update incarnation traits failed")
	}

	// No projection (NIM-281, parity with REST): the replace lands on
	// incarnation.traits alone, and no member host reads it.

	// audit: EventIncarnationTraitsChanged {name, old_keys, new_keys},
	// source=mcp (writeAudit). trait VALUES are not included — parity with
	// REST (secret hygiene).
	h.writeAudit(audit.EventIncarnationTraitsChanged, claims.Subject, map[string]any{
		"name":     a.Name,
		"old_keys": res.OldKeys,
		"new_keys": res.NewKeys,
	})

	return h.toolResult(req.ID, incarnationTraitsSetOutput{
		Incarnation: a.Name,
		Keys:        res.NewKeys,
	})
}
