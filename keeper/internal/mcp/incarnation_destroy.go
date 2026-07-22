package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationDestroyArgs — arguments for keeper.incarnation.destroy
// (schemaIncarnationDestroyInput): name + allow_destroy are required.
// Operator-facing allow_destroy maps to internal force (unifying
// force↔allow_destroy): false — destroy via the `destroy` teardown scenario;
// true — removal without teardown (force-DELETE).
type incarnationDestroyArgs struct {
	Name         string `json:"name"`
	AllowDestroy *bool  `json:"allow_destroy"`
}

// incarnationDestroyOutput — output of keeper.incarnation.destroy
// (schemaApplyIDOutput): apply_id of a single destroy operation.
type incarnationDestroyOutput struct {
	ApplyID string `json:"_apply_id"`
}

// destroyForceDeleteTimeout — timeout for the detached-ctx force-DELETE
// (parity with REST Destroy and run.go S-D3). The removal outlives the tool
// result return.
const destroyForceDeleteTimeout = 5 * time.Second

// callIncarnationDestroy — mutating async tool keeper.incarnation.destroy.
// Parity with REST IncarnationHandler.Destroy (S-D4): resolve incarnation →
// PrepareDestroy (S-D2a) → Destroy (S-D1, source=mcp) → force? DeleteAfterTeardown
// (S-D3 force path) : StartDestroy (S-D2b teardown).
//
// allow_destroy — required bool (missing/non-bool → malformed-request,
// strictUnmarshal). false with no scenario `destroy` → validation-failed
// (PrepareDestroy). RBAC context — {"incarnation": name} (name-bound).
//
// audit destroy_started is written by [incarnation.Destroy] itself (source=mcp,
// archonAID from JWT) — NOT duplicated via h.writeAudit; destroy_completed
// (force path) is written by [incarnation.DeleteAfterTeardown].
func (h *Handler) callIncarnationDestroy(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.destroy"

	var a incarnationDestroyArgs
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
	if a.AllowDestroy == nil {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'allow_destroy' is required (boolean confirmation flag)")
	}
	force := *a.AllowDestroy

	// destroyer / services / loader are required (parity with REST): without
	// them the endpoint isn't functional (nothing to pre-check the snapshot or
	// start teardown with).
	if h.deps.ScenarioDestroyer == nil || h.deps.ServiceRegistry == nil || h.deps.ServiceLoader == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "destroy is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC on a not-found/failed incarnation lookup (parity with REST).
		if scopeErr := h.checkIncarnationScope(claims, "destroy", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.destroy")
		}
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.destroy select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// RBAC OR-check over the incarnation's coven/service scope (covens ∪ {name})
	// — mirrors the REST middleware, scope from inc.Service / inc.Covens.
	if err := h.checkIncarnationScope(claims, "destroy", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.destroy")
	}

	// S-D2a pre-check: resolve the snapshot (force=true — the scenario-missing
	// gate is applied after reading lifecycle.auto_destroy, same as REST Destroy).
	art, err := incarnation.PrepareDestroy(ctx, h.deps.ServiceRegistry, h.deps.ServiceLoader, inc, true)
	if err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.destroy prepare failed",
				slog.String("name", a.Name),
				slog.String("service", inc.Service),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// S3 enforcement: lifecycle.auto_destroy=false → removal is ALWAYS direct
	// (takes priority over allow_destroy). effectiveForce = the operator forces
	// it OR the service disallows teardown.
	autoDestroy := true
	if art != nil && art.Manifest != nil {
		autoDestroy = art.Manifest.Lifecycle.AutoDestroyEnabled()
	}
	effectiveForce := force || !autoDestroy

	// effectiveForce=false → teardown is required: scenario `destroy` must be
	// present in the snapshot, otherwise validation-failed BEFORE transitioning
	// to destroying.
	if !effectiveForce {
		hasScenario, herr := incarnation.HasDestroyScenario(h.deps.ServiceLoader, art)
		if herr != nil {
			h.deps.Logger.Error("mcp: incarnation.destroy scenario probe failed",
				slog.String("name", a.Name),
				slog.String("service", inc.Service),
				slog.Any("error", herr),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "prepare incarnation destroy failed")
		}
		if !hasScenario {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"service "+inc.Service+" has no `destroy` scenario — pass allow_destroy=true to force destroy without teardown")
		}
	}

	applyID := audit.NewULID()

	// S-D1: transition to destroying + audit destroy_started (source=mcp).
	// AuditWriter is narrowed to mcp.AuditWriter, structurally = audit.Writer
	// (Destroy accepts it).
	if _, err := incarnation.Destroy(ctx, h.deps.IncarnationDB, h.deps.AuditWriter, a.Name, effectiveForce,
		audit.SourceMCP, claims.Subject, applyID, h.deps.Logger); err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.destroy transition failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// effectiveForce=true → S-D3 force path: removes the row directly (teardown
	// is skipped; allow_destroy=true OR lifecycle.auto_destroy=false). Detached
	// ctx — the removal outlives the tool-result return.
	if effectiveForce {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), destroyForceDeleteTimeout)
		defer cancel()
		if _, err := incarnation.DeleteAfterTeardown(dctx, h.deps.IncarnationDB, h.deps.AuditWriter, a.Name, effectiveForce, h.deps.Logger); err != nil {
			h.deps.Logger.Error("mcp: incarnation.destroy force delete failed",
				slog.String("name", a.Name),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
			return h.toolError(req.ID, toolName, mcpCodeInternalError, "force-destroy delete failed")
		}
		return h.toolResult(req.ID, incarnationDestroyOutput{ApplyID: applyID})
	}

	// effectiveForce=false → S-D2b: async teardown scenario `destroy` (TerminalDestroy).
	serviceRef, ok := h.deps.ServiceRegistry.Resolve(inc.Service)
	if !ok {
		// A service-deregistration race between the pre-check and starting teardown.
		h.deps.Logger.Error("mcp: incarnation.destroy service deregistered between prepare and teardown",
			slog.String("name", a.Name), slog.String("service", inc.Service))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "service "+inc.Service+" is not registered")
	}
	serviceRef.Ref = inc.ServiceVersion
	if err := h.deps.ScenarioDestroyer.StartDestroy(ctx, scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: a.Name,
		ServiceRef:      serviceRef,
		StartedByAID:    claims.Subject,
	}); err != nil {
		h.deps.Logger.Error("mcp: incarnation.destroy teardown start failed",
			slog.String("name", a.Name),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "start destroy teardown failed")
	}

	return h.toolResult(req.ID, incarnationDestroyOutput{ApplyID: applyID})
}
