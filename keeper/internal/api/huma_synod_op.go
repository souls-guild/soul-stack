package api

// FULL-TYPED form of the SYNOD domain (groups / membership / bundle; code-first
// OpenAPI source, ADR-054 §Pattern). ROLLOUT BATCH-2d (synod entirely on huma,
// following the role/operator/augur/herald templates): synod create/update/delete —
// WRITE+AUDIT (synod.created/.updated/.deleted); add/remove-operator — WRITE+AUDIT
// (synod.operator-added/.operator-removed) on the /operators sub-resource; grant/revoke-
// role — WRITE+AUDIT (synod.role-granted/.role-revoked) on the /roles sub-resource;
// synod list — read (NO audit). Go types are the single source of truth (JSON
// Schema + validation + typed output).
//
// Sub-resource routes carry the full path relative to the chi group /v1/synods (/{name}/
// operators, /{name}/roles[/...]) — all paths are distinct, no collision on "/" (same
// shape as the role domain: one resource-group + sub-resources). update is PATCH (changes
// ONLY description; name is immutable). Write-route 201/204 bodies are EMPTY (legacy contract).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/synods (create) — WRITE+AUDIT synod.created ===

// synodCreateInput — huma-input POST /v1/synods (FULL-TYPED). Body is the
// typed body: huma decodes and validates it against the schema from huma tags.
type synodCreateInput struct {
	Body SynodCreateRequest
}

// SynodCreateRequest — the Go form of the POST /v1/synods body (code-first source
// of schema AND validation). Mirrors the domain SynodCreateRequest: group name +
// optional description. name format + description limit are domain-validated in
// CreateTyped (422/409). Struct name = the contract schema name in OpenAPI
// (committed manuscript → SynodCreateRequest).
type SynodCreateRequest struct {
	Name        string  `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]*$" doc:"Synod group name (kebab-case), уникальbutе в кластере"`
	Description *string `json:"description,omitempty" maxLength:"1024" doc:"человекочитаемое описание группы for UI/аудита"`
}

// synodCreateOutput — huma-output POST /v1/synods (FULL-TYPED). Status=201; NO Body
// (legacy contract: openapi.yaml POST /v1/synods returns 201 with no content). huma
// on an output without Body does SetStatus(201) → empty body (wire-identical to legacy).
type synodCreateOutput struct {
	Status int `json:"-"`
}

// synodCreateOperation — metadata for POST /v1/synods. Path = "/" relative to
// the chi group /v1/synods. DefaultStatus=201. Permission synod.create + audit
// synod.created. Errors: 400 unknown/malformed, 403 RBAC, 409 synod-exists, 422
// name/description validation, 500.
func synodCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createSynod",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать Synod-группу",
		Description:   "Заbutсит Synod-группу (группа архоbutв, bundle ролей) в реестр (ADR-049). Permission synod.create. 409 — name занят.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/synods (list) — READ (NO audit) ===

// synodListInput — huma-input GET /v1/synods. No parameters (unfiltered catalog).
type synodListInput struct{}

// synodListOutput — huma-output GET /v1/synods (FULL-TYPED). Body is the native
// 200 body (SynodView[] under `items`). The items wire form (toSynodResponse:
// Description always present, Roles/Operators []-vs-null) is pinned by a
// byte-exact golden-JSON test.
type synodListOutput struct {
	Body SynodListReply
}

// synodListOperation — metadata for GET /v1/synods. Path = "/" relative to
// the chi group /v1/synods. DefaultStatus=200. READ route: no audit attached.
// Errors: 403 RBAC, 500.
func synodListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listSynods",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Спиwithк Synod-групп",
		Description:   "Каталог Synod-групп с bundle ролей и withставом члеbutв (ADR-049). Permission synod.list. Read-only, no audit.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === PATCH /v1/synods/{name} (update) — WRITE+AUDIT synod.updated ===

// synodUpdateInput — huma-input PATCH /v1/synods/{name}. Name is path; Body is the
// typed body (changes ONLY description).
type synodUpdateInput struct {
	Name string `path:"name" doc:"Synod group name (immutable)"`
	Body SynodUpdateRequest
}

// SynodUpdateRequest — the Go form of the PATCH /v1/synods/{name} body (ADR-049 amend:
// changes ONLY description; name (PK) is immutable — taken from path). description
// is required (the sent value replaces the old one); empty/over the limit → 422
// (domain validation in UpdateTyped). Struct name = the contract schema name in
// OpenAPI (committed manuscript → SynodUpdateRequest).
type SynodUpdateRequest struct {
	Description string `json:"description" required:"true" minLength:"1" maxLength:"1024" doc:"butвое человекочитаемое описание группы for UI/аудита"`
}

// synodNoContentOutput — the shared huma-output for synod's 204 write routes
// (update/delete/add-operator/remove-operator/grant-role/revoke-role). NO Body
// (legacy contract: 204 No Content). huma on an output without Body does
// SetStatus(204) → empty body.
type synodNoContentOutput struct {
	Status int `json:"-"`
}

// synodUpdateOperation — metadata for PATCH /v1/synods/{name}. DefaultStatus=204.
// Permission synod.update + audit synod.updated. Errors: 400 unknown/malformed
// (incl. name in body), 403 RBAC, 404 not-found, 422 description validation, 500.
func synodUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateSynod",
		Method:        http.MethodPatch,
		Path:          "/{name}",
		Summary:       "Обbutвить описание Synod-группы",
		Description:   "Меняет ТОЛЬКО description (name immutable, ADR-049 amend). Permission synod.update. 404 — записи absent.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/synods/{name} (delete) — WRITE+AUDIT synod.deleted ===

