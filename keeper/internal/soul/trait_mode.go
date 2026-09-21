package soul

import (
	"fmt"
	"regexp"
	"sort"
)

// TraitMode — mode for applying operator trait labels (ADR-060) to a set of
// hosts via the bulk API `POST /v1/souls/traits`. Traits is a map (key →
// scalar | list-of-scalar), unlike the flat Coven label list, so the mode
// vocabulary is adapted for map semantics (Coven counterpart — [CovenMode]):
//
//   - merge   — set/overwrite the given keys, keep the rest;
//   - replace — replace the WHOLE traits map (footgun: empty = clear all);
//   - remove  — delete the given keys (by name).
type TraitMode string

const (
	// TraitMerge — existing map ⊕ given keys (overwrite by key, other keys
	// kept). Default mode of the bulk API.
	TraitMerge TraitMode = "merge"
	// TraitReplace — the given map wholesale (empty = clear all traits).
	TraitReplace TraitMode = "replace"
	// TraitRemove — existing map minus the given keys (by name).
	TraitRemove TraitMode = "remove"
)

// ValidTraitMode — closed-enum mode check.
func ValidTraitMode(m TraitMode) bool {
	switch m {
	case TraitMerge, TraitReplace, TraitRemove:
		return true
	}
	return false
}

// TraitKeyPattern — trait label key shape: kebab OR snake_case (`-`/`_` as
// internal separators). Differs from [CovenPattern] (`-` only): a trait key
// is a free-form operator attribute name (`owner_team`), `_` is allowed
// (NIM-67, ADR-060 "arbitrary string keys"). Grammar = reScenarioName.
const TraitKeyPattern = `^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`

var traitKeyRe = regexp.MustCompile(TraitKeyPattern)

const traitKeyMaxLen = covenMaxLen

// ValidTraitKey validates one trait label key (kebab/snake_case, 1..63 chars).
func ValidTraitKey(key string) bool {
	if len(key) == 0 || len(key) > traitKeyMaxLen {
		return false
	}
	return traitKeyRe.MatchString(key)
}

// ValidTraitValue validates a trait label value: a scalar (string/number/
// bool) or a list of scalars (depth ≤ 1) is allowed. Nested maps and
// lists-in-lists are rejected — a trait value stays "flat" (the read/target
// path projects it into `soulprint.self.traits.<key>` for CEL targeting,
// where nesting isn't needed and would complicate predicates).
//
// huma/encoding's JSON decoder returns numbers as float64, so the numeric
// case covers the float64/int/int64 branches; a nil value under a key is
// rejected (ambiguous with "delete" — use mode=remove to delete instead).
func ValidTraitValue(v any) error {
	switch val := v.(type) {
	case string, bool, float64, int, int64:
		return nil
	case []any:
		for i, elem := range val {
			if !isScalar(elem) {
				return fmt.Errorf("soul: trait value list element %d must be scalar (string/number/bool), got %T", i, elem)
			}
		}
		return nil
	case nil:
		return fmt.Errorf("soul: trait value must not be null (use mode=remove to delete a key)")
	default:
		return fmt.Errorf("soul: trait value must be scalar or list of scalars, got %T (nested objects/arrays are not allowed)", v)
	}
}

// isScalar — a valid list-value element (scalar; nested list/map forbidden,
// depth ≤ 1).
func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64, int, int64:
		return true
	}
	return false
}

// ValidateTraitDelta validates a (key → value) set for mode=merge/replace:
// each key via [ValidTraitKey], each value via [ValidTraitValue]. Returns
// the first error found; a nil map is allowed (replace = "clear").
func ValidateTraitDelta(delta map[string]any) error {
	for _, key := range sortedTraitKeys(delta) {
		if !ValidTraitKey(key) {
			return fmt.Errorf("soul: invalid trait key %q (must match %s)", key, TraitKeyPattern)
		}
		if err := ValidTraitValue(delta[key]); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTraitKeys validates a key list for mode=remove: each key via
// [ValidTraitKey]. Returns the first error.
func ValidateTraitKeys(keys []string) error {
	for _, key := range keys {
		if !ValidTraitKey(key) {
			return fmt.Errorf("soul: invalid trait key %q (must match %s)", key, TraitKeyPattern)
		}
	}
	return nil
}

// sortedTraitKeys — deterministic map key iteration (stable validation
// error messages).
func sortedTraitKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
