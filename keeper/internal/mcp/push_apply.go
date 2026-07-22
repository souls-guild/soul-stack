package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
)

// pushNotConfigured — public-detail nil-guard for push-tools. PushRun is an
// optional HandlerDeps field (production wire-up passes *pushorch.PushRun
// only when SshDispatcher is up): when nil, the tool still dispatches but
// returns internal-error "not configured" (RBACRoles/SigilSvc pattern).
const pushNotConfigured = "push orchestrator is not configured"

// pushApplyArgs — arguments for keeper.push.apply (schemaPushApplyInput):
// inventory + destiny are required, the rest is optional.
// cleanup_stale_versions maps to request.CleanupStale (short name in
// pushorch.ApplyRequest).
type pushApplyArgs struct {
	Inventory            []string       `json:"inventory"`
	Destiny              string         `json:"destiny"`
	Input                map[string]any `json:"input,omitempty"`
	SSHProvider          string         `json:"ssh_provider,omitempty"`
	CleanupStaleVersions bool           `json:"cleanup_stale_versions,omitempty"`
}

// callPushApply — mutating tool keeper.push.apply. Transport over
// [pushorch.PushRun.Apply]: all business logic (parse destiny, Insert(pending),
// async-execute) lives in the orchestrator; the tool decodes input, checks
// permission, maps sentinels to MCP codes. Audit (push.applied) is written
// by the orchestrator — no duplicate needed here.
//
// RBAC — push.apply without a selector (push has no targeting by
// incarnation/coven in MVP — the service/coven/incarnation/host selectors
// from the closed enum don't cover push.* in the current slice). Context
// nil — the permission doesn't depend on the request body.
func (h *Handler) callPushApply(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.push.apply"

	if h.deps.PushRun == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, pushNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "push", "apply", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission push.apply")
	}

	var a pushApplyArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if len(a.Inventory) == 0 {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'inventory' is required and must be non-empty")
	}
	if a.Destiny == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'destiny' is required")
	}

	applyID, err := h.deps.PushRun.Apply(ctx, pushorch.ApplyRequest{
		InventorySIDs: a.Inventory,
		DestinyRef:    a.Destiny,
		SSHProvider:   a.SSHProvider,
		Input:         a.Input,
		CleanupStale:  a.CleanupStaleVersions,
		StartedByAID:  claims.Subject,
	})
	if err != nil {
		if errors.Is(err, pushorch.ErrInvalidDestinyRef) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, err.Error())
		}
		h.deps.Logger.Error("mcp: push.apply orchestrator accept failed",
			slog.String("destiny", a.Destiny),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "push apply failed")
	}

	// schemaApplyIDOutput → `{ "_apply_id": "<ULID>" }`. The JSON tag _apply_id
	// is the schemaApplyIDOutput contract (see manifest.go).
	return h.toolResult(req.ID, struct {
		ApplyID string `json:"_apply_id"`
	}{ApplyID: applyID})
}
