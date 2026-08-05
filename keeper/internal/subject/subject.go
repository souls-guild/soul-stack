// Package subject resolves a rule's subject — the selector answering "which
// Souls does this rule bind to?".
//
// Three registries carry a subject: Vigil (which hosts run a beacon, ADR-030),
// Decree (which hosts may trigger a reaction, ADR-030(b)) and Rite (which hosts
// an Augur grant covers, ADR-025). All three used to spell it `coven XOR sid`,
// which left no way to say "the hosts of this incarnation": NIM-124 moved
// membership into its own relation and NIM-281 removed label inheritance, so
// neither an incarnation's name nor its labels reach its members any more.
//
// A subject is now EXACTLY ONE of four dimensions, named after the RBAC scope
// grammar (coven | service | incarnation | host | trait.<key>, see
// rbac.ParseScope) so an operator learns one vocabulary:
//
//	sid=<sid>[,<sid>…]              named hosts, by identity
//	incarnation=<service>.<name>    every host on that incarnation's roster
//	coven=<label>[,<label>…]        hosts carrying any of those labels
//	trait.<key>=<value>             hosts carrying that pair
//
// Deliberately WITHOUT the RBAC scope's boolean algebra: a subject is a flat
// one-of-four so it stays a set of indexed columns, and so "which rule reached
// this host" has a one-line answer in an audit record.
//
// # Two levels, resolved at match time
//
// coven and trait read BOTH levels (NIM-280):
//
//   - the label on the host — `souls.coven[]` / `souls.traits`;
//   - the same label on an incarnation the host belongs to —
//     `incarnation.covens` / `incarnation.traits`, reaching every member.
//
// This is not the label inheritance NIM-281 removed. Inheritance made the HOST
// CARRY the incarnation's label, so every consumer saw it — RBAC scopes,
// `soulprint.self.covens` in CEL, the push provider choice — including
// consumers that never asked about incarnations. Here nothing is written to
// `souls`: the host still carries only what an operator attached to it, and the
// union exists for the duration of one selector match.
//
// # The boundary
//
// ★ Targeting expands; operator authorization never does. An RBAC role scope
// (`soul.list on coven=prod`) keeps reading the row's OWN column and nothing
// else (rbac.CovenScopeSQL) — otherwise tagging an incarnation `prod` would
// hand out permanent visibility of every host on its roster, which is exactly
// the escalation NIM-281 closed. A subject decides which hosts a rule REACHES;
// a scope decides what an Archon MAY DO. Never wire one to the other.
//
// ⚠ The label namespace is shared, so tagging an incarnation `prod` widens
// every existing `coven=prod` subject to its members without touching a single
// rule. That is the declared behaviour, not a bug — but it is why a subject and
// a scope must not share a resolver.
package subject

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Dimension names the selector's populated dimension. Reported in audit
// records, debug logs and metrics so a skip is attributable to a dimension
// rather than to "subject mismatch" alone.
type Dimension string

const (
	DimNone        Dimension = ""            // nothing set — never matches (default-deny)
	DimSID         Dimension = "sid"         //
	DimIncarnation Dimension = "incarnation" //
	DimCoven       Dimension = "coven"       //
	DimTrait       Dimension = "trait"       //
)

// Patterns duplicating the CHECKs of migration 113 — reject a bad value before
// the round-trip (better diagnostics, no wasted call). CovenPattern is the
// shared label shape (ADR-008); ServicePattern / IncarnationPattern mirror the
// `incarnation` registry (migration 005); TraitKeyPattern mirrors the RBAC
// scope's trait key (rbac.reScopeTraitKey) — one grammar, one shape. The SID
// form is not restated here: it is [soul.SIDPattern], the one every other
// surface already validates against.
const (
	CovenPattern       = `^[a-z0-9][a-z0-9-]*$`
	ServicePattern     = `^[a-z0-9][a-z0-9-]{0,62}$`
	IncarnationPattern = `^[a-z0-9][a-z0-9-]{0,62}$`
	TraitKeyPattern    = `^[a-z][a-z0-9_.-]*$`
)

