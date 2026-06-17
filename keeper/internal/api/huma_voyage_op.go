package api

// FULL-TYPED форма VOYAGE-домена (code-first источник OpenAPI, ADR-054 §Pattern).
// БАТЧ-2f WRITE-SELF-AUDIT (create/cancel пишут audit ВНУТРИ handler-а через
// emitCreated/emitCancelled, БЕЗ audit-middleware — отличие от middleware-audit-доменов
// role/operator): create — scenario_run.started / command_run.invoked (202+body+Location);
// cancel — scenario_run.cancelled / command_run.cancelled (202+body). preview — dry-resolve
// БЕЗ audit (read-like POST, 200+body). list/get/targets — read (БЕЗ audit). RBAC-by-kind
// (ADR-043 §6) живёт ВНУТРИ handler-а (kind виден только из тела/строки) — router навешивает
// лишь base auth + Tempo (на create/preview).

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/voyages (create) — WRITE-SELF-AUDIT scenario_run.started/command_run.invoked (202+body) ===

// voyageCreateInput — huma-input POST /v1/voyages (FULL-TYPED). Body — типизированное
// тело: huma декодит и валидирует его по схеме из huma-тегов VoyageCreateRequest.
type voyageCreateInput struct {
	Body VoyageCreateRequest
}

// VoyageCreateRequest — Go-форма тела POST /v1/voyages (code-first источник схемы И
// валидации). Повторяет доменный voyageCreateRequest: рецепт прогона (kind/scenario_
// name|module/target/input/scheduling/batch*) + notify[]. kind-зависимая валидация
// (scenario↔scenario_name / command↔module, target-непустота, on_failure/batch_mode-enum,
// диапазоны) — доменная (CreateTyped → 422). additionalProperties:false (huma-дефолт) →
// unknown поле → 400. kind/batch_mode/on_failure — инлайн-enum (рукопись НЕ выносит их
// standalone-схемой → enum-alias-механизм не применяется). Имя структуры = контрактное
// имя схемы (huma DefaultSchemaNamer берёт reflect.Type.Name()) — выровнено под committed-
// рукопись (тираж N3). Доменный VoyageCreateRequest в спеку не попадает (huma-input —
// эта структура; oapi-тип не используется как huma-body).
type VoyageCreateRequest struct {
	Kind         string         `json:"kind" required:"true" enum:"scenario,command" doc:"тип рецепта прогона"`
	ScenarioName string         `json:"scenario_name,omitempty" doc:"имя сценария для kind=scenario"`
	Module       string         `json:"module,omitempty" doc:"модуль для kind=command"`
	Input        map[string]any `json:"input,omitempty" doc:"параметры рецепта"`
	Target       VoyageTarget   `json:"target" required:"true" doc:"декларативный таргет (резолвится в snapshot единиц)"`

	Batch                *string    `json:"batch,omitempty" doc:"размер батча: N хостов или N%"`
	BatchSize            *int       `json:"batch_size,omitempty" minimum:"1"`
	BatchPercent         *int       `json:"batch_percent,omitempty" minimum:"1" maximum:"100"`
	Concurrency          *int       `json:"concurrency,omitempty" minimum:"1" maximum:"500"`
	BatchMode            string     `json:"batch_mode,omitempty" doc:"barrier (default) | window"`
	DryRun               bool       `json:"dry_run,omitempty"`
	ScheduleAt           *time.Time `json:"schedule_at,omitempty" doc:"RFC3339 отложенный старт"`
	InterBatchIntervalMS *int       `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int       `json:"inter_unit_interval_ms,omitempty"`

	MaxFailures   *string `json:"max_failures,omitempty" doc:"порог провалов: N абсолют или N%"`
	FailThreshold *int    `json:"fail_threshold,omitempty" minimum:"1"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     string  `json:"on_failure,omitempty" doc:"abort | continue (default)"`

	Notify []VoyageNotify `json:"notify,omitempty" doc:"разовые подписки на ЭТОТ прогон (ephemeral)"`
}

