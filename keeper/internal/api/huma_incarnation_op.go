package api

// FULL-TYPED form of the INCARNATION domain (code-first OpenAPI source, ADR-054 §Pattern,
// batch 2g). Go types are the single source of truth: huma builds from them the JSON Schema
// of the OpenAPI fragment, input validation (required/enum/additionalProperties:false HONEST)
// and typed output. MIXED domain — TWO classes of audit:
//
//   - MIDDLEWARE-AUDIT (create / run / unlock / upgrade): huma-audit-middleware writes the
//     event OUTSIDE (variant B). The registerHuma* func sets the payload via
//     SetHumaAuditPayload from the *Typed reply.AuditPayload.
//   - SELF-AUDIT (rerun-last / check-drift / destroy / update-hosts): the handler ITSELF
//     writes audit INSIDE *Typed; audit-middleware is not wired (newHumaCadenceAPI).
//
// All incarnation huma ops carry the FULL path /{name}[/...] relative to the group
// /v1/incarnations (chi.Route("/{name}") REMOVED — otherwise sibling-shadowing → 405, the
// blocker of batch 2f cadence). Coexists with the choir-mount (batch 2f) on the same group.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
)

// === POST /v1/incarnations (create) — MIDDLEWARE-AUDIT incarnation.created (202+body) ===

// incCreateInput — huma input for POST /v1/incarnations (FULL-TYPED). Body — typed body.
type incCreateInput struct {
	Body IncarnationCreateRequest
}

// IncarnationCreateRequest — Go form of the POST /v1/incarnations body. name/service
// required; covens/input optional. Format of name/service/coven — domain validation
// (422 in CreateTyped). additionalProperties:false (huma default) → unknown field → 400.
// Struct name = contract schema name in OpenAPI (huma DefaultSchemaNamer takes
// reflect.Type.Name() directly) — aligned to the committed hand-written spec (T4b pilot).
type IncarnationCreateRequest struct {
	Name    string         `json:"name" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"new instance name (kebab-case), root Coven tag"`
	Service string         `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"service name from registry (ADR-029)"`
	Covens  []string       `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"declared environment tags (ADR-008 amendment a)"`
	Input   map[string]any `json:"input,omitempty" doc:"input for selected create scenario"`
	// Traits — operator-set trait labels of the incarnation (ADR-060 amend R1): map key →
	// scalar | list of scalars. Stored in incarnation.traits (source of truth) and
	// materialized into souls.traits of member hosts. Format/value is validated by the domain
	// (nested object/array → 422). Operational replacement — PUT .../traits.
	Traits map[string]any `json:"traits,omitempty" doc:"operator-set trait labels (key → scalar|list of scalars), ADR-060"`
	// CreateScenario — choice of the start scenario (mechanism of multiple create scenarios).
	// Optional. Empty-choice contract (Phase 2, union removed): a service offering create
	// scenarios (scenario with `create: true`) + empty → 422 create_scenario_required; a
	// service without them + empty → bare incarnation (ready without a run, created_scenario=
	// NULL). Auto-create by the default `create` is gone. A non-empty name must belong to the
	// service's create set, otherwise 422; the choice is saved in incarnation.created_scenario
	// (rerun-last uses it on the create path).
	CreateScenario string `json:"create_scenario,omitempty" pattern:"^[a-z][a-z0-9_]*$" doc:"name of start scenario (mechanism for multiple creates, scenario with create:true). Empty: service offers create scenarios → 422 create_scenario_required; service without them → bare incarnation (ready without run)"`
}

// incCreateOutput — huma output for POST /v1/incarnations (FULL-TYPED). Status=202;
// Body — native 202 body (IncarnationCreateReply: incarnation + optional apply_id).
type incCreateOutput struct {
	Status int `json:"-"`
	Body   IncarnationCreateReply
}

func incCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createIncarnation",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Create incarnation",
		Description:   "Service runtime instance (ADR-029). Runs create scenario (async, if lifecycle.auto_create). Permission incarnation.create.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations (list) — READ with typed query (no audit) ===

