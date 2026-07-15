package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// roleManagementNotConfigured — public-detail of the nil-guard for
// role-tools. RBACRoles is an optional HandlerDeps field (Slice 1.5 wires it,
// but a build without the RBAC-CRUD facade is still valid): when nil,
// role-tools dispatch but return internal-error "not configured" (symmetric
// with the incarnation deps-guards ScenarioRunner/ServiceRegistry).
const roleManagementNotConfigured = "role management is not configured"

// roleCreateArgs — arguments for the keeper.role.create tool
// (schemaRoleCreateInput): name + permissions are required, description is
// optional. permissions are '<resource>.<action>' permission strings
// (validated in rbac.Service).
type roleCreateArgs struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Permissions  []string `json:"permissions"`
	DefaultScope *string  `json:"default_scope,omitempty"`
}

// callRoleCreate — mutating-tool keeper.role.create. A transport layer over
// [rbac.Service.CreateRole]: all business validation (name format,
// ParsePermission, the UNIQUE constraint) lives in Service; the tool decodes
// input, checks permission, and maps sentinels to MCP codes.
//
// RBAC — role.create without a selector (role-permissions have no selectors;
// the closed enum service/coven/incarnation/host doesn't cover them →
// nil-context).
func (h *Handler) callRoleCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.create"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context is nil —
	// the permission doesn't depend on the request body.
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

	// Audit — mirrors the HTTP handler (authorization change, ADR-022):
	// payload {name, permissions, created_by_aid}. permission strings aren't secret.
	h.writeAudit(audit.EventRoleCreated, claims.Subject, map[string]any{
		"name":           a.Name,
		"permissions":    a.Permissions,
		"created_by_aid": claims.Subject,
	})

	return h.toolResult(req.ID, struct{}{})
}
