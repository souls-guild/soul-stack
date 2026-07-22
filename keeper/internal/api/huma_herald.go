package api

// Registration and spec-dump of the HERALD domain (heralds + tidings) on huma full-typed
// (handler-native T5d-2c following the role/operator/augur/push-provider references, ADR-054
// §Pattern). ONE [handlers.HeraldHandler] serves BOTH resources. herald create/
// update/delete + tiding create/update/delete — WRITE+AUDIT (variant B, huma-audit
// middleware; events herald.created/.updated/.deleted and tiding.created/.updated/
// .deleted); herald/tiding list/get — read (no audit). The domain *Typed functions
// (handlers/herald.go) take NATIVE request types and return domain results
// with flat wire fields; the register-func projects them into native wire-DTO
// (huma_herald_reply.go) DIRECTLY — the legacy generator is not involved. MCP herald-tools call
// herald.Service directly (bypassing the handler).

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

// registerHumaHeraldCreate mounts POST /v1/heralds via huma (WRITE+AUDIT
// variant B — event herald.created). heraldH nil → no-op. Handler: claims →
// CreateHeraldTyped → audit-payload on the huma ctx (SetHumaAuditPayload) → 201 typed output.
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
			Secret:    in.Body.Secret,
			Enabled:   in.Body.Enabled,
		})
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &heraldCreateOutput{Status: 201, Body: newHerald(reply.View)}, nil
	})
}

// registerHumaHeraldList mounts GET /v1/heralds via huma (READ with typed query,
// no audit). heraldH nil → no-op. Handler: typed-query → ListHeraldsTyped → typed
// envelope-output. RBAC herald.list — on the group.
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

// registerHumaHeraldGet mounts GET /v1/heralds/{name} via huma (READ with path,
// no audit). heraldH nil → no-op. Handler: GetHeraldTyped(name) → typed output
// (404/422 via problem). RBAC herald.read — on the group.
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

// registerHumaHeraldUpdate mounts PUT /v1/heralds/{name} via huma (WRITE+AUDIT
// variant B — event herald.updated). heraldH nil → no-op. Handler: UpdateHeraldTyped
// (replace) → audit-payload → 200 WITH BODY.
func registerHumaHeraldUpdate(humaAPI huma.API, heraldH *handlers.HeraldHandler) {
	if heraldH == nil {
		return
	}
	huma.Register(humaAPI, heraldUpdateOperation(), func(ctx context.Context, in *heraldUpdateInput) (*heraldUpdateOutput, error) {
		reply, err := heraldH.UpdateHeraldTyped(ctx, in.Name, handlers.HeraldUpdateInput{
			Type:      in.Body.Type,
			Config:    in.Body.Config,
			SecretRef: in.Body.SecretRef,
			Secret:    in.Body.Secret,
			Enabled:   in.Body.Enabled,
		})
		if err != nil {
			return nil, heraldProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &heraldUpdateOutput{Status: 200, Body: newHerald(reply.View)}, nil
	})
}

// registerHumaHeraldDelete mounts DELETE /v1/heralds/{name} via huma (WRITE+AUDIT
// variant B — event herald.deleted). heraldH nil → no-op. Handler: DeleteHeraldTyped →
// audit-payload → empty 204 output.
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

// registerHumaTidingCreate mounts POST /v1/tidings via huma (WRITE+AUDIT variant B
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

// registerHumaTidingList mounts GET /v1/tidings via huma (READ with typed query,
// no audit). heraldH nil → no-op. Handler: typed-query (offset/limit/include_ephemeral)
// → ListTidingsTyped → typed envelope-output. RBAC tiding.list — on the group.
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

// registerHumaTidingGet mounts GET /v1/tidings/{name} via huma (READ with path,
// no audit). heraldH nil → no-op. Handler: GetTidingTyped(name) → typed output
// (404/422 via problem). RBAC tiding.read — on the group.
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

// registerHumaTidingUpdate mounts PUT /v1/tidings/{name} via huma (WRITE+AUDIT
// variant B — event tiding.updated). heraldH nil → no-op. Handler: UpdateTidingTyped
// (replace) → audit-payload → 200 WITH BODY.
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

// registerHumaTidingDelete mounts DELETE /v1/tidings/{name} via huma (WRITE+AUDIT
// variant B — event tiding.deleted). heraldH nil → no-op. Handler: DeleteTidingTyped →
// audit-payload → empty 204 output.
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

// heraldMissingClaims — a defensive response when claims are absent from ctx (unreachable:
// RequireJWT sets claims before huma). problem+json (parity roleMissingClaims).
func heraldMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// heraldProblem delivers a *Typed-function error through huma as problem+json.
// A domain *handlers.problemError → humaProblemError; a non-problem → 500 (parity
// roleProblem).
func heraldProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaHeraldAPI assembles a huma.API over a chi group with the huma-audit middleware
// (variant B) under the given event type (parity newHumaRoleAPI). Each herald/tiding write
// route (create/update/delete) is mounted on ITS OWN chi group with its own
// event type.
func newHumaHeraldAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaHeraldSpecYAML assembles the OpenAPI fragment of ALL herald-/tiding routes migrated
// to huma as a YAML string, WITHOUT mounting on a real router. A hook for the
// rollout's spec-merge target and the guard-test. Delegates to the generic [humaDumpSpec] via the
// same register functions (single register path). Returns a 3.1.0 spec (huma default).
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
