package api

// FULL-TYPED форма SERVICE-домена (реестр Service-ов; code-first источник OpenAPI,
// ADR-054 §Pattern). ТИРАЖ-БАТЧ-2d (service целиком на huma по эталонам role/
// operator/augur/herald): register — WRITE+AUDIT (service.registered, 201 С ТЕЛОМ);
// update — WRITE+AUDIT (service.updated, 200 С ТЕЛОМ); deregister — WRITE+AUDIT
// (service.deregistered, 204); list/get — read; refs/scenarios/state-schema/
// dependencies — read-with-path (опц. ?ref=, tier 502 на git-loader). Go-типы —
// единственный источник правды (JSON Schema + валидация + typed-output).
//
// update — PATCH replace-семантика mutable-полей git/ref/refresh (НЕ presence-tier:
// git/ref обязательны, refresh *string omitempty). list/get/refs — без пагинации
// (ServiceListReply несёт только items).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === POST /v1/services (register) — WRITE+AUDIT service.registered ===

// serviceRegisterInput — huma-input POST /v1/services (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме из huma-тегов.
type serviceRegisterInput struct {
	Body ServiceRegisterRequest
}

// ServiceRegisterRequest — Go-форма тела POST /v1/services (code-first источник
// схемы И валидации). Повторяет доменный ServiceRegisterRequest: name+git+ref
// обязательны, refresh опц. (duration авто-refresh). Формат name/git/ref/refresh —
// доменная валидация в RegisterTyped (422/409/404). Имя структуры = контрактное имя
// схемы в OpenAPI (committed-рукопись → ServiceRegisterRequest).
type ServiceRegisterRequest struct {
	Name    string  `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]*$" doc:"имя Service-а (kebab-case)"`
	Git     string  `json:"git" required:"true" doc:"git-источник service-репо (URL; не секрет)"`
	Ref     string  `json:"ref" required:"true" doc:"git ref (tag/branch) — версия Service-а (ADR-007)"`
	Refresh *string `json:"refresh,omitempty" doc:"опц. duration авто-refresh ('5m'); опущено — без авто-refresh"`
}

// serviceRegisterOutput — huma-output POST /v1/services (FULL-TYPED). Status=201;
// Body — native 201-тело (ServiceView). Wire-форма (created_by_aid omitempty,
// created_at/updated_at секундной точности) зафиксирована golden-JSON byte-exact-тестом.
type serviceRegisterOutput struct {
	Status int `json:"-"`
	Body   ServiceView
}

// serviceRegisterOperation — метаданные POST /v1/services. Path = "/" относительно
// chi-группы /v1/services. DefaultStatus=201. Permission service.register + audit
// service.registered. Errors: 400 unknown/malformed, 403 RBAC, 404 caller-not-found
// (FK), 409 service-exists, 422 валидация name/git/ref/refresh, 500.
func serviceRegisterOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "registerService",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Зарегистрировать Service",
		Description:   "Заносит Service в реестр service_registry (ADR-028). Permission service.register. 409 — name занят. 404 — caller AID отсутствует в реестре операторов.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/services (list) — READ (БЕЗ audit) ===

// serviceListInput — huma-input GET /v1/services. Параметров нет (реестр без
// фильтров/пагинации — ServiceListReply несёт только items).
type serviceListInput struct{}

// serviceListOutput — huma-output GET /v1/services (FULL-TYPED). Body — native
// 200-тело (ServiceListReply: items под `items`, БЕЗ offset/limit/total).
// Wire-форма items зафиксирована golden-JSON byte-exact-тестом.
type serviceListOutput struct {
	Body ServiceListReply
}

// serviceListOperation — метаданные GET /v1/services. Path = "/" относительно
// chi-группы /v1/services. DefaultStatus=200. READ-роут: audit НЕ навешан. Errors:
// 403 RBAC, 500.
func serviceListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServices",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Service-ов",
		Description:   "Реестр Service-ов (sort name ASC, ADR-028). Permission service.list. Read-only, без audit.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/services/{name} (get) — READ-with-path (БЕЗ audit) ===

// serviceGetInput — huma-input GET /v1/services/{name}. Name — path-параметр.
type serviceGetInput struct {
	Name string `path:"name" doc:"имя Service-а"`
}

// serviceGetOutput — huma-output GET /v1/services/{name} (FULL-TYPED). Body —
// native 200-тело (ServiceView). Wire-форма зафиксирована golden-тестом.
type serviceGetOutput struct {
	Body ServiceView
}

