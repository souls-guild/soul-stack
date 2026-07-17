package api

// FULL-TYPED shape of the ORACLE domain (vigils + decrees; code-first source of OpenAPI,
// ADR-054 §Pattern). ROLLOUT BATCH 2b (oracle entirely on huma following the role/operator/
// augur patterns): vigil create (WRITE+AUDIT vigil.created), vigil list (read-with-typed-
// query), vigil get (read-with-path), vigil delete (WRITE+AUDIT vigil.deleted);
// decree symmetrically (decree.created / decree.deleted). Go types are the single
// source of truth (JSON Schema + validation + typed output).
//
// vigil/decree operations carry FULL paths (/vigils[/{name}], /decrees[/{name}]) and
// are mounted directly on /v1 (per-route RBAC chi group) — a distinct path for the
// spec dump (otherwise vigil-POST and decree-POST would land on the same "/").

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === POST /v1/vigils (create) — WRITE+AUDIT vigil.created ===

// vigilCreateInput — huma input POST /v1/vigils (FULL-TYPED). Body —
// the typed body: huma decodes and validates it against the schema.
type vigilCreateInput struct {
	Body VigilCreateRequest
}

// VigilCreateRequest — the Go shape of the POST /v1/vigils body (code-first source of the schema AND
// validation). Mirrors the domain VigilCreateRequest: name + XOR subject
// (coven/sid) + interval/check + params (byte-passthrough JSONB, ADR-051 category D)
// + enabled. params — *json.RawMessage: the raw body bytes go straight to the service.
// The XOR subject and the shape of interval/check/params are domain validation in CreateVigilTyped
// (422). required:"true" — missing→422; additionalProperties:false → unknown→400.
// The struct name = the contract schema name in OpenAPI (committed hand-written spec → VigilCreateRequest).
type VigilCreateRequest struct {
	Name     string           `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Vigil name (kebab-case, 1..63)"`
	Coven    *[]string        `json:"coven,omitempty" doc:"subject Coven tags (XOR with sid)"`
	SID      *string          `json:"sid,omitempty" doc:"subject — single specific SID (XOR with coven)"`
	Interval string           `json:"interval" required:"true" doc:"check frequency (duration convention, e.g. '30s')"`
	Check    string           `json:"check" required:"true" doc:"core-beacon address (e.g. 'core.beacon.file_changed')"`
	Params   *json.RawMessage `json:"params,omitempty" doc:"check parameters; shape depends on check (passed through as-is)"`
	Enabled  *bool            `json:"enabled,omitempty" doc:"whether the check is active (default true)"`
}

// vigilCreateOutput — huma output POST /v1/vigils (FULL-TYPED). Status=201; Body —
// the native 201 body (VigilView). params — byte-passthrough JSONB. The wire shape
// is pinned by a golden-JSON byte-exact test.
type vigilCreateOutput struct {
	Status int `json:"-"`
	Body   VigilView
}

// vigilCreateOperation — metadata for POST /v1/vigils. Path = "/vigils" (full, for a
// distinct spec dump). DefaultStatus=201. Permission vigil.create + audit
// vigil.created. Errors: 400 unknown/malformed, 403 RBAC, 409 vigil-exists, 422
// validation of name/interval/check/subject, 500.
func vigilCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createVigil",
		Method:        http.MethodPost,
		Path:          "/vigils",
		Summary:       "Create Vigil",
		Description:   "Registers a Vigil (Soul-side check) in the oracle registry (ADR-030). Permission vigil.create. 409 -- name taken.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/vigils (list) — READ-with-typed-query (no audit) ===

// vigilListInput — huma input GET /v1/vigils (FULL-TYPED typed query). offset/limit
// — int32 (the committed spec carries int32) with a default. bad-int → 400 (parseInto). BOUNDS
// are enforced by CheckPageBounds in ListVigilsTyped → 400 (NOT huma minimum/maximum).
type vigilListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// vigilListOutput — huma output GET /v1/vigils (FULL-TYPED). Body — the native
// 200 envelope (VigilListReply: items/offset/limit/total). The wire shape is pinned
// by a golden test.
type vigilListOutput struct {
	Body VigilListReply
}

