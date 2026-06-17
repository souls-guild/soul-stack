package api

// Регистрация и spec-dump HERALD-домена (heralds + tidings) на huma full-typed
// (handler-native T5d-2c по эталонам role/operator/augur/push-provider, ADR-054
// §Pattern). ОДИН [handlers.HeraldHandler] обслуживает ОБА ресурса. herald create/
// update/delete + tiding create/update/delete — WRITE+AUDIT (вариант B, huma-audit-
// middleware; события herald.created/.updated/.deleted и tiding.created/.updated/
// .deleted); herald/tiding list/get — read (БЕЗ audit). Доменные *Typed-функции
// (handlers/herald.go) принимают NATIVE request-типы и возвращают доменные result-ы
// с плоскими wire-полями; register-func проецирует их в native wire-DTO
// (huma_herald_reply.go) НАПРЯМУЮ — legacy-генерата не участвует. MCP herald-tools зовут
// herald.Service напрямую (мимо handler).

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

// --- Herald ----------------------------------------------------------

// registerHumaHeraldCreate монтирует POST /v1/heralds через huma (WRITE+AUDIT
// вариант B — event herald.created). heraldH nil → no-op. Handler: claims →
// CreateHeraldTyped → audit-payload на huma-ctx (SetHumaAuditPayload) → 201 typed output.
func registerHumaHeraldCreate(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, heraldCreateOperation(), func(ctx context.Context, in *heraldCreateInput) (*heraldCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, heraldMissingClaims()
		}
		reply, err := heraldH.CreateHeraldTyped(ctx, claims, handlers.HeraldCreateInput{
			Name:      in.Body.Name,
			Type:      in.Body.Type,
			Config:    in.Body.Config,
			SecretRef: in.Body.SecretRef,
			Enabled:   in.Body.Enabled,
		})
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &heraldCreateOutput{Status: 201, Body: newHerald(reply.View)}, nil
	})
}

// registerHumaHeraldList монтирует GET /v1/heralds через huma (READ-with-typed-query,
// БЕЗ audit). heraldH nil → no-op. Handler: typed-query → ListHeraldsTyped → typed
// envelope-output. RBAC herald.list — на группе.
func registerHumaHeraldList(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, heraldListOperation(), func(ctx context.Context, in *heraldListInput) (*heraldListOutput, error) {
		reply, err := heraldH.ListHeraldsTyped(ctx, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, heraldProblem(err)
		}
		return &heraldListOutput{Body: newHeraldListReply(reply)}, nil
	})
}

// registerHumaHeraldGet монтирует GET /v1/heralds/{name} через huma (READ-with-path,
// БЕЗ audit). heraldH nil → no-op. Handler: GetHeraldTyped(name) → typed output
// (404/422 через problem). RBAC herald.read — на группе.
func registerHumaHeraldGet(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, heraldGetOperation(), func(ctx context.Context, in *heraldGetInput) (*heraldGetOutput, error) {
		reply, err := heraldH.GetHeraldTyped(ctx, in.Name)
		if err != nil {
			return nil, heraldProblem(err)
		}
		return &heraldGetOutput{Body: newHerald(reply)}, nil
	})
}

// registerHumaHeraldUpdate монтирует PUT /v1/heralds/{name} через huma (WRITE+AUDIT
// вариант B — event herald.updated). heraldH nil → no-op. Handler: UpdateHeraldTyped
// (replace) → audit-payload → 200 С ТЕЛОМ.
func registerHumaHeraldUpdate(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, heraldUpdateOperation(), func(ctx context.Context, in *heraldUpdateInput) (*heraldUpdateOutput, error) {
		reply, err := heraldH.UpdateHeraldTyped(ctx, in.Name, handlers.HeraldUpdateInput{
			Type:      in.Body.Type,
			Config:    in.Body.Config,
			SecretRef: in.Body.SecretRef,
			Enabled:   in.Body.Enabled,
		})
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &heraldUpdateOutput{Status: 200, Body: newHerald(reply.View)}, nil
	})
}

// registerHumaHeraldDelete монтирует DELETE /v1/heralds/{name} через huma (WRITE+AUDIT
// вариант B — event herald.deleted). heraldH nil → no-op. Handler: DeleteHeraldTyped →
// audit-payload → пустой 204-output.
func registerHumaHeraldDelete(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, heraldDeleteOperation(), func(ctx context.Context, in *heraldDeleteInput) (*heraldNoContentOutput, error) {
		reply, err := heraldH.DeleteHeraldTyped(ctx, in.Name)
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &heraldNoContentOutput{Status: 204}, nil
	})
}

// --- Tiding ----------------------------------------------------------

