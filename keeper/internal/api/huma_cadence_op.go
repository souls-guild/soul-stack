package api

// FULL-TYPED shape of POST /v1/cadences (code-first source of OpenAPI, ADR-054
// Amendment 2026-06-12, §Pattern (b) thin envelope). Go types are the single
// source of truth: huma builds from them both the JSON Schema OpenAPI fragment and input
// validation (required/enum/additionalProperties:false HONEST) and the typed output.
// There is no more RawBody bridge — huma validates the typed Body natively (§Invariant-2):
// unknown→400 (error-override detects "unexpected property"), required/enum→422.

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// cadenceCreateInput — huma input for POST /v1/cadences (FULL-TYPED). Body is
// the typed body: huma decodes and validates it against the schema from the huma tags of
// CadenceCreateRequest. Conversion to the domain model happens in registerHumaCadence.
type cadenceCreateInput struct {
	Body CadenceCreateRequest
}

// CadenceCreateRequest — Go shape of the POST /v1/cadences body (code-first source of
// schema AND validation). Mirrors the domain run recipe (voyage parity) + the repetition
// rule + overlap_policy + notify[]. The struct name = the contract schema name in
// OpenAPI (huma DefaultSchemaNamer takes reflect.Type.Name()) — aligned with the
// committed hand-written spec (docs/keeper/openapi.yaml → CadenceCreateRequest, N4).
//
// huma tags: `required:"true"` — a required field (missing → 422); `enum:"…"` —
// allowed values (mismatch → 422); `doc:"…"` — description. omitempty/pointer —
// optional fields. additionalProperties:false (huma default, NOT removed) →
// unknown field → error-override classifies it as 400 (cluster contract).
type CadenceCreateRequest struct {
	Name         string `json:"name" required:"true" doc:"human-readable schedule name"`
	Enabled      *bool  `json:"enabled,omitempty" doc:"enable/disable scheduler (default true)"`
	ScheduleKind string `json:"schedule_kind" required:"true" enum:"interval,cron" doc:"schedule type"`

	IntervalSeconds *int   `json:"interval_seconds,omitempty" minimum:"30" doc:"period for schedule_kind=interval (minimum 30s — absolute poll_floor, ADR-046/048)"`
	CronExpr        string `json:"cron_expr,omitempty" doc:"cron expression for schedule_kind=cron"`
	OverlapPolicy   string `json:"overlap_policy" required:"true" enum:"skip,queue,parallel" doc:"overlap policy for runs"`

	Kind         string         `json:"kind" required:"true" enum:"scenario,command" doc:"run recipe type"`
	ScenarioName string         `json:"scenario_name,omitempty" doc:"scenario name for kind=scenario"`
	Module       string         `json:"module,omitempty" doc:"module for kind=command"`
	Input        map[string]any `json:"input,omitempty" doc:"recipe parameters"`
	Target       VoyageTarget   `json:"target" required:"true" doc:"run target (resolved on spawn)"`

	Batch        *string `json:"batch,omitempty" doc:"batch size: N hosts/incarnations or N%"`
	BatchSize    *int    `json:"batch_size,omitempty" minimum:"1"`
	BatchPercent *int    `json:"batch_percent,omitempty" minimum:"1" maximum:"100"`
	Concurrency  *int    `json:"concurrency,omitempty" minimum:"1"`
	BatchMode    string  `json:"batch_mode,omitempty"`

	MaxFailures          *string `json:"max_failures,omitempty" doc:"failure threshold: N absolute or N%"`
	FailThreshold        *int    `json:"fail_threshold,omitempty" minimum:"1"`
	InterBatchIntervalMS *int    `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int    `json:"inter_unit_interval_ms,omitempty"`
	RequireAlive         *bool   `json:"require_alive,omitempty"`
	OnFailure            string  `json:"on_failure,omitempty"`

	Notify []VoyageNotify `json:"notify,omitempty" doc:"subscriptions to run notifications for this schedule"`
}

// Nested target/notify — the single api.VoyageTarget/api.VoyageNotify (huma_voyage_target.go),
// shared with the voyage domain; the shape is aligned with the committed hand-written spec (one schema each).

// cadenceCreateOutput — huma output (FULL-TYPED). Status=201; Location — header;
// Body — the typed 201 body. Conversion of the domain cadenceCreateReply → this type is in
// registerHumaCadence. Replaces the former empty output + manual write to (w).
type cadenceCreateOutput struct {
	Status   int                `json:"-"`
	Location string             `header:"Location" json:"-"`
	Body     CadenceCreateReply `json:"-"`
}

// CadenceCreateReply — Go shape of the 201 body (source of the response schema AND wire form).
// Matches the domain CadenceCreateReply: all scalars; NextRunAt nullable
// (*time.Time → RFC3339Nano on marshal, like the legacy oapi reply — wire-identical).
// The struct name = the contract schema name (huma DefaultSchemaNamer; hand-written spec
// CadenceCreateReply, N4). omitempty/nullable are pinned by a golden-JSON snapshot
// test (the rollout's wire-regression guard).
type CadenceCreateReply struct {
	CadenceID string     `json:"cadence_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"ULID of created schedule"` // ULID (audit.NewULID)
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	NextRunAt *time.Time `json:"next_run_at,omitempty" doc:"RFC3339 time of next run"`
	Location  string     `json:"location" doc:"relative resource URL"`
}

