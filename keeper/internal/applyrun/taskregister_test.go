package applyrun

import (
	"context"
	"strings"
	"testing"
)

func TestUpsertTaskRegister_HappyPath(t *testing.T) {
	f := &fakeDB{}
	tr := &TaskRegister{
		ApplyID:      "01HAPPLY",
		SID:          "host.example.com",
		PlanIndex:    5,
		TaskIdx:      2,
		RegisterData: map[string]any{"stdout": "leader", "rc": float64(0)},
		Passage:      1,
	}
	if err := UpsertTaskRegister(context.Background(), f, tr); err != nil {
		t.Fatalf("UpsertTaskRegister: %v", err)
	}
	if f.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.execSQL, "INSERT INTO apply_task_register") || !strings.Contains(f.execSQL, "ON CONFLICT") {
		t.Errorf("SQL is not upsert: %q", f.execSQL)
	}
	// args: apply_id($1), sid($2), plan_index($3), task_idx($4), register_data($5),
	// passage($6) — plan_index correlation key (migration 079).
	if len(f.execArgs) != 6 {
		t.Fatalf("args len = %d, want 6", len(f.execArgs))
	}
	if f.execArgs[0] != "01HAPPLY" || f.execArgs[1] != "host.example.com" || f.execArgs[2] != 5 || f.execArgs[3] != 2 {
		t.Errorf("args[0..3] = %v / %v / %v / %v", f.execArgs[0], f.execArgs[1], f.execArgs[2], f.execArgs[3])
	}
	// register_data is serialized into jsonb bytes.
	if _, ok := f.execArgs[4].([]byte); !ok {
		t.Errorf("args[4] register_data = %T, want []byte (jsonb)", f.execArgs[4])
	}
	// passage (ADR-056) is the FK component for apply_runs(apply_id, sid, passage).
	if f.execArgs[5] != 1 {
		t.Errorf("args[5] passage = %v, want 1", f.execArgs[5])
	}
}

func TestUpsertTaskRegister_EmptyDataNoop(t *testing.T) {
	for _, tr := range []*TaskRegister{
		{ApplyID: "a", SID: "s", TaskIdx: 0, RegisterData: nil},
		{ApplyID: "a", SID: "s", TaskIdx: 0, RegisterData: map[string]any{}},
	} {
		f := &fakeDB{}
		if err := UpsertTaskRegister(context.Background(), f, tr); err != nil {
			t.Fatalf("UpsertTaskRegister: %v", err)
		}
		if f.execCalls != 0 {
			t.Errorf("empty register_data called Exec (calls=%d)", f.execCalls)
		}
	}
}

func TestUpsertTaskRegister_RejectsBadInput(t *testing.T) {
	cases := []*TaskRegister{
		nil,
		{ApplyID: "", SID: "s", TaskIdx: 0, RegisterData: map[string]any{"k": "v"}},
		{ApplyID: "a", SID: "", TaskIdx: 0, RegisterData: map[string]any{"k": "v"}},
		{ApplyID: "a", SID: "s", TaskIdx: -1, RegisterData: map[string]any{"k": "v"}},
	}
	for i, tr := range cases {
		f := &fakeDB{}
		if err := UpsertTaskRegister(context.Background(), f, tr); err == nil {
			t.Errorf("case %d: UpsertTaskRegister returned nil for invalid input", i)
		}
		if f.execCalls != 0 {
			t.Errorf("case %d: execCalls = %d, want 0", i, f.execCalls)
		}
	}
}
