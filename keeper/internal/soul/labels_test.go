package soul

import (
	"reflect"
	"testing"
)

// Guard tests for label inheritance (ADR-080). The invariant under test is that
// a key held on BOTH the host and its incarnation yields BOTH values — no
// precedence, nothing dropped — because either value must grant access.

func TestUnionTraits_KeyOnBothSides_UnionsNotOverwrites(t *testing.T) {
	own := map[string]any{"owner": "bobik"}
	inherited := map[string]any{"owner": "dba"}

	got := UnionTraits(own, inherited)

	want := map[string]any{"owner": []any{"bobik", "dba"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owner on host AND incarnation = %#v, want both values %#v (neither side may win)", got, want)
	}
}

func TestUnionTraits_ScalarStaysScalarWhenUncontested(t *testing.T) {
	// A key from one side only must keep its shape: `traits.namespace ==
	// 'dba-ns'` is a scalar comparison and would break against a list.
	got := UnionTraits(map[string]any{"role": "cache"}, map[string]any{"namespace": "dba-ns"})

	want := map[string]any{"role": "cache", "namespace": "dba-ns"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uncontested keys = %#v, want scalars preserved %#v", got, want)
	}
}

func TestUnionTraits_SameValueBothSides_CollapsesToScalar(t *testing.T) {
	got := UnionTraits(map[string]any{"owner": "dba"}, map[string]any{"owner": "dba"})

	want := map[string]any{"owner": "dba"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identical value on both sides = %#v, want scalar %#v (not a one-element list)", got, want)
	}
}

func TestUnionTraits_ListAndScalar_FlattensOwnFirst(t *testing.T) {
	own := map[string]any{"owners": []any{"alice", "bob"}}
	inherited := map[string]any{"owners": "dba"}

	got := UnionTraits(own, inherited)

	want := map[string]any{"owners": []any{"alice", "bob", "dba"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list ∪ scalar = %#v, want flat own-first %#v", got, want)
	}
}

func TestUnionTraits_DedupsAcrossTypes(t *testing.T) {
	// Dedup keys on the text form, the way PG's `->>` renders a value, so the
	// in-Go union and the SQL scope predicate agree on what "the same value" is.
	got := UnionTraits(map[string]any{"tier": float64(1)}, map[string]any{"tier": "1"})

	want := map[string]any{"tier": float64(1)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("1 ∪ \"1\" = %#v, want a single value %#v", got, want)
	}
}

func TestUnionTraits_EmptySideIsIdentity(t *testing.T) {
	own := map[string]any{"owner": "bobik"}
	if got := UnionTraits(own, nil); !reflect.DeepEqual(got, own) {
		t.Fatalf("union with no inherited = %#v, want own %#v", got, own)
	}
	inherited := map[string]any{"owner": "dba"}
	if got := UnionTraits(nil, inherited); !reflect.DeepEqual(got, inherited) {
		t.Fatalf("union with no own = %#v, want inherited %#v", got, inherited)
	}
}

func TestUnionCovens_OwnFirstDeduped(t *testing.T) {
	got := UnionCovens([]string{"dev", "eu-west"}, []string{"eu-west", "redis-prod"})

	want := []string{"dev", "eu-west", "redis-prod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coven union = %#v, want own-first deduped %#v", got, want)
	}
}

func TestUnionCovens_EmptySideIsIdentity(t *testing.T) {
	own := []string{"dev"}
	if got := UnionCovens(own, nil); !reflect.DeepEqual(got, own) {
		t.Fatalf("union with no inherited = %#v, want own %#v", got, own)
	}
	if got := UnionCovens(nil, own); !reflect.DeepEqual(got, own) {
		t.Fatalf("union with no own = %#v, want inherited %#v", got, own)
	}
}
