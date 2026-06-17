package api

// Регистрация и spec-dump SIGIL-KEY-домена (/v1/sigil/keys) на huma full-typed
// (ТИРАЖ-БАТЧ-2a по эталонам role, ADR-054 §Pattern). introduce/set-primary/retire —
// WRITE+AUDIT (вариант B, huma-audit-middleware; события sigil.key-introduced /
// sigil.key-primary-set / sigil.key-retired). list — read-bare (БЕЗ audit). Доменные
// *Typed-функции (handlers/sigil_key.go) извлечены из (w,r); старый (w,r) — тонкая
// strict-оболочка (MCP sigil-key-tools зовут sigil.KeyService напрямую, мимо handler).

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

// registerHumaSigilKeyIntroduce монтирует POST /v1/sigil/keys через huma (WRITE+AUDIT
// вариант B — event sigil.key-introduced). sigilKeyH nil → no-op. Handler: claims →
// IntroduceTyped → audit-payload на huma-ctx → 201 typed output. Приватник НЕ в ответе.
func registerHumaSigilKeyIntroduce(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeyIntroduceOperation(), func(ctx context.Context, in *sigilKeyIntroduceInput) (*sigilKeyIntroduceOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilKeyMissingClaims()
		}
		makePrimary := in.Body.MakePrimary != nil && *in.Body.MakePrimary
		reply, err := sigilKeyH.IntroduceTyped(ctx, claims, makePrimary)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		// Проекция доменного reply.View (SigilKeyIntroduceView, Status — plain string)
		// в native SigilKeyIntroduceReply (Status — native enum). Json-теги совпадают →
		// wire-байты идентичны легаси writeJSON (golden фиксирует).
		return &sigilKeyIntroduceOutput{Status: 201, Body: newSigilKeyIntroduceReply(reply.View)}, nil
	})
}

// registerHumaSigilKeyList монтирует GET /v1/sigil/keys через huma (READ-bare, БЕЗ
// audit). sigilKeyH nil → no-op. Handler: ListTyped → typed output. RBAC
// sigil.key-list — на группе.
func registerHumaSigilKeyList(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeyListOperation(), func(ctx context.Context, _ *sigilKeyListInput) (*sigilKeyListOutput, error) {
		reply, err := sigilKeyH.ListTyped(ctx)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		return &sigilKeyListOutput{Body: newSigilKeyListReply(reply)}, nil
	})
}

// registerHumaSigilKeySetPrimary монтирует POST /v1/sigil/keys/{key_id}/primary через
// huma (WRITE+AUDIT вариант B — event sigil.key-primary-set). sigilKeyH nil → no-op.
// Handler: claims → SetPrimaryTyped(key_id) → audit-payload → пустой 204-output.
func registerHumaSigilKeySetPrimary(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeySetPrimaryOperation(), func(ctx context.Context, in *sigilKeySetPrimaryInput) (*sigilKeyNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilKeyMissingClaims()
		}
		reply, err := sigilKeyH.SetPrimaryTyped(ctx, claims, in.KeyID)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilKeyNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSigilKeyRetire монтирует DELETE /v1/sigil/keys/{key_id} через huma
// (WRITE+AUDIT вариант B — event sigil.key-retired). sigilKeyH nil → no-op. Handler:
// claims → RetireTyped(key_id) → audit-payload → пустой 204-output.
func registerHumaSigilKeyRetire(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeyRetireOperation(), func(ctx context.Context, in *sigilKeyRetireInput) (*sigilKeyNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilKeyMissingClaims()
		}
		reply, err := sigilKeyH.RetireTyped(ctx, claims, in.KeyID)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilKeyNoContentOutput{Status: 204}, nil
	})
}

// sigilKeyMissingClaims — defensive-ответ при отсутствии claims (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func sigilKeyMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// sigilKeyProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func sigilKeyProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSigilKeyAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// sigil-key (introduce/set-primary/retire) монтируется на СВОЕЙ chi-группе с
// собственным event-типом.
func newHumaSigilKeyAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSigilKeySpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma sigil-key-
// роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-
// таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же
// register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaSigilKeySpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SigilKeySpecStub()
		registerHumaSigilKeyIntroduce(api, stub)
		registerHumaSigilKeyList(api, stub)
		registerHumaSigilKeySetPrimary(api, stub)
		registerHumaSigilKeyRetire(api, stub)
		return nil
	})
}
