package api

// FULL-TYPED форма HERALD-домена (heralds + tidings; code-first источник OpenAPI,
// ADR-054 §Pattern). ТИРАЖ-БАТЧ-2c (herald целиком на huma по эталонам role/
// operator/augur/push-provider): herald create/update/delete — WRITE+AUDIT
// (herald.created/.updated/.deleted), herald list/get — read; tiding create/update/
// delete — WRITE+AUDIT (tiding.created/.updated/.deleted), tiding list/get — read.
// Go-типы — единственный источник правды (JSON Schema + валидация + typed-output).
//
// update — PUT replace-семантика (НЕ presence-tier): *T omitempty в body, omit==clear
// (урок N4). Optional[T] здесь не нужен — FE шлёт правило целиком.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/heralds (create) — WRITE+AUDIT herald.created ===

// heraldCreateInput — huma-input POST /v1/heralds (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме из huma-тегов.
type heraldCreateInput struct {
	Body HeraldCreateRequest
}

// HeraldCreateRequest — Go-форма тела POST /v1/heralds (code-first источник схемы И
// валидации). Повторяет доменный HeraldCreateRequest: имя канала + type
// (enum webhook в MVP) + config (per-type, для webhook url+опц.) + опц. secret_ref
// (vault-ref) + опц. enabled.
//
// huma-теги: required:"true" — обязательное (missing→422); enum type — значение вне
// набора → 422 (schema-validate; inline-enum, рукопись НЕ выносит type в standalone
// components/schemas — мех-2 пропущен). additionalProperties:false (huma-дефолт) →
// unknown-поле → 400. Формат name/config/secret_ref — доменная валидация в
// CreateHeraldTyped (422). Имя структуры = контрактное имя схемы в OpenAPI
// (committed-рукопись → HeraldCreateRequest).
type HeraldCreateRequest struct {
	Name      string         `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Herald-канала (kebab-case, 1..63), уникальное в кластере"`
	Type      string         `json:"type" required:"true" enum:"custom,discord,email,mattermost,slack,telegram,webhook" doc:"тип канала (closed-enum: webhook|telegram|slack|mattermost|discord|custom|email); значение вне enum → 422"`
	Config    map[string]any `json:"config" required:"true" doc:"per-type config (форма зависит от type; см. каталог GET /v1/herald-types). Секрет канала (bot_token/webhook_url/header_secret) — dual-mode: значение (plaintext) ИЛИ *_ref (vault-путь)"`
	SecretRef *string        `json:"secret_ref,omitempty" doc:"опц. vault-ref на webhook signing-token (vault:<mount>/<path>); XOR с secret"`
	Secret    *string        `json:"secret,omitempty" doc:"опц. plaintext webhook signing-token (dual-mode, ADR-064): keeper пишет его в Vault сам; XOR с secret_ref. Требует TLS-фронта (secret_ingest.accept_plaintext)"`
	Enabled   *bool          `json:"enabled,omitempty" doc:"канал включён (опущено → true)"`
}

// heraldCreateOutput — huma-output POST /v1/heralds (FULL-TYPED). Status=201; Body —
// typed 201-тело (huma-native api.Herald, T5b — конверт legacy-генерата→native в register-func).
// Wire-форма (created_by_aid omitempty, secret_ref nullable, created_at/updated_at)
// зафиксирована golden-JSON byte-exact-тестом.
type heraldCreateOutput struct {
	Status int `json:"-"`
	Body   Herald
}

// heraldCreateOperation — метаданные POST /v1/heralds. Path = "/" относительно
// chi-группы /v1/heralds. DefaultStatus=201. Permission herald.create + audit
// herald.created. Errors: 400 unknown/malformed, 403 RBAC, 409 herald-exists, 422
// валидация name/type/config/secret_ref, 500.
func heraldCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createHerald",
		Method:        http.MethodPost,
		Path:          "/heralds",
		Summary:       "Создать Herald-канал",
		Description:   "Заносит Herald (канал доставки уведомлений) в реестр (ADR-052). Permission herald.create. 409 — name занят. Секрет не хранится (только secret_ref).",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/heralds (list) — READ-with-typed-query (БЕЗ audit) ===

