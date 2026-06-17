package api

// FULL-TYPED форма ORACLE-домена (vigils + decrees; code-first источник OpenAPI,
// ADR-054 §Pattern). ТИРАЖ-БАТЧ-2b (oracle целиком на huma по эталонам role/operator/
// augur): vigil create (WRITE+AUDIT vigil.created), vigil list (read-with-typed-
// query), vigil get (read-with-path), vigil delete (WRITE+AUDIT vigil.deleted);
// decree симметрично (decree.created / decree.deleted). Go-типы — единственный
// источник правды (JSON Schema + валидация + typed-output).
//
// vigil/decree-операции несут ПОЛНЫЕ пути (/vigils[/{name}], /decrees[/{name}]) и
// монтируются на /v1 напрямую (per-route RBAC chi-группа) — distinct-path для
// spec-dump (иначе vigil-POST и decree-POST осели бы на одном «/»).

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/vigils (create) — WRITE+AUDIT vigil.created ===

// vigilCreateInput — huma-input POST /v1/vigils (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме.
type vigilCreateInput struct {
	Body VigilCreateRequest
}

// VigilCreateRequest — Go-форма тела POST /v1/vigils (code-first источник схемы И
// валидации). Повторяет доменный VigilCreateRequest: имя + XOR-субъект
// (coven/sid) + interval/check + params (byte-passthrough JSONB, ADR-051 категория D)
// + enabled. params — *json.RawMessage: сырые байты тела едут в service напрямую.
// XOR-субъект и форма interval/check/params — доменная валидация в CreateVigilTyped
// (422). required:"true" — missing→422; additionalProperties:false → unknown→400.
// Имя структуры = контрактное имя схемы в OpenAPI (committed-рукопись → VigilCreateRequest).
type VigilCreateRequest struct {
	Name     string           `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Vigil-а (kebab-case, 1..63)"`
	Coven    *[]string        `json:"coven,omitempty" doc:"субъект-метки coven (XOR с sid)"`
	SID      *string          `json:"sid,omitempty" doc:"субъект — один конкретный SID (XOR с coven)"`
	Interval string           `json:"interval" required:"true" doc:"частота проверки (duration-конвенция, напр. '30s')"`
	Check    string           `json:"check" required:"true" doc:"адрес core-beacon (напр. 'core.beacon.file_changed')"`
	Params   *json.RawMessage `json:"params,omitempty" doc:"параметры проверки; форма зависит от check (передаётся как есть)"`
	Enabled  *bool            `json:"enabled,omitempty" doc:"активна ли проверка (по умолчанию true)"`
}

// vigilCreateOutput — huma-output POST /v1/vigils (FULL-TYPED). Status=201; Body —
// native 201-тело (VigilView). params — byte-passthrough JSONB. Wire-форма
// зафиксирована golden-JSON byte-exact-тестом.
type vigilCreateOutput struct {
	Status int `json:"-"`
	Body   VigilView
}

// vigilCreateOperation — метаданные POST /v1/vigils. Path = "/vigils" (полный, для
// distinct spec-dump). DefaultStatus=201. Permission vigil.create + audit
// vigil.created. Errors: 400 unknown/malformed, 403 RBAC, 409 vigil-exists, 422
// валидация name/interval/check/субъект, 500.
func vigilCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createVigil",
		Method:        http.MethodPost,
		Path:          "/vigils",
		Summary:       "Создать Vigil",
		Description:   "Заносит Vigil (Soul-side проверку) в реестр oracle (ADR-030). Permission vigil.create. 409 — name занят.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/vigils (list) — READ-with-typed-query (БЕЗ audit) ===

// vigilListInput — huma-input GET /v1/vigils (FULL-TYPED typed-query). offset/limit
// — int32 (committed-спека несёт int32) с default. bad-int → 400 (parseInto). ГРАНИЦЫ
// enforce-ит CheckPageBounds в ListVigilsTyped → 400 (НЕ huma minimum/maximum).
type vigilListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// vigilListOutput — huma-output GET /v1/vigils (FULL-TYPED). Body — native
// 200-envelope (VigilListReply: items/offset/limit/total). Wire-форма зафиксирована
// golden-тестом.
type vigilListOutput struct {
	Body VigilListReply
}

