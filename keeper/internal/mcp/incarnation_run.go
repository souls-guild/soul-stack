package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationRunArgs — arguments tool-а keeper.incarnation.run
// (schemaIncarnationRunInput): name + scenario обязательны, input опционален.
type incarnationRunArgs struct {
	Name     string         `json:"name"`
	Scenario string         `json:"scenario"`
	Input    map[string]any `json:"input,omitempty"`
}

// incarnationRunOutput — output keeper.incarnation.run classic single-run
// (schemaIncarnationRunOutput): apply_id + echo incarnation / scenario.
// Симметричен REST runScenarioResponse.
type incarnationRunOutput struct {
	ApplyID     string `json:"_apply_id"`
	Incarnation string `json:"incarnation"`
	Scenario    string `json:"scenario"`
}

// callIncarnationRun — mutating async-tool keeper.incarnation.run. Паритет
// REST IncarnationHandler.Run: резолв incarnation → вторичный probe
// error_locked → резолв service → runner.Start → 202 + apply_id.
//
// RBAC-context — {"incarnation": name} (name-bound). audit:
// EventIncarnationScenarioStarted {name, scenario, apply_id}, source=mcp.
func (h *Handler) callIncarnationRun(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.run"

	var a incarnationRunArgs
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
	if a.Scenario == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'scenario' is required")
	}
	if !scenario.ValidScenarioName(a.Scenario) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'scenario' must match "+scenario.ScenarioNamePattern)
	}

	// runner / services обязательны для запуска прогона (паритет REST Run).
	if h.deps.ScenarioRunner == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError,
			"scenario runner is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC при ненайденной/сбойной incarnation (паритет REST:
		// IncarnationScopeSelector вернул бы nil-набор → scoped deny, bare/`*`
		// pass → handler отдаёт 404/500). Forbidden имеет приоритет над 404.
		if scopeErr := h.checkIncarnationScope(claims, "run", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.run")
		}
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.run select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// RBAC OR-Check по coven/service-scope incarnation (covens ∪ {name}) —
	// зеркало REST middleware. Проверка ПОСЛЕ select: scope строится из
	// inc.Service / inc.Covens.
	if err := h.checkIncarnationScope(claims, "run", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.run")
	}

	// Вторичный слой gate-а: быстрый отказ для error_locked до запуска прогона
	// (авторитет — lockRun под FOR UPDATE; паритет REST).
	if inc.Status == incarnation.StatusErrorLocked {
		return h.toolError(req.ID, toolName, mcpCodeIncarnationLocked,
			"incarnation "+a.Name+" is error_locked — unlock required before next run")
	}

	serviceRef, ok := h.deps.ServiceRegistry.Resolve(inc.Service)
	if !ok {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"service "+inc.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	// Sync-валидация input против `input:`-схемы запускаемого scenario — ДО
	// enqueue (паритет REST Run, оба режима). nil loader → деградация без
	// проверки. Невалидный input → validation-failed; сбой снапшота → internal.
	if h.deps.ServiceLoader != nil {
		if err := scenario.ValidateInput(ctx, h.deps.ServiceLoader, serviceRef, a.Scenario, a.Input); err != nil {
			if errors.Is(err, scenario.ErrInputInvalid) {
				return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
					"input_invalid: "+err.Error())
			}
			h.deps.Logger.Error("mcp: incarnation.run input validation failed",
				slog.String("name", a.Name),
				slog.String("scenario", a.Scenario),
				slog.Any("error", err),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError,
				"validate scenario "+a.Scenario+" input failed")
		}
	}

	applyID := audit.NewULID()
	if err := h.deps.ScenarioRunner.Start(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: a.Name,
		ServiceRef:      serviceRef,
		ScenarioName:    a.Scenario,
		Input:           a.Input,
		StartedByAID:    claims.Subject,
	}); err != nil {
		h.deps.Logger.Error("mcp: incarnation.run scenario start failed",
			slog.String("name", a.Name),
			slog.String("scenario", a.Scenario),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError,
			"start scenario "+a.Scenario+" failed")
	}

	h.writeAudit(audit.EventIncarnationScenarioStarted, claims.Subject, map[string]any{
		"name":     a.Name,
		"scenario": a.Scenario,
		"apply_id": applyID,
	})

	return h.toolResult(req.ID, incarnationRunOutput{
		ApplyID:     applyID,
		Incarnation: a.Name,
		Scenario:    a.Scenario,
	})
}
