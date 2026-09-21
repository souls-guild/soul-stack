package stateop

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestSetNestedPath_ProdNoSilentClobber — the prod side of the
// setNestedPath unit guard (mirrors trial.TestSetNestedPath_NoSilentClobber):
// missing gets created, an existing non-map → error without mutation.
func TestSetNestedPath_ProdNoSilentClobber(t *testing.T) {
	m := map[string]any{}
	if err := setNestedPath(m, "config.maxmemory", "256mb"); err != nil {
		t.Fatalf("setNestedPath missing: %v", err)
	}
	if m["config"].(map[string]any)["maxmemory"] != "256mb" {
		t.Errorf("config was not materialized: %+v", m)
	}
	m2 := map[string]any{"config": "scalar"}
	if err := setNestedPath(m2, "config.maxmemory", "256mb"); err == nil {
		t.Fatal("* setNestedPath over config=\"scalar\" should return an error")
	}
	if m2["config"] != "scalar" {
		t.Errorf("* scalar value clobbered: %+v", m2)
	}
}

func TestDeepCopyMap(t *testing.T) {
	src := map[string]any{
		"nested": map[string]any{"k": "v"},
		"list":   []any{float64(1), float64(2)},
	}
	cp := deepCopyMap(src)
	nested := cp["nested"].(map[string]any)
	nested["k"] = "changed"
	if src["nested"].(map[string]any)["k"] != "v" {
		t.Errorf("deep copy is not deep: original mutated")
	}
}

// merge is the three-new-verbs shorthand: none of `present`/`append`/`unset`
// takes a match predicate, so both evaluators are legitimately nil here.
func merge(t *testing.T, before map[string]any, ops ...render.RenderedOp) map[string]any {
	t.Helper()
	out, err := Merge(before, ops, nil, nil, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return out
}

// TestPresent_ExistingValueWins is the whole point of the verb: `present` is the
// one that does NOT overwrite. An empty list counts as a value — a field
// deliberately emptied must not silently refill.
func TestPresent_ExistingValueWins(t *testing.T) {
	op := render.RenderedOp{Verb: config.VerbPresent, Field: "port", Value: float64(6379)}
	for name, tc := range map[string]struct {
		before map[string]any
		want   any
	}{
		"absent":     {map[string]any{}, float64(6379)},
		"null":       {map[string]any{"port": nil}, float64(6379)},
		"has value":  {map[string]any{"port": float64(6380)}, float64(6380)},
		"empty list": {map[string]any{"port": []any{}}, []any{}},
		"false":      {map[string]any{"port": false}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := merge(t, tc.before, op)["port"]
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("* present over %v: got %#v, want %#v", tc.before, got, tc.want)
			}
		})
	}
}

// TestAppend_NoIdentityCheck separates `append` from `add`: the same element
// twice yields two elements. `add` would have deduped it.
func TestAppend_NoIdentityCheck(t *testing.T) {
	op := render.RenderedOp{Verb: config.VerbAppend, Field: "events", Value: "restarted"}
	out := merge(t, map[string]any{}, op, op)
	if got := out["events"]; !reflect.DeepEqual(got, []any{"restarted", "restarted"}) {
		t.Errorf("* append is deduping: %#v", got)
	}
}

func TestAppend_MaterializesFromNothing(t *testing.T) {
	op := render.RenderedOp{Verb: config.VerbAppend, Field: "events", Value: "started"}
	for name, before := range map[string]map[string]any{
		"absent": {},
		"null":   {"events": nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := merge(t, before, op)["events"]; !reflect.DeepEqual(got, []any{"started"}) {
				t.Errorf("* got %#v, want a one-element list", got)
			}
		})
	}
}

// TestAppend_RefusesANonList — coercing a map or a scalar into a list would
// silently drop what was there.
func TestAppend_RefusesANonList(t *testing.T) {
	for name, tc := range map[string]struct {
		before map[string]any
		schema config.InputSchemaMap
	}{
		"map value":    {map[string]any{"users": map[string]any{"alice": "x"}}, nil},
		"scalar value": {map[string]any{"users": "alice"}, nil},
		"schema object": {map[string]any{}, config.InputSchemaMap{
			"users": &config.InputSchema{Type: "object"}}},
	} {
		t.Run(name, func(t *testing.T) {
			op := render.RenderedOp{Verb: config.VerbAppend, Field: "users", Value: "bob"}
			out, err := Merge(tc.before, []render.RenderedOp{op}, tc.schema, nil, nil)
			if err == nil {
				t.Fatalf("* append onto %#v should fail, got %#v", tc.before, out)
			}
			if !strings.Contains(err.Error(), "append") {
				t.Errorf("error does not name the verb: %v", err)
			}
		})
	}
}

// TestUnset_DropsTheFieldItself — `unset` removes the key, where `remove`
// removes elements from inside it. Absent field → no-op, not an error.
func TestUnset_DropsTheFieldItself(t *testing.T) {
	out := merge(t, map[string]any{"port": float64(6379), "keep": "yes"},
		render.RenderedOp{Verb: config.VerbUnset, Field: "port"},
		render.RenderedOp{Verb: config.VerbUnset, Field: "never_was"})
	if _, still := out["port"]; still {
		t.Errorf("* unset left the key behind: %#v", out)
	}
	if out["keep"] != "yes" {
		t.Errorf("unset touched a neighbouring field: %#v", out)
	}
}

// TestAdd_IdentityBelongsToTheCollectionKind — `key:` names a property of a MAP
// entry and `match:` a predicate over a LIST element, and which one the engine
// reads is decided by the field's kind, never by which the author wrote. Silently
// ignoring the wrong one is what makes it worth an error: `key: name` on a list
// used to fall through to deep-equal, so a re-run with the same name and one
// changed property appended a SECOND element instead of hitting on_conflict.
func TestAdd_IdentityBelongsToTheCollectionKind(t *testing.T) {
	t.Run("key on a list", func(t *testing.T) {
		before := map[string]any{"users": []any{map[string]any{"name": "alice", "perms": "ro"}}}
		op := render.RenderedOp{
			Verb: config.VerbAdd, Field: "users", Key: "name",
			Value: map[string]any{"name": "alice", "perms": "rw"},
		}
		out, err := Merge(before, []render.RenderedOp{op}, nil, nil, nil)
		if err == nil {
			t.Fatalf("* add with key: onto a list should fail, got %#v", out)
		}
		if !strings.Contains(err.Error(), "match:") {
			t.Errorf("error does not name the spelling that works here: %v", err)
		}
	})

	t.Run("match on a map", func(t *testing.T) {
		before := map[string]any{"users": map[string]any{"alice": "ro"}}
		op := render.RenderedOp{
			Verb: config.VerbAdd, Field: "users", Match: "elem.name == value.name", Value: "rw",
		}
		out, err := Merge(before, []render.RenderedOp{op}, nil, nil, nil)
		if err == nil {
			t.Fatalf("* add with match: onto a map should fail, got %#v", out)
		}
		// "requires key:" is what a map add without an identity says anyway, so the
		// assertion is on the diagnosis: the error must name the match: the author
		// wrote, or it is not evidence that the wrong spelling was noticed.
		if !strings.Contains(err.Error(), "match:") {
			t.Errorf("error does not name the param that does not belong here: %v", err)
		}
	})
}
