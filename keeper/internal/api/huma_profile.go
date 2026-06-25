package api

// Регистрация и spec-dump PROFILE-домена (Cloud Profile CRUD, ADR-017) на huma
// full-typed по эталону push-provider/provider. create/delete — WRITE+AUDIT
// (события profile.created/.deleted); list/get — read (БЕЗ audit). MCP
// profile-tools зовут profile.Service напрямую.

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

// newProfile проецирует плоский handlers.ProfileView в native Profile. params
// нормализован handler-ом nil→{}.
func newProfile(v handlers.ProfileView) Profile {
	return Profile{
		CloudInit:    v.CloudInit,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Name:         v.Name,
		Params:       v.Params,
		Provider:     v.Provider,
	}
}

// newProfileListReply проецирует доменный ProfileListPage в native envelope.
func newProfileListReply(p handlers.ProfileListPage) ProfileListReply {
	var items []Profile
	if p.Items != nil {
		items = make([]Profile, len(p.Items))
		for i := range p.Items {
			items[i] = newProfile(p.Items[i])
		}
	}
	return ProfileListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// registerHumaProfileCreate монтирует POST /v1/profiles (WRITE+AUDIT —
// profile.created). profileH nil → no-op.
func registerHumaProfileCreate(humaAPI huma.API, profileH *handlers.ProfileHandler) {
	if profileH == nil {
		return
	}
	huma.Register(humaAPI, profileCreateOperation(), func(ctx context.Context, in *profileCreateInput) (*profileCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, profileMissingClaims()
		}
		req := handlers.ProfileCreateInput{
			Name:      in.Body.Name,
			Provider:  in.Body.Provider,
			CloudInit: in.Body.CloudInit,
		}
		if in.Body.Params != nil {
			p := in.Body.Params
			req.Params = &p
		}
		reply, err := profileH.CreateTyped(ctx, claims, req)
		if err != nil {
			return nil, profileProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &profileCreateOutput{Status: 201, Body: newProfile(reply.Body)}, nil
	})
}

// registerHumaProfileList монтирует GET /v1/profiles (READ-with-typed-query,
// БЕЗ audit). RBAC profile.read — на группе.
func registerHumaProfileList(humaAPI huma.API, profileH *handlers.ProfileHandler) {
	if profileH == nil {
		return
	}
	huma.Register(humaAPI, profileListOperation(), func(ctx context.Context, in *profileListInput) (*profileListOutput, error) {
		reply, err := profileH.ListTyped(ctx, in.Provider, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, profileProblem(err)
		}
		return &profileListOutput{Body: newProfileListReply(reply)}, nil
	})
}

// registerHumaProfileGet монтирует GET /v1/profiles/{name} (READ-with-path,
// БЕЗ audit). RBAC profile.read — на группе.
func registerHumaProfileGet(humaAPI huma.API, profileH *handlers.ProfileHandler) {
	if profileH == nil {
		return
	}
	huma.Register(humaAPI, profileGetOperation(), func(ctx context.Context, in *profileGetInput) (*profileGetOutput, error) {
		reply, err := profileH.GetTyped(ctx, in.Name)
		if err != nil {
			return nil, profileProblem(err)
		}
		return &profileGetOutput{Body: newProfile(reply)}, nil
	})
}

// registerHumaProfileDelete монтирует DELETE /v1/profiles/{name} (WRITE+AUDIT —
// profile.deleted). profileH nil → no-op.
func registerHumaProfileDelete(humaAPI huma.API, profileH *handlers.ProfileHandler) {
	if profileH == nil {
		return
	}
	huma.Register(humaAPI, profileDeleteOperation(), func(ctx context.Context, in *profileDeleteInput) (*profileNoContentOutput, error) {
		reply, err := profileH.DeleteTyped(ctx, in.Name)
		if err != nil {
			return nil, profileProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &profileNoContentOutput{Status: 204}, nil
	})
}

func profileMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

func profileProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaProfileAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// под переданный event-тип.
func newHumaProfileAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaProfileSpecYAML собирает OpenAPI-фрагмент всех profile-роутов как
// YAML-строку без монтирования.
func HumaProfileSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.ProfileSpecStub()
		registerHumaProfileCreate(api, stub)
		registerHumaProfileList(api, stub)
		registerHumaProfileGet(api, stub)
		registerHumaProfileDelete(api, stub)
		return nil
	})
}
