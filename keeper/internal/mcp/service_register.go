package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serviceRegistryNotConfigured — public-detail nil-guard for service-tools.
// ServiceSvc is an optional HandlerDeps field (production wire-up passes the
// same *serviceregistry.Service as REST): when nil, service-tools still
// dispatch but return internal-error "not configured" (mirrors the
// RBACRoles-guard for role-tools / SigilSvc-guard for plugin-tools).
const serviceRegistryNotConfigured = "service registry is not configured"

// serviceView — output projection of a registry entry for service-tools
// (schemaServiceView). 1:1 with REST serviceResponse / [serviceregistry.
// ServiceEntry]: name + git/ref/refresh + audit metadata. created_by_aid /
// updated_by_aid / refresh are optional (omitempty; nil = NULL in the DB).
type serviceView struct {
	Name         string  `json:"name"`
	Git          string  `json:"git"`
	Ref          string  `json:"ref"`
	Refresh      *string `json:"refresh,omitempty"`
	CreatedByAID *string `json:"created_by_aid,omitempty"`
	UpdatedByAID *string `json:"updated_by_aid,omitempty"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// toServiceView projects a [serviceregistry.ServiceEntry] into a serviceView
// (dates are RFC 3339). Shared helper for register/update/list-tools.
func toServiceView(e *serviceregistry.ServiceEntry) serviceView {
	return serviceView{
		Name:         e.Name,
		Git:          e.Git,
		Ref:          e.Ref,
		Refresh:      e.Refresh,
		CreatedByAID: e.CreatedByAID,
		UpdatedByAID: e.UpdatedByAID,
		CreatedAt:    e.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    e.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// serviceRegisterArgs — arguments for keeper.service.register
// (schemaServiceRegisterInput): name + git + ref are required, refresh is optional.
type serviceRegisterArgs struct {
	Name    string  `json:"name"`
	Git     string  `json:"git"`
	Ref     string  `json:"ref"`
	Refresh *string `json:"refresh"`
}

// callServiceRegister — mutating tool keeper.service.register. Transport over
// [serviceregistry.Service.CreateService]: all business validation (name
// format, non-empty git/ref, refresh format, UNIQUE/FK constraints) and the
// invalidate hook live in Service; the tool decodes input, checks permission,
// maps sentinels to MCP codes, and writes audit service.registered.
//
// RBAC — service.register without a selector (rbac.md: NoSelector, like role.*).
func (h *Handler) callServiceRegister(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.service.register"

	if h.deps.ServiceSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, serviceRegistryNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "service", "register", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission service.register")
	}

	var a serviceRegisterArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	callerAID := claims.Subject
	entry, err := h.deps.ServiceSvc.CreateService(ctx, serviceregistry.CreateServiceInput{
		Name:      a.Name,
		Git:       a.Git,
		Ref:       a.Ref,
		Refresh:   a.Refresh,
		CallerAID: &callerAID,
	})
	if err != nil {
		code, detail := mapServiceRegistryErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: service.register failed",
				slog.String("name", a.Name),
				slog.String("by_aid", callerAID),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallels the REST handler (ADR-028 pattern): payload {name, git,
	// ref, created_by_aid}. The git URL isn't a secret.
	h.writeAudit(audit.EventServiceRegistered, callerAID, map[string]any{
		"name":           entry.Name,
		"git":            entry.Git,
		"ref":            entry.Ref,
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toServiceView(entry))
}