// registerHumaTidingCreate монтирует POST /v1/tidings через huma (WRITE+AUDIT вариант B
// — event tiding.created). heraldH nil → no-op. Handler: claims → CreateTidingTyped →
// audit-payload → 201 typed output.
func registerHumaTidingCreate(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, tidingCreateOperation(), func(ctx context.Context, in *tidingCreateInput) (*tidingCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, heraldMissingClaims()
		}
		reply, err := heraldH.CreateTidingTyped(ctx, claims, handlers.TidingCreateInput{
			Name:         in.Body.Name,
			Herald:       in.Body.Herald,
			EventTypes:   in.Body.EventTypes,
			OnlyFailures: in.Body.OnlyFailures,
			OnlyChanges:  in.Body.OnlyChanges,
			Incarnation:  in.Body.Incarnation,
			Cadence:      in.Body.Cadence,
			Task:         in.Body.Task,
			Annotations:  in.Body.Annotations,
			Projection:   in.Body.Projection,
			Enabled:      in.Body.Enabled,
		})
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &tidingCreateOutput{Status: 201, Body: newTiding(reply.View)}, nil
	})
}

// registerHumaTidingList монтирует GET /v1/tidings через huma (READ-with-typed-query,
// БЕЗ audit). heraldH nil → no-op. Handler: typed-query (offset/limit/include_ephemeral)
// → ListTidingsTyped → typed envelope-output. RBAC tiding.list — на группе.
func registerHumaTidingList(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, tidingListOperation(), func(ctx context.Context, in *tidingListInput) (*tidingListOutput, error) {
		reply, err := heraldH.ListTidingsTyped(ctx, in.IncludeEphemeral, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, heraldProblem(err)
		}
		return &tidingListOutput{Body: newTidingListReply(reply)}, nil
	})
}

// registerHumaTidingGet монтирует GET /v1/tidings/{name} через huma (READ-with-path,
// БЕЗ audit). heraldH nil → no-op. Handler: GetTidingTyped(name) → typed output
// (404/422 через problem). RBAC tiding.read — на группе.
func registerHumaTidingGet(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, tidingGetOperation(), func(ctx context.Context, in *tidingGetInput) (*tidingGetOutput, error) {
		reply, err := heraldH.GetTidingTyped(ctx, in.Name)
		if err != nil {
			return nil, heraldProblem(err)
		}
		return &tidingGetOutput{Body: newTiding(reply)}, nil
	})
}

// registerHumaTidingUpdate монтирует PUT /v1/tidings/{name} через huma (WRITE+AUDIT
// вариант B — event tiding.updated). heraldH nil → no-op. Handler: UpdateTidingTyped
// (replace) → audit-payload → 200 С ТЕЛОМ.
func registerHumaTidingUpdate(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, tidingUpdateOperation(), func(ctx context.Context, in *tidingUpdateInput) (*tidingUpdateOutput, error) {
		reply, err := heraldH.UpdateTidingTyped(ctx, in.Name, handlers.TidingUpdateInput{
			Herald:       in.Body.Herald,
			EventTypes:   in.Body.EventTypes,
			OnlyFailures: in.Body.OnlyFailures,
			OnlyChanges:  in.Body.OnlyChanges,
			Incarnation:  in.Body.Incarnation,
			Cadence:      in.Body.Cadence,
			Task:         in.Body.Task,
			Annotations:  in.Body.Annotations,
			Projection:   in.Body.Projection,
			Enabled:      in.Body.Enabled,
		})
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &tidingUpdateOutput{Status: 200, Body: newTiding(reply.View)}, nil
	})
}

// registerHumaTidingDelete монтирует DELETE /v1/tidings/{name} через huma (WRITE+AUDIT
// вариант B — event tiding.deleted). heraldH nil → no-op. Handler: DeleteTidingTyped →
// audit-payload → пустой 204-output.
func registerHumaTidingDelete(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, tidingDeleteOperation(), func(ctx context.Context, in *tidingDeleteInput) (*heraldNoContentOutput, error) {
		reply, err := heraldH.DeleteTidingTyped(ctx, in.Name)
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &heraldNoContentOutput{Status: 204}, nil
	})
}

// heraldMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func heraldMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// heraldProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func heraldProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaHeraldAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// herald/tiding (create/update/delete) монтируется на СВОЕЙ chi-группе с собственным
// event-типом.
func newHumaHeraldAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaHeraldSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma herald-/
// tiding-роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для
// спека-мерж-таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те
// же register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaHeraldSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.HeraldSpecStub()
		registerHumaHeraldCreate(api, stub)
		registerHumaHeraldList(api, stub)
		registerHumaHeraldGet(api, stub)
		registerHumaHeraldUpdate(api, stub)
		registerHumaHeraldDelete(api, stub)
		registerHumaTidingCreate(api, stub)
		registerHumaTidingList(api, stub)
		registerHumaTidingGet(api, stub)
		registerHumaTidingUpdate(api, stub)
		registerHumaTidingDelete(api, stub)
		return nil
	})
}
