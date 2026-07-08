package api

// Регистрация и spec-dump SERVICE-домена (реестр Service-ов) на huma full-typed
// (ТИРАЖ-БАТЧ-2d по эталонам role/operator/augur/herald, ADR-054 §Pattern).
// register/update/deregister — WRITE+AUDIT (вариант B, huma-audit-middleware;
// события service.registered/.updated/.deregistered); list/get + refs/scenarios/
// state-schema/dependencies — read (БЕЗ audit). Доменные *Typed-функции
// (handlers/service.go) извлечены из (w,r); старый (w,r) — тонкая strict-оболочка
// (service-MCP-tools зовут serviceregistry.Service напрямую, мимо handler —
// извлечение не затрагивает).

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaServiceRegister монтирует POST /v1/services через huma (WRITE+AUDIT
// вариант B — event service.registered). serviceH nil → no-op. Handler: claims →
// RegisterTyped → audit-payload на huma-ctx (SetHumaAuditPayload) → 201 typed output.
func registerHumaServiceRegister(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceRegisterOperation(), func(ctx context.Context, in *serviceRegisterInput) (*serviceRegisterOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, serviceMissingClaims()
		}
		reply, err := serviceH.RegisterTyped(ctx, claims, handlers.ServiceRegisterInput{
			Name:    in.Body.Name,
			Git:     in.Body.Git,
			Ref:     in.Body.Ref,
			Refresh: in.Body.Refresh,
		})
		if err != nil {
			return nil, serviceProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &serviceRegisterOutput{Status: 201, Body: newServiceView(reply.Body)}, nil
	})
}

// registerHumaServiceList монтирует GET /v1/services через huma (READ, БЕЗ audit).
// serviceH nil → no-op. Handler: ListTyped → typed envelope-output. RBAC
// service.list — на группе.
func registerHumaServiceList(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceListOperation(), func(ctx context.Context, _ *serviceListInput) (*serviceListOutput, error) {
		reply, err := serviceH.ListTyped(ctx)
		if err != nil {
			return nil, serviceProblem(err)
		}
		return &serviceListOutput{Body: newServiceListReply(reply)}, nil
	})
}

// registerHumaServiceGet монтирует GET /v1/services/{name} через huma (READ-with-
// path, БЕЗ audit). serviceH nil → no-op. Handler: GetTyped(name) → typed output
// (404 через problem). RBAC service.list — на группе.
func registerHumaServiceGet(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceGetOperation(), func(ctx context.Context, in *serviceGetInput) (*serviceGetOutput, error) {
		reply, err := serviceH.GetTyped(ctx, in.Name)
		if err != nil {
			return nil, serviceProblem(err)
		}
		return &serviceGetOutput{Body: newServiceView(reply)}, nil
	})
}

// registerHumaServiceUpdate монтирует PATCH /v1/services/{name} через huma (WRITE+AUDIT
// вариант B — event service.updated). serviceH nil → no-op. Handler: claims →
// UpdateTyped (replace + invalidate) → audit-payload → 200 С ТЕЛОМ.
func registerHumaServiceUpdate(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceUpdateOperation(), func(ctx context.Context, in *serviceUpdateInput) (*serviceUpdateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, serviceMissingClaims()
		}
		reply, err := serviceH.UpdateTyped(ctx, claims, in.Name, handlers.ServiceUpdateInput{
			Git:     in.Body.Git,
			Ref:     in.Body.Ref,
			Refresh: in.Body.Refresh,
		})
		if err != nil {
			return nil, serviceProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &serviceUpdateOutput{Status: 200, Body: newServiceView(reply.Body)}, nil
	})
}

// registerHumaServiceDeregister монтирует DELETE /v1/services/{name} через huma
// (WRITE+AUDIT вариант B — event service.deregistered). serviceH nil → no-op.
// Handler: DeregisterTyped (удаление + invalidate) → audit-payload → пустой 204-output.
func registerHumaServiceDeregister(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceDeregisterOperation(), func(ctx context.Context, in *serviceDeregisterInput) (*serviceNoContentOutput, error) {
		reply, err := serviceH.DeregisterTyped(ctx, in.Name)
		if err != nil {
			return nil, serviceProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{"name": reply.Name})
		return &serviceNoContentOutput{Status: 204}, nil
	})
}

