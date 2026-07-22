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

// incarnationCheckDriftArgs — arguments for keeper.incarnation.check-drift
// (schemaIncarnationCheckDriftInput): name is required, input is an
// opt-override of converge parameters.
type incarnationCheckDriftArgs struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// callIncarnationCheckDrift — sync tool keeper.incarnation.check-drift (ADR-031
// Slice B). Parity with REST IncarnationHandler.CheckDrift: resolve
// incarnation → scope check → resolve service → drift.CheckDrift →
// MarkDriftStatus → audit → 200 + DriftReport.
//
// RBAC OR-Check on incarnation coven/service-scope (covens ∪ {name}) —
// mirrors the REST middleware (handlers.IncarnationCovenContexts).
// Fail-closed on a not-found incarnation (scoped → deny before 404, parity
// with REST Run/Destroy).
//
// audit: EventIncarnationDriftChecked {name, scenario, apply_id, drift_summary},
// source=mcp. archon_aid — claims.Subject.
func (h *Handler) callIncarnationCheckDrift(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.check-drift"

	var a incarnationCheckDriftArgs
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

	if h.deps.ScenarioDrift == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError,
			"drift checker is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC on a not-found/failed incarnation (parity with REST):
		// a scoped operator gets Forbidden before 404, bare/`*` gets 404.
		if scopeErr := h.checkIncarnationScope(claims, "check-drift", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.check-drift")
		}
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.check-drift select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	if err := h.checkIncarnationScope(claims, "check-drift", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.check-drift")
	}

	serviceRef, ok := h.deps.ServiceRegistry.Resolve(inc.Service)
	if !ok {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"service "+inc.Service+" is not registered (manage via service.* API, ADR-029)")
	}

	applyID := audit.NewULID()
	report, err := h.deps.ScenarioDrift.CheckDrift(ctx, scenario.CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: a.Name,
		ServiceRef:      serviceRef,
		InputOverride:   a.Input,
		StartedByAID:    claims.Subject,
	})
	if err != nil {
		if errors.Is(err, scenario.ErrConvergeMissing) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"drift check unavailable for service "+inc.Service+": converge scenario missing from the current service snapshot")
		}
		if errors.Is(err, scenario.ErrDriftInputMissing) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"drift-input does not resolve: "+err.Error())
		}
		h.deps.Logger.Error("mcp: incarnation.check-drift failed",
			slog.String("name", a.Name),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "check-drift failed")
	}

	hasDrift := report.Summary.HostsDrifted > 0 || report.Summary.HostsFailed > 0
	if err := h.deps.ScenarioDrift.MarkDriftStatus(ctx, a.Name, hasDrift); err != nil {
		h.deps.Logger.Warn("mcp: incarnation.check-drift status not recorded",
			slog.String("name", a.Name), slog.Any("error", err))
	}

	h.writeAuditCorrelated(audit.EventIncarnationDriftChecked, claims.Subject, applyID, map[string]any{
		"name":     a.Name,
		"scenario": scenario.ConvergeScenarioName,
		"apply_id": applyID,
		"drift_summary": map[string]any{
			"hosts_drifted":     report.Summary.HostsDrifted,
			"hosts_clean":       report.Summary.HostsClean,
			"hosts_unsupported": report.Summary.HostsUnsupported,
			"hosts_failed":      report.Summary.HostsFailed,
		},
	})

	return h.toolResult(req.ID, report)
}