// vigilListOperation — метаданные GET /v1/vigils. DefaultStatus=200. READ-роут:
// audit НЕ навешан. Errors: 400 (out-of-range pagination), 403 RBAC, 500.
func vigilListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listVigils",
		Method:        http.MethodGet,
		Path:          "/vigils",
		Summary:       "Список Vigil-ов (paged)",
		Description:   "Реестр Vigil-ов с пагинацией (ADR-030). Permission vigil.list. Read-only, без audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/vigils/{name} (get) — READ-with-path (БЕЗ audit) ===

// vigilGetInput — huma-input GET /v1/vigils/{name}. Name — path. Формат name
// (reOracleName) — доменная валидация в GetVigilTyped (422).
type vigilGetInput struct {
	Name string `path:"name" doc:"имя Vigil-а"`
}

// vigilGetOutput — huma-output GET /v1/vigils/{name} (FULL-TYPED). Body — native
// 200-тело (VigilView). Wire-форма зафиксирована golden-тестом.
type vigilGetOutput struct {
	Body VigilView
}

// vigilGetOperation — метаданные GET /v1/vigils/{name}. DefaultStatus=200. READ-роут:
// audit НЕ навешан. Permission vigil.list (read покрыт list-правом). Errors: 403,
// 404, 422 bad path-name, 500.
func vigilGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getVigil",
		Method:        http.MethodGet,
		Path:          "/vigils/{name}",
		Summary:       "Карточка Vigil-а",
		Description:   "Метаданные одного Vigil-а по имени (ADR-030). Permission vigil.list (read покрыт list-правом). Read-only, без audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/vigils/{name} (delete) — WRITE+AUDIT vigil.deleted ===

// vigilDeleteInput — huma-input DELETE /v1/vigils/{name}. Name — path. Body нет.
type vigilDeleteInput struct {
	Name string `path:"name" doc:"имя Vigil-а"`
}

// oracleNoContentOutput — общий huma-output 204-write-роутов oracle (vigil.delete /
// decree.delete). БЕЗ Body (легаси-контракт: 204 No Content). huma на output без Body
// делает SetStatus(204) → пустое тело (wire-идентично прежнему WriteHeader(204)).
type oracleNoContentOutput struct {
	Status int `json:"-"`
}

// vigilDeleteOperation — метаданные DELETE /v1/vigils/{name}. DefaultStatus=204.
// Permission vigil.delete + audit vigil.deleted. Errors: 403, 404, 422 bad path-name,
// 500.
func vigilDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteVigil",
		Method:        http.MethodDelete,
		Path:          "/vigils/{name}",
		Summary:       "Удалить Vigil",
		Description:   "Удаляет Vigil из реестра oracle (ADR-030). Permission vigil.delete. 404 — записи нет.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/decrees (create) — WRITE+AUDIT decree.created ===

// decreeCreateInput — huma-input POST /v1/decrees (FULL-TYPED). Body —
// типизированное тело.
type decreeCreateInput struct {
	Body DecreeCreateRequest
}

// DecreeCreateRequest — Go-форма тела POST /v1/decrees (code-first источник схемы
// И валидации). Повторяет доменный DecreeCreateRequest: имя + on_beacon +
// XOR-субъект (coven/sid) + incarnation_name + action_scenario/action_input
// (byte-passthrough JSONB) + where-CEL + cooldown + enabled. action_input —
// *json.RawMessage. Валидация субъекта/where-CEL/cooldown — доменная (422). Имя
// структуры = контрактное имя схемы в OpenAPI (committed-рукопись → DecreeCreateRequest).
type DecreeCreateRequest struct {
	Name            string           `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Decree-а (kebab-case, 1..63)"`
	OnBeacon        string           `json:"on_beacon" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Vigil-а, на чей Portent правило реагирует"`
	Coven           *[]string        `json:"coven,omitempty" doc:"субъект-метки coven (XOR с sid)"`
	SID             *string          `json:"sid,omitempty" doc:"субъект — один конкретный SID (XOR с coven)"`
	IncarnationName string           `json:"incarnation_name" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"таргет-incarnation реакции (обязательно)"`
	ActionScenario  string           `json:"action_scenario" required:"true" pattern:"^[a-z][a-z0-9_]*$" doc:"named scenario (whitelist; raw-команда отвергнута)"`
	ActionInput     *json.RawMessage `json:"action_input,omitempty" doc:"вход сценария (vault-ref едет как есть)"`
	Where           *string          `json:"where,omitempty" doc:"опц. CEL-предикат над event.data; compile-проверяется"`
	Cooldown        *string          `json:"cooldown,omitempty" doc:"минимальный интервал между срабатываниями per-(decree, subject)"`
	Enabled         *bool            `json:"enabled,omitempty" doc:"активно ли правило (по умолчанию true)"`
}

