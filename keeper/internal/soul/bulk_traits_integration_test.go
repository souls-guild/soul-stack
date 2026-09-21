//go:build integration

// Integration tests for bulk trait-assign (BulkAssignTraits / BulkReplaceTraits)
// against real Postgres (jsonb operators ||/-/?|, keyset chunking, idempotency,
// scope-intersection — SQL-driven, can't be verified on a fake pool). Use the
// shared integration_test.go harness (integrationPool / resetAll) and
// seedBulkSoul/equalStr from bulk_coven_integration_test.go.

package soul

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// seedTraitSoul inserts a host with the given traits set (status pending).
func seedTraitSoul(t *testing.T, sid string, traits map[string]any) {
	t.Helper()
	s := &Soul{SID: sid, Status: StatusPending, Coven: []string{"dev"}, Traits: traits}
	if err := Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedTraitSoul(%s): %v", sid, err)
	}
}

func traitsOf(t *testing.T, sid string) map[string]any {
	t.Helper()
	got, err := SelectBySID(context.Background(), integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID(%s): %v", sid, err)
	}
	return got.Traits
}

// TestIntegration_BulkTraits_Merge_OverwritesAndKeeps — merge overwrites the
// given keys, keeps the rest.
func TestIntegration_BulkTraits_Merge_OverwritesAndKeeps(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedTraitSoul(t, "a.example.com", map[string]any{"namespace": "old", "keep": "yes"})

	rep, err := BulkAssignTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitMerge,
		map[string]any{"namespace": "new", "tier": float64(1)}, nil)
	if err != nil {
		t.Fatalf("BulkAssignTraits merge: %v", err)
	}
	if rep.Status != BulkCompleted || rep.Matched != 1 || rep.Changed != 1 {
		t.Errorf("rep = %+v, want completed/1/1", rep)
	}
	got := traitsOf(t, "a.example.com")
	if got["namespace"] != "new" {
		t.Errorf("namespace = %v, want new (overwritten)", got["namespace"])
	}
	if got["keep"] != "yes" {
		t.Errorf("keep = %v, want yes (preserved)", got["keep"])
	}
	if got["tier"] != float64(1) {
		t.Errorf("tier = %v, want 1 (added)", got["tier"])
	}

	// Idem: repeating the same merge → 0 changed.
	rep2, err := BulkAssignTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitMerge,
		map[string]any{"namespace": "new", "tier": float64(1)}, nil)
	if err != nil {
		t.Fatalf("BulkAssignTraits merge repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0 (idem-merge)", rep2.Changed)
	}
}

// TestIntegration_BulkTraits_Merge_ListValue — a list-of-scalars value is
// serialized as a jsonb array and reads back correctly.
func TestIntegration_BulkTraits_Merge_ListValue(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedTraitSoul(t, "a.example.com", nil)

	_, err := BulkAssignTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitMerge,
		map[string]any{"tags": []any{"web", "edge"}}, nil)
	if err != nil {
		t.Fatalf("BulkAssignTraits: %v", err)
	}
	got := traitsOf(t, "a.example.com")
	tags, ok := got["tags"].([]any)
	if !ok || !reflect.DeepEqual(tags, []any{"web", "edge"}) {
		t.Errorf("tags = %#v, want [web edge]", got["tags"])
	}
}

// TestIntegration_BulkTraits_Remove — remove deletes the given keys, ignores
// absent ones; idem on repeat.
func TestIntegration_BulkTraits_Remove(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedTraitSoul(t, "a.example.com", map[string]any{"namespace": "dba", "drop": "x", "keep": "y"})

	rep, err := BulkAssignTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitRemove, nil, []string{"drop", "missing"})
	if err != nil {
		t.Fatalf("BulkAssignTraits remove: %v", err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1 (only 'drop' present)", rep.Changed)
	}
	got := traitsOf(t, "a.example.com")
	if _, has := got["drop"]; has {
		t.Errorf("drop still present after remove: %v", got)
	}
	if got["namespace"] != "dba" || got["keep"] != "y" {
		t.Errorf("non-removed keys mutated: %v", got)
	}

	rep2, err := BulkAssignTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitRemove, nil, []string{"drop"})
	if err != nil {
		t.Fatalf("remove repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0 (idem-remove)", rep2.Changed)
	}
}

