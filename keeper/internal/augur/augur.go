// Package augur — Postgres registry for Augur (ADR-025, docs/keeper/augur.md):
// two tables, omens (external systems) and rites (access grants).
//
// The layer holds types + CRUD (Insert / Select* / Delete) + service validation
// that DB CHECKs can't cover: vault-ref auth_ref format, allow shape by
// source_type, token fields only for vault-delegate, token_ttl format.
// AugurRequest authorization, token minting, and EventStream wiring are a
// separate slice (not here).
package augur

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Sentinel errors from Rite shape storage validators. The service layer maps
// them to 422 via errors.Is — not by string prefix (renaming the diagnostic
// text must not silently break the 422→500 mapping). These validators live in
// the storage slice and know nothing of management Service [ErrValidation];
// Service wraps their result itself.
var (
	// ErrAllowShape — allow JSONB doesn't match the source_type shape (ValidateAllow).
	ErrAllowShape = errors.New("augur: allow shape invalid")
	// ErrTokenFields — token fields invariant violated (ValidateTokenFields).
	ErrTokenFields = errors.New("augur: token fields invalid")
)

// SourceType — descriptive closed enum of the external system type (omens.source_type,
// augur.md §7). Extending it requires propose-and-wait + a PR to augur.md and naming-rules.md.
type SourceType string

const (
	SourceVault      SourceType = "vault"
	SourcePrometheus SourceType = "prometheus"
	SourceELK        SourceType = "elk"
)

// ValidSourceType — closed enum membership check. Duplicates the CHECK
// omens_source_type_enum (032), but rejects a bad value before the round trip.
func ValidSourceType(s SourceType) bool {
	switch s {
	case SourceVault, SourcePrometheus, SourceELK:
		return true
	default:
		return false
	}
}

// NamePattern — canonical Omen name shape: kebab-case, length 1..63. Same as
// the CHECK omens_name_format in migration 032 (like providers.NamePattern).
const NamePattern = `^[a-z0-9-]{1,63}$`

// CovenPattern — shape of a Coven label. Re-exported from [subject] so the
// three subject-bearing registries state one shape once.
const CovenPattern = subject.CovenPattern

var nameRe = regexp.MustCompile(NamePattern)

// ValidName checks an Omen name against the canonical shape (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCoven checks a single Coven label.
func ValidCoven(coven string) bool { return subject.ValidCoven(coven) }

// ValidAuthRef checks that auth_ref is a valid vault-ref
// (`vault:<mount>/<path>`), using the same parser as providers.credentials_ref
// and other `*_ref` fields in keeper.yml. The master credential is never
// stored in the DB — only the reference (invariant augur.md §4.1).
func ValidAuthRef(ref string) bool {
	_, err := vault.ParseRef(ref)
	return err == nil
}

