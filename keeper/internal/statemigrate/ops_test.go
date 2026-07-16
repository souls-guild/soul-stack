package statemigrate

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func apply(t *testing.T, ops []Op, state map[string]any) (map[string]any, error) {
	t.Helper()
	ev := mustEvaluator(t)
	res, err := Apply(context.Background(), state, Chain{{FromVersion: 1, ToVersion: 2, Transform: ops}}, ev)
	if err != nil {
		return nil, err
	}
	return res.FinalState, nil
}

func mustApply(t *testing.T, ops []Op, state map[string]any) map[string]any {
	t.Helper()
	out, err := apply(t, ops, state)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return out
}

// TestDelete_NonexistentNoOp — delete of a non-existent path = no-op (not an error).
func TestDelete_NonexistentNoOp(t *testing.T) {
	out := mustApply(t, []Op{
		{Delete: &DeleteOp{Path: "state.does.not.exist"}},
		{Delete: &DeleteOp{Path: "state.also_missing"}},
	}, map[string]any{"keep": 1})
	assertDeepEqualJSON(t, out, map[string]any{"keep": 1})
}

// TestRename_Basic — moves the value, the source is removed.
func TestRename_Basic(t *testing.T) {
	out := mustApply(t, []Op{
		{Rename: &RenameOp{From: "state.old", To: "state.new"}},
	}, map[string]any{"old": "v"})
	assertDeepEqualJSON(t, out, map[string]any{"new": "v"})
}

// TestRename_ToExists — rename into an existing to = ClassRenameToExists error.
func TestRename_ToExists(t *testing.T) {
	_, err := apply(t, []Op{
		{Rename: &RenameOp{From: "state.a", To: "state.b"}},
	}, map[string]any{"a": 1, "b": 2})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassRenameToExists {
		t.Fatalf("ошибка = %v, want ClassRenameToExists", err)
	}
}

// TestRename_SourceMissingNoOp — rename of a non-existent source = no-op.
func TestRename_SourceMissingNoOp(t *testing.T) {
	out := mustApply(t, []Op{
		{Rename: &RenameOp{From: "state.missing", To: "state.new"}},
	}, map[string]any{"keep": 1})
	assertDeepEqualJSON(t, out, map[string]any{"keep": 1})
}

// TestMove_AliasOfRename — move = rename (shared code path).
func TestMove_AliasOfRename(t *testing.T) {
	// move parses into Op.Rename — verify via parse that the move discriminator
	// produces the same type.
	mig, err := Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - move: { from: state.a, to: state.b }\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(mig.Transform) != 1 || mig.Transform[0].Rename == nil {
		t.Fatalf("move не разобран в Rename: %#v", mig.Transform)
	}
	out := mustApply(t, mig.Transform, map[string]any{"a": "x"})
	assertDeepEqualJSON(t, out, map[string]any{"b": "x"})
}

// TestSet_CELIntMath — set.value with ${ int(...) * ... } yields a native number.
func TestSet_CELIntMath(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.bytes", Value: "${ int(state.mb) * 1048576 }"}},
	}, map[string]any{"mb": 512})
	if got := out["bytes"]; got != int64(536870912) {
		t.Fatalf("bytes = %v (%T), want 536870912", got, got)
	}
}

// TestSet_NestedStructInterpolation — a nested value structure with embedded
// ${ … } in the leaves is resolved leaf-by-leaf.
func TestSet_NestedStructInterpolation(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.cfg", Value: map[string]any{
			"name":    "${ state.who }",
			"static":  "off ~* &* +@all",
			"nested":  map[string]any{"port": "${ 6379 + 1 }"},
			"literal": 42,
		}}},
	}, map[string]any{"who": "app"})
	cfg, _ := out["cfg"].(map[string]any)
	if cfg["name"] != "app" {
		t.Errorf("name = %v, want app", cfg["name"])
	}
	if cfg["static"] != "off ~* &* +@all" {
		t.Errorf("static = %v", cfg["static"])
	}
	nested, _ := cfg["nested"].(map[string]any)
	if nested["port"] != int64(6380) {
		t.Errorf("nested.port = %v (%T), want 6380", nested["port"], nested["port"])
	}
	if cfg["literal"] != 42 {
		t.Errorf("literal = %v", cfg["literal"])
	}
}

// TestSet_CreatesIntermediateMaps — set creates intermediate maps.
func TestSet_CreatesIntermediateMaps(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.a.b.c", Value: "deep"}},
	}, map[string]any{})
	assertDeepEqualJSON(t, out, map[string]any{"a": map[string]any{"b": map[string]any{"c": "deep"}}})
}

