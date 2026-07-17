package statemigrate

import (
	"context"
	"errors"
	"testing"
)

// This file rounds out completeness coverage of the DSL on top of ops_test.go /
// parse_test.go / statemigrate_test.go: operators (literal/missing-path
// branches), foreach (empty map, null-in), CEL-sandbox negatives (one case
// per forbidden identifier), atomicity on error, and edge paths.
// Does not duplicate already-covered cases (rename happy/to-exists/source-missing, set
// CEL/nested/intermediate, delete-noop, foreach map/list/nested/scalar).

// --- 1. Operators: literals and behavior on a missing path ----------------

// TestSet_LiteralScalar — set of a string literal without ${ … } passes through as-is
// (interpolateValue doesn't run CEL for strings without the marker).
func TestSet_LiteralScalar(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.acl", Value: "off ~* &* +@all"}},
	}, map[string]any{})
	if out["acl"] != "off ~* &* +@all" {
		t.Fatalf("acl = %v, want a literal without interpolation", out["acl"])
	}
}

// TestSet_OverwritesExisting — set on an existing path overwrites the value
// (documented semantics: "an existing Path gets overwritten").
func TestSet_OverwritesExisting(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.x", Value: "new"}},
	}, map[string]any{"x": "old"})
	if out["x"] != "new" {
		t.Fatalf("x = %v, want new (overwrite)", out["x"])
	}
}

// TestDelete_ExistingRemovesKey — happy-path delete: an existing key is removed.
func TestDelete_ExistingRemovesKey(t *testing.T) {
	out := mustApply(t, []Op{
		{Delete: &DeleteOp{Path: "state.drop"}},
	}, map[string]any{"drop": 1, "keep": 2})
	if _, ok := out["drop"]; ok {
		t.Fatalf("key drop not deleted: %#v", out)
	}
	// keep remains; JSON normalization of numeric types (deepCopyMap in Apply).
	assertDeepEqualJSON(t, out, map[string]any{"keep": 2})
}

// TestDelete_NestedRemovesLeafKeepsParent — deleting a nested leaf removes
// only the leaf, the parent map remains.
func TestDelete_NestedRemovesLeafKeepsParent(t *testing.T) {
	out := mustApply(t, []Op{
		{Delete: &DeleteOp{Path: "state.cfg.port"}},
	}, map[string]any{"cfg": map[string]any{"port": 6379, "host": "h"}})
	cfg, _ := out["cfg"].(map[string]any)
	if cfg == nil {
		t.Fatalf("parent cfg deleted along with the leaf: %#v", out)
	}
	if _, ok := cfg["port"]; ok {
		t.Fatalf("port not deleted: %#v", cfg)
	}
	if cfg["host"] != "h" {
		t.Fatalf("host affected: %#v", cfg)
	}
}

// TestDelete_ThroughScalarMidPathNoOp — delete with an intermediate non-map segment
// (state.a is a scalar, path state.a.b) = no-op, not an error (the path doesn't
// exist at all — nothing to delete, see deletePath).
func TestDelete_ThroughScalarMidPathNoOp(t *testing.T) {
	out := mustApply(t, []Op{
		{Delete: &DeleteOp{Path: "state.a.b.c"}},
	}, map[string]any{"a": "scalar"})
	if out["a"] != "scalar" {
		t.Fatalf("a affected by delete through a scalar: %#v", out)
	}
}

// --- 2. foreach: edge-case collections ------------------------------------------

// TestForeach_EmptyMapNoOp — foreach over an empty map does not iterate (the Do body
// never runs), state is unchanged. (An empty list is covered in
// statemigrate_test.go TestApply_EmptyForeachNoMaterialize.)
func TestForeach_EmptyMapNoOp(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{In: "${ state.src }", As: "v", Do: []Op{
			{Set: &SetOp{Path: "state.touched", Value: true}},
		}}},
	}, map[string]any{"src": map[string]any{}, "keep": 1})
	if _, ok := out["touched"]; ok {
		t.Fatalf("foreach body executed on an empty map: %#v", out)
	}
	// state is unchanged (src remains an empty map, keep is untouched). Comparison
	// via JSON normalization: deepCopyMap in Apply converts int to float64.
	assertDeepEqualJSON(t, out, map[string]any{"src": map[string]any{}, "keep": 1})
}

