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

// CovenPattern — shape of a Rite subject's Coven label. Same as the CHECK
// rites_coven_format in migration 033.
const CovenPattern = `^[a-z0-9][a-z0-9-]*$`

var (
	nameRe  = regexp.MustCompile(NamePattern)
	covenRe = regexp.MustCompile(CovenPattern)
)

// ValidName checks an Omen name against the canonical shape (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCoven checks a Rite subject's Coven label.
func ValidCoven(coven string) bool { return covenRe.MatchString(coven) }

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
	Name         string     `json:"name"`
	SourceType   SourceType `json:"source_type"`
	Endpoint     string     `json:"endpoint"`
	AuthRef      string     `json:"auth_ref"`
	CreatedByAID *string    `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Rite — runtime representation of a `rites` registry row (access grant).
//
// Subject is strictly XOR: exactly one of Coven / SID is non-empty. allow is
// raw JSONB (shape depends on the Omen's SourceType, validated via
// ValidateAllow). TokenTTL / TokenNumUses are only meaningful for a
// vault-Omen with Delegate=true.
type Rite struct {
	ID           int64           `json:"id"`
	Omen         string          `json:"omen"`
	Coven        *string         `json:"coven,omitempty"`
	SID          *string         `json:"sid,omitempty"`
	Allow        json.RawMessage `json:"allow"`
	Delegate     bool            `json:"delegate"`
	TokenTTL     *string         `json:"token_ttl,omitempty"`
	TokenNumUses *int            `json:"token_num_uses,omitempty"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
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

// ValidateSubjectXOR checks the Rite subject XOR invariant: exactly one of
// Coven / SID is non-empty, and a given Coven matches the shape. SID format
// isn't enforced at this layer (SID's FQDN semantics belong to the registry
// side).
func ValidateSubjectXOR(r *Rite) error {
	hasCoven := r.Coven != nil && *r.Coven != ""
	hasSID := r.SID != nil && *r.SID != ""
	if hasCoven == hasSID {
		return fmt.Errorf("augur: rite subject must be exactly one of coven / sid (XOR)")
	}
	if hasCoven && !ValidCoven(*r.Coven) {
		return fmt.Errorf("augur: invalid coven %q (must match %s)", *r.Coven, CovenPattern)
	}
	return nil
}
