// Package profile is the Cloud Profile registry in Postgres (ADR-017,
// docs/keeper/cloud.md).
//
// Cloud.CRUD.a: types + CRUD (Insert / SelectByName / SelectAll /
// SelectByProvider). Profile is a VM spec on top of a concrete Provider:
// `Params` (jsonb, freeform VM spec) + optional `CloudInit` (userdata).
//
// Validation of `Params` against CloudDriver.Schema lives at the service layer
// (Cloud.CRUD.b), not here.
package profile

import (
	"regexp"
	"time"
)

// IDPattern is the canonical Profile id form: kebab-case, length 1..63. Same
// as CHECK profiles_id_format in migration 118 (profiles_name_format before it).
//
// The form is UNCHANGED by the `name` -> `id` rename ([ADR-0085], NIM-729): the
// identifier moved spelling, not grammar.
const IDPattern = `^[a-z0-9-]{1,63}$`

var idRe = regexp.MustCompile(IDPattern)

// ValidID checks that id matches the canonical form (kebab 1..63).
func ValidID(id string) bool { return idRe.MatchString(id) }

// Profile is the runtime representation of a `profiles` registry row.
//
// Params is `map[string]any` for freeform VM spec; concrete CloudDriver typing
// lives in its schema, not in this layer. CloudInit nil means NULL column (userdata
// absent).
type Profile struct {
	// ID is the immutable identifier ([ADR-0085]): set once at creation, the
	// PRIMARY KEY of `profiles`. There is no rename operation.
	ID string `json:"id"`
	// Label is the display caption ([ADR-0085]): free text, mutable via
	// SetLabel, not unique, optional. nil means the column is NULL and a
	// consumer shows ID instead. It participates in nothing derived.
	//
	// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
	Label        *string        `json:"label,omitempty"`
	Provider     string         `json:"provider"`
	Params       map[string]any `json:"params"`
	CloudInit    *string        `json:"cloud_init,omitempty"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}
