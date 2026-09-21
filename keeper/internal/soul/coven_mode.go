package soul

import "sort"

// CovenMode — mode for applying Coven label(s) to a host set. A single
// vocabulary for both coven mutation paths: the keeper-side core module
// `core.soul.registered` (scenario path) and the bulk API `POST /v1/souls/coven`.
//
// Values match docs/keeper/modules.md → mode semantics. In the pilot the bulk
// API only accepts append/remove (one label per call); replace-set is a
// separate future slice, but the enum here is complete so
// core.soul.registered and bulk share one source of truth.
type CovenMode string

const (
	// CovenAppend — existing ∪ given (idempotent).
	CovenAppend CovenMode = "append"
	// CovenReplace — the given set entirely (footgun: empty = an error at the
	// caller's boundary, ApplyCovenMode doesn't enforce it — it's a pure function).
	CovenReplace CovenMode = "replace"
	// CovenRemove — existing \ given.
	CovenRemove CovenMode = "remove"
)

// CovenLabelValidator — a validation hook for an assigned Coven label beyond
// format (for the future environment directory Q1 — ADR-008 amend). Called by
// the Service layer's bulk coven-assign BEFORE the UPDATE. In the pilot it's
// [NoopCovenLabelValidator] (format-only: format is already checked by [ValidCoven]).
// When the directory appears, its implementation is swapped without changing the API.
type CovenLabelValidator interface {
	Validate(label string) error
}

// NoopCovenLabelValidator — the pilot no-op implementation of [CovenLabelValidator].
// Label format is checked separately by [ValidCoven]; this is the slot for the directory.
type NoopCovenLabelValidator struct{}

// Validate always passes (format-only — format is already checked by ValidCoven).
func (NoopCovenLabelValidator) Validate(string) error { return nil }

// ValidCovenMode — closed-enum check of the mode.
func ValidCovenMode(m CovenMode) bool {
	switch m {
	case CovenAppend, CovenReplace, CovenRemove:
		return true
	}
	return false
}

// ApplyCovenMode — a pure function applying a mode to an existing set of
// Coven labels. Returns (final, removed):
//
//   - final   — the resulting set, sorted and deduplicated;
//   - removed — labels actually removed (only for CovenRemove;
//     sorted; nil for append/replace).
//
// A single source of set semantics for core.soul.registered (per-host) and
// the bulk API. Extracted from coremod/soul/registered.go (pilot-spec refactor)
// so the two paths don't diverge in how they interpret modes. For an unknown
// mode it returns (existing, nil) — mode validation is the caller's job before the call.
func ApplyCovenMode(existing, wanted []string, mode CovenMode) (final, removed []string) {
	switch mode {
	case CovenAppend:
		set := covenSetOf(existing)
		for _, c := range wanted {
			set[c] = struct{}{}
		}
		return covenSortedKeys(set), nil
	case CovenReplace:
		return covenUniqueSorted(wanted), nil
	case CovenRemove:
		set := covenSetOf(existing)
		var rem []string
		for _, c := range wanted {
			if _, has := set[c]; has {
				delete(set, c)
				rem = append(rem, c)
			}
		}
		sort.Strings(rem)
		return covenSortedKeys(set), rem
	}
	return existing, nil
}

// CovenSetEqual performs order-independent comparison of two sets of Coven tags
// (after deduplication). Used to avoid unnecessary UPDATE and
// spurious changed=true when the new set matches the current one.
func CovenSetEqual(a, b []string) bool {
	sa, sb := covenSetOf(a), covenSetOf(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if _, ok := sb[k]; !ok {
			return false
		}
	}
	return true
}

func covenSetOf(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

func covenSortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func covenUniqueSorted(xs []string) []string {
	return covenSortedKeys(covenSetOf(xs))
}