// registerHumaServiceRefs монтирует GET /v1/services/{name}/refs через huma (READ-
// with-path, БЕЗ audit). serviceH nil → no-op. Handler: ListRefsTyped(name) → typed
// output (404/502 через problem). RBAC service.list — на группе.
func registerHumaServiceRefs(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceRefsOperation(), func(ctx context.Context, in *serviceRefsInput) (*serviceRefsOutput, error) {
		reply, err := serviceH.ListRefsTyped(ctx, in.Name)
		if err != nil {
			return nil, serviceProblem(err)
		}
		return &serviceRefsOutput{Body: newServiceRefsListReply(reply)}, nil
	})
}

// registerHumaServiceScenarios монтирует GET /v1/services/{name}/scenarios через huma
// (READ-with-path+query, БЕЗ audit). serviceH nil → no-op. Handler: ListScenariosTyped
// (name + опц. ref) → typed output (404/502 через problem). RBAC service.list — на группе.
func registerHumaServiceScenarios(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceScenariosOperation(), func(ctx context.Context, in *serviceScenariosInput) (*serviceScenariosOutput, error) {
		reply, err := serviceH.ListScenariosTyped(ctx, in.Name, in.Ref)
		if err != nil {
			return nil, serviceProblem(err)
		}
		return &serviceScenariosOutput{Body: reply}, nil
	})
}

// registerHumaServiceStateSchema монтирует GET /v1/services/{name}/state-schema через
// huma (READ-with-path+query, БЕЗ audit). serviceH nil → no-op. Handler:
// ListStateSchemaTyped (name + опц. ref) → typed output (404/502 через problem).
// RBAC service.list — на группе.
func registerHumaServiceStateSchema(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceStateSchemaOperation(), func(ctx context.Context, in *serviceStateSchemaInput) (*serviceStateSchemaOutput, error) {
		reply, err := serviceH.ListStateSchemaTyped(ctx, in.Name, in.Ref)
		if err != nil {
			return nil, serviceProblem(err)
		}
		return &serviceStateSchemaOutput{Body: newServiceStateSchemaReply(reply)}, nil
	})
}

// registerHumaServiceDependencies монтирует GET /v1/services/{name}/dependencies через
// huma (READ-with-path+query, БЕЗ audit). serviceH nil → no-op. Handler:
// ListDependenciesTyped (name + опц. ref) → typed output (404/502 через problem).
// RBAC service.list — на группе.
func registerHumaServiceDependencies(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceDependenciesOperation(), func(ctx context.Context, in *serviceDependenciesInput) (*serviceDependenciesOutput, error) {
		reply, err := serviceH.ListDependenciesTyped(ctx, in.Name, in.Ref)
		if err != nil {
			return nil, serviceProblem(err)
		}
		return &serviceDependenciesOutput{Body: newServiceDependenciesReply(reply)}, nil
	})
}

// registerHumaServiceDirectives монтирует GET /v1/services/{name}/directives через huma
// (READ-with-path+query, БЕЗ audit). serviceH nil → no-op. Handler: ListDirectivesTyped
// (name + опц. ref/version) → typed output (404/502 через problem) + ETag/Cache-Control
// (каталог immutable на git-ref); If-None-Match совпал с SHA1 → 304 без тела. RBAC
// service.list — на группе.
func registerHumaServiceDirectives(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceDirectivesOperation(), func(ctx context.Context, in *serviceDirectivesInput) (*serviceDirectivesOutput, error) {
		reply, err := serviceH.ListDirectivesTyped(ctx, in.Name, in.Ref, in.Version)
		if err != nil {
			return nil, serviceProblem(err)
		}
		out := &serviceDirectivesOutput{ETag: etagQuote(reply.SHA1), CacheControl: directivesCacheControlFor(reply.Ref)}
		if etagMatchesSHA1(in.IfNoneMatch, reply.SHA1) {
			out.Status = http.StatusNotModified // huma пропускает тело на 304
			return out, nil
		}
		out.Status = http.StatusOK
		out.Body = reply
		return out, nil
	})
}

// serviceMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func serviceMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// serviceProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func serviceProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaServiceAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// service (register/update/deregister) монтируется на СВОЕЙ chi-группе с собственным
// event-типом.
func newHumaServiceAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaServiceSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma service-
// роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-
// таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же
// register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaServiceSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ServiceSpecStub()
		registerHumaServiceRegister(api, stub)
		registerHumaServiceList(api, stub)
		registerHumaServiceGet(api, stub)
		registerHumaServiceUpdate(api, stub)
		registerHumaServiceDeregister(api, stub)
		registerHumaServiceRefs(api, stub)
		registerHumaServiceScenarios(api, stub)
		registerHumaServiceStateSchema(api, stub)
		registerHumaServiceDependencies(api, stub)
		registerHumaServiceDirectives(api, stub)
		return nil
	})
}
