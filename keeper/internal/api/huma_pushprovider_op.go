package api

// FULL-TYPED форма PUSH-PROVIDER-домена (code-first источник OpenAPI, ADR-054
// §Pattern). ТИРАЖ-БАТЧ-2b (push-provider целиком на huma по эталонам role/operator):
// create (WRITE+AUDIT push-provider.created), list (read-with-typed-query), get
// (read-with-path), update (WRITE+AUDIT push-provider.updated, PUT replace-семантика),
// delete (WRITE+AUDIT push-provider.deleted). Go-типы — единственный источник правды.
//
// update — PUT с replace-семантикой (params полностью заменяет существующий набор,
// read-modify-write на клиенте), НЕ presence-tier Optional[T]: у поля params нет
// «omitted vs null»-семантики (его всегда шлют целиком), поэтому Optional не нужен.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/push-providers (create) — WRITE+AUDIT push-provider.created ===

// pushProviderCreateInput — huma-input POST /v1/push-providers (FULL-TYPED). Body —
// типизированное тело.
type pushProviderCreateInput struct {
	Body PushProviderCreateRequest
}

// PushProviderCreateRequest — Go-форма тела POST /v1/push-providers (code-first
// источник схемы И валидации). name + опц. params (opaque map; sensitive-ключи —
// vault-refs). additionalProperties в params:true (opaque payload), но НА ВЕРХНЕМ
// уровне тела — false (huma-дефолт) → unknown поле тела → 400. Формат name и
// sensitive-инвариант params — доменная валидация в CreateTyped (422). Имя структуры =
// контрактное имя схемы (huma DefaultSchemaNamer берёт reflect.Type.Name()) — выровнено
// под committed-рукопись (тираж N3). Register-func проецирует в native
// handlers.PushProviderCreateInput.
type PushProviderCreateRequest struct {
	Name   string         `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"имя Push-Provider-а (= plugins.ssh_providers[].name)"`
	Params map[string]any `json:"params,omitempty" doc:"opaque params; sensitive — vault-refs (значения не логируются)"`
}

// pushProviderCreateOutput — huma-output POST /v1/push-providers (FULL-TYPED).
// Status=201; Body — native 201-тело (PushProvider). Wire-форма (params
// нормализован в {}, updated_by_aid nullable, date-time RFC3339Nano без Truncate)
// зафиксирована golden-JSON byte-exact-тестом.
type pushProviderCreateOutput struct {
	Status int `json:"-"`
	Body   PushProvider
}

// pushProviderCreateOperation — метаданные POST /v1/push-providers. Path = "/"
// относительно chi-группы /v1/push-providers. DefaultStatus=201. Permission
// push-provider.create + audit push-provider.created. Errors: 400 unknown/malformed,
// 403 RBAC, 409 push-provider-exists, 422 валидация name/sensitive-param, 500.
func pushProviderCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createPushProvider",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать Push-Provider",
		Description:   "Заносит Push-Provider (per-provider env-payload, ADR-032 S7-2). Permission push-provider.create. 409 — name занят. sensitive-ключи обязаны быть vault-refs.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/push-providers (list) — READ-with-typed-query (БЕЗ audit) ===

