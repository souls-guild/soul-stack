package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// roleUpdateArgs — arguments for keeper.role.update (schemaRoleUpdateInput):
// name + permissions are required. permissions is the new set (replace
// semantics). default_scope (ADR-047 S1) is optional: key ABSENT → scope
// untouched; present (including null) → replaces it (null clears scope).
// parent_role (ADR-078) follows the same presence rule — null makes the role
// plain again. REST parity: PATCH /v1/roles/{name}/permissions takes both.
type roleUpdateArgs struct {
	Name         string   `json:"name"`
	Permissions  []string `json:"permissions"`
	DefaultScope *string  `json:"default_scope"`
	ParentRole   *string  `json:"parent_role"`
}

// callRoleUpdate — mutating tool keeper.role.update. Transport over
// [rbac.Service.UpdateRolePermissions] (replace semantics): the builtin
// boundary, permissions validation, and self-lockout (removing the last `*`)
// live in Service; the tool maps sentinels to MCP codes.
//
// RBAC — role.update without a selector (nil-context).
func (h *Handler) callRoleUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.role.update"

	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, roleManagementNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
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

	// presence of the default_scope / parent_role keys in raw args: omitted (leave
	// as is) vs explicit (replace, including null → clear). *string alone can't
	// distinguish these.
	hasScope := rawArgHasKey(args, "default_scope")
	hasParent := rawArgHasKey(args, "parent_role")

	err := h.deps.RBACRoles.UpdateRolePermissions(ctx, rbac.UpdateRolePermissionsInput{
		Name:            a.Name,
		Permissions:     a.Permissions,
		CallerAID:       claims.Subject,
		SetDefaultScope: hasScope,
		DefaultScope:    a.DefaultScope,
		SetParentRole:   hasParent,
		ParentRole:      a.ParentRole,
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

	// Audit — parallels the HTTP handler (authorization change, ADR-022): payload
	// {name, permissions} (new set), plus parent_role/default_scope ONLY when the
	// call carried them. An absent key means "untouched", a present null means
	// "cleared", and the two are different authorization changes (ADR-078).
	// Permission strings aren't secret.
	payload := map[string]any{
		"name":        a.Name,
		"permissions": a.Permissions,
	}
	if hasParent {
		payload["parent_role"] = a.ParentRole
	}
	if hasScope {
		payload["default_scope"] = a.DefaultScope
	}
	h.writeAudit(audit.EventRolePermissionsUpdated, claims.Subject, payload)

	return h.toolResult(req.ID, struct{}{})
}
