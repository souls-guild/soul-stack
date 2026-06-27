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

// keeper.incarnation.traits-set — паритет REST PUT /v1/incarnations/{name}/traits
// (IncarnationHandler.SetTraitsTyped, ADR-060 amend R1). Целостно ЗАМЕНЯЕТ
// operator-set trait-метки инкарнации (`incarnation.traits` — источник истины) →
// персист одной tx FOR UPDATE → материализованная проекция в `souls.traits`
// хостов-членов ([incarnation.SyncTraitsToHosts]). Перенос operator-facing
// trait-управления с per-soul (keeper.soul.traits-assign, deprecated) на
// per-incarnation.
//
// БЕЗОПАСНОСТЬ. RBAC — body-scoped OR-Check по coven/service-scope incarnation
// (covens ∪ {name}, зеркало REST IncarnationScopeSelector + permission
// incarnation.traits-set). Без неё MCP стал бы обходом REST-защиты (у MCP нет
// chi-middleware). scope резолвится отдельным probe-SelectByName (тот же холодный
// RBAC-round-trip, что REST). trait-КЛЮЧ НЕ scope-измерение — гейта на ключи нет.

type incarnationTraitsSetArgs struct {
	Name   string         `json:"name"`
	Traits map[string]any `json:"traits,omitempty"`
}

// incarnationTraitsSetOutput — output keeper.incarnation.traits-set. trait-ЗНАЧЕНИЯ
// в output НЕ эхуются (секрет-гигиена, симметрия с audit): фиксируем факт замены и
// набор ключей. Полный state — через keeper.incarnation.get.
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
	// Формат/значение trait (запрет nested) — паритет REST SetTraitsTyped.
	if err := soul.ValidateTraitDelta(a.Traits); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
	}

	// RBAC OR-Check по coven/service-scope incarnation (covens ∪ {name}) —
	// зеркало REST middleware. UpdateTraits сам делает FOR UPDATE-select внутри,
	// поэтому scope резолвим отдельным probe-SelectByName (паттерн unlock/destroy).
	// Битый probe → fail-closed (scoped deny, bare/`*` pass → UpdateTraits вернёт
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

	// Sync-hook (ADR-060 amend R1): incarnation.traits → souls.traits хостов-членов.
	// Best-effort (лог, не валим tool): incarnation.traits уже записан, проекция
	// до-сойдётся при следующем bind/sync.
	if serr := incarnation.SyncTraitsToHosts(ctx, h.deps.IncarnationDB, a.Name, res.Incarnation.Traits); serr != nil {
		h.deps.Logger.Warn("mcp: incarnation.traits-set sync traits → souls провален (best-effort)",
			slog.String("name", a.Name), slog.Any("error", serr))
	}

	// audit: EventIncarnationTraitsChanged {name, old_keys, new_keys}, source=mcp
	// (writeAudit). trait-ЗНАЧЕНИЯ не кладутся — паритет REST (секрет-гигиена).
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
