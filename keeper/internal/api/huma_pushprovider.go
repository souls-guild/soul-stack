package api

// Registration and spec-dump of the PUSH-PROVIDER domain on huma full-typed (ROLLOUT BATCH 2b,
// following role/operator, ADR-054 §Pattern). create/update/delete — WRITE+AUDIT
// (variant B, huma-audit-middleware; events push-provider.created/.updated/.deleted);
// list/get — read (no audit). The domain *Typed functions (handlers/pushprovider.go) are
// extracted from (w,r); the old (w,r) is a thin strict wrapper (MCP push-provider tools call
// pushprovider.Service directly, bypassing the handler — the extraction does not affect them).

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

// === projection of the push-provider handler's domain views → native wire-DTO (handler-native:
// the api↔handlers boundary builds the wire body from flat domain fields; oapi-generated types
// do not participate). ===

// newPushProvider projects the flat handlers.PushProviderView into the native PushProvider
// (Create-201 / Get-200 / Update-200 / list element). params normalized by the handler nil→{};
// created_at/updated_at — nanosecond time-wire; updated_by_aid — optional pointer.
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

// newPushProviderListReply projects the domain handlers.PushProviderListPage into the native
// envelope PushProviderListReply. Items: nil → nil, otherwise a non-nil slice (the handler does
// make([]…, 0, n), so on success Items is always non-nil [] — byte-exact with the former legacy generator).
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

// registerHumaPushProviderCreate mounts POST /v1/push-providers via huma
// (WRITE+AUDIT variant B — event push-provider.created). pushProviderH nil → no-op.
// Handler: claims → CreateTyped → audit-payload on huma ctx → 201 typed output.
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

// registerHumaPushProviderList mounts GET /v1/push-providers via huma (READ with typed query,
// no audit). pushProviderH nil → no-op. Handler: typed query → ListTyped → typed envelope
// output. RBAC push-provider.list — on the group.
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

// registerHumaPushProviderGet mounts GET /v1/push-providers/{name} via huma
// (READ with path, no audit). pushProviderH nil → no-op. Handler: GetTyped(name) →
// typed output (404/422 via problem). RBAC push-provider.read — on the group.
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

// registerHumaPushProviderUpdate mounts PUT /v1/push-providers/{name} via huma
// (WRITE+AUDIT variant B — event push-provider.updated). pushProviderH nil → no-op.
// Handler: claims → UpdateTyped (replace params) → audit-payload → 200 WITH BODY.
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

// registerHumaPushProviderDelete mounts DELETE /v1/push-providers/{name} via huma
// (WRITE+AUDIT variant B — event push-provider.deleted). pushProviderH nil → no-op.
// Handler: DeleteTyped → audit-payload → empty 204 output.
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

// pushProviderMissingClaims — defensive response when claims are absent in ctx (unreachable:
// RequireJWT sets claims before huma). problem+json (parity roleMissingClaims).
func pushProviderMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// pushProviderProblem delivers a *Typed function's error through huma as problem+json.
// A domain *handlers.problemError → humaProblemError; non-problem → 500 (parity roleProblem).
func pushProviderProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaPushProviderAPI assembles a huma.API over a chi group with huma-audit-middleware
// (variant B) for the given event type (parity newHumaRoleAPI). Each push-provider write route
// (create/update/delete) is mounted on ITS OWN chi group with its own event type.
func newHumaPushProviderAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaPushProviderSpecYAML assembles the OpenAPI fragment of ALL huma-migrated push-provider
// routes as a YAML string, WITHOUT mounting on a real router. Hook for the rollout spec-merge
// target and the guard test. Delegates to the generic [humaDumpSpec] via the same register
// functions (single register path). Returns a 3.1.0 spec (huma default).
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
