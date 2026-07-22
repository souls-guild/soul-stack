package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationUpgradeArgs — arguments for keeper.incarnation.upgrade
// (schemaIncarnationUpgradeInput): name + to_version required. to_version is
// the git-ref of the target service version (ADR-007).
type incarnationUpgradeArgs struct {
	Name      string `json:"name"`
	ToVersion string `json:"to_version"`
}

// incarnationUpgradeOutput — output of keeper.incarnation.upgrade
// (schemaApplyIDOutput): apply_id of a single upgrade operation (shared
// across the migration chain). Mirrors REST upgradeResponse.
type incarnationUpgradeOutput struct {
	ApplyID string `json:"_apply_id"`
}

// callIncarnationUpgrade — mutating async tool keeper.incarnation.upgrade.
// Parity with REST IncarnationHandler.Upgrade: SelectByName →
// [incarnation.PrepareUpgrade] (same helper as REST) →
// [incarnation.UpgradeStateSchema] (sync under a 202) → apply_id. apply_id
// is generated here (ULID) and passed into PrepareUpgrade and
// UpgradeStateSchema; ChangedByAID = claims.Subject.
//
// RBAC-context — {"incarnation": name} (name-bound). audit:
// EventIncarnationUpgradeStarted {name, to_version, apply_id}, source=mcp.
//
// Sentinel mapping for the prepare and tx phases shares
// [mapIncarnationErrorToMCP] (downgrade / no-op / chain-broken / busy /
// locked / schema-mismatch — same codes as REST). migration_failed status
// is covered by incarnation-locked (see errors.go § mcpCodeMigrationFailed).
func (h *Handler) callIncarnationUpgrade(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.upgrade"

	var a incarnationUpgradeArgs
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
	if a.ToVersion == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'to_version' is required")
	}

	// loader / services are required: without them there's nothing to
	// materialize the target ref's snapshot from (parity with REST Upgrade).
	if h.deps.ServiceLoader == nil || h.deps.ServiceRegistry == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError,
			"service loader is not configured")
	}

	inc, err := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if err != nil {
		// Fail-closed RBAC on a not-found/failed incarnation lookup (parity with REST).
		if scopeErr := h.checkIncarnationScope(claims, "upgrade", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.upgrade")
		}
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.upgrade select failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// RBAC OR-Check over the incarnation's coven/service scope (covens ∪
	// {name}) — mirrors REST middleware, scope from inc.Service / inc.Covens.
	if err := h.checkIncarnationScope(claims, "upgrade", inc.Name, inc.Service, inc.Covens); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.upgrade")
	}

	applyID := audit.NewULID()
	changedBy := claims.Subject
	upIn, err := incarnation.PrepareUpgrade(ctx, h.deps.ServiceRegistry, h.deps.ServiceLoader, inc, a.ToVersion, applyID, &changedBy)
	if err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.upgrade prepare failed",
				slog.String("name", a.Name),
				slog.String("service", inc.Service),
				slog.String("to_version", a.ToVersion),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	if _, err := incarnation.UpgradeStateSchema(ctx, h.deps.IncarnationDB, upIn); err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.upgrade failed",
				slog.String("name", a.Name),
				slog.String("to_version", a.ToVersion),
				slog.String("apply_id", applyID),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventIncarnationUpgradeStarted, claims.Subject, map[string]any{
		"name":       a.Name,
		"to_version": a.ToVersion,
		"apply_id":   applyID,
	})

	return h.toolResult(req.ID, incarnationUpgradeOutput{ApplyID: applyID})
}