// TestForeach_NullInNotIterable — foreach in: yields null (missing key →
// CEL no-such-key) → error (not a list and not a map). Confirms that null
// is treated as non-iterable, not as an empty collection.
func TestForeach_NullInNotIterable(t *testing.T) {
	_, err := apply(t, []Op{
		{Foreach: &ForeachOp{In: "${ state.missing }", As: "v", Do: []Op{
			{Delete: &DeleteOp{Path: "state.x"}},
		}}},
	}, map[string]any{"x": 1})
	if err == nil {
		t.Fatalf("error = nil, want an error on a null collection")
	}
	// Can be either ClassForeachType (if CEL returned nil) or ClassCELInterp
	// (if no-such-key surfaced as a resolution error) — both are valid: what matters
	// is that foreach over null doesn't stay silent.
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("error = %v (%T), want *EvalError", err, err)
	}
	if ee.Class != ClassForeachType && ee.Class != ClassCELInterp {
		t.Fatalf("class = %s, want ForeachType|CELInterp", ee.Class)
	}
}

// TestForeach_NestedAsAccessInValue — nested access to <as-name> inside
// set.value (v.field), separate from the already-covered access in a path segment.
func TestForeach_NestedAsAccessInValue(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{In: "${ state.items }", As: "it", Do: []Op{
			{Set: &SetOp{Path: "state.out.${ it.key }", Value: "${ it.nested.deep }"}},
		}}},
	}, map[string]any{"items": []any{
		map[string]any{"key": "a", "nested": map[string]any{"deep": "DA"}},
		map[string]any{"key": "b", "nested": map[string]any{"deep": "DB"}},
	}})
	res, _ := out["out"].(map[string]any)
	if res["a"] != "DA" || res["b"] != "DB" {
		t.Fatalf("out = %#v, want {a:DA, b:DB}", res)
	}
}

// --- 3. CEL-sandbox negative: forbidden identifiers in set.value ---------

// applySetValueErr runs a single set with the given value expression over an
// empty state and returns the Apply error (or fails if there is none). All
// sandbox violations in set.value surface as *EvalError of class
// ClassCELInterp (interpolateValue wraps the Evaluator.Interpolate error).
func applySetValueErr(t *testing.T, valueExpr string) *EvalError {
	t.Helper()
	_, err := apply(t, []Op{
		{Set: &SetOp{Path: "state.x", Value: valueExpr}},
	}, map[string]any{})
	if err == nil {
		t.Fatalf("set.value %q: error = nil, want a sandbox error", valueExpr)
	}
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("set.value %q: error = %v (%T), want *EvalError", valueExpr, err, err)
	}
	if ee.Class != ClassCELInterp {
		t.Fatalf("set.value %q: class = %s, want %s", valueExpr, ee.Class, ClassCELInterp)
	}
	return ee
}

// TestSet_SandboxForbidsContextVars — register/soulprint/essence/input are
// forbidden in set.value (a migration is a pure function of the old state, ADR-019):
// these variables aren't declared in migration-CEL → resolution error. One
// negative case per forbidden identifier.
func TestSet_SandboxForbidsContextVars(t *testing.T) {
	for _, expr := range []string{
		"${ register.foo }",
		"${ soulprint.self.os.family }",
		"${ essence.bar }",
		"${ input.baz }",
	} {
		t.Run(expr, func(t *testing.T) {
			applySetValueErr(t, expr)
		})
	}
}

// TestSet_SandboxForbidsVault — vault(...) is forbidden in set.value (a migration
// doesn't pull secrets): the migration-CEL guard blocks the call.
func TestSet_SandboxForbidsVault(t *testing.T) {
	applySetValueErr(t, "${ vault('secret/x').password }")
}

// TestSet_SandboxForbidsNow — now() is forbidden in set.value (migration
// reproducibility): the guard blocks eval-time time.
func TestSet_SandboxForbidsNow(t *testing.T) {
	applySetValueErr(t, "${ now() }")
}

// TestForeach_SandboxForbidsContextVarInIn — the sandbox restriction also applies to
// foreach.in (not just set.value): the adjacent context is unavailable there too.
func TestForeach_SandboxForbidsContextVarInIn(t *testing.T) {
	_, err := apply(t, []Op{
		{Foreach: &ForeachOp{In: "${ input.items }", As: "v", Do: []Op{
			{Delete: &DeleteOp{Path: "state.x"}},
		}}},
	}, map[string]any{"x": 1})
	if err == nil {
		t.Fatalf("foreach in: with input.* -- error = nil, want a sandbox error")
	}
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassCELInterp {
		t.Fatalf("error = %v, want *EvalError ClassCELInterp", err)
	}
}

// --- 4. Atomicity / forward-only at the core level ---------------------------

