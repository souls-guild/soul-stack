package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serviceDeregisterArgs — arguments for keeper.service.deregister
// (schemaServiceDeregisterInput): name only.
type serviceDeregisterArgs struct {
	Name string `json:"name"`
}

// callServiceDeregister — mutating tool keeper.service.deregister. Transport
// over [serviceregistry.Service.DeleteService]: delete by PK + the
// invalidate hook live in Service; the tool maps ErrNotFound to an MCP code
// and writes audit service.deregistered.
//
// RBAC — service.deregister without a selector (rbac.md: NoSelector).
func (h *Handler) callServiceDeregister(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.service.deregister"

	if h.deps.ServiceSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, serviceRegistryNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "service", "deregister", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission service.deregister")
	}

	var a serviceDeregisterArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.Name == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'name' is required")
	}

	if err := h.deps.ServiceSvc.DeleteService(ctx, a.Name); err != nil {
		code, detail := mapServiceRegistryErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: service.deregister failed",
				slog.String("name", a.Name),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallels the REST handler: payload {name}.
	h.writeAudit(audit.EventServiceDeregistered, claims.Subject, map[string]any{
		"name": a.Name,
	})

	// REST returns 204 No Content; the MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}
