package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// roleManagementNotConfigured — public-detail nil-guard-а role-tools.
// RBACRoles — опц. поле HandlerDeps (Slice 1.5 прокидывает его, но сборка
// без RBAC-CRUD-фасада допустима): при nil role-tools диспатчатся, но
// возвращают internal-error «не сконфигурировано» (симметрично incarnation-
// deps-guard-ам ScenarioRunner/ServiceRegistry).
const roleManagementNotConfigured = "role management is not configured"

// roleCreateArgs — arguments tool-а keeper.role.create (schemaRoleCreateInput):
// name + permissions обязательны, description опционален. permissions —
// permission-строки '<resource>.<action>' (валидируются в rbac.Service).
type roleCreateArgs struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Permissions  []string `json:"permissions"`
	DefaultScope *string  `json:"default_scope,omitempty"`
}

// callRoleCreate — mutating-tool keeper.role.create. Транспорт поверх
// [rbac.Service.CreateRole]: вся бизнес-валидация (формат name, ParsePermission,
// UNIQUE-граница) — в Service; tool декодирует input, проверяет permission,
// маппит sentinel-ы в MCP-коды.
//
// RBAC — role.create без селектора (role-permissions селекторов не имеют,
// closed enum service/coven/incarnation/host их не покрывает → nil-context).
func (h *Handler) callRoleCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.create"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
	if err := h.deps.RBAC.Check(claims.Subject, "role", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission role.create")
	}

	var a roleCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	err := h.deps.RBACRoles.CreateRole(ctx, rbac.CreateRoleInput{
		Name:         a.Name,
		Description:  a.Description,
		Permissions:  a.Permissions,
		CallerAID:    claims.Subject,
		DefaultScope: a.DefaultScope,
	})
	if err != nil {
		code, detail := mapRoleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: role.create failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — параллельно HTTP-handler-у (изменение авторизации, ADR-022):
	// payload {name, permissions, created_by_aid}. permission-строки не секрет.
	h.writeAudit(audit.EventRoleCreated, claims.Subject, map[string]any{
		"name":           a.Name,
		"permissions":    a.Permissions,
		"created_by_aid": claims.Subject,
	})

	return h.toolResult(req.ID, struct{}{})
}
