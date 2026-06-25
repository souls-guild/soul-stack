package api

// Регистрация и spec-dump PROVIDER-домена (Cloud Provider CRUD, ADR-017) на huma
// full-typed по эталону push-provider. create/delete — WRITE+AUDIT (вариант B,
// huma-audit-middleware; события provider.created/.deleted); list/get — read
// (БЕЗ audit). Доменные *Typed-функции (handlers/provider.go) извлечены из (w,r);
// MCP provider-tools зовут provider.Service напрямую.

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

// newProvider проецирует плоский handlers.ProviderView в native Provider.
func newProvider(v handlers.ProviderView) Provider {
	return Provider{
		CreatedAt:      v.CreatedAt,
		CreatedByAID:   v.CreatedByAID,
		CredentialsRef: v.CredentialsRef,
		Name:           v.Name,
		Region:         v.Region,
		Type:           v.Type,
	}
}

// newProviderListReply проецирует доменный ProviderListPage в native envelope.
// Items: nil → nil, иначе non-nil срез (handler делает make([]…, 0, n) → на
// success Items всегда non-nil []).
func newProviderListReply(p handlers.ProviderListPage) ProviderListReply {
	var items []Provider
	if p.Items != nil {
		items = make([]Provider, len(p.Items))
		for i := range p.Items {
			items[i] = newProvider(p.Items[i])
		}
	}
	return ProviderListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// registerHumaProviderCreate монтирует POST /v1/providers (WRITE+AUDIT —
// provider.created). providerH nil → no-op.
func registerHumaProviderCreate(humaAPI huma.API, providerH *handlers.ProviderHandler) {
	if providerH == nil {
		return
	}
	huma.Register(humaAPI, providerCreateOperation(), func(ctx context.Context, in *providerCreateInput) (*providerCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, providerMissingClaims()
		}
		reply, err := providerH.CreateTyped(ctx, claims, handlers.ProviderCreateInput{
			Name:           in.Body.Name,
			Type:           in.Body.Type,
			Region:         in.Body.Region,
			CredentialsRef: in.Body.CredentialsRef,
		})
		if err != nil {
			return nil, providerProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &providerCreateOutput{Status: 201, Body: newProvider(reply.Body)}, nil
	})
}

// registerHumaProviderList монтирует GET /v1/providers (READ-with-typed-query,
// БЕЗ audit). RBAC provider.read — на группе.
func registerHumaProviderList(humaAPI huma.API, providerH *handlers.ProviderHandler) {
	if providerH == nil {
		return
	}
	huma.Register(humaAPI, providerListOperation(), func(ctx context.Context, in *providerListInput) (*providerListOutput, error) {
		reply, err := providerH.ListTyped(ctx, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, providerProblem(err)
		}
		return &providerListOutput{Body: newProviderListReply(reply)}, nil
	})
}

// registerHumaProviderGet монтирует GET /v1/providers/{name} (READ-with-path,
// БЕЗ audit). RBAC provider.read — на группе.
func registerHumaProviderGet(humaAPI huma.API, providerH *handlers.ProviderHandler) {
	if providerH == nil {
		return
	}
	huma.Register(humaAPI, providerGetOperation(), func(ctx context.Context, in *providerGetInput) (*providerGetOutput, error) {
		reply, err := providerH.GetTyped(ctx, in.Name)
		if err != nil {
			return nil, providerProblem(err)
		}
		return &providerGetOutput{Body: newProvider(reply)}, nil
	})
}

// registerHumaProviderDelete монтирует DELETE /v1/providers/{name} (WRITE+AUDIT —
// provider.deleted). providerH nil → no-op.
func registerHumaProviderDelete(humaAPI huma.API, providerH *handlers.ProviderHandler) {
	if providerH == nil {
		return
	}
	huma.Register(humaAPI, providerDeleteOperation(), func(ctx context.Context, in *providerDeleteInput) (*providerNoContentOutput, error) {
		reply, err := providerH.DeleteTyped(ctx, in.Name)
		if err != nil {
			return nil, providerProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &providerNoContentOutput{Status: 204}, nil
	})
}

// providerMissingClaims — defensive-ответ при отсутствии claims (недостижим:
// RequireJWT кладёт claims до huma).
func providerMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// providerProblem доставляет ошибку *Typed-функции через huma как problem+json.
func providerProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaProviderAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип. Каждый write-роут (create/delete) —
// своя chi-группа с собственным event-типом.
func newHumaProviderAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaProviderSpecYAML собирает OpenAPI-фрагмент всех provider-роутов как
// YAML-строку без монтирования (хук для spec-merge + guard-теста).
func HumaProviderSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ProviderSpecStub()
		registerHumaProviderCreate(api, stub)
		registerHumaProviderList(api, stub)
		registerHumaProviderGet(api, stub)
		registerHumaProviderDelete(api, stub)
		return nil
	})
}
