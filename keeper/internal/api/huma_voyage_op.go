package api

// FULL-TYPED form of the VOYAGE domain (code-first source of OpenAPI, ADR-054 §Pattern).
// BATCH 2f WRITE-SELF-AUDIT (create/cancel write audit INSIDE the handler via
// emitCreated/emitCancelled, with no audit-middleware — differing from the middleware-audit domains
// role/operator): create — scenario_run.started / command_run.invoked (202+body+Location);
// cancel — scenario_run.cancelled / command_run.cancelled (202+body). preview — dry-resolve
// with no audit (read-like POST, 200+body). list/get/targets — read (no audit). RBAC-by-kind
// (ADR-043 §6) lives INSIDE the handler (kind is visible only from the body/path) — the router wires
// only base auth + Tempo (on create/preview).

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/voyages (create) — WRITE-SELF-AUDIT scenario_run.started/command_run.invoked (202+body) ===

// voyageCreateInput — huma input POST /v1/voyages (FULL-TYPED). Body — the typed
// body: huma decodes and validates it against the schema from the huma tags of VoyageCreateRequest.
type voyageCreateInput struct {
	Body VoyageCreateRequest
}

// VoyageCreateRequest — the Go form of the POST /v1/voyages body (code-first source of the schema AND
// validation). Mirrors the domain voyageCreateRequest: the run recipe (kind/scenario_
// name|module/target/input/scheduling/batch*) + notify[]. kind-dependent validation
// (scenario↔scenario_name / command↔module, non-empty target, on_failure/batch_mode enum,
// ranges) is domain (CreateTyped → 422). additionalProperties:false (huma default) →
// unknown field → 400. kind/batch_mode/on_failure — inline enums (the spec does NOT hoist them
// as a standalone schema → the enum-alias mechanism does not apply). The struct name = the contract
// schema name (huma DefaultSchemaNamer takes reflect.Type.Name()) — aligned with the committed
// hand-written spec (rollout N3). The domain VoyageCreateRequest does not reach the spec (the huma input is
// this struct; the oapi type is not used as the huma body).
type VoyageCreateRequest struct {
	Kind         string         `json:"kind" required:"true" enum:"scenario,command" doc:"тип рецепта прогоon"`
	ScenarioName string         `json:"scenario_name,omitempty" doc:"имя сцеonрия for kind=scenario"`
	Module       string         `json:"module,omitempty" doc:"модуль for kind=command"`
	Input        map[string]any `json:"input,omitempty" doc:"параметры рецепта"`
	Target       VoyageTarget   `json:"target" required:"true" doc:"декларативный таргет (резолвится в snapshot единиц)"`

	Batch                *string    `json:"batch,omitempty" doc:"размер батча: N хостов or N%"`
	BatchSize            *int       `json:"batch_size,omitempty" minimum:"1"`
	BatchPercent         *int       `json:"batch_percent,omitempty" minimum:"1" maximum:"100"`
	Concurrency          *int       `json:"concurrency,omitempty" minimum:"1" maximum:"500"`
	BatchMode            string     `json:"batch_mode,omitempty" doc:"barrier (default) | window"`
	DryRun               bool       `json:"dry_run,omitempty"`
	ScheduleAt           *time.Time `json:"schedule_at,omitempty" doc:"RFC3339 отложенный старт"`
	InterBatchIntervalMS *int       `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int       `json:"inter_unit_interval_ms,omitempty"`

	MaxFailures   *string `json:"max_failures,omitempty" doc:"порог провалов: N абwithлют or N%"`
	FailThreshold *int    `json:"fail_threshold,omitempty" minimum:"1"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     string  `json:"on_failure,omitempty" doc:"abort | continue (default)"`

	Notify []VoyageNotify `json:"notify,omitempty" doc:"разовые подписки on ЭТОТ прогон (ephemeral)"`
}

// Nested target/notify — the single api.VoyageTarget/api.VoyageNotify (huma_voyage_target.go),
// shared with the cadence domain; the shape is aligned with the committed hand-written spec (one schema each).

