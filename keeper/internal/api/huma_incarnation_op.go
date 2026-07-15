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
	Name    string         `json:"name" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"имя нового instance (kebab-case), корневая Coven-метка"`
	Service string         `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"имя сервиса из реестра (ADR-029)"`
	Covens  []string       `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"declared environment-теги (ADR-008 amendment a)"`
	Input   map[string]any `json:"input,omitempty" doc:"input для выбранного create-сценария"`
	// Traits — operator-set trait labels of the incarnation (ADR-060 amend R1): map key →
	// scalar | list of scalars. Stored in incarnation.traits (source of truth) and
	// materialized into souls.traits of member hosts. Format/value is validated by the domain
	// (nested object/array → 422). Operational replacement — PUT .../traits.
	Traits map[string]any `json:"traits,omitempty" doc:"operator-set trait-метки (ключ → scalar|list of scalars), ADR-060"`
	// CreateScenario — choice of the start scenario (mechanism of multiple create scenarios).
	// Optional. Empty-choice contract (Phase 2, union removed): a service offering create
	// scenarios (scenario with `create: true`) + empty → 422 create_scenario_required; a
	// service without them + empty → bare incarnation (ready without a run, created_scenario=
	// NULL). Auto-create by the default `create` is gone. A non-empty name must belong to the
	// service's create set, otherwise 422; the choice is saved in incarnation.created_scenario
	// (rerun-last uses it on the create path).
	CreateScenario string `json:"create_scenario,omitempty" pattern:"^[a-z][a-z0-9_]*$" doc:"имя стартового сценария (механизм нескольких create, scenario с create:true). Пусто: сервис предлагает create-сценарии → 422 create_scenario_required; сервис без них → bare-инкарнация (ready без прогона)"`
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
		Summary:       "Создать инкарнацию",
		Description:   "Runtime-инстанс сервиса (ADR-029). Запускает scenario create (async, если lifecycle.auto_create). Permission incarnation.create.",
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
	Offset  int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
	Service string `query:"service" doc:"фильтр по имени сервиса"`
	Status  string `query:"status" doc:"фильтр по статусу (ready/applying/error_locked/migration_failed); невалидный → 422"`
	Coven   string `query:"coven" doc:"exact-match по covens[] (ADR-008); невалидная метка → 422"`
	SortBy  string `query:"sort" doc:"поле сортировки (created_at/name/status/service или state.<field>)"`
	SortDir string `query:"sort_dir" doc:"направление сортировки (asc/desc)"`
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
		Summary:       "Список инкарнаций (paged)",
		Description:   "Фильтры service/status/coven/state.<field> + сортировка. Видимость scoped по RBAC (ADR-047). Permission incarnation.list. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name} (get) — READ with path (no audit) ===

// incGetInput — huma input for GET /v1/incarnations/{name}. Name — path.
type incGetInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
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
		Summary:       "Получить инкарнацию",
		Description:   "Деталь runtime-инстанса. Вне RBAC-scope → 404 (не палим существование). Permission incarnation.get. Read-only.",
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
	Name    string `path:"name" doc:"имя инкарнации"`
	ApplyID string `query:"apply_id" doc:"опц. ULID-фильтр по state_history.apply_id; не-ULID → 400"`
	Offset  int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
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
		Summary:       "История state-переходов инкарнации (paged)",
		Description:   "state_history с фильтром apply_id и пагинацией. Вне RBAC-scope → 404. Permission incarnation.history. Read-only.",
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
	Name   string `path:"name" doc:"имя инкарнации"`
	Offset int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit  int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
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
		Summary:       "Список прогонов инкарнации (paged)",
		Description:   "Свёртка apply_runs по apply_id: статус прогона (applying/success/failed/cancelled), границы времени, инициатор. Прогон (apply_run) — НЕ Voyage. Вне RBAC-scope → 404. Permission incarnation.history. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/runs/{apply_id} (run detail) — READ-with-path (NO audit) ===

