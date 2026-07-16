package api

// FULL-TYPED shape of the PUSH-PROVIDER domain (code-first OpenAPI source, ADR-054
// §Pattern). ROLLOUT BATCH 2b (push-provider entirely onto huma, following the
// role/operator references): create (WRITE+AUDIT push-provider.created), list
// (read-with-typed-query), get (read-with-path), update (WRITE+AUDIT
// push-provider.updated, PUT replace semantics), delete (WRITE+AUDIT
// push-provider.deleted). The Go types are the single source of truth.
//
// update — a PUT with replace semantics (params fully replaces the existing set,
// read-modify-write on the client), NOT the presence-tier Optional[T]: the params
// field has no "omitted vs null" semantics (it is always sent whole), so Optional
// isn't needed.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/push-providers (create) — WRITE+AUDIT push-provider.created ===

// pushProviderCreateInput — huma input for POST /v1/push-providers (FULL-TYPED). Body —
// a typed body.
type pushProviderCreateInput struct {
	Body PushProviderCreateRequest
}

// PushProviderCreateRequest — the Go shape of the POST /v1/push-providers body (code-first
// source of the schema AND validation). name + optional params (opaque map; sensitive
// keys — vault-refs). additionalProperties in params:true (opaque payload), but at the
// TOP level of the body — false (the huma default) → an unknown body field → 400. The
// format of name and the sensitive-invariant of params — domain validation in
// CreateTyped (422). The struct name = the contract schema name (huma DefaultSchemaNamer
// takes reflect.Type.Name()) — aligned to the committed hand-written spec (rollout batch
// N3). The register-func projects into the native handlers.PushProviderCreateInput.
type PushProviderCreateRequest struct {
	Name   string         `json:"name" required:"true" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"Push Provider name (= plugins.ssh_providers[].name)"`
	Params map[string]any `json:"params,omitempty" doc:"opaque params; sensitive — vault-refs (зonчения не логируются)"`
}

// pushProviderCreateOutput — huma output for POST /v1/push-providers (FULL-TYPED).
// Status=201; Body — the native 201 body (PushProvider). The wire shape (params
// normalized to {}, updated_by_aid nullable, date-time RFC3339Nano without Truncate)
// is pinned by a golden-JSON byte-exact test.
type pushProviderCreateOutput struct {
	Status int `json:"-"`
	Body   PushProvider
}

// pushProviderCreateOperation — the metadata for POST /v1/push-providers. Path = "/"
// relative to the /v1/push-providers chi group. DefaultStatus=201. Permission
// push-provider.create + audit push-provider.created. Errors: 400 unknown/malformed,
// 403 RBAC, 409 push-provider-exists, 422 name/sensitive-param validation, 500.
func pushProviderCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createPushProvider",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать Push-Provider",
		Description:   "Заbutсит Push-Provider (per-provider env-payload, ADR-032 S7-2). Permission push-provider.create. 409 — name занят. sensitive-ключи обязаны быть vault-refs.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/push-providers (list) — READ-with-typed-query (no audit) ===

