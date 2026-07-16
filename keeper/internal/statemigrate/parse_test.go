package statemigrate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func parseErr(t *testing.T, src string) *ParseError {
	t.Helper()
	_, err := Parse([]byte(src))
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("ожидался *ParseError, получено: %v", err)
	}
	return pe
}

// TestParse_RealFixture — a real migration file parses without errors and yields
// the expected operation shape.
func TestParse_RealFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureDir, "001_to_002.yml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	mig, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if mig.FromVersion != 1 || mig.ToVersion != 2 {
		t.Fatalf("версии = %d→%d", mig.FromVersion, mig.ToVersion)
	}
	if len(mig.Transform) != 4 {
		t.Fatalf("операций = %d, want 4", len(mig.Transform))
	}
	if mig.Transform[0].Rename == nil {
		t.Errorf("op0 не rename")
	}
	if mig.Transform[1].Set == nil {
		t.Errorf("op1 не set (материализация целевого map)")
	}
	if mig.Transform[2].Foreach == nil {
		t.Errorf("op2 не foreach")
	} else {
		fe := mig.Transform[2].Foreach
		if fe.As != "user_name" || len(fe.Do) != 1 || fe.Do[0].Set == nil {
			t.Errorf("foreach форма = %#v", fe)
		}
	}
	if mig.Transform[3].Delete == nil {
		t.Errorf("op3 не delete")
	}
}

func TestParse_Empty(t *testing.T) {
	if pe := parseErr(t, ""); pe.Code != CodeEmptyDocument {
		t.Fatalf("code = %s, want %s", pe.Code, CodeEmptyDocument)
	}
}

func TestParse_VersionMissing(t *testing.T) {
	if pe := parseErr(t, "from_version: 1\ntransform: []\n"); pe.Code != CodeVersionMissing {
		t.Fatalf("code = %s, want %s", pe.Code, CodeVersionMissing)
	}
}

func TestParse_VersionNotConsecutive(t *testing.T) {
	if pe := parseErr(t, "from_version: 1\nto_version: 3\ntransform: []\n"); pe.Code != CodeVersionInvalid {
		t.Fatalf("code = %s, want %s", pe.Code, CodeVersionInvalid)
	}
}

func TestParse_NoDiscriminator(t *testing.T) {
	src := "from_version: 1\nto_version: 2\ntransform:\n  - foo: bar\n"
	if pe := parseErr(t, src); pe.Code != CodeOpDiscriminator {
		t.Fatalf("code = %s, want %s", pe.Code, CodeOpDiscriminator)
	}
}

func TestParse_MultipleDiscriminators(t *testing.T) {
	src := "from_version: 1\nto_version: 2\ntransform:\n  - delete: { path: state.a }\n    set: { path: state.b, value: 1 }\n"
	if pe := parseErr(t, src); pe.Code != CodeOpDiscriminator {
		t.Fatalf("code = %s, want %s", pe.Code, CodeOpDiscriminator)
	}
}

func TestParse_SetMissingPath(t *testing.T) {
	src := "from_version: 1\nto_version: 2\ntransform:\n  - set: { value: 1 }\n"
	if pe := parseErr(t, src); pe.Code != CodeOpFieldMissing {
		t.Fatalf("code = %s, want %s", pe.Code, CodeOpFieldMissing)
	}
}

func TestParse_ForeachMissingAs(t *testing.T) {
	src := "from_version: 1\nto_version: 2\ntransform:\n  - foreach: \"${ state.x }\"\n    do:\n      - delete: { path: state.y }\n"
	if pe := parseErr(t, src); pe.Code != CodeForeachMissingAs {
		t.Fatalf("code = %s, want %s", pe.Code, CodeForeachMissingAs)
	}
}

func TestParse_ForeachStructuralForm(t *testing.T) {
	src := "from_version: 1\nto_version: 2\ntransform:\n  - foreach:\n      in: \"${ state.x }\"\n      as: it\n      do:\n        - delete: { path: state.y }\n"
	mig, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fe := mig.Transform[0].Foreach
	if fe == nil || fe.In != "${ state.x }" || fe.As != "it" || len(fe.Do) != 1 {
		t.Fatalf("структурный foreach = %#v", fe)
	}
}
