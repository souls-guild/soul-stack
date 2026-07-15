package api

// HUMA-NATIVE reply-DTO of the CATALOG domain (permissions / event-types / me-permissions;
// handler-native T5d). The reply/output Body of the three READ catalogs — native Go structs in
// the api package, no legacy generator. The handler (handlers/) returns FLAT domain results;
// the register-func (huma_catalog.go) projects them INTO THESE types directly. All three are
// read-only, no audit.
//
// INVARIANTS (★ wire byte-exact + ★ schema name is stable). Exported struct names =
// the contract schema names (PermissionCatalogReply / PermissionCatalogItem /
// PermissionAction / EventTypeCatalogReply / EventTypeArea / EventTypePoint /
// MyPermissionsReply / MyPermission / MyPermissionScope). Shape 1:1, golden byte-
// exact is pinned by huma_catalog_reply_test.go.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === permissions catalog (GET /v1/permissions) ===

// PermissionAction — a native action within a resource (shape 1:1 with PermissionAction):
// action + selector_keys (both without omitempty; selector_keys — []string).
type PermissionAction struct {
	Action       string   `json:"action"`
	SelectorKeys []string `json:"selector_keys"`
}

// PermissionCatalogItem — a native group of actions for one resource (shape 1:1 with
// PermissionCatalogItem): actions []PermissionAction + resource.
type PermissionCatalogItem struct {
	Actions  []PermissionAction `json:"actions"`
	Resource string             `json:"resource"`
}

// PermissionCatalogReply — the native 200 body of GET /v1/permissions (shape 1:1 with
// PermissionCatalogReply): items []PermissionCatalogItem.
type PermissionCatalogReply struct {
	Items []PermissionCatalogItem `json:"items"`
}

// === event-types catalog (GET /v1/event-types) ===

// EventTypeArea — a native area-glob record (shape 1:1 with EventTypeArea): name.
type EventTypeArea struct {
	Name string `json:"name"`
}

// EventTypePoint — a native point event-type (shape 1:1 with EventTypePoint): name.
type EventTypePoint struct {
	Name string `json:"name"`
}

// EventTypeCatalogReply — the native 200 body of GET /v1/event-types (shape 1:1 with
// EventTypeCatalogReply): areas []EventTypeArea + point_events []EventTypePoint
// (both without omitempty).
type EventTypeCatalogReply struct {
	Areas       []EventTypeArea  `json:"areas"`
	PointEvents []EventTypePoint `json:"point_events"`
}

// === herald-types catalog (GET /v1/herald-types) ===

// HeraldTypeFieldSpec — a native description of one config field of a channel type (shape 1:1
// with the domain HeraldFieldView): name/label — strings; required/secret — bool without
// omitempty; kind — a string (herald.FieldKind); enum_values — []string with omitempty
// (present only for kind=enum; the UI renders the field as a select). An empty element
// "" in the set = "field omitted/plain" is allowed.
type HeraldTypeFieldSpec struct {
	Name       string   `json:"name"`
	Label      string   `json:"label"`
	Required   bool     `json:"required"`
	Secret     bool     `json:"secret"`
	Kind       string   `json:"kind"`
	EnumValues []string `json:"enum_values,omitempty"`
}

// HeraldTypeCatalogEntry — a native descriptor of one channel type: type + fields
// (both non-nil; fields []HeraldTypeFieldSpec without omitempty) + secret_required
// (bool without omitempty: whether a top-level type-level secret_ref exists, true for webhook,
// false for the rest; the UI shows the secret_ref field by it, not by a hardcoded type).
type HeraldTypeCatalogEntry struct {
	Type           string                `json:"type"`
	Fields         []HeraldTypeFieldSpec `json:"fields"`
	SecretRequired bool                  `json:"secret_required"`
}

// HeraldTypeCatalogReply — the native 200 body of GET /v1/herald-types: types
// []HeraldTypeCatalogEntry (without omitempty).
type HeraldTypeCatalogReply struct {
	Types []HeraldTypeCatalogEntry `json:"types"`
}

// === me-permissions (GET /v1/me/permissions) ===

// MyPermissionScope — a native scope summary of one effective permission (shape 1:1 with
// MyPermissionScope): covens/regex/soulprint/state — `*[]string` with omitempty
// (dimension fields are omitted when empty); unrestricted — bool without omitempty.
type MyPermissionScope struct {
	Covens       *[]string `json:"covens,omitempty"`
	Regex        *[]string `json:"regex,omitempty"`
	Soulprint    *[]string `json:"soulprint,omitempty"`
	State        *[]string `json:"state,omitempty"`
	Unrestricted bool      `json:"unrestricted"`
}