// pushProviderListInput — huma input for GET /v1/push-providers (FULL-TYPED typed-query).
// name_pattern — a LIKE-prefix filter (string). offset/limit — int32 with a default; the
// range is enforced by CheckPageBounds in ListTyped → 400 (NOT huma minimum/maximum).
// bad-int → 400 (parseInto).
type pushProviderListInput struct {
	NamePattern string `query:"name_pattern" doc:"LIKE-prefix-фильтр по имени (опц.)"`
	Offset      int32  `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit       int32  `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// pushProviderListOutput — huma output for GET /v1/push-providers (FULL-TYPED). Body —
// the native 200 envelope (PushProviderListReply: items/offset/limit/total).
// The wire shape is pinned by a golden test.
type pushProviderListOutput struct {
	Body PushProviderListReply
}

// pushProviderListOperation — the metadata for GET /v1/push-providers. DefaultStatus=200.
// READ route: audit is NOT wired. Errors: 400 (out-of-range pagination), 403 RBAC, 500.
func pushProviderListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listPushProviders",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Спиwithк Push-Provider-ов (paged)",
		Description:   "Реестр Push-Provider-ов с пагиonцией и фильтром name_pattern (ADR-032 S7-2). Permission push-provider.list. Read-only, no audit.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/push-providers/{name} (get) — READ-with-path (no audit) ===

// pushProviderGetInput — huma input for GET /v1/push-providers/{name}. Name — path.
// The format of name (ValidName) — domain validation in GetTyped (422).
type pushProviderGetInput struct {
	Name string `path:"name" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"Push Provider name"`
}

// pushProviderGetOutput — huma output for GET /v1/push-providers/{name} (FULL-TYPED).
// Body — the native 200 body (PushProvider).
type pushProviderGetOutput struct {
	Body PushProvider
}

// pushProviderGetOperation — the metadata for GET /v1/push-providers/{name}.
// DefaultStatus=200. READ route: audit is NOT wired. Permission push-provider.read.
// Errors: 403, 404, 422 bad path-name, 500.
func pushProviderGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getPushProvider",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Карточка Push-Provider-а",
		Description:   "Метаданные одbutго Push-Provider-а по имени (ADR-032 S7-2). Permission push-provider.read. Read-only, no audit.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/push-providers/{name} (update) — WRITE+AUDIT push-provider.updated ===

// pushProviderUpdateInput — huma input for PUT /v1/push-providers/{name}. Name — path;
// Body — a typed body (replace semantics for params).
type pushProviderUpdateInput struct {
	Name string `path:"name" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"Push Provider name"`
	Body PushProviderUpdateRequest
}

// PushProviderUpdateRequest — the Go shape of the PUT /v1/push-providers/{name} body
// (replace semantics: params fully replaces the existing set). Params is
// required:"true" (PUT sends the full new set; an empty {} is legitimate — it clears
// params). NOT Optional[T]: read-modify-write on the client, no presence distinction.
// The struct name = the contract schema name (huma DefaultSchemaNamer) — aligned to
// the committed hand-written spec (rollout batch N3). The register-func projects into
// the native handlers.PushProviderUpdateInput.
type PushProviderUpdateRequest struct {
	Params map[string]any `json:"params" required:"true" doc:"полный butвый onбор params (replace); sensitive — vault-refs"`
}

// pushProviderUpdateOutput — huma output for PUT /v1/push-providers/{name} (FULL-TYPED).
// Status=200; Body — the native 200 body (PushProvider).
type pushProviderUpdateOutput struct {
	Status int `json:"-"`
	Body   PushProvider
}

// pushProviderUpdateOperation — the metadata for PUT /v1/push-providers/{name}.
// DefaultStatus=200. Permission push-provider.update + audit push-provider.updated.
// Errors: 400 unknown/malformed, 403 RBAC, 404 not-found, 422 bad path-name/
// sensitive-param, 500.
func pushProviderUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updatePushProvider",
		Method:        http.MethodPut,
		Path:          "/{name}",
		Summary:       "Заменить params Push-Provider-а",
		Description:   "Replace-семантика: params полbutстью заменяет существующий onбор (ADR-032 S7-2). Permission push-provider.update. 404 — записи absent.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/push-providers/{name} (delete) — WRITE+AUDIT push-provider.deleted ===

// pushProviderDeleteInput — huma input for DELETE /v1/push-providers/{name}. Name — path.
// No Body.
type pushProviderDeleteInput struct {
	Name string `path:"name" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"Push Provider name"`
}

// pushProviderNoContentOutput — huma output for the delete 204-write route. No Body
// (legacy contract: 204 No Content). On an output with no Body huma does SetStatus(204) →
// an empty body (wire-identical to the former WriteHeader(204)).
type pushProviderNoContentOutput struct {
	Status int `json:"-"`
}

// pushProviderDeleteOperation — the metadata for DELETE /v1/push-providers/{name}.
// DefaultStatus=204. Permission push-provider.delete + audit push-provider.deleted.
// Errors: 403, 404, 422 bad path-name, 500.
func pushProviderDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deletePushProvider",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Удалить Push-Provider",
		Description:   "Удаляет запись Push-Provider-а (ADR-032 S7-2). Permission push-provider.delete. 404 — записи absent.",
		Tags:          []string{"push-provider"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