// TestIntegration_BulkTraits_Replace — replace sets the map exactly, dropping
// existing keys; an empty map clears it.
func TestIntegration_BulkTraits_Replace(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedTraitSoul(t, "a.example.com", map[string]any{"old": "1", "gone": "2"})

	rep, err := BulkReplaceTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, map[string]any{"namespace": "fresh"})
	if err != nil {
		t.Fatalf("BulkReplaceTraits: %v", err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1", rep.Changed)
	}
	got := traitsOf(t, "a.example.com")
	if !reflect.DeepEqual(got, map[string]any{"namespace": "fresh"}) {
		t.Errorf("traits = %#v, want {namespace:fresh} (old keys dropped)", got)
	}

	// Empty replace = clear.
	repEmpty, err := BulkReplaceTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, map[string]any{})
	if err != nil {
		t.Fatalf("BulkReplaceTraits empty: %v", err)
	}
	if repEmpty.Changed != 1 {
		t.Errorf("empty replace changed = %d, want 1", repEmpty.Changed)
	}
	if got := traitsOf(t, "a.example.com"); len(got) != 0 {
		t.Errorf("traits = %#v, want empty after empty replace", got)
	}
}

// TestIntegration_BulkTraits_Scope_HostsSubset — gate (a) least-privilege: a
// coven-scoped operator doesn't touch traits of hosts outside scope even with
// all=true.
func TestIntegration_BulkTraits_Scope_HostsSubset(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// dev-host is in scope, prod-host is outside (its coven is set explicitly).
	seedTraitSoul(t, "dev-host.example.com", map[string]any{"x": "1"})
	prod := &Soul{SID: "prod-host.example.com", Status: StatusPending, Coven: []string{"prod"}, Traits: map[string]any{"x": "1"}}
	if err := Insert(ctx, integrationPool, prod); err != nil {
		t.Fatalf("seed prod: %v", err)
	}

	rep, err := BulkAssignTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, TraitMerge, map[string]any{"x": "2"}, nil)
	if err != nil {
		t.Fatalf("BulkAssignTraits: %v", err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (prod-host out of scope)", rep.Matched)
	}
	if got := traitsOf(t, "prod-host.example.com"); got["x"] != "1" {
		t.Errorf("prod-host traits mutated despite out-of-scope: %v", got)
	}
	if got := traitsOf(t, "dev-host.example.com"); got["x"] != "2" {
		t.Errorf("dev-host not mutated in scope: %v", got)
	}
}

// TestIntegration_BulkTraits_Replace_Scope_HostsSubset — gate (a) on replace.
func TestIntegration_BulkTraits_Replace_Scope_HostsSubset(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedTraitSoul(t, "dev-host.example.com", map[string]any{"x": "1"})
	prod := &Soul{SID: "prod-host.example.com", Status: StatusPending, Coven: []string{"prod"}, Traits: map[string]any{"x": "1"}}
	if err := Insert(ctx, integrationPool, prod); err != nil {
		t.Fatalf("seed prod: %v", err)
	}

	rep, err := BulkReplaceTraits(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, map[string]any{"y": "z"})
	if err != nil {
		t.Fatalf("BulkReplaceTraits: %v", err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (prod out of scope)", rep.Matched)
	}
	if got := traitsOf(t, "prod-host.example.com"); !reflect.DeepEqual(got, map[string]any{"x": "1"}) {
		t.Errorf("prod-host traits replaced despite out-of-scope: %v", got)
	}
}

func TestIntegration_BulkTraits_EmptySelector(t *testing.T) {
	resetAll(t)
	_, err := BulkAssignTraits(context.Background(), integrationPool, BulkSelector{},
		BulkScope{Unrestricted: true}, TraitMerge, map[string]any{"k": "v"}, nil)
	if !errors.Is(err, ErrBulkEmptySelector) {
		t.Errorf("err = %v, want ErrBulkEmptySelector", err)
	}
}
