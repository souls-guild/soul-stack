package api

// FULL-TYPED форма SYNOD-домена (группы / membership / bundle; code-first источник
// OpenAPI, ADR-054 §Pattern). ТИРАЖ-БАТЧ-2d (synod целиком на huma по эталонам
// role/operator/augur/herald): synod create/update/delete — WRITE+AUDIT
// (synod.created/.updated/.deleted); add/remove-operator — WRITE+AUDIT
// (synod.operator-added/.operator-removed) на под-ресурсе /operators; grant/revoke-
// role — WRITE+AUDIT (synod.role-granted/.role-revoked) на под-ресурсе /roles;
// synod list — read (БЕЗ audit). Go-типы — единственный источник правды (JSON
// Schema + валидация + typed-output).
//
// Sub-resource роуты несут полный путь относительно chi-группы /v1/synods (/{name}/
// operators, /{name}/roles[/...]) — все пути distinct, коллизии на «/» нет (форма
// role-домена: единый resource-group + sub-resources). update — PATCH (меняет ТОЛЬКО
// description; name immutable). 201/204-тела write-роутов ПУСТЫЕ (легаси-контракт).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/synods (create) — WRITE+AUDIT synod.created ===

// synodCreateInput — huma-input POST /v1/synods (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме из huma-тегов.
type synodCreateInput struct {
	Body SynodCreateRequest
}

// SynodCreateRequest — Go-форма тела POST /v1/synods (code-first источник схемы И
// валидации). Повторяет доменный SynodCreateRequest: имя группы + опц.
// описание. Формат name + лимит описания — доменная валидация в CreateTyped (422/409).
// Имя структуры = контрактное имя схемы в OpenAPI (committed-рукопись → SynodCreateRequest).
type SynodCreateRequest struct {
	Name        string  `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]*$" doc:"имя Synod-группы (kebab-case), уникальное в кластере"`
	Description *string `json:"description,omitempty" maxLength:"1024" doc:"человекочитаемое описание группы для UI/аудита"`
}

// synodCreateOutput — huma-output POST /v1/synods (FULL-TYPED). Status=201; БЕЗ Body
// (легаси-контракт: openapi.yaml POST /v1/synods отдаёт 201 без content). huma на
// output без Body делает SetStatus(201) → пустое тело (wire-идентично легаси).
type synodCreateOutput struct {
	Status int `json:"-"`
}

// synodCreateOperation — метаданные POST /v1/synods. Path = "/" относительно
// chi-группы /v1/synods. DefaultStatus=201. Permission synod.create + audit
// synod.created. Errors: 400 unknown/malformed, 403 RBAC, 409 synod-exists, 422
// валидация name/description, 500.
func synodCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createSynod",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать Synod-группу",
		Description:   "Заносит Synod-группу (группа архонов, bundle ролей) в реестр (ADR-049). Permission synod.create. 409 — name занят.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/synods (list) — READ (БЕЗ audit) ===

// synodListInput — huma-input GET /v1/synods. Параметров нет (каталог без фильтров).
type synodListInput struct{}

// synodListOutput — huma-output GET /v1/synods (FULL-TYPED). Body — native
// 200-тело (SynodView[] под `items`). Wire-форма items (toSynodResponse:
// Description всегда, Roles/Operators []-vs-null) зафиксирована golden-JSON
// byte-exact-тестом.
type synodListOutput struct {
	Body SynodListReply
}

// synodListOperation — метаданные GET /v1/synods. Path = "/" относительно
// chi-группы /v1/synods. DefaultStatus=200. READ-роут: audit НЕ навешан. Errors:
// 403 RBAC, 500.
func synodListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listSynods",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Synod-групп",
		Description:   "Каталог Synod-групп с bundle ролей и составом членов (ADR-049). Permission synod.list. Read-only, без audit.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === PATCH /v1/synods/{name} (update) — WRITE+AUDIT synod.updated ===

// synodUpdateInput — huma-input PATCH /v1/synods/{name}. Name — path; Body — typed
// тело (меняет ТОЛЬКО description).
type synodUpdateInput struct {
	Name string `path:"name" doc:"имя Synod-группы (immutable)"`
	Body SynodUpdateRequest
}

// SynodUpdateRequest — Go-форма тела PATCH /v1/synods/{name} (ADR-049 amend:
// меняется ТОЛЬКО description; name (PK) immutable — берётся из path). description
// обязателен (присланное заменяет старое); пустой/длиннее лимита → 422 (доменная
// валидация в UpdateTyped). Имя структуры = контрактное имя схемы в OpenAPI
// (committed-рукопись → SynodUpdateRequest).
type SynodUpdateRequest struct {
	Description string `json:"description" required:"true" minLength:"1" maxLength:"1024" doc:"новое человекочитаемое описание группы для UI/аудита"`
}

// synodNoContentOutput — общий huma-output 204-write-роутов synod (update/delete/
// add-operator/remove-operator/grant-role/revoke-role). БЕЗ Body (легаси-контракт:
// 204 No Content). huma на output без Body делает SetStatus(204) → пустое тело.
type synodNoContentOutput struct {
	Status int `json:"-"`
}

// synodUpdateOperation — метаданные PATCH /v1/synods/{name}. DefaultStatus=204.
// Permission synod.update + audit synod.updated. Errors: 400 unknown/malformed (в
// т.ч. name в теле), 403 RBAC, 404 not-found, 422 валидация description, 500.
func synodUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateSynod",
		Method:        http.MethodPatch,
		Path:          "/{name}",
		Summary:       "Обновить описание Synod-группы",
		Description:   "Меняет ТОЛЬКО description (name immutable, ADR-049 amend). Permission synod.update. 404 — записи нет.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/synods/{name} (delete) — WRITE+AUDIT synod.deleted ===

