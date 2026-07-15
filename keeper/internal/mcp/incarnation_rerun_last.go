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

// incarnationRerunLastArgs — arguments for keeper.incarnation.rerun-last
// (schemaIncarnationRerunLastInput): name + reason required. reason is
// written to the audit payload (parity with REST RerunLast).
type incarnationRerunLastArgs struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// incarnationRerunLastOutput — output of keeper.incarnation.rerun-last
// (schemaIncarnationRerunLastOutput): apply_id of the restarted scenario +
// echo incarnation + scenario (name of the one restarted). Mirrors REST
// IncarnationRerunLastReply.
type incarnationRerunLastOutput struct {
	ApplyID     string `json:"_apply_id"`
	Incarnation string `json:"incarnation"`
	Scenario    string `json:"scenario"`
}

// callIncarnationRerunLast — mutating async tool keeper.incarnation.rerun-last.
// Parity with REST IncarnationHandler.RerunLast: clears error_locked and
// restarts the LAST failed scenario in the same action — bootstrap
// `create`/… or day-2 add_user/… (architecture.md → "Atomicity and
// error_locked"). Under one FOR UPDATE: error_locked → applying, skipping ready.
//
// RBAC-context — covens ∪ {name} (name-bound, parity with unlock). audit:
// EventIncarnationRerunLast {name, reason, scenario, previous_status,
// apply_id}, source=mcp, correlation_id=apply_id. RBAC before the business
// call; audit after a successful start.
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

	// runner / services are required: rerun restarts a scenario.
	if h.deps.ScenarioRunner == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "scenario runner is not configured")
	}

	// RBAC OR-Check over the incarnation's coven/service scope (covens ∪
	// {name}) — mirrors REST middleware. A failed probe → fail-closed.
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

	// applyID is shared: the unlock-transition state_history snapshot and the restarted run.
	applyID := audit.NewULID()

	// Unlock step under FOR UPDATE: error_locked → applying, skipping ready (race-free).
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

	// Restart the last failed scenario (async): status is already applying
	// (UnlockForRerun). FromLocked — lockRun must not transition status
	// again, it expects to see applying already. ScenarioName — name of the
	// failed scenario from UnlockResult (create or day-2). Input — its saved
	// input (spec.input on the create path / recipe.input on the day-2 path,
	// read under the same FOR UPDATE): without it, a restart with required
	// fields (redis cluster: version/shards) would fail input validation or
	// silently apply defaults (parity with REST RerunLastTyped).
	if err := h.deps.ScenarioRunner.Start(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: a.Name,
		ServiceRef:      serviceRef,
		ScenarioName:    res.Scenario,
		Input:           res.Input,
		StartedByAID:    claims.Subject,
		FromLocked:      true,
		// The failed run may have been an upgrade scenario (recipe.from_upgrade)
		// — restart from upgrade/<slug>/, not scenario/ (ADR-0068, parity with
		// REST RerunLastTyped).
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
