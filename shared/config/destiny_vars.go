package config

import (
	"fmt"
	"os"
	"sort"

	"github.com/goccy/go-yaml"
)

// LoadDestinyVars parses destiny `vars.yml` — the top-level YAML map of destiny
// locals (docs/destiny/vars.md). Returns a RAW name→value map WITHOUT schema
// validation: vars are not typed by the spec (a plain map of values from the
// destiny author, see the `vars` vs `input` table in vars.md), so the
// parseAndValidate path (as for destiny.yml/scenario.yml) does not apply here —
// a plain yaml.Unmarshal.
//
// Values pass through as Go data (string/number/bool/collection); CEL
// expressions `${ … }` in string cells are resolved LATER, in the render phase
// (renderApplyDestiny over input+soulprint.self), not here.
//
// A missing file is NOT an error: destiny without locals (vars.yml is optional)
// yields a nil map, and `${ vars.<x> }` then fails with the normal no-such-key.
func LoadDestinyVars(path string) (map[string]any, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	return LoadDestinyVarsFromBytes(path, src)
}

// LoadDestinyVarsFromBytes is the I/O-free entry point (the destiny snapshot is
// already read, in-memory fixtures in tests). An empty document (only
// comments/whitespace) → nil map, not an error. filename is used only as a label
// in the message.
func LoadDestinyVarsFromBytes(filename string, data []byte) (map[string]any, error) {
	data = stripBOM(data)
	if len(data) == 0 {
		return nil, nil
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("config: parsing %s (vars.yml - top-level YAML map): %w", filename, err)
	}
	return out, nil
}

// DestinyVarsCollisions returns a sorted list of names declared in BOTH the
// file-level `vars.yml` (fileVars) AND the task-level `vars:` of at least one task
// (tasks). This is not an error — Variant A (vars.md "Merging file-vars ↔
// task-vars") is deterministic: a task-var overrides a file-var of the same name.
// But a collision is a common source of confusion ("why is my file-var
// ignored?"), so soul-lint raises a warn. An empty result → no overlaps.
func DestinyVarsCollisions(fileVars map[string]any, tasks []Task) []string {
	if len(fileVars) == 0 || len(tasks) == 0 {
		return nil
	}
	clash := make(map[string]struct{})
	for i := range tasks {
		for name := range tasks[i].Vars {
			if _, ok := fileVars[name]; ok {
				clash[name] = struct{}{}
			}
		}
	}
	if len(clash) == 0 {
		return nil
	}
	out := make([]string, 0, len(clash))
	for name := range clash {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