var (
	covenRe       = regexp.MustCompile(CovenPattern)
	serviceRe     = regexp.MustCompile(ServicePattern)
	incarnationRe = regexp.MustCompile(IncarnationPattern)
	traitKeyRe    = regexp.MustCompile(TraitKeyPattern)
)

// ValidCoven / ValidService / ValidIncarnation / ValidTraitKey — form checks
// for one selector element.
func ValidCoven(s string) bool       { return covenRe.MatchString(s) }
func ValidService(s string) bool     { return serviceRe.MatchString(s) }
func ValidIncarnation(s string) bool { return incarnationRe.MatchString(s) }
func ValidTraitKey(s string) bool    { return traitKeyRe.MatchString(s) }

// Selector — a rule's subject. Exactly one dimension is populated; [Validate]
// enforces it and so does the per-table CHECK (defence in depth).
//
// Service and Incarnation are one dimension and travel together. The address of
// an incarnation is the PAIR, never the bare name: incarnation names are unique
// today (migration 005, PK = name), and spelling the address as
// `<service>.<name>` everywhere is what keeps dropping that uniqueness a
// migration rather than a re-spelling of every stored rule.
//
// SIDs and Covens are lists because one rule may name several; a match is
// "any of". They are lists in the same way and for the same reason, so a
// scalar SID here would be the one place where the stored column (text[]) and
// the matcher disagree about how many hosts a rule can name.
type Selector struct {
	SIDs        []string
	Service     string
	Incarnation string
	Covens      []string
	TraitKey    string
	TraitValue  string
}

// Dimension reports which dimension is populated, or [DimNone] when the
// selector is empty. An empty selector never matches — see [Selector.Matches].
func (s Selector) Dimension() Dimension {
	switch {
	case len(s.SIDs) > 0:
		return DimSID
	case s.Incarnation != "" || s.Service != "":
		return DimIncarnation
	case len(s.Covens) > 0:
		return DimCoven
	// Either half claims the dimension, symmetrically with the incarnation arm
	// above and with [Validate]'s count. Reporting DimNone for a half-written pair
	// would let it slip past Validate's per-dimension switch as "no dimension at
	// all" and be stored as a rule that can never match.
	case s.TraitKey != "" || s.TraitValue != "":
		return DimTrait
	default:
		return DimNone
	}
}

// String renders the selector in the grammar an operator writes, for logs,
// audit payloads and 422 diagnostics. Never carries a secret (every component
// is an identifier or an operator-set label).
func (s Selector) String() string {
	switch s.Dimension() {
	case DimSID:
		return "sid=" + strings.Join(s.SIDs, ",")
	case DimIncarnation:
		return "incarnation=" + s.Service + "." + s.Incarnation
	case DimCoven:
		return "coven=" + strings.Join(s.Covens, ",")
	case DimTrait:
		return "trait." + s.TraitKey + "=" + s.TraitValue
	default:
		return "<empty>"
	}
}

// Incarnation — one incarnation a host belongs to, carrying the labels an
// operator attached to the INCARNATION (not to the host).
type Incarnation struct {
	Service string
	Name    string
	Covens  []string
	Traits  map[string]any
}

// Host — the authoritative facts a match is evaluated against. Every field
// comes from the registry, never from the payload of the event being handled
// (a Portent and an AugurRequest are untrusted input, ADR-030(b), augur.md §6.2).
//
// Covens / Traits are the host's OWN labels (`souls.coven[]`, `souls.traits`).
// Member is its roster membership with each incarnation's own labels — read
// from `incarnation_membership`, the relation, never inferred from a label.
type Host struct {
	SID    string
	Covens []string
	Traits map[string]any
	Member []Incarnation
}

