package api

// FULL-TYPED форма POST /v1/cadences (code-first источник OpenAPI, ADR-054
// Amendment 2026-06-12, §Pattern (б) тонкий-конверт). Go-типы — единственный
// источник правды: huma строит из них И JSON Schema OpenAPI-фрагмента, И валидацию
// входа (required/enum/additionalProperties:false ЧЕСТНЫЙ), И typed-output.
// RawBody-моста больше нет — huma валидирует typed Body нативно (§Инвариант-2):
// unknown→400 (error-override детектит "unexpected property"), required/enum→422.

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// cadenceCreateInput — huma-input операции POST /v1/cadences (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует его по схеме из huma-тегов
// CadenceCreateRequest. Конверт в доменную модель — в registerHumaCadence.
type cadenceCreateInput struct {
	Body CadenceCreateRequest
}

// CadenceCreateRequest — Go-форма тела POST /v1/cadences (code-first источник
// схемы И валидации). Повторяет доменный рецепт прогона (parity voyage) + правило
// повторения + overlap_policy + notify[]. Имя структуры = контрактное имя схемы в
// OpenAPI (huma DefaultSchemaNamer берёт reflect.Type.Name()) — выровнено под
// committed-рукопись (docs/keeper/openapi.yaml → CadenceCreateRequest, N4).
//
// huma-теги: `required:"true"` — обязательное поле (missing → 422); `enum:"…"` —
// допустимые значения (mismatch → 422); `doc:"…"` — описание. omitempty/pointer —
// опциональные поля. additionalProperties:false (huma-дефолт, НЕ снимается) →
// unknown-поле → error-override классифицирует как 400 (контракт кластера).
type CadenceCreateRequest struct {
	Name         string `json:"name" required:"true" doc:"человекочитаемое имя расписания"`
	Enabled      *bool  `json:"enabled,omitempty" doc:"вкл/выкл планировщика (default true)"`
	ScheduleKind string `json:"schedule_kind" required:"true" enum:"interval,cron" doc:"тип расписания"`

	IntervalSeconds *int   `json:"interval_seconds,omitempty" minimum:"30" doc:"период для schedule_kind=interval (минимум 30с — абсолютный poll_floor, ADR-046/048)"`
	CronExpr        string `json:"cron_expr,omitempty" doc:"cron-выражение для schedule_kind=cron"`
	OverlapPolicy   string `json:"overlap_policy" required:"true" enum:"skip,queue,parallel" doc:"политика наложения прогонов"`

	Kind         string         `json:"kind" required:"true" enum:"scenario,command" doc:"тип рецепта прогона"`
	ScenarioName string         `json:"scenario_name,omitempty" doc:"имя сценария для kind=scenario"`
	Module       string         `json:"module,omitempty" doc:"модуль для kind=command"`
	Input        map[string]any `json:"input,omitempty" doc:"параметры рецепта"`
	Target       VoyageTarget   `json:"target" required:"true" doc:"таргет прогона (резолвится на спавне)"`

	Batch        *string `json:"batch,omitempty" doc:"размер батча: N хостов/инкарнаций или N%"`
	BatchSize    *int    `json:"batch_size,omitempty" minimum:"1"`
	BatchPercent *int    `json:"batch_percent,omitempty" minimum:"1" maximum:"100"`
	Concurrency  *int    `json:"concurrency,omitempty" minimum:"1"`
	BatchMode    string  `json:"batch_mode,omitempty"`

	MaxFailures          *string `json:"max_failures,omitempty" doc:"порог провалов: N абсолют или N%"`
	FailThreshold        *int    `json:"fail_threshold,omitempty" minimum:"1"`
	InterBatchIntervalMS *int    `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int    `json:"inter_unit_interval_ms,omitempty"`
	RequireAlive         *bool   `json:"require_alive,omitempty"`
	OnFailure            string  `json:"on_failure,omitempty"`

	Notify []VoyageNotify `json:"notify,omitempty" doc:"подписки на уведомления о прогонах этого расписания"`
}

// Вложенные target/notify — единые api.VoyageTarget/api.VoyageNotify (huma_voyage_target.go),
// shared с voyage-доменом; форма выровнена под committed-рукопись (одна схема на каждую).

// cadenceCreateOutput — huma-output (FULL-TYPED). Status=201; Location — header;
// Body — typed 201-тело. Конверт доменной cadenceCreateReply → этот тип — в
// registerHumaCadence. Заменяет прежний пустой output + ручную запись в (w).
type cadenceCreateOutput struct {
	Status   int                `json:"-"`
	Location string             `header:"Location" json:"-"`
	Body     CadenceCreateReply `json:"-"`
}

// CadenceCreateReply — Go-форма 201-тела (источник схемы ответа И wire-формы).
// Совпадает с доменным CadenceCreateReply: все скаляры; NextRunAt nullable
// (*time.Time → RFC3339Nano при marshal, как legacy oapi-reply — wire-идентично).
// Имя структуры = контрактное имя схемы (huma DefaultSchemaNamer; рукопись
// CadenceCreateReply, N4). omitempty/nullable зафиксированы golden-JSON snapshot-
// тестом (wire-регресс-guard тиража).
type CadenceCreateReply struct {
	CadenceID string     `json:"cadence_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"ULID созданного расписания"` // ULID (audit.NewULID)
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	NextRunAt *time.Time `json:"next_run_at,omitempty" doc:"RFC3339 время следующего запуска"`
	Location  string     `json:"location" doc:"относительный URL ресурса"`
}