// cadenceCreateOperation — huma.Operation metadata for POST /v1/cadences.
// huma derives RequestBody/Responses AUTOMATICALLY from cadenceCreateInput.Body /
// cadenceCreateOutput at huma.Register (FULL-TYPED — schema and validation from the same
// Go types). Path = "/" — RELATIVE to the chi group /v1/cadences on which
// huma.API is mounted (chi mounts the route as /v1/cadences; chi.Walk sees it,
// drift-test green). DefaultStatus=201 — the success code (huma takes it from
// output.Status, but we pin it in the schema too). Errors pins the problem codes
// (400 unknown/malformed, 403 RBAC-by-kind, 422 recipe/schedule validation, 500).
func cadenceCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createCadence",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Create schedule (Cadence)",
		Description:   "Regular/recurring Voyage (ADR-046). Two-tier RBAC: cadence.create + Voyage-permission by recipe kind.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PATCH /v1/cadences/{id} (patch) — WRITE-SELF-AUDIT cadence.updated (200+body) ===
//
// PATCH semantics — read-modify-write over the full-replace cadence.Update: a given
// field → overwrite, an omitted one → the current value is kept. Pointers to the
// recipe's nullable fields semantically "do not distinguish" "omitted" from "explicit null" — for
// the MVP, PATCH treats the PRESENCE of a key as an overwrite (absence → keep). So the
// huma shape is `*T omitempty` (NOT the Optional[T] presence-tier of huma_optional.go):
// distinguishing omitted/null/value is NOT required (a nil pointer = "leave alone", exact
// parity with the domain cadencePatchRequest). kind does NOT change in PATCH (the field is absent
// from the body — changing kind = delete + create).

// cadencePatchInput — huma input for PATCH /v1/cadences/{id} (FULL-TYPED). ID — path
// (ULID validation is domain-side, in PatchTyped). Body — the typed PATCH body.
type cadencePatchInput struct {
	ID   string `path:"id" doc:"ULID of schedule"`
	Body CadencePatchRequest
}

