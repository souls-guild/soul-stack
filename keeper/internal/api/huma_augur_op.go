package api

// FULL-TYPED shape of the AUGUR domain (omens + rites; code-first OpenAPI source,
// ADR-054 §Pattern). ROLLOUT-BATCH-2b (augur fully on huma following the role/operator
// pattern): omen create (WRITE+AUDIT omen.created), omen list (read-with-typed-query),
// omen get (read-with-path), omen delete (WRITE+AUDIT omen.revoked); rite create
// (WRITE+AUDIT rite.created), rite list (read-with-typed-query, omen= required),
// rite delete (WRITE+AUDIT rite.revoked). Go types are the single source of truth
// (JSON Schema + validation + typed output).

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/augur/omens (create) — WRITE+AUDIT omen.created ===

// omenCreateInput — huma-input for POST /v1/augur/omens (FULL-TYPED). Body is
// the typed body: huma decodes and validates it against the schema from the huma tags.
type omenCreateInput struct {
	Body OmenCreateRequest
}

// OmenCreateRequest — Go shape of the POST /v1/augur/omens body (code-first source
// of both the schema AND validation, handler-native). Omen name + source_type (enum
// vault/prometheus/elk) + endpoint + auth_ref (vault-ref).
//
// huma tags: required:"true" — mandatory (missing→422); enum source_type —
// a value outside the set → 422 (schema-validate, not a duplicate domain check).
// additionalProperties:false (huma default) → unknown field → 400. The format of
// name/endpoint/auth_ref is domain-validated in CreateOmenTyped (422). source_type
// is an inline enum (the handwritten spec does NOT hoist it into standalone
// components/schemas — mechanism-2 is skipped). The struct name is the contract
// schema name in OpenAPI (DefaultSchemaNamer takes reflect.Type.Name()) — aligned
// with the committed handwritten spec (OmenCreateRequest).
type OmenCreateRequest struct {
	Name       string `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя Omen-а (kebab-case, 1..63)"`
	SourceType string `json:"source_type" required:"true" enum:"vault,prometheus,elk" doc:"тип внешней системы; значение вне enum → 422"`
	Endpoint   string `json:"endpoint" required:"true" doc:"URL внешней системы (не секрет)"`
	AuthRef    string `json:"auth_ref" required:"true" doc:"vault-ref на master-credential (vault:<mount>/<path>); сам секрет не хранится"`
}

// omenCreateOutput — huma-output for POST /v1/augur/omens (FULL-TYPED). Status=201;
// Body is the huma-native 201 body (OmenView). The wire shape
// (created_by_aid nullable, created_at second precision) is pinned by a golden-JSON
// byte-exact test (huma_augur_reply_test.go).
type omenCreateOutput struct {
	Status int `json:"-"`
	Body   OmenView
}

// omenCreateOperation — metadata for POST /v1/augur/omens. Path = "/omens"
// relative to the chi group /v1/augur (the full /augur sub-path — a distinct path
// for the spec dump, otherwise a collision with rite-POST). DefaultStatus=201.
// Permission omen.create + audit omen.created. Errors: 400 unknown/malformed,
// 403 RBAC, 409 omen-exists, 422 name/source_type/endpoint/auth_ref validation, 500.
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

// === GET /v1/augur/omens (list) — READ-with-typed-query (no audit) ===

// omenListInput — huma-input for GET /v1/augur/omens (FULL-TYPED typed-query).
// offset/limit are int32 (NOT Go int: huma emits int64 for int, the committed spec
// carries int32) with a default (offset 0, limit 50, matching shared/api.ParsePage).
// A bad int → 400 (parseInto). The range BOUNDS are enforced by the DOMAIN
// ListOmensTyped via CheckPageBounds → 400 (NOT huma minimum/maximum, which would
// give 422 — a wire change against the legacy ParsePage 400).
type omenListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// omenListOutput — huma-output for GET /v1/augur/omens (FULL-TYPED). Body is the
// huma-native 200 envelope (OmenListReply: items/offset/limit/total). The wire
// shape of items is pinned by a golden-JSON byte-exact test.
type omenListOutput struct {
	Body OmenListReply
}

// omenListOperation — metadata for GET /v1/augur/omens. Path = "/omens" relative
// to the chi group /v1/augur. DefaultStatus=200. READ route: no audit attached.
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

// === GET /v1/augur/omens/{name} (get) — READ-with-path (no audit) ===

// omenGetInput — huma-input for GET /v1/augur/omens/{name}. Name is a path
// parameter. The name format (reOmenName) is domain-validated in GetOmenTyped (422).
type omenGetInput struct {
	Name string `path:"name" doc:"имя Omen-а"`
}

// omenGetOutput — huma-output for GET /v1/augur/omens/{name} (FULL-TYPED). Body
// is the huma-native 200 body (OmenView). The wire shape is pinned by a golden test.
type omenGetOutput struct {
	Body OmenView
}

