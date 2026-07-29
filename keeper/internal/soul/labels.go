package soul

import (
	"context"
	"encoding/json"
	"fmt"
)

// Label inheritance (ADR-080). A Coven tag or a Trait pair lives only where the
// operator attached it — on the host (`souls.coven` / `souls.traits`) or on the
// incarnation (`incarnation.covens` / `incarnation.traits`) — and is never copied
// between them. What consumers see is the UNION of the two, computed here at read
// time: a host's effective labels are its own plus those of every incarnation it
// belongs to (`incarnation_membership`, M:N since NIM-124).
//
// A key held on both sides does not contest: `owner=dba` on the incarnation and
// `owner=bobik` on the host yield `owner=[dba, bobik]`, and either value grants
// access. Precedence would make one deliberately-attached label invisible.

// InheritedLabels are the labels a host receives from the incarnations it belongs
// to. Covens carries each incarnation's `covens[]` PLUS its name — the
// incarnation-side scope resolver already treats the name as a coven tag
// (`covens && $scope OR name = ANY($scope)`), so host visibility must agree. This
// does not revert NIM-124: the name is never written into `souls.coven`.
type InheritedLabels struct {
	Covens []string
	Traits map[string]any
}

// InheritedLabelsQueryMarker tags the standalone [LoadInheritedLabels] statement
// so it is identifiable on sight — in a query log, and in the pgx stubs that
// dispatch on SQL text.
const InheritedLabelsQueryMarker = "inherited-labels"

// InheritedLabelsSelectSQL renders the two SELECT-list expressions that carry a
// host's inherited labels, correlated on sidCol (which MUST be table-qualified —
// the subqueries alias `incarnation_membership m`, so a bare `sid` would bind
// there and correlate every host to itself).
//
// Covens arrive pre-flattened (each incarnation's tags plus its name, distinct);
// traits arrive as a jsonb ARRAY of per-incarnation maps, because a union that
// folds scalars and lists into one set is far clearer in Go ([UnionTraits]) than
// in SQL. Feed both to [ParseInheritedLabels]. Rides
// `incarnation_membership_sid_idx`.
//
// Shared by every reader of effective labels (souls reads, topology roster and
// push inventory) so they cannot drift into disagreeing about one host.
func InheritedLabelsSelectSQL(sidCol string) string {
	return fmt.Sprintf(`COALESCE((
        SELECT array_agg(DISTINCT u.c)
        FROM incarnation_membership m
        JOIN incarnation i ON i.name = m.incarnation_name
        CROSS JOIN LATERAL unnest(i.covens || ARRAY[i.name]) AS u(c)
        WHERE m.sid = %s
    ), ARRAY[]::text[]),
    COALESCE((
        SELECT jsonb_agg(i.traits)
        FROM incarnation_membership m
        JOIN incarnation i ON i.name = m.incarnation_name
        WHERE m.sid = %s AND i.traits <> '{}'::jsonb
    ), '[]'::jsonb)`, sidCol, sidCol)
}

// ParseInheritedLabels folds the two columns produced by
// [InheritedLabelsSelectSQL] into one label set, unioning the per-incarnation
// trait maps of a host that belongs to several.
func ParseInheritedLabels(covens []string, traitsJSON []byte) (InheritedLabels, error) {
	out := InheritedLabels{Covens: covens}
	if len(traitsJSON) == 0 {
		return out, nil
	}
	var perIncarnation []map[string]any
	if err := json.Unmarshal(traitsJSON, &perIncarnation); err != nil {
		return InheritedLabels{}, fmt.Errorf("soul: unmarshal inherited traits: %w", err)
	}
	for _, m := range perIncarnation {
		out.Traits = UnionTraits(out.Traits, m)
	}
	return out, nil
}

// LoadInheritedLabels returns the labels host `sid` inherits from its
// incarnations. A host in no incarnation (or in incarnations carrying no labels)
// yields an empty result, not an error — inheritance is absence-tolerant.
func LoadInheritedLabels(ctx context.Context, db ExecQueryRower, sid string) (InheritedLabels, error) {
	var (
		covens     []string
		traitsJSON []byte
	)
	// The marker names this statement unambiguously: the same subqueries also
	// appear inside the scope predicate and the topology roster, so matching on
	// `incarnation_membership` alone would not tell those three apart.
	sql := "SELECT /* " + InheritedLabelsQueryMarker + " */ " + InheritedLabelsSelectSQL("$1")
	if err := db.QueryRow(ctx, sql, sid).Scan(&covens, &traitsJSON); err != nil {
		return InheritedLabels{}, fmt.Errorf("soul: load inherited labels for %q: %w", sid, err)
	}
	labels, err := ParseInheritedLabels(covens, traitsJSON)
	if err != nil {
		return InheritedLabels{}, fmt.Errorf("soul: inherited labels for %q: %w", sid, err)
	}
	return labels, nil
}