// cadenceCreateOperation — метаданные huma.Operation для POST /v1/cadences.
// RequestBody/Responses huma выводит АВТОМАТИЧЕСКИ из cadenceCreateInput.Body /
// cadenceCreateOutput при huma.Register (FULL-TYPED — схема и валидация из тех же
// Go-типов). Path = "/" — ОТНОСИТЕЛЬНЫЙ к chi-группе /v1/cadences, на которой
// смонтирован huma.API (chi смонтирует роут как /v1/cadences; chi.Walk видит его,
// drift-test зелёный). DefaultStatus=201 — успешный код (huma возьмёт его из
// output.Status, но фиксируем и в схеме). Errors фиксирует problem-коды
// (400 unknown/malformed, 403 RBAC-by-kind, 422 валидация рецепта/расписания, 500).
func cadenceCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createCadence",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать расписание (Cadence)",
		Description:   "Регулярный/повторяющийся Voyage (ADR-046). Двухуровневый RBAC: cadence.create + Voyage-permission по kind рецепта.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PATCH /v1/cadences/{id} (patch) — WRITE-SELF-AUDIT cadence.updated (200+body) ===
//
// PATCH-семантика — read-modify-write поверх full-replace cadence.Update: заданное
// поле → перезапись, опущенное → текущее значение сохраняется. Указатели на
// nullable-поля рецепта семантически «не различают» «опущено» и «явный null» — для
// MVP PATCH трактует ПРИСУТСТВИЕ ключа как перезапись (отсутствие → keep). Поэтому
// huma-форма — `*T omitempty` (НЕ Optional[T] presence-tier huma_optional.go):
// различение omitted/null/value НЕ требуется (nil-указатель = «не трогать», точная
// parity доменного cadencePatchRequest). kind в PATCH НЕ меняется (поле отсутствует
// в теле — смена kind = delete + create).

// cadencePatchInput — huma-input PATCH /v1/cadences/{id} (FULL-TYPED). ID — path
// (ULID-валидация — доменная, в PatchTyped). Body — typed PATCH-тело.
type cadencePatchInput struct {
	ID   string `path:"id" doc:"ULID расписания"`
	Body CadencePatchRequest
}

