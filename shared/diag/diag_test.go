package diag

import (
	"encoding/json"
	"testing"
)

func TestDiagnostic_JSONRoundTrip(t *testing.T) {
	in := Diagnostic{
		Level:    LevelError,
		Phase:    PhaseSchemaValidate,
		File:     "keeper.yml",
		Line:     12,
		Column:   3,
		Code:     "unknown_key",
		Message:  `unknown field "reactor"`,
		Hint:     "remove the key",
		YAMLPath: "$.reactor",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Diagnostic
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if in != out {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
}

func TestDiagnostic_OmitEmpty(t *testing.T) {
	in := Diagnostic{
		Level:   LevelError,
		Phase:   PhaseParse,
		Code:    "yaml_parse_error",
		Message: "boom",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"level":"error","phase":"parse","code":"yaml_parse_error","message":"boom"}`
	if got != want {
		t.Fatalf("omitempty mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestHasErrors(t *testing.T) {
	if HasErrors(nil) {
		t.Fatal("nil should not have errors")
	}
	if HasErrors([]Diagnostic{{Level: LevelWarning}}) {
		t.Fatal("warning-only should not have errors")
	}
	if !HasErrors([]Diagnostic{{Level: LevelWarning}, {Level: LevelError}}) {
		t.Fatal("expected to find error")
	}
}
