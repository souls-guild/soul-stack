package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// roleView is the output projection of a role for keeper.role.list
// (schemaRoleListOutput). Mirrors [rbac.RoleView]: catalog fields plus
// expanded permissions and assigned Archons (AID). Permissions/Operators are
// non-nil slices (a role with no entries serializes as [], not null), for
// predictable JSON output.
type roleView struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Builtin      bool     `json:"builtin"`
	Permissions  []string `json:"permissions"`
	Operators    []string `json:"operators"`
	DefaultScope string   `json:"default_scope,omitempty"`
}

// roleListOutput is the output of keeper.role.list: an array of roles under
// the roles key.
type roleListOutput struct {
	Roles []roleView `json:"roles"`
}

// callRoleList is the read-tool keeper.role.list. Transport over
// [rbac.Service.ListRoles] (read-only, no tx). Reads are NOT audited.
//
// RBAC: role.list with no selector (nil-context). arguments is an empty
// object (schemaEmptyObject); strictUnmarshal rejects extra fields.
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

// nonNilStrings guarantees a non-nil slice ([] instead of null in JSON).
// RoleView fills an empty set with an empty slice, but we guard against nil
// at the boundary.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
