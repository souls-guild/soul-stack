package api

// FULL-TYPED shape of POST /v1/roles (code-first OpenAPI source, ADR-054 PILOT-2
// §Pattern (b) thin-envelope). The Go types are the single source of truth: huma
// builds from them the OpenAPI-fragment JSON Schema, the input validation
// (required/additionalProperties:false HONEST), and the typed output. There is no
// RawBody bridge.
//
// The 201 body of role.create is EMPTY (legacy contract: openapi.yaml `POST
// /v1/roles` returns 201 with no `content` — the handler only did
// w.WriteHeader(201)). So roleCreateOutput carries no Body field: on an output with
// no Body huma calls ctx.SetStatus(DefaultStatus) → an empty 201 body (wire-identical
// to legacy, the golden-guard pins it).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// roleCreateInput — huma input for the POST /v1/roles operation (FULL-TYPED). Body —
// a typed body: huma decodes and validates it against the schema from the huma tags
// of RoleCreateRequest. The envelope into the domain model is in registerHumaRole.
type roleCreateInput struct {
	Body RoleCreateRequest
}

// RoleCreateRequest — the Go shape of the POST /v1/roles body (code-first source of
// the schema AND validation). Mirrors the domain RoleCreateRequest: role name + an
// optional description + a set of permission strings + an optional default_scope
// (ADR-047 S1).
//
// huma tags: `required:"true"` — a required field (missing → 422); `doc:"…"` — the
// description. omitempty/pointer — optional. additionalProperties:false (the huma
// default, NOT removed) → an unknown field → the error-override classifies it as 400.
// permission/default_scope semantics (format, RBAC subset-check) are in rbac.Service.
// The struct name = the contract schema name in OpenAPI (committed hand-written spec
// → RoleCreateRequest).
type RoleCreateRequest struct {
	Name         string   `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]*$" doc:"имя роли (kebab-case), уникальное в кластере"`
	Description  string   `json:"description,omitempty" doc:"человекочитаемое описание роли для UI/аудита"`
	Permissions  []string `json:"permissions,omitempty" doc:"набор permission-строк роли (например incarnation.run, soul.*, *)"`
	DefaultScope *string  `json:"default_scope,omitempty" doc:"селектор scope роли формы key=v1,v2,… (service/coven/incarnation/host); omitted/null → роль без scope"`
}

// roleCreateOutput — huma output (FULL-TYPED). Status=201; no Body (legacy contract:
// 201 with no body). On an output with no Body field huma does ctx.SetStatus(201) →
// an empty body (wire-identical to the former w.WriteHeader(201)).
type roleCreateOutput struct {
	Status int `json:"-"`
}

// roleCreateOperation — the huma.Operation metadata for POST /v1/roles. huma derives
// the RequestBody AUTOMATICALLY from roleCreateInput.Body (FULL-TYPED — schema and
// validation from the same Go types). Path = "/" — RELATIVE to the /v1/roles chi
// group (chi mounts the route as /v1/roles; chi.Walk sees it, the drift-test is
// green). DefaultStatus=201; a response with no body (a 201 description with no
// content — the legacy shape). Errors pins the problem codes (400 unknown/malformed,
// 403 RBAC/permission-not-held, 409 role-exists, 422 name/permission/default_scope
// validation, 500).
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

// === READ: GET /v1/roles (list) — FULL-TYPED, no audit (the READ variant of pilot-1) ===

// roleListInput — huma input for GET /v1/roles. No parameters (a catalog without
// filters) — an empty struct. huma requires no Body/Path/Query fields for a bare-GET.
type roleListInput struct{}

// roleListOutput — huma output for GET /v1/roles (FULL-TYPED). Body — the typed 200
// body (RoleView[] under `items`). The envelope from the domain listRolesResponse →
// this type is in registerHumaRoleList. The items wire shape (Description always,
// DefaultScope nil→omitted, []-vs-null) is pinned by a golden-JSON snapshot test.
type roleListOutput struct {
	Body RoleListReply
}

// RoleListReply — the Go shape of the GET /v1/roles 200 body (the source of the
// response schema AND the wire shape). The shape is verified against the committed
// hand-written spec (docs/keeper/openapi.yaml → RoleListReply): EXACTLY one field
// items (RoleView[], required), with NO offset/limit/total — role.list returns the
// whole catalog without pagination. Items — native RoleView (T5b: the reply-DTO is
// decoupled from the legacy generator; shape 1:1, see huma_role_reply.go).
// omitempty/[]-vs-null is held by the native RoleView itself. The struct name = the
// contract schema name in OpenAPI.
type RoleListReply struct {
	Items []RoleView `json:"items" doc:"каталог ролей (метаданные + permissions + operators)"`
}

// roleListOperation — metadata for GET /v1/roles. Path = "/" relative to the
// /v1/roles chi group. DefaultStatus=200. READ route: audit not wired (the role.list
// pattern).
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
// All four are full-typed (typed path/body) + the huma audit-middleware variant B
// (each has its own event type, see newHumaAuditAPI in router.go). 204 output with no
// Body (legacy contract: no-content). path parameters via huma's `path:"…"` tag.

// roleDeleteInput — huma input for DELETE /v1/roles/{name}. Name — a path parameter
// (huma extracts it by `path:"name"`, passes it to the handler). No Body.
type roleDeleteInput struct {
	Name string `path:"name" doc:"имя роли"`
}

// roleNoContentOutput — a shared huma output for the role 204 write routes
// (delete/update/grant/revoke). No Body (legacy contract: 204 No Content). On an
// output with no Body huma does SetStatus(204) → an empty body (wire-identical to the
// former WriteHeader(204)).
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

// roleUpdatePermissionsInput — huma input for PATCH /v1/roles/{name}/permissions.
// Name — path; Body — a typed body. The PATCH-presence of the default_scope key
// (omitted vs explicit null → different semantics) is carried by the Body.DefaultScope
// type itself ([Optional]), not by a raw RawBody []byte — the latter dragged an
// octet-stream artifact into the OpenAPI fragment (ADR-054 §Pattern third tier; the
// RawBody bridge is REJECTED).
type roleUpdatePermissionsInput struct {
	Name string `path:"name" doc:"имя роли"`
	Body RolePermissionsUpdateRequest
}

// RolePermissionsUpdateRequest — the Go shape of the PATCH /v1/roles/{name}/permissions
// body (replace semantics). Permissions is required (the full new set); default_scope
// — [Optional] with PATCH-presence semantics: presence is carried by the type itself.
// `required:"false"` keeps the field optional in the schema (Optional is a struct
// value, omitempty does not drop it). The struct name = the contract schema name in
// OpenAPI (committed hand-written spec → RolePermissionsUpdateRequest — note the word
// order in the contract name).
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

// roleGrantOperatorInput — huma input for POST /v1/roles/{name}/operators. Name —
// path; Body — the shared GrantOperatorRequest (AID). AID required:"true" in the
// schema, but an empty/malformed format (domain validation operator.ValidAID) yields
// 422 in GrantOperatorTyped.
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

// roleRevokeOperatorInput — huma input for DELETE /v1/roles/{name}/operators/{aid}.
// Both parameters are path. No Body.
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
