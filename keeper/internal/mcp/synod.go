// Synod-MCP-tools (ADR-049): keeper.synod.<action> ↔ permission synod.<action>
// (1:1, Variant A — role-tools pattern). Business logic (builtin boundary,
// self-lockout, least-privilege subset) lives in [rbac.Service]; the tool is
// transport: RBAC-Check BEFORE unmarshal (least-disclosure), decode args, map
// sentinels to MCP codes, audit mirroring the HTTP handler. All tools
// dispatch only when RBACRoles is non-nil (same *rbac.Service as role-tools).
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

// synodManagementNotConfigured — public detail of the synod-tools nil-guard
// (RBACRoles nil: a build without the RBAC-CRUD facade is valid, same
// pattern as roleManagementNotConfigured).
const synodManagementNotConfigured = "synod management is not configured"

// synodCreateArgs — keeper.synod.create (schemaSynodCreateInput): name is
// required, description is optional.
type synodCreateArgs struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Handler) callSynodCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.create"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.create")
	}
	var a synodCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if len(a.Description) > rbac.SynodDescriptionMaxLen {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'description' exceeds max length")
	}
	err := h.deps.RBACRoles.CreateSynod(ctx, rbac.CreateSynodInput{
		Name:        a.Name,
		Description: a.Description,
		CallerAID:   claims.Subject,
	})
	if err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.create failed",
				slog.String("name", a.Name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	h.writeAudit(audit.EventSynodCreated, claims.Subject, map[string]any{
		"name": a.Name, "created_by_aid": claims.Subject,
	})
	return h.toolResult(req.ID, struct{}{})
}

// synodDeleteArgs — keeper.synod.delete: name only.
type synodDeleteArgs struct {
	Name string `json:"name"`
}

func (h *Handler) callSynodDelete(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.delete"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "delete", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.delete")
	}
	var a synodDeleteArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if err := h.deps.RBACRoles.DeleteSynod(ctx, a.Name); err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.delete failed",
				slog.String("name", a.Name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	h.writeAudit(audit.EventSynodDeleted, claims.Subject, map[string]any{"name": a.Name})
	return h.toolResult(req.ID, struct{}{})
}

// synodUpdateArgs — keeper.synod.update (schemaSynodUpdateInput): name +
// description are required. ONLY description changes; name (PK) is
// immutable (ADR-049 amend).
type synodUpdateArgs struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// callSynodUpdate — mutating-tool keeper.synod.update. Transport over
// [rbac.Service.UpdateSynodDescription]: builtin is ALLOWED, no subset/self-
// lockout check (description grants no permissions). RBAC BEFORE unmarshal
// (least-disclosure).
func (h *Handler) callSynodUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.update"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "update", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.update")
	}
	var a synodUpdateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}
	if a.Description == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'description' is required")
	}
	if len(a.Description) > rbac.SynodDescriptionMaxLen {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'description' exceeds max length")
	}
	if err := h.deps.RBACRoles.UpdateSynodDescription(ctx, a.Name, a.Description); err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.update failed",
				slog.String("name", a.Name), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	h.writeAudit(audit.EventSynodUpdated, claims.Subject, map[string]any{
		"name": a.Name, "description": a.Description,
	})
	return h.toolResult(req.ID, struct{}{})
}

// synodView — output projection of a group for keeper.synod.list (schemaSynodListOutput).
type synodView struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Builtin     bool     `json:"builtin"`
	Roles       []string `json:"roles"`
	Operators   []string `json:"operators"`
}

type synodListOutput struct {
	Synods []synodView `json:"synods"`
}

func (h *Handler) callSynodList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.list"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if len(args) > 0 {
		var empty struct{}
		if err := strictUnmarshal(args, &empty); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.list")
	}
	views, err := h.deps.RBACRoles.ListSynods(ctx)
	if err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.list failed",
				slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	out := synodListOutput{Synods: make([]synodView, 0, len(views))}
	for _, v := range views {
		out.Synods = append(out.Synods, synodView{
			Name:        v.Name,
			Description: v.Description,
			Builtin:     v.Builtin,
			Roles:       nonNilStrings(v.Roles),
			Operators:   nonNilStrings(v.Operators),
		})
	}
	return h.toolResult(req.ID, out)
}

