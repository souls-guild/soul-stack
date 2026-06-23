package api

// Регистрация и spec-dump PROVISIONING-POLICY-домена (runtime-политика способов
// СОЗДАНИЯ операторов, ADR-058 Часть B) на huma full-typed. GET — read (БЕЗ audit);
// PUT — WRITE+AUDIT (вариант B, huma-audit-middleware, event
// provisioning.policy_changed). Доменные *Typed-функции (handlers/provisioning.go)
// несут бизнес-логику; register-func проецирует их результат в native wire-DTO.

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaProvisioningPolicyGet монтирует GET /v1/provisioning-policy через huma
// (READ, БЕЗ audit). h nil → no-op. Handler: GetTyped → typed envelope-output. RBAC
// provisioning.read — на группе.
func registerHumaProvisioningPolicyGet(humaAPI huma.API, h *handlers.ProvisioningPolicyHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, provisioningPolicyGetOperation(), func(_ context.Context, _ *provisioningPolicyGetInput) (*provisioningPolicyGetOutput, error) {
		return &provisioningPolicyGetOutput{Body: newProvisioningPolicyReply(h.GetTyped())}, nil
	})
}

// registerHumaProvisioningPolicyPut монтирует PUT /v1/provisioning-policy через huma
// (WRITE+AUDIT вариант B — event provisioning.policy_changed). h nil → no-op. Handler:
// claims → PutTyped (валидация + SetSetting + invalidate) → audit-payload → 200 С ТЕЛОМ.
func registerHumaProvisioningPolicyPut(humaAPI huma.API, h *handlers.ProvisioningPolicyHandler) {
	if h == nil {
		return
	}
	huma.Register(humaAPI, provisioningPolicyPutOperation(), func(ctx context.Context, in *provisioningPolicyPutInput) (*provisioningPolicyPutOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, provisioningMissingClaims()
		}
		reply, err := h.PutTyped(ctx, claims, handlers.ProvisioningPolicyUpdateInput{
			AllowedMethods: in.Body.AllowedMethods,
		})
		if err != nil {
			return nil, provisioningProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &provisioningPolicyPutOutput{Status: 200, Body: newProvisioningPolicyReply(reply.Body)}, nil
	})
}

// provisioningMissingClaims — defensive-ответ при отсутствии claims (недостижим:
// RequireJWT кладёт claims до huma). parity serviceMissingClaims.
func provisioningMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// provisioningProblem доставляет ошибку *Typed-функции через huma как problem+json
// (parity serviceProblem). Не-problem → 500.
func provisioningProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaProvisioningAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaServiceAPI). PUT-роут
// монтируется на СВОЕЙ chi-группе с собственным event-типом.
func newHumaProvisioningAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaProvisioningSpecYAML собирает OpenAPI-фрагмент provisioning-policy-роутов как
// YAML-строку, БЕЗ монтирования на реальный router (хук спека-мерж-таргета +
// guard-теста; parity HumaServiceSpecYAML).
func HumaProvisioningSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ProvisioningPolicySpecStub()
		registerHumaProvisioningPolicyGet(api, stub)
		registerHumaProvisioningPolicyPut(api, stub)
		return nil
	})
}