// Вложенные target/notify — единые api.VoyageTarget/api.VoyageNotify (huma_voyage_target.go),
// shared с cadence-доменом; форма выровнена под committed-рукопись (одна схема на каждую).

// voyageCreateOutput — huma-output POST /v1/voyages (FULL-TYPED). Status=202; Location —
// header; Body — huma-native 202-тело (api.VoyageCreateReply: voyage_id/kind/scope_size/
// status/location). Конверт handler-reply (legacy-генерата) → native — в register-func.
type voyageCreateOutput struct {
	Status   int    `json:"-"`
	Location string `header:"Location" json:"-"`
	Body     VoyageCreateReply
}

// voyageCreateOperation — метаданные POST /v1/voyages. DefaultStatus=202. WRITE-SELF-
// AUDIT (handler пишет scenario_run.started/command_run.invoked). RBAC-by-kind — в handler-е.
// Errors: 400 unknown/malformed, 403 RBAC-by-kind, 404 инкарнация, 422 валидация рецепта/
// target/batch / пустой резолв / scope-cap, 429 Tempo, 500.
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

// === POST /v1/voyages/preview (preview) — READ-like (БЕЗ audit) dry-resolve (200+body) ===

// voyagePreviewInput — huma-input POST /v1/voyages/preview (FULL-TYPED). Body — то же
// типизированное тело, что create (та же валидация/резолв).
type voyagePreviewInput struct {
	Body VoyageCreateRequest
}

// voyagePreviewOutput — huma-output POST /v1/voyages/preview (FULL-TYPED). Status=200;
// Body — huma-native 200-тело (api.VoyagePreviewReply: kind/scope_size/total_batches/
// batch_mode/effective_batch_size?). Конверт — в register-func.
type voyagePreviewOutput struct {
	Body VoyagePreviewReply
}

// voyagePreviewOperation — метаданные POST /v1/voyages/preview. DefaultStatus=200.
// READ-like: audit НЕ навешан (preview не пишет audit-event, в отличие от Create — он
// dry-resolve без создания Voyage). RBAC-by-kind — в handler-е. Errors: 400, 403, 404,
// 422 (консистентно с Create — preview отказывает там же), 429 Tempo, 500.
func voyagePreviewOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "previewVoyage",
		Method:        http.MethodPost,
		Path:          "/preview",
		Summary:       "Dry-resolve scope Voyage",
		Description:   "Предпоказ числа единиц/батчей БЕЗ создания Voyage (ADR-043 amendment §4). Та же валидация/резолв/RBAC, что Create. Без раскрытия SID-списка. Read-like — без audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// === GET /v1/voyages (list) — READ-with-typed-query (БЕЗ audit) ===

