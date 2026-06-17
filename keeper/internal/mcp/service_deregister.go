package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serviceDeregisterArgs — arguments tool-а keeper.service.deregister
// (schemaServiceDeregisterInput): только name.
type serviceDeregisterArgs struct {
	Name string `json:"name"`
}

// callServiceDeregister — mutating-tool keeper.service.deregister. Транспорт
// поверх [serviceregistry.Service.DeleteService]: удаление по PK + invalidate-
// хук — в Service; tool маппит ErrNotFound в MCP-код и пишет audit
// service.deregistered.
//
// RBAC — service.deregister без селектора (rbac.md: NoSelector).
func (h *Handler) callServiceDeregister(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.service.deregister"

	if h.deps.ServiceSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, serviceRegistryNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно REST-handler-у: payload {name}.
	h.writeAudit(audit.EventServiceDeregistered, claims.Subject, map[string]any{
		"name": a.Name,
	})

	// REST возвращает 204 No Content; MCP-эквивалент — пустой output-объект.
	return h.toolResult(req.ID, struct{}{})
}