// heraldListInput — huma-input GET /v1/heralds (FULL-TYPED typed-query). offset/limit —
// int32 с default (offset 0, limit 50, совпадает с shared/api.ParsePage). bad-int →
// 400 (parseInto). ГРАНИЦЫ диапазона enforce-ит ДОМЕННАЯ ListHeraldsTyped через
// CheckPageBounds → 400 (НЕ huma minimum/maximum, иначе 422 — wire-change против
// легаси ParsePage 400).
type heraldListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// heraldListOutput — huma-output GET /v1/heralds (FULL-TYPED). Body — typed
// 200-envelope (huma-native api.HeraldListReply: items/offset/limit/total; element
// api.Herald). Wire-форма items зафиксирована golden-JSON byte-exact-тестом.
type heraldListOutput struct {
	Body HeraldListReply
}

// heraldListOperation — метаданные GET /v1/heralds. Path = "/" относительно chi-группы
// /v1/heralds. DefaultStatus=200. READ-роут: audit НЕ навешан. Errors: 400 (out-of-
// range pagination), 403 RBAC, 500.
func heraldListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listHeralds",
		Method:        http.MethodGet,
		Path:          "/heralds",
		Summary:       "Список Herald-каналов (paged)",
		Description:   "Реестр Herald-каналов с пагинацией (ADR-052). Permission herald.list. Read-only, без audit.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/heralds/{name} (get) — READ-with-path (БЕЗ audit) ===

// heraldGetInput — huma-input GET /v1/heralds/{name}. Name — path-параметр. Формат
// name (herald.NamePattern) — доменная валидация в GetHeraldTyped (422).
type heraldGetInput struct {
	Name string `path:"name" doc:"имя Herald-канала"`
}

// heraldGetOutput — huma-output GET /v1/heralds/{name} (FULL-TYPED). Body — typed
// 200-тело (huma-native api.Herald). Wire-форма зафиксирована golden-тестом.
type heraldGetOutput struct {
	Body Herald
}

// heraldGetOperation — метаданные GET /v1/heralds/{name}. DefaultStatus=200. READ-
// роут: audit НЕ навешан. Permission herald.read. Errors: 403 RBAC, 404 not-found,
// 422 bad path-name, 500.
func heraldGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getHerald",
		Method:        http.MethodGet,
		Path:          "/heralds/{name}",
		Summary:       "Карточка Herald-канала",
		Description:   "Метаданные одного Herald-канала по имени (ADR-052). Permission herald.read. Read-only, без audit.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/heralds/{name} (update) — WRITE+AUDIT herald.updated ===

// heraldUpdateInput — huma-input PUT /v1/heralds/{name}. Name — path; Body — typed
// тело (replace-семантика).
type heraldUpdateInput struct {
	Name string `path:"name" doc:"имя Herald-канала (immutable)"`
	Body HeraldUpdateRequest
}

// HeraldUpdateRequest — Go-форма тела PUT /v1/heralds/{name} (replace-семантика:
// поля полностью заменяют существующие, name immutable). type/config обязательны;
// secret_ref/enabled опциональны. Имя структуры = контрактное имя схемы в OpenAPI
// (committed-рукопись → HeraldUpdateRequest).
type HeraldUpdateRequest struct {
	Type      string         `json:"type" required:"true" enum:"custom,discord,email,mattermost,slack,telegram,webhook" doc:"тип канала (closed-enum: webhook|telegram|slack|mattermost|discord|custom|email)"`
	Config    map[string]any `json:"config" required:"true" doc:"per-type config (replace — полностью заменяет существующий). Секрет канала — dual-mode: значение (plaintext) ИЛИ *_ref"`
	SecretRef *string        `json:"secret_ref,omitempty" doc:"опц. vault-ref на signing-token; XOR с secret; отсутствие обоих очищает подпись"`
	Secret    *string        `json:"secret,omitempty" doc:"опц. plaintext webhook signing-token (dual-mode, ADR-064): keeper перезаписывает его в Vault по тому же пути; XOR с secret_ref"`
	Enabled   *bool          `json:"enabled,omitempty" doc:"канал включён (опущено → true)"`
}

