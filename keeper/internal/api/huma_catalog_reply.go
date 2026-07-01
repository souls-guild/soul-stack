package api

// HUMA-NATIVE reply-DTO CATALOG-домена (permissions / event-types / me-permissions;
// handler-native T5d). Reply/output Body трёх READ-каталогов — native Go-struct в
// пакете api, БЕЗ legacy-генерата. Handler (handlers/) возвращает ПЛОСКИЕ доменные result-ы;
// register-func (huma_catalog.go) проецирует их В ЭТИ типы напрямую. Все три —
// read-only, БЕЗ audit.
//
// ИНВАРИАНТЫ (★ wire byte-exact + ★ имя схемы стабильно). Имена exported-структур =
// контрактные имена схем (PermissionCatalogReply / PermissionCatalogItem /
// PermissionAction / EventTypeCatalogReply / EventTypeArea / EventTypePoint /
// MyPermissionsReply / MyPermission / MyPermissionScope). Форма 1:1, golden byte-
// exact фиксирует huma_catalog_reply_test.go.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === permissions catalog (GET /v1/permissions) ===

// PermissionAction — native action в составе resource (форма 1:1 с PermissionAction):
// action + selector_keys (оба БЕЗ omitempty; selector_keys — []string).
type PermissionAction struct {
	Action       string   `json:"action"`
	SelectorKeys []string `json:"selector_keys"`
}

// PermissionCatalogItem — native группа actions одного resource (форма 1:1 с
// PermissionCatalogItem): actions []PermissionAction + resource.
type PermissionCatalogItem struct {
	Actions  []PermissionAction `json:"actions"`
	Resource string             `json:"resource"`
}

// PermissionCatalogReply — native 200-тело GET /v1/permissions (форма 1:1 с
// PermissionCatalogReply): items []PermissionCatalogItem.
type PermissionCatalogReply struct {
	Items []PermissionCatalogItem `json:"items"`
}

// === event-types catalog (GET /v1/event-types) ===

// EventTypeArea — native area-glob-запись (форма 1:1 с EventTypeArea): name.
type EventTypeArea struct {
	Name string `json:"name"`
}

// EventTypePoint — native точечный event-type (форма 1:1 с EventTypePoint): name.
type EventTypePoint struct {
	Name string `json:"name"`
}

// EventTypeCatalogReply — native 200-тело GET /v1/event-types (форма 1:1 с
// EventTypeCatalogReply): areas []EventTypeArea + point_events []EventTypePoint
// (оба БЕЗ omitempty).
type EventTypeCatalogReply struct {
	Areas       []EventTypeArea  `json:"areas"`
	PointEvents []EventTypePoint `json:"point_events"`
}

// === herald-types catalog (GET /v1/herald-types) ===

// HeraldTypeFieldSpec — native описание одного config-поля типа канала (форма 1:1
// с доменным HeraldFieldView): name/label — строки; required/secret — bool БЕЗ
// omitempty; kind — строка (herald.FieldKind); enum_values — []string С omitempty
// (присутствует только у kind=enum; UI рендерит поле как select). Пустой элемент
// "" в наборе = «поле опущено/plain» разрешён.
type HeraldTypeFieldSpec struct {
	Name       string   `json:"name"`
	Label      string   `json:"label"`
	Required   bool     `json:"required"`
	Secret     bool     `json:"secret"`
	Kind       string   `json:"kind"`
	EnumValues []string `json:"enum_values,omitempty"`
}

// HeraldTypeCatalogEntry — native дескриптор одного типа канала: type + fields
// (оба non-nil; fields []HeraldTypeFieldSpec БЕЗ omitempty) + secret_required
// (bool БЕЗ omitempty: признак top-level secret_ref уровня типа, у webhook true,
// у прочих false; UI показывает поле secret_ref по нему, не по хардкоду типа).
type HeraldTypeCatalogEntry struct {
	Type           string                `json:"type"`
	Fields         []HeraldTypeFieldSpec `json:"fields"`
	SecretRequired bool                  `json:"secret_required"`
}

// HeraldTypeCatalogReply — native 200-тело GET /v1/herald-types: types
// []HeraldTypeCatalogEntry (БЕЗ omitempty).
type HeraldTypeCatalogReply struct {
	Types []HeraldTypeCatalogEntry `json:"types"`
}

// === me-permissions (GET /v1/me/permissions) ===

// MyPermissionScope — native scope-сводка одного эффективного права (форма 1:1 с
// MyPermissionScope): covens/regex/soulprint/state — `*[]string` С omitempty
// (поля-измерения опускаются при пустоте); unrestricted — bool БЕЗ omitempty.
type MyPermissionScope struct {
	Covens       *[]string `json:"covens,omitempty"`
	Regex        *[]string `json:"regex,omitempty"`
	Soulprint    *[]string `json:"soulprint,omitempty"`
	State        *[]string `json:"state,omitempty"`
	Unrestricted bool      `json:"unrestricted"`
}

// MyPermission — native одно эффективное право (форма 1:1 с MyPermission):
// action/resource/wildcard — опц. указатели С omitempty; scope — `*MyPermissionScope`
// С omitempty.
type MyPermission struct {
	Action   *string            `json:"action,omitempty"`
	Resource *string            `json:"resource,omitempty"`
	Scope    *MyPermissionScope `json:"scope,omitempty"`
	Wildcard *bool              `json:"wildcard,omitempty"`
}

// MyPermissionsReply — native 200-тело GET /v1/me/permissions (форма 1:1 с
// MyPermissionsReply): permissions []MyPermission.
type MyPermissionsReply struct {
	Permissions []MyPermission `json:"permissions"`
}

// === проекция ПЛОСКИХ доменных handler-result-ов → native wire-DTO ===

// newPermissionCatalogReply проецирует handlers.PermissionCatalog в native (items —
// non-nil, handler даёт детерминированно отсортированный набор).
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

// newEventTypeCatalogReply проецирует handlers.EventTypeCatalog в native (areas/
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

// newHeraldTypeCatalogReply проецирует handlers.HeraldTypeCatalog в native
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

// newMyPermissionScope проецирует плоскую handlers.MyScope в native pointer-optional
// (пустое измерение → nil-указатель, omitempty опускает ключ).
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

// newMyPermission проецирует плоскую handlers.MyPermission в native pointer-optional.
// Wildcard → только wildcard=true (resource/action/scope опущены); иначе resource.action
// + scope (пустые "" → nil-указатели через ptrIfNotEmpty).
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

// newMyPermissionsReply проецирует handlers.MyPermissions в native (permissions non-nil).
func newMyPermissionsReply(m handlers.MyPermissions) MyPermissionsReply {
	perms := make([]MyPermission, 0, len(m.Permissions))
	for _, p := range m.Permissions {
		perms = append(perms, newMyPermission(p))
	}
	return MyPermissionsReply{Permissions: perms}
}

// slicePtrIfNotEmpty — nil для пустого/nil-среза (omitempty над массивом), иначе указатель.
func slicePtrIfNotEmpty(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}

// strPtrIfNotEmpty — nil для пустой строки (omitempty), иначе указатель.
func strPtrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