// CadencePatchRequest — Go-форма тела PATCH /v1/cadences/{id} (code-first источник
// схемы И валидации). Все поля опциональны (omitempty pointer): присутствие → set,
// отсутствие → keep. enum — closed-set (mismatch → 422). additionalProperties:false
// (huma-дефолт) → unknown-поле → 400. Повторяет доменный cadencePatchRequest. Имя
// структуры = контрактное имя схемы (huma DefaultSchemaNamer; рукопись
// CadencePatchRequest, N4).
type CadencePatchRequest struct {
	Name            *string `json:"name,omitempty" doc:"человекочитаемое имя расписания"`
	Enabled         *bool   `json:"enabled,omitempty" doc:"вкл/выкл планировщика"`
	ScheduleKind    *string `json:"schedule_kind,omitempty" enum:"interval,cron" doc:"тип расписания"`
	IntervalSeconds *int    `json:"interval_seconds,omitempty" minimum:"30" doc:"период для schedule_kind=interval (минимум 30с — абсолютный poll_floor, ADR-046/048)"`
	CronExpr        *string `json:"cron_expr,omitempty" doc:"cron-выражение (пустая строка → очистить)"`
	OverlapPolicy   *string `json:"overlap_policy,omitempty" enum:"skip,queue,parallel" doc:"политика наложения прогонов"`

	ScenarioName *string        `json:"scenario_name,omitempty" doc:"имя сценария (пустая строка → очистить)"`
	Module       *string        `json:"module,omitempty" doc:"модуль для kind=command (пустая строка → очистить)"`
	Input        map[string]any `json:"input,omitempty" doc:"параметры рецепта"`
	Target       *VoyageTarget  `json:"target,omitempty" doc:"таргет прогона"`

	Batch         *string `json:"batch,omitempty" doc:"размер батча: N хостов/инкарнаций или N%"`
	BatchSize     *int    `json:"batch_size,omitempty" minimum:"1"`
	BatchPercent  *int    `json:"batch_percent,omitempty" minimum:"1" maximum:"100"`
	Concurrency   *int    `json:"concurrency,omitempty" minimum:"1"`
	BatchMode     *string `json:"batch_mode,omitempty"`
	MaxFailures   *string `json:"max_failures,omitempty" doc:"порог провалов: N абсолют или N%"`
	FailThreshold *int    `json:"fail_threshold,omitempty" minimum:"1"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     *string `json:"on_failure,omitempty" doc:"abort|continue (пустая строка → очистить)"`
}

// cadencePatchOutput — huma-output PATCH /v1/cadences/{id} (FULL-TYPED). Status=200;
// Body — typed 200-тело (полный cadenceDTO обновлённого расписания).
type cadencePatchOutput struct {
	Body handlers.CadenceDTO
}

// cadencePatchOperation — метаданные PATCH /v1/cadences/{id}. DefaultStatus=200.
// WRITE-SELF-AUDIT: cadence.updated пишет САМ handler (PatchTyped → emitWrite), audit-
// middleware НЕ навешан. Errors: 400 unknown/malformed, 403 RBAC, 404 cadence_not_found,
// 422 валидация рецепта/расписания, 500.
func cadencePatchOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "patchCadence",
		Method:        http.MethodPatch,
		Path:          "/{id}",
		Summary:       "Обновить расписание (Cadence)",
		Description:   "Read-modify-write рецепта/расписания/enabled-toggle. Двухуровневый RBAC (cadence.update + Voyage-permission по kind). kind не меняется.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/cadences/{id} (delete) — WRITE-SELF-AUDIT cadence.deleted (204) ===

// cadenceDeleteInput — huma-input DELETE /v1/cadences/{id}. ID — path. Body нет.
type cadenceDeleteInput struct {
	ID string `path:"id" doc:"ULID расписания"`
}

// cadenceDeleteOutput — huma-output DELETE /v1/cadences/{id} (FULL-TYPED). Status=204;
// тела нет (Body не объявлен — huma 204 без content).
type cadenceDeleteOutput struct {
	Status int `json:"-"`
}

// cadenceDeleteOperation — метаданные DELETE /v1/cadences/{id}. DefaultStatus=204.
// WRITE-SELF-AUDIT: cadence.deleted пишет САМ handler (DeleteTyped → emitDeleted).
// Errors: 403 RBAC, 404 cadence_not_found, 422 невалидный id, 500.
func cadenceDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteCadence",
		Method:        http.MethodDelete,
		Path:          "/{id}",
		Summary:       "Снять расписание (Cadence)",
		Description:   "Удаляет расписание; порождённые Voyage остаются (FK ON DELETE SET NULL). Permission cadence.delete.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/cadences/{id}/enable|/disable (toggle) — WRITE-SELF-AUDIT cadence.updated (200+body) ===

// cadenceToggleInput — huma-input POST /v1/cadences/{id}/enable|/disable. ID — path.
// Body нет (toggle без тела).
type cadenceToggleInput struct {
	ID string `path:"id" doc:"ULID расписания"`
}

// cadenceToggleOutput — huma-output enable/disable (FULL-TYPED, handler-native T5d). Status=200;
// Body — huma-native 200-тело (api.CadenceEnabledReply: cadence_id + enabled). Конверт доменной
// handlers.CadenceEnabledReply → этот тип — в register-func.
type cadenceToggleOutput struct {
	Body CadenceEnabledReply
}

// CadenceEnabledReply — Go-форма 200-тела POST /v1/cadences/{id}/enable|/disable (источник
// схемы И wire-формы, handler-native T5d). Плоская форма 1:1 с прежним CadenceEnabledReply
// (cadence_id + enabled). Имя структуры = контрактное имя схемы (huma DefaultSchemaNamer).
type CadenceEnabledReply struct {
	CadenceID string `json:"cadence_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	Enabled   bool   `json:"enabled"`
}

