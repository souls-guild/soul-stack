package trial

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// deepEqualJSON compares two Go structs that passed through structpb
// (map[string]any / []any / float64 / string / bool / nil). Direct
// reflect.DeepEqual is sufficient: both sides are normalized identically
// by structpb.AsMap (numbers → float64, map traversal is deterministic).
func deepEqualJSON(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// collKind/collectionKind/schemaFieldType — ★ logic identical to stateop.* (same names).
type collKind int

const (
	collKindUnknown collKind = iota
	collKindList
	collKindMap
)

func collectionKind(existing any, present bool, schema map[string]any, field string) collKind {
	if present {
		switch existing.(type) {
		case []any:
			return collKindList
		case map[string]any:
			return collKindMap
		}
		return collKindUnknown
	}
	switch schemaFieldType(schema, field) {
	case "array":
		return collKindList
	case "object":
		return collKindMap
	}
	return collKindUnknown
}

func schemaFieldType(schema map[string]any, field string) string {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return ""
	}
	fieldSchema, ok := props[field].(map[string]any)
	if !ok {
		return ""
	}
	t, _ := fieldSchema["type"].(string)
	return t
}

// deepCopyState — deep-copy base state via JSON round-trip (expected result
// must not hold reference to fixtures.state). nil/empty → empty map. Data is
// JSON-safe (YAML fixtures); on marshal failure — empty base, verification catches
// divergence. ★ Logic identical to stateop.deepCopyMap (name local to avoid collision).
func deepCopyState(m map[string]any) map[string]any {
	out := map[string]any{}
	if len(m) > 0 {
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &out)
		}
	}
	return out
}

// hasErrors — are there error-level diagnostics.
func hasErrors(ds []diag.Diagnostic) bool {
	return diag.HasErrors(ds)
}

// formatDiags merges diagnostics into one string for error message.
func formatDiags(ds []diag.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Level != diag.LevelError {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(d.Code)
		if d.YAMLPath != "" {
			b.WriteString(" @ " + d.YAMLPath)
		}
		b.WriteString(": " + d.Message)
	}
	return b.String()
}
