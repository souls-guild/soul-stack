package api

// Registration and spec-dump of the PROFILE domain (Cloud Profile CRUD, ADR-017)
// on huma full-typed, following the push-provider/provider pattern. create/delete —
// WRITE+AUDIT (profile.created/.deleted events); list/get — read (no audit). MCP
// profile-tools call profile.Service directly.

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

// newProfile projects the flat handlers.ProfileView into a native Profile. params
// is normalized by the handler nil→{}.
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

// newProfileListReply projects the domain ProfileListPage into a native envelope.
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

// registerHumaProfileCreate mounts POST /v1/profiles (WRITE+AUDIT —
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

// registerHumaProfileList mounts GET /v1/profiles (READ with typed query,
// no audit). RBAC profile.read — on the group.
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

// registerHumaProfileGet mounts GET /v1/profiles/{name} (READ with path,
// no audit). RBAC profile.read — on the group.
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

// registerHumaProfileDelete mounts DELETE /v1/profiles/{name} (WRITE+AUDIT —
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

// newHumaProfileAPI builds a huma.API over the chi group with huma-audit-middleware
// for the given event type.
func newHumaProfileAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaProfileSpecYAML assembles the OpenAPI fragment of all profile routes as
// a YAML string without mounting.
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
