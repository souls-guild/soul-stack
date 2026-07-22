package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// roleRevokeOperatorArgs — arguments for the keeper.role.revoke-operator tool
// (schemaRoleRevokeOperatorInput): role + aid are required.
type roleRevokeOperatorArgs struct {
	Role string `json:"role"`
	AID  string `json:"aid"`
}

// callRoleRevokeOperator — mutating-tool keeper.role.revoke-operator. A
// transport layer over [rbac.Service.RevokeOperator]: removes the membership
// row (role, aid). The self-lockout check (removing the last admin with `*`)
// lives in Service under FOR UPDATE; the tool maps ErrRoleOperatorNotFound to
// not-found, ErrWouldLockOutCluster to would-lock-out-cluster.
//
// RBAC — role.revoke-operator without a selector (nil-context).
func (h *Handler) callRoleRevokeOperator(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.revoke-operator"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context is nil —
	// the permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "role", "revoke-operator", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission role.revoke-operator")
	}

	var a roleRevokeOperatorArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Role == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'role' is required")
	}
	if a.AID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' is required")
	}
	if !operator.ValidAID(a.AID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'aid' must match "+operator.AIDPattern)
	}

	err := h.deps.RBACRoles.RevokeOperator(ctx, rbac.RevokeOperatorInput{
		RoleName: a.Role,
		AID:      a.AID,
	})
	if err != nil {
		code, detail := mapRoleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: role.revoke-operator failed",
				slog.String("role", a.Role),
				slog.String("aid", a.AID),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — mirrors the HTTP handler (authorization change, ADR-022):
	// payload {name, aid}. AIDs aren't secret.
	h.writeAudit(audit.EventRoleOperatorRevoked, claims.Subject, map[string]any{
		"name": a.Role,
		"aid":  a.AID,
	})

	// HTTP equivalent — 204 No Content; MCP — empty output object.
	return h.toolResult(req.ID, struct{}{})
}
