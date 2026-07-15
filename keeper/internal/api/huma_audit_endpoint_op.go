package api

// FULL-TYPED shape of GET /v1/audit (code-first OpenAPI source, ADR-054 §Pattern
// the FOURTH tier — read with typed query). Go types are the single source of truth:
// huma builds from them BOTH the JSON Schema of the OpenAPI fragment (query params with types/
// bounds/enum), AND the typed-bind of the input, AND the typed output. The REFERENCE for ~13-15
// list endpoints with a typed query.
//
// The KEY invariant of the tier (contract preserved, decision A 2026-06-13): a bad-value
// typed-query (started_after/before date-time, offset/limit int) → 400
// TypeMalformedRequest (huma parseInto failure → error-override hasQueryParseError); a bad
// source-enum → 422 TypeValidationFailed (schema-validate enum mismatch — a different
// Message, not in the parse set). This continues ADR-051 Amendment (the strict
// bind phase gave the same 400/422), without a product fork.

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// auditListInput — the huma-input of GET /v1/audit (FULL-TYPED typed-query). EVERY field
// carries a `query:"<name>"` tag → huma binds from url.Values and validates against the schema
// from the tags. GET has NO RequestBody (huma emits no body without a Body field).
//
// Bind-phase semantics (parity with the legacy AuditHandler.List → ListTyped):
//   - Types/Sources — multi-value (`?type=X&type=Y`), OR semantics in the domain
//     ListTyped (event_type/source IN (...)); an empty slice → do not apply the filter.
//     `query:"…,explode"` is REQUIRED on these []string fields: the huma default for a query
//     array is explode=false (reads comma-separated `?source=a,b` AS ONE "a,b"
//     and emits `explode: false` in the spec → the generated client encodes a composite
//     value → a broken OR filter). `,explode` → huma reads a repeated
//     key `?source=a&source=b` AND emits `explode: true` (huma v2.38 huma.go:157,
//     openapi.go Param.Explode); matches the committed spec (style:form +
//     explode:true) — matches the rollout multi-value contract;
//   - Sources carries `enum:"…"` — huma rejects a value outside the set with 422
//     (schema-validate, NOT parseInto) → error-override classifies it as
//     TypeValidationFailed (the KEY contract invariant: enum→422, not 400). The
//     enum set = the FULL domain valid-set audit.Source.Valid() (signal/api/mcp/
//     keeper_internal/soul_grpc/background/config_bootstrap): config_bootstrap
//     is actually emitted (push/auto_import.go) and accepted by the domain → dropping it
//     would reject the working filter `?source=config_bootstrap` with 422 (wire regression
//     200→422). The enum tag syncs WITH THE DOMAIN, the committed spec follows;
//   - StartedAfter/StartedBefore — time.Time: huma parseInto on a bad value gives
//     "invalid date/time for format …" → 400 (hasQueryParseError); zero-time
//     (parameter omitted) → the domain filter applies no bound (filter.IsZero);
//   - Offset/Limit — int32 (NOT Go int: huma emits format:int64 for int, the committed
//     spec/OffsetQuery/LimitQuery carry int32; pagination fits in int32) with
//     `default` (offset 0, limit 50) matching shared/api.
//     ParsePage. A bad int (non-numeric) → 400 (parseInto). The range BOUNDS (offset≥0,
//     limit∈[1,1000]) are NOT expressed via huma `minimum`/`maximum` tags DELIBERATELY: huma
//     would reject out-of-range with 422 (schema-validate), while the legacy/strict contract
//     gives exactly 400 for limit=0/1001/offset<0 (ParsePage TypeMalformedRequest). The range is
//     enforced by the DOMAIN ListTyped with the same ParsePage message → 400 — otherwise a
//     wire change. The range is documented via `doc:` (not via schema-min/max).
type auditListInput struct {
	Types         []string  `query:"type,explode" doc:"multi-value ?type=X&type=Y — exact-match OR по event_type"`
	Sources       []string  `query:"source,explode" enum:"signal,api,mcp,keeper_internal,soul_grpc,background,config_bootstrap" doc:"multi-value ?source=api&source=mcp — exact-match OR; значение вне enum → 422"`
	ArchonAID     string    `query:"archon_aid" doc:"AID Архонта-инициатора (case-insensitive substring, ILIKE)"`
	CorrelationID string    `query:"correlation_id" doc:"ULID цепочки связанных событий (case-insensitive substring, ILIKE)"`
	PayloadHerald string    `query:"payload_herald" doc:"имя Herald-канала из payload->>'herald' (exact match)"`
	PayloadVoyage string    `query:"payload_voyage" doc:"voyage_id из payload->>'voyage_id' (exact match)"`
	StartedAfter  time.Time `query:"started_after" doc:"created_at >= started_after (RFC3339, включающая); bad-value → 400"`
	StartedBefore time.Time `query:"started_before" doc:"created_at <= started_before (RFC3339, включающая); bad-value → 400"`
	Offset        int32     `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit         int32     `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// auditListOutput — the huma-output of GET /v1/audit (FULL-TYPED). Body — the native 200 body
// (AuditEventListReply, the same envelope {items,offset,limit,total} the legacy
// writeJSON returned). Projection of the domain handlers.AuditListPage → native is on the register-func
// boundary (newAuditEventListReply). The wire shape (items non-nil [], Source native enum type, created_at
// second-precision) is pinned by a golden-JSON byte-exact test (the tier's main guard).
type auditListOutput struct {
	Body AuditEventListReply
}

// auditListOperation — metadata for GET /v1/audit. Path = "/audit" relative to the
// chi group /v1 (huma.API is mounted on it; chi.Walk sees /v1/audit, drift-test
// green — absolute, like permissionsListOperation, so a distinct path excludes
// an operation collision on the shared /v1 API). DefaultStatus=200. READ route: audit not wired
// (reading audit_log spawns no audit event itself — recursion). Errors: 400 (bad
// typed-query bind), 422 (bad source-enum / out-of-range pagination), 500 (DB).
func auditListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listAuditEvents",
		Method:        http.MethodGet,
		Path:          "/audit",
		Summary:       "Лента audit-events (paged + фильтры)",
		Description:   "Read-only-лента audit_log с фильтрами (type/source multi-OR, archon_aid/correlation_id case-insensitive substring ILIKE, payload_herald/payload_voyage exact, started_after/before RFC3339) и пагинацией. Permission audit.read. Read-only, без audit (чтение не пишется — рекурсия).",
		Tags:          []string{"audit"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
