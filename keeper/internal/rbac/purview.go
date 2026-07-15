package rbac

import "sort"

// Purview is the typed result of [Enforcer.ResolvePurview]: the upper bound
// of an operator's scope-visibility/targeting for a given (resource, action),
// broken down by dimension (ADR-047).
//
// Covens plus the terminal flags Unrestricted/Deny have been populated since
// S0 (a generalization of the old [Enforcer.CovenScope]); the Regexes (S2a) /
// SoulprintExprs (S2b) / StateExprs (S2c) dimensions were each added as an
// additive slice without changing the signature or its consumers — each one
// simply started populating its own field.
//
// Terminal-flag semantics:
//   - Unrestricted=true — the operator has no scope restrictions for this
//     action (bare permission, `*` permission, or `coven=*`). The dimension
//     fields are empty and ignored in this case.
//   - Deny — NOT used yet in S1 either (always false). S1's default-deny is
//     expressed through the ABSENCE of Unrestricted (a role with
//     default_scope → its bare permissions are restricted to that scope,
//     not unrestricted). An "introduced but empty dimension" is unreachable
//     in the coven MVP (parseSelector requires a non-empty value list; a
//     NULL default_scope means the dimension was NOT introduced =
//     unrestricted). This field remains a placeholder until S2, where
//     dimensions with a possibly empty value will appear.
//
// Multiple values within one dimension are a union (OR within the dimension).
type Purview struct {
	// Covens — exact coven labels the right applies to (deduped, sorted).
	// Populated since S0.
	Covens []string

	// Unrestricted — the operator has no scope restrictions for (resource, action).
	Unrestricted bool

	// Deny — access denied by scope. Placeholder for S1 (default-deny);
	// always false in S0.
	Deny bool

	// Regexes — RE2 patterns over SID (ADR-047 S2a; deduped, sorted). Union
	// across roles. The real matching against SID happens in
	// [Permission.Matches]; here Purview just carries the patterns (as
	// Covens carries coven labels).
	Regexes []string

	// SoulprintExprs — CEL predicates over `soulprint.self.*` (ADR-047 S2b,
	// ADR-018 canonical form; deduped, sorted). Union across roles. The real
	// CEL eval against host facts happens in slices S3/S4
	// ([EvalSoulprintExpr]); here Purview just carries the predicates (as
	// Covens carries coven labels).
	SoulprintExprs []string

	// StateExprs — CEL predicates over `incarnation.state` (ADR-047 S2c;
	// deduped, sorted). Union across roles. The real CEL eval against
	// incarnation state happens in slice S3b ([EvalStateExpr] via
	// keeper/internal/statepredicate); here Purview just carries the
	// predicates (as Covens carries coven labels).
	StateExprs []string

	// TraitExprs — `key:value` exact-match pairs over `incarnation.traits`
	// (ADR-047 amendment, ADR-060 §7 slice 1; deduped, sorted). Union across
	// roles (an OR dimension, like Covens). The real match against an
	// incarnation's traits happens in the incarnation-list/get resolver
	// (slice 1 §7: `inc.Traits[key]==value`); here Purview just carries the
	// pairs (as Covens carries coven labels). Unlike Soulprint/State, this is
	// an exact equality, not a CEL predicate.
	TraitExprs []string
}