// Omen — runtime representation of an omens registry row (external system).
type Omen struct {
	Name string `json:"name"`
	// Label is the display caption ([ADR-0085]): free text, mutable via
	// SetOmenLabel, not unique, optional. nil means the column is NULL and a
	// consumer shows Name instead. It participates in nothing derived — not the
	// Rite grant's `omen` FK, and not any Vault path.
	//
	// Not to be confused with [Rite.ID], the int64 surrogate that lives in this
	// same package: an entity id is a kebab code word, a surrogate is neither
	// ([ADR-0085] "`id` means code word, not opaque identifier").
	//
	// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
	Label        *string    `json:"label,omitempty"`
	SourceType   SourceType `json:"source_type"`
	Endpoint     string     `json:"endpoint"`
	AuthRef      string     `json:"auth_ref"`
	CreatedByAID *string    `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Rite — runtime representation of a `rites` registry row (access grant).
//
// The subject is exactly one of four dimensions (CHECK rites_subject_one_of,
// NIM-280) — see [subject.Selector] and [Rite.Subject]. allow is raw JSONB
// (shape depends on the Omen's SourceType, validated via ValidateAllow).
// TokenTTL / TokenNumUses are only meaningful for a vault-Omen with
// Delegate=true.
//
// A Rite is a GRANT, so widening its subject widens access to a real external
// system. That is why the resolution stays strictly one-directional: an
// incarnation's label reaches its members (an operator asked for that by
// tagging the incarnation), but nothing here ever reaches back the other way,
// and no Rite is consulted for what an ARCHON may do — see the boundary note on
// package [subject].
type Rite struct {
	ID           int64           `json:"id"`
	Omen         string          `json:"omen"`
	SID          []string        `json:"sid,omitempty"`
	Service      *string         `json:"service,omitempty"`
	Incarnation  *string         `json:"incarnation,omitempty"`
	Coven        []string        `json:"coven,omitempty"`
	TraitKey     *string         `json:"trait_key,omitempty"`
	TraitValue   *string         `json:"trait_value,omitempty"`
	Allow        json.RawMessage `json:"allow"`
	Delegate     bool            `json:"delegate"`
	TokenTTL     *string         `json:"token_ttl,omitempty"`
	TokenNumUses *int            `json:"token_num_uses,omitempty"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Subject renders the Rite's stored columns as the shared selector.
func (r *Rite) Subject() subject.Selector {
	return subject.Selector{
		SIDs:        r.SID,
		Service:     derefStr(r.Service),
		Incarnation: derefStr(r.Incarnation),
		Covens:      r.Coven,
		TraitKey:    derefStr(r.TraitKey),
		TraitValue:  derefStr(r.TraitValue),
	}
}

// SetSubject writes the selector back into the stored columns. Every unset
// dimension becomes NULL, which is what keeps the SQL predicate exact: a Rite
// grants only through the dimension it was written with.
func (r *Rite) SetSubject(s subject.Selector) {
	r.SID, r.Coven = s.SIDs, s.Covens
	r.Service, r.Incarnation = ptrStr(s.Service), ptrStr(s.Incarnation)
	r.TraitKey, r.TraitValue = ptrStr(s.TraitKey), ptrStr(s.TraitValue)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// allow shape by source_type (augur.md §4.2). Shapes are closed: unknown
// keys are rejected so a typo in allow can't silently produce an empty
// allow-list (security invariant — an overly broad grant from a typo).
//
// vault     → {paths?, policies?}  (at least one non-empty)
// prometheus→ {queries}            (non-empty)
// elk       → {indices}            (non-empty)

type allowVault struct {
	Paths    []string `json:"paths"`
	Policies []string `json:"policies"`
}

type allowPrometheus struct {
	Queries []string `json:"queries"`
}

type allowELK struct {
	Indices []string `json:"indices"`
}

// ValidateAllow checks the allow JSONB shape against the Omen's source_type.
// Returns nil when the shape is valid; otherwise a diagnostic naming the
// expected shape.
//
// Service layer note: a declarative CHECK can't tie a JSONB shape to another
// row's source_type without a trigger, so the check lives here (augur.md §4.2).
func ValidateAllow(src SourceType, allow json.RawMessage) error {
	if len(allow) == 0 {
		return fmt.Errorf("%w: allow is empty", ErrAllowShape)
	}
	switch src {
	case SourceVault:
		var a allowVault
		if err := strictUnmarshal(allow, &a); err != nil {
			return fmt.Errorf("%w: allow for vault must be {paths?, policies?}: %s", ErrAllowShape, err)
		}
		if len(a.Paths) == 0 && len(a.Policies) == 0 {
			return fmt.Errorf("%w: allow for vault must carry at least one of paths/policies", ErrAllowShape)
		}
		return nil
	case SourcePrometheus:
		var a allowPrometheus
		if err := strictUnmarshal(allow, &a); err != nil {
			return fmt.Errorf("%w: allow for prometheus must be {queries}: %s", ErrAllowShape, err)
		}
		if len(a.Queries) == 0 {
			return fmt.Errorf("%w: allow for prometheus must carry non-empty queries", ErrAllowShape)
		}
		return nil
	case SourceELK:
		var a allowELK
		if err := strictUnmarshal(allow, &a); err != nil {
			return fmt.Errorf("%w: allow for elk must be {indices}: %s", ErrAllowShape, err)
		}
		if len(a.Indices) == 0 {
			return fmt.Errorf("%w: allow for elk must carry non-empty indices", ErrAllowShape)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown source_type %q", ErrAllowShape, src)
	}
}

// strictUnmarshal rejects unknown keys (DisallowUnknownFields) — a typo in
// allow must not silently grant more than intended.
func strictUnmarshal(data json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ValidateTokenFields implements the other half of the token fields invariant
// that the DB CHECK can't catch: token_ttl / token_num_uses are allowed ONLY
// when Delegate=true AND SourceType=vault (CHECK rites_token_fields_vault_only
// only catches ⇒delegate; ⇒vault would need a join to omens — augur.md §4.2).
// Also validates the token_ttl format via config.ParseDuration.
func ValidateTokenFields(src SourceType, r *Rite) error {
	hasTTL := r.TokenTTL != nil
	hasUses := r.TokenNumUses != nil
	if !hasTTL && !hasUses {
		return nil
	}
	if !r.Delegate {
		return fmt.Errorf("%w: token_ttl/token_num_uses require delegate=true", ErrTokenFields)
	}
	if src != SourceVault {
		return fmt.Errorf("%w: token_ttl/token_num_uses are vault-only, got source_type %q", ErrTokenFields, src)
	}
	if hasTTL {
		if _, err := config.ParseDuration(*r.TokenTTL); err != nil {
			return fmt.Errorf("%w: invalid token_ttl %q: %s", ErrTokenFields, *r.TokenTTL, err)
		}
	}
	if hasUses && *r.TokenNumUses < 0 {
		return fmt.Errorf("%w: token_num_uses must be >= 0, got %d", ErrTokenFields, *r.TokenNumUses)
	}
	return nil
}

// ValidateSubject checks the Rite subject's exactly-one-of-four invariant and
// the form of every populated element, through the validator all three
// subject-bearing registries share. Symmetric with the CHECK
// rites_subject_one_of (defence in depth).
func ValidateSubject(r *Rite) error {
	return subject.Validate(r.Subject(), "augur: rite")
}