// incRunDetailInput — huma-input GET .../runs/{apply_id}. Name/ApplyID — path. The
// apply_id format (ULID) is validated by RunDetailTyped → 400 (non-ULID), symmetric with the History filter.
type incRunDetailInput struct {
	Name    string `path:"name" doc:"имя инкарнации"`
	ApplyID string `path:"apply_id" doc:"ULID прогона; не-ULID → 400"`
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
		Summary:       "Детали прогона инкарнации (per-host)",
		Description:   "Срез по хостам одного apply_id: статус каждого хоста + адрес упавшей задачи (task_idx/plan_index/error). Чужой apply_id / вне RBAC-scope → 404. Permission incarnation.history. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/runs/{apply_id}/tasks (run task plan + per-host) — READ (NO audit) — NIM-37 ===

// incRunTasksInput — huma-input GET .../runs/{apply_id}/tasks. Name/ApplyID — path.
// The apply_id format (ULID) is validated by RunTasksTyped → 400 (non-ULID).
type incRunTasksInput struct {
	Name    string `path:"name" doc:"имя инкарнации"`
	ApplyID string `path:"apply_id" doc:"ULID прогона; не-ULID → 400"`
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
		Summary:       "Задачи прогона инкарнации (план + per-host)",
		Description:   "План задач одного apply_id (plan_index/name/module/no_log/passage) + per-host статус/output/ошибка из журнала аудита (task.executed) джойном по plan_index. Чужой apply_id / вне RBAC-scope → 404. Permission incarnation.history. Read-only.",
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
	Name     string                 `path:"name" doc:"имя инкарнации"`
	Scenario string                 `path:"scenario" doc:"имя сценария"`
	Body     *IncarnationRunRequest `doc:"опц. тело: input scenario"`
}

// IncarnationRunRequest — Go form of the POST .../scenarios/{scenario} body. name/scenario
// echoed from the path are ignored (the path is authoritative). input is optional.
// additionalProperties:false → unknown field → 400. The name = the contract schema name (T4b).
type IncarnationRunRequest struct {
	Name     *string        `json:"name,omitempty" doc:"echo path-name (игнорируется)"`
	Scenario *string        `json:"scenario,omitempty" doc:"echo path-scenario (игнорируется)"`
	Input    map[string]any `json:"input,omitempty" doc:"input scenario"`
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
		Summary:       "Запустить сценарий инкарнации",
		Description:   "Async-прогон именованного scenario (ADR-009). Блокируется при cluster:degraded (503). Permission incarnation.run.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/unlock (unlock) — MIDDLEWARE-AUDIT incarnation.unlocked (200+body) ===

// incUnlockInput — huma-input POST .../unlock. Name — path; Body — a typed body.
type incUnlockInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationUnlockRequest
}

// IncarnationUnlockRequest — Go-форма тела POST .../unlock. reason required; name echo
// игнорируется. additionalProperties:false → unknown поле → 400. Имя = контрактное
// имя схемы (T4b).
type IncarnationUnlockRequest struct {
	Name   *string `json:"name,omitempty" doc:"echo path-name (игнорируется)"`
	Reason string  `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"свободный текст подтверждения"`
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
		Summary:       "Снять блокирующий статус инкарнации",
		Description:   "error_locked / migration_failed → ready под FOR UPDATE; state не меняется (ADR-009/019). Permission incarnation.unlock.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/upgrade (upgrade) — MIDDLEWARE-AUDIT incarnation.upgrade_started (202+body) ===

// incUpgradeInput — huma-input POST .../upgrade. Name — path; Body — typed тело.
type incUpgradeInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationUpgradeRequest
}

// IncarnationUpgradeRequest — Go-форма тела POST .../upgrade. to_version required; name
// echo игнорируется. additionalProperties:false → unknown поле → 400. Имя = контрактное
// имя схемы (T4b).
type IncarnationUpgradeRequest struct {
	Name      *string `json:"name,omitempty" doc:"echo path-name (игнорируется)"`
	ToVersion string  `json:"to_version" required:"true" doc:"целевая версия сервиса (git-ref)"`
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
		Summary:       "Перевести инкарнацию на новую версию",
		Description:   "Sync-под-202 миграция state_schema (ADR-019) + смена service_version одной tx. Permission incarnation.upgrade.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/upgrade-paths (upgrade-paths) — READ (БЕЗ audit) ===

// incUpgradePathsInput — huma-input GET .../upgrade-paths. Name — path; To — опц.
// query-ref для on-demand анализа одной цели (пусто → дешёвый список тегов).
type incUpgradePathsInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	To   string `query:"to" doc:"опц. целевой git-ref для on-demand анализа одной цели; пусто → список тегов реестра + is_current"`
}

// incUpgradePathsOutput — huma-output GET .../upgrade-paths (FULL-TYPED). Body —
// native IncarnationUpgradePathsReply (paths ИЛИ target по режиму).
type incUpgradePathsOutput struct {
	Body IncarnationUpgradePathsReply
}

func incUpgradePathsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationUpgradePaths",
		Method:        http.MethodGet,
		Path:          "/{name}/upgrade-paths",
		Summary:       "Пути апгрейда инкарнации",
		Description:   "Дешёвый список тегов реестра сервиса (пометка is_current) без ?to=; on-demand анализ одной цели (direction / found-legacy / state-миграции) с ?to=<ref> (ADR-0068 §6). Permission incarnation.upgrade (read-грань). Read-only, без audit.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === POST /v1/incarnations/{name}/rerun-last (rerun-last) — SELF-AUDIT incarnation.rerun_last (202+body) ===

// incRerunInput — huma-input POST .../rerun-last. Name — path; Body — typed тело.
type incRerunInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationRerunLastRequest
}

