package api

// Registration and spec-dump of the SYNOD domain (groups / membership / bundle) on huma
// full-typed (ROLLOUT BATCH 2d following role/operator/augur/herald, ADR-054
// §Pattern). synod create/update/delete + add/remove-operator + grant/revoke-role —
// WRITE+AUDIT (variant B, huma-audit-middleware; events synod.created/.updated/
// .deleted/.operator-added/.operator-removed/.role-granted/.role-revoked); synod
// list — read (no audit). The domain *Typed functions (handlers/synod.go) are extracted
// from (w,r); the old (w,r) is a thin strict wrapper (synod MCP tools call rbac.Service
// directly, bypassing the handler — the extraction does not affect them).

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

// registerHumaSynodCreate mounts POST /v1/synods via huma (WRITE+AUDIT
// variant B — event synod.created). synodH nil → no-op. Handler: claims →
// CreateTyped → audit-payload on the huma ctx (SetHumaAuditPayload) → empty 201 output.
func registerHumaSynodCreate(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodCreateOperation(), func(ctx context.Context, in *synodCreateInput) (*synodCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.CreateTyped(ctx, claims, handlers.SynodCreateInput{
			Name:        in.Body.Name,
			Description: in.Body.Description,
		})
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &synodCreateOutput{Status: 201}, nil
	})
}

// registerHumaSynodList mounts GET /v1/synods via huma (READ, no audit).
// synodH nil → no-op. Handler: ListTyped → typed envelope output. RBAC synod.list —
// on the group.
func registerHumaSynodList(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodListOperation(), func(ctx context.Context, _ *synodListInput) (*synodListOutput, error) {
		reply, err := synodH.ListTyped(ctx)
		if err != nil {
			return nil, synodProblem(err)
		}
		return &synodListOutput{Body: newSynodListReply(reply)}, nil
	})
}

// registerHumaSynodUpdate mounts PATCH /v1/synods/{name} via huma (WRITE+AUDIT
// variant B — event synod.updated). synodH nil → no-op. Handler: claims →
// UpdateTyped (changes description) → audit-payload → empty 204 output.
func registerHumaSynodUpdate(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodUpdateOperation(), func(ctx context.Context, in *synodUpdateInput) (*synodNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.UpdateTyped(ctx, claims, in.Name, handlers.SynodUpdateInput{
			Description: in.Body.Description,
		})
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodDelete mounts DELETE /v1/synods/{name} via huma (WRITE+AUDIT
// variant B — event synod.deleted). synodH nil → no-op. Handler: DeleteTyped →
// audit-payload → empty 204 output.
func registerHumaSynodDelete(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodDeleteOperation(), func(ctx context.Context, in *synodDeleteInput) (*synodNoContentOutput, error) {
		reply, err := synodH.DeleteTyped(ctx, in.Name)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{"name": reply.Name})
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodAddOperator mounts POST /v1/synods/{name}/operators via huma
// (WRITE+AUDIT variant B — event synod.operator-added). synodH nil → no-op.
// Handler: claims → AddOperatorTyped (AID validation + binding) → audit-payload →
// empty 204 output.
func registerHumaSynodAddOperator(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodAddOperatorOperation(), func(ctx context.Context, in *synodAddOperatorInput) (*synodNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.AddOperatorTyped(ctx, claims, in.Name, in.Body.AID)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AddOperatorAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodRemoveOperator mounts DELETE /v1/synods/{name}/operators/{aid}
// via huma (WRITE+AUDIT variant B — event synod.operator-removed). synodH nil →
// no-op. Handler: RemoveOperatorTyped (path-AID validation + removal) → audit-payload
// → empty 204 output.
func registerHumaSynodRemoveOperator(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodRemoveOperatorOperation(), func(ctx context.Context, in *synodRemoveOperatorInput) (*synodNoContentOutput, error) {
		reply, err := synodH.RemoveOperatorTyped(ctx, in.Name, in.AID)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.RemoveOperatorAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodGrantRole mounts POST /v1/synods/{name}/roles via huma
// (WRITE+AUDIT variant B — event synod.role-granted). synodH nil → no-op. Handler:
// claims → GrantRoleTyped (role validation + adding to the bundle) → audit-payload →
// empty 204 output.
func registerHumaSynodGrantRole(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodGrantRoleOperation(), func(ctx context.Context, in *synodGrantRoleInput) (*synodNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, synodMissingClaims()
		}
		reply, err := synodH.GrantRoleTyped(ctx, claims, in.Name, in.Body.Role)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.GrantRoleAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSynodRevokeRole mounts DELETE /v1/synods/{name}/roles/{role_name}
// via huma (WRITE+AUDIT variant B — event synod.role-revoked). synodH nil → no-op.
// Handler: RevokeRoleTyped (removing the role from the bundle) → audit-payload → empty 204 output.
func registerHumaSynodRevokeRole(humaAPI huma.API, synodH *handlers.SynodHandler) {
	if synodH == nil {
		return
	}
	huma.Register(humaAPI, synodRevokeRoleOperation(), func(ctx context.Context, in *synodRevokeRoleInput) (*synodNoContentOutput, error) {
		reply, err := synodH.RevokeRoleTyped(ctx, in.Name, in.Role)
		if err != nil {
			return nil, synodProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.RevokeRoleAuditPayload()))
		return &synodNoContentOutput{Status: 204}, nil
	})
}

// synodMissingClaims — defensive response when claims are absent from ctx (unreachable:
// RequireJWT sets claims before huma). problem+json (parity with roleMissingClaims).
func synodMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// synodProblem delivers a *Typed-function error through huma as problem+json.
// A domain *handlers.problemError → humaProblemError; non-problem → 500 (parity with
// roleProblem).
func synodProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSynodAPI builds a huma.API over a chi group with huma-audit-middleware
// (variant B) under the given event type (parity with newHumaRoleAPI). Each synod write route
// (create/update/delete, add/remove-operator, grant/revoke-role) is mounted
// on its OWN chi group with its own event type.
func newHumaSynodAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSynodSpecYAML assembles the OpenAPI fragment of ALL synod routes migrated to huma
// as a YAML string, WITHOUT mounting on a real router. A hook for the rollout spec-merge
// target and the guard-test. Delegates to the generic [humaDumpSpec] through the same
// register functions (a single register path). Returns a 3.1.0 spec (huma default).
func HumaSynodSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SynodSpecStub()
		registerHumaSynodCreate(api, stub)
		registerHumaSynodList(api, stub)
		registerHumaSynodUpdate(api, stub)
		registerHumaSynodDelete(api, stub)
		registerHumaSynodAddOperator(api, stub)
		registerHumaSynodRemoveOperator(api, stub)
		registerHumaSynodGrantRole(api, stub)
		registerHumaSynodRevokeRole(api, stub)
		return nil
	})
}
