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

// NamePattern is the canonical Profile name form: kebab-case, length 1..63. Same
// as CHECK profiles_name_format in migration 020.
const NamePattern = `^[a-z0-9-]{1,63}$`

var nameRe = regexp.MustCompile(NamePattern)

// ValidName checks that name matches the canonical form (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Profile is the runtime representation of a `profiles` registry row.
//
// Params is `map[string]any` for freeform VM spec; concrete CloudDriver typing
// lives in its schema, not in this layer. CloudInit nil means NULL column (userdata
// absent).
type Profile struct {
	Name         string         `json:"name"`
	Provider     string         `json:"provider"`
	Params       map[string]any `json:"params"`
	CloudInit    *string        `json:"cloud_init,omitempty"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}
