package api

// FULL-TYPED form of the HERALD domain (heralds + tidings; code-first source of OpenAPI,
// ADR-054 §Pattern). ROLLOUT-BATCH-2c (herald migrated wholesale to huma, modeled on the
// role/operator/augur/push-provider references): herald create/update/delete — WRITE+AUDIT
// (herald.created/.updated/.deleted), herald list/get — read; tiding create/update/
// delete — WRITE+AUDIT (tiding.created/.updated/.deleted), tiding list/get — read.
// Go types are the single source of truth (JSON Schema + validation + typed-output).
//
// update — PUT replace semantics (NOT presence-tier): *T omitempty in body, omit==clear
// (lesson N4). Optional[T] is not needed here — the FE sends the whole rule.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/heralds (create) — WRITE+AUDIT herald.created ===

// heraldCreateInput — huma-input POST /v1/heralds (FULL-TYPED). Body —
// a typed body: huma decodes and validates it against the schema from the huma tags.
type heraldCreateInput struct {
	Body HeraldCreateRequest
}

// HeraldCreateRequest — Go form of the POST /v1/heralds body (code-first source of the
// schema AND validation). Mirrors the domain HeraldCreateRequest: channel name + type
// (enum webhook in MVP) + config (per-type, for webhook url+opt.) + opt. secret_ref
// (vault-ref) + opt. enabled.
//
// huma tags: required:"true" — mandatory (missing→422); enum type — a value outside
// the set → 422 (schema-validate; inline-enum, the hand-written spec does NOT hoist type into a
// standalone components/schemas — mech-2 skipped). additionalProperties:false (huma default) →
// unknown field → 400. name/config/secret_ref format — domain validation in
// CreateHeraldTyped (422). The struct name = the contract schema name in the OpenAPI
// (committed hand-written spec → HeraldCreateRequest).
type HeraldCreateRequest struct {
	Name      string         `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Herald channel name (kebab-case, 1..63), unique in the cluster"`
	Type      string         `json:"type" required:"true" enum:"custom,discord,email,mattermost,slack,telegram,webhook" doc:"channel type (closed-enum: webhook|telegram|slack|mattermost|discord|custom|email); value outside the enum -> 422"`
	Config    map[string]any `json:"config" required:"true" doc:"per-type config (form depends on type; see catalog GET /v1/herald-types). Channel secret (bot_token/webhook_url/header_secret) — dual-mode: value (plaintext) OR *_ref (vault path)"`
	SecretRef *string        `json:"secret_ref,omitempty" doc:"opt. vault-ref on webhook signing-token (vault:<mount>/<path>); XOR with secret"`
	Secret    *string        `json:"secret,omitempty" doc:"opt. plaintext webhook signing-token (dual-mode, ADR-064): keeper writes it into Vault itself; XOR with secret_ref. Requires TLS-front (secret_ingest.accept_plaintext)"`
	Enabled   *bool          `json:"enabled,omitempty" doc:"channel enabled (omitted → true)"`
}

// heraldCreateOutput — huma-output POST /v1/heralds (FULL-TYPED). Status=201; Body —
// the typed 201 body (huma-native api.Herald, T5b — legacy-generated→native envelope in the register func).
// Wire form (created_by_aid omitempty, secret_ref nullable, created_at/updated_at)
// is pinned by a golden-JSON byte-exact test.
type heraldCreateOutput struct {
	Status int `json:"-"`
	Body   Herald
}

// heraldCreateOperation — metadata for POST /v1/heralds. Path = "/" relative to the
// /v1/heralds chi group. DefaultStatus=201. Permission herald.create + audit
// herald.created. Errors: 400 unknown/malformed, 403 RBAC, 409 herald-exists, 422
// name/type/config/secret_ref validation, 500.
func heraldCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createHerald",
		Method:        http.MethodPost,
		Path:          "/heralds",
		Summary:       "Create Herald channel",
		Description:   "Registers a Herald (notification delivery channel) in the registry (ADR-052). Permission herald.create. 409 — name taken. Secret is not stored (only secret_ref).",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/heralds (list) — READ-with-typed-query (NO audit) ===

