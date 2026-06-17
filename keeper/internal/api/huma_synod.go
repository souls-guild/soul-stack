package api

// Регистрация и spec-dump SYNOD-домена (группы / membership / bundle) на huma
// full-typed (ТИРАЖ-БАТЧ-2d по эталонам role/operator/augur/herald, ADR-054
// §Pattern). synod create/update/delete + add/remove-operator + grant/revoke-role —
// WRITE+AUDIT (вариант B, huma-audit-middleware; события synod.created/.updated/
// .deleted/.operator-added/.operator-removed/.role-granted/.role-revoked); synod
// list — read (БЕЗ audit). Доменные *Typed-функции (handlers/synod.go) извлечены
// из (w,r); старый (w,r) — тонкая strict-оболочка (synod-MCP-tools зовут rbac.Service
// напрямую, мимо handler — извлечение не затрагивает).

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

// registerHumaSynodCreate монтирует POST /v1/synods через huma (WRITE+AUDIT
// вариант B — event synod.created). synodH nil → no-op. Handler: claims →
// CreateTyped → audit-payload на huma-ctx (SetHumaAuditPayload) → пустой 201-output.
func registerHumaSynodCreate(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodCreateOperation(), func(ctx context.Context, in *synodCreateInput) (*synodCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.CreateTyped(ctx, claims, handlers.SynodCreateInput{
			Name:        in.Body.Name,
			Description: in.Body.Description,
		})
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &synodCreateOutput{Status: 201}, nil
	})
}

// registerHumaSynodList монтирует GET /v1/synods через huma (READ, БЕЗ audit).
// synodH nil → no-op. Handler: ListTyped → typed envelope-output. RBAC synod.list —
// на группе.
func registerHumaSynodList(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodListOperation(), func(ctx context.Context, _ *synodListInput) (*synodListOutput, error) {
		reply, err := synodH.ListTyped(ctx)
		if err != nil {
			return nil, synodProblem(err)
		}
		return &synodListOutput{Body: newSynodListReply(reply)}, nil
	})
}

// registerHumaSynodUpdate монтирует PATCH /v1/synods/{name} через huma (WRITE+AUDIT
// вариант B — event synod.updated). synodH nil → no-op. Handler: claims →
// UpdateTyped (меняет description) → audit-payload → пустой 204-output.
func registerHumaSynodUpdate(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodUpdateOperation(), func(ctx context.Context, in *synodUpdateInput) (*synodNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.UpdateTyped(ctx, claims, in.Name, handlers.SynodUpdateInput{
			Description: in.Body.Description,
		})
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodDelete монтирует DELETE /v1/synods/{name} через huma (WRITE+AUDIT
// вариант B — event synod.deleted). synodH nil → no-op. Handler: DeleteTyped →
// audit-payload → пустой 204-output.
func registerHumaSynodDelete(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodDeleteOperation(), func(ctx context.Context, in *synodDeleteInput) (*synodNoContentOutput, error) {
		reply, err := synodH.DeleteTyped(ctx, in.Name)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{"name": reply.Name})
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodAddOperator монтирует POST /v1/synods/{name}/operators через huma
// (WRITE+AUDIT вариант B — event synod.operator-added). synodH nil → no-op.
// Handler: claims → AddOperatorTyped (валидация AID + привязка) → audit-payload →
// пустой 204-output.
func registerHumaSynodAddOperator(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodAddOperatorOperation(), func(ctx context.Context, in *synodAddOperatorInput) (*synodNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.AddOperatorTyped(ctx, claims, in.Name, in.Body.AID)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AddOperatorAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodRemoveOperator монтирует DELETE /v1/synods/{name}/operators/{aid}
// через huma (WRITE+AUDIT вариант B — event synod.operator-removed). synodH nil →
// no-op. Handler: RemoveOperatorTyped (валидация path-AID + снятие) → audit-payload
// → пустой 204-output.
func registerHumaSynodRemoveOperator(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodRemoveOperatorOperation(), func(ctx context.Context, in *synodRemoveOperatorInput) (*synodNoContentOutput, error) {
		reply, err := synodH.RemoveOperatorTyped(ctx, in.Name, in.AID)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.RemoveOperatorAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodGrantRole монтирует POST /v1/synods/{name}/roles через huma
// (WRITE+AUDIT вариант B — event synod.role-granted). synodH nil → no-op. Handler:
// claims → GrantRoleTyped (валидация role + добавление в bundle) → audit-payload →
// пустой 204-output.
func registerHumaSynodGrantRole(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodGrantRoleOperation(), func(ctx context.Context, in *synodGrantRoleInput) (*synodNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.GrantRoleTyped(ctx, claims, in.Name, in.Body.Role)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.GrantRoleAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodRevokeRole монтирует DELETE /v1/synods/{name}/roles/{role_name}
// через huma (WRITE+AUDIT вариант B — event synod.role-revoked). synodH nil → no-op.
// Handler: RevokeRoleTyped (снятие роли из bundle) → audit-payload → пустой 204-output.
func registerHumaSynodRevokeRole(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodRevokeRoleOperation(), func(ctx context.Context, in *synodRevokeRoleInput) (*synodNoContentOutput, error) {
		reply, err := synodH.RevokeRoleTyped(ctx, in.Name, in.Role)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.RevokeRoleAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// synodMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func synodMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// synodProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func synodProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSynodAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// synod (create/update/delete, add/remove-operator, grant/revoke-role) монтируется
// на СВОЕЙ chi-группе с собственным event-типом.
func newHumaSynodAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSynodSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma synod-роутов
// как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-таргета
// тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же register-
// функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaSynodSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SynodSpecStub()
		registerHumaSynodCreate(api, stub)
		registerHumaSynodList(api, stub)
		registerHumaSynodUpdate(api, stub)
		registerHumaSynodDelete(api, stub)
		registerHumaSynodAddOperator(api, stub)
		registerHumaSynodRemoveOperator(api, stub)
		registerHumaSynodGrantRole(api, stub)
		registerHumaSynodRevokeRole(api, stub)
		return nil
	})
}