// incListInput — huma input for GET /v1/incarnations (FULL-TYPED typed query). offset/limit
// — int32 with default; the range is enforced by CheckPageBounds in ListTyped → 400 (parity
// ParsePage). The other filters are string/enum (422 validation done by ListTyped). state
// filters `state.<field>` huma does NOT bind as typed parameters (dynamic keys) — the caller
// passes them from the original query (see registerHumaIncarnationList).
type incListInput struct {
	Offset  int32  `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
	Service string `query:"service" doc:"filter by service name"`
	Status  string `query:"status" doc:"filter by status (ready/applying/error_locked/migration_failed); invalid → 422"`
	Coven   string `query:"coven" doc:"exact-match by covens[] (ADR-008); invalid label → 422"`
	SortBy  string `query:"sort" doc:"sort field (created_at/name/status/service or state.<field>)"`
	SortDir string `query:"sort_dir" doc:"sort direction (asc/desc)"`
}

// incListOutput — huma output for GET /v1/incarnations (FULL-TYPED). Body — TAGGED native
// envelope incarnationListReply (items.$ref to native IncarnationGetReply with json tags:
// snake_case wire). Previously Body was handlers.IncarnationListReply (= PagedResponse[
// IncarnationGetView]) — untagged View → PascalCase wire (contract bug #7). The register func
// projects reply.Items through newIncarnationGetReply. The OpenAPI schema is unchanged (same
// alias target incarnationListReply).
type incListOutput struct {
	Body incarnationListReply
}

func incListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listIncarnations",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "List incarnations (paged)",
		Description:   "Filters service/status/coven/state.<field> + sorting. Visibility scoped by RBAC (ADR-047). Permission incarnation.list. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name} (get) — READ with path (no audit) ===

// incGetInput — huma input for GET /v1/incarnations/{name}. Name — path.
type incGetInput struct {
	Name string `path:"name" doc:"incarnation name"`
}

// incGetOutput — huma output for GET /v1/incarnations/{name} (FULL-TYPED). Body — full
// native IncarnationGetReply (byte-exact with legacy GET {name}).
type incGetOutput struct {
	Body IncarnationGetReply
}

func incGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnation",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Get incarnation",
		Description:   "Service runtime instance detail. Outside RBAC scope -> 404 (does not leak existence). Permission incarnation.get. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/history (history) — READ with typed query (no audit) ===

// incHistoryInput — huma input for GET /v1/incarnations/{name}/history. Name — path;
// apply_id — optional ULID filter (bad → 400 in HistoryTyped); offset/limit — int32 with
// default (out-of-range → 400).
type incHistoryInput struct {
	Name    string `path:"name" doc:"incarnation name"`
	ApplyID string `query:"apply_id" doc:"opt. ULID filter by state_history.apply_id; non-ULID → 400"`
	Offset  int32  `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// incHistoryOutput — huma-output GET /v1/incarnations/{name}/history (FULL-TYPED). Body
// — TAGGED native envelope incarnationHistoryReply (items.$ref to native StateHistoryEntry
// with json tags: snake_case wire). Previously Body was handlers.IncarnationHistoryReply (=
// PagedResponse[StateHistoryView]) — untagged View → PascalCase wire (contract bug #7).
// The register func projects reply.Items through newStateHistoryEntry. The OpenAPI schema is
// unchanged (same alias target incarnationHistoryReply).
type incHistoryOutput struct {
	Body incarnationHistoryReply
}

func incHistoryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationHistory",
		Method:        http.MethodGet,
		Path:          "/{name}/history",
		Summary:       "Incarnation state transition history (paged)",
		Description:   "state_history with apply_id filter and pagination. Outside RBAC scope -> 404. Permission incarnation.history. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/runs (runs) — READ-with-typed-query (NO audit) ===
//
// List of incarnation runs (apply_runs folded by apply_id) + per-run details below.
// Closes the UI bug apply_id→/voyages/ 404: an incarnation run (apply_run) is NOT a Voyage,
// it has its own read-view. The scope gate is the same inScope predicate as History (action=
// incarnation.history): the endpoints live in the incarnation domain, per-{name} scope in-handler.

// incRunsInput — huma-input GET /v1/incarnations/{name}/runs. Name — path; offset/limit
// — int32 with a default (out-of-range → 400 in RunsTyped).
type incRunsInput struct {
	Name   string `path:"name" doc:"incarnation name"`
	Offset int32  `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32  `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// incRunsOutput — huma-output GET .../runs (FULL-TYPED). Body — TAGGED native envelope
// incarnationRunsReply (items.$ref to native RunSummaryEntry: snake_case wire).
type incRunsOutput struct {
	Body incarnationRunsReply
}

func incRunsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listIncarnationRuns",
		Method:        http.MethodGet,
		Path:          "/{name}/runs",
		Summary:       "List incarnation runs (paged)",
		Description:   "Fold apply_runs by apply_id: run status (applying/success/failed/cancelled), time bounds, initiator. Run (apply_run) - NOT Voyage. Outside RBAC scope -> 404. Permission incarnation.history. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/runs/{apply_id} (run detail) — READ-with-path (NO audit) ===

// incRunDetailInput — huma-input GET .../runs/{apply_id}. Name/ApplyID — path. The
// apply_id format (ULID) is validated by RunDetailTyped → 400 (non-ULID), symmetric with the History filter.
type incRunDetailInput struct {
	Name    string `path:"name" doc:"incarnation name"`
	ApplyID string `path:"apply_id" doc:"run ULID; non-ULID → 400"`
}

// incRunDetailOutput — huma-output GET .../runs/{apply_id} (FULL-TYPED). Body — native
// RunDetailReply (run header + per-host slice with the address of the failed task).
type incRunDetailOutput struct {
	Body RunDetailReply
}

func incRunDetailOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationRun",
		Method:        http.MethodGet,
		Path:          "/{name}/runs/{apply_id}",
		Summary:       "Incarnation run details (per-host)",
		Description:   "Slice by hosts of one apply_id: status per host + address of failed task (task_idx/plan_index/error). Foreign apply_id / outside RBAC scope -> 404. Permission incarnation.history. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/runs/{apply_id}/tasks (run task plan + per-host) — READ (NO audit) — NIM-37 ===

// incRunTasksInput — huma-input GET .../runs/{apply_id}/tasks. Name/ApplyID — path.
// The apply_id format (ULID) is validated by RunTasksTyped → 400 (non-ULID).
type incRunTasksInput struct {
	Name    string `path:"name" doc:"incarnation name"`
	ApplyID string `path:"apply_id" doc:"run ULID; non-ULID → 400"`
}

// incRunTasksOutput — huma-output GET .../runs/{apply_id}/tasks (FULL-TYPED). Body —
// native RunTasksReply (the run's task plan + per-host results).
type incRunTasksOutput struct {
	Body RunTasksReply
}

func incRunTasksOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationRunTasks",
		Method:        http.MethodGet,
		Path:          "/{name}/runs/{apply_id}/tasks",
		Summary:       "Incarnation run tasks (plan + per-host)",
		Description:   "Task plan of one apply_id (plan_index/name/module/no_log/passage) + per-host status/output/error from the audit log (task.executed) joined by plan_index. Foreign apply_id / outside RBAC scope -> 404. Permission incarnation.history. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/scenarios/{scenario} (run) — MIDDLEWARE-AUDIT incarnation.scenario_started (202+body) ===

// incRunInput — huma-input POST .../scenarios/{scenario}. Name/Scenario — path; Body —
// A POINTER (opt. body: huma marks RequestBody.Required=false for *T, on an empty body
// Body=nil — parity with legacy io.EOF→zero-value). input is optional.
type incRunInput struct {
	Name     string                 `path:"name" doc:"incarnation name"`
	Scenario string                 `path:"scenario" doc:"scenario name"`
	Body     *IncarnationRunRequest `doc:"opt. body: scenario input"`
}

// IncarnationRunRequest — Go form of the POST .../scenarios/{scenario} body. name/scenario
// echoed from the path are ignored (the path is authoritative). input is optional.
// additionalProperties:false → unknown field → 400. The name = the contract schema name (T4b).
type IncarnationRunRequest struct {
	Name     *string        `json:"name,omitempty" doc:"echo path-name (ignored)"`
	Scenario *string        `json:"scenario,omitempty" doc:"echo path-scenario (ignored)"`
	Input    map[string]any `json:"input,omitempty" doc:"scenario input"`
}

// incRunOutput — huma-output POST .../scenarios/{scenario} (FULL-TYPED). Status=202;
// Body — native IncarnationRunReply (apply_id + echo incarnation/scenario).
type incRunOutput struct {
	Status int `json:"-"`
	Body   IncarnationRunReply
}

func incRunOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "runIncarnationScenario",
		Method:        http.MethodPost,
		Path:          "/{name}/scenarios/{scenario}",
		Summary:       "Run incarnation scenario",
		Description:   "Async run of a named scenario (ADR-009). Blocked on cluster:degraded (503). Permission incarnation.run.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/unlock (unlock) — MIDDLEWARE-AUDIT incarnation.unlocked (200+body) ===

// incUnlockInput — huma-input POST .../unlock. Name — path; Body — a typed body.
type incUnlockInput struct {
	Name string `path:"name" doc:"incarnation name"`
	Body IncarnationUnlockRequest
}

// IncarnationUnlockRequest — Go form of the POST .../unlock body. reason required; name echo
// is ignored. additionalProperties:false → unknown field → 400. The name = the contract
// schema name (T4b).
type IncarnationUnlockRequest struct {
	Name   *string `json:"name,omitempty" doc:"echo path-name (ignored)"`
	Reason string  `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"free text confirmation"`
}

// incUnlockOutput — huma-output POST .../unlock (FULL-TYPED). Status=200; Body —
// native IncarnationUnlockReply.
type incUnlockOutput struct {
	Status int `json:"-"`
	Body   IncarnationUnlockReply
}

func incUnlockOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "unlockIncarnation",
		Method:        http.MethodPost,
		Path:          "/{name}/unlock",
		Summary:       "Remove incarnation blocking status",
		Description:   "error_locked / migration_failed → ready under FOR UPDATE; state does not change (ADR-009/019). Permission incarnation.unlock.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/upgrade (upgrade) — MIDDLEWARE-AUDIT incarnation.upgrade_started (202+body) ===

// incUpgradeInput — huma input for POST .../upgrade. Name — path; Body — typed body.
type incUpgradeInput struct {
	Name string `path:"name" doc:"incarnation name"`
	Body IncarnationUpgradeRequest
}

// IncarnationUpgradeRequest — Go form of the POST .../upgrade body. to_version required; name
// echo is ignored. additionalProperties:false → unknown field → 400. The name = the contract
// schema name (T4b).
type IncarnationUpgradeRequest struct {
	Name      *string `json:"name,omitempty" doc:"echo path-name (ignored)"`
	ToVersion string  `json:"to_version" required:"true" doc:"target service version (git-ref)"`
}

// incUpgradeOutput — huma-output POST .../upgrade (FULL-TYPED). Status=202; Body —
// native IncarnationUpgradeReply (apply_id).
type incUpgradeOutput struct {
	Status int `json:"-"`
	Body   IncarnationUpgradeReply
}

func incUpgradeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "upgradeIncarnation",
		Method:        http.MethodPost,
		Path:          "/{name}/upgrade",
		Summary:       "Migrate incarnation to new version",
		Description:   "Sync-under-202 migration state_schema (ADR-019) + service_version change in one tx. Permission incarnation.upgrade.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/upgrade-paths (upgrade-paths) — READ (NO audit) ===

// incUpgradePathsInput — huma input for GET .../upgrade-paths. Name — path; To — an optional
// query-ref for on-demand analysis of a single target (empty → a cheap list of tags).
type incUpgradePathsInput struct {
	Name string `path:"name" doc:"incarnation name"`
	To   string `query:"to" doc:"opt. target git-ref for on-demand analysis of single target; empty → list of registry tags + is_current"`
}

// incUpgradePathsOutput — huma-output GET .../upgrade-paths (FULL-TYPED). Body —
// native IncarnationUpgradePathsReply (paths OR target depending on mode).
type incUpgradePathsOutput struct {
	Body IncarnationUpgradePathsReply
}

func incUpgradePathsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationUpgradePaths",
		Method:        http.MethodGet,
		Path:          "/{name}/upgrade-paths",
		Summary:       "Incarnation upgrade paths",
		Description:   "Cheap list of service registry refs (marking is_current) without ?to=; on-demand analysis of one target (direction / found-legacy / state migrations) with ?to=<ref> (ADR-0068 6). Permission incarnation.upgrade (read facet). Read-only, no audit.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === POST /v1/incarnations/{name}/rerun-last (rerun-last) — SELF-AUDIT incarnation.rerun_last (202+body) ===

// incRerunInput — huma input for POST .../rerun-last. Name — path; Body — typed body.
type incRerunInput struct {
	Name string `path:"name" doc:"incarnation name"`
	Body IncarnationRerunLastRequest
}

// IncarnationRerunLastRequest — Go form of the POST .../rerun-last body. reason required.
type IncarnationRerunLastRequest struct {
	Reason string `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"free text confirmation"`
}

// incRerunOutput — huma-output POST .../rerun-last (FULL-TYPED). Status=202; Body —
// native IncarnationRerunLastReply (apply_id + echo incarnation + scenario).
type incRerunOutput struct {
	Status int `json:"-"`
	Body   IncarnationRerunLastReply
}

func incRerunOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "rerunLastIncarnation",
		Method:        http.MethodPost,
		Path:          "/{name}/rerun-last",
		Summary:       "Restart the last failed scenario from error_locked",
		Description:   "Clears error_locked and, in the same action, restarts the incarnation last failed scenario (bootstrap create/... or operational add_user/...) with the stored input of the failed run (one tx FOR UPDATE). Permission incarnation.rerun-last.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/check-drift (check-drift) — SELF-AUDIT incarnation.drift_checked (200+body) ===

// incCheckDriftInput — huma input for POST .../check-drift. Name — path; Body — a POINTER
// (opt. body: huma RequestBody.Required=false for *T, on an empty body Body=nil — parity
// with legacy io.EOF→zero-value).
type incCheckDriftInput struct {
	Name string                        `path:"name" doc:"incarnation name"`
	Body *IncarnationCheckDriftRequest `doc:"optional body: override converge parameters"`
}

// IncarnationCheckDriftRequest — Go form of the POST .../check-drift body. input — an override
// of converge parameters (opt.). additionalProperties:false → unknown field → 400. The name =
// the contract schema name (T4b).
type IncarnationCheckDriftRequest struct {
	Input map[string]any `json:"input,omitempty" doc:"override converge parameters (ADR-031 Slice B)"`
}

// incCheckDriftOutput — huma-output POST .../check-drift (FULL-TYPED). Status=200; Body
// — *scenario.DriftReport (the same type legacy writeJSON wrote). CheckDriftTyped
// returns non-nil on success.
type incCheckDriftOutput struct {
	Body *scenario.DriftReport
}

func incCheckDriftOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "checkIncarnationDrift",
		Method:        http.MethodPost,
		Path:          "/{name}/check-drift",
		Summary:       "Check incarnation drift (Scry)",
		Description:   "Sync dry_run converge -> DriftReport (ADR-031 Slice B). Informational status=drift marking. Permission incarnation.check-drift.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/incarnations/{name} (destroy) — SELF-AUDIT incarnation.destroy_started (202+body) ===

// incDestroyInput — huma-input DELETE /v1/incarnations/{name}. Name — path; AllowDestroy
// — required boolean query (confirmation-flag). huma binds bool in a typed way: missing/
// non-boolean → 400 (parity strict required-param + legacy ParseBool).
type incDestroyInput struct {
	Name         string `path:"name" doc:"incarnation name"`
	AllowDestroy bool   `query:"allow_destroy" required:"true" doc:"confirmation-flag: true -> destroy without teardown"`
}

// incDestroyOutput — huma-output DELETE /v1/incarnations/{name} (FULL-TYPED). Status=202;
// Body — native IncarnationDestroyReply (apply_id).
type incDestroyOutput struct {
	Status int `json:"-"`
	Body   IncarnationDestroyReply
}

func incDestroyOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "destroyIncarnation",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Destroy an incarnation",
		Description:   "allow_destroy=true -> DELETE without teardown; false -> scenario destroy (S-D4). Permission incarnation.destroy.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PATCH /v1/incarnations/{name}/hosts (update-hosts) — SELF-AUDIT incarnation.hosts_updated (200+body) ===
//
// PATCH presence: mode semantics (replace/append/remove) do NOT require distinguishing
// omitted/null/value (mode/hosts are required-operation semantics, not sparse-update fields).
// Hence the form — `*string omitempty` for role (parity legacy IncarnationSpecHost), NOT
// the Optional[T] presence-tier from huma_optional.go (see huma_optional.go §"Other PATCH ...
// don't detect presence").

// incUpdateHostsInput — huma input for PATCH .../hosts. Name — path; Body — typed body.
type incUpdateHostsInput struct {
	Name string `path:"name" doc:"incarnation name"`
	Body IncarnationUpdateHostsRequest
}

// IncarnationSpecHost — one hosts[] entry. sid required; role opt. (kebab-case 1..63
// or empty — domain validation 422). additionalProperties:false → unknown field → 400.
// The name = the contract schema name (T4b); a huma form with validation tags, distinct from
// IncarnationSpecHost (the domain model without huma tags).
type IncarnationSpecHost struct {
	SID  string  `json:"sid" required:"true" doc:"SID (FQDN) of the host - must already exist in souls"`
	Role *string `json:"role,omitempty" maxLength:"63" doc:"declared role (kebab-case 1..63) or null"`
}

// IncarnationUpdateHostsRequest — Go form of the PATCH .../hosts body. mode required (enum);
// hosts — an array (empty is legitimate for replace). additionalProperties:false → unknown
// field → 400. The name = the contract schema name (T4b).
type IncarnationUpdateHostsRequest struct {
	Mode  string                `json:"mode" required:"true" enum:"replace,append,remove" doc:"operation type over spec.hosts[]"`
	Hosts []IncarnationSpecHost `json:"hosts" required:"true" doc:"host list for mode operation (empty legitimate for replace)"`
}

// incUpdateHostsOutput — huma-output PATCH .../hosts (FULL-TYPED). Status=200; Body —
// the full native IncarnationGetReply after the edit (byte-exact with legacy).
type incUpdateHostsOutput struct {
	Body IncarnationGetReply
}

func incUpdateHostsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateIncarnationHosts",
		Method:        http.MethodPatch,
		Path:          "/{name}/hosts",
		Summary:       "Edit declared spec.hosts[] of an incarnation",
		Description:   "Three modes (replace/append/remove) over declared hosts (ADR-008). Permission incarnation.update-hosts.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/incarnations/{name}/traits (set-traits) — SELF-AUDIT incarnation.traits_changed (200+body) ===

// incSetTraitsInput — huma input for PUT .../traits. Name — path; Body — typed body.
type incSetTraitsInput struct {
	Name string `path:"name" doc:"incarnation name"`
	Body IncarnationSetTraitsRequest
}

// IncarnationSetTraitsRequest — Go form of the PUT .../traits body. traits — a full
// replacement of operator-set trait labels (key → scalar|list of scalars); empty/absent
// = clear. The value format (nested forbidden) is validated by the domain → 422.
// additionalProperties:false → unknown field → 400. The name = the contract schema name.
type IncarnationSetTraitsRequest struct {
	Traits map[string]any `json:"traits,omitempty" doc:"full set of trait-tags (key -> scalar|list of scalars); empty/omitted = clear (ADR-060)"`
}

// incSetTraitsOutput — huma-output PUT .../traits (FULL-TYPED). Status=200; Body —
// the full native IncarnationGetReply after the replacement (byte-exact with GET / update-hosts).
type incSetTraitsOutput struct {
	Body IncarnationGetReply
}

func incSetTraitsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "setIncarnationTraits",
		Method:        http.MethodPut,
		Path:          "/{name}/traits",
		Summary:       "Replace operator-set trait labels of an incarnation",
		Description:   "Wholesale replacement of incarnation.traits (ADR-060) - source of truth, projected onto souls.traits of member hosts. Permission incarnation.traits-set.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
