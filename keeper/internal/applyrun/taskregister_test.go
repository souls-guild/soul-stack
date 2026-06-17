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
		TaskIdx:      2,
		RegisterData: map[string]any{"stdout": "leader", "rc": float64(0)},
	}
	if err := UpsertTaskRegister(context.Background(), f, tr); err != nil {
		t.Fatalf("UpsertTaskRegister: %v", err)
	}
	if f.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.execSQL, "INSERT INTO apply_task_register") || !strings.Contains(f.execSQL, "ON CONFLICT") {
		t.Errorf("SQL не upsert: %q", f.execSQL)
	}
	if len(f.execArgs) != 4 {
		t.Fatalf("args len = %d, want 4", len(f.execArgs))
	}
	if f.execArgs[0] != "01HAPPLY" || f.execArgs[1] != "host.example.com" || f.execArgs[2] != 2 {
		t.Errorf("args[0..2] = %v / %v / %v", f.execArgs[0], f.execArgs[1], f.execArgs[2])
	}
	// register_data сериализуется в jsonb-байты.
	if _, ok := f.execArgs[3].([]byte); !ok {
		t.Errorf("args[3] register_data = %T, want []byte (jsonb)", f.execArgs[3])
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
			t.Errorf("пустой register_data вызвал Exec (calls=%d)", f.execCalls)
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
			t.Errorf("case %d: UpsertTaskRegister вернул nil для невалидного входа", i)
		}
		if f.execCalls != 0 {
			t.Errorf("case %d: execCalls = %d, want 0", i, f.execCalls)
		}
	}
}
