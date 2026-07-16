package api

// PILOT-2 of the OpenAPI spec-first → code-first rollout onto huma v2, FULL-TYPED form
// (ADR-054 §Audit). Proves the POST /v1/roles ROUTE on top of chi-mux through huma as
// the REFERENCE for rolling out ~30 middleware-audit domains: the same FULL-TYPED envelope
// as pilot-1 (cadence), PLUS huma-native audit-middleware (variant B, huma_audit.go) —
// because role used to write audit through apimiddleware.Audit + SetAuditPayload, while
// full-typed huma writes the response ITSELF (StatusRecorder does not apply to it). Pilot-1
// cadence wrote self-audit INSIDE CreateTyped (emitWrite) and had no middleware-audit —
// rolling out domains with middleware-audit needs exactly variant B.
//
// The boundary is the same as pilot-1 (huma_cadence.go §FULL-TYPED PATTERN): typed input +
// extracted CreateTyped + a thin envelope + typed output (no Body — empty 201,
// legacy contract). The difference — the payload is placed on the huma-ctx via
// SetHumaAuditPayload (rather than on *http.Request via SetAuditPayload), the middleware
// reads it after next.

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

// registerHumaRole mounts POST /v1/roles via huma onto the given chi.Router
// (the group that already carries RequireJWT/RequirePermission(role.create) +
// huma-audit-middleware). roleH is the domain handler; nil → no-op (router.go
// opt-in-domain pattern: the route is wired only when roleH is non-nil).
//
// FULL-TYPED handler: huma validates the typed Body → converts to the domain
// model → CreateTyped → audit payload on the huma-ctx (SetHumaAuditPayload,
// read by humaAuditMiddleware after next) → empty typed output (201). Domain
// problem errors go through humaProblemError (the same error contract as
// huma validation).
func registerHumaRole(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleCreateOperation(), func(ctx context.Context, in *roleCreateInput) (*roleCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, roleMissingClaims()
		}
		reply, err := roleH.CreateTyped(ctx, claims, handlers.RoleCreateInput{
			Name:         in.Body.Name,
			Description:  in.Body.Description,
			Permissions:  in.Body.Permissions,
			DefaultScope: in.Body.DefaultScope,
		})
		if err != nil {
			return nil, roleProblem(err)
		}
		// Audit-payload on huma-ctx: humaAuditMiddleware (variant B) seeds carrier
		// BEFORE next, reads payload AFTER. Fields — parity with legacy SetAuditPayload
		// (name + permissions + created_by_aid; without secrets, ADR-022).
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &roleCreateOutput{Status: http.StatusCreated}, nil
	})
}

// registerHumaRoleList mounts GET /v1/roles via huma on chi group
// /v1/roles (READ variant pilot-1 — full-typed output, WITHOUT audit-middleware).
// roleH nil → no-op. Handler reads catalog (ListTyped) → envelope to typed output;
// error reading → roleProblem (500). RBAC role.list — on group (huma inherits).
func registerHumaRoleList(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleListOperation(), func(ctx context.Context, _ *roleListInput) (*roleListOutput, error) {
		reply, err := roleH.ListTyped(ctx)
		if err != nil {
			return nil, roleProblem(err)
		}
		return &roleListOutput{Body: newRoleListReply(reply)}, nil
	})
}

