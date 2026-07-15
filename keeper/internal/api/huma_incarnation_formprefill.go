package api

// FULL-TYPED shape of the INCARNATION form-prefill route (code-first OpenAPI source,
// ADR-054 §Pattern). POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill
// — day-2 pre-fill of the scenario's UI form with the current incarnation.state values
// (docs/input.md → "Pre-fill from state"). A resolve (not a mutation), no body: audit is NOT
// wired. RBAC incarnation.get + scope predicate (ADR-047) — on the group.
// Go types — the single source of truth for the schema.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// incFormPrefillInput — huma input for POST .../scenarios/{scenario}/form-prefill.
// Name/Scenario — path; no body. The client sets NEITHER the state path (the path-whitelist
// is built from the scenario schema on the backend) NOR the service version: the schema is always taken
// by inc.ServiceVersion (an anti-version-craft invariant, see FormPrefillTyped).
type incFormPrefillInput struct {
	Name     string `path:"name" doc:"имя инкарнации"`
	Scenario string `path:"scenario" doc:"имя сценария"`
}

// IncarnationFormPrefillReply — the native 200 body of POST .../form-prefill. Values —
// a map `field → current-value` (only prefill-declared non-secret fields with a
// covered state path; the rest are omitted). The struct name = the contract schema name.
type IncarnationFormPrefillReply struct {
	Values map[string]any `json:"values" doc:"field → текущее значение из incarnation.state (prefill-hint)"`
}

// incFormPrefillOutput — huma output for POST .../form-prefill (FULL-TYPED). Body —
// the native 200 body (IncarnationFormPrefillReply: {values}).
type incFormPrefillOutput struct {
	Body IncarnationFormPrefillReply
}

// incFormPrefillOperation — metadata for POST .../scenarios/{scenario}/form-prefill.
// DefaultStatus=200. A READ route (a resolve, not a mutation, no body): audit is NOT wired.
// Permission incarnation.get. Errors: 403 RBAC, 404 no incarnation/out of scope,
// 422 invalid name/scenario, 500.
func incFormPrefillOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "incarnationFormPrefill",
		Method:        http.MethodPost,
		Path:          "/{name}/scenarios/{scenario}/form-prefill",
		Summary:       "Pre-fill day-2-формы сценария из incarnation.state",
		Description:   "Текущие значения state под поля схемы сценария с prefill_from_state (docs/input.md). Path-whitelist (клиент путь не задаёт), secret-поля исключены. Вне RBAC-scope → 404. Permission incarnation.get. Read-only, без audit.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationFormPrefill mounts POST .../scenarios/{scenario}/
// form-prefill via huma (READ, no body, no audit). scope predicate (ADR-047,
// action=get) → out of scope 404. incH nil → no-op.
func registerHumaIncarnationFormPrefill(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incFormPrefillOperation(), func(ctx context.Context, in *incFormPrefillInput) (*incFormPrefillOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		res, err := incH.FormPrefillTyped(ctx, in.Name, in.Scenario, incH.GetInScopeFor(claims, "get"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incFormPrefillOutput{Body: IncarnationFormPrefillReply{Values: coalesceFormPrefillValues(res.Values)}}, nil
	})
}

// coalesceFormPrefillValues guarantees a non-nil map in the wire (`values: {}` instead of
// `null` when there are no prefill fields) — a stable contract for the UI form.
func coalesceFormPrefillValues(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
