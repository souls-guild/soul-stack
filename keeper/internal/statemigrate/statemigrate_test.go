package statemigrate

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func mustEvaluator(t *testing.T) Evaluator {
	t.Helper()
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev
}

// TestApply_EmptyForeachNoMaterialize: engine invariant — foreach over empty
// list doesn't create the key by itself (no-op without body). Checked on a synthetic
// migration WITHOUT a prior set: the subject is the engine, not a service's intent.
func TestApply_EmptyForeachNoMaterialize(t *testing.T) {
	ev := mustEvaluator(t)
	mig := &Migration{FromVersion: 1, ToVersion: 2, Transform: []Op{
		{Rename: &RenameOp{From: "state.redis_users", To: "state.redis_users_legacy_v1"}},
		{Foreach: &ForeachOp{In: "${ state.redis_users_legacy_v1 }", As: "user_name", Do: []Op{
			{Set: &SetOp{Path: "state.redis_users.${ user_name }", Value: map[string]any{"perms": "x"}}},
		}}},
		{Delete: &DeleteOp{Path: "state.redis_users_legacy_v1"}},
	}}

	in := map[string]any{"redis_users": []any{}, "redis_type": "cluster"}
	res, err := Apply(context.Background(), in, Chain{mig}, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertDeepEqualJSON(t, res.FinalState, map[string]any{"redis_type": "cluster"})
}

// TestApply_DoesNotMutateInput: caller's input state is not mutated. The chain is
// synthetic and must stay so: the guard is only as wide as the set of writing
// operations the chain exercises, and a corpus ladder only happens to cover them.
func TestApply_DoesNotMutateInput(t *testing.T) {
	ev := mustEvaluator(t)
	mig := &Migration{FromVersion: 1, ToVersion: 2, Transform: []Op{
		{Rename: &RenameOp{From: "state.redis_users", To: "state.redis_users_legacy_v1"}},
		{Set: &SetOp{Path: "state.redis_users", Value: map[string]any{}}},
		{Foreach: &ForeachOp{In: "${ state.redis_users_legacy_v1 }", As: "user_name", Do: []Op{
			{Set: &SetOp{Path: "state.redis_users.${ user_name }", Value: map[string]any{"perms": "x"}}},
		}}},
		{Delete: &DeleteOp{Path: "state.redis_users_legacy_v1"}},
	}}

	in := map[string]any{"redis_users": []any{"app"}, "redis_type": "standalone"}
	snapshot := map[string]any{"redis_users": []any{"app"}, "redis_type": "standalone"}

	if _, err := Apply(context.Background(), in, Chain{mig}, ev); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input state mutated: %#v", in)
	}
}

// TestApply_StepSnapshots: before/after snapshot per chain step.
func TestApply_StepSnapshots(t *testing.T) {
	ev := mustEvaluator(t)
	chain := Chain{
		{FromVersion: 1, ToVersion: 2, Transform: []Op{
			{Set: &SetOp{Path: "state.a", Value: 1}},
		}},
		{FromVersion: 2, ToVersion: 3, Transform: []Op{
			{Set: &SetOp{Path: "state.b", Value: 2}},
		}},
	}
	res, err := Apply(context.Background(), map[string]any{}, chain, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(res.Steps))
	}
	if _, ok := res.Steps[0].StateBefore["a"]; ok {
		t.Errorf("step0.StateBefore should not contain a")
	}
	if res.Steps[0].StateAfter["a"] != float64(1) && res.Steps[0].StateAfter["a"] != 1 {
		t.Errorf("step0.StateAfter[a] = %v", res.Steps[0].StateAfter["a"])
	}
	if res.Steps[1].FromVersion != 2 || res.Steps[1].ToVersion != 3 {
		t.Errorf("step1 versions = %d->%d", res.Steps[1].FromVersion, res.Steps[1].ToVersion)
	}
}

// TestApply_ChainVersionGap: version gap in chain → error.
func TestApply_ChainVersionGap(t *testing.T) {
	ev := mustEvaluator(t)
	chain := Chain{
		{FromVersion: 1, ToVersion: 2},
		{FromVersion: 3, ToVersion: 4}, // gap: 2 != 3
	}
	_, err := Apply(context.Background(), map[string]any{}, chain, ev)
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassChainVersion {
		t.Fatalf("error = %v, want ClassChainVersion", err)
	}
}

func assertDeepEqualJSON(t *testing.T, got, want map[string]any) {
	t.Helper()
	// Normalize numeric types through JSON round-trip (YAML int vs Apply
	// preserves cel int64 — compare in unified form).
	if !reflect.DeepEqual(normalizeJSON(t, got), normalizeJSON(t, want)) {
		t.Errorf("state mismatch:\n got = %#v\nwant = %#v", got, want)
	}
}

func normalizeJSON(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	return deepCopyMap(m)
}
