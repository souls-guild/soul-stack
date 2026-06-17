package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// roleDeleteArgs — arguments tool-а keeper.role.delete (schemaRoleDeleteInput):
// только name.
type roleDeleteArgs struct {
	Name string `json:"name"`
}

// callRoleDelete — mutating-tool keeper.role.delete. Транспорт поверх
// [rbac.Service.DeleteRole]: builtin-граница и self-lockout-проверка — в
// Service (под FOR UPDATE); tool маппит ErrRoleNotFound/ErrRoleBuiltin/
// ErrWouldLockOutCluster в MCP-коды.
//
// RBAC — role.delete без селектора (nil-context).
func (h *Handler) callRoleDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.delete"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно HTTP-handler-у (изменение авторизации, ADR-022).
	h.writeAudit(audit.EventRoleDeleted, claims.Subject, map[string]any{
		"name": a.Name,
	})

	// HTTP-эквивалент — 204 No Content; MCP — пустой output-объект.
	return h.toolResult(req.ID, struct{}{})
}
