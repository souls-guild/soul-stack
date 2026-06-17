package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// serviceListOutput — output keeper.service.list: реестр Service-ов под ключом
// services (паритет REST GET /v1/services items).
type serviceListOutput struct {
	Services []serviceView `json:"services"`
}

// callServiceList — read-tool keeper.service.list. Транспорт поверх
// [serviceregistry.Service.ListServices] (read-only). reads НЕ аудируются.
//
// RBAC — service.list без селектора (rbac.md: NoSelector). arguments — пустой
// объект (schemaEmptyObject); strictUnmarshal отвергает лишние поля.
func (h *Handler) callServiceList(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.service.list"

	if h.deps.ServiceSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, serviceRegistryNotConfigured)
	}

	if len(args) > 0 {
		var empty struct{}
		if err := strictUnmarshal(args, &empty); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}

	if err := h.deps.RBAC.Check(claims.Subject, "service", "list", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission service.list")
	}

	entries, err := h.deps.ServiceSvc.ListServices(ctx)
	if err != nil {
		h.deps.Logger.Error("mcp: service.list failed",
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "internal error")
	}

	out := serviceListOutput{Services: make([]serviceView, 0, len(entries))}
	for _, e := range entries {
		out.Services = append(out.Services, toServiceView(e))
	}
	return h.toolResult(req.ID, out)
}