// voyageListInput — huma-input GET /v1/voyages (FULL-TYPED typed-query). kind — enum-
// фильтр; status — multi-value enum (explode:true ОБЯЗАТЕЛЕН); offset/limit — int32 с
// default (диапазон enforce-ит ДОМЕН через ParsePage/CheckPageBounds → 400, НЕ huma-
// schema-min/max → иначе out-of-range дало бы 422 вместо контрактного 400).
type voyageListInput struct {
	Kind     string   `query:"kind" enum:"scenario,command" doc:"фильтр по kind; вне enum → 422"`
	Statuses []string `query:"status,explode" enum:"scheduled,pending,running,succeeded,failed,partial_failed,cancelled" doc:"multi-value ?status=X&status=Y OR; вне enum → 422"`
	Offset   int32    `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit    int32    `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// voyageListOutput — huma-output GET /v1/voyages (FULL-TYPED). Body — huma-native envelope
// (api.VoyageListReply: items/offset/limit/total; items.$ref на native Voyage). ★ ОБЩИЙ
// VoyageListReply/Voyage с cadence-runs (дедуп byte-identical). Конверт — в register-func.
type voyageListOutput struct {
	Body VoyageListReply
}

// voyageListOperation — метаданные GET /v1/voyages. DefaultStatus=200. READ: audit НЕ
// навешан. Permission incarnation.history. Errors: 400 (out-of-range pagination), 403,
// 422 (bad kind/status enum), 500.
func voyageListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listVoyages",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Voyage-прогонов (paged)",
		Description:   "Список прогонов с фильтрами kind/status и пагинацией (ADR-043). target_resolved НЕ раскрывается (UI читает scope_size). Permission incarnation.history. Read-only, без audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/voyages/{id} (get) — READ-with-path (БЕЗ audit) ===

// voyageGetInput — huma-input GET /v1/voyages/{id}. ID — path (ULID-валидация — доменная).
type voyageGetInput struct {
	ID string `path:"id" doc:"ULID Voyage-прогона"`
}

// voyageGetOutput — huma-output GET /v1/voyages/{id} (FULL-TYPED). Body — huma-native
// 200-тело (api.Voyage: detail + summary; shared с list/cadence-runs). Конверт — в register-func.
type voyageGetOutput struct {
	Body Voyage
}

// voyageGetOperation — метаданные GET /v1/voyages/{id}. DefaultStatus=200. READ: audit НЕ
// навешан. Permission incarnation.history. Errors: 403, 404, 422 bad id, 500.
func voyageGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getVoyage",
		Method:        http.MethodGet,
		Path:          "/{id}",
		Summary:       "Snapshot Voyage-прогона",
		Description:   "Detail + summary одного прогона (ADR-043). Permission incarnation.history. Read-only, без audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/voyages/{id}/targets (targets) — READ-with-path (БЕЗ audit) ===

// voyageTargetsInput — huma-input GET /v1/voyages/{id}/targets. ID — path.
type voyageTargetsInput struct {
	ID string `path:"id" doc:"ULID Voyage-прогона"`
}

// voyageTargetsOutput — huma-output GET /v1/voyages/{id}/targets (FULL-TYPED). Body —
// huma-native 200-тело (api.VoyageTargetsReply: voyage_id + targets[]). Конверт — в register-func.
type voyageTargetsOutput struct {
	Body VoyageTargetsReply
}

// voyageTargetsOperation — метаданные GET /v1/voyages/{id}/targets. DefaultStatus=200.
// READ: audit НЕ навешан. Permission incarnation.history. Errors: 403, 404 (existence-
// probe), 422 bad id, 500.
func voyageTargetsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listVoyageTargets",
		Method:        http.MethodGet,
		Path:          "/{id}/targets",
		Summary:       "All-runs drill Voyage-прогона",
		Description:   "Per-target batch/status/back-link одного прогона (ADR-043). Permission incarnation.history. Read-only, без audit.",
		Tags:          []string{"voyage"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/voyages/{id} (cancel) — WRITE-SELF-AUDIT scenario_run.cancelled/command_run.cancelled (202+body) ===

// voyageCancelInput — huma-input DELETE /v1/voyages/{id}. ID — path.
type voyageCancelInput struct {
	ID string `path:"id" doc:"ULID Voyage-прогона"`
}

// voyageCancelOutput — huma-output DELETE /v1/voyages/{id} (FULL-TYPED). Status=202; Body —
// huma-native 202-тело (api.VoyageCancelReply: voyage_id + status:cancelled). Конверт — в register-func.
type voyageCancelOutput struct {
	Status int `json:"-"`
	Body   VoyageCancelReply
}

// voyageCancelOperation — метаданные DELETE /v1/voyages/{id}. DefaultStatus=202. WRITE-
// SELF-AUDIT (handler пишет scenario_run.cancelled/command_run.cancelled). RBAC-by-kind —
// в handler-е (kind виден из строки). Errors: 403, 404, 409 (running/terminal — не
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
