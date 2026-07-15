package incarnation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- TraitsFromSpec (bridge incarnation.spec.traits → incarnation.traits) ---

func TestTraitsFromSpec_NilSpec(t *testing.T) {
	got, err := TraitsFromSpec(nil)
	if err != nil {
		t.Fatalf("TraitsFromSpec(nil): %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestTraitsFromSpec_NoTraitsKey(t *testing.T) {
	got, err := TraitsFromSpec(map[string]any{"input": map[string]any{"x": 1}})
	if err != nil {
		t.Fatalf("TraitsFromSpec: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil (traits не заданы)", got)
	}
}

func TestTraitsFromSpec_EmptyMap(t *testing.T) {
	got, err := TraitsFromSpec(map[string]any{"traits": map[string]any{}})
	if err != nil {
		t.Fatalf("TraitsFromSpec: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil (пустой traits = не задано)", got)
	}
}

func TestTraitsFromSpec_ScalarAndList(t *testing.T) {
	spec := map[string]any{
		"traits": map[string]any{
			"namespace": "dba-ns",
			"owners":    []any{"alice", "bob"},
		},
	}
	got, err := TraitsFromSpec(spec)
	if err != nil {
		t.Fatalf("TraitsFromSpec: %v", err)
	}
	if got["namespace"] != "dba-ns" {
		t.Errorf("namespace = %v", got["namespace"])
	}
	owners, ok := got["owners"].([]any)
	if !ok || len(owners) != 2 || owners[0] != "alice" {
		t.Errorf("owners = %v", got["owners"])
	}
}

func TestTraitsFromSpec_RejectsNonObject(t *testing.T) {
	_, err := TraitsFromSpec(map[string]any{"traits": "not-a-map"})
	if err == nil {
		t.Fatal("TraitsFromSpec(traits=string) returned nil")
	}
}

func TestTraitsFromSpec_RejectsInvalidValue(t *testing.T) {
	// a nested-map Trait value is not allowed (scalar|list only) — ValidateTraitDelta.
	_, err := TraitsFromSpec(map[string]any{
		"traits": map[string]any{"bad": map[string]any{"nested": 1}},
	})
	if err == nil {
		t.Fatal("TraitsFromSpec(nested value) returned nil")
	}
}

// --- Create: the traits arg reaches INSERT ($11) ---

// TestCreate_TraitsPassedThrough — incarnation.Traits is serialized into the
// jsonb arg $11 (index 10) of INSERT (round trip of the Trait source of
// truth, ADR-060 amend R1).
func TestCreate_TraitsPassedThrough(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		Traits: map[string]any{"team": "dba"},
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, ok := f.queryRowArgs[10].([]byte)
	if !ok {
		t.Fatalf("args[10] traits = %T, want []byte", f.queryRowArgs[10])
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("traits not JSON: %v", err)
	}
	if got["team"] != "dba" {
		t.Errorf("traits arg = %v, want team=dba", got)
	}
}

// TestCreate_NilTraitsBecomesEmptyObject — Traits=nil → `{}` (NOT NULL DEFAULT,
// the projection path doesn't distinguish "no column" / "no labels").
func TestCreate_NilTraitsBecomesEmptyObject(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		// Traits nil
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s, _ := f.queryRowArgs[10].([]byte); string(s) != "{}" {
		t.Errorf("traits bytes = %s, want \"{}\"", s)
	}
}

// --- SyncTraitsToHosts: invalid incarnation name ---

func TestSyncTraitsToHosts_RejectsInvalidName(t *testing.T) {
	err := SyncTraitsToHosts(context.Background(), nil, "Bad_Name", map[string]any{"team": "dba"})
	if err == nil {
		t.Fatal("SyncTraitsToHosts(invalid name) returned nil")
	}
}

// --- UpdateTraits: name-guard + traitKeys ---

// TestUpdateTraits_RejectsInvalidName — an invalid name is rejected BEFORE
// BeginTx (pool=nil is safe here: the name check comes first).
func TestUpdateTraits_RejectsInvalidName(t *testing.T) {
	_, err := UpdateTraits(context.Background(), nil, "Bad_Name", map[string]any{"team": "dba"})
	if err == nil {
		t.Fatal("UpdateTraits(invalid name) returned nil")
	}
}

// TestTraitKeys — a sorted nil-safe key set for the audit payload.
func TestTraitKeys(t *testing.T) {
	got := traitKeys(map[string]any{"team": "dba", "env": "prod", "az": "a"})
	want := []string{"az", "env", "team"}
	if len(got) != len(want) {
		t.Fatalf("traitKeys len = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("traitKeys[%d] = %q, want %q (sorted)", i, got[i], want[i])
		}
	}
	// nil / empty → an empty slice (not nil), stable JSON.
	if empty := traitKeys(nil); empty == nil || len(empty) != 0 {
		t.Errorf("traitKeys(nil) = %v, want non-nil empty slice", empty)
	}
}