// serviceGetOperation — метаданные GET /v1/services/{name}. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission service.list (read покрыт list-правом).
// Errors: 403 RBAC, 404 not-found, 500.
func serviceGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getService",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Карточка Service-а",
		Description:   "Метаданные одной записи реестра по имени (ADR-028). Permission service.list. Read-only, без audit.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}
}

// === PATCH /v1/services/{name} (update) — WRITE+AUDIT service.updated ===

// serviceUpdateInput — huma-input PATCH /v1/services/{name}. Name — path; Body —
// typed тело (replace mutable-полей git/ref/refresh).
type serviceUpdateInput struct {
	Name string `path:"name" doc:"имя Service-а (immutable)"`
	Body ServiceUpdateRequest
}

// ServiceUpdateRequest — Go-форма тела PATCH /v1/services/{name} (replace-семантика
// mutable-полей: git/ref обязательны, refresh опц.; name immutable — из path). Имя
// структуры = контрактное имя схемы в OpenAPI (committed-рукопись → ServiceUpdateRequest).
type ServiceUpdateRequest struct {
	Git     string  `json:"git" required:"true" doc:"новый git-источник"`
	Ref     string  `json:"ref" required:"true" doc:"новый git ref"`
	Refresh *string `json:"refresh,omitempty" doc:"опц. duration авто-refresh ('5m')"`
}

// serviceUpdateOutput — huma-output PATCH /v1/services/{name} (FULL-TYPED).
// Status=200 С ТЕЛОМ (native ServiceView — обновлённая запись). Wire-форма
// зафиксирована golden-тестом.
type serviceUpdateOutput struct {
	Status int `json:"-"`
	Body   ServiceView
}

// serviceUpdateOperation — метаданные PATCH /v1/services/{name}. DefaultStatus=200.
// Permission service.update + audit service.updated. Errors: 400 unknown/malformed,
// 403 RBAC, 404 not-found/caller-not-found, 422 валидация git/ref/refresh, 500.
func serviceUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateService",
		Method:        http.MethodPatch,
		Path:          "/{name}",
		Summary:       "Обновить Service (replace mutable-полей)",
		Description:   "Replace-семантика git/ref/refresh, name immutable (ADR-028). Permission service.update. 404 — записи нет.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/services/{name} (deregister) — WRITE+AUDIT service.deregistered ===

// serviceDeregisterInput — huma-input DELETE /v1/services/{name}. Name — path. Body нет.
type serviceDeregisterInput struct {
	Name string `path:"name" doc:"имя Service-а"`
}

// serviceNoContentOutput — huma-output 204-write-роута deregister. БЕЗ Body
// (легаси-контракт: 204 No Content). huma на output без Body делает SetStatus(204)
// → пустое тело (wire-идентично прежнему WriteHeader(204)).
type serviceNoContentOutput struct {
	Status int `json:"-"`
}

// serviceDeregisterOperation — метаданные DELETE /v1/services/{name}.
// DefaultStatus=204. Permission service.deregister + audit service.deregistered.
// Errors: 403 RBAC, 404 not-found, 500.
func serviceDeregisterOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deregisterService",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Удалить Service из реестра",
		Description:   "Удаляет запись реестра по имени + инвалидирует кеши (ADR-028). Permission service.deregister. 404 — записи нет.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}
}

// === GET /v1/services/{name}/refs (list-refs) — READ-with-path (БЕЗ audit) ===

// serviceRefsInput — huma-input GET /v1/services/{name}/refs. Name — path. Без
// ?ref= (refs перечисляет ВСЕ tags+branches remote-репо).
type serviceRefsInput struct {
	Name string `path:"name" doc:"имя Service-а"`
}

// serviceRefsOutput — huma-output GET /v1/services/{name}/refs (FULL-TYPED). Body —
// native 200-тело (ServiceRefsListReply: service + refs[]). Wire-форма
// зафиксирована golden-тестом.
type serviceRefsOutput struct {
	Body ServiceRefsListReply
}

// serviceRefsOperation — метаданные GET /v1/services/{name}/refs. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission service.list (refs — проекция записи).
// Errors: 403 RBAC, 404 not-found, 500 (нет lister-а/сбой реестра), 502 ls-remote упал.
func serviceRefsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceRefs",
		Method:        http.MethodGet,
		Path:          "/{name}/refs",
		Summary:       "git-tag-и + branch-и Service-а",
		Description:   "Список git-ref-ов remote-репозитория Service-а для UI Upgrade-modal (ADR-028). Permission service.list. Read-only, без audit. 502 — git-источник unreachable.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/scenarios (list-scenarios) — READ-with-path+query (БЕЗ audit) ===