// heraldListInput — huma-input GET /v1/heralds (FULL-TYPED typed-query). offset/limit —
// int32 with a default (offset 0, limit 50, matches shared/api.ParsePage). bad-int →
// 400 (parseInto). Range BOUNDS are enforced by the DOMAIN ListHeraldsTyped via
// CheckPageBounds → 400 (NOT huma minimum/maximum, which would give 422 — a wire-change vs.
// the legacy ParsePage 400).
type heraldListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (matches shared/api.ParsePage; out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"page size 1..1000 (matches shared/api.ParsePage; out-of-range → 400)"`
}

// heraldListOutput — huma-output GET /v1/heralds (FULL-TYPED). Body — the typed
// 200 envelope (huma-native api.HeraldListReply: items/offset/limit/total; element
// api.Herald). The wire form of items is pinned by a golden-JSON byte-exact test.
type heraldListOutput struct {
	Body HeraldListReply
}

// heraldListOperation — metadata for GET /v1/heralds. Path = "/" relative to the
// /v1/heralds chi group. DefaultStatus=200. A READ route: audit is NOT wired. Errors: 400 (out-of-
// range pagination), 403 RBAC, 500.
func heraldListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listHeralds",
		Method:        http.MethodGet,
		Path:          "/heralds",
		Summary:       "List Herald channels (paged)",
		Description:   "Herald channel registry with pagination (ADR-052). Permission herald.list. Read-only, no audit.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/heralds/{name} (get) — READ-with-path (NO audit) ===

// heraldGetInput — huma-input GET /v1/heralds/{name}. Name — a path parameter. The
// name format (herald.NamePattern) is domain validation in GetHeraldTyped (422).
type heraldGetInput struct {
	Name string `path:"name" doc:"Herald channel name"`
}

// heraldGetOutput — huma-output GET /v1/heralds/{name} (FULL-TYPED). Body — the typed
// 200 body (huma-native api.Herald). The wire form is pinned by a golden test.
type heraldGetOutput struct {
	Body Herald
}

// heraldGetOperation — metadata for GET /v1/heralds/{name}. DefaultStatus=200. A READ
// route: audit is NOT wired. Permission herald.read. Errors: 403 RBAC, 404 not-found,
// 422 bad path-name, 500.
func heraldGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getHerald",
		Method:        http.MethodGet,
		Path:          "/heralds/{name}",
		Summary:       "Herald channel card",
		Description:   "Metadata of a single Herald channel by name (ADR-052). Permission herald.read. Read-only, no audit.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/heralds/{name} (update) — WRITE+AUDIT herald.updated ===

// heraldUpdateInput — huma-input PUT /v1/heralds/{name}. Name — path; Body — the typed
// body (replace semantics).
type heraldUpdateInput struct {
	Name string `path:"name" doc:"Herald channel name (immutable)"`
	Body HeraldUpdateRequest
}

// HeraldUpdateRequest — Go form of the PUT /v1/heralds/{name} body (replace semantics:
// fields fully replace the existing ones, name immutable). type/config are mandatory;
// secret_ref/enabled are optional. The struct name = the contract schema name in the OpenAPI
// (committed hand-written spec → HeraldUpdateRequest).
type HeraldUpdateRequest struct {
	Type      string         `json:"type" required:"true" enum:"custom,discord,email,mattermost,slack,telegram,webhook" doc:"channel type (closed-enum: webhook|telegram|slack|mattermost|discord|custom|email)"`
	Config    map[string]any `json:"config" required:"true" doc:"per-type config (replace — fully replaces the existing one). Channel secret — dual-mode: value (plaintext) OR *_ref"`
	SecretRef *string        `json:"secret_ref,omitempty" doc:"opt. vault-ref on signing-token; XOR with secret; absence of both clears the signature"`
	Secret    *string        `json:"secret,omitempty" doc:"opt. plaintext webhook signing-token (dual-mode, ADR-064): keeper overwrites it in Vault at the same path; XOR with secret_ref"`
	Enabled   *bool          `json:"enabled,omitempty" doc:"channel enabled (omitted → true)"`
}

// heraldUpdateOutput — huma-output PUT /v1/heralds/{name} (FULL-TYPED). Status=200 WITH
// A BODY (huma-native api.Herald — the updated record). The wire form is pinned by a golden test.
type heraldUpdateOutput struct {
	Status int `json:"-"`
	Body   Herald
}

// heraldUpdateOperation — metadata for PUT /v1/heralds/{name}. DefaultStatus=200.
// Permission herald.update + audit herald.updated. Errors: 400 unknown/malformed,
// 403 RBAC, 404 not-found, 422 body/path-name validation, 500.
func heraldUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateHerald",
		Method:        http.MethodPut,
		Path:          "/heralds/{name}",
		Summary:       "Update Herald channel (replace)",
		Description:   "Replace semantics: fields fully replace the existing ones, name immutable (ADR-052). Permission herald.update. 404 — record absent.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/heralds/{name} (delete) — WRITE+AUDIT herald.deleted ===

// heraldDeleteInput — huma-input DELETE /v1/heralds/{name}. Name — path. No Body.
type heraldDeleteInput struct {
	Name string `path:"name" doc:"Herald channel name"`
}

// heraldNoContentOutput — the common huma-output for herald's 204 write routes (herald.delete /
// tiding.delete). WITHOUT a Body (legacy contract: 204 No Content). huma on an output with no Body
// does SetStatus(204) → an empty body (wire-identical to the previous WriteHeader(204)).
type heraldNoContentOutput struct {
	Status int `json:"-"`
}

// heraldDeleteOperation — metadata for DELETE /v1/heralds/{name}. DefaultStatus=204.
// Permission herald.delete + audit herald.deleted (cascades to clean up related Tidings).
// Errors: 403 RBAC, 404 not-found, 422 bad path-name, 500.
func heraldDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteHerald",
		Method:        http.MethodDelete,
		Path:          "/heralds/{name}",
		Summary:       "Delete Herald channel",
		Description:   "Deletes the Herald cascadingly (related Tidings, ADR-052). Permission herald.delete. 404 — record absent.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/tidings (create) — WRITE+AUDIT tiding.created ===

// tidingCreateInput — huma-input POST /v1/tidings (FULL-TYPED). Body — a typed
// body: huma decodes and validates it against the schema.
type tidingCreateInput struct {
	Body TidingCreateRequest
}

// TidingCreateRequest — Go form of the POST /v1/tidings body (code-first source of the
// schema AND validation). Mirrors the domain TidingCreateRequest: rule name + herald (FK)
// + event_types (run-scope) + opt. filters/selectors + annotations/projection.
// ephemeral/voyage_id are absent — they are server-side (ADR-052(g)). name/event_types/
// projection format — domain validation in CreateTidingTyped (422/409/404). The struct name =
// the contract schema name in the OpenAPI (committed hand-written spec → TidingCreateRequest).
type TidingCreateRequest struct {
	Name         string          `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Tiding rule name (kebab-case, 1..63)"`
	Herald       string          `json:"herald" required:"true" doc:"Herald channel name for delivery (FK on heralds.name)"`
	EventTypes   []string        `json:"event_types" required:"true" doc:"list of event-types in run scope (area-glob or exact); empty -> 422"`
	OnlyFailures *bool           `json:"only_failures,omitempty" doc:"deliver only failures (omitted → false)"`
	OnlyChanges  *bool           `json:"only_changes,omitempty" doc:"deliver only on changes (omitted → false)"`
	Incarnation  *string         `json:"incarnation,omitempty" doc:"opt. selector binding to the source incarnation"`
	Cadence      *string         `json:"cadence,omitempty" doc:"opt. selector binding to the source Cadence schedule"`
	Task         *string         `json:"task,omitempty" doc:"opt. selector subscribing to a specific task (register ∪ id from changed_tasks)"`
	Annotations  *map[string]any `json:"annotations,omitempty" doc:"static operator fields, merged into the webhook body under the annotations key"`
	Projection   *[]string       `json:"projection,omitempty" doc:"allow-list of payload paths; empty/omitted — full form"`
	Enabled      *bool           `json:"enabled,omitempty" doc:"rule enabled (omitted → true)"`
}

