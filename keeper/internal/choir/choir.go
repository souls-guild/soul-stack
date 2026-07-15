// Package choir — Choir/Voice registry in Postgres (tables `incarnation_choirs`
// + `incarnation_choir_voices`, 060_create_choirs.up.sql) per ADR-044.
//
// Choir — a named host topology WITHIN an incarnation (a declared "choir
// part"); Voice — membership of a specific SID in a specific Choir. Three
// DISTINCT layers (ADR-044 item 1): membership = `souls.coven[]`, coven =
// stable tags (ADR-008), Choir = host position within an incarnation. Choir
// does not collapse into either membership or coven.
//
// Source of truth for the declared topology is these tables, NOT
// `incarnation.state` (state is committed only under a cross-host barrier,
// ADR-044 item 4). `voice.role` subsumes the declared role
// `incarnation.spec.hosts[].role` (ADR-044 item 2).
//
// Membership invariant (ADR-044 item 3): a Voice is created only for a SID
// that is ALREADY a member of this incarnation — its `souls.coven[]` contains
// `incarnation.name`. One SID can legally be a Voice in Choirs of DIFFERENT
// incarnations (the PK includes incarnation_name; there is no global
// UNIQUE(sid)).
//
// Package scope (S-T2): transactional Choir/Voice CRUD following the
// `incarnation/hosts.go` pattern (SELECT FOR UPDATE → mutate → commit;
// membership validation). API endpoints / RBAC / audit — S-T3; the
// `choirs[]` resolver in soulprint — S-T4.
package choir

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// choirNamePattern — Choir name format, matches the CHECK
// `incarnation_choirs_name_format` in migration 060_create_choirs.up.sql
// (kebab/snake, starts with a letter).
var choirNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidChoirName reports whether name matches [choirNamePattern].
func ValidChoirName(name string) bool {
	return choirNamePattern.MatchString(name)
}

// Choir — runtime representation of an `incarnation_choirs` row. MinSize/MaxSize
// are optional part-size limits (declared under the typed-input S-T1; at S-T2
// they are merely stored — Voice cardinality isn't softly enforced at the CRUD
// level, min/max validation happens in the S-T1 typed-input layer).
type Choir struct {
	IncarnationName string
	ChoirName       string
	Description     *string
	MinSize         *int
	MaxSize         *int
	CreatedAt       time.Time
	CreatedByAID    *string
}

// Voice — runtime representation of an `incarnation_choir_voices` row (a SID's
// membership in a Choir). Role — the subsumed declared role (ADR-044 item 2,
// nullable); Position — ordinal index within the part (nullable, e.g. a seed
// node).
type Voice struct {
	IncarnationName string
	ChoirName       string
	SID             string
	Role            *string
	Position        *int
	AddedAt         time.Time
	AddedByAID      *string
}

// Sentinel errors of the CRUD layer.
//   - ErrIncarnationNotFound — no incarnation row by that name (404).
//   - ErrChoirNotFound       — no Choir for the (incarnation_name, choir_name) pair.
//   - ErrChoirExists         — a Choir with that name already exists in the incarnation (409).
//   - ErrVoiceNotFound       — no Voice for the PK triple (for RemoveVoice).
//   - ErrVoiceExists         — a Voice for this SID already exists in this Choir (409).
//   - ErrInvalidChoirName    — choir_name doesn't match [choirNamePattern].
//   - ErrInvalidSizeBounds   — min_size > max_size (or ≤ 0) at creation.
var (
	ErrIncarnationNotFound = errors.New("choir: incarnation not found")
	ErrChoirNotFound       = errors.New("choir: choir not found")
	ErrChoirExists         = errors.New("choir: choir already exists in incarnation")
	ErrVoiceNotFound       = errors.New("choir: voice not found")
	ErrVoiceExists         = errors.New("choir: voice already exists in choir")
	ErrInvalidChoirName    = errors.New("choir: invalid choir name")
	ErrInvalidSizeBounds   = errors.New("choir: invalid min/max size bounds")
)

// ErrNotMembers — the given SIDs are NOT members of the incarnation (their
// `souls.coven[]` doesn't contain `incarnation.name`). ADR-044 item 3
// invariant. The handler side (S-T3) maps this to 422; offending SIDs go into
// .Missing.
//
// Missing includes both SIDs absent from the `souls` registry entirely and
// SIDs that exist but aren't members of this incarnation — for the operator
// the distinction doesn't matter (a Voice can't be created either way), and
// splitting them into separate classes would complicate the contract for no
// benefit.
type ErrNotMembers struct {
	Incarnation string
	Missing     []string
}

func (e *ErrNotMembers) Error() string {
	return fmt.Sprintf("choir: %d SID(s) not members of incarnation %q: %v",
		len(e.Missing), e.Incarnation, e.Missing)
}
