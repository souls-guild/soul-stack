package api

// Registration and spec-dump of the SERVICE domain (the Service registry) on huma
// full-typed (ROLLOUT BATCH 2d, modeled on role/operator/augur/herald, ADR-054 §Pattern).
// register/update/deregister — WRITE+AUDIT (variant B, huma audit middleware; events
// service.registered/.updated/.deregistered); list/get + refs/scenarios/state-schema/
// dependencies — read (no audit). The domain *Typed functions (handlers/service.go) are
// extracted from (w,r); the old (w,r) is a thin strict shell (the service MCP tools call
// serviceregistry.Service directly, bypassing the handler — the extraction does not affect
// them).

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

// registerHumaServiceRegister mounts POST /v1/services via huma (WRITE+AUDIT variant B —
// event service.registered). serviceH nil → no-op. Handler: claims → RegisterTyped →
// audit payload on the huma ctx (SetHumaAuditPayload) → 201 typed output.
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

// registerHumaServiceList mounts GET /v1/services via huma (READ, no audit). serviceH nil
// → no-op. Handler: ListTyped → typed envelope output. RBAC service.list — on the group.
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

// registerHumaServiceGet mounts GET /v1/services/{name} via huma (READ with path, no
// audit). serviceH nil → no-op. Handler: GetTyped(name) → typed output (404 via problem).
// RBAC service.list — on the group.
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

// registerHumaServiceUpdate mounts PATCH /v1/services/{name} via huma (WRITE+AUDIT variant
// B — event service.updated). serviceH nil → no-op. Handler: claims → UpdateTyped
// (replace + invalidate) → audit payload → 200 WITH BODY.
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

// registerHumaServiceDeregister mounts DELETE /v1/services/{name} via huma (WRITE+AUDIT
// variant B — event service.deregistered). serviceH nil → no-op. Handler: DeregisterTyped
// (deletion + invalidate) → audit payload → empty 204 output.
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

// registerHumaServiceRefs mounts GET /v1/services/{name}/refs via huma (READ with path, no
// audit). serviceH nil → no-op. Handler: ListRefsTyped(name) → typed output (404/502 via
// problem). RBAC service.list — on the group.
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

// registerHumaServiceScenarios mounts GET /v1/services/{name}/scenarios via huma (READ with
// path+query, no audit). serviceH nil → no-op. Handler: ListScenariosTyped (name + optional
// ref) → typed output (404/502 via problem). RBAC service.list — on the group.
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

// registerHumaServiceStateSchema mounts GET /v1/services/{name}/state-schema via huma (READ
// with path+query, no audit). serviceH nil → no-op. Handler: ListStateSchemaTyped (name +
// optional ref) → typed output (404/502 via problem). RBAC service.list — on the group.
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

// registerHumaServiceDependencies mounts GET /v1/services/{name}/dependencies via huma
// (READ with path+query, no audit). serviceH nil → no-op. Handler: ListDependenciesTyped
// (name + optional ref) → typed output (404/502 via problem). RBAC service.list — on the group.
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

// registerHumaServiceDirectives mounts GET /v1/services/{name}/directives via huma (READ
// with path+query, no audit). serviceH nil → no-op. Handler: ListDirectivesTyped (name +
// optional ref/version) → typed output (404/502 via problem) + ETag/Cache-Control (the
// catalog is immutable per git-ref); If-None-Match matched SHA1 → 304 without a body. RBAC
// service.list — on the group.
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
			out.Status = http.StatusNotModified // huma skips the body on 304
			return out, nil
		}
		out.Status = http.StatusOK
		out.Body = reply
		return out, nil
	})
}

// registerHumaServiceTelemetry mounts GET /v1/services/{name}/telemetry via huma
// (READ-with-path+query, NO audit). serviceH nil → no-op. Handler:
// ListServiceTelemetryTyped (name + optional ref) → typed output (404/502 via problem) +
// ETag/Cache-Control (config immutable on git-ref); If-None-Match matches SHA1 → 304
// with no body. RBAC service.list — on the group.
func registerHumaServiceTelemetry(humaAPI huma.API, serviceH *handlers.ServiceHandler) {
	if serviceH == nil {
		return
	}
	huma.Register(humaAPI, serviceTelemetryOperation(), func(ctx context.Context, in *serviceTelemetryInput) (*serviceTelemetryOutput, error) {
		reply, err := serviceH.ListServiceTelemetryTyped(ctx, in.Name, in.Ref)
		if err != nil {
			return nil, serviceProblem(err)
		}
		out := &serviceTelemetryOutput{ETag: etagQuote(reply.SHA1), CacheControl: directivesCacheControlFor(reply.Ref)}
		if etagMatchesSHA1(in.IfNoneMatch, reply.SHA1) {
			out.Status = http.StatusNotModified // huma skips the body on 304
			return out, nil
		}
		out.Status = http.StatusOK
		out.Body = reply
		return out, nil
	})
}

// serviceMissingClaims — a defensive response when claims are absent from the ctx
// (unreachable: RequireJWT sets claims before huma). problem+json (parity roleMissingClaims).
func serviceMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// serviceProblem delivers a *Typed function's error through huma as problem+json. A domain
// *handlers.problemError → humaProblemError; non-problem → 500 (parity roleProblem).
func serviceProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaServiceAPI assembles a huma.API over a chi group with the huma audit middleware
// (variant B) for the given event type (parity newHumaRoleAPI). Each service write route
// (register/update/deregister) is mounted on ITS OWN chi group with its own event type.
func newHumaServiceAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaServiceSpecYAML assembles the OpenAPI fragment of ALL migrated-to-huma service routes
// as a YAML string, WITHOUT mounting on a real router. A hook for the rollout's spec merge
// target and the guard test. Delegates to the generic [humaDumpSpec] via the same register
// functions (single register path). Returns a 3.1.0 spec (huma default).
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
		registerHumaServiceTelemetry(api, stub)
		return nil
	})
}
