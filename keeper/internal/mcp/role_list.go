package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// roleView — output-проекция роли для keeper.role.list (schemaRoleListOutput).
// Симметрична [rbac.RoleView]: каталожные поля + развёрнутые permissions и
// назначенные Архонты (AID). Permissions/Operators — non-nil слайсы (роль без
// записей сериализуется как [], не null), для предсказуемого JSON-вывода.
type roleView struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Builtin      bool     `json:"builtin"`
	Permissions  []string `json:"permissions"`
	Operators    []string `json:"operators"`
	DefaultScope string   `json:"default_scope,omitempty"`
}

// roleListOutput — output keeper.role.list: массив ролей под ключом roles.
type roleListOutput struct {
	Roles []roleView `json:"roles"`
}

// callRoleList — read-tool keeper.role.list. Транспорт поверх
// [rbac.Service.ListRoles] (read-only, без tx). reads НЕ аудируются.
//
// RBAC — role.list без селектора (nil-context). arguments — пустой объект
// (schemaEmptyObject); strictUnmarshal отвергает лишние поля.
func (h *Handler) callRoleList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.list"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	if len(args) > 0 {
		var empty struct{}
		if err := strictUnmarshal(args, &empty); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	if err := h.deps.RBAC.Check(claims.Subject, "role", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission role.list")
	}

	views, err := h.deps.RBACRoles.ListRoles(ctx)
	if err != nil {
		code, detail := mapRoleErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: role.list failed",
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	out := roleListOutput{Roles: make([]roleView, 0, len(views))}
	for _, v := range views {
		out.Roles = append(out.Roles, roleView{
			Name:         v.Name,
			Description:  v.Description,
			Builtin:      v.Builtin,
			Permissions:  nonNilStrings(v.Permissions),
			Operators:    nonNilStrings(v.Operators),
			DefaultScope: v.DefaultScope,
		})
	}
	return h.toolResult(req.ID, out)
}

// nonNilStrings гарантирует non-nil слайс ([] вместо null в JSON). RoleView
// заполняет пустой набор пустым слайсом, но защищаемся от nil на границе.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
