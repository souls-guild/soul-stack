// EventType-catalog handler, Operator API (`GET /v1/event-types`) — publishes a
// machine-readable catalog of event-types valid for Tiding subscription (ADR-052(b)).
// Purpose — the UI Tiding form (`POST /v1/tidings`): the UI fetches valid types from
// the catalog instead of hardcoding them (fixes ADR-042, permission/module-catalog pattern).
//
// Source of truth — keeper/internal/herald/eventtypes.go (the same scope that
// validates Tiding CRUD via herald.ValidateEventTypes). The handler does NOT duplicate
// the list: it reads it via the getters [herald.RunScopeAreas] / [herald.RunScopePointEvents].
// Extending scope via an ADR-052 amendment (editing runScope* in herald) is automatically
// reflected in the output — catalog/validator drift is impossible.
//
// RBAC — authentication only (a valid JWT), with NO separate permission: the catalog is
// self-describing (permission-catalog pattern — requiring a read permission for the list
// of values would be chicken-and-egg). Read-only, no audit (health/meta pattern).
package handlers

import (
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
)

// EventTypeCatalogHandler — `GET /v1/event-types`. Holds no state (the catalog is
// read-only static data from the herald package); safe for concurrent use.
type EventTypeCatalogHandler struct {
	logger *slog.Logger
}

// NewEventTypeCatalogHandler creates the handler. logger nil → io.Discard.
func NewEventTypeCatalogHandler(logger *slog.Logger) *EventTypeCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &EventTypeCatalogHandler{logger: logger}
}

// Response bodies — FLAT domain types (handler-native T5d). The api package projects
// them into the native schema EventTypeCatalogReply (register-func).
type (
	// eventTypeArea — one area-glob region of the output.
	eventTypeArea = EventTypeAreaView
	// eventTypePoint — one point event-type of the output.
	eventTypePoint = EventTypePointView
)

// EventTypeAreaView — a FLAT domain area-glob entry (name).
type EventTypeAreaView struct {
	Name string
}

// EventTypePointView — a FLAT domain point event-type (name).
type EventTypePointView struct {
	Name string
}

// EventTypeCatalog — the FLAT domain body of `GET /v1/event-types` (handler-native T5d).
type EventTypeCatalog struct {
	Areas       []EventTypeAreaView
	PointEvents []EventTypePointView
}

// ListTyped — the domain function for `GET /v1/event-types` (handler-native T5d, READ without
// audit): assembles the catalog without http.ResponseWriter/*http.Request. The catalog is
// read-only static data from the herald package, errors are impossible → returns only the
// value (the native projection in api builds the wire). The wire shape (areas/point_events
// non-nil) is preserved — golden pins it byte-for-byte.
func (h *EventTypeCatalogHandler) ListTyped() EventTypeCatalog {
	return buildEventTypeCatalog()
}

// buildEventTypeCatalog assembles the catalog from the single source of truth (herald).
// Slices are non-nil (the native projection emits `[]`, not `null`) even for an empty scope.
func buildEventTypeCatalog() EventTypeCatalog {
	areaNames := herald.RunScopeAreas()
	areas := make([]eventTypeArea, 0, len(areaNames))
	for _, name := range areaNames {
		// area-glob in ready form `<area>.*` — subscribable as-is. A bare area name
		// (`scenario_run`) is NOT valid for herald.ValidateEventTypes
		// (which requires `<area>.*` or `<area>.<action>`), so the catalog emits the glob.
		areas = append(areas, eventTypeArea{Name: name + ".*"})
	}

	pointNames := herald.RunScopePointEvents()
	points := make([]eventTypePoint, 0, len(pointNames))
	for _, name := range pointNames {
		points = append(points, eventTypePoint{Name: name})
	}

	return EventTypeCatalog{Areas: areas, PointEvents: points}
}
