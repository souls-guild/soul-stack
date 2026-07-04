package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationRerunLastArgs — arguments tool-а keeper.incarnation.rerun-last
// (schemaIncarnationRerunLastInput): name + reason обязательны. reason пишется
// в audit-payload (паритет REST RerunLast).
type incarnationRerunLastArgs struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// incarnationRerunLastOutput — output keeper.incarnation.rerun-last
// (schemaIncarnationRerunLastOutput): apply_id перезапущенного сценария + echo
// incarnation + scenario (имя перезапущенного). Симметричен REST
// IncarnationRerunLastReply.
type incarnationRerunLastOutput struct {
	ApplyID     string `json:"_apply_id"`
	Incarnation string `json:"incarnation"`
	Scenario    string `json:"scenario"`
}

// callIncarnationRerunLast — mutating async-tool keeper.incarnation.rerun-last.
// Паритет REST IncarnationHandler.RerunLast: снимает error_locked и тем же
// действием перезапускает ПОСЛЕДНИЙ упавший сценарий — bootstrap `create`/… ИЛИ
// day-2 add_user/… (architecture.md → «Атомарность и error_locked»). Под одним
// FOR UPDATE: error_locked → applying минуя ready.
//
// RBAC-context — covens ∪ {name} (name-bound, паритет unlock). audit:
// EventIncarnationRerunLast {name, reason, scenario, previous_status, apply_id},
// source=mcp, correlation_id=apply_id. RBAC ДО бизнес-вызова; audit — после
// успешного запуска.
func (h *Handler) callIncarnationRerunLast(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.rerun-last"

	var a incarnationRerunLastArgs
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
	if a.Reason == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'reason' is required")
	}

	// runner / services обязательны: rerun перезапускает сценарий.
	if h.deps.ScenarioRunner == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "scenario runner is not configured")
	}

	// RBAC OR-Check по coven/service-scope incarnation (covens ∪ {name}) —
	// зеркало REST middleware. Битый probe → fail-closed.
	inc, probeErr := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "rerun-last", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.rerun-last")
		}
		code, detail := mapIncarnationErrorToMCP(probeErr)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.rerun-last select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", probeErr),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	if scopeErr := h.checkIncarnationScope(claims, "rerun-last", inc.Name, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.rerun-last")
	}

	serviceRef, ok := h.deps.ServiceRegistry.Resolve(inc.Service)
	if !ok {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"service "+inc.Service+" is not registered (manage via service.* API, ADR-029)")
	}
	serviceRef.Ref = inc.ServiceVersion

	// applyID общий: state_history-snapshot unlock-перехода + перезапускаемый прогон.
	applyID := audit.NewULID()

	// Unlock-часть под FOR UPDATE: error_locked → applying минуя ready (race-free).
	res, err := incarnation.UnlockForRerun(ctx, h.deps.IncarnationDB, a.Name, a.Reason, claims.Subject, applyID, applyID)
	if err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.rerun-last unlock failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Перезапуск последнего упавшего сценария (async): статус уже applying
	// (UnlockForRerun). FromLocked — lockRun не транзитит статус повторно, обязан
	// увидеть applying. ScenarioName — имя упавшего сценария из UnlockResult (create
	// ИЛИ day-2). Input — его сохранённый input (spec.input на create-пути / recipe.
	// input на day-2-пути, прочитан тем же FOR UPDATE): без него перезапуск с
	// required-полями (redis cluster: version/shards) упал бы на input-валидации /
	// применил дефолты (паритет REST RerunLastTyped).
	if err := h.deps.ScenarioRunner.Start(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: a.Name,
		ServiceRef:      serviceRef,
		ScenarioName:    res.Scenario,
		Input:           res.Input,
		StartedByAID:    claims.Subject,
		FromLocked:      true,
		// Упавший прогон мог быть upgrade-сценарием (recipe.from_upgrade) — перезапуск
		// из upgrade/<slug>/, а не scenario/ (ADR-0068, паритет REST RerunLastTyped).
		FromUpgrade: res.FromUpgrade,
	}); err != nil {
		h.deps.Logger.Error("mcp: incarnation.rerun-last scenario start failed",
			slog.String("name", a.Name),
			slog.String("apply_id", applyID),
			slog.String("scenario", res.Scenario),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "start scenario "+res.Scenario+" failed")
	}

	h.writeAuditCorrelated(audit.EventIncarnationRerunLast, claims.Subject, applyID, map[string]any{
		"name":            a.Name,
		"reason":          a.Reason,
		"scenario":        res.Scenario,
		"previous_status": string(res.PreviousStatus),
		"apply_id":        applyID,
	})

	return h.toolResult(req.ID, incarnationRerunLastOutput{
		ApplyID:     applyID,
		Incarnation: a.Name,
		Scenario:    res.Scenario,
	})
}
