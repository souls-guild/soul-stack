package api

// FULL-TYPED форма AUGUR-домена (omens + rites; code-first источник OpenAPI,
// ADR-054 §Pattern). ТИРАЖ-БАТЧ-2b (augur целиком на huma по эталонам role/operator):
// omen create (WRITE+AUDIT omen.created), omen list (read-with-typed-query),
// omen get (read-with-path), omen delete (WRITE+AUDIT omen.revoked); rite create
// (WRITE+AUDIT rite.created), rite list (read-with-typed-query, обязательный omen=),
// rite delete (WRITE+AUDIT rite.revoked). Go-типы — единственный источник правды
// (JSON Schema + валидация + typed-output).

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/augur/omens (create) — WRITE+AUDIT omen.created ===

// omenCreateInput — huma-input POST /v1/augur/omens (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме из huma-тегов.
type omenCreateInput struct {
	Body OmenCreateRequest
}

// OmenCreateRequest — Go-форма тела POST /v1/augur/omens (code-first источник
// схемы И валидации, handler-native). Имя Omen-а + source_type (enum
// vault/prometheus/elk) + endpoint + auth_ref (vault-ref).
//
// huma-теги: required:"true" — обязательное (missing→422); enum source_type —
// значение вне набора → 422 (schema-validate, не доменная проверка дубль).
// additionalProperties:false (huma-дефолт) → unknown-поле → 400. Формат name/
// endpoint/auth_ref — доменная валидация в CreateOmenTyped (422). source_type
// inline-enum (рукопись НЕ выносит его в standalone components/schemas — мех-2
// пропущен). Имя структуры = контрактное имя схемы в OpenAPI (DefaultSchemaNamer
// берёт reflect.Type.Name()) — выровнено под committed-рукопись (OmenCreateRequest).
type OmenCreateRequest struct {
	Name       string `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Omen-а (kebab-case, 1..63)"`
	SourceType string `json:"source_type" required:"true" enum:"vault,prometheus,elk" doc:"тип внешней системы; значение вне enum → 422"`
	Endpoint   string `json:"endpoint" required:"true" doc:"URL внешней системы (не секрет)"`
	AuthRef    string `json:"auth_ref" required:"true" doc:"vault-ref на master-credential (vault:<mount>/<path>); сам секрет не хранится"`
}

// omenCreateOutput — huma-output POST /v1/augur/omens (FULL-TYPED). Status=201;
// Body — huma-native 201-тело (OmenView). Wire-форма
// (created_by_aid nullable, created_at секундной точности) зафиксирована golden-JSON
// byte-exact-тестом (huma_augur_reply_test.go).
type omenCreateOutput struct {
	Status int `json:"-"`
	Body   OmenView
}

