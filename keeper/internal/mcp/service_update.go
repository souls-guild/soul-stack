package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serviceUpdateArgs — arguments tool-а keeper.service.update
// (schemaServiceUpdateInput): name + git + ref обязательны (replace-семантика
// mutable-полей), refresh опционален. name — ключ записи, не меняется.
type serviceUpdateArgs struct {
	Name    string  `json:"name"`
	Git     string  `json:"git"`
	Ref     string  `json:"ref"`
	Refresh *string `json:"refresh"`
}

// callServiceUpdate — mutating-tool keeper.service.update. Транспорт поверх
// [serviceregistry.Service.UpdateService] (replace-семантика git/ref/refresh):
// валидация и invalidate-хук — в Service; tool маппит sentinel-ы в MCP-коды и
// пишет audit service.updated.
//
// RBAC — service.update без селектора (rbac.md: NoSelector).
func (h *Handler) callServiceUpdate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.service.update"

	if h.deps.ServiceSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, serviceRegistryNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно REST-handler-у: payload {name, git, ref}. git-URL не
	// секрет.
	h.writeAudit(audit.EventServiceUpdated, callerAID, map[string]any{
		"name": entry.Name,
		"git":  entry.Git,
		"ref":  entry.Ref,
	})

	return h.toolResult(req.ID, toServiceView(entry))
}
