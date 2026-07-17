package handlers

// Unit test for traitScalarEquals (GET arm of trait-scope, ADR-047 amendment / ADR-060
// §7 slice 1) — scalar-only semantics, aligned with the List SQL arm
// (incarnation.appendScopeClause `traits->>$ = $`, BUG #1 fix). Self-contained
// (a pure function of map[string]any), independent of the package's shared fakes.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTraitScalarEquals_Table(t *testing.T) {
	tests := []struct {
		name   string
		traits map[string]any
		key    string
		value  string
		want   bool
	}{
		{
			name:   "string hit",
			traits: map[string]any{"owner": "alice"},
			key:    "owner", value: "alice", want: true,
		},
		{
			name:   "string miss (different value)",
			traits: map[string]any{"owner": "alice"},
			key:    "owner", value: "bob", want: false,
		},
		{
			name:   "float64 hit (jsonb number decodes to float64)",
			traits: map[string]any{"shard": float64(3)},
			key:    "shard", value: "3", want: true,
		},
		{
			name:   "bool hit",
			traits: map[string]any{"managed": true},
			key:    "managed", value: "true", want: true,
		},
		{
			name:   "missing key -> miss",
			traits: map[string]any{"owner": "alice"},
			key:    "team", value: "dba", want: false,
		},
		{
			name:   "nil traits → miss",
			traits: nil,
			key:    "owner", value: "alice", want: false,
		},
		{
			name:   "list-Trait -> false (scalar-only, does NOT match an array element)",
			traits: map[string]any{"env": []any{"prod", "stage"}},
			key:    "env", value: "prod", want: false,
		},
		{
			name:   "map-Trait → false (non-scalar)",
			traits: map[string]any{"meta": map[string]any{"k": "v"}},
			key:    "meta", value: "v", want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := traitScalarEquals(tc.traits, tc.key, tc.value); got != tc.want {
				t.Errorf("traitScalarEquals(%v, %q, %q) = %v, want %v",
					tc.traits, tc.key, tc.value, got, tc.want)
			}
		})
	}
}

// TestTraitScalarEquals_JSONNumber — values that arrive as json.Number (decoder
// with UseNumber) also match by their string form (the scalar branch covers json.Number).
func TestTraitScalarEquals_JSONNumber(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"shard": 7}`))
	dec.UseNumber()
	var traits map[string]any
	if err := dec.Decode(&traits); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !traitScalarEquals(traits, "shard", "7") {
		t.Errorf("json.Number 7 did not match \"7\" (scalar branch must cover json.Number)")
	}
}