// voyageCreateOutput — huma output POST /v1/voyages (FULL-TYPED). Status=202; Location —
// header; Body — the huma-native 202 body (api.VoyageCreateReply: voyage_id/kind/scope_size/
// status/location). The handler-reply (legacy-generated) → native conversion is in the register func.
type voyageCreateOutput struct {
	Status   int    `json:"-"`
	Location string `header:"Location" json:"-"`
	Body     VoyageCreateReply
}

// voyageCreateOperation — metadata for POST /v1/voyages. DefaultStatus=202. WRITE-SELF-
// AUDIT (the handler writes scenario_run.started/command_run.invoked). RBAC-by-kind — in the handler.
// Errors: 400 unknown/malformed, 403 RBAC-by-kind, 404 incarnation, 422 recipe/
// target/batch validation / empty resolve / scope-cap, 429 Tempo, 500.
func voyageCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createVoyage",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать Voyage",
		Description:   "Унифицированный батчевый прогон (ADR-043). RBAC-by-kind: scenario→incarnation.run, command→errand.run (fail-closed, в handler-е). Tempo per-AID rate-limit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// === POST /v1/voyages/preview (preview) — READ-like (no audit) dry-resolve (200+body) ===

// voyagePreviewInput — huma input POST /v1/voyages/preview (FULL-TYPED). Body — the same
// typed body as create (the same validation/resolve).
type voyagePreviewInput struct {
	Body VoyageCreateRequest
}

// voyagePreviewOutput — huma output POST /v1/voyages/preview (FULL-TYPED). Status=200;
// Body — the huma-native 200 body (api.VoyagePreviewReply: kind/scope_size/total_batches/
// batch_mode/effective_batch_size?). The conversion is in the register func.
type voyagePreviewOutput struct {
	Body VoyagePreviewReply
}

// voyagePreviewOperation — metadata for POST /v1/voyages/preview. DefaultStatus=200.
// READ-like: audit not wired (preview writes no audit event, unlike Create — it is a
// dry-resolve without creating a Voyage). RBAC-by-kind — in the handler. Errors: 400, 403, 404,
// 422 (consistent with Create — preview rejects at the same points), 429 Tempo, 500.
func voyagePreviewOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "previewVoyage",
		Method:        http.MethodPost,
		Path:          "/preview",
		Summary:       "Dry-resolve scope Voyage",
		Description:   "Предпоказ числа единиц/батчей БЕЗ withздания Voyage (ADR-043 amendment §4). Та же validation/резолв/RBAC, which Create. Без раhiddenия SID-списка. Read-like — no audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// === GET /v1/voyages (list) — READ with typed query (no audit) ===

