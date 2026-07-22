package audit

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestFormatDiagnostics_Nil(t *testing.T) {
	if got := FormatDiagnostics(nil); got != nil {
		t.Errorf("FormatDiagnostics(nil) = %v, want nil", got)
	}
	if got := FormatDiagnostics([]diag.Diagnostic{}); got != nil {
		t.Errorf("FormatDiagnostics(empty) = %v, want nil", got)
	}
}

func TestFormatDiagnostics_MandatoryKeys(t *testing.T) {
	in := []diag.Diagnostic{{
		Level:   diag.LevelError,
		Phase:   diag.PhaseSchemaValidate,
		Code:    "unknown_key",
		Message: "unknown top-level key",
	}}
	got := FormatDiagnostics(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	want := map[string]any{
		"code":    "unknown_key",
		"message": "unknown top-level key",
		"phase":   "schema_validate",
		"level":   "error",
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("entry = %#v, want %#v", got[0], want)
	}
}

func TestFormatDiagnostics_OptionalKeysOmittedWhenZero(t *testing.T) {
	in := []diag.Diagnostic{{
		Level:   diag.LevelError,
		Phase:   diag.PhaseParse,
		Code:    "io_error",
		Message: "no such file",
	}}
	got := FormatDiagnostics(in)
	if _, ok := got[0]["yaml_path"]; ok {
		t.Errorf("yaml_path included when empty: %#v", got[0])
	}
	if _, ok := got[0]["line"]; ok {
		t.Errorf("line included when zero: %#v", got[0])
	}
	if _, ok := got[0]["column"]; ok {
		t.Errorf("column included when zero: %#v", got[0])
	}
}

func TestFormatDiagnostics_OptionalKeysPresentWhenSet(t *testing.T) {
	in := []diag.Diagnostic{{
		Level:    diag.LevelError,
		Phase:    diag.PhaseSemanticValidate,
		Code:     "kid_invalid",
		Message:  "kid does not match pattern",
		YAMLPath: "$.kid",
		Line:     1,
		Column:   6,
	}}
	got := FormatDiagnostics(in)
	if got[0]["yaml_path"] != "$.kid" {
		t.Errorf("yaml_path = %v, want $.kid", got[0]["yaml_path"])
	}
	if got[0]["line"] != 1 {
		t.Errorf("line = %v, want 1", got[0]["line"])
	}
	if got[0]["column"] != 6 {
		t.Errorf("column = %v, want 6", got[0]["column"])
	}
}

func TestFormatDiagnostics_PreservesOrder(t *testing.T) {
	in := []diag.Diagnostic{
		{Level: diag.LevelError, Phase: diag.PhaseParse, Code: "first", Message: "1"},
		{Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate, Code: "second", Message: "2"},
		{Level: diag.LevelError, Phase: diag.PhaseSemanticValidate, Code: "third", Message: "3"},
	}
	got := FormatDiagnostics(in)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i]["code"] != want {
			t.Errorf("got[%d].code = %v, want %s", i, got[i]["code"], want)
		}
	}
}