// Matches reports whether the selector reaches this host.
//
// Fail-safe: an empty selector matches nothing. The per-table CHECK guarantees
// exactly one dimension is stored, so reaching the default arm means the row
// was written around the constraint — deny rather than let a rule with no
// subject bind to every host.
func (s Selector) Matches(h Host) bool {
	switch s.Dimension() {
	case DimSID:
		return slices.Contains(s.SIDs, h.SID)
	case DimIncarnation:
		for _, inc := range h.Member {
			if inc.Service == s.Service && inc.Name == s.Incarnation {
				return true
			}
		}
		return false
	case DimCoven:
		want := make(map[string]struct{}, len(s.Covens))
		for _, c := range s.Covens {
			want[c] = struct{}{}
		}
		if anyIn(want, h.Covens) {
			return true
		}
		for _, inc := range h.Member {
			if anyIn(want, inc.Covens) {
				return true
			}
		}
		return false
	case DimTrait:
		if traitHolds(h.Traits, s.TraitKey, s.TraitValue) {
			return true
		}
		for _, inc := range h.Member {
			if traitHolds(inc.Traits, s.TraitKey, s.TraitValue) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func anyIn(want map[string]struct{}, have []string) bool {
	for _, c := range have {
		if _, ok := want[c]; ok {
			return true
		}
	}
	return false
}

// traitHolds reports whether traits carries key=value, matching the RBAC trait
// dimension exactly (rbac.scopeSQLBuilder.cond, dimTrait): a scalar compares by
// its text rendering, a list matches on membership. Keeping the two in step
// matters — an operator who writes `trait.owner=dba` in a role scope and in a
// subject must not get two different answers about one host.
func traitHolds(traits map[string]any, key, value string) bool {
	v, ok := traits[key]
	if !ok {
		return false
	}
	if list, ok := v.([]any); ok {
		for _, item := range list {
			if scalarText(item) == value {
				return true
			}
		}
		return false
	}
	return scalarText(v) == value
}

// scalarText renders a JSON scalar the way Postgres `->>` does, so the Go
// matcher and the SQL prefilter agree on one host. json.Unmarshal decodes every
// JSON number into float64, so an integral value must not come back as "3e+00".
func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// Validate enforces the exactly-one-of-four invariant and the form of every
// populated element. It is the service-layer half of the per-table CHECK: the
// CHECK is the last barrier, this is the one that produces a 422 an operator
// can read.
//
// errPrefix names the registry in the diagnostic ("oracle: decree", "augur:
// rite") so a caller does not have to re-wrap.
func Validate(s Selector, errPrefix string) error {
	set := 0
	if len(s.SIDs) > 0 {
		set++
	}
	if s.Service != "" || s.Incarnation != "" {
		set++
	}
	if len(s.Covens) > 0 {
		set++
	}
	if s.TraitKey != "" || s.TraitValue != "" {
		set++
	}
	if set != 1 {
		return fmt.Errorf("%s subject must be exactly one of sid / incarnation (service+name) / coven / trait.<key>, got %d dimensions", errPrefix, set)
	}

	switch s.Dimension() {
	case DimSID:
		for _, sid := range s.SIDs {
			if !soul.ValidSID(sid) {
				return fmt.Errorf("%s invalid subject sid %q (must match %s)", errPrefix, sid, soul.SIDPattern)
			}
		}
	case DimIncarnation:
		// Both halves or neither: `incarnation=<service>.<name>` is one address.
		// A half-written pair is the shape that would silently never match.
		if s.Service == "" || s.Incarnation == "" {
			return fmt.Errorf("%s subject incarnation must carry both service and name (address is <service>.<name>)", errPrefix)
		}
		if !ValidService(s.Service) {
			return fmt.Errorf("%s invalid subject service %q (must match %s)", errPrefix, s.Service, ServicePattern)
		}
		if !ValidIncarnation(s.Incarnation) {
			return fmt.Errorf("%s invalid subject incarnation %q (must match %s)", errPrefix, s.Incarnation, IncarnationPattern)
		}
	case DimCoven:
		for _, c := range s.Covens {
			if !ValidCoven(c) {
				return fmt.Errorf("%s invalid subject coven %q (must match %s)", errPrefix, c, CovenPattern)
			}
		}
	case DimTrait:
		if s.TraitKey == "" || s.TraitValue == "" {
			return fmt.Errorf("%s subject trait must carry both key and value (trait.<key>=<value>)", errPrefix)
		}
		if !ValidTraitKey(s.TraitKey) {
			return fmt.Errorf("%s invalid subject trait key %q (must match %s)", errPrefix, s.TraitKey, TraitKeyPattern)
		}
	}
	return nil
}
