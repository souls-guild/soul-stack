package api

// FULL-TYPED форма ERRAND-домена (list + get + cancel; code-first источник OpenAPI,
// ADR-054 §Pattern). ТИРАЖ-БАТЧ-2c (errand read+cancel на huma по эталонам augur/
// audit-endpoint): list — read-with-typed-query (started_after date-time→400,
// offset/limit→400, status enum, sid/module string/array); get — read-with-path
// (200 ErrandResult / 202 running); cancel — WRITE+AUDIT (errand.cancelled).
// Go-типы — единственный источник правды (JSON Schema + валидация + typed-output).

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/errands (list) — READ-with-typed-query (БЕЗ audit) ===

// errandListInput — huma-input GET /v1/errands (FULL-TYPED typed-query). offset/limit —
// int32 с default (parity ParsePage; out-of-range → 400 через CheckPageBounds, НЕ
// huma minimum/maximum). started_after — time.Time (date-time): bad value → 400 на
// huma-bind ДО делегации (ADR-051 Amendment 2026-06-10 — единственный source, прежний
// доменный 422 недостижим через router). status — enum закрытого набора (значение вне
// набора → 422). sid — string-фильтр (формат FQDN валидирует домен → 422). module —
// multi-value exact-match OR (?module=X&module=Y; explode-форма).
type errandListInput struct {
	SID          string    `query:"sid" doc:"фильтр по целевому Soul (FQDN); битый формат → 422"`
	Status       string    `query:"status" enum:"running,success,failed,timed_out,cancelled,module_not_allowed" doc:"фильтр по статусу Errand-а; значение вне enum → 422"`
	StartedAfter time.Time `query:"started_after" doc:"фильтр по началу (started_at > value, RFC3339); bad value → 400"`
	Modules      []string  `query:"module" explode:"true" doc:"multi-value exact-match OR по имени модуля (?module=X&module=Y)"`
	Offset       int32     `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit        int32     `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// errandListOutput — huma-output GET /v1/errands (FULL-TYPED). Body — typed
// 200-envelope (native api.ErrandListReply: items/offset/limit/total; element
// api.ErrandResult — проекция плоского handlers.ErrandResultView в register-func).
// Wire-форма items (status голая enum-строка, started_at/finished_at секундный RFC3339
// UTC) зафиксирована golden-JSON byte-exact-тестом.
type errandListOutput struct {
	Body ErrandListReply
}

// errandListOperation — метаданные GET /v1/errands. Path = "/errands" относительно
// chi-группы /v1 (полный под-/v1 путь — distinct-path для spec-dump). DefaultStatus=200.
// READ-роут: audit НЕ навешан. Errors: 400 (out-of-range pagination / bad started_after),
// 403 RBAC, 422 (bad sid format / bad status enum), 500.
func errandListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listErrands",
		Method:        http.MethodGet,
		Path:          "/errands",
		Summary:       "Список Errand-ов (paged)",
		Description:   "Реестр Errand-ов с фильтрами и пагинацией (ADR-033). Permission errand.list. Read-only, без audit.",
		Tags:          []string{"errand"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/errands/{errand_id} (get) — READ-with-path (БЕЗ audit) ===

// errandGetInput — huma-input GET /v1/errands/{errand_id}. ErrandID — path-параметр
// (ULID; пустой → 422 в GetTyped).
type errandGetInput struct {
	ErrandID string `path:"errand_id" doc:"ULID Errand-а"`
}

// errandGetOutput — huma-output GET /v1/errands/{errand_id} с ДВУМЯ success-кодами
// под одним OperationID (200 терминал ErrandResult / 202 running ErrandAccepted —
// разные тела). Status — field-конвенция huma (override response-кода: handler ставит
// 200 либо 202). Body — json.RawMessage: handler пред-маршалит выбранное тело (его
// схема в huma-фрагменте = `{}`, НЕ octet-stream — rawMessageType → пустой Schema;
// committed openapi.yaml несёт типизированные 200/ErrandResult + 202/ErrandAccepted,
// это авторитет, фрагмент лишь дублирует op-id/путь для drift-теста). Wire-тело
// register-func маршалит из native-проекции плоского доменного view-а
// (newErrandResult / newErrandAccepted) — байты идентичны легаси.
type errandGetOutput struct {
	Status int             `json:"-"`
	Body   json.RawMessage `json:"body"`
}

// errandGetOperation — метаданные GET /v1/errands/{errand_id}. DefaultStatus=200 (терминал).
// 202 (running) — дополнительный success-код (объявлен в Errors-наборе для документации
// контракта; huma-handler сам выставляет SetStatus(202) на running). READ-роут: audit НЕ
// навешан. Permission errand.list. Errors: 202 running, 403 RBAC, 404 not-found, 422 bad id, 500.
func errandGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getErrand",
		Method:        http.MethodGet,
		Path:          "/errands/{errand_id}",
		Summary:       "Состояние Errand-а",
		Description:   "Терминал-строка (200) либо running-poll (202) по ULID (ADR-033). Permission errand.list. Read-only, без audit.",
		Tags:          []string{"errand"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusAccepted, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/errands/{errand_id} (cancel) — WRITE+AUDIT errand.cancelled ===

// errandCancelInput — huma-input DELETE /v1/errands/{errand_id}. ErrandID — path. Body нет.
type errandCancelInput struct {
	ErrandID string `path:"errand_id" doc:"ULID Errand-а"`
}

// errandNoContentOutput — huma-output 204-cancel-роута errand. БЕЗ Body (легаси-контракт:
// 204 No Content). huma на output без Body делает SetStatus(204) → пустое тело.
type errandNoContentOutput struct {
	Status int `json:"-"`
}

// errandCancelOperation — метаданные DELETE /v1/errands/{errand_id}. DefaultStatus=204.
// Permission errand.cancel + audit errand.cancelled. Errors: 403 RBAC, 404 not-found
// (errand / soul-not-connected), 409 terminal (нечего отменять), 422 пустой id, 500.
func errandCancelOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "cancelErrand",
		Method:        http.MethodDelete,
		Path:          "/errands/{errand_id}",
		Summary:       "Отменить Errand",
		Description:   "Отправляет cancel-сигнал Soul-у (ADR-033, slice E5). Permission errand.cancel. 409 — уже терминал.",
		Tags:          []string{"errand"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
