package api

// BATCH-1 read-tier rollout of OpenAPI spec-first → code-first on huma v2, FULL-TYPED
// form (ADR-054 §Pattern, the READ variant of pilot-1, no audit). Migrates three
// READ catalogs from strict (bridge+strictWrapper.X) to huma full-typed: typed input
// (empty) → the handler's existing read logic (*Typed function) → typed output.
//
// All three catalogs are auth-only (RequireJWT on /v1/* above), WITHOUT
// RequirePermission: self-describing (requiring a permission to read the list of
// permissions/types is a "chicken-and-egg" problem, architect verdict). Audit is NOT
// wired (read does not write audit). The old (w,r) is a thin strict wrapper over
// *Typed (until the final strict-method teardown).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaPermissionsList mounts GET /v1/permissions via huma on the given
// chi.Router (the group that already carries RequireJWT/maxBody/metrics). permH —
// the domain handler; nil → no-op (the opt-in-domain pattern from router.go). READ
// variant of pilot-1: huma calls ListTyped → typed output, WITHOUT audit
// middleware.
func registerHumaPermissionsList(humaAPI huma.API, permH *handlers.PermissionCatalogHandler) {
	if permH == nil {
		return
	}
	huma.Register(humaAPI, permissionsListOperation(), func(_ context.Context, _ *permissionsListInput) (*permissionsListOutput, error) {
		return &permissionsListOutput{Body: newPermissionCatalogReply(permH.ListTyped())}, nil
	})
}

// registerHumaEventTypesList mounts GET /v1/event-types via huma. eventTypeH
// nil → no-op. READ variant of pilot-1: huma calls ListTyped → typed output, no audit.
func registerHumaEventTypesList(humaAPI huma.API, eventTypeH *handlers.EventTypeCatalogHandler) {
	if eventTypeH == nil {
		return
	}
	huma.Register(humaAPI, eventTypesListOperation(), func(_ context.Context, _ *eventTypesListInput) (*eventTypesListOutput, error) {
		return &eventTypesListOutput{Body: newEventTypeCatalogReply(eventTypeH.ListTyped())}, nil
	})
}

// registerHumaHeraldTypesList mounts GET /v1/herald-types via huma. heraldTypeH
// nil → no-op. READ variant: huma calls ListTyped → typed output, no audit.
func registerHumaHeraldTypesList(humaAPI huma.API, heraldTypeH *handlers.HeraldTypeCatalogHandler) {
	if heraldTypeH == nil {
		return
	}
	huma.Register(humaAPI, heraldTypesListOperation(), func(_ context.Context, _ *heraldTypesListInput) (*heraldTypesListOutput, error) {
		return &heraldTypesListOutput{Body: newHeraldTypeCatalogReply(heraldTypeH.ListTyped())}, nil
	})
}

// registerHumaMyPermissionsList mounts GET /v1/me/permissions via huma. meH
// nil → no-op. READ variant of pilot-1: claims (AID) from ctx (RequireJWT put them
// there before humachi) → GetTyped(aid) → typed output, no audit. No claims (the
// auth chain is not assembled — a server error) → 500 problem+json (parity with the
// domain Get).
func registerHumaMyPermissionsList(humaAPI huma.API, meH *handlers.MyPermissionsHandler) {
	if meH == nil {
		return
	}
	huma.Register(humaAPI, myPermissionsListOperation(), func(ctx context.Context, _ *myPermissionsListInput) (*myPermissionsListOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing operator claims in request context")}
		}
		return &myPermissionsListOutput{Body: newMyPermissionsReply(meH.GetTyped(claims.Subject))}, nil
	})
}

// HumaCatalogSpecYAML assembles the OpenAPI fragment of the three huma-migrated READ
// catalogs (permissions / event-types / me-permissions) as a YAML string, WITHOUT
// mounting on a real router. A hook for the rollout's spec-merge target and the guard
// test. Delegates to the generic [humaDumpSpec], registering operations through the
// same register functions (a single register path — no dump-vs-mount duplication):
// handler methods are not called during dump (huma.Register does not execute them),
// so real constructors with nil deps suffice. Returns the 3.1.0 spec (huma default).
func HumaCatalogSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaPermissionsList(api, handlers.NewPermissionCatalogHandler(nil))
		registerHumaEventTypesList(api, handlers.NewEventTypeCatalogHandler(nil))
		registerHumaHeraldTypesList(api, handlers.NewHeraldTypeCatalogHandler(nil))
		registerHumaMyPermissionsList(api, handlers.NewMyPermissionsHandler(nil, nil))
		return nil
	})
}
