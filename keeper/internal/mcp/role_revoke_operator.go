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

// roleRevokeOperatorArgs — arguments tool-а keeper.role.revoke-operator
// (schemaRoleRevokeOperatorInput): role + aid обязательны.
type roleRevokeOperatorArgs struct {
	Role string `json:"role"`
	AID  string `json:"aid"`
}

// callRoleRevokeOperator — mutating-tool keeper.role.revoke-operator.
// Транспорт поверх [rbac.Service.RevokeOperator]: снятие membership-строки
// (role, aid). self-lockout-проверка (снятие последнего админа с `*`) — в
// Service под FOR UPDATE; tool маппит ErrRoleOperatorNotFound в not-found,
// ErrWouldLockOutCluster в would-lock-out-cluster.
//
// RBAC — role.revoke-operator без селектора (nil-context).
func (h *Handler) callRoleRevokeOperator(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.revoke-operator"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно HTTP-handler-у (изменение авторизации, ADR-022):
	// payload {name, aid}. AID-ы не секрет.
	h.writeAudit(audit.EventRoleOperatorRevoked, claims.Subject, map[string]any{
		"name": a.Role,
		"aid":  a.AID,
	})

	// HTTP-эквивалент — 204 No Content; MCP — пустой output-объект.
	return h.toolResult(req.ID, struct{}{})
}