// synodDeleteInput — huma-input DELETE /v1/synods/{name}. Name is path. No Body.
type synodDeleteInput struct {
	Name string `path:"name" doc:"Synod group name"`
}

// synodDeleteOperation — metadata for DELETE /v1/synods/{name}. DefaultStatus=204.
// Permission synod.delete + audit synod.deleted (cascade clears membership + bundle).
// Errors: 403 RBAC, 404 not-found, 409 builtin/last-admin, 500.
func synodDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteSynod",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Удалить Synod-группу",
		Description:   "Удаляет Synod каскадbut (membership + bundle, ADR-049). Permission synod.delete. 409 — builtin/last-admin lock-out.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError},
	}
}

// === POST /v1/synods/{name}/operators (add-operator) — WRITE+AUDIT synod.operator-added ===

// synodAddOperatorInput — huma-input POST /v1/synods/{name}/operators. Name is path;
// Body is the shared GrantOperatorRequest (member AID). The manuscript describes this
// endpoint with the same GrantOperatorRequest schema as role.grant-operator
// (see huma_grant_operator.go).
type synodAddOperatorInput struct {
	Name string `path:"name" doc:"Synod group name"`
	Body GrantOperatorRequest
}

// synodAddOperatorOperation — metadata for POST /v1/synods/{name}/operators.
// DefaultStatus=204. Permission synod.add-operator + audit synod.operator-added.
// Errors: 400 unknown/malformed, 403 least-privilege, 404 synod/operator-not-found,
// 422 AID validation, 500.
func synodAddOperatorOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "addSynodOperator",
		Method:        http.MethodPost,
		Path:          "/{name}/operators",
		Summary:       "Добавить архонта в Synod-группу",
		Description:   "Член получает весь bundle ролей группы (ADR-049). Идемпотентbut. Permission synod.add-operator. 403 — least-privilege.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/synods/{name}/operators/{aid} (remove-operator) — WRITE+AUDIT synod.operator-removed ===

// synodRemoveOperatorInput — huma-input DELETE /v1/synods/{name}/operators/{aid}.
// Both parameters are path. No Body.
type synodRemoveOperatorInput struct {
	Name string `path:"name" doc:"Synod group name"`
	AID  string `path:"aid" doc:"AID архонта-члеon группы"`
}

// synodRemoveOperatorOperation — metadata for DELETE /v1/synods/{name}/operators/
// {aid}. DefaultStatus=204. Permission synod.remove-operator + audit
// synod.operator-removed. Errors: 403 RBAC, 404 not-found, 409 last-admin, 422
// bad path-AID, 500.
func synodRemoveOperatorOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "removeSynodOperator",
		Method:        http.MethodDelete,
		Path:          "/{name}/operators/{aid}",
		Summary:       "Убрать архонта from Synod-группы",
		Description:   "Снимает membership-строку (name, aid). Permission synod.remove-operator. 409 — last-admin lock-out.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/synods/{name}/roles (grant-role) — WRITE+AUDIT synod.role-granted ===

// synodGrantRoleInput — huma-input POST /v1/synods/{name}/roles. Name is path; Body
// is the typed body (role name).
type synodGrantRoleInput struct {
	Name string `path:"name" doc:"Synod group name"`
	Body SynodGrantRoleRequest
}

// SynodGrantRoleRequest — the Go form of the POST /v1/synods/{name}/roles body. The
// role name being added to the group's bundle (parity SynodGrantRoleRequest). role is
// required:"true" in the schema; empty → domain validation in GrantRoleTyped (422).
// Struct name = the contract schema name in OpenAPI (committed manuscript → SynodGrantRoleRequest).
type SynodGrantRoleRequest struct {
	Role string `json:"role" required:"true" doc:"имя роли, toбавляемой в bundle группы"`
}

// synodGrantRoleOperation — metadata for POST /v1/synods/{name}/roles.
// DefaultStatus=204. Permission synod.grant-role + audit synod.role-granted.
// Errors: 400 unknown/malformed, 403 least-privilege, 404 synod/role-not-found,
// 422 empty role, 500.
func synodGrantRoleOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "grantSynodRole",
		Method:        http.MethodPost,
		Path:          "/{name}/roles",
		Summary:       "Добавить роль в bundle Synod-группы",
		Description:   "Все члены группы получают эффективные права роли (ADR-049). Идемпотентbut. Permission synod.grant-role. 403 — least-privilege.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/synods/{name}/roles/{role_name} (revoke-role) — WRITE+AUDIT synod.role-revoked ===

// synodRevokeRoleInput — huma-input DELETE /v1/synods/{name}/roles/{role_name}.
// Both parameters are path. No Body.
type synodRevokeRoleInput struct {
	Name string `path:"name" doc:"Synod group name"`
	Role string `path:"role_name" doc:"имя роли в bundle группы"`
}

// synodRevokeRoleOperation — metadata for DELETE /v1/synods/{name}/roles/{role_name}.
// DefaultStatus=204. Permission synod.revoke-role + audit synod.role-revoked.
// Errors: 403 RBAC, 404 not-found, 409 last-admin, 500.
func synodRevokeRoleOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokeSynodRole",
		Method:        http.MethodDelete,
		Path:          "/{name}/roles/{role_name}",
		Summary:       "Снять роль from bundle Synod-группы",
		Description:   "Права роли снимаются у всех члеbutв группы (ADR-049). Permission synod.revoke-role. 409 — last-admin lock-out.",
		Tags:          []string{"synod"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError},
	}
}