// pushProviderListInput — huma-input GET /v1/push-providers (FULL-TYPED typed-query).
// name_pattern — LIKE-prefix-фильтр (string). offset/limit — int32 с default; диапазон
// enforce-ит CheckPageBounds в ListTyped → 400 (НЕ huma minimum/maximum). bad-int →
// 400 (parseInto).
type pushProviderListInput struct {
	NamePattern string `query:"name_pattern" doc:"LIKE-prefix-фильтр по имени (опц.)"`
	Offset      int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit       int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// pushProviderListOutput — huma-output GET /v1/push-providers (FULL-TYPED). Body —
// native 200-envelope (PushProviderListReply: items/offset/limit/total).
// Wire-форма зафиксирована golden-тестом.
type pushProviderListOutput struct {
	Body PushProviderListReply
}

// pushProviderListOperation — метаданные GET /v1/push-providers. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Errors: 400 (out-of-range pagination), 403 RBAC, 500.
func pushProviderListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listPushProviders",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Push-Provider-ов (paged)",
		Description:   "Реестр Push-Provider-ов с пагинацией и фильтром name_pattern (ADR-032 S7-2). Permission push-provider.list. Read-only, без audit.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/push-providers/{name} (get) — READ-with-path (БЕЗ audit) ===

// pushProviderGetInput — huma-input GET /v1/push-providers/{name}. Name — path.
// Формат name (ValidName) — доменная валидация в GetTyped (422).
type pushProviderGetInput struct {
	Name string `path:"name" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"имя Push-Provider-а"`
}

// pushProviderGetOutput — huma-output GET /v1/push-providers/{name} (FULL-TYPED).
// Body — native 200-тело (PushProvider).
type pushProviderGetOutput struct {
	Body PushProvider
}

// pushProviderGetOperation — метаданные GET /v1/push-providers/{name}.
// DefaultStatus=200. READ-роут: audit НЕ навешан. Permission push-provider.read.
// Errors: 403, 404, 422 bad path-name, 500.
func pushProviderGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getPushProvider",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Карточка Push-Provider-а",
		Description:   "Метаданные одного Push-Provider-а по имени (ADR-032 S7-2). Permission push-provider.read. Read-only, без audit.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/push-providers/{name} (update) — WRITE+AUDIT push-provider.updated ===

// pushProviderUpdateInput — huma-input PUT /v1/push-providers/{name}. Name — path;
// Body — typed тело (replace-семантика params).
type pushProviderUpdateInput struct {
	Name string `path:"name" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"имя Push-Provider-а"`
	Body PushProviderUpdateRequest
}

// PushProviderUpdateRequest — Go-форма тела PUT /v1/push-providers/{name}
// (replace-семантика: params полностью заменяет существующий набор). Params
// required:"true" (PUT шлёт полный новый набор; пустой {} легитимен — обнуляет
// params). НЕ Optional[T]: read-modify-write на клиенте, presence-различения нет.
// Имя структуры = контрактное имя схемы (huma DefaultSchemaNamer) — выровнено под
// committed-рукопись (тираж N3). Register-func проецирует в native
// handlers.PushProviderUpdateInput.
type PushProviderUpdateRequest struct {
	Params map[string]any `json:"params" required:"true" doc:"полный новый набор params (replace); sensitive — vault-refs"`
}

// pushProviderUpdateOutput — huma-output PUT /v1/push-providers/{name} (FULL-TYPED).
// Status=200; Body — native 200-тело (PushProvider).
type pushProviderUpdateOutput struct {
	Status int `json:"-"`
	Body   PushProvider
}

// pushProviderUpdateOperation — метаданные PUT /v1/push-providers/{name}.
// DefaultStatus=200. Permission push-provider.update + audit push-provider.updated.
// Errors: 400 unknown/malformed, 403 RBAC, 404 not-found, 422 bad path-name/
// sensitive-param, 500.
func pushProviderUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updatePushProvider",
		Method:        http.MethodPut,
		Path:          "/{name}",
		Summary:       "Заменить params Push-Provider-а",
		Description:   "Replace-семантика: params полностью заменяет существующий набор (ADR-032 S7-2). Permission push-provider.update. 404 — записи нет.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/push-providers/{name} (delete) — WRITE+AUDIT push-provider.deleted ===

// pushProviderDeleteInput — huma-input DELETE /v1/push-providers/{name}. Name — path.
// Body нет.
type pushProviderDeleteInput struct {
	Name string `path:"name" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"имя Push-Provider-а"`
}

// pushProviderNoContentOutput — huma-output 204-write-роута delete. БЕЗ Body
// (легаси-контракт: 204 No Content). huma на output без Body → SetStatus(204) →
// пустое тело (wire-идентично прежнему WriteHeader(204)).
type pushProviderNoContentOutput struct {
	Status int `json:"-"`
}

// pushProviderDeleteOperation — метаданные DELETE /v1/push-providers/{name}.
// DefaultStatus=204. Permission push-provider.delete + audit push-provider.deleted.
// Errors: 403, 404, 422 bad path-name, 500.
func pushProviderDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deletePushProvider",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Удалить Push-Provider",
		Description:   "Удаляет запись Push-Provider-а (ADR-032 S7-2). Permission push-provider.delete. 404 — записи нет.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
