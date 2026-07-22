package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// roleDeleteArgs — arguments for the keeper.role.delete tool
// (schemaRoleDeleteInput): name only.
type roleDeleteArgs struct {
	Name string `json:"name"`
}

// callRoleDelete — mutating tool keeper.role.delete. Transport over
// [rbac.Service.DeleteRole]: the builtin boundary and self-lockout check live
// in Service (under FOR UPDATE); the tool maps ErrRoleNotFound/ErrRoleBuiltin/
// ErrWouldLockOutCluster to MCP codes.
//
// RBAC — role.delete has no selector (nil context).
func (h *Handler) callRoleDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.delete"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "role", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission role.delete")
	}

	var a roleDeleteArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	if err := h.deps.RBACRoles.DeleteRole(ctx, a.Name); err != nil {
		code, detail := mapRoleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: role.delete failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallel to the HTTP handler (authorization change, ADR-022).
	h.writeAudit(audit.EventRoleDeleted, claims.Subject, map[string]any{
		"name": a.Name,
	})

	// HTTP equivalent — 204 No Content; MCP — an empty output object.
	return h.toolResult(req.ID, struct{}{})
}