// heraldUpdateOutput — huma-output PUT /v1/heralds/{name} (FULL-TYPED). Status=200 С
// ТЕЛОМ (huma-native api.Herald — обновлённая запись). Wire-форма зафиксирована golden-тестом.
type heraldUpdateOutput struct {
	Status int `json:"-"`
	Body   Herald
}

// heraldUpdateOperation — метаданные PUT /v1/heralds/{name}. DefaultStatus=200.
// Permission herald.update + audit herald.updated. Errors: 400 unknown/malformed,
// 403 RBAC, 404 not-found, 422 валидация body/path-name, 500.
func heraldUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateHerald",
		Method:        http.MethodPut,
		Path:          "/heralds/{name}",
		Summary:       "Обновить Herald-канал (replace)",
		Description:   "Replace-семантика: поля полностью заменяют существующие, name immutable (ADR-052). Permission herald.update. 404 — записи нет.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/heralds/{name} (delete) — WRITE+AUDIT herald.deleted ===

// heraldDeleteInput — huma-input DELETE /v1/heralds/{name}. Name — path. Body нет.
type heraldDeleteInput struct {
	Name string `path:"name" doc:"имя Herald-канала"`
}

// heraldNoContentOutput — общий huma-output 204-write-роутов herald (herald.delete /
// tiding.delete). БЕЗ Body (легаси-контракт: 204 No Content). huma на output без Body
// делает SetStatus(204) → пустое тело (wire-идентично прежнему WriteHeader(204)).
type heraldNoContentOutput struct {
	Status int `json:"-"`
}

// heraldDeleteOperation — метаданные DELETE /v1/heralds/{name}. DefaultStatus=204.
// Permission herald.delete + audit herald.deleted (каскад чистит связанные Tiding-ы).
// Errors: 403 RBAC, 404 not-found, 422 bad path-name, 500.
func heraldDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteHerald",
		Method:        http.MethodDelete,
		Path:          "/heralds/{name}",
		Summary:       "Удалить Herald-канал",
		Description:   "Удаляет Herald каскадно (связанные Tiding-ы, ADR-052). Permission herald.delete. 404 — записи нет.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/tidings (create) — WRITE+AUDIT tiding.created ===

// tidingCreateInput — huma-input POST /v1/tidings (FULL-TYPED). Body — типизированное
// тело: huma декодит и валидирует по схеме.
type tidingCreateInput struct {
	Body TidingCreateRequest
}

// TidingCreateRequest — Go-форма тела POST /v1/tidings (code-first источник схемы И
// валидации). Повторяет доменный TidingCreateRequest: имя правила + herald (FK)
// + event_types (run-scope) + опц. фильтры/селекторы + annotations/projection.
// ephemeral/voyage_id отсутствуют — серверные (ADR-052(g)). Формат name/event_types/
// projection — доменная валидация в CreateTidingTyped (422/409/404). Имя структуры =
// контрактное имя схемы в OpenAPI (committed-рукопись → TidingCreateRequest).
type TidingCreateRequest struct {
	Name         string          `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Tiding-правила (kebab-case, 1..63)"`
	Herald       string          `json:"herald" required:"true" doc:"имя Herald-канала доставки (FK на heralds.name)"`
	EventTypes   []string        `json:"event_types" required:"true" doc:"список event-types в scope прогонов (area-glob или точный); пустой → 422"`
	OnlyFailures *bool           `json:"only_failures,omitempty" doc:"доставлять только провалы (опущено → false)"`
	OnlyChanges  *bool           `json:"only_changes,omitempty" doc:"доставлять только при изменениях (опущено → false)"`
	Incarnation  *string         `json:"incarnation,omitempty" doc:"опц. селектор привязки к инкарнации-источнику"`
	Cadence      *string         `json:"cadence,omitempty" doc:"опц. селектор привязки к Cadence-расписанию-источнику"`
	Task         *string         `json:"task,omitempty" doc:"опц. селектор подписки на конкретную задачу (register ∪ id из changed_tasks)"`
	Annotations  *map[string]any `json:"annotations,omitempty" doc:"статические поля оператора, мержатся в тело webhook ключом annotations"`
	Projection   *[]string       `json:"projection,omitempty" doc:"allow-list путей payload; пусто/опущено — полная форма"`
	Enabled      *bool           `json:"enabled,omitempty" doc:"правило включено (опущено → true)"`
}

