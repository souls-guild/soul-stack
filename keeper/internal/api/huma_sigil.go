package api

// Регистрация и spec-dump SIGIL-домена (plugins/sigils) на huma full-typed (ТИРАЖ-
// БАТЧ-2a по эталонам role, ADR-054 §Pattern). allow/revoke — WRITE+AUDIT (вариант B,
// huma-audit-middleware; event-типы plugin.allowed / plugin.revoked — permission-
// домен `plugin`, НЕ `sigil`). list — read-bare (БЕЗ audit). Доменные *Typed-функции
// (handlers/sigil.go) извлечены из (w,r); старый (w,r) — тонкая strict-оболочка (MCP
// sigil-tools зовут sigil.Service напрямую, мимо handler — извлечение не затрагивает).

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

// registerHumaSigilAllow монтирует POST /v1/plugins/sigils через huma (WRITE+AUDIT
// вариант B — event plugin.allowed). sigilH nil → no-op. Handler: claims →
// AllowTyped → audit-payload на huma-ctx → 201 typed output.
func registerHumaSigilAllow(humaAPI huma.API, sigilH *handlers.SigilHandler) {
	if sigilH == nil {
		return
	}
	huma.Register(humaAPI, sigilAllowOperation(), func(ctx context.Context, in *sigilAllowInput) (*sigilAllowOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilMissingClaims()
		}
		reply, err := sigilH.AllowTyped(ctx, claims, handlers.SigilAllowInput{
			Namespace: in.Body.Namespace,
			Name:      in.Body.Name,
			Ref:       in.Body.Ref,
		})
		if err != nil {
			return nil, sigilProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilAllowOutput{Status: 201, Body: newPluginSigilAllowReply(reply.View)}, nil
	})
}

// registerHumaSigilList монтирует GET /v1/plugins/sigils через huma (READ-bare, БЕЗ
// audit). sigilH nil → no-op. Handler: ListTyped → typed output. RBAC plugin.list —
// на группе.
func registerHumaSigilList(humaAPI huma.API, sigilH *handlers.SigilHandler) {
	if sigilH == nil {
		return
	}
	huma.Register(humaAPI, sigilListOperation(), func(ctx context.Context, _ *sigilListInput) (*sigilListOutput, error) {
		reply, err := sigilH.ListTyped(ctx)
		if err != nil {
			return nil, sigilProblem(err)
		}
		return &sigilListOutput{Body: newPluginSigilListReply(reply)}, nil
	})
}

// registerHumaSigilRevoke монтирует DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}
// через huma (WRITE+AUDIT вариант B — event plugin.revoked). sigilH nil → no-op.
// Handler: claims → RevokeTyped(тройка) → audit-payload → пустой 204-output.
func registerHumaSigilRevoke(humaAPI huma.API, sigilH *handlers.SigilHandler) {
	if sigilH == nil {
		return
	}
	huma.Register(humaAPI, sigilRevokeOperation(), func(ctx context.Context, in *sigilRevokeInput) (*sigilNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilMissingClaims()
		}
		reply, err := sigilH.RevokeTyped(ctx, claims, in.Namespace, in.Name, in.Ref)
		if err != nil {
			return nil, sigilProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilNoContentOutput{Status: 204}, nil
	})
}

// sigilMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func sigilMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// sigilProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func sigilProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSigilAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// sigil (allow/revoke) монтируется на СВОЕЙ chi-группе с собственным event-типом.
func newHumaSigilAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSigilSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma sigil-роутов
// как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-таргета
// тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же register-
// функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaSigilSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SigilSpecStub()
		registerHumaSigilAllow(api, stub)
		registerHumaSigilList(api, stub)
		registerHumaSigilRevoke(api, stub)
		return nil
	})
}