// CadencePatchRequest — Go shape of the PATCH /v1/cadences/{id} body (code-first source of
// schema AND validation). All fields are optional (omitempty pointer): presence → set,
// absence → keep. enum is a closed set (mismatch → 422). additionalProperties:false
// (huma default) → unknown field → 400. Mirrors the domain cadencePatchRequest. The struct
// name = the contract schema name (huma DefaultSchemaNamer; hand-written spec
// CadencePatchRequest, N4).
type CadencePatchRequest struct {
	Name            *string `json:"name,omitempty" doc:"human-readable schedule name"`
	Enabled         *bool   `json:"enabled,omitempty" doc:"enable/disable scheduler"`
	ScheduleKind    *string `json:"schedule_kind,omitempty" enum:"interval,cron" doc:"schedule type"`
	IntervalSeconds *int    `json:"interval_seconds,omitempty" minimum:"30" doc:"period for schedule_kind=interval (minimum 30s — absolute poll_floor, ADR-046/048)"`
	CronExpr        *string `json:"cron_expr,omitempty" doc:"cron expression (empty string → clear)"`
	OverlapPolicy   *string `json:"overlap_policy,omitempty" enum:"skip,queue,parallel" doc:"overlap policy for runs"`

	ScenarioName *string        `json:"scenario_name,omitempty" doc:"scenario name (empty string → clear)"`
	Module       *string        `json:"module,omitempty" doc:"module for kind=command (empty string → clear)"`
	Input        map[string]any `json:"input,omitempty" doc:"recipe parameters"`
	Target       *VoyageTarget  `json:"target,omitempty" doc:"run target"`

	Batch         *string `json:"batch,omitempty" doc:"batch size: N hosts/incarnations or N%"`
	BatchSize     *int    `json:"batch_size,omitempty" minimum:"1"`
	BatchPercent  *int    `json:"batch_percent,omitempty" minimum:"1" maximum:"100"`
	Concurrency   *int    `json:"concurrency,omitempty" minimum:"1"`
	BatchMode     *string `json:"batch_mode,omitempty"`
	MaxFailures   *string `json:"max_failures,omitempty" doc:"failure threshold: N absolute or N%"`
	FailThreshold *int    `json:"fail_threshold,omitempty" minimum:"1"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     *string `json:"on_failure,omitempty" doc:"abort|continue (empty string → clear)"`
}

// cadencePatchOutput — huma output for PATCH /v1/cadences/{id} (FULL-TYPED). Status=200;
// Body — the typed 200 body (the full cadenceDTO of the updated schedule).
type cadencePatchOutput struct {
	Body handlers.CadenceDTO
}

// cadencePatchOperation — metadata for PATCH /v1/cadences/{id}. DefaultStatus=200.
// WRITE-SELF-AUDIT: cadence.updated is written by the handler ITSELF (PatchTyped → emitWrite), the audit
// middleware is NOT wired. Errors: 400 unknown/malformed, 403 RBAC, 404 cadence_not_found,
// 422 recipe/schedule validation, 500.
func cadencePatchOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "patchCadence",
		Method:        http.MethodPatch,
		Path:          "/{id}",
		Summary:       "Update schedule (Cadence)",
		Description:   "Read-modify-write recipe/schedule/enabled-toggle. Two-tier RBAC (cadence.update + Voyage-permission by kind). kind does not change.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/cadences/{id} (delete) — WRITE-SELF-AUDIT cadence.deleted (204) ===

// cadenceDeleteInput — huma input for DELETE /v1/cadences/{id}. ID — path. No body.
type cadenceDeleteInput struct {
	ID string `path:"id" doc:"ULID of schedule"`
}

// cadenceDeleteOutput — huma output for DELETE /v1/cadences/{id} (FULL-TYPED). Status=204;
// no body (Body not declared — huma 204 without content).
type cadenceDeleteOutput struct {
	Status int `json:"-"`
}

// cadenceDeleteOperation — metadata for DELETE /v1/cadences/{id}. DefaultStatus=204.
// WRITE-SELF-AUDIT: cadence.deleted is written by the handler ITSELF (DeleteTyped → emitDeleted).
// Errors: 403 RBAC, 404 cadence_not_found, 422 invalid id, 500.
func cadenceDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteCadence",
		Method:        http.MethodDelete,
		Path:          "/{id}",
		Summary:       "Delete schedule (Cadence)",
		Description:   "Deletes schedule; spawned Voyages remain (FK ON DELETE SET NULL). Permission cadence.delete.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/cadences/{id}/enable|/disable (toggle) — WRITE-SELF-AUDIT cadence.updated (200+body) ===

// cadenceToggleInput — huma input for POST /v1/cadences/{id}/enable|/disable. ID — path.
// No body (toggle has no body).
type cadenceToggleInput struct {
	ID string `path:"id" doc:"ULID of schedule"`
}

// cadenceToggleOutput — huma output for enable/disable (FULL-TYPED, handler-native T5d). Status=200;
// Body — the huma-native 200 body (api.CadenceEnabledReply: cadence_id + enabled). Conversion of the domain
// handlers.CadenceEnabledReply → this type is in the register-func.
type cadenceToggleOutput struct {
	Body CadenceEnabledReply
}

// CadenceEnabledReply — Go shape of the 200 body of POST /v1/cadences/{id}/enable|/disable (source of
// the schema AND wire form, handler-native T5d). Flat shape 1:1 with the former CadenceEnabledReply
// (cadence_id + enabled). The struct name = the contract schema name (huma DefaultSchemaNamer).
type CadenceEnabledReply struct {
	CadenceID string `json:"cadence_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	Enabled   bool   `json:"enabled"`
}

// cadenceEnableOperation — metadata for POST /v1/cadences/{id}/enable. DefaultStatus=200.
// WRITE-SELF-AUDIT: cadence.updated is written by the handler ITSELF (SetEnabledTyped → emitEnabledToggle).
// Errors: 403 RBAC, 404 cadence_not_found, 422 invalid id, 500.
func cadenceEnableOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "enableCadence",
		Method:        http.MethodPost,
		Path:          "/{id}/enable",
		Summary:       "Enable schedule (Cadence)",
		Description:   "Resume scheduler. Permission cadence.enable OR backcompat cadence.update.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// cadenceDisableOperation — metadata for POST /v1/cadences/{id}/disable. DefaultStatus=200.