// decreeCreateOutput — huma-output POST /v1/decrees (FULL-TYPED). Status=201; Body —
// native 201-тело (DecreeView). action_input — byte-passthrough JSONB. Wire-форма
// зафиксирована golden-JSON byte-exact-тестом.
type decreeCreateOutput struct {
	Status int `json:"-"`
	Body   DecreeView
}

// decreeCreateOperation — метаданные POST /v1/decrees. Path = "/decrees".
// DefaultStatus=201. Permission decree.create + audit decree.created. Errors: 400
// unknown/malformed, 403 RBAC, 409 decree-exists, 422 валидация полей/субъекта/
// where-CEL/cooldown, 500.
func decreeCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createDecree",
		Method:        http.MethodPost,
		Path:          "/decrees",
		Summary:       "Создать Decree",
		Description:   "Заносит Decree (правило reactor) в реестр oracle (ADR-030). Permission decree.create. 409 — name занят.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/decrees (list) — READ-with-typed-query (БЕЗ audit) ===

// decreeListInput — huma-input GET /v1/decrees (FULL-TYPED typed-query). offset/limit
// — int32 с default; диапазон enforce-ит CheckPageBounds → 400.
type decreeListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// decreeListOutput — huma-output GET /v1/decrees (FULL-TYPED). Body — native
// 200-envelope (DecreeListReply: items/offset/limit/total).
type decreeListOutput struct {
	Body DecreeListReply
}

// decreeListOperation — метаданные GET /v1/decrees. DefaultStatus=200. READ-роут:
// audit НЕ навешан. Errors: 400 (out-of-range pagination), 403 RBAC, 500.
func decreeListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listDecrees",
		Method:        http.MethodGet,
		Path:          "/decrees",
		Summary:       "Список Decree-ов (paged)",
		Description:   "Реестр Decree-ов с пагинацией (ADR-030). Permission decree.list. Read-only, без audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/decrees/{name} (get) — READ-with-path (БЕЗ audit) ===

// decreeGetInput — huma-input GET /v1/decrees/{name}. Name — path. Формат name —
// доменная валидация в GetDecreeTyped (422).
type decreeGetInput struct {
	Name string `path:"name" doc:"имя Decree-а"`
}

// decreeGetOutput — huma-output GET /v1/decrees/{name} (FULL-TYPED). Body — native
// 200-тело (DecreeView).
type decreeGetOutput struct {
	Body DecreeView
}

// decreeGetOperation — метаданные GET /v1/decrees/{name}. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission decree.list (read покрыт list-правом).
// Errors: 403, 404, 422 bad path-name, 500.
func decreeGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getDecree",
		Method:        http.MethodGet,
		Path:          "/decrees/{name}",
		Summary:       "Карточка Decree-а",
		Description:   "Метаданные одного Decree-а по имени (ADR-030). Permission decree.list (read покрыт list-правом). Read-only, без audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/decrees/{name} (delete) — WRITE+AUDIT decree.deleted ===

// decreeDeleteInput — huma-input DELETE /v1/decrees/{name}. Name — path. Body нет.
type decreeDeleteInput struct {
	Name string `path:"name" doc:"имя Decree-а"`
}

// decreeDeleteOperation — метаданные DELETE /v1/decrees/{name}. DefaultStatus=204.
// Permission decree.delete + audit decree.deleted (каскад чистит cooldown-state).
// Errors: 403, 404, 422 bad path-name, 500.
func decreeDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteDecree",
		Method:        http.MethodDelete,
		Path:          "/decrees/{name}",
		Summary:       "Удалить Decree",
		Description:   "Удаляет Decree каскадно (cooldown-state, ADR-030). Permission decree.delete. 404 — записи нет.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
