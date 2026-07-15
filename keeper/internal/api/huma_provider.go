package api

// Registration and spec-dump of the PROVIDER domain (Cloud Provider CRUD, ADR-017) on huma,
// full-typed after the push-provider reference. create/delete — WRITE+AUDIT (variant B,
// huma-audit-middleware; events provider.created/.deleted); list/get — read
// (no audit). Domain *Typed functions (handlers/provider.go) extracted from (w,r);
// MCP provider-tools call provider.Service directly.

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

// newProvider projects the flat handlers.ProviderView into a native Provider.
func newProvider(v handlers.ProviderView) Provider {
	return Provider{
		CreatedAt:      v.CreatedAt,
		CreatedByAID:   v.CreatedByAID,
		CredentialsRef: v.CredentialsRef,
		FQDNSuffix:     v.FQDNSuffix,
		Name:           v.Name,
		Region:         v.Region,
		Type:           v.Type,
	}
}

// newProviderListReply projects the domain ProviderListPage into a native envelope.
// Items: nil → nil, otherwise a non-nil slice (handler does make([]…, 0, n) → on
// success Items is always a non-nil []).
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

// registerHumaProviderCreate mounts POST /v1/providers (WRITE+AUDIT —
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
			Credentials:    in.Body.Credentials,
			FQDNSuffix:     in.Body.FQDNSuffix,
		})
		if err != nil {
			return nil, providerProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &providerCreateOutput{Status: 201, Body: newProvider(reply.Body)}, nil
	})
}

// registerHumaProviderList mounts GET /v1/providers (READ with typed query,
// no audit). RBAC provider.read — on the group.
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

// registerHumaProviderGet mounts GET /v1/providers/{name} (READ with path,
// no audit). RBAC provider.read — on the group.
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

// registerHumaProviderDelete mounts DELETE /v1/providers/{name} (WRITE+AUDIT —
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

// providerMissingClaims — defensive reply when claims are missing (unreachable:
// RequireJWT sets claims before huma).
func providerMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// providerProblem delivers a *Typed-function error through huma as problem+json.
func providerProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaProviderAPI builds a huma.API over a chi group with huma-audit-middleware
// (variant B) for the given event type. Each write route (create/delete) —
// its own chi group with its own event type.
func newHumaProviderAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaProviderSpecYAML assembles the OpenAPI fragment of all provider routes as
// a YAML string without mounting (hook for spec-merge + guard test).
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