// TestForeach_OverMapBindsValue — foreach over a map binds <as> to the VALUE.
func TestForeach_OverMapBindsValue(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{
			In: "${ state.src }",
			As: "v",
			Do: []Op{
				{Set: &SetOp{Path: "state.dst.${ v.id }", Value: "${ v.label }"}},
			},
		}},
	}, map[string]any{"src": map[string]any{
		"k1": map[string]any{"id": "a", "label": "L-a"},
		"k2": map[string]any{"id": "b", "label": "L-b"},
	}})
	dst, _ := out["dst"].(map[string]any)
	if dst["a"] != "L-a" || dst["b"] != "L-b" {
		t.Fatalf("dst = %#v, want {a:L-a, b:L-b}", dst)
	}
}

// TestForeach_OverListBindsElement — foreach over a list binds <as> to the ELEMENT.
func TestForeach_OverListBindsElement(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{
			In: "${ state.names }",
			As: "n",
			Do: []Op{
				{Set: &SetOp{Path: "state.users.${ n }", Value: "enabled"}},
			},
		}},
	}, map[string]any{"names": []any{"app", "monitor"}})
	users, _ := out["users"].(map[string]any)
	if users["app"] != "enabled" || users["monitor"] != "enabled" {
		t.Fatalf("users = %#v", users)
	}
}

// TestForeach_Nested — nested foreach: outer and inner <as> both in scope.
func TestForeach_Nested(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{
			In: "${ state.groups }",
			As: "g",
			Do: []Op{
				{Foreach: &ForeachOp{
					In: "${ g.members }",
					As: "m",
					Do: []Op{
						{Set: &SetOp{Path: "state.flat.${ m }", Value: "${ g.name }"}},
					},
				}},
			},
		}},
	}, map[string]any{"groups": []any{
		map[string]any{"name": "admins", "members": []any{"alice", "bob"}},
		map[string]any{"name": "users", "members": []any{"carol"}},
	}})
	flat, _ := out["flat"].(map[string]any)
	want := map[string]any{"alice": "admins", "bob": "admins", "carol": "users"}
	if !reflect.DeepEqual(flat, want) {
		t.Fatalf("flat = %#v, want %#v", flat, want)
	}
}

// TestForeach_ScalarNotIterable — foreach in: yields a scalar → error.
func TestForeach_ScalarNotIterable(t *testing.T) {
	_, err := apply(t, []Op{
		{Foreach: &ForeachOp{In: "${ state.x }", As: "i", Do: []Op{{Delete: &DeleteOp{Path: "state.x"}}}}},
	}, map[string]any{"x": 5})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassForeachType {
		t.Fatalf("ошибка = %v, want ClassForeachType", err)
	}
}

// TestSet_TraverseThroughScalar — intermediate path segment is a scalar → error.
func TestSet_TraverseThroughScalar(t *testing.T) {
	_, err := apply(t, []Op{
		{Set: &SetOp{Path: "state.a.b", Value: 1}},
	}, map[string]any{"a": "scalar"})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassPathTraverse {
		t.Fatalf("ошибка = %v, want ClassPathTraverse", err)
	}
}

// TestSet_PathWithInterpolatedMiddleSegment — a ${ … } segment in the middle of a path
// (state.users.${ name }.acl) is resolved and navigation continues.
func TestSet_PathWithInterpolatedMiddleSegment(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{
			In: "${ state.names }",
			As: "name",
			Do: []Op{
				{Set: &SetOp{Path: "state.users.${ name }.acl", Value: "allow"}},
			},
		}},
	}, map[string]any{"names": []any{"app"}})
	users, _ := out["users"].(map[string]any)
	app, _ := users["app"].(map[string]any)
	if app["acl"] != "allow" {
		t.Fatalf("users.app.acl = %v, want allow", app["acl"])
	}
}

// TestOps_AppliedInOrder — operations see the state as mutated by previous ones.
func TestOps_AppliedInOrder(t *testing.T) {
	out := mustApply(t, []Op{
		{Rename: &RenameOp{From: "state.a", To: "state.tmp"}},
		{Set: &SetOp{Path: "state.b", Value: "${ state.tmp }"}},
		{Delete: &DeleteOp{Path: "state.tmp"}},
	}, map[string]any{"a": "val"})
	assertDeepEqualJSON(t, out, map[string]any{"b": "val"})
}