// IncarnationRerunLastRequest — Go-форма тела POST .../rerun-last. reason required.
type IncarnationRerunLastRequest struct {
	Reason string `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"свободный текст подтверждения"`
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
		Summary:       "Перезапустить последний упавший сценарий из error_locked",
		Description:   "Снимает error_locked и тем же действием перезапускает последний упавший сценарий инкарнации (bootstrap create/… или day-2 add_user/…) с сохранённым input упавшего прогона (одна tx FOR UPDATE). Permission incarnation.rerun-last.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/check-drift (check-drift) — SELF-AUDIT incarnation.drift_checked (200+body) ===

// incCheckDriftInput — huma-input POST .../check-drift. Name — path; Body — ПОИНТЕР
// (опц. тело: huma RequestBody.Required=false для *T, на пустом body Body=nil — parity
// legacy io.EOF→zero-value).
type incCheckDriftInput struct {
	Name string                        `path:"name" doc:"имя инкарнации"`
	Body *IncarnationCheckDriftRequest `doc:"опц. тело: override converge-параметров"`
}

// IncarnationCheckDriftRequest — Go-форма тела POST .../check-drift. input — override
// converge-параметров (опц.). additionalProperties:false → unknown поле → 400. Имя =
// контрактное имя схемы (T4b).
type IncarnationCheckDriftRequest struct {
	Input map[string]any `json:"input,omitempty" doc:"override converge-параметров (ADR-031 Slice B)"`
}

// incCheckDriftOutput — huma-output POST .../check-drift (FULL-TYPED). Status=200; Body
// — *scenario.DriftReport (тот же тип, что писал legacy writeJSON). CheckDriftTyped на
// успехе возвращает non-nil.
type incCheckDriftOutput struct {
	Body *scenario.DriftReport
}

func incCheckDriftOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "checkIncarnationDrift",
		Method:        http.MethodPost,
		Path:          "/{name}/check-drift",
		Summary:       "Проверить drift инкарнации (Scry)",
		Description:   "Sync dry_run converge → DriftReport (ADR-031 Slice B). Информационная маркировка status=drift. Permission incarnation.check-drift.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/incarnations/{name} (destroy) — SELF-AUDIT incarnation.destroy_started (202+body) ===

// incDestroyInput — huma-input DELETE /v1/incarnations/{name}. Name — path; AllowDestroy
// — required boolean query (confirmation-flag). huma биндит bool типизированно: missing/
// non-boolean → 400 (parity strict required-param + legacy ParseBool).
type incDestroyInput struct {
	Name         string `path:"name" doc:"имя инкарнации"`
	AllowDestroy bool   `query:"allow_destroy" required:"true" doc:"confirmation-flag: true → destroy без teardown"`
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
		Summary:       "Снести инкарнацию",
		Description:   "allow_destroy=true → DELETE без teardown; false → scenario destroy (S-D4). Permission incarnation.destroy.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PATCH /v1/incarnations/{name}/hosts (update-hosts) — SELF-AUDIT incarnation.hosts_updated (200+body) ===
//
// PATCH-presence: mode-семантика (replace/append/remove) НЕ требует различения
// omitted/null/value (mode/hosts — required-семантика операции, не sparse-update поля).
// Поэтому форма — `*string omitempty` для role (parity legacy IncarnationSpecHost), НЕ
// Optional[T] presence-tier huma_optional.go (см. huma_optional.go §«Прочие PATCH ...
// presence НЕ детектят»).

// incUpdateHostsInput — huma-input PATCH .../hosts. Name — path; Body — typed тело.
type incUpdateHostsInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationUpdateHostsRequest
}

// IncarnationSpecHost — одна запись hosts[]. sid required; role опц. (kebab-case 1..63
// или пусто — доменная валидация 422). additionalProperties:false → unknown поле → 400.
// Имя = контрактное имя схемы (T4b); huma-форма с валидационными тегами, отличается от
// IncarnationSpecHost (доменная модель без huma-тегов).
type IncarnationSpecHost struct {
	SID  string  `json:"sid" required:"true" doc:"SID (FQDN) хоста — обязан существовать в souls"`
	Role *string `json:"role,omitempty" maxLength:"63" doc:"declared-роль (kebab-case 1..63) или null"`
}

// IncarnationUpdateHostsRequest — Go-форма тела PATCH .../hosts. mode required (enum);
// hosts — массив (пустой legitimate для replace). additionalProperties:false → unknown
// поле → 400. Имя = контрактное имя схемы (T4b).
type IncarnationUpdateHostsRequest struct {
	Mode  string                `json:"mode" required:"true" enum:"replace,append,remove" doc:"тип операции над spec.hosts[]"`
	Hosts []IncarnationSpecHost `json:"hosts" required:"true" doc:"список hosts для mode-операции (пустой legitimate для replace)"`
}

// incUpdateHostsOutput — huma-output PATCH .../hosts (FULL-TYPED). Status=200; Body —
// полный native IncarnationGetReply после правки (byte-exact с legacy).
type incUpdateHostsOutput struct {
	Body IncarnationGetReply
}

func incUpdateHostsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateIncarnationHosts",
		Method:        http.MethodPatch,
		Path:          "/{name}/hosts",
		Summary:       "Править declared spec.hosts[] инкарнации",
		Description:   "Три mode (replace/append/remove) над declared hosts (ADR-008). Permission incarnation.update-hosts.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/incarnations/{name}/traits (set-traits) — SELF-AUDIT incarnation.traits_changed (200+body) ===

// incSetTraitsInput — huma-input PUT .../traits. Name — path; Body — typed тело.
type incSetTraitsInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationSetTraitsRequest
}

// IncarnationSetTraitsRequest — Go-форма тела PUT .../traits. traits — целостная
// замена operator-set trait-меток (key → scalar|list of scalars); пустой/отсутствует
// = очистить. Формат значения (запрет nested) валидирует домен → 422.
// additionalProperties:false → unknown поле → 400. Имя = контрактное имя схемы.
type IncarnationSetTraitsRequest struct {
	Traits map[string]any `json:"traits,omitempty" doc:"полный набор trait-меток (ключ → scalar|list of scalars); пустой/опущен = очистить (ADR-060)"`
}

// incSetTraitsOutput — huma-output PUT .../traits (FULL-TYPED). Status=200; Body —
// полный native IncarnationGetReply после замены (byte-exact с GET / update-hosts).
type incSetTraitsOutput struct {
	Body IncarnationGetReply
}

func incSetTraitsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "setIncarnationTraits",
		Method:        http.MethodPut,
		Path:          "/{name}/traits",
		Summary:       "Заменить operator-set trait-метки инкарнации",
		Description:   "Целостная замена incarnation.traits (ADR-060) — источника истины, проецируемого в souls.traits хостов-членов. Permission incarnation.traits-set.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
