package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serviceUpdateArgs — arguments for keeper.service.update
// (schemaServiceUpdateInput): name + git + ref are required (replace
// semantics for mutable fields), refresh is optional. name is the record key
// and doesn't change.
type serviceUpdateArgs struct {
	Name    string  `json:"name"`
	Git     string  `json:"git"`
	Ref     string  `json:"ref"`
	Refresh *string `json:"refresh"`
}

// callServiceUpdate — mutating tool keeper.service.update. Transport over
// [serviceregistry.Service.UpdateService] (replace semantics for
// git/ref/refresh): validation and the invalidate hook live in Service; the
// tool maps sentinels to MCP codes and writes audit service.updated.
//
// RBAC — service.update without a selector (rbac.md: NoSelector).
func (h *Handler) callServiceUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.service.update"

	if h.deps.ServiceSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, serviceRegistryNotConfigured)
	}

	// RBAC BEFORE unmarshal/validation (least-disclosure): an unauthorized
	// operator gets no validation feedback about the body. Context nil — the
	// permission doesn't depend on the request body.
	if err := h.deps.RBAC.Check(claims.Subject, "service", "update", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission service.update")
	}

	var a serviceUpdateArgs
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
	entry, err := h.deps.ServiceSvc.UpdateService(ctx, serviceregistry.UpdateServiceInput{
		Name:      a.Name,
		Git:       a.Git,
		Ref:       a.Ref,
		Refresh:   a.Refresh,
		CallerAID: &callerAID,
	})
	if err != nil {
		code, detail := mapServiceRegistryErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: service.update failed",
				slog.String("name", a.Name),
				slog.String("by_aid", callerAID),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit — parallels the REST handler: payload {name, git, ref}. The git
	// URL isn't a secret.
	h.writeAudit(audit.EventServiceUpdated, callerAID, map[string]any{
		"name": entry.Name,
		"git":  entry.Git,
		"ref":  entry.Ref,
	})

	return h.toolResult(req.ID, toServiceView(entry))
}
