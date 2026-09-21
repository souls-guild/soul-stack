package statemigrate

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// stepPath is the address a parse error carries in these tests — a real step
// document path, since the parser now quotes it back when refusing a retired header.
const stepPath = "migrations/002_demo_step/" + config.MigrationStepFile

func parseErr(t *testing.T, src string) *ParseError {
	t.Helper()
	_, err := Parse([]byte(src), 2, stepPath)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ParseError, got: %v", err)
	}
	return pe
}

func TestParse_Empty(t *testing.T) {
	if pe := parseErr(t, ""); pe.Code != CodeEmptyDocument {
		t.Fatalf("code = %s, want %s", pe.Code, CodeEmptyDocument)
	}
}

// TestParse_RejectsVersionHeader — a step that states its own place is REFUSED,
// with the address of the document that stated it (NIM-735).
//
// Refused rather than ignored on purpose. A silently-dropped `to_version: 3` inside
// `migrations/002_.../main.yml` is the exact defect the layout removed: two records
// of one integer, free to disagree, with the loser invisible. The guard covers each
// key alone as well as the pair, because a half-migrated file carries only one.
func TestParse_RejectsVersionHeader(t *testing.T) {
	for _, src := range []string{
		"from_version: 1\ntransform: []\n",
		"to_version: 2\ntransform: []\n",
		"from_version: 1\nto_version: 3\ntransform: []\n",
	} {
		pe := parseErr(t, src)
		if pe.Code != CodeVersionKey {
			t.Fatalf("src %q: code = %s, want %s", src, pe.Code, CodeVersionKey)
		}
		if !strings.Contains(pe.Msg, stepPath) {
			t.Fatalf("src %q: message %q does not name the step document", src, pe.Msg)
		}
	}
}

// TestParse_AcceptsStepWithoutHeader — the same document without the header parses,
// and takes its place from the argument. The other half of the guard above: without
// it, a Parse that refused everything would pass.
func TestParse_AcceptsStepWithoutHeader(t *testing.T) {
	mig, err := Parse([]byte("description: x\ntransform: []\n"), 7, stepPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if mig.FromVersion != 6 || mig.ToVersion != 7 {
		t.Fatalf("versions = %d→%d, want 6→7", mig.FromVersion, mig.ToVersion)
	}
}

func TestParse_NoDiscriminator(t *testing.T) {
	src := "transform:\n  - foo: bar\n"
	if pe := parseErr(t, src); pe.Code != CodeOpDiscriminator {
		t.Fatalf("code = %s, want %s", pe.Code, CodeOpDiscriminator)
	}
}

func TestParse_MultipleDiscriminators(t *testing.T) {
	src := "transform:\n  - delete: { path: state.a }\n    set: { path: state.b, value: 1 }\n"
	if pe := parseErr(t, src); pe.Code != CodeOpDiscriminator {
		t.Fatalf("code = %s, want %s", pe.Code, CodeOpDiscriminator)
	}
}

func TestParse_SetMissingPath(t *testing.T) {
	src := "transform:\n  - set: { value: 1 }\n"
	if pe := parseErr(t, src); pe.Code != CodeOpFieldMissing {
		t.Fatalf("code = %s, want %s", pe.Code, CodeOpFieldMissing)
	}
}

func TestParse_ForeachMissingAs(t *testing.T) {
	src := "transform:\n  - foreach: \"${ state.x }\"\n    do:\n      - delete: { path: state.y }\n"
	if pe := parseErr(t, src); pe.Code != CodeForeachMissingAs {
		t.Fatalf("code = %s, want %s", pe.Code, CodeForeachMissingAs)
	}
}

func TestParse_ForeachStructuralForm(t *testing.T) {
	src := "transform:\n  - foreach:\n      in: \"${ state.x }\"\n      as: it\n      do:\n        - delete: { path: state.y }\n"
	mig, err := Parse([]byte(src), 2, stepPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fe := mig.Transform[0].Foreach
	if fe == nil || fe.In != "${ state.x }" || fe.As != "it" || len(fe.Do) != 1 {
		t.Fatalf("structural foreach = %#v", fe)
	}
}
