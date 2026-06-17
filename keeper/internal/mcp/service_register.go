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

// serviceRegistryNotConfigured — public-detail nil-guard-а service-tools.
// ServiceSvc — опц. поле HandlerDeps (production-wire-up передаёт тот же
// *serviceregistry.Service, что REST): при nil service-tools диспатчатся, но
// возвращают internal-error «не сконфигурировано» (симметрично RBACRoles-guard-у
// role-tools / SigilSvc-guard-у plugin-tools).
const serviceRegistryNotConfigured = "service registry is not configured"

// serviceView — output-проекция записи реестра для service-tools
// (schemaServiceView). 1:1 с REST serviceResponse / [serviceregistry.
// ServiceEntry]: name + git/ref/refresh + audit-метаданные. created_by_aid /
// updated_by_aid / refresh — опциональны (omitempty; nil = NULL в БД).
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

// toServiceView проецирует [serviceregistry.ServiceEntry] в serviceView (даты —
// RFC 3339). Общий хелпер register/update/list-tools.
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

// serviceRegisterArgs — arguments tool-а keeper.service.register
// (schemaServiceRegisterInput): name + git + ref обязательны, refresh опционален.
type serviceRegisterArgs struct {
	Name    string  `json:"name"`
	Git     string  `json:"git"`
	Ref     string  `json:"ref"`
	Refresh *string `json:"refresh"`
}

// callServiceRegister — mutating-tool keeper.service.register. Транспорт поверх
// [serviceregistry.Service.CreateService]: вся бизнес-валидация (формат name,
// непустые git/ref, формат refresh, UNIQUE/FK-границы) и invalidate-хук — в
// Service; tool декодирует input, проверяет permission, маппит sentinel-ы в
// MCP-коды и пишет audit service.registered.
//
// RBAC — service.register без селектора (rbac.md: NoSelector, как role.*).
func (h *Handler) callServiceRegister(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.service.register"

	if h.deps.ServiceSvc == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, serviceRegistryNotConfigured)
	}

	// RBAC ДО unmarshal/валидации (least-disclosure): неавторизованный оператор
	// не получает validation-feedback по телу. Контекст nil — право не зависит
	// от тела запроса.
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

	// Audit — параллельно REST-handler-у (ADR-028-паттерн): payload {name, git,
	// ref, created_by_aid}. git-URL не секрет.
	h.writeAudit(audit.EventServiceRegistered, callerAID, map[string]any{
		"name":           entry.Name,
		"git":            entry.Git,
		"ref":            entry.Ref,
		"created_by_aid": callerAID,
	})

	return h.toolResult(req.ID, toServiceView(entry))
}
