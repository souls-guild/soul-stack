package subject

import (
	"fmt"
	"sort"
	"strings"
)

// SQLColumns names one registry table's subject columns. The three registries
// spell them differently (`decrees` carries a `subject_` prefix, `vigils` and
// `rites` do not), but the SHAPE is identical — that is what lets all three
// share one predicate.
type SQLColumns struct {
	SIDs        string // text[]
	Service     string // text
	Incarnation string // text
	Covens      string // text[]
	TraitKey    string // text
	TraitValue  string // text
}

// Columns of each registry — the single place a column rename has to be made.
var (
	VigilColumns = SQLColumns{
		SIDs: "sid", Service: "service", Incarnation: "incarnation",
		Covens: "coven", TraitKey: "trait_key", TraitValue: "trait_value",
	}
	DecreeColumns = SQLColumns{
		SIDs: "subject_sid", Service: "subject_service", Incarnation: "subject_incarnation",
		Covens: "subject_coven", TraitKey: "subject_trait_key", TraitValue: "subject_trait_value",
	}
	RiteColumns = SQLColumns{
		SIDs: "sid", Service: "service", Incarnation: "incarnation",
		Covens: "coven", TraitKey: "trait_key", TraitValue: "trait_value",
	}
)

// HostArgs — a [Host] flattened into the four value sets a subject predicate
// compares against. Both levels are already folded in: Covens and TraitPairs
// carry the host's own labels UNION those of every incarnation it belongs to.
//
// Flattening in Go rather than joining in SQL is deliberate. The predicate then
// consumes the very same resolved [Host] that [Selector.Matches] does, so the
// SQL prefilter and the Go matcher cannot answer differently about one host —
// the failure mode where a hand-written query drifts from the rule it is
// supposed to implement, and every unit test keeps passing because it never
// touches the query.
type HostArgs struct {
	SID         string
	Covens      []string
	IncServices []string // parallel with IncNames
	IncNames    []string
	TraitPairs  []string // "<key>=<value>"
}

// Args flattens the host for [MatchSQL]. Results are deduplicated and sorted so
// a query plan (and a test assertion) does not depend on roster order.
func (h Host) Args() HostArgs {
	a := HostArgs{SID: h.SID}
	covens := map[string]struct{}{}
	pairs := map[string]struct{}{}

	for _, c := range h.Covens {
		covens[c] = struct{}{}
	}
	collectTraitPairs(h.Traits, pairs)
	for _, inc := range h.Member {
		a.IncServices = append(a.IncServices, inc.Service)
		a.IncNames = append(a.IncNames, inc.Name)
		for _, c := range inc.Covens {
			covens[c] = struct{}{}
		}
		collectTraitPairs(inc.Traits, pairs)
	}

	a.Covens = sortedKeys(covens)
	a.TraitPairs = sortedKeys(pairs)
	return a
}

// collectTraitPairs renders each trait into the "<key>=<value>" form the
// predicate compares against, expanding a list value into one pair per item
// (`{"role": ["primary","seed"]}` reaches both `trait.role=primary` and
// `trait.role=seed`) — the same rule [traitHolds] applies in Go.
//
// A trait key cannot contain '=' (TraitKeyPattern), so the first '=' always
// separates key from value and the encoding is unambiguous.
func collectTraitPairs(traits map[string]any, into map[string]struct{}) {
	for k, v := range traits {
		if list, ok := v.([]any); ok {
			for _, item := range list {
				into[k+"="+scalarText(item)] = struct{}{}
			}
			continue
		}
		into[k+"="+scalarText(v)] = struct{}{}
	}
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MatchSQL renders the predicate selecting rows whose subject reaches this
// host. ph binds one argument and returns its placeholder (`$3`), which lets a
// caller splice the predicate into a larger query with its own numbering.
//
// Every dimension is NULL on a row that does not use it, and NULL propagates to
// NULL rather than TRUE in each arm — so a row matches through exactly the
// dimension it was written with, and a row with no subject at all matches
// nothing (default-deny survives even a row written around the CHECK).
func MatchSQL(cols SQLColumns, a HostArgs, ph func(any) string) string {
	sid := ph(a.SID)
	covens := ph(a.Covens)
	svcs := ph(a.IncServices)
	names := ph(a.IncNames)
	pairs := ph(a.TraitPairs)

	return strings.Join([]string{
		"(",
		fmt.Sprintf("     %s = ANY(%s)", sid, cols.SIDs),
		fmt.Sprintf("  OR %s && %s::text[]", cols.Covens, covens),
		fmt.Sprintf("  OR EXISTS (SELECT 1 FROM unnest(%s::text[], %s::text[]) AS _inc(svc, nm)"+
			" WHERE _inc.svc = %s AND _inc.nm = %s)", svcs, names, cols.Service, cols.Incarnation),
		fmt.Sprintf("  OR (%s || '=' || %s) = ANY(%s::text[])", cols.TraitKey, cols.TraitValue, pairs),
		")",
	}, "\n")
}

// ArgBinder is the trivial ph implementation for a query whose only arguments
// are the subject ones: it appends to args and returns $1, $2, … Callers that
// already have placeholders in flight pass their own ph instead.
type ArgBinder struct{ Args []any }

func (b *ArgBinder) Bind(v any) string {
	b.Args = append(b.Args, v)
	return fmt.Sprintf("$%d", len(b.Args))
}