// ResolvePurview resolves an operator's [Purview] for (resource, action) —
// the allowed scope, broken down by dimension (ADR-047).
//
// S1 (role default_scope + inheritance/override + default-deny for
// introduced dimensions). For each matching (resource, action) permission,
// the resulting scope = the effective selector:
//   - a `*` permission → ALWAYS [Purview.Unrestricted] (cluster-admin isn't
//     locked by default-deny — ADR-047(b), exception #1).
//   - a per-perm selector (`on coven=X`) → FULLY OVERRIDES the role's
//     default_scope (no merge) — effective selector = per-perm.
//   - a bare permission (Selector==nil):
//     · role WITHOUT default_scope → [Purview.Unrestricted] (backcompat,
//     exception #2: NULL scope = dimension NOT introduced);
//     · role WITH default_scope → INHERITS it (not unrestricted, restricted).
//
// The effective selector is broken down by dimension the same way as S0: a
// `coven` key contributes covens; `coven=*` → Unrestricted; a selector
// without `coven` (host/incarnation/service) contributes nothing to covens
// (symmetric with Matches).
//
// Union across roles: if ANY matching permission yields Unrestricted → the
// result is Unrestricted; otherwise covens is the union of the concrete
// coven values (deduped, sorted).
//
// Deny is NOT set in S1 (coven MVP): NULL default_scope = unrestricted, and
// an "introduced but empty coven dimension" is unreachable in the grammar
// (parseSelector requires a non-empty value list). The [Purview.Deny] field
// remains a placeholder until S2.
func (e *Enforcer) ResolvePurview(aid, resource, action string) Purview {
	// Revoked shortcut (ADR-047 G1, mirrors the revoked branch in
	// [Enforcer.Check]): a revoked Archon gets Deny regardless of roles —
	// BEFORE any dimension collection, otherwise a bare `*` role would
	// return Unrestricted. This is the single point of revoked-aware
	// resolution for all read-souls consumers: gate (HoldsAction→Deny→
	// false→403), single-read (soulpurview.Resolve→Empty→404), InScope
	// (Deny→false). On read, revoked = "no access" (403/404), NOT the 401
	// parity of Check: souls visibility shouldn't distinguish revoked from
	// no-permission.
	if _, ok := e.revoked[aid]; ok {
		return Purview{Deny: true}
	}
	roles, ok := e.rolesByAID[aid]
	if !ok {
		return Purview{}
	}
	seen := make(map[string]struct{})
	seenRegex := make(map[string]struct{})
	seenSoulprint := make(map[string]struct{})
	seenState := make(map[string]struct{})
	seenTrait := make(map[string]struct{})
	for _, role := range roles {
		for _, p := range role.Permissions {
			if p.IsWildcard {
				return Purview{Unrestricted: true}
			}
			if p.Resource != resource {
				continue
			}
			if p.Action != "*" && p.Action != action {
				continue
			}

			// Effective selector: per-perm fully overrides default_scope;
			// bare inherits the role's default_scope (nil → unrestricted).
			eff := p.Selector
			if eff == nil {
				eff = role.DefaultScope
			}
			if eff == nil {
				// A bare permission with no role default_scope → unrestricted
				// (backcompat, ADR-047(b) exception #2).
				return Purview{Unrestricted: true}
			}

			// regex dimension (ADR-047 S2a): selector patterns are a union
			// across roles. The real matching against SID happens in S3/S4;
			// here Purview just carries the patterns (as Covens carries
			// coven labels).
			for _, pat := range eff["regex"] {
				seenRegex[pat] = struct{}{}
			}

			// soulprint dimension (ADR-047 S2b): selector CEL predicates are
			// a union across roles (like regex). The real CEL eval against
			// host facts happens in S3/S4 (the list/target resolver feeds
			// SoulprintFacts); here Purview carries the predicates for
			// S3/S4 and the least-privilege subset.
			for _, expr := range eff["soulprint"] {
				seenSoulprint[expr] = struct{}{}
			}

			// state dimension (ADR-047 S2c): CEL predicates over
			// incarnation.state are a union across roles (like soulprint).
			// The real CEL eval against incarnation state happens in S3b
			// (the list/target resolver feeds incarnation.state); here
			// Purview carries the predicates for S3b and the least-privilege
			// subset.
			for _, expr := range eff["state"] {
				seenState[expr] = struct{}{}
			}

			// trait dimension (ADR-047 amendment, ADR-060 §7 slice 1):
			// `key:value` exact-match pairs over incarnation.traits — union
			// across roles (like coven). The real match against an
			// incarnation's traits happens in the incarnation-list/get
			// resolver (slice 1 §7); here Purview carries the pairs for the
			// resolver and the subset.
			for _, pair := range eff["trait"] {
				seenTrait[pair] = struct{}{}
			}

			vals, hasCoven := eff["coven"]
			if !hasCoven {
				// An effective selector without a coven key
				// (host/incarnation/service/regex/soulprint) restricts a
				// different dimension: not unrestricted by coven, but no
				// contribution to covens either (symmetric with Matches).
				continue
			}
			for _, v := range vals {
				// `coven=*` is a wildcard value that lifts the scope like a
				// bare permission. Defensive/unreachable: parseSelector
				// doesn't allow `*` as a value (rejected at snapshot load);
				// the branch is kept for symmetry with the wildcard
				// convention.
				if v == "*" {
					return Purview{Unrestricted: true}
				}
				seen[v] = struct{}{}
			}
		}
	}
	if len(seen) == 0 && len(seenRegex) == 0 && len(seenSoulprint) == 0 &&
		len(seenState) == 0 && len(seenTrait) == 0 {
		return Purview{}
	}
	return Purview{
		Covens:         sortedKeys(seen),
		Regexes:        sortedKeys(seenRegex),
		SoulprintExprs: sortedKeys(seenSoulprint),
		StateExprs:     sortedKeys(seenState),
		TraitExprs:     sortedKeys(seenTrait),
	}
}

// sortedKeys returns a sorted slice of a set's keys (nil for an empty set, to
// preserve Purview's "dimension not introduced" = nil semantics).
func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
