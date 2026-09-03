package mcp

import (
	"context"
	"encoding/json"
	"errors"
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
// ServiceEntry]: id + git/ref/refresh + audit metadata. created_by_aid /
// updated_by_aid / refresh are optional (omitempty; nil = NULL in the DB).
type serviceView struct {
	ID string `json:"id"`
	// Label — display caption (ADR-0085); absent → a consumer shows `id`.
	// NOT segment 2 of a derived secret path; `id` is.
	Label        *string `json:"label,omitempty"`
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
		ID:           e.ID,
		Label:        e.Label,
		Git:          e.Git,
		Ref:          e.Ref,
		Refresh:      e.Refresh,
		CreatedByAID: e.CreatedByAID,
		UpdatedByAID: e.UpdatedByAID,
		CreatedAt:    e.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    e.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// callServiceSetLabel — keeper.service.label-set, the MCP mirror of
// PUT /v1/services/{id}/label (ADR-0085). Narrower than keeper.service.update,
// which re-points git/ref and invalidates every artifact cache.
func (h *Handler) callServiceSetLabel(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	return callLabelSet(h, ctx, claims, req, args, labelSetSpec[serviceView]{
		tool:          "keeper.service.label-set",
		resource:      "service",
		configured:    h.deps.ServiceSvc != nil,
		notConfigured: serviceRegistryNotConfigured,
		validID:       serviceregistry.ValidID,
		idPattern:     serviceregistry.IDPattern,
		set: func(ctx context.Context, name string, label *string) (serviceView, *string, error) {
			entry, previous, err := h.deps.ServiceSvc.SetServiceLabel(ctx, name, label)
			if err != nil {
				return serviceView{}, nil, err
			}
			return toServiceView(entry), previous, nil
		},
		isNotFound: func(err error) bool { return errors.Is(err, serviceregistry.ErrNotFound) },
		notFoundf:  func(name string) string { return "service " + name + " not found" },
		failMsg:    "set service label failed",
		event:      audit.EventServiceLabelChanged,
	})
}

// serviceRegisterArgs — arguments for keeper.service.register
// (schemaServiceRegisterInput): id + git + ref are required, refresh is optional.
type serviceRegisterArgs struct {
	ID string `json:"id"`
	// Label — optional display caption (ADR-0085), free text; changed afterwards
	// by keeper.service.label-set.
	Label   *string `json:"label"`
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
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}

	callerAID := claims.Subject
	entry, err := h.deps.ServiceSvc.CreateService(ctx, serviceregistry.CreateServiceInput{
		ID:        a.ID,
		Label:     a.Label,
		Git:       a.Git,
		Ref:       a.Ref,
		Refresh:   a.Refresh,
		CallerAID: &callerAID,
	})
	if err != nil {
		code, detail := mapServiceRegistryErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: service.register failed",
				slog.String("id", a.ID),
				slog.String("by_aid", callerAID),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallels the REST handler (ADR-028 pattern): payload {id, git,
	// ref, created_by_aid}. The git URL is not a secret.
	h.writeAudit(audit.EventServiceRegistered, callerAID, map[string]any{
		"id":             entry.ID,
		"label":          entry.Label,
		"git":            entry.Git,
		"ref":            entry.Ref,
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toServiceView(entry))
}