// omenCreateOperation — метаданные POST /v1/augur/omens. Path = "/omens"
// относительно chi-группы /v1/augur (полный под-/augur путь — distinct-path для
// spec-dump, иначе коллизия с rite-POST). DefaultStatus=201. Permission omen.create
// + audit omen.created. Errors: 400 unknown/malformed, 403 RBAC, 409 omen-exists,
// 422 валидация name/source_type/endpoint/auth_ref, 500.
func omenCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createOmen",
		Method:        http.MethodPost,
		Path:          "/omens",
		Summary:       "Создать Omen",
		Description:   "Заносит Omen (внешняя система) в реестр augur (ADR-025). Permission omen.create. 409 — name занят. master-credential не хранится (только auth_ref).",
		Tags:          []string{"augur"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/augur/omens (list) — READ-with-typed-query (БЕЗ audit) ===

// omenListInput — huma-input GET /v1/augur/omens (FULL-TYPED typed-query).
// offset/limit — int32 (НЕ Go-int: huma на int эмитит int64, committed-спека несёт
// int32) с default (offset 0, limit 50, совпадает с shared/api.ParsePage). bad-int →
// 400 (parseInto). ГРАНИЦЫ диапазона enforce-ит ДОМЕННАЯ ListOmensTyped через
// CheckPageBounds → 400 (НЕ huma minimum/maximum, иначе 422 — wire-change против
// легаси ParsePage 400).
type omenListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// omenListOutput — huma-output GET /v1/augur/omens (FULL-TYPED). Body — huma-native
// 200-envelope (OmenListReply: items/offset/limit/total). Wire-форма items зафиксирована
// golden-JSON byte-exact-тестом.
type omenListOutput struct {
	Body OmenListReply
}

// omenListOperation — метаданные GET /v1/augur/omens. Path = "/omens" относительно
// chi-группы /v1/augur. DefaultStatus=200. READ-роут: audit НЕ навешан.
// Errors: 400 (out-of-range pagination), 403 RBAC, 500.
func omenListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listOmens",
		Method:        http.MethodGet,
		Path:          "/omens",
		Summary:       "Список Omen-ов (paged)",
		Description:   "Реестр Omen-ов с пагинацией (ADR-025). Permission omen.list. Read-only, без audit.",
		Tags:          []string{"augur"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/augur/omens/{name} (get) — READ-with-path (БЕЗ audit) ===

// omenGetInput — huma-input GET /v1/augur/omens/{name}. Name — path-параметр.
// Формат name (reOmenName) — доменная валидация в GetOmenTyped (422).
type omenGetInput struct {
	Name string `path:"name" doc:"имя Omen-а"`
}

// omenGetOutput — huma-output GET /v1/augur/omens/{name} (FULL-TYPED). Body —
// huma-native 200-тело (OmenView). Wire-форма зафиксирована golden-тестом.
type omenGetOutput struct {
	Body OmenView
}

// omenGetOperation — метаданные GET /v1/augur/omens/{name}. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission omen.list (read покрыт list-правом).
// Errors: 403 RBAC, 404 not-found, 422 bad path-name, 500.
func omenGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getOmen",
		Method:        http.MethodGet,
		Path:          "/omens/{name}",
		Summary:       "Карточка Omen-а",
		Description:   "Метаданные одного Omen-а по имени (ADR-025). Permission omen.list (read покрыт list-правом). Read-only, без audit.",
		Tags:          []string{"augur"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/augur/omens/{name} (delete) — WRITE+AUDIT omen.revoked ===

// omenDeleteInput — huma-input DELETE /v1/augur/omens/{name}. Name — path. Body нет.
type omenDeleteInput struct {
	Name string `path:"name" doc:"имя Omen-а"`
}

// augurNoContentOutput — общий huma-output 204-write-роутов augur (omen.delete /
// rite.delete). БЕЗ Body (легаси-контракт: 204 No Content). huma на output без Body
// делает SetStatus(204) → пустое тело (wire-идентично прежнему WriteHeader(204)).
type augurNoContentOutput struct {
	Status int `json:"-"`
}

// omenDeleteOperation — метаданные DELETE /v1/augur/omens/{name}. DefaultStatus=204.
// Permission omen.delete + audit omen.revoked (каскад чистит связанные Rite-ы).
// Errors: 403 RBAC, 404 not-found, 422 bad path-name, 500.
func omenDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteOmen",
		Method:        http.MethodDelete,
		Path:          "/omens/{name}",
		Summary:       "Удалить Omen",
		Description:   "Удаляет Omen каскадно (связанные Rite-ы, ADR-025). Permission omen.delete. 404 — записи нет.",
		Tags:          []string{"augur"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/augur/rites (create) — WRITE+AUDIT rite.created ===

// riteCreateInput — huma-input POST /v1/augur/rites (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме.
type riteCreateInput struct {
	Body RiteCreateRequest
}

// RiteCreateRequest — Go-форма тела POST /v1/augur/rites (code-first источник
// схемы И валидации, handler-native). omen + XOR-субъект (coven/sid) + allow
// (byte-passthrough JSONB, ADR-051 категория D) + delegate +
// token-поля. allow — json.RawMessage (required:"true"): сырые байты тела едут в
// service-валидатор напрямую. XOR-субъект и форма allow/token — доменная валидация
// в CreateRiteTyped (422). additionalProperties:false → unknown → 400. Имя структуры =
// контрактное имя схемы в OpenAPI (committed-рукопись → RiteCreateRequest).
type RiteCreateRequest struct {
	Omen         string          `json:"omen" required:"true" doc:"Omen, к которому относится grant"`
	Coven        *string         `json:"coven,omitempty" doc:"субъект-grant по Coven-метке (XOR с sid)"`
	SID          *string         `json:"sid,omitempty" doc:"субъект-grant по конкретному SID (XOR с coven)"`
	Allow        json.RawMessage `json:"allow" required:"true" doc:"allow-list; форма по source_type Omen-а (передаётся как есть)"`
	Delegate     *bool           `json:"delegate,omitempty" doc:"false — брокер (MVP-1); true — делегация (MVP-2)"`
	TokenTTL     *string         `json:"token_ttl,omitempty" doc:"TTL минтуемого scoped-токена; только vault-delegate"`
	TokenNumUses *int            `json:"token_num_uses,omitempty" doc:"лимит использований токена; только vault-delegate"`
}

// riteCreateOutput — huma-output POST /v1/augur/rites (FULL-TYPED). Status=201;
// Body — huma-native 201-тело (RiteView). allow — byte-passthrough JSONB. Wire-форма
// зафиксирована golden-JSON byte-exact-тестом.
type riteCreateOutput struct {
	Status int `json:"-"`
	Body   RiteView
}

// riteCreateOperation — метаданные POST /v1/augur/rites. Path = "/rites"
// относительно chi-группы /v1/augur. DefaultStatus=201. Permission rite.create +
// audit rite.created. Errors: 400 unknown/malformed, 403 RBAC, 404 omen-not-found,
// 422 XOR-нарушение/битый allow/token-поля, 500.
func riteCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createRite",
		Method:        http.MethodPost,
		Path:          "/rites",
		Summary:       "Создать Rite (grant)",
		Description:   "Заносит Rite (grant) в реестр augur (ADR-025). Permission rite.create. 404 — Omen не существует. 422 — XOR-нарушение субъекта/битый allow.",
		Tags:          []string{"augur"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/augur/rites (list by omen) — READ-with-typed-query (БЕЗ audit) ===

// riteListInput — huma-input GET /v1/augur/rites?omen=<name>. Omen — обязательный
// query-фильтр (augur.md §6 — list-all без omen-скоупа отложен). Формат/наличие omen
// — доменная валидация в ListRitesTyped (422; query-string без enum, как path-name).
type riteListInput struct {
	Omen string `query:"omen" doc:"фильтр by-omen (обязателен в MVP); пустой/битый → 422"`
}

// riteListOutput — huma-output GET /v1/augur/rites (FULL-TYPED). Body — huma-native
// 200-тело (RiteListReply: items[] под `items`, БЕЗ offset/limit/total — list-by-omen
// без пагинации). Wire-форма зафиксирована golden-тестом.
type riteListOutput struct {
	Body RiteListReply
}

// riteListOperation — метаданные GET /v1/augur/rites. Path = "/rites" относительно
// chi-группы /v1/augur. DefaultStatus=200. READ-роут: audit НЕ навешан.
// Errors: 403 RBAC, 422 omen не передан/битый, 500.
func riteListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listRites",
		Method:        http.MethodGet,
		Path:          "/rites",
		Summary:       "Список Rite-ов по Omen",
		Description:   "Rite-ы (grant-ы) одного Omen-а (ADR-025). Permission rite.list. Обязательный фильтр omen=<name>. Read-only, без audit.",
		Tags:          []string{"augur"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/augur/rites/{id} (delete) — WRITE+AUDIT rite.revoked ===

// riteDeleteInput — huma-input DELETE /v1/augur/rites/{id}. ID — path-параметр
// (string: положительное число валидирует доменная DeleteRiteTyped → 422 на не-числе).
type riteDeleteInput struct {
	ID string `path:"id" doc:"числовой id Rite-а"`
}

// riteDeleteOperation — метаданные DELETE /v1/augur/rites/{id}. DefaultStatus=204.
// Permission rite.delete + audit rite.revoked. Errors: 403 RBAC, 404 not-found, 422
// bad path-id (не положительное число), 500.
func riteDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteRite",
		Method:        http.MethodDelete,
		Path:          "/rites/{id}",
		Summary:       "Удалить Rite",
		Description:   "Снимает grant-запись Rite по id (ADR-025). Permission rite.delete. 404 — записи нет. 422 — id не положительное число.",
		Tags:          []string{"augur"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
