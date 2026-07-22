package api

// FULL-TYPED shape of the Operator API READ catalogs (code-first OpenAPI source,
// ADR-054 §Pattern, the READ variant of pilot-1, no audit). The read-tier rollout
// batch: bare-GET catalogs with no input parameters — `GET /v1/permissions`,
// `GET /v1/event-types`, `GET /v1/herald-types`, `GET /v1/me/permissions`. The Go
// types are the single source of truth: huma builds both the OpenAPI-fragment JSON
// Schema and the typed output from them.
//
// All are READ without filters: input — an empty struct (huma requires no
// Body/Path/Query fields for a bare-GET, like roleListInput). Output — a typed Body
// = alias for the generated oapi-reply (the same type the legacy writeJSON emitted),
// so the wire bytes are identical; huma merely serializes the slice the handler has
// already assembled. omitempty/[]-vs-null are held by the oapi types themselves — a
// golden-JSON snapshot pins this byte-for-byte (the main read-tier guard).

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/permissions — RBAC permissions catalog ===

// permissionsListInput — huma input for GET /v1/permissions. No parameters (a
// catalog without filters) — an empty struct (parity with roleListInput).
type permissionsListInput struct{}

// permissionsListOutput — huma output for GET /v1/permissions (FULL-TYPED). Body —
// the typed 200 body (huma-native api.PermissionCatalogReply, T5b — the
// legacy-generator→native envelope in the register func). The wire shape (items
// non-nil, resource/action sorting, selector_keys) is pinned by a golden-JSON
// snapshot test.
type permissionsListOutput struct {
	Body PermissionCatalogReply
}

// permissionsListOperation — metadata for GET /v1/permissions. Path = "/permissions"
// relative to the /v1 chi group (huma.API is mounted on it; chi.Walk sees the route
// /v1/permissions, the drift-test is green). Absolute (not "/") — the catalogs live
// on ONE huma.API/spec dump, and a distinct path rules out an operation collision
// (unlike cadence/role, where the `/`+`/{name}` shape gives a distinct path by
// itself). DefaultStatus=200. READ route: audit not wired.
func permissionsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listPermissions",
		Method:        http.MethodGet,
		Path:          "/permissions",
		Summary:       "RBAC permissions catalog",
		Description:   "Machine-readable catalog of `<resource>.<action>` (sourced from rbac.catalog.go), grouped by resource. Auth-only, no dedicated permission (self-describing). Read-only, no audit.",
		Tags:          []string{"permission"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}

// === GET /v1/event-types — event-types catalog for Tiding subscriptions ===

// eventTypesListInput — huma input for GET /v1/event-types. No parameters — an
// empty struct (parity with roleListInput).
type eventTypesListInput struct{}

// eventTypesListOutput — huma output for GET /v1/event-types (FULL-TYPED). Body —
// the typed 200 body (huma-native api.EventTypeCatalogReply). The wire shape
// (areas/point_events non-nil, area-glob `<name>.*`) is pinned by a golden-JSON
// snapshot test.
type eventTypesListOutput struct {
	Body EventTypeCatalogReply
}

// eventTypesListOperation — metadata for GET /v1/event-types. Path = "/event-types"
// relative to the /v1 chi group (absolute — a distinct path on the shared
// huma.API/dump, see permissionsListOperation). DefaultStatus=200. READ route:
// audit not wired.
func eventTypesListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listEventTypes",
		Method:        http.MethodGet,
		Path:          "/event-types",
		Summary:       "Event-types catalog for Tiding subscriptions",
		Description:   "Valid event types for Tiding subscriptions: areas (area-glob `<name>.*`) + point-wise point_events (sourced from herald/eventtypes.go). Auth-only, no dedicated permission (self-describing). Read-only, no audit.",
		Tags:          []string{"event-type"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}

// === GET /v1/herald-types — Herald channel types catalog ===

// heraldTypesListInput — huma input for GET /v1/herald-types. No parameters — an
// empty struct (parity with roleListInput).
type heraldTypesListInput struct{}

// heraldTypesListOutput — huma output for GET /v1/herald-types (FULL-TYPED). Body —
// the typed 200 body (huma-native api.HeraldTypeCatalogReply). The wire shape
// (types/fields non-nil, types sorted like AllHeraldTypes) is pinned by a
// golden-JSON snapshot test.
type heraldTypesListOutput struct {
	Body HeraldTypeCatalogReply
}

// heraldTypesListOperation — metadata for GET /v1/herald-types. Path =
// "/herald-types" relative to the /v1 chi group (absolute — a distinct path on the
// shared huma.API/dump, see permissionsListOperation). DefaultStatus=200. READ
// route: audit not wired.
func heraldTypesListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listHeraldTypes",
		Method:        http.MethodGet,
		Path:          "/herald-types",
		Summary:       "Herald channel types catalog",
		Description:   "Notification channel types and their config fields (webhook/telegram/slack/mattermost/discord/custom/email): name/label/required/secret/kind. Sourced from herald.TypeCatalog (the same one which validates CRUD). Auth-only, no dedicated permission (self-describing). Read-only, no audit.",
		Tags:          []string{"herald"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}

// === GET /v1/me/permissions — effective permissions of the current Archon ===

// myPermissionsListInput — huma input for GET /v1/me/permissions. No parameters (the
// AID comes from claims, NOT from the query) — an empty struct (parity with
// roleListInput).
type myPermissionsListInput struct{}

// myPermissionsListOutput — huma output for GET /v1/me/permissions (FULL-TYPED).
// Body — the typed 200 body (huma-native api.MyPermissionsReply). The wire shape
// (permissions non-nil, pointer-optional, snake_case scope keys) is pinned by a
// golden-JSON snapshot test.
type myPermissionsListOutput struct {
	Body MyPermissionsReply
}

// myPermissionsListOperation — metadata for GET /v1/me/permissions. Path =
// "/me/permissions" relative to the /v1 chi group (absolute — a distinct path on the
// shared huma.API/dump, see permissionsListOperation). DefaultStatus=200. READ
// route: audit not wired. 500 — no claims in the context (the auth chain is not
// assembled, a server error).
func myPermissionsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listMyPermissions",
		Method:        http.MethodGet,
		Path:          "/me/permissions",
		Summary:       "Effective permissions of the current Archon",
		Description:   "Subset of the catalog actually granted to the current operator (AID from JWT-claims). Auth-only (any authenticated caller can see their own permissions; others' are not exposed). Read-only, no audit.",
		Tags:          []string{"permission"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusInternalServerError},
	}
}