// synodDeleteInput — huma-input DELETE /v1/synods/{name}. Name — path. Body нет.
type synodDeleteInput struct {
	Name string `path:"name" doc:"имя Synod-группы"`
}

// synodDeleteOperation — метаданные DELETE /v1/synods/{name}. DefaultStatus=204.
// Permission synod.delete + audit synod.deleted (каскад чистит membership + bundle).
// Errors: 403 RBAC, 404 not-found, 409 builtin/last-admin, 500.
func synodDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteSynod",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Удалить Synod-группу",
		Description:   "Удаляет Synod каскадно (membership + bundle, ADR-049). Permission synod.delete. 409 — builtin/last-admin lock-out.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError},
	}
}

// === POST /v1/synods/{name}/operators (add-operator) — WRITE+AUDIT synod.operator-added ===

// synodAddOperatorInput — huma-input POST /v1/synods/{name}/operators. Name — path;
// Body — общий GrantOperatorRequest (AID члена). Рукопись описывает этот эндпоинт той
// же схемой GrantOperatorRequest, что и role.grant-operator (см. huma_grant_operator.go).
type synodAddOperatorInput struct {
	Name string `path:"name" doc:"имя Synod-группы"`
	Body GrantOperatorRequest
}

// synodAddOperatorOperation — метаданные POST /v1/synods/{name}/operators.
// DefaultStatus=204. Permission synod.add-operator + audit synod.operator-added.
// Errors: 400 unknown/malformed, 403 least-privilege, 404 synod/operator-not-found,
// 422 валидация AID, 500.
func synodAddOperatorOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "addSynodOperator",
		Method:        http.MethodPost,
		Path:          "/{name}/operators",
		Summary:       "Добавить архонта в Synod-группу",
		Description:   "Член получает весь bundle ролей группы (ADR-049). Идемпотентно. Permission synod.add-operator. 403 — least-privilege.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/synods/{name}/operators/{aid} (remove-operator) — WRITE+AUDIT synod.operator-removed ===

// synodRemoveOperatorInput — huma-input DELETE /v1/synods/{name}/operators/{aid}.
// Оба параметра — path. Body нет.
type synodRemoveOperatorInput struct {
	Name string `path:"name" doc:"имя Synod-группы"`
	AID  string `path:"aid" doc:"AID архонта-члена группы"`
}

// synodRemoveOperatorOperation — метаданные DELETE /v1/synods/{name}/operators/
// {aid}. DefaultStatus=204. Permission synod.remove-operator + audit
// synod.operator-removed. Errors: 403 RBAC, 404 not-found, 409 last-admin, 422
// bad path-AID, 500.
func synodRemoveOperatorOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "removeSynodOperator",
		Method:        http.MethodDelete,
		Path:          "/{name}/operators/{aid}",
		Summary:       "Убрать архонта из Synod-группы",
		Description:   "Снимает membership-строку (name, aid). Permission synod.remove-operator. 409 — last-admin lock-out.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/synods/{name}/roles (grant-role) — WRITE+AUDIT synod.role-granted ===

// synodGrantRoleInput — huma-input POST /v1/synods/{name}/roles. Name — path; Body
// — typed тело (имя роли).
type synodGrantRoleInput struct {
	Name string `path:"name" doc:"имя Synod-группы"`
	Body SynodGrantRoleRequest
}

// SynodGrantRoleRequest — Go-форма тела POST /v1/synods/{name}/roles. Имя роли,
// добавляемой в bundle группы (parity SynodGrantRoleRequest). role
// required:"true" в схеме; пустой — доменная валидация в GrantRoleTyped (422).
// Имя структуры = контрактное имя схемы в OpenAPI (committed-рукопись → SynodGrantRoleRequest).
type SynodGrantRoleRequest struct {
	Role string `json:"role" required:"true" doc:"имя роли, добавляемой в bundle группы"`
}

// synodGrantRoleOperation — метаданные POST /v1/synods/{name}/roles.
// DefaultStatus=204. Permission synod.grant-role + audit synod.role-granted.
// Errors: 400 unknown/malformed, 403 least-privilege, 404 synod/role-not-found,
// 422 пустой role, 500.
func synodGrantRoleOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "grantSynodRole",
		Method:        http.MethodPost,
		Path:          "/{name}/roles",
		Summary:       "Добавить роль в bundle Synod-группы",
		Description:   "Все члены группы получают эффективные права роли (ADR-049). Идемпотентно. Permission synod.grant-role. 403 — least-privilege.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/synods/{name}/roles/{role_name} (revoke-role) — WRITE+AUDIT synod.role-revoked ===

// synodRevokeRoleInput — huma-input DELETE /v1/synods/{name}/roles/{role_name}.
// Оба параметра — path. Body нет.
type synodRevokeRoleInput struct {
	Name string `path:"name" doc:"имя Synod-группы"`
	Role string `path:"role_name" doc:"имя роли в bundle группы"`
}

// synodRevokeRoleOperation — метаданные DELETE /v1/synods/{name}/roles/{role_name}.
// DefaultStatus=204. Permission synod.revoke-role + audit synod.role-revoked.
// Errors: 403 RBAC, 404 not-found, 409 last-admin, 500.
func synodRevokeRoleOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokeSynodRole",
		Method:        http.MethodDelete,
		Path:          "/{name}/roles/{role_name}",
		Summary:       "Снять роль из bundle Synod-группы",
		Description:   "Права роли снимаются у всех членов группы (ADR-049). Permission synod.revoke-role. 409 — last-admin lock-out.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError},
	}
}