// serviceScenariosInput — huma-input GET /v1/services/{name}/scenarios. Name — path;
// Ref — опц. query-override (опущено → ref из реестра).
type serviceScenariosInput struct {
	Name string `path:"name" doc:"имя Service-а"`
	Ref  string `query:"ref" doc:"опц. git-ref override (опущено → ref из реестра)"`
}

// serviceScenariosOutput — huma-output GET /v1/services/{name}/scenarios (FULL-TYPED).
// Body — handlers.ServiceScenariosReply (НЕ oapi-алиас: элемент artifact.Scenario с
// plain-string Kind, см. handlers/service.go). Wire-форма зафиксирована golden-тестом.
type serviceScenariosOutput struct {
	Body handlers.ServiceScenariosReply
}

// serviceScenariosOperation — метаданные GET /v1/services/{name}/scenarios.
// DefaultStatus=200. READ-роут: audit НЕ навешан. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (нет lister-а/сбой реестра), 502 loader упал.
func serviceScenariosOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceScenarios",
		Method:        http.MethodGet,
		Path:          "/{name}/scenarios",
		Summary:       "scenario из снапшота Service-репо",
		Description:   "Список scenario из материализованного снапшота git-репо Service-а для UI Run-modal (ADR-028). Permission service.list. Read-only, без audit. 502 — loader упал.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/state-schema (list-state-schema) — READ-with-path+query (БЕЗ audit) ===

// serviceStateSchemaInput — huma-input GET /v1/services/{name}/state-schema. Name —
// path; Ref — опц. query-override.
type serviceStateSchemaInput struct {
	Name string `path:"name" doc:"имя Service-а"`
	Ref  string `query:"ref" doc:"опц. git-ref override (опущено → ref из реестра)"`
}

// serviceStateSchemaOutput — huma-output GET /v1/services/{name}/state-schema
// (FULL-TYPED). Body — native 200-тело (ServiceStateSchemaReply). Wire-форма
// зафиксирована golden-тестом.
type serviceStateSchemaOutput struct {
	Body ServiceStateSchemaReply
}

// serviceStateSchemaOperation — метаданные GET /v1/services/{name}/state-schema.
// DefaultStatus=200. READ-роут: audit НЕ навешан. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (нет lister-а/сбой реестра), 502 loader упал.
func serviceStateSchemaOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceStateSchema",
		Method:        http.MethodGet,
		Path:          "/{name}/state-schema",
		Summary:       "state_schema-метаданные Service-а",
		Description:   "state_schema-версия + декларация структуры + цепочка миграций (metadata-only) для UI Schema explorer (ADR-019/028). Permission service.list. Read-only, без audit. 502 — loader упал.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}

// === GET /v1/services/{name}/dependencies (list-dependencies) — READ-with-path+query (БЕЗ audit) ===

// serviceDependenciesInput — huma-input GET /v1/services/{name}/dependencies. Name —
// path; Ref — опц. query-override.
type serviceDependenciesInput struct {
	Name string `path:"name" doc:"имя Service-а"`
	Ref  string `query:"ref" doc:"опц. git-ref override (опущено → ref из реестра)"`
}

// serviceDependenciesOutput — huma-output GET /v1/services/{name}/dependencies
// (FULL-TYPED). Body — native 200-тело (ServiceDependenciesReply: service/ref +
// destiny[]/modules[]). Wire-форма зафиксирована golden-тестом.
type serviceDependenciesOutput struct {
	Body ServiceDependenciesReply
}

// serviceDependenciesOperation — метаданные GET /v1/services/{name}/dependencies.
// DefaultStatus=200. READ-роут: audit НЕ навешан. Permission service.list. Errors:
// 403 RBAC, 404 not-found, 500 (нет lister-а/сбой реестра), 502 loader упал.
func serviceDependenciesOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listServiceDependencies",
		Method:        http.MethodGet,
		Path:          "/{name}/dependencies",
		Summary:       "git-зависимости Service-а",
		Description:   "Задекларированные в service.yml destiny-кирпичики + custom-модули со своими git-ref-ами для UI Service Detail (ADR-007/028). Permission service.list. Read-only, без audit. 502 — loader упал.",
		Tags:          []string{"service"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway},
	}
}
