package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// incarnationUnlockArgs — arguments tool-а keeper.incarnation.unlock
// (schemaIncarnationUnlockInput): name + reason обязательны. reason пишется
// в audit-payload (паритет REST).
type incarnationUnlockArgs struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// incarnationUnlockOutput — output keeper.incarnation.unlock. Симметричен REST
// unlockResponse (IncarnationUnlockReply).
type incarnationUnlockOutput struct {
	Name           string    `json:"name"`
	PreviousStatus string    `json:"previous_status"`
	Status         string    `json:"status"`
	UnlockedByAID  string    `json:"unlocked_by_aid"`
	UnlockedAt     time.Time `json:"unlocked_at"`
}

// callIncarnationUnlock — mutating-tool keeper.incarnation.unlock. Паритет
// REST IncarnationHandler.Unlock: снятие блокирующего статуса (error_locked /
// migration_failed → ready) под FOR UPDATE, state не меняется.
//
// RBAC-context — {"incarnation": name} (name-bound). audit:
// EventIncarnationUnlocked {name, previous_status, reason}, source=mcp
// (writeAudit). RBAC ДО бизнес-вызова; audit — после успешного Unlock.
func (h *Handler) callIncarnationUnlock(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.unlock"

	var a incarnationUnlockArgs
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

	// RBAC OR-Check по coven/service-scope incarnation (covens ∪ {name}) —
	// зеркало REST middleware. Unlock сам делает FOR UPDATE-select внутри
	// бизнес-вызова, поэтому scope резолвим отдельным probe-SelectByName (тот
	// же холодный RBAC-round-trip, что REST IncarnationScopeSelector). Битый
	// probe → fail-closed (scoped deny, bare/`*` pass → Unlock вернёт 404/500).
	inc, probeErr := incarnation.SelectByName(ctx, h.deps.IncarnationDB, a.Name)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "unlock", a.Name, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.unlock")
		}
	} else if scopeErr := h.checkIncarnationScope(claims, "unlock", inc.Name, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.unlock")
	}

	historyID := audit.NewULID()
	res, err := incarnation.Unlock(ctx, h.deps.IncarnationDB, a.Name, a.Reason, claims.Subject, historyID)
	if err != nil {
		code, detail := mapIncarnationErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: incarnation.unlock failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventIncarnationUnlocked, claims.Subject, map[string]any{
		"name":            a.Name,
		"previous_status": string(res.PreviousStatus),
		"reason":          a.Reason,
	})

	return h.toolResult(req.ID, incarnationUnlockOutput{
		Name:           a.Name,
		PreviousStatus: string(res.PreviousStatus),
		Status:         string(incarnation.StatusReady),
		UnlockedByAID:  claims.Subject,
		UnlockedAt:     time.Now().UTC(),
	})
}
