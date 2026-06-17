package api

// FULL-TYPED форма POST /v1/roles (code-first источник OpenAPI, ADR-054 PILOT-2
// §Pattern (б) тонкий-конверт). Go-типы — единственный источник правды: huma
// строит из них И JSON Schema OpenAPI-фрагмента, И валидацию входа
// (required/additionalProperties:false ЧЕСТНЫЙ), И typed-output. RawBody-моста нет.
//
// 201-тело role.create ПУСТОЕ (легаси-контракт: openapi.yaml `POST /v1/roles`
// отдаёт 201 без `content` — handler писал лишь w.WriteHeader(201)). Поэтому
// roleCreateOutput НЕ несёт Body-поля: huma на output без Body вызывает
// ctx.SetStatus(DefaultStatus) → пустое 201-тело (wire-идентично легаси,
// golden-guard это фиксирует).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// roleCreateInput — huma-input операции POST /v1/roles (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме из huma-тегов
// RoleCreateRequest. Конверт в доменную модель — в registerHumaRole.
type roleCreateInput struct {
	Body RoleCreateRequest
}

// RoleCreateRequest — Go-форма тела POST /v1/roles (code-first источник схемы И
// валидации). Повторяет доменный RoleCreateRequest: имя роли + опц.
// описание + набор permission-строк + опц. default_scope (ADR-047 S1).
//
// huma-теги: `required:"true"` — обязательное поле (missing → 422); `doc:"…"` —
// описание. omitempty/pointer — опциональные. additionalProperties:false (huma-
// дефолт, НЕ снимается) → unknown-поле → error-override классифицирует как 400.
// Семантика permission/default_scope (формат, RBAC subset-check) — в rbac.Service.
// Имя структуры = контрактное имя схемы в OpenAPI (committed-рукопись → RoleCreateRequest).
type RoleCreateRequest struct {
	Name         string   `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]*$" doc:"имя роли (kebab-case), уникальное в кластере"`
	Description  string   `json:"description,omitempty" doc:"человекочитаемое описание роли для UI/аудита"`
	Permissions  []string `json:"permissions,omitempty" doc:"набор permission-строк роли (например incarnation.run, soul.*, *)"`
	DefaultScope *string  `json:"default_scope,omitempty" doc:"селектор scope роли формы key=v1,v2,… (service/coven/incarnation/host); omitted/null → роль без scope"`
}

// roleCreateOutput — huma-output (FULL-TYPED). Status=201; БЕЗ Body (легаси-
// контракт: 201 без тела). huma на output без Body-поля делает
// ctx.SetStatus(201) → пустое тело (wire-идентично прежнему w.WriteHeader(201)).
type roleCreateOutput struct {
	Status int `json:"-"`
}

// roleCreateOperation — метаданные huma.Operation для POST /v1/roles. RequestBody
// huma выводит АВТОМАТИЧЕСКИ из roleCreateInput.Body (FULL-TYPED — схема и
// валидация из тех же Go-типов). Path = "/" — ОТНОСИТЕЛЬНЫЙ к chi-группе /v1/roles
// (chi смонтирует роут как /v1/roles; chi.Walk видит его, drift-test зелёный).
// DefaultStatus=201; ответ без тела (201 description без content — легаси-форма).
// Errors фиксирует problem-коды (400 unknown/malformed, 403 RBAC/permission-not-
// held, 409 role-exists, 422 валидация name/permission/default_scope, 500).
func roleCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createRole",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать роль",
		Description:   "Создаёт RBAC-роль с набором permissions (ADR-022). Permission role.create. 409 — name уже занят.",
		Tags:          []string{"role"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === READ: GET /v1/roles (list) — FULL-TYPED без audit (READ-вариант pilot-1) ===

// roleListInput — huma-input GET /v1/roles. Параметров нет (каталог без фильтров) —
// пустая структура. huma не требует Body/Path/Query-полей для bare-GET.
type roleListInput struct{}

// roleListOutput — huma-output GET /v1/roles (FULL-TYPED). Body — typed 200-тело
// (RoleView[] под `items`). Конверт доменной listRolesResponse → этот тип — в
// registerHumaRoleList. Wire-форма items (Description всегда, DefaultScope nil→
// пропуск, []-vs-null) зафиксирована golden-JSON snapshot-тестом.
type roleListOutput struct {
	Body RoleListReply
}

// RoleListReply — Go-форма 200-тела GET /v1/roles (источник схемы ответа И wire-формы).
// Форма сверена с committed-рукописью (docs/keeper/openapi.yaml → RoleListReply): РОВНО
// одно поле items (RoleView[], required), БЕЗ offset/limit/total — role.list отдаёт
// весь каталог без пагинации. Items — native RoleView (T5b: reply-DTO отвязан от legacy-генерата;
// форма 1:1, см. huma_role_reply.go). omitempty/[]-vs-null держит сам native RoleView.
// Имя структуры = контрактное имя схемы в OpenAPI.
type RoleListReply struct {
	Items []RoleView `json:"items" doc:"каталог ролей (метаданные + permissions + operators)"`
}

// roleListOperation — метаданные GET /v1/roles. Path = "/" относительно chi-группы
// /v1/roles. DefaultStatus=200. READ-роут: audit НЕ навешан (паттерн role.list).
func roleListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listRoles",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список ролей",
		Description:   "Каталог RBAC-ролей с развёрнутыми permissions и составом операторов (ADR-022). Permission role.list. Read-only, без audit.",
		Tags:          []string{"role"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}

// === WRITE+AUDIT: DELETE/PATCH/POST/DELETE /v1/roles/{name}[/...] ===
//
// Все четыре — full-typed (typed path/body) + huma-audit-middleware вариант B
// (event-тип у каждого свой, см. newHumaAuditAPI в router.go). 204-output без Body
// (легаси-контракт: no-content). path-параметры через `path:"…"`-тег huma.

// roleDeleteInput — huma-input DELETE /v1/roles/{name}. Name — path-параметр
// (huma извлекает по `path:"name"`, передаёт в handler). Body нет.
type roleDeleteInput struct {
	Name string `path:"name" doc:"имя роли"`
}

// roleNoContentOutput — общий huma-output 204-write-роутов role (delete/update/
// grant/revoke). БЕЗ Body (легаси-контракт: 204 No Content). huma на output без
// Body делает SetStatus(204) → пустое тело (wire-идентично прежнему WriteHeader(204)).
type roleNoContentOutput struct {
	Status int `json:"-"`
}

// roleDeleteOperation — DELETE /v1/roles/{name}. DefaultStatus=204. Permission
// role.delete + audit role.deleted.
func roleDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteRole",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Удалить роль",
		Description:   "Удаляет RBAC-роль каскадом (permissions + membership). Permission role.delete. 409 — builtin/last-admin.",
		Tags:          []string{"role"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError},
	}
}