// vigilListOperation — metadata for GET /v1/vigils. DefaultStatus=200. READ route:
// audit not wired. Errors: 400 (out-of-range pagination), 403 RBAC, 500.
func vigilListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listVigils",
		Method:        http.MethodGet,
		Path:          "/vigils",
		Summary:       "List of Vigils (paged)",
		Description:   "Vigil registry with pagination (ADR-030). Permission vigil.list. Read-only, no audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/vigils/{name} (get) — READ-with-path (no audit) ===

// vigilGetInput — huma input GET /v1/vigils/{name}. Name — path. The name format
// (reOracleName) is domain validation in GetVigilTyped (422).
type vigilGetInput struct {
	Name string `path:"name" doc:"Vigil name"`
}

// vigilGetOutput — huma output GET /v1/vigils/{name} (FULL-TYPED). Body — the native
// 200 body (VigilView). The wire shape is pinned by a golden test.
type vigilGetOutput struct {
	Body VigilView
}

// vigilGetOperation — metadata for GET /v1/vigils/{name}. DefaultStatus=200. READ route:
// audit not wired. Permission vigil.list (read is covered by the list permission). Errors: 403,
// 404, 422 bad path-name, 500.
func vigilGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getVigil",
		Method:        http.MethodGet,
		Path:          "/vigils/{name}",
		Summary:       "Vigil card",
		Description:   "Metadata of a single Vigil by name (ADR-030). Permission vigil.list (read is covered by the list permission). Read-only, no audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/vigils/{name} (delete) — WRITE+AUDIT vigil.deleted ===

// vigilDeleteInput — huma input DELETE /v1/vigils/{name}. Name — path. No Body.
type vigilDeleteInput struct {
	Name string `path:"name" doc:"Vigil name"`
}

// oracleNoContentOutput — the shared huma output for 204 write routes of oracle (vigil.delete /
// decree.delete). NO Body (legacy contract: 204 No Content). huma on an output without Body
// does SetStatus(204) → empty body (wire-identical to the former WriteHeader(204)).
type oracleNoContentOutput struct {
	Status int `json:"-"`
}

// vigilDeleteOperation — metadata for DELETE /v1/vigils/{name}. DefaultStatus=204.
// Permission vigil.delete + audit vigil.deleted. Errors: 403, 404, 422 bad path-name,
// 500.
func vigilDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteVigil",
		Method:        http.MethodDelete,
		Path:          "/vigils/{name}",
		Summary:       "Delete Vigil",
		Description:   "Deletes a Vigil from the oracle registry (ADR-030). Permission vigil.delete. 404 -- record absent.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/decrees (create) — WRITE+AUDIT decree.created ===

// decreeCreateInput — huma input POST /v1/decrees (FULL-TYPED). Body —
// the typed body.
type decreeCreateInput struct {
	Body DecreeCreateRequest
}

// DecreeCreateRequest — the Go shape of the POST /v1/decrees body (code-first source of the schema
// AND validation). Mirrors the domain DecreeCreateRequest: name + on_beacon +
// XOR subject (coven/sid) + incarnation_name + action_scenario/action_input
// (byte-passthrough JSONB) + where-CEL + cooldown + enabled. action_input —
// *json.RawMessage. Subject/where-CEL/cooldown validation is domain-level (422). The struct
// name = the contract schema name in OpenAPI (committed hand-written spec → DecreeCreateRequest).
type DecreeCreateRequest struct {
	Name            string           `json:"name" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Decree name (kebab-case, 1..63)"`
	OnBeacon        string           `json:"on_beacon" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"Vigil name whose Portent the rule reacts to"`
	Coven           *[]string        `json:"coven,omitempty" doc:"subject Coven tags (XOR with sid)"`
	SID             *string          `json:"sid,omitempty" doc:"subject — single specific SID (XOR with coven)"`
	IncarnationName string           `json:"incarnation_name" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"target incarnation of the reaction (required)"`
	ActionScenario  string           `json:"action_scenario" required:"true" pattern:"^[a-z][a-z0-9_]*$" doc:"named scenario (whitelist; raw command rejected)"`
	ActionInput     *json.RawMessage `json:"action_input,omitempty" doc:"scenario input (vault-ref passed through as-is)"`
	Where           *string          `json:"where,omitempty" doc:"opt. CEL predicate over event.data; compile-checked"`
	Cooldown        *string          `json:"cooldown,omitempty" doc:"minimum interval between triggers per-(decree, subject)"`
	Enabled         *bool            `json:"enabled,omitempty" doc:"whether the rule is active (default true)"`
}

