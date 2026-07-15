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

// incarnationRunArgs — arguments for keeper.incarnation.run
// (schemaIncarnationRunInput): name + scenario are required, input is optional.
type incarnationRunArgs struct {
	Name     string         `json:"name"`
	Scenario string         `json:"scenario"`
	Input    map[string]any `json:"input,omitempty"`
}

// incarnationRunOutput — output of keeper.incarnation.run classic single-run
// (schemaIncarnationRunOutput): apply_id + echoed incarnation / scenario.
// Mirrors REST runScenarioResponse.
type incarnationRunOutput struct {
	ApplyID     string `json:"_apply_id"`
	Incarnation string `json:"incarnation"`
	Scenario    string `json:"scenario"`
}

// callIncarnationRun — mutating async tool keeper.incarnation.run. Parity
// with REST IncarnationHandler.Run: resolve incarnation → secondary
// error_locked probe → resolve service → runner.Start → 202 + apply_id.
//
// RBAC context — {"incarnation": name} (name-bound). audit:
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

	// runner / services are required to start a run (parity with REST Run).
	if h.deps.ScenarioRunner == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError,
			"scenario runner is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC when the incarnation is missing/failed to load
		// (parity with REST: IncarnationScopeSelector would return a nil set →
		// scoped deny, bare/`*` passes → handler returns 404/500). Forbidden
		// takes priority over 404.
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

	// RBAC OR-Check over the incarnation's coven/service scope (covens ∪
	// {name}) — mirrors REST middleware. Checked AFTER select: the scope is
	// built from inc.Service / inc.Covens.
	if err := h.checkIncarnationScope(claims, "run", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.run")
	}

	// Secondary gate layer: fast rejection for error_locked before starting a
	// run (authority is lockRun under FOR UPDATE; parity with REST).
	if inc.Status == incarnation.StatusErrorLocked {
		return h.toolError(req.ID, toolName, mcpCodeIncarnationLocked,
			"incarnation "+a.Name+" is error_locked — unlock required before next run")
	}

	serviceRef, ok := h.deps.ServiceRegistry.Resolve(inc.Service)
	if !ok {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"service "+inc.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	// Sync validation of input against the scenario's `input:` schema — BEFORE
	// enqueue (parity with REST Run, both modes). nil loader → degrades to no
	// validation. Invalid input → validation-failed; snapshot failure → internal.
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
