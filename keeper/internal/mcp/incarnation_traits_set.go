package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeper.incarnation.traits-set — parity with REST PUT
// /v1/incarnations/{id}/traits (IncarnationHandler.SetTraitsTyped, ADR-060).
// Wholesale REPLACES the incarnation's operator-set trait labels in one FOR
// UPDATE tx. The write ends there, in both senses: nothing is projected onto a
// host row, and no member host reads these labels either (NIM-281). A host
// carries only what keeper.soul.traits-assign put on it.
//
// SECURITY — two gates, the same pair as the per-host write.
//
// Gate (a): body-scoped OR-Check over the incarnation's coven/service scope (its
// declared covens, with the name as the `incarnation=` dimension — mirrors REST
// IncarnationScopeSelector + permission incarnation.traits-set). Without it MCP
// would bypass REST protection (MCP has no chi middleware). scope is resolved via
// a separate probe-SelectByName (same cold RBAC round-trip as REST).
//
// Gate (b), NIM-587: every pair being stamped must lie inside the operator's own
// trait-scope ([handlers.ScreenTraitPairsInScope], shared with REST and with the
// per-host write). This header used to assert the opposite — that gate (b) was a
// per-HOST concern because "the operator already holds the incarnation by scope".
// That inference is false, and it was the hole. `trait.<key>` is a live scope
// dimension for incarnations as well (incScopeColumns.Traits), so stamping
// `tier=gold` here grants every `trait.tier=gold` role sight of this incarnation —
// visibility the caller may not hold itself. Holding the object is gate (a);
// holding the label is a separate question, and this is where it is asked.

type incarnationTraitsSetArgs struct {
	ID     string         `json:"id"`
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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !incarnation.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'id' must match "+incarnation.IDPattern)
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
	inc, probeErr := incarnation.SelectByID(ctx, h.deps.IncarnationDB, a.ID)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "traits-set", a.ID, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.traits-set")
		}
	} else if scopeErr := h.checkIncarnationScope(claims, "traits-set", inc.ID, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.traits-set")
	}

	// Gate (b), NIM-587 (parity with REST SetTraitsTyped): the stamped pairs must
	// lie inside the operator's own trait-scope. Checked BEFORE the write, through
	// the SAME function every other trait write calls.
	if err := handlers.ScreenTraitPairsInScope(ctx, h.deps.IncarnationDB, h.deps.PurviewResolver,
		claims.Subject, "incarnation", "traits-set", a.Traits); err != nil {
		var outOfScope *handlers.TraitPairOutOfScopeError
		if errors.As(err, &outOfScope) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, outOfScope.Error())
		}
		h.deps.Logger.Error("mcp: incarnation.traits-set trait-scope gate unavailable",
			slog.String("name", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "update incarnation traits failed")
	}

	res, err := incarnation.UpdateTraits(ctx, h.deps.IncarnationDB, a.ID, a.Traits)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound,
				"incarnation "+a.ID+" not found")
		}
		h.deps.Logger.Error("mcp: incarnation.traits-set failed",
			slog.String("name", a.ID),
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
		"id":       a.ID,
		"old_keys": res.OldKeys,
		"new_keys": res.NewKeys,
	})

	return h.toolResult(req.ID, incarnationTraitsSetOutput{
		Incarnation: a.ID,
		Keys:        res.NewKeys,
	})
}
