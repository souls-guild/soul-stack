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
		t.Fatalf("ожидали хеш как state, получили %q", state1)
	}
	if data.GetFields()["sha256"].GetStringValue() != state1 {
		t.Error("data.sha256 должно совпадать со state")
	}

	// Тот же контент → тот же хеш (idempotent).
	state2, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if state2 != state1 {
		t.Fatalf("хеш неизменного файла поменялся: %q → %q", state1, state2)
	}

	// Изменение контента → другой хеш.
	if err := os.WriteFile(path, []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	state3, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if state3 == state1 {
		t.Fatal("хеш изменённого файла не поменялся")
	}
}

func TestFileChangedMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.conf")

	b := NewFileChanged()
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("Check для отсутствующего файла не должен возвращать ошибку: %v", err)
	}
	if state != stateFileMissing {
		t.Fatalf("state = %q, want missing", state)
	}
	if data.GetFields()["path"].GetStringValue() != path {
		t.Error("data.path должно нести путь")
	}
}

func TestFileChangedMissingToPresentIsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.conf")

	b := NewFileChanged()
	s1, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if s1 != stateFileMissing {
		t.Fatalf("предусловие: ожидали missing, получили %q", s1)
	}

	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, _, _ := b.Check(context.Background(), paramStruct(t, map[string]any{"path": path}))
	if s2 == stateFileMissing || s2 == s1 {
		t.Fatal("появление файла должно сменить state с missing на хеш")
	}
}

func TestFileChangedMissingParam(t *testing.T) {
	b := NewFileChanged()
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("ожидали ошибку при отсутствии param path")
	}
}
