package api

// FULL-TYPED shape of GET /v1/cadences (code-first OpenAPI source, ADR-054 §Pattern
// FOURTH tier — read with typed query). Go types are the single source of truth:
// huma builds from them BOTH the JSON Schema OpenAPI fragment (query params with types/
// bounds/enum), the typed-bind input, AND the typed output. Teardown T1 completes the
// migration of the cadence domain to huma (the last live strict-mount in /v1).
//
// KEY tier invariant (contract preserved, parity with legacy List): bad-int
// offset/limit → 400 TypeMalformedRequest (huma parseInto failure → error-override
// hasQueryParseError); out-of-range offset/limit → 400 (domain CheckPageBounds, NOT
// huma min/max); bad enabled/kind enum → 422 TypeValidationFailed (schema-validate
// enum-mismatch — a different Message, not in the parse set; exactly like leglist).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// cadenceListInput — huma input for GET /v1/cadences (FULL-TYPED typed-query). EVERY
// field carries a `query:"<name>"` tag → huma binds from url.Values and validates by the
// schema from the tags. GET has NO RequestBody (huma emits no body without a Body field).
//
// Bind-phase semantics (parity with legacy CadenceHandler.List → ListTyped):
//   - Enabled — string with enum:"true,false": empty (omitted) → do not apply the filter;
//     "true" → only enabled; "false" → no filter (show all). A value outside the set
//     → 422 (schema-validate enum-mismatch → error-override TypeValidationFailed,
//     exactly like legacy "query 'enabled' must be 'true' or 'false'"). NOT *bool: the
//     legacy contract returns 422 on a bad value, whereas *bool parsing would give 400.
//   - Kind — string with enum:"scenario,command": exact-match on the recipe kind; outside
//     the set → 422 (parity with legacy ValidKind → 422). Omitted → no filter.
//   - Offset/Limit — int32 (NOT Go int: huma emits format:int64 for int, the committed
//     spec carries int32; pagination fits in int32) with `default` (offset 0, limit 50)
//     matching shared/api.ParsePage. bad-int (non-numeric) → 400 (parseInto).
//     The range BOUNDS (offset≥0, limit∈[1,1000]) are DELIBERATELY not expressed via huma
//     minimum/maximum tags: huma would reject out-of-range as 422, whereas legacy/
//     ParsePage returns exactly 400 (TypeMalformedRequest). The domain ListTyped enforces
//     the range via CheckPageBounds → 400 — otherwise a wire change.
type cadenceListInput struct {
	Enabled string `query:"enabled" enum:"true,false" doc:"фильтр по enabled: true → только включённые, false → все (без фильтра); опущен → все"`
	Kind    string `query:"kind" enum:"scenario,command" doc:"фильтр по типу рецепта (exact); вне набора → 422"`
	Offset  int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// cadenceListOutput — huma output for GET /v1/cadences (FULL-TYPED). Body is the typed
// 200 envelope (handlers.CadenceListReply = sharedapi.PagedResponse[cadenceDTO], the same
// {items,offset,limit,total} envelope the legacy writeJSON returned). The wire shape
// (items non-nil []) is pinned by a golden-JSON byte-exact test (the main guard).
type cadenceListOutput struct {
	Body handlers.CadenceListReply
}

// cadenceListOperation — metadata for GET /v1/cadences. Path = "/" RELATIVE to the
// chi group /v1/cadences (huma.API is mounted on it; chi.Walk sees /v1/cadences,
// drift-test green). DefaultStatus=200. READ route: audit not wired. Permission
// cadence.list (read tier — like legacy strict ListCadences). Errors: 400 (bad int /
// out-of-range pagination), 403 RBAC, 422 (bad enabled/kind enum), 500 (DB).
func cadenceListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listCadences",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список расписаний (Cadence, paged)",
		Description:   "Read-only-список Cadence с фильтрами enabled/kind и пагinацией (sort created_at DESC). Permission cadence.list. Read-only, без audit.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