// tidingCreateOutput — huma-output POST /v1/tidings (FULL-TYPED). Status=201; Body —
// the typed 201 body (huma-native api.Tiding). The wire form is pinned by a golden-JSON byte-exact test.
type tidingCreateOutput struct {
	Status int `json:"-"`
	Body   Tiding
}

// tidingCreateOperation — metadata for POST /v1/tidings. Path = "/" relative to the
// /v1/tidings chi group. DefaultStatus=201. Permission tiding.create + audit
// tiding.created. Errors: 400 unknown/malformed, 403 RBAC, 404 herald-not-found
// (FK), 409 tiding-exists, 422 name/event_types validation, 500.
func tidingCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createTiding",
		Method:        http.MethodPost,
		Path:          "/tidings",
		Summary:       "Create Tiding rule",
		Description:   "Registers a permanent Tiding subscription rule (ADR-052). Permission tiding.create. 404 — Herald channel does not exist. 409 — name taken.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/tidings (list) — READ-with-typed-query (NO audit) ===

// tidingListInput — huma-input GET /v1/tidings (FULL-TYPED typed-query). offset/limit
// — int32 with a default (parity ParsePage; out-of-range → 400 via CheckPageBounds).
// include_ephemeral — a typed bool with default false (omitted → false; bad bool → 400 at
// the huma-bind phase, parity with the legacy strconv.ParseBool 400).
type tidingListInput struct {
	Offset           int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (matches shared/api.ParsePage; out-of-range → 400)"`
	Limit            int32 `query:"limit" default:"50" doc:"page size 1..1000 (matches shared/api.ParsePage; out-of-range → 400)"`
	IncludeEphemeral bool  `query:"include_ephemeral" default:"false" doc:"include ephemeral (one-off) rules (debugging); omitted → false hides ephemeral ones (ADR-052(g))"`
}

