// Herald-type-catalog handler, Operator API (`GET /v1/herald-types`, ADR-052
// amendment) — publishes a machine-readable catalog of Herald channel types and their
// config fields. Its purpose is the Herald form UI (`POST /v1/heralds`): the UI builds the
// form per-type (which fields, what is required, what is secret) from the catalog instead of
// hardcoding it (the permission/event-type/module catalog pattern).
//
// Source of truth is herald.TypeCatalog (the same [herald.HeraldFieldSpec] that
// validate Herald CRUD via herald.ValidateConfig). The handler does NOT duplicate the
// set: extending a type (editing channelDrivers/emailFields) is automatically
// reflected in the output — catalog/validator drift is impossible.
//
// RBAC — authentication only (a valid JWT), no dedicated permission: the catalog is
// self-describing (the event-type catalog pattern). Read-only, no audit.
package handlers

import (
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
)

// HeraldTypeCatalogHandler — `GET /v1/herald-types`. Holds no state (the catalog is
// read-only static data from package herald); safe for concurrent use.
type HeraldTypeCatalogHandler struct {
	logger *slog.Logger
}

// NewHeraldTypeCatalogHandler builds the handler. logger nil → io.Discard.
func NewHeraldTypeCatalogHandler(logger *slog.Logger) *HeraldTypeCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &HeraldTypeCatalogHandler{logger: logger}
}

// HeraldFieldView — FLAT domain record of one type's config field (name/label/
// required/secret/kind/enum_values). kind is a string ([herald.FieldKind]) for the UI
// form render. EnumValues is non-empty only for kind=enum (the set of allowed strings, incl.
// "" = "field omitted/plain") — the UI renders such a field as a select, not a text input.
type HeraldFieldView struct {
	Name       string
	Label      string
	Required   bool
	Secret     bool
	Kind       string
	EnumValues []string
}

// HeraldTypeView — FLAT domain descriptor of one channel type (type + fields +
// secret_required). SecretRequired=true ⟹ the type has a top-level secret_ref
// (webhook) — the UI shows the field by this flag, not by hardcoding the type.
type HeraldTypeView struct {
	Type           string
	Fields         []HeraldFieldView
	SecretRequired bool
}

// HeraldTypeCatalog — FLAT domain body of `GET /v1/herald-types` (handler-native).
type HeraldTypeCatalog struct {
	Types []HeraldTypeView
}

// ListTyped — domain function `GET /v1/herald-types` (READ, no audit): assembles the
// catalog from the single source of truth (herald.TypeCatalog). Errors are impossible →
// returns only the value (the native projection in api builds the wire).
func (h *HeraldTypeCatalogHandler) ListTyped() HeraldTypeCatalog {
	return buildHeraldTypeCatalog()
}

// buildHeraldTypeCatalog projects herald.TypeCatalog into flat views. Slices are
// non-nil (the native projection returns `[]`, not `null`).
func buildHeraldTypeCatalog() HeraldTypeCatalog {
	descriptors := herald.TypeCatalog()
	types := make([]HeraldTypeView, 0, len(descriptors))
	for _, d := range descriptors {
		fields := make([]HeraldFieldView, 0, len(d.Fields))
		for _, f := range d.Fields {
			fields = append(fields, HeraldFieldView{
				Name:       f.Name,
				Label:      f.Label,
				Required:   f.Required,
				Secret:     f.Secret,
				Kind:       string(f.Kind),
				EnumValues: f.EnumValues,
			})
		}
		types = append(types, HeraldTypeView{Type: string(d.Type), Fields: fields, SecretRequired: d.SecretRequired})
	}
	return HeraldTypeCatalog{Types: types}
}