// TestApply_FailedOpLeavesInputUntouched — an operation error in the middle of a step does NOT
// mutate the caller's input state (deep-copy on Apply entry) and returns a
// zero Result (FinalState/Steps are empty). This is the atomicity guarantee
// available to the core: the transactional layer on top performs a ROLLBACK on this error.
func TestApply_FailedOpLeavesInputUntouched(t *testing.T) {
	ev := mustEvaluator(t)
	in := map[string]any{"a": 1, "b": 2}

	chain := Chain{{FromVersion: 1, ToVersion: 2, Transform: []Op{
		{Set: &SetOp{Path: "state.c", Value: 3}},            // successful mutation…
		{Rename: &RenameOp{From: "state.a", To: "state.b"}}, // …then error: to already exists
	}}}

	res, err := Apply(context.Background(), in, chain, ev)
	if err == nil {
		t.Fatalf("error = nil, want ClassRenameToExists")
	}
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassRenameToExists {
		t.Fatalf("error = %v, want ClassRenameToExists", err)
	}
	// Input state is untouched (including the partial mutation of state.c from the first op).
	if len(in) != 2 || in["a"] != 1 || in["b"] != 2 {
		t.Fatalf("input state mutated on error: %#v", in)
	}
	// Result is zero-valued (the core doesn't return a partial result).
	if res.FinalState != nil || res.Steps != nil {
		t.Fatalf("Result not zero on error: %#v", res)
	}
}

// TestApply_LaterStepFailureDiscardsEarlierStep — an error in the chain's second step
// doesn't leave a partially applied result (FinalState is empty), even though the first
// step succeeded on its own. Forward-only: recovery happens via state_history
// in the transactional layer, not via a partial return from the core.
func TestApply_LaterStepFailureDiscardsEarlierStep(t *testing.T) {
	ev := mustEvaluator(t)
	in := map[string]any{"v": 1}

	chain := Chain{
		{FromVersion: 1, ToVersion: 2, Transform: []Op{
			{Set: &SetOp{Path: "state.step1", Value: "ok"}},
		}},
		{FromVersion: 2, ToVersion: 3, Transform: []Op{
			{Set: &SetOp{Path: "state.bad", Value: "${ now() }"}}, // sandbox error
		}},
	}

	res, err := Apply(context.Background(), in, chain, ev)
	if err == nil {
		t.Fatalf("error = nil, want a sandbox error on the second step")
	}
	if res.FinalState != nil || res.Steps != nil {
		t.Fatalf("partial Result on step-2 error: %#v", res)
	}
	if len(in) != 1 || in["v"] != 1 {
		t.Fatalf("input state mutated: %#v", in)
	}
}

// --- 5. Edge: deep nesting and conflicts ------------------------------

// TestRename_DeepNestedPaths — rename between deeply nested paths: the value
// is moved along with its whole structure, the source is removed, target intermediate
// maps are created.
func TestRename_DeepNestedPaths(t *testing.T) {
	out := mustApply(t, []Op{
		{Rename: &RenameOp{From: "state.a.b.c.d", To: "state.x.y.z"}},
	}, map[string]any{
		"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": "moved"}}},
	})
	x, _ := out["x"].(map[string]any)
	y, _ := x["y"].(map[string]any)
	if y["z"] != "moved" {
		t.Fatalf("state.x.y.z = %v, want moved", y["z"])
	}
	// Source — the remaining empty parent map (deletePath only removes the
	// leaf d, it doesn't collapse empty parents — this documents actual behavior).
	a, _ := out["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	c, _ := b["c"].(map[string]any)
	if _, ok := c["d"]; ok {
		t.Fatalf("source state.a.b.c.d not deleted: %#v", c)
	}
}

// TestRename_ToExistsNested — rename into a nested already-existing to → error
// ClassRenameToExists (target key conflict at depth).
func TestRename_ToExistsNested(t *testing.T) {
	_, err := apply(t, []Op{
		{Rename: &RenameOp{From: "state.src", To: "state.dst.inner"}},
	}, map[string]any{
		"src": "v",
		"dst": map[string]any{"inner": "occupied"},
	})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassRenameToExists {
		t.Fatalf("error = %v, want ClassRenameToExists", err)
	}
}

// TestSet_TypeMismatchOverwritesMapWithScalar — set with a scalar at a path that currently
// holds a map: the target leaf is overwritten entirely (set on a LEAF is always an overwrite,
// type mismatch is NOT an error — unlike an intermediate segment).
func TestSet_TypeMismatchOverwritesMapWithScalar(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.obj", Value: "scalar"}},
	}, map[string]any{"obj": map[string]any{"was": "map"}})
	if out["obj"] != "scalar" {
		t.Fatalf("obj = %#v, want a map overwritten by a scalar", out["obj"])
	}
}

// TestSet_DeepNestedThroughExistingMaps — set of a deeply nested leaf through
// already-existing intermediate maps (navigation, not creation from scratch).
func TestSet_DeepNestedThroughExistingMaps(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.a.b.c.new", Value: "added"}},
	}, map[string]any{
		"a": map[string]any{"b": map[string]any{"c": map[string]any{"old": "kept"}}},
	})
	a, _ := out["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	c, _ := b["c"].(map[string]any)
	if c["new"] != "added" || c["old"] != "kept" {
		t.Fatalf("state.a.b.c = %#v, want {old:kept, new:added}", c)
	}
}
