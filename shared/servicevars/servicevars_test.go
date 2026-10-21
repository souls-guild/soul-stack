package servicevars

import (
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// TestIncarnationContext_NoTraitsIsAnEmptyMap — an incarnation context carrying no
// traits reads `incarnation.traits` as the empty map a run reads off the NOT NULL
// DEFAULT '{}' column: `has()` is true, and `default()` over a key falls back rather
// than failing on `traits`. A definition and a `_stack.yaml` selector alike.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in celMap, set `traits` only
// `if c.Traits != nil`. The nil cases go red.
func TestIncarnationContext_NoTraitsIsAnEmptyMap(t *testing.T) {
	defs := map[string]any{
		"team":       "${ default(incarnation.traits.team, 'none') }",
		"has_traits": "${ has(incarnation.traits) }",
	}
	for name, tc := range map[string]struct {
		traits map[string]any
		want   map[string]any
	}{
		"nil":       {nil, map[string]any{"team": "none", "has_traits": true}},
		"empty":     {map[string]any{}, map[string]any{"team": "none", "has_traits": true}},
		"one trait": {map[string]any{"team": "ops"}, map[string]any{"team": "ops", "has_traits": true}},
	} {
		inc := IncarnationContext{ID: "inc-1", Service: "redis", Traits: tc.traits}
		t.Run("a definition, traits "+name, func(t *testing.T) {
			layer, err := NewLayer("service vars", defs, inc)
			if err != nil {
				t.Fatalf("NewLayer: %v", err)
			}
			got, err := cel.NewVarView(nil, layer).Materialize()
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("vars = %#v, %v; want %#v", got, err, tc.want)
			}
		})
		t.Run("a stack selector, traits "+name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"vars/00-base.yaml": {Data: []byte("tier: base\n")},
				"vars/10-team.yaml": {Data: []byte("tier: team\n")},
				"vars/_stack.yaml": {Data: []byte("stack:\n  - file: 00-base.yaml\n  - file: 10-team.yaml\n" +
					"    when: \"has(incarnation.traits) && default(incarnation.traits.team, 'ops') == 'ops'\"\n")},
			}
			layers, err := Layers(fsys, inc, nil)
			if err != nil {
				t.Fatalf("Layers: %v", err)
			}
			got, _, err := cel.NewVarView(nil, layers...).Lookup("tier")
			if err != nil || got != "team" {
				t.Fatalf("tier = %#v, %v; want team: the selector must see the traits map", got, err)
			}
		})
	}
}