// decreeCreateOutput — huma output POST /v1/decrees (FULL-TYPED). Status=201; Body —
// the native 201 body (DecreeView). action_input — byte-passthrough JSONB. The wire shape
// is pinned by a golden-JSON byte-exact test.
type decreeCreateOutput struct {
	Status int `json:"-"`
	Body   DecreeView
}

// decreeCreateOperation — metadata for POST /v1/decrees. Path = "/decrees".
// DefaultStatus=201. Permission decree.create + audit decree.created. Errors: 400
// unknown/malformed, 403 RBAC, 409 decree-exists, 422 validation of fields/subject/
// where-CEL/cooldown, 500.
func decreeCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createDecree",
		Method:        http.MethodPost,
		Path:          "/decrees",
		Summary:       "Create Decree",
		Description:   "Registers a Decree (reactor rule) in the oracle registry (ADR-030). Permission decree.create. 409 -- name taken.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/decrees (list) — READ-with-typed-query (no audit) ===

// decreeListInput — huma input GET /v1/decrees (FULL-TYPED typed query). offset/limit
// — int32 with a default; the range is enforced by CheckPageBounds → 400.
type decreeListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// decreeListOutput — huma-output GET /v1/decrees (FULL-TYPED). Body — native
// 200-envelope (DecreeListReply: items/offset/limit/total).
type decreeListOutput struct {
	Body DecreeListReply
}

// decreeListOperation — metadata for GET /v1/decrees. DefaultStatus=200. READ route:
// audit not wired. Errors: 400 (out-of-range pagination), 403 RBAC, 500.
func decreeListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listDecrees",
		Method:        http.MethodGet,
		Path:          "/decrees",
		Summary:       "List of Decrees (paged)",
		Description:   "Decree registry with pagination (ADR-030). Permission decree.list. Read-only, no audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/decrees/{name} (get) — READ-with-path (no audit) ===

// decreeGetInput — huma input GET /v1/decrees/{name}. Name — path. The name format is
// domain validation in GetDecreeTyped (422).
type decreeGetInput struct {
	Name string `path:"name" doc:"Decree name"`
}

// decreeGetOutput — huma output GET /v1/decrees/{name} (FULL-TYPED). Body — the native
// 200 body (DecreeView).
type decreeGetOutput struct {
	Body DecreeView
}

// decreeGetOperation — metadata for GET /v1/decrees/{name}. DefaultStatus=200.
// READ route: audit not wired. Permission decree.list (read is covered by the list permission).
// Errors: 403, 404, 422 bad path-name, 500.
func decreeGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getDecree",
		Method:        http.MethodGet,
		Path:          "/decrees/{name}",
		Summary:       "Decree card",
		Description:   "Metadata of a single Decree by name (ADR-030). Permission decree.list (read is covered by the list permission). Read-only, no audit.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/decrees/{name} (delete) — WRITE+AUDIT decree.deleted ===

// decreeDeleteInput — huma input DELETE /v1/decrees/{name}. Name — path. No Body.
type decreeDeleteInput struct {
	Name string `path:"name" doc:"Decree name"`
}

// decreeDeleteOperation — metadata for DELETE /v1/decrees/{name}. DefaultStatus=204.
// Permission decree.delete + audit decree.deleted (cascade clears cooldown state).
// Errors: 403, 404, 422 bad path-name, 500.
func decreeDeleteOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "deleteDecree",
		Method:        http.MethodDelete,
		Path:          "/decrees/{name}",
		Summary:       "Delete Decree",
		Description:   "Deletes a Decree cascading (cooldown state, ADR-030). Permission decree.delete. 404 -- record absent.",
		Tags:          []string{"oracle"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