// omenGetOperation — metadata for GET /v1/augur/omens/{name}. DefaultStatus=200.
// READ route: no audit attached. Permission omen.list (read is covered by the
// list permission). Errors: 403 RBAC, 404 not-found, 422 bad path-name, 500.
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

// omenDeleteInput — huma-input for DELETE /v1/augur/omens/{name}. Name is a path param. No Body.
type omenDeleteInput struct {
	Name string `path:"name" doc:"имя Omen-а"`
}

// augurNoContentOutput — the shared huma-output for augur's 204 write routes
// (omen.delete / rite.delete). No Body (legacy contract: 204 No Content). huma on
// an output without Body does SetStatus(204) → empty body (wire-identical to the
// previous WriteHeader(204)).
type augurNoContentOutput struct {
	Status int `json:"-"`
}

// omenDeleteOperation — metadata for DELETE /v1/augur/omens/{name}. DefaultStatus=204.
// Permission omen.delete + audit omen.revoked (the cascade cleans up related Rites).
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

// riteCreateInput — huma-input for POST /v1/augur/rites (FULL-TYPED). Body is
// the typed body: huma decodes and validates it against the schema.
type riteCreateInput struct {
	Body RiteCreateRequest
}

// RiteCreateRequest — Go shape of the POST /v1/augur/rites body (code-first source
// of both the schema AND validation, handler-native). omen + XOR subject (coven/sid)
// + allow (byte-passthrough JSONB, ADR-051 category D) + delegate + token fields.
// allow is json.RawMessage (required:"true"): the raw body bytes go straight to the
// service validator. The XOR subject and the allow/token shape are domain-validated
// in CreateRiteTyped (422). additionalProperties:false → unknown → 400. The struct
// name is the contract schema name in OpenAPI (committed handwritten spec →
// RiteCreateRequest).
type RiteCreateRequest struct {
	Omen         string          `json:"omen" required:"true" doc:"Omen, к которому относится grant"`
	Coven        *string         `json:"coven,omitempty" doc:"субъект-grant по Coven-метке (XOR с sid)"`
	SID          *string         `json:"sid,omitempty" doc:"субъект-grant по конкретному SID (XOR с coven)"`
	Allow        json.RawMessage `json:"allow" required:"true" doc:"allow-list; форма по source_type Omen-а (передаётся как есть)"`
	Delegate     *bool           `json:"delegate,omitempty" doc:"false — брокер (MVP-1); true — делегация (MVP-2)"`
	TokenTTL     *string         `json:"token_ttl,omitempty" doc:"TTL минтуемого scoped-токена; только vault-delegate"`
	TokenNumUses *int            `json:"token_num_uses,omitempty" doc:"лимит использований токена; только vault-delegate"`
}

// riteCreateOutput — huma-output for POST /v1/augur/rites (FULL-TYPED). Status=201;
// Body is the huma-native 201 body (RiteView). allow is byte-passthrough JSONB. The
// wire shape is pinned by a golden-JSON byte-exact test.
type riteCreateOutput struct {
	Status int `json:"-"`
	Body   RiteView
}

// riteCreateOperation — metadata for POST /v1/augur/rites. Path = "/rites"
// relative to the chi group /v1/augur. DefaultStatus=201. Permission rite.create +
// audit rite.created. Errors: 400 unknown/malformed, 403 RBAC, 404 omen-not-found,
// 422 XOR violation/broken allow/token fields, 500.
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

// === GET /v1/augur/rites (list by omen) — READ-with-typed-query (no audit) ===

// riteListInput — huma-input for GET /v1/augur/rites?omen=<name>. Omen is a
// required query filter (augur.md §6 — list-all without an omen scope is deferred).
// The format/presence of omen is domain-validated in ListRitesTyped (422; a
// query string without an enum, like path-name).
type riteListInput struct {
	Omen string `query:"omen" doc:"фильтр by-omen (обязателен в MVP); пустой/битый → 422"`
}

// riteListOutput — huma-output for GET /v1/augur/rites (FULL-TYPED). Body is the
// huma-native 200 body (RiteListReply: items[] under `items`, with NO
// offset/limit/total — list-by-omen has no pagination). The wire shape is pinned
// by a golden test.
type riteListOutput struct {
	Body RiteListReply
}

// riteListOperation — metadata for GET /v1/augur/rites. Path = "/rites" relative
// to the chi group /v1/augur. DefaultStatus=200. READ route: no audit attached.
// Errors: 403 RBAC, 422 omen missing/broken, 500.
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

// riteDeleteInput — huma-input for DELETE /v1/augur/rites/{id}. ID is a path
// parameter (string: DeleteRiteTyped domain-validates it's a positive number →
// 422 on a non-number).
type riteDeleteInput struct {
	ID string `path:"id" doc:"числовой id Rite-а"`
}

// riteDeleteOperation — metadata for DELETE /v1/augur/rites/{id}. DefaultStatus=204.
// Permission rite.delete + audit rite.revoked. Errors: 403 RBAC, 404 not-found, 422
// bad path-id (not a positive number), 500.
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