// tidingListOutput — huma-output GET /v1/tidings (FULL-TYPED). Body — the typed
// 200 envelope (huma-native api.TidingListReply: items/offset/limit/total; element
// api.Tiding). The wire form of items is pinned by a golden-JSON byte-exact test.
type tidingListOutput struct {
	Body TidingListReply
}

// tidingListOperation — metadata for GET /v1/tidings. Path = "/" relative to the
// /v1/tidings chi group. DefaultStatus=200. A READ route: audit is NOT wired. Errors: 400 (out-of-
// range pagination / bad include_ephemeral), 403 RBAC, 500.
func tidingListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listTidings",
		Method:        http.MethodGet,
		Path:          "/tidings",
		Summary:       "List Tiding rules (paged)",
		Description:   "Tiding rule registry with pagination (ADR-052). Permission tiding.list. Hides ephemeral rules by default (include_ephemeral=true returns all). Read-only, no audit.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/tidings/{name} (get) — READ-with-path (NO audit) ===

// tidingGetInput — huma-input GET /v1/tidings/{name}. Name — a path parameter. The
// name format (herald.NamePattern) is domain validation in GetTidingTyped (422).
type tidingGetInput struct {
	Name string `path:"name" doc:"Tiding rule name"`
}

// tidingGetOutput — huma-output GET /v1/tidings/{name} (FULL-TYPED). Body — the typed
// 200 body (huma-native api.Tiding). The wire form is pinned by a golden test.
type tidingGetOutput struct {
	Body Tiding
}