// tidingCreateOutput — huma-output POST /v1/tidings (FULL-TYPED). Status=201; Body —
// typed 201-тело (huma-native api.Tiding). Wire-форма зафиксирована golden-JSON byte-exact-тестом.
type tidingCreateOutput struct {
	Status int `json:"-"`
	Body   Tiding
}

// tidingCreateOperation — метаданные POST /v1/tidings. Path = "/" относительно
// chi-группы /v1/tidings. DefaultStatus=201. Permission tiding.create + audit
// tiding.created. Errors: 400 unknown/malformed, 403 RBAC, 404 herald-not-found
// (FK), 409 tiding-exists, 422 валидация name/event_types, 500.
func tidingCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createTiding",
		Method:        http.MethodPost,
		Path:          "/tidings",
		Summary:       "Создать Tiding-правило",
		Description:   "Заносит постоянное Tiding-правило подписки (ADR-052). Permission tiding.create. 404 — Herald-канал не существует. 409 — name занят.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/tidings (list) — READ-with-typed-query (БЕЗ audit) ===

// tidingListInput — huma-input GET /v1/tidings (FULL-TYPED typed-query). offset/limit
// — int32 с default (parity ParsePage; out-of-range → 400 через CheckPageBounds).
// include_ephemeral — typed bool с default false (опущено → false; bad bool → 400 на
// huma-bind-фазе, parity легаси strconv.ParseBool 400).
type tidingListInput struct {
	Offset           int32 `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit            int32 `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	IncludeEphemeral bool  `query:"include_ephemeral" default:"false" doc:"отдавать разовые (ephemeral) правила (отладка); опущено → false скрывает разовые (ADR-052(g))"`
}

// tidingListOutput — huma-output GET /v1/tidings (FULL-TYPED). Body — typed
// 200-envelope (huma-native api.TidingListReply: items/offset/limit/total; element
// api.Tiding). Wire-форма items зафиксирована golden-JSON byte-exact-тестом.
type tidingListOutput struct {
	Body TidingListReply
}

// tidingListOperation — метаданные GET /v1/tidings. Path = "/" относительно chi-группы
// /v1/tidings. DefaultStatus=200. READ-роут: audit НЕ навешан. Errors: 400 (out-of-
// range pagination / bad include_ephemeral), 403 RBAC, 500.
func tidingListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listTidings",
		Method:        http.MethodGet,
		Path:          "/tidings",
		Summary:       "Список Tiding-правил (paged)",
		Description:   "Реестр Tiding-правил с пагинацией (ADR-052). Permission tiding.list. По умолчанию скрывает разовые (include_ephemeral=true отдаёт все). Read-only, без audit.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/tidings/{name} (get) — READ-with-path (БЕЗ audit) ===

// tidingGetInput — huma-input GET /v1/tidings/{name}. Name — path-параметр. Формат
// name (herald.NamePattern) — доменная валидация в GetTidingTyped (422).
type tidingGetInput struct {
	Name string `path:"name" doc:"имя Tiding-правила"`
}

// tidingGetOutput — huma-output GET /v1/tidings/{name} (FULL-TYPED). Body — typed
// 200-тело (huma-native api.Tiding). Wire-форма зафиксирована golden-тестом.
type tidingGetOutput struct {
	Body Tiding
}

