package beacon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileChangedHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watched.conf")
	if err := os.WriteFile(path, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewFileChanged()
	state1, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state1 == stateFileMissing || state1 == "" {
		t.Fatalf("expected a hash as state, got %q", state1)
	}
	if data.GetFields()["sha256"].GetStringValue() != state1 {
		t.Error("data.sha256 should match state")
	}

	// Same content → same hash (idempotent).
	state2, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if state2 != state1 {
		t.Fatalf("hash of an unchanged file changed: %q -> %q", state1, state2)
	}

	// Content change → different hash.
	if err := os.WriteFile(path, []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	state3, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if state3 == state1 {
		t.Fatal("hash of a changed file did not change")
	}
}

func TestFileChangedMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.conf")

	b := NewFileChanged()
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("Check for a missing file should not return an error: %v", err)
	}
	if state != stateFileMissing {
		t.Fatalf("state = %q, want missing", state)
	}
	if data.GetFields()["path"].GetStringValue() != path {
		t.Error("data.path should carry the path")
	}
}

func TestFileChangedMissingToPresentIsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.conf")

	b := NewFileChanged()
	s1, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if s1 != stateFileMissing {
		t.Fatalf("precondition: expected missing, got %q", s1)
	}

	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if s2 == stateFileMissing || s2 == s1 {
		t.Fatal("a file appearing should change state from missing to a hash")
	}
}

func TestFileChangedMissingParam(t *testing.T) {
	b := NewFileChanged()
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("expected an error when param path is missing")
	}
}