// tidingGetOperation — metadata for GET /v1/tidings/{name}. DefaultStatus=200. A READ
// route: audit is NOT wired. Permission tiding.read. Errors: 403 RBAC, 404 not-found,
// 422 bad path-name, 500.
func tidingGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getTiding",
		Method:        http.MethodGet,
		Path:          "/tidings/{name}",
		Summary:       "Tiding rule card",
		Description:   "Metadata of a single Tiding rule by name (ADR-052). Permission tiding.read. Read-only, no audit.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/tidings/{name} (update) — WRITE+AUDIT tiding.updated ===

// tidingUpdateInput — huma-input PUT /v1/tidings/{name}. Name — path; Body — the typed
// body (replace semantics).
type tidingUpdateInput struct {
	Name string `path:"name" doc:"Tiding rule name (immutable)"`
	Body TidingUpdateRequest
}

// TidingUpdateRequest — Go form of the PUT /v1/tidings/{name} body (replace semantics:
// fields fully replace the existing ones, name immutable; omit==clear for opt. fields —
// lesson N4). herald/event_types are mandatory; ephemeral/voyage_id are absent (server-side).
// The struct name = the contract schema name in the OpenAPI (committed hand-written spec → TidingUpdateRequest).
type TidingUpdateRequest struct {
	Herald       string          `json:"herald" required:"true" doc:"Herald channel name for delivery (FK)"`
	EventTypes   []string        `json:"event_types" required:"true" doc:"list of event-types in run scope (replace)"`
	OnlyFailures *bool           `json:"only_failures,omitempty" doc:"deliver only failures (omitted → false)"`
	OnlyChanges  *bool           `json:"only_changes,omitempty" doc:"deliver only on changes (omitted → false)"`
	Incarnation  *string         `json:"incarnation,omitempty" doc:"opt. selector binding to the incarnation; absence clears it"`
	Cadence      *string         `json:"cadence,omitempty" doc:"opt. selector binding to the Cadence; absence clears it"`
	Task         *string         `json:"task,omitempty" doc:"opt. selector subscribing to a task; absence clears it"`
	Annotations  *map[string]any `json:"annotations,omitempty" doc:"static operator fields (replace — absence clears)"`
	Projection   *[]string       `json:"projection,omitempty" doc:"allow-list of payload paths (replace — absence = full form)"`
	Enabled      *bool           `json:"enabled,omitempty" doc:"rule enabled (omitted → true)"`
}

// tidingUpdateOutput — huma-output PUT /v1/tidings/{name} (FULL-TYPED). Status=200 WITH
// A BODY (huma-native api.Tiding — the updated record). The wire form is pinned by a golden test.
type tidingUpdateOutput struct {
	Status int `json:"-"`
	Body   Tiding
}

// tidingUpdateOperation — metadata for PUT /v1/tidings/{name}. DefaultStatus=200.
// Permission tiding.update + audit tiding.updated. Errors: 400 unknown/malformed,
// 403 RBAC, 404 not-found/herald-not-found, 422 body validation, 500.
func tidingUpdateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateTiding",
		Method:        http.MethodPut,
		Path:          "/tidings/{name}",
		Summary:       "Update Tiding rule (replace)",
		Description:   "Replace semantics: fields fully replace the existing ones, name immutable (ADR-052). Permission tiding.update. 404 — record/Herald absent.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/tidings/{name} (delete) — WRITE+AUDIT tiding.deleted ===

// tidingDeleteInput — huma-input DELETE /v1/tidings/{name}. Name — path. No Body.
type tidingDeleteInput struct {
	Name string `path:"name" doc:"Tiding rule name"`
}

// tidingDeleteOperation — metadata for DELETE /v1/tidings/{name}. DefaultStatus=204.
// Permission tiding.delete + audit tiding.deleted. Errors: 403 RBAC, 404 not-found,
// 422 bad path-name, 500.
func tidingDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteTiding",
		Method:        http.MethodDelete,
		Path:          "/tidings/{name}",
		Summary:       "Delete Tiding rule",
		Description:   "Removes the Tiding subscription rule by name (ADR-052). Permission tiding.delete. 404 — record absent.",
		Tags:          []string{"tiding"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