// synodAddOperatorArgs — keeper.synod.add-operator: synod + aid are required.
type synodAddOperatorArgs struct {
	Synod string `json:"synod"`
	AID   string `json:"aid"`
}

func (h *Handler) callSynodAddOperator(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.add-operator"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "add-operator", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.add-operator")
	}
	var a synodAddOperatorArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Synod == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'synod' is required")
	}
	if a.AID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' is required")
	}
	if !operator.ValidAID(a.AID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' must match "+operator.AIDPattern)
	}
	err := h.deps.RBACRoles.AddOperator(ctx, rbac.AddOperatorInput{
		SynodName: a.Synod, AID: a.AID, CallerAID: claims.Subject,
	})
	if err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.add-operator failed",
				slog.String("synod", a.Synod), slog.String("aid", a.AID),
				slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	h.writeAudit(audit.EventSynodOperatorAdded, claims.Subject, map[string]any{
		"name": a.Synod, "aid": a.AID, "added_by_aid": claims.Subject,
	})
	return h.toolResult(req.ID, struct{}{})
}

// synodRemoveOperatorArgs — keeper.synod.remove-operator: synod + aid are required.
type synodRemoveOperatorArgs struct {
	Synod string `json:"synod"`
	AID   string `json:"aid"`
}

func (h *Handler) callSynodRemoveOperator(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.remove-operator"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "remove-operator", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.remove-operator")
	}
	var a synodRemoveOperatorArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Synod == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'synod' is required")
	}
	if a.AID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' is required")
	}
	if !operator.ValidAID(a.AID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' must match "+operator.AIDPattern)
	}
	err := h.deps.RBACRoles.RemoveOperator(ctx, rbac.RemoveOperatorInput{
		SynodName: a.Synod, AID: a.AID,
	})
	if err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.remove-operator failed",
				slog.String("synod", a.Synod), slog.String("aid", a.AID),
				slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	h.writeAudit(audit.EventSynodOperatorRemoved, claims.Subject, map[string]any{
		"name": a.Synod, "aid": a.AID,
	})
	return h.toolResult(req.ID, struct{}{})
}

// synodGrantRoleArgs — keeper.synod.grant-role: synod + role are required.
type synodGrantRoleArgs struct {
	Synod string `json:"synod"`
	Role  string `json:"role"`
}

func (h *Handler) callSynodGrantRole(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.grant-role"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "grant-role", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.grant-role")
	}
	var a synodGrantRoleArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Synod == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'synod' is required")
	}
	if a.Role == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'role' is required")
	}
	err := h.deps.RBACRoles.GrantRole(ctx, rbac.GrantRoleInput{
		SynodName: a.Synod, RoleName: a.Role, CallerAID: claims.Subject,
	})
	if err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.grant-role failed",
				slog.String("synod", a.Synod), slog.String("role", a.Role),
				slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	h.writeAudit(audit.EventSynodRoleGranted, claims.Subject, map[string]any{
		"name": a.Synod, "role": a.Role, "granted_by_aid": claims.Subject,
	})
	return h.toolResult(req.ID, struct{}{})
}

// synodRevokeRoleArgs — keeper.synod.revoke-role: synod + role are required.
type synodRevokeRoleArgs struct {
	Synod string `json:"synod"`
	Role  string `json:"role"`
}

func (h *Handler) callSynodRevokeRole(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.synod.revoke-role"
	if h.deps.RBACRoles == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, synodManagementNotConfigured)
	}
	if err := h.deps.RBAC.Check(claims.Subject, "synod", "revoke-role", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission synod.revoke-role")
	}
	var a synodRevokeRoleArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.Synod == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'synod' is required")
	}
	if a.Role == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'role' is required")
	}
	err := h.deps.RBACRoles.RevokeRole(ctx, rbac.RevokeRoleInput{
		SynodName: a.Synod, RoleName: a.Role,
	})
	if err != nil {
		code, detail := mapSynodErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: synod.revoke-role failed",
				slog.String("synod", a.Synod), slog.String("role", a.Role),
				slog.String("by_aid", claims.Subject), slog.Any("error", err))
		}
		return h.toolError(req.ID, toolName, code, detail)
	}
	h.writeAudit(audit.EventSynodRoleRevoked, claims.Subject, map[string]any{
		"name": a.Synod, "role": a.Role,
	})
	return h.toolResult(req.ID, struct{}{})
}
