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

// incarnationRerunCreateArgs — arguments tool-а keeper.incarnation.rerun-create
// (schemaIncarnationRerunCreateInput): name + reason обязательны. reason пишется
// в audit-payload (паритет REST RerunCreate).
type incarnationRerunCreateArgs struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// incarnationRerunCreateOutput — output keeper.incarnation.rerun-create
// (schemaApplyIDOutputWithIncarnation): apply_id перезапущенного scenario
// `create` + echo incarnation. Симметричен REST IncarnationRerunCreateReply.
type incarnationRerunCreateOutput struct {
	ApplyID     string `json:"_apply_id"`
	Incarnation string `json:"incarnation"`
}

// callIncarnationRerunCreate — mutating async-tool keeper.incarnation.rerun-create.
// Паритет REST IncarnationHandler.RerunCreate: снимает error_locked и тем же
// действием перезапускает scenario `create` (architecture.md → «Атомарность и
// error_locked»). Под одним FOR UPDATE: error_locked → applying минуя ready.
//
// RBAC-context — covens ∪ {name} (name-bound, паритет unlock). audit:
// EventIncarnationCreateRerun {name, reason, previous_status, apply_id},
// source=mcp, correlation_id=apply_id. RBAC ДО бизнес-вызова; audit — после
// успешного запуска.
func (h *Handler) callIncarnationRerunCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.rerun-create"

	var a incarnationRerunCreateArgs
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

	// runner / services обязательны: rerun перезапускает scenario `create`.
	if h.deps.ScenarioRunner == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "scenario runner is not configured")
	}

	// RBAC OR-Check по coven/service-scope incarnation (covens ∪ {name}) —
	// зеркало REST middleware. Битый probe → fail-closed.
	inc, probeErr := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "create-rerun", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.create-rerun")
		}
		code, detail := mapIncarnationErrorToMCP(probeErr)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.rerun-create select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", probeErr),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	if scopeErr := h.checkIncarnationScope(claims, "create-rerun", inc.Name, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.create-rerun")
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
			h.deps.Logger.Error("mcp: incarnation.rerun-create unlock failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Перезапуск scenario `create` (async): статус уже applying (UnlockForRerun).
	// FromLocked — lockRun не транзитит статус повторно, обязан увидеть applying.
	if err := h.deps.ScenarioRunner.Start(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: a.Name,
		ServiceRef:      serviceRef,
		ScenarioName:    scenario.CreateScenarioName,
		StartedByAID:    claims.Subject,
		FromLocked:      true,
	}); err != nil {
		h.deps.Logger.Error("mcp: incarnation.rerun-create scenario start failed",
			slog.String("name", a.Name),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "start scenario create failed")
	}

	h.writeAuditCorrelated(audit.EventIncarnationCreateRerun, claims.Subject, applyID, map[string]any{
		"name":            a.Name,
		"reason":          a.Reason,
		"previous_status": string(res.PreviousStatus),
		"apply_id":        applyID,
	})

	return h.toolResult(req.ID, incarnationRerunCreateOutput{
		ApplyID:     applyID,
		Incarnation: a.Name,
	})
}