// roleUpdatePermissionsInput — huma-input PATCH /v1/roles/{name}/permissions. Name —
// path; Body — typed тело. PATCH-presence ключа default_scope (omitted vs explicit
// null → разная семантика) несёт сам тип Body.DefaultScope ([Optional]), а не сырой
// RawBody []byte — последний тащил в OpenAPI-фрагмент octet-stream artifact (ADR-054
// §Pattern третий tier; RawBody-мост ОТВЕРГНУТ).
type roleUpdatePermissionsInput struct {
	Name string `path:"name" doc:"имя роли"`
	Body RolePermissionsUpdateRequest
}

// RolePermissionsUpdateRequest — Go-форма тела PATCH /v1/roles/{name}/permissions
// (replace-семантика). Permissions обязателен (полный новый набор); default_scope —
// [Optional] с PATCH-presence-семантикой: presence несёт сам тип. `required:"false"`
// держит поле опциональным в схеме (Optional — struct-value, omitempty его не снимает).
// Имя структуры = контрактное имя схемы в OpenAPI (committed-рукопись →
// RolePermissionsUpdateRequest — обрати внимание на порядок слов в имени контракта).
type RolePermissionsUpdateRequest struct {
	Permissions  []string         `json:"permissions" required:"true" doc:"полный новый набор permission-строк (replace)"`
	DefaultScope Optional[string] `json:"default_scope" required:"false" doc:"селектор scope: omitted → scope не трогается; присутствует (вкл. null) → заменяет (null снимает scope)"`
}

// roleUpdatePermissionsOperation — PATCH /v1/roles/{name}/permissions.
// DefaultStatus=204. Permission role.update + audit role.permissions-updated.
func roleUpdatePermissionsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateRolePermissions",
		Method:        http.MethodPatch,
		Path:          "/{name}/permissions",
		Summary:       "Заменить permissions роли",
		Description:   "Replace-семантика: набор полностью заменяет существующий (ADR-022). Permission role.update. 409 — builtin/last-admin.",
		Tags:          []string{"role"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// roleGrantOperatorInput — huma-input POST /v1/roles/{name}/operators. Name — path;
// Body — общий GrantOperatorRequest (AID). AID required:"true" в схеме, но пустой/
// битый формат (доменная валидация operator.ValidAID) даёт 422 в GrantOperatorTyped.
type roleGrantOperatorInput struct {
	Name string `path:"name" doc:"имя роли"`
	Body GrantOperatorRequest
}

// roleGrantOperatorOperation — POST /v1/roles/{name}/operators. DefaultStatus=204.
// Permission role.grant-operator + audit role.operator-granted.
func roleGrantOperatorOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "grantRoleOperator",
		Method:        http.MethodPost,
		Path:          "/{name}/operators",
		Summary:       "Привязать оператора к роли",
		Description:   "Идемпотентно (повтор — no-op). Permission role.grant-operator. 404 — роль/оператор не найдены.",
		Tags:          []string{"role"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// roleRevokeOperatorInput — huma-input DELETE /v1/roles/{name}/operators/{aid}.
// Оба параметра — path. Body нет.
type roleRevokeOperatorInput struct {
	Name string `path:"name" doc:"имя роли"`
	AID  string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID оператора-члена роли"`
}

// roleRevokeOperatorOperation — DELETE /v1/roles/{name}/operators/{aid}.
// DefaultStatus=204. Permission role.revoke-operator + audit role.operator-revoked.
func roleRevokeOperatorOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokeRoleOperator",
		Method:        http.MethodDelete,
		Path:          "/{name}/operators/{aid}",
		Summary:       "Отвязать оператора от роли",
		Description:   "Снимает membership-строку (name, aid). Permission role.revoke-operator. 409 — last-admin lock-out.",
		Tags:          []string{"role"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