// EffectiveCovens returns the coven labels host `sid` effectively carries: its
// own `souls.coven[]` unioned with the ones it inherits from every incarnation it
// belongs to — each incarnation's `covens[]` plus its name (ADR-080).
//
// This is the one resolution every reader of the coven axis is meant to call.
// Until NIM-124 an incarnation's name was physically copied into `souls.coven[]`,
// so reading the bare column happened to answer the same question; NIM-124
// removed the copy and ADR-080 replaced it with this read-time union. A consumer
// left on the bare column keeps compiling and keeps returning rows — it just
// matches a strictly smaller set of hosts, which is why the same defect surfaced
// three times over (Oracle NIM-224, telemetry NIM-248, Augur NIM-249) before
// anyone noticed. Prefer this over `SelectBySID(...).Coven` whenever the covens
// are about to be matched against something an operator wrote.
//
// [ErrSoulNotFound] passes through unwrapped: what an unregistered host means is
// the caller's policy — "no config" for telemetry, an empty match set for the
// Oracle, an outright denial for Augur — and flattening it to an empty result
// here would quietly make that choice for them.
//
// ★ Effective covens answer "which rules may see this host". They do NOT answer
// "which incarnation does it belong to": the union deliberately admits a
// host-attached tag spelled exactly like an incarnation's name. Membership is
// `incarnation_membership` and nothing else — see [incarnation.IsMember] and the
// ADR-030 amendment of 2026-07-28.
func EffectiveCovens(ctx context.Context, db ExecQueryRower, sid string) ([]string, error) {
	s, err := SelectBySID(ctx, db, sid)
	if err != nil {
		return nil, err
	}
	inherited, err := LoadInheritedLabels(ctx, db, sid)
	if err != nil {
		return nil, err
	}
	return UnionCovens(s.Coven, inherited.Covens), nil
}

// UnionCovens merges two coven sets preserving first-seen order (own tags first,
// then inherited) and dropping duplicates. Order is preserved rather than sorted:
// the operator's tag order is theirs, and every consumer treats coven as a set.
func UnionCovens(own, inherited []string) []string {
	if len(inherited) == 0 {
		return own
	}
	if len(own) == 0 {
		return inherited
	}
	seen := make(map[string]struct{}, len(own)+len(inherited))
	out := make([]string, 0, len(own)+len(inherited))
	for _, list := range [][]string{own, inherited} {
		for _, c := range list {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// UnionTraits merges two trait maps. A key present on one side only is carried
// over as-is (a scalar stays a scalar — otherwise every existing
// `traits.namespace == 'dba-ns'` predicate would break). A key present on both
// becomes the union of their values as a list, own values first, duplicates
// dropped by their text form (the way PG's `->>` renders them, so the in-Go union
// and the SQL predicate agree).
//
// A union that collapses to a single value stays scalar: `owner=dba` on both
// sides is still `owner=dba`, not `owner=[dba]`.
func UnionTraits(own, inherited map[string]any) map[string]any {
	if len(inherited) == 0 {
		return own
	}
	if len(own) == 0 {
		return inherited
	}
	out := make(map[string]any, len(own)+len(inherited))
	for k, v := range own {
		out[k] = v
	}
	for k, iv := range inherited {
		ov, clash := out[k]
		if !clash {
			out[k] = iv
			continue
		}
		out[k] = unionTraitValues(ov, iv)
	}
	return out
}

// unionTraitValues folds two trait values (each scalar or list) into one, keeping
// first-seen order and deduplicating by text form. A single surviving value is
// returned as a scalar, not a one-element list.
func unionTraitValues(a, b any) any {
	seen := make(map[string]struct{}, 4)
	out := make([]any, 0, 4)
	for _, v := range []any{a, b} {
		for _, e := range traitValueElems(v) {
			key := traitScalarText(e)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, e)
		}
	}
	if len(out) == 1 {
		return out[0]
	}
	return out
}

// traitValueElems flattens a trait value into its elements (a scalar yields
// itself, a list its members).
func traitValueElems(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return []any{v}
}

// traitScalarText renders a scalar trait value as text the way PG's jsonb `->>`
// does — the dedup key, and the same normalization the scope predicate uses.
func traitScalarText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