// tidingGetOperation — метаданные GET /v1/tidings/{name}. DefaultStatus=200. READ-
// роут: audit НЕ навешан. Permission tiding.read. Errors: 403 RBAC, 404 not-found,
// 422 bad path-name, 500.
func tidingGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getTiding",
		Method:        http.MethodGet,
		Path:          "/tidings/{name}",
		Summary:       "Карточка Tiding-правила",
		Description:   "Метаданные одного Tiding-правила по имени (ADR-052). Permission tiding.read. Read-only, без audit.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/tidings/{name} (update) — WRITE+AUDIT tiding.updated ===

// tidingUpdateInput — huma-input PUT /v1/tidings/{name}. Name — path; Body — typed
// тело (replace-семантика).
type tidingUpdateInput struct {
	Name string `path:"name" doc:"имя Tiding-правила (immutable)"`
	Body TidingUpdateRequest
}

// TidingUpdateRequest — Go-форма тела PUT /v1/tidings/{name} (replace-семантика:
// поля полностью заменяют существующие, name immutable; omit==clear для опц. полей —
// урок N4). herald/event_types обязательны; ephemeral/voyage_id отсутствуют (серверные).
// Имя структуры = контрактное имя схемы в OpenAPI (committed-рукопись → TidingUpdateRequest).
type TidingUpdateRequest struct {
	Herald       string          `json:"herald" required:"true" doc:"имя Herald-канала доставки (FK)"`
	EventTypes   []string        `json:"event_types" required:"true" doc:"список event-types в scope прогонов (replace)"`
	OnlyFailures *bool           `json:"only_failures,omitempty" doc:"доставлять только провалы (опущено → false)"`
	OnlyChanges  *bool           `json:"only_changes,omitempty" doc:"доставлять только при изменениях (опущено → false)"`
	Incarnation  *string         `json:"incarnation,omitempty" doc:"опц. селектор привязки к инкарнации; отсутствие очищает"`
	Cadence      *string         `json:"cadence,omitempty" doc:"опц. селектор привязки к Cadence; отсутствие очищает"`
	Task         *string         `json:"task,omitempty" doc:"опц. селектор подписки на задачу; отсутствие очищает"`
	Annotations  *map[string]any `json:"annotations,omitempty" doc:"статические поля оператора (replace — отсутствие очищает)"`
	Projection   *[]string       `json:"projection,omitempty" doc:"allow-list путей payload (replace — отсутствие = полная форма)"`
	Enabled      *bool           `json:"enabled,omitempty" doc:"правило включено (опущено → true)"`
}

// tidingUpdateOutput — huma-output PUT /v1/tidings/{name} (FULL-TYPED). Status=200 С
// ТЕЛОМ (huma-native api.Tiding — обновлённая запись). Wire-форма зафиксирована golden-тестом.
type tidingUpdateOutput struct {
	Status int `json:"-"`
	Body   Tiding
}

// tidingUpdateOperation — метаданные PUT /v1/tidings/{name}. DefaultStatus=200.
// Permission tiding.update + audit tiding.updated. Errors: 400 unknown/malformed,
// 403 RBAC, 404 not-found/herald-not-found, 422 валидация body, 500.
func tidingUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateTiding",
		Method:        http.MethodPut,
		Path:          "/tidings/{name}",
		Summary:       "Обновить Tiding-правило (replace)",
		Description:   "Replace-семантика: поля полностью заменяют существующие, name immutable (ADR-052). Permission tiding.update. 404 — записи/Herald нет.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/tidings/{name} (delete) — WRITE+AUDIT tiding.deleted ===

// tidingDeleteInput — huma-input DELETE /v1/tidings/{name}. Name — path. Body нет.
type tidingDeleteInput struct {
	Name string `path:"name" doc:"имя Tiding-правила"`
}

// tidingDeleteOperation — метаданные DELETE /v1/tidings/{name}. DefaultStatus=204.
// Permission tiding.delete + audit tiding.deleted. Errors: 403 RBAC, 404 not-found,
// 422 bad path-name, 500.
func tidingDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteTiding",
		Method:        http.MethodDelete,
		Path:          "/tidings/{name}",
		Summary:       "Удалить Tiding-правило",
		Description:   "Снимает Tiding-правило подписки по имени (ADR-052). Permission tiding.delete. 404 — записи нет.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