// cadenceEnableOperation — метаданные POST /v1/cadences/{id}/enable. DefaultStatus=200.
// WRITE-SELF-AUDIT: cadence.updated пишет САМ handler (SetEnabledTyped → emitEnabledToggle).
// Errors: 403 RBAC, 404 cadence_not_found, 422 невалидный id, 500.
func cadenceEnableOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "enableCadence",
		Method:        http.MethodPost,
		Path:          "/{id}/enable",
		Summary:       "Включить расписание (Cadence)",
		Description:   "Возобновление планировщика. Permission cadence.enable ИЛИ backcompat cadence.update.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// cadenceDisableOperation — метаданные POST /v1/cadences/{id}/disable. DefaultStatus=200.
// WRITE-SELF-AUDIT: cadence.updated пишет САМ handler. Errors: 403, 404, 422, 500.
func cadenceDisableOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "disableCadence",
		Method:        http.MethodPost,
		Path:          "/{id}/disable",
		Summary:       "Выключить расписание (Cadence)",
		Description:   "Пауза планировщика. Permission cadence.disable ИЛИ backcompat cadence.update.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/cadences/{id} (get) — READ-with-path (БЕЗ audit) ===
//
// Перенос read-роутов GET/{id}+/runs на huma завершает cadence-домен целиком и
// СНИМАЕТ блокер sibling-саброутера r.Route("/{id}") (chi отдавал ВЕСЬ /{id}-узел
// строгому саброутеру → PATCH/DELETE huma-op были недостижимы, 405). Теперь все
// /{id}-роуты — huma-op с полным path относительно группы /v1/cadences, без
// chi.Route на том же узле.

// cadenceGetInput — huma-input GET /v1/cadences/{id}. ID — path (ULID-валидация —
// доменная, в GetTyped).
type cadenceGetInput struct {
	ID string `path:"id" doc:"ULID расписания"`
}

// cadenceGetOutput — huma-output GET /v1/cadences/{id} (FULL-TYPED). Body — typed
// 200-тело (полный cadenceDTO). Wire-форма byte-exact с legacy GET {id}.
type cadenceGetOutput struct {
	Body handlers.CadenceDTO
}

// cadenceGetOperation — метаданные GET /v1/cadences/{id}. DefaultStatus=200. READ-роут:
// audit НЕ навешан. Permission cadence.list (read-tier — как legacy strict GetCadence).
// Errors: 403 RBAC, 404 cadence_not_found, 422 невалидный id, 500.
func cadenceGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getCadence",
		Method:        http.MethodGet,
		Path:          "/{id}",
		Summary:       "Получить расписание (Cadence)",
		Description:   "Деталь расписания по ULID. Permission cadence.list. Read-only, без audit.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/cadences/{id}/runs (runs) — READ-with-typed-query (БЕЗ audit) ===

// cadenceRunsInput — huma-input GET /v1/cadences/{id}/runs (FULL-TYPED typed-query).
// ID — path (ULID). Statuses — multi-value (?status=X&status=Y) exact-match OR;
// enum-набор = полный домен voyage.Status (значение вне набора → 422). explode:true
// ОБЯЗАТЕЛЕН (huma-дефолт query-array — explode=false: comma-separated как одно
// значение → сломанный OR). offset/limit — int32 с default; диапазон enforce-ит
// CheckPageBounds в RunsTyped → 400 (НЕ huma min/max — parity legacy ParsePage); bad-int → 400.
type cadenceRunsInput struct {
	ID       string   `path:"id" doc:"ULID расписания"`
	Statuses []string `query:"status,explode" enum:"scheduled,pending,running,succeeded,failed,partial_failed,cancelled" doc:"multi-value ?status=X&status=Y — exact-match OR; значение вне enum → 422"`
	Offset   int32    `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit    int32    `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// cadenceRunsOutput — huma-output GET /v1/cadences/{id}/runs (FULL-TYPED). Body —
// typed 200-envelope (handlers.CadenceRunsReply: items/offset/limit/total). Wire-форма
// byte-exact с legacy (sharedapi.PagedResponse[voyageDTO]).
type cadenceRunsOutput struct {
	Body handlers.CadenceRunsReply
}

// cadenceRunsOperation — метаданные GET /v1/cadences/{id}/runs. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission incarnation.history (Voyage — история
// incarnation, как legacy strict ListCadenceRuns). Errors: 400 out-of-range pagination,
// 403 RBAC, 404 cadence_not_found, 422 bad id/status enum, 500.
func cadenceRunsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listCadenceRuns",
		Method:        http.MethodGet,
		Path:          "/{id}/runs",
		Summary:       "Прогоны расписания (Cadence runs, paged)",
		Description:   "Список Voyage, порождённых расписанием, с фильтром status[] и пагинацией. Permission incarnation.history. Read-only, без audit.",
		Tags:          []string{"cadence"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
