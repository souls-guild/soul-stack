package statemigrate

import (
	"encoding/json"
	"sort"
	"strings"
)

// deepCopyMap makes a deep copy of map[string]any via a JSON round-trip.
// incarnation.state values are JSON-safe (read from JSONB / YAML fixtures),
// so marshal doesn't fail. nil/empty → empty map (the core's state is never nil).
//
// A narrow copy of the keeper/internal/scenario/state.deepCopyMap pattern: the
// statemigrate core doesn't pull in a dependency on the scenario package.
func deepCopyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// sortedKeys returns map keys in lexicographic order (determinism for
// foreach iteration over a map: a migration is a reproducible function).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// joinKeys assembles a flat list of keys into a human-readable address (for
// path diagnostics). Segments in state are JSON-safe strings.
func joinKeys(keys []string) string {
	return "state." + strings.Join(keys, ".")
}
