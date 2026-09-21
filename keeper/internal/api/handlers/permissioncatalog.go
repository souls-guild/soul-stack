// Permission-catalog handler, Operator API (`GET /v1/permissions`) — publishes the
// machine-readable RBAC permission catalog (source of truth: rbac.catalog.go,
// closed enum `<resource>.<action>`). Purpose — the role-permission assignment UI
// (`PATCH /v1/roles/{name}/permissions`): the UI fetches real names from the catalog
// instead of hardcoding them (fixes the "unknown_permission" bug on a guessed name).
//
// RBAC — authentication only (valid JWT), no dedicated permission: the catalog is
// self-describing, and requiring a read permission on the permission list is a chicken-and-egg
// (an operator can't learn which permission to assign without already holding one).
// Like health/meta, no audit record (self-describing read API).
//
// selector_keys — the COMMON list of allowed scope keys ([rbac.SelectorKeys]):
// the MVP catalog has no per-permission scope metadata, so we don't invent it
// per-permission and return the same common list for every action.
package handlers

import (
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// PermissionCatalogHandler — `GET /v1/permissions`. Holds no state (the catalog is
// read-only static data from package rbac); safe for concurrent use.
type PermissionCatalogHandler struct {
	logger *slog.Logger
}

// NewPermissionCatalogHandler creates the handler. logger nil → io.Discard.
func NewPermissionCatalogHandler(logger *slog.Logger) *PermissionCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &PermissionCatalogHandler{logger: logger}
}

// Response bodies — FLAT domain types (handler-native T5d). Package api projects
// them into the native PermissionCatalogReply schema (register func). selector_keys — the common
// list of allowed scope keys (see the package doc comment).
type (
	// permissionAction — a single action within a resource.
	permissionAction = PermissionActionView
	// permissionResource — a group of actions of one resource.
	permissionResource = PermissionCatalogItemView
)

// PermissionActionView — a FLAT domain action record (action + selector_keys).
type PermissionActionView struct {
	Action       string
	SelectorKeys []string
}

// PermissionCatalogItemView — a FLAT group of actions of one resource.
type PermissionCatalogItemView struct {
	Resource string
	Actions  []PermissionActionView
}

// PermissionCatalog — the FLAT domain body of `GET /v1/permissions` (handler-native T5d).
type PermissionCatalog struct {
	Items []PermissionCatalogItemView
}

// ListTyped — the domain function for `GET /v1/permissions` (handler-native T5d, READ, no
// audit): assembles the catalog without http.ResponseWriter/*http.Request. The catalog is read-only
// static data from package rbac, errors are impossible → returns only the value (the native
// projection in api builds the wire). The wire shape (items non-nil, resource/action sort order)
// is preserved — a golden pins it byte-for-byte.
func (h *PermissionCatalogHandler) ListTyped() PermissionCatalog {
	return PermissionCatalog{Items: buildPermissionCatalog()}
}

// buildPermissionCatalog parses rbac.AllowedPermissions (keys
// `<resource>.<action>`) into a form grouped by resource. selector_keys —
// the common rbac.SelectorKeys() for every action. Order is deterministic.
func buildPermissionCatalog() []permissionResource {
	selectorKeys := rbac.SelectorKeys()

	byResource := make(map[string][]string)
	for name := range rbac.AllowedPermissions {
		// The catalog grammar is exactly `<resource>.<action>` (an action may
		// contain a hyphen: `soul.ssh-target-update`). Split on the FIRST dot —
		// a resource holds no dot in the MVP catalog.
		dot := strings.IndexByte(name, '.')
		if dot < 0 {
			continue // defensive: the catalog guarantees a dot, but don't crash on drift
		}
		resource, action := name[:dot], name[dot+1:]
		byResource[resource] = append(byResource[resource], action)
	}

	resources := make([]string, 0, len(byResource))
	for res := range byResource {
		resources = append(resources, res)
	}
	sort.Strings(resources)

	items := make([]permissionResource, 0, len(resources))
	for _, res := range resources {
		actions := byResource[res]
		sort.Strings(actions)
		acts := make([]permissionAction, 0, len(actions))
		for _, a := range actions {
			acts = append(acts, permissionAction{Action: a, SelectorKeys: selectorKeys})
		}
		items = append(items, permissionResource{Resource: res, Actions: acts})
	}
	return items
}
