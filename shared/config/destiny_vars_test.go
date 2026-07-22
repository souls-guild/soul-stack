package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadDestinyVars_TopLevelMap — vars.yml is read as a raw top-level YAML map
// without schema validation; CEL expressions pass through as strings (resolved
// later, at render).
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

// TestLoadDestinyVars_MissingFile — a missing file → nil, not an error
// (vars.yml is optional).
func TestLoadDestinyVars_MissingFile(t *testing.T) {
	got, err := LoadDestinyVars(filepath.Join(t.TempDir(), "nope-vars.yml"))
	if err != nil {
		t.Fatalf("LoadDestinyVars(missing): %v", err)
	}
	if got != nil {
		t.Errorf("vars = %v, want nil for a missing file", got)
	}
}

// TestLoadDestinyVars_EmptyFile — an empty file → nil map, not an error.
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
		t.Errorf("vars = %v, want nil for an empty file", got)
	}
}

// TestLoadDestinyVars_Malformed — invalid YAML (sequence instead of a map) → error.
func TestLoadDestinyVars_Malformed(t *testing.T) {
	_, err := LoadDestinyVarsFromBytes("vars.yml", []byte("- not\n- a\n- map\n"))
	if err == nil {
		t.Fatal("LoadDestinyVarsFromBytes: expected an error on a non-map root")
	}
}

// TestDestinyVarsCollisions — the name overlap between file-level vars.yml and
// task-level vars is returned sorted; non-overlapping names and empty inputs → nil.
func TestDestinyVarsCollisions(t *testing.T) {
	fileVars := map[string]any{"unit": "redis-server", "conf": "/etc/redis", "user": "redis"}
	tasks := []Task{
		{Name: "a", Vars: map[string]any{"unit": "x", "extra": "y"}},
		{Name: "b", Vars: map[string]any{"user": "z"}},
		{Name: "c"}, // no vars:
	}
	got := DestinyVarsCollisions(fileVars, tasks)
	want := []string{"unit", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collisions = %v, want %v (sorted)", got, want)
	}

	// Non-overlapping.
	if c := DestinyVarsCollisions(map[string]any{"a": 1}, []Task{{Vars: map[string]any{"b": 2}}}); c != nil {
		t.Errorf("collisions = %v, want nil for non-overlapping", c)
	}
	// Empty inputs.
	if c := DestinyVarsCollisions(nil, tasks); c != nil {
		t.Errorf("collisions(nil fileVars) = %v, want nil", c)
	}
	if c := DestinyVarsCollisions(fileVars, nil); c != nil {
		t.Errorf("collisions(nil tasks) = %v, want nil", c)
	}
}