// WRITE-SELF-AUDIT: cadence.updated is written by the handler ITSELF. Errors: 403, 404, 422, 500.
func cadenceDisableOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "disableCadence",
		Method:        http.MethodPost,
		Path:          "/{id}/disable",
		Summary:       "Disable schedule (Cadence)",
		Description:   "Pause scheduler. Permission cadence.disable OR backcompat cadence.update.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/cadences/{id} (get) — READ with path (no audit) ===
//
// Moving the read routes GET/{id}+/runs to huma completes the cadence domain entirely and
// LIFTS the sibling-subrouter blocker r.Route("/{id}") (chi handed the WHOLE /{id} node
// to the strict subrouter → the PATCH/DELETE huma ops were unreachable, 405). Now all
// /{id} routes are huma ops with a full path relative to the /v1/cadences group, without
// a chi.Route on the same node.

// cadenceGetInput — huma input for GET /v1/cadences/{id}. ID — path (ULID validation is
// domain-side, in GetTyped).
type cadenceGetInput struct {
	ID string `path:"id" doc:"ULID of schedule"`
}

// cadenceGetOutput — huma output for GET /v1/cadences/{id} (FULL-TYPED). Body — the typed
// 200 body (the full cadenceDTO). Wire form byte-exact with legacy GET {id}.
type cadenceGetOutput struct {
	Body handlers.CadenceDTO
}

// cadenceGetOperation — metadata for GET /v1/cadences/{id}. DefaultStatus=200. READ route:
// audit is NOT wired. Permission cadence.list (read-tier — like the legacy strict GetCadence).
// Errors: 403 RBAC, 404 cadence_not_found, 422 invalid id, 500.
func cadenceGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getCadence",
		Method:        http.MethodGet,
		Path:          "/{id}",
		Summary:       "Get schedule (Cadence)",
		Description:   "Schedule detail by ULID. Permission cadence.list. Read-only, no audit.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/cadences/{id}/runs (runs) — READ with typed query (no audit) ===

// cadenceRunsInput — huma input for GET /v1/cadences/{id}/runs (FULL-TYPED typed-query).
// ID — path (ULID). Statuses — multi-value (?status=X&status=Y) exact-match OR;
// the enum set = the full voyage.Status domain (a value outside it → 422). explode:true
// is MANDATORY (huma default for query arrays is explode=false: comma-separated as one
// value → broken OR). offset/limit — int32 with default; the range is enforced by
// CheckPageBounds in RunsTyped → 400 (NOT huma min/max — parity with legacy ParsePage); bad-int → 400.
type cadenceRunsInput struct {
	ID       string   `path:"id" doc:"ULID of schedule"`
	Statuses []string `query:"status,explode" enum:"scheduled,pending,running,succeeded,failed,partial_failed,cancelled" doc:"multi-value ?status=X&status=Y — exact-match OR; value outside enum → 422"`
	Offset   int32    `query:"offset" default:"0" doc:"offset from start of set, ≥0 (matches shared/api.ParsePage; out-of-range → 400)"`
	Limit    int32    `query:"limit" default:"50" doc:"page size 1..1000 (matches shared/api.ParsePage; out-of-range → 400)"`
}

// cadenceRunsOutput — huma output for GET /v1/cadences/{id}/runs (FULL-TYPED). Body —
// the typed 200 envelope (handlers.CadenceRunsReply: items/offset/limit/total). Wire form
// byte-exact with legacy (sharedapi.PagedResponse[voyageDTO]).
type cadenceRunsOutput struct {
	Body handlers.CadenceRunsReply
}

// cadenceRunsOperation — metadata for GET /v1/cadences/{id}/runs. DefaultStatus=200.
// READ route: audit is NOT wired. Permission incarnation.history (a Voyage is
// incarnation history, like the legacy strict ListCadenceRuns). Errors: 400 out-of-range pagination,
// 403 RBAC, 404 cadence_not_found, 422 bad id/status enum, 500.
func cadenceRunsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listCadenceRuns",
		Method:        http.MethodGet,
		Path:          "/{id}/runs",
		Summary:       "Schedule runs (Cadence runs, paged)",
		Description:   "List of Voyages spawned by schedule, with status[] filter and pagination. Permission incarnation.history. Read-only, no audit.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
