package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadDestinyVars_TopLevelMap — vars.yml читается как raw top-level YAML-map
// без схемо-валидации; CEL-выражения проходят строкой (резолв позже, в render).
func TestLoadDestinyVars_TopLevelMap(t *testing.T) {
	src := `redis_unit_name: redis-server
port: 6379
enabled: true
acl_path: "/etc/redis/${ input.user }.acl"
`
	got, err := LoadDestinyVarsFromBytes("vars.yml", []byte(src))
	if err != nil {
		t.Fatalf("LoadDestinyVarsFromBytes: %v", err)
	}
	want := map[string]any{
		"redis_unit_name": "redis-server",
		"port":            uint64(6379),
		"enabled":         true,
		"acl_path":        "/etc/redis/${ input.user }.acl",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("vars = %#v, want %#v", got, want)
	}
}

// TestLoadDestinyVars_MissingFile — отсутствие файла → nil, не ошибка
// (vars.yml опционален).
func TestLoadDestinyVars_MissingFile(t *testing.T) {
	got, err := LoadDestinyVars(filepath.Join(t.TempDir(), "nope-vars.yml"))
	if err != nil {
		t.Fatalf("LoadDestinyVars(missing): %v", err)
	}
	if got != nil {
		t.Errorf("vars = %v, want nil для отсутствующего файла", got)
	}
}

// TestLoadDestinyVars_EmptyFile — пустой файл → nil-карта, не ошибка.
func TestLoadDestinyVars_EmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vars.yml")
	if err := os.WriteFile(p, []byte("# only a comment\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadDestinyVars(p)
	if err != nil {
		t.Fatalf("LoadDestinyVars(empty): %v", err)
	}
	if got != nil {
		t.Errorf("vars = %v, want nil для пустого файла", got)
	}
}

// TestLoadDestinyVars_Malformed — невалидный YAML (sequence вместо map) → ошибка.
func TestLoadDestinyVars_Malformed(t *testing.T) {
	_, err := LoadDestinyVarsFromBytes("vars.yml", []byte("- not\n- a\n- map\n"))
	if err == nil {
		t.Fatal("LoadDestinyVarsFromBytes: ожидалась ошибка на не-map корне")
	}
}

// TestDestinyVarsCollisions — пересечение имён file-level vars.yml и task-level
// vars: возвращается отсортированным; непересекающиеся имена и пустые входы — nil.
func TestDestinyVarsCollisions(t *testing.T) {
	fileVars := map[string]any{"unit": "redis-server", "conf": "/etc/redis", "user": "redis"}
	tasks := []Task{
		{Name: "a", Vars: map[string]any{"unit": "x", "extra": "y"}},
		{Name: "b", Vars: map[string]any{"user": "z"}},
		{Name: "c"}, // без vars:
	}
	got := DestinyVarsCollisions(fileVars, tasks)
	want := []string{"unit", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collisions = %v, want %v (отсортировано)", got, want)
	}

	// Непересекающиеся.
	if c := DestinyVarsCollisions(map[string]any{"a": 1}, []Task{{Vars: map[string]any{"b": 2}}}); c != nil {
		t.Errorf("collisions = %v, want nil для непересекающихся", c)
	}
	// Пустые входы.
	if c := DestinyVarsCollisions(nil, tasks); c != nil {
		t.Errorf("collisions(nil fileVars) = %v, want nil", c)
	}
	if c := DestinyVarsCollisions(fileVars, nil); c != nil {
		t.Errorf("collisions(nil tasks) = %v, want nil", c)
	}
}
