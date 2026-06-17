package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// roleUpdateArgs — arguments tool-а keeper.role.update (schemaRoleUpdateInput):
// name + permissions обязательны. permissions — новый набор (replace-семантика).
// default_scope (ADR-047 S1) опционален: ключ ОТСУТСТВУЕТ → scope не трогается;
// присутствует (включая null) → заменяет (null снимает scope).
type roleUpdateArgs struct {
	Name         string   `json:"name"`
	Permissions  []string `json:"permissions"`
	DefaultScope *string  `json:"default_scope"`
}

// callRoleUpdate — mutating-tool keeper.role.update. Транспорт поверх
// [rbac.Service.UpdateRolePermissions] (replace-семантика): builtin-граница,
// валидация permissions и self-lockout (снятие последнего `*`) — в Service;
// tool маппит sentinel-ы в MCP-коды.
//
// RBAC — role.update без селектора (nil-context).
func (h *Handler) callRoleUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.update"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
	if err := h.deps.RBAC.Check(claims.Subject, "role", "update", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission role.update")
	}

	var a roleUpdateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	// presence ключа default_scope в сырых args: omitted (не трогать scope)
	// vs explicit (заменить, в т.ч. null → снять). *string не различает.
	hasScope := rawArgHasKey(args, "default_scope")

	err := h.deps.RBACRoles.UpdateRolePermissions(ctx, rbac.UpdateRolePermissionsInput{
		Name:            a.Name,
		Permissions:     a.Permissions,
		CallerAID:       claims.Subject,
		SetDefaultScope: hasScope,
		DefaultScope:    a.DefaultScope,
	})
	if err != nil {
		code, detail := mapRoleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: role.update failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — параллельно HTTP-handler-у (изменение авторизации, ADR-022):
	// payload {name, permissions} (новый набор). permission-строки не секрет.
	h.writeAudit(audit.EventRolePermissionsUpdated, claims.Subject, map[string]any{
		"name":        a.Name,
		"permissions": a.Permissions,
	})

	return h.toolResult(req.ID, struct{}{})
}