// registerHumaRoleDelete mounts DELETE /v1/roles/{name} via huma (WRITE+AUDIT
// variant B — event role.deleted attached newHumaAuditAPI on group). roleH nil →
// no-op. Handler: DeleteTyped → audit-payload on huma-ctx → empty 204 output.
func registerHumaRoleDelete(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleDeleteOperation(), func(ctx context.Context, in *roleDeleteInput) (*roleNoContentOutput, error) {
		reply, err := roleH.DeleteTyped(ctx, in.Name)
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{"name": reply.Name})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaRoleUpdatePermissions mounts PATCH /v1/roles/{name}/permissions
// via huma (WRITE+AUDIT — event role.permissions-updated). roleH nil → no-op.
// Handler: claims → envelope presence default_scope from [Optional] to domain
// SetDefaultScope/DefaultScope (omitted→Set=false do not touch; null→Set=true reset;
// value→Set=true set) → UpdatePermissionsTyped → audit-payload → 204.
func registerHumaRoleUpdatePermissions(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleUpdatePermissionsOperation(), func(ctx context.Context, in *roleUpdatePermissionsInput) (*roleNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, roleMissingClaims()
		}
		reply, err := roleH.UpdatePermissionsTyped(ctx, claims, handlers.UpdatePermissionsInput{
			Name:            in.Name,
			Permissions:     in.Body.Permissions,
			SetDefaultScope: in.Body.DefaultScope.Set,
			DefaultScope:    optionalToPtr(in.Body.DefaultScope),
		})
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{
			"name":        reply.Name,
			"permissions": reply.Permissions,
		})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaRoleGrantOperator mounts POST /v1/roles/{name}/operators via
// huma (WRITE+AUDIT — event role.operator-granted). roleH nil → no-op. Handler:
// claims → GrantOperatorTyped (validation AID + binding) → audit-payload → 204.
func registerHumaRoleGrantOperator(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleGrantOperatorOperation(), func(ctx context.Context, in *roleGrantOperatorInput) (*roleNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, roleMissingClaims()
		}
		reply, err := roleH.GrantOperatorTyped(ctx, claims, in.Name, in.Body.AID)
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{
			"name":           reply.Name,
			"aid":            reply.AID,
			"granted_by_aid": reply.GrantedByAID,
		})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaRoleRevokeOperator mounts DELETE /v1/roles/{name}/operators/{aid}
// via huma (WRITE+AUDIT — event role.operator-revoked). roleH nil → no-op.
// Handler: RevokeOperatorTyped (validation path-AID + removal) → audit-payload → 204.
func registerHumaRoleRevokeOperator(humaAPI huma.API, roleH *handlers.RoleHandler) {
	if roleH == nil {
		return
	}
	huma.Register(humaAPI, roleRevokeOperatorOperation(), func(ctx context.Context, in *roleRevokeOperatorInput) (*roleNoContentOutput, error) {
		reply, err := roleH.RevokeOperatorTyped(ctx, in.Name, in.AID)
		if err != nil {
			return nil, roleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload{
			"name": reply.Name,
			"aid":  reply.AID,
		})
		return &roleNoContentOutput{Status: http.StatusNoContent}, nil
	})
}

// roleMissingClaims — defensive-response when absent claims in ctx (unreachable:
// RequireJWT puts claims to huma). Returned as problem+json (not huma.NewError),
// to defensive-golden path carried same error contract, which other errors routes.
func roleMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// roleProblem delivers error CreateTyped via huma as problem+json. Domain
// *handlers.problemError → humaProblemError (its Details, status from table). No-
// problem (non-standard path) → 500 internal (parity cadenceProblem).
func roleProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaRoleSpecYAML builds OpenAPI-fragment of ALL migrated-on-huma role routes
// (create/list/delete/update-permissions/grant/revoke-operator) as YAML string, WITHOUT
// mounting on real router. Hook for spec-merge target batch/rollout and guard test.
// Delegates generic [humaDumpSpec], registering operations via same register-
// functions (single register path, no duplicate dump-vs-mount): handler stub on dump
// not called; audit-wrapper not needed (newHumaCadenceAPI without UseMiddleware
// sufficient for schema). Returns 3.1.0-spec (huma default).
func HumaRoleSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.RoleSpecStub()
		registerHumaRole(api, stub)
		registerHumaRoleList(api, stub)
		registerHumaRoleDelete(api, stub)
		registerHumaRoleUpdatePermissions(api, stub)
		registerHumaRoleGrantOperator(api, stub)
		registerHumaRoleRevokeOperator(api, stub)
		return nil
	})
}

// newHumaRoleAPI builds huma.API on top of chi group /v1/roles with huma-audit-
// middleware (variant B) under passed event type. Parallel newHumaCadenceAPI, but
// role writes audit OUTSIDE *Typed (via middleware) — cadence wrote self-audit
// inside. evt parameterized: each write route role (create/delete/update/grant/
// revoke) mounted on OWN chi-group with own event-type.
func newHumaRoleAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}