// voyageListInput — huma input GET /v1/voyages (FULL-TYPED typed query). kind — an enum
// filter; status — multi-value enum (explode:true is MANDATORY); offset/limit — int32 with a
// default (the DOMAIN enforces the range via ParsePage/CheckPageBounds → 400, NOT a huma
// schema min/max → otherwise out-of-range would give 422 instead of the contract 400).
type voyageListInput struct {
	Kind     string   `query:"kind" enum:"scenario,command" doc:"фильтр по kind; вне enum → 422"`
	Statuses []string `query:"status,explode" enum:"scheduled,pending,running,succeeded,failed,partial_failed,cancelled" doc:"multi-value ?status=X&status=Y OR; вне enum → 422"`
	Offset   int32    `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit    int32    `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// voyageListOutput — huma output GET /v1/voyages (FULL-TYPED). Body — the huma-native envelope
// (api.VoyageListReply: items/offset/limit/total; items.$ref to native Voyage). ★ SHARED
// VoyageListReply/Voyage with cadence-runs (byte-identical dedup). The conversion is in the register func.
type voyageListOutput struct {
	Body VoyageListReply
}

// voyageListOperation — metadata for GET /v1/voyages. DefaultStatus=200. READ: audit not
// wired. Permission incarnation.history. Errors: 400 (out-of-range pagination), 403,
// 422 (bad kind/status enum), 500.
func voyageListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listVoyages",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Спиwithк Voyage-прогоbutв (paged)",
		Description:   "Спиwithк прогоbutв с фильтрами kind/status и пагиonцией (ADR-043). target_resolved NOT раскрывается (UI читает scope_size). Permission incarnation.history. Read-only, no audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/voyages/{id} (get) — READ with path (no audit) ===

// voyageGetInput — huma input GET /v1/voyages/{id}. ID — path (ULID validation is domain).
type voyageGetInput struct {
	ID string `path:"id" doc:"ULID of Voyage run"`
}

// voyageGetOutput — huma output GET /v1/voyages/{id} (FULL-TYPED). Body — the huma-native
// 200 body (api.Voyage: detail + summary; shared with list/cadence-runs). The conversion is in the register func.
type voyageGetOutput struct {
	Body Voyage
}

// voyageGetOperation — metadata for GET /v1/voyages/{id}. DefaultStatus=200. READ: audit not
// wired. Permission incarnation.history. Errors: 403, 404, 422 bad id, 500.
func voyageGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getVoyage",
		Method:        http.MethodGet,
		Path:          "/{id}",
		Summary:       "Snapshot Voyage-прогоon",
		Description:   "Detail + summary одbutго прогоon (ADR-043). Permission incarnation.history. Read-only, no audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/voyages/{id}/targets (targets) — READ with path (no audit) ===

// voyageTargetsInput — huma input GET /v1/voyages/{id}/targets. ID — path.
type voyageTargetsInput struct {
	ID string `path:"id" doc:"ULID of Voyage run"`
}

// voyageTargetsOutput — huma output GET /v1/voyages/{id}/targets (FULL-TYPED). Body —
// the huma-native 200 body (api.VoyageTargetsReply: voyage_id + targets[]). The conversion is in the register func.
type voyageTargetsOutput struct {
	Body VoyageTargetsReply
}

// voyageTargetsOperation — metadata for GET /v1/voyages/{id}/targets. DefaultStatus=200.
// READ: audit not wired. Permission incarnation.history. Errors: 403, 404 (existence
// probe), 422 bad id, 500.
func voyageTargetsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listVoyageTargets",
		Method:        http.MethodGet,
		Path:          "/{id}/targets",
		Summary:       "All-runs drill Voyage-прогоon",
		Description:   "Per-target batch/status/back-link одbutго прогоon (ADR-043). Permission incarnation.history. Read-only, no audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/voyages/{id} (cancel) — WRITE-SELF-AUDIT scenario_run.cancelled/command_run.cancelled (202+body) ===

// voyageCancelInput — huma input DELETE /v1/voyages/{id}. ID — path.
type voyageCancelInput struct {
	ID string `path:"id" doc:"ULID of Voyage run"`
}

// voyageCancelOutput — huma output DELETE /v1/voyages/{id} (FULL-TYPED). Status=202; Body —
// the huma-native 202 body (api.VoyageCancelReply: voyage_id + status:cancelled). The conversion is in the register func.
type voyageCancelOutput struct {
	Status int `json:"-"`
	Body   VoyageCancelReply
}

// voyageCancelOperation — metadata for DELETE /v1/voyages/{id}. DefaultStatus=202. WRITE-
// SELF-AUDIT (the handler writes scenario_run.cancelled/command_run.cancelled). RBAC-by-kind —
// in the handler (kind is visible from the path). Errors: 403, 404, 409 (running/terminal — not
// cancellable), 422 bad id, 500.
func voyageCancelOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "cancelVoyage",
		Method:        http.MethodDelete,
		Path:          "/{id}",
		Summary:       "Отменить Voyage-прогон",
		Description:   "Cancel pending/scheduled (running-abort — post-MVP). RBAC-by-kind в handler-е. 409 — running/terminal. Permission по kind.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
