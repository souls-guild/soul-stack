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

// roleGrantOperatorArgs — arguments for keeper.role.grant-operator
// (schemaRoleGrantOperatorInput): role + aid are required.
type roleGrantOperatorArgs struct {
	Role string `json:"role"`
	AID  string `json:"aid"`
}

// callRoleGrantOperator — mutating-tool keeper.role.grant-operator. Transport
// over [rbac.Service.GrantOperator]: binds an AID to a role (idempotent).
// CallerAID comes from claims (granted_by_aid). Role/AID existence is
// checked in Service (role lock + FK on aid); the tool maps
// ErrRoleNotFound/ErrOperatorNotFound to not-found.
//
// RBAC — role.grant-operator has no selector (nil-context).
func (h *Handler) callRoleGrantOperator(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.grant-operator"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. nil context —
	// the permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "role", "grant-operator", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission role.grant-operator")
	}

	var a roleGrantOperatorArgs
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

	callerAID := claims.Subject
	err := h.deps.RBACRoles.GrantOperator(ctx, rbac.GrantOperatorInput{
		RoleName:  a.Role,
		AID:       a.AID,
		CallerAID: &callerAID,
	})
	if err != nil {
		code, detail := mapRoleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: role.grant-operator failed",
				slog.String("role", a.Role),
				slog.String("aid", a.AID),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit mirrors the HTTP handler (authorization change, ADR-022):
	// payload {name, aid, granted_by_aid}. AIDs aren't secret.
	h.writeAudit(audit.EventRoleOperatorGranted, claims.Subject, map[string]any{
		"name":           a.Role,
		"aid":            a.AID,
		"granted_by_aid": callerAID,
	})

	return h.toolResult(req.ID, struct{}{})
}