// MyPermission — a single native effective permission (shape 1:1 with MyPermission):
// action/resource/wildcard — optional pointers with omitempty; scope — `*MyPermissionScope`
// with omitempty.
type MyPermission struct {
	Action   *string            `json:"action,omitempty"`
	Resource *string            `json:"resource,omitempty"`
	Scope    *MyPermissionScope `json:"scope,omitempty"`
	Wildcard *bool              `json:"wildcard,omitempty"`
}

// MyPermissionsReply — the native 200 body of GET /v1/me/permissions (shape 1:1 with
// MyPermissionsReply): permissions []MyPermission.
type MyPermissionsReply struct {
	Permissions []MyPermission `json:"permissions"`
}

// === projection of FLAT domain handler results → native wire-DTO ===

// newPermissionCatalogReply projects handlers.PermissionCatalog into native (items —
// non-nil, the handler gives a deterministically sorted set).
func newPermissionCatalogReply(c handlers.PermissionCatalog) PermissionCatalogReply {
	items := make([]PermissionCatalogItem, 0, len(c.Items))
	for _, it := range c.Items {
		actions := make([]PermissionAction, 0, len(it.Actions))
		for _, a := range it.Actions {
			actions = append(actions, PermissionAction{Action: a.Action, SelectorKeys: a.SelectorKeys})
		}
		items = append(items, PermissionCatalogItem{Actions: actions, Resource: it.Resource})
	}
	return PermissionCatalogReply{Items: items}
}

// newEventTypeCatalogReply projects handlers.EventTypeCatalog into native (areas/
// point_events — non-nil).
func newEventTypeCatalogReply(c handlers.EventTypeCatalog) EventTypeCatalogReply {
	areas := make([]EventTypeArea, 0, len(c.Areas))
	for _, a := range c.Areas {
		areas = append(areas, EventTypeArea{Name: a.Name})
	}
	points := make([]EventTypePoint, 0, len(c.PointEvents))
	for _, p := range c.PointEvents {
		points = append(points, EventTypePoint{Name: p.Name})
	}
	return EventTypeCatalogReply{Areas: areas, PointEvents: points}
}

// newHeraldTypeCatalogReply projects handlers.HeraldTypeCatalog into native
// (types/fields — non-nil).
func newHeraldTypeCatalogReply(c handlers.HeraldTypeCatalog) HeraldTypeCatalogReply {
	types := make([]HeraldTypeCatalogEntry, 0, len(c.Types))
	for _, ty := range c.Types {
		fields := make([]HeraldTypeFieldSpec, 0, len(ty.Fields))
		for _, f := range ty.Fields {
			fields = append(fields, HeraldTypeFieldSpec{
				Name:       f.Name,
				Label:      f.Label,
				Required:   f.Required,
				Secret:     f.Secret,
				Kind:       f.Kind,
				EnumValues: f.EnumValues,
			})
		}
		types = append(types, HeraldTypeCatalogEntry{Type: ty.Type, Fields: fields, SecretRequired: ty.SecretRequired})
	}
	return HeraldTypeCatalogReply{Types: types}
}

// newMyPermissionScope projects the flat handlers.MyScope into a native pointer-optional
// (an empty dimension → nil pointer, omitempty drops the key).
func newMyPermissionScope(s *handlers.MyScope) *MyPermissionScope {
	if s == nil {
		return nil
	}
	return &MyPermissionScope{
		Covens:       slicePtrIfNotEmpty(s.Covens),
		Regex:        slicePtrIfNotEmpty(s.Regex),
		Soulprint:    slicePtrIfNotEmpty(s.Soulprint),
		State:        slicePtrIfNotEmpty(s.State),
		Unrestricted: s.Unrestricted,
	}
}

// newMyPermission projects the flat handlers.MyPermission into a native pointer-optional.
// Wildcard → only wildcard=true (resource/action/scope omitted); otherwise resource.action
// + scope (empty "" → nil pointers via ptrIfNotEmpty).
func newMyPermission(p handlers.MyPermission) MyPermission {
	if p.Wildcard {
		t := true
		return MyPermission{Wildcard: &t}
	}
	return MyPermission{
		Action:   strPtrIfNotEmpty(p.Action),
		Resource: strPtrIfNotEmpty(p.Resource),
		Scope:    newMyPermissionScope(p.Scope),
	}
}

// newMyPermissionsReply projects handlers.MyPermissions into native (permissions non-nil).
func newMyPermissionsReply(m handlers.MyPermissions) MyPermissionsReply {
	perms := make([]MyPermission, 0, len(m.Permissions))
	for _, p := range m.Permissions {
		perms = append(perms, newMyPermission(p))
	}
	return MyPermissionsReply{Permissions: perms}
}

// slicePtrIfNotEmpty — nil for an empty/nil slice (omitempty over the array), otherwise a pointer.
func slicePtrIfNotEmpty(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}

// strPtrIfNotEmpty — nil for an empty string (omitempty), otherwise a pointer.
func strPtrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
