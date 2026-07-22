package handlers

// NIM-128: incarnation trait-scope is evaluated by the unified resolver
// (traitsToScopeInput → rbac.EvalScope), replacing the former scalar-only
// traitScalarEquals. A jsonb trait value projects into rbac.ScopeInput.Traits:
// scalars become a one-element slice; LIST values contribute each element
// (so a `trait.env=prod` scope now matches a `{env:[prod,stage]}` label —
// List and Get agree, resolving the former List↔Get divergence).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// traitVisible mirrors the incarnation Get/List scope check: build the scope
// input from an incarnation's traits and evaluate a single `trait.<key>=value`
// predicate against it.
func traitVisible(traits map[string]any, key, value string) bool {
	expr, err := rbac.ParseScopeExpr("trait." + key + "=" + value)
	if err != nil {
		panic(err)
	}
	return rbac.EvalScope(expr, rbac.ScopeInput{Traits: traitsToScopeInput(traits)})
}

func TestTraitScope_Table(t *testing.T) {
	tests := []struct {
		name   string
		traits map[string]any
		key    string
		value  string
		want   bool
	}{
		{"string hit", map[string]any{"owner": "alice"}, "owner", "alice", true},
		{"string miss", map[string]any{"owner": "alice"}, "owner", "bob", false},
		{"float64 hit (jsonb number → float64)", map[string]any{"shard": float64(3)}, "shard", "3", true},
		{"bool hit", map[string]any{"managed": true}, "managed", "true", true},
		{"missing key → miss", map[string]any{"owner": "alice"}, "team", "dba", false},
		{"nil traits → miss", nil, "owner", "alice", false},
		// NIM-128: a list value matches on any element (List↔Get consistent).
		{"list-Trait hit (element match)", map[string]any{"env": []any{"prod", "stage"}}, "env", "prod", true},
		{"list-Trait miss (element absent)", map[string]any{"env": []any{"prod", "stage"}}, "env", "dev", false},
		{"map-Trait → miss (non-addressable)", map[string]any{"meta": map[string]any{"k": "v"}}, "meta", "v", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := traitVisible(tc.traits, tc.key, tc.value); got != tc.want {
				t.Errorf("traitVisible(%v, %q, %q) = %v, want %v", tc.traits, tc.key, tc.value, got, tc.want)
			}
		})
	}
}

// TestTraitScope_JSONNumber — a value decoded as json.Number (UseNumber) still
// matches by its string form.
func TestTraitScope_JSONNumber(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"shard": 7}`))
	dec.UseNumber()
	var traits map[string]any
	if err := dec.Decode(&traits); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !traitVisible(traits, "shard", "7") {
		t.Errorf("json.Number 7 did not match \"7\"")
	}
}
