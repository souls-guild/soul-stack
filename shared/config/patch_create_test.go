package config

import (
	"errors"
	"strings"
	"testing"
)

// docFrom builds a parse-only Document over an inline YAML fragment.
func docFrom(t *testing.T, src string) *Document {
	t.Helper()
	doc, err := parseDocumentOnly("test.yml", []byte(src))
	if err != nil {
		t.Fatalf("parseDocumentOnly: %v", err)
	}
	return doc
}

// renderDoc renders the mutated document, failing the test on a render error.
// The round-trip warning is expected on any mutation and is not an error here.
func renderDoc(t *testing.T, doc *Document) string {
	t.Helper()
	out, _, err := renderBytes(doc)
	if err != nil {
		t.Fatalf("renderBytes: %v", err)
	}
	return string(out)
}

func TestPatchKeeperOrCreate_ExistingPathIsPlainPatch(t *testing.T) {
	doc := docFrom(t, "kid: keeper-1\ntoll:\n  threshold: 0.9 # inline\n")
	if err := PatchKeeperOrCreate(doc, "$.toll.threshold", 0.5); err != nil {
		t.Fatalf("PatchKeeperOrCreate: %v", err)
	}
	got := renderDoc(t, doc)
	if !strings.Contains(got, "threshold: 0.5") {
		t.Errorf("value not patched:\n%s", got)
	}
	if !strings.Contains(got, "# inline") {
		t.Errorf("inline comment lost:\n%s", got)
	}
}

func TestPatchKeeperOrCreate_AbsentBlockIsCreated(t *testing.T) {
	doc := docFrom(t, "kid: keeper-1\n")
	if err := PatchKeeperOrCreate(doc, "$.toll.threshold", 0.5); err != nil {
		t.Fatalf("PatchKeeperOrCreate: %v", err)
	}
	got := renderDoc(t, doc)
	if !strings.Contains(got, "toll:") || !strings.Contains(got, "threshold: 0.5") {
		t.Errorf("block not created:\n%s", got)
	}
	// The created document must survive a re-parse — the merged bytes go
	// straight into the validation pipeline.
	if _, err := parseDocumentOnly("test.yml", []byte(got)); err != nil {
		t.Fatalf("merged document does not re-parse: %v\n%s", err, got)
	}
}

func TestPatchKeeperOrCreate_DeepPathIsCreated(t *testing.T) {
	doc := docFrom(t, "kid: keeper-1\nlisten:\n  grpc:\n    addr: ':9443'\n")
	if err := PatchKeeperOrCreate(doc, "$.tempo.voyage_create.rate", 1.0); err != nil {
		t.Fatalf("PatchKeeperOrCreate: %v", err)
	}
	got := renderDoc(t, doc)
	for _, want := range []string{"tempo:", "voyage_create:", "rate: 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "addr: ':9443'") {
		t.Errorf("unrelated block damaged:\n%s", got)
	}
}

func TestPatchKeeperOrCreate_SiblingKeysSurvive(t *testing.T) {
	doc := docFrom(t, "toll:\n  window_size: 30s\n")
	if err := PatchKeeperOrCreate(doc, "$.toll.threshold", 0.5); err != nil {
		t.Fatalf("PatchKeeperOrCreate: %v", err)
	}
	got := renderDoc(t, doc)
	if !strings.Contains(got, "window_size: 30s") {
		t.Errorf("sibling key dropped:\n%s", got)
	}
	if !strings.Contains(got, "threshold: 0.5") {
		t.Errorf("key not created:\n%s", got)
	}
}

func TestPatchKeeperOrCreate_NullBlockBecomesMapping(t *testing.T) {
	doc := docFrom(t, "kid: keeper-1\ntoll:\n")
	if err := PatchKeeperOrCreate(doc, "$.toll.threshold", 0.5); err != nil {
		t.Fatalf("PatchKeeperOrCreate: %v", err)
	}
	got := renderDoc(t, doc)
	if !strings.Contains(got, "threshold: 0.5") {
		t.Errorf("null block not replaced by a mapping:\n%s", got)
	}
	if _, err := parseDocumentOnly("test.yml", []byte(got)); err != nil {
		t.Fatalf("merged document does not re-parse: %v\n%s", err, got)
	}
}

// A scalar on the way is a conflict, never a silent overwrite: replacing it
// with a mapping would drop an operator's value without a word.
func TestPatchKeeperOrCreate_NonMappingOnPathIsRejected(t *testing.T) {
	doc := docFrom(t, "toll: 5\n")
	err := PatchKeeperOrCreate(doc, "$.toll.threshold", 0.5)
	if err == nil {
		t.Fatalf("expected an error for a scalar on the path")
	}
	if !strings.Contains(err.Error(), "non-mapping") {
		t.Errorf("unexpected error: %v", err)
	}
	if got := renderDoc(t, doc); !strings.Contains(got, "toll: 5") {
		t.Errorf("document mutated on a rejected create:\n%s", got)
	}
}

func TestPatchKeeperOrCreate_UnsupportedPathForms(t *testing.T) {
	for _, p := range []string{"$.services[0].ref", "$.toll.*", "$", "", "toll.threshold", "$..threshold"} {
		doc := docFrom(t, "kid: keeper-1\n")
		if err := PatchKeeperOrCreate(doc, p, 1); err == nil {
			t.Errorf("path %q: expected an error", p)
		}
	}
}

// PatchKeeper itself must keep reporting an absent path — the write-back
// consumer wants a 404 on a typo, not a new key.
func TestPatchKeeper_StillReportsPathNotFound(t *testing.T) {
	doc := docFrom(t, "kid: keeper-1\n")
	err := PatchKeeper(doc, "$.toll.threshold", 0.5)
	if !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("expected ErrPathNotFound, got %v", err)
	}
}
