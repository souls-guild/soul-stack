package api

// Регистрация и spec-dump PUSH-PROVIDER-домена на huma full-typed (ТИРАЖ-БАТЧ-2b по
// эталонам role/operator, ADR-054 §Pattern). create/update/delete — WRITE+AUDIT
// (вариант B, huma-audit-middleware; события push-provider.created/.updated/.deleted);
// list/get — read (БЕЗ audit). Доменные *Typed-функции (handlers/pushprovider.go)
// извлечены из (w,r); старый (w,r) — тонкая strict-оболочка (MCP push-provider-tools
// зовут pushprovider.Service напрямую, мимо handler — извлечение не затрагивает).

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

// === проекция доменных view-ов handler-а push-provider → native wire-DTO (handler-native:
// граница api↔handlers строит wire-тело из плоских доменных полей; oapi-генерёные типы не
// участвуют). ===

// newPushProvider проецирует плоский handlers.PushProviderView в native PushProvider
// (Create-201 / Get-200 / Update-200 / list-element). params normalized handler-ом nil→{};
// created_at/updated_at — наносекундный time-wire; updated_by_aid — опц. указатель.
func newPushProvider(v handlers.PushProviderView) PushProvider {
	return PushProvider{
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Name:         v.Name,
		Params:       v.Params,
		UpdatedAt:    v.UpdatedAt,
		UpdatedByAID: v.UpdatedByAID,
	}
}

// newPushProviderListReply проецирует доменный handlers.PushProviderListPage в native
// envelope PushProviderListReply. Items: nil → nil, иначе non-nil срез (handler делает
// make([]…, 0, n), поэтому на success Items всегда non-nil [] — byte-exact с прежним legacy-генерата).
func newPushProviderListReply(p handlers.PushProviderListPage) PushProviderListReply {
	var items []PushProvider
	if p.Items != nil {
		items = make([]PushProvider, len(p.Items))
		for i := range p.Items {
			items[i] = newPushProvider(p.Items[i])
		}
	}
	return PushProviderListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// registerHumaPushProviderCreate монтирует POST /v1/push-providers через huma
// (WRITE+AUDIT вариант B — event push-provider.created). pushProviderH nil → no-op.
// Handler: claims → CreateTyped → audit-payload на huma-ctx → 201 typed output.
func registerHumaPushProviderCreate(humaAPI huma.API, pushProviderH *handlers.PushProviderHandler) {
	if pushProviderH == nil {
		return
	}
	huma.Register(humaAPI, pushProviderCreateOperation(), func(ctx context.Context, in *pushProviderCreateInput) (*pushProviderCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, pushProviderMissingClaims()
		}
		req := handlers.PushProviderCreateInput{Name: in.Body.Name}
		if in.Body.Params != nil {
			p := in.Body.Params
			req.Params = &p
		}
		reply, err := pushProviderH.CreateTyped(ctx, claims, req)
		if err != nil {
			return nil, pushProviderProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &pushProviderCreateOutput{Status: 201, Body: newPushProvider(reply.Body)}, nil
	})
}

// registerHumaPushProviderList монтирует GET /v1/push-providers через huma (READ-with-
// typed-query, БЕЗ audit). pushProviderH nil → no-op. Handler: typed-query →
// ListTyped → typed envelope-output. RBAC push-provider.list — на группе.
func registerHumaPushProviderList(humaAPI huma.API, pushProviderH *handlers.PushProviderHandler) {
	if pushProviderH == nil {
		return
	}
	huma.Register(humaAPI, pushProviderListOperation(), func(ctx context.Context, in *pushProviderListInput) (*pushProviderListOutput, error) {
		reply, err := pushProviderH.ListTyped(ctx, in.NamePattern, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, pushProviderProblem(err)
		}
		return &pushProviderListOutput{Body: newPushProviderListReply(reply)}, nil
	})
}

// registerHumaPushProviderGet монтирует GET /v1/push-providers/{name} через huma
// (READ-with-path, БЕЗ audit). pushProviderH nil → no-op. Handler: GetTyped(name) →
// typed output (404/422 через problem). RBAC push-provider.read — на группе.
func registerHumaPushProviderGet(humaAPI huma.API, pushProviderH *handlers.PushProviderHandler) {
	if pushProviderH == nil {
		return
	}
	huma.Register(humaAPI, pushProviderGetOperation(), func(ctx context.Context, in *pushProviderGetInput) (*pushProviderGetOutput, error) {
		reply, err := pushProviderH.GetTyped(ctx, in.Name)
		if err != nil {
			return nil, pushProviderProblem(err)
		}
		return &pushProviderGetOutput{Body: newPushProvider(reply)}, nil
	})
}

// registerHumaPushProviderUpdate монтирует PUT /v1/push-providers/{name} через huma
// (WRITE+AUDIT вариант B — event push-provider.updated). pushProviderH nil → no-op.
// Handler: claims → UpdateTyped (replace params) → audit-payload → 200 С ТЕЛОМ.
func registerHumaPushProviderUpdate(humaAPI huma.API, pushProviderH *handlers.PushProviderHandler) {
	if pushProviderH == nil {
		return
	}
	huma.Register(humaAPI, pushProviderUpdateOperation(), func(ctx context.Context, in *pushProviderUpdateInput) (*pushProviderUpdateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, pushProviderMissingClaims()
		}
		reply, err := pushProviderH.UpdateTyped(ctx, claims, in.Name, handlers.PushProviderUpdateInput{Params: in.Body.Params})
		if err != nil {
			return nil, pushProviderProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &pushProviderUpdateOutput{Status: 200, Body: newPushProvider(reply.Body)}, nil
	})
}

// registerHumaPushProviderDelete монтирует DELETE /v1/push-providers/{name} через huma
// (WRITE+AUDIT вариант B — event push-provider.deleted). pushProviderH nil → no-op.
// Handler: DeleteTyped → audit-payload → пустой 204-output.
func registerHumaPushProviderDelete(humaAPI huma.API, pushProviderH *handlers.PushProviderHandler) {
	if pushProviderH == nil {
		return
	}
	huma.Register(humaAPI, pushProviderDeleteOperation(), func(ctx context.Context, in *pushProviderDeleteInput) (*pushProviderNoContentOutput, error) {
		reply, err := pushProviderH.DeleteTyped(ctx, in.Name)
		if err != nil {
			return nil, pushProviderProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &pushProviderNoContentOutput{Status: 204}, nil
	})
}

// pushProviderMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func pushProviderMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// pushProviderProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func pushProviderProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaPushProviderAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// push-provider (create/update/delete) монтируется на СВОЕЙ chi-группе с собственным
// event-типом.
func newHumaPushProviderAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaPushProviderSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma
// push-provider-роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для
// спека-мерж-таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те
// же register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaPushProviderSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.PushProviderSpecStub()
		registerHumaPushProviderCreate(api, stub)
		registerHumaPushProviderList(api, stub)
		registerHumaPushProviderGet(api, stub)
		registerHumaPushProviderUpdate(api, stub)
		registerHumaPushProviderDelete(api, stub)
		return nil
	})
}
