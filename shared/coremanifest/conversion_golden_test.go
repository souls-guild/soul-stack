package coremanifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// goldenPath holds what the PRE-NIM-377 code produced for all 24 core modules: the
// hand-written `<module>.yaml` files parsed by the old `shared/plugin` manifest parser
// and serialized field for field.
//
// It exists because the conversion from YAML to Go values (NIM-377) rewrote the
// validation contract of every built-in module at once. A mistyped `required`, a dropped
// `enum` or a lost `items` would not break a build — it would quietly widen or narrow
// what every destiny in the fleet is allowed to say. The golden is the only thing that
// can tell the difference, so it is frozen: regenerate it only when the core catalog
// itself is meant to change, and review the diff as a contract change.
const goldenPath = "testdata/core_schema_golden.json"

// TestCoreSchemaMatchesPreConversionGolden — the Go declarations in `mod_*.go` describe
// the same schema the hand-written YAML did.
//
// The comparison runs over the PRODUCTION serializer ([schema.Marshal]) applied to the
// PRODUCTION values, against a snapshot taken from the old parser's output. Nothing in
// between is hand-projected, so a field the conversion dropped is present on one side and
// absent on the other and the test names it by path.
func TestCoreSchemaMatchesPreConversionGolden(t *testing.T) {
	wantRaw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	gotRaw, err := schema.Marshal(sortedDocument())
	if err != nil {
		t.Fatalf("marshal live core schema: %v", err)
	}

	want, got := decodeJSON(t, wantRaw), decodeJSON(t, gotRaw)
	var diffs []string
	diffJSON("$", keyModulesByName(t, "golden", want), keyModulesByName(t, "live", got), &diffs)
	if len(diffs) == 0 {
		return
	}
	t.Errorf("core schema differs from %s in %d place(s) — the conversion is not lossless:", goldenPath, len(diffs))
	for _, d := range diffs {
		t.Errorf("  %s", d)
	}
	t.Log("if the core catalog genuinely changed, regenerate the golden in the same commit and review it as a contract change")
}

// TestCoreDeclarationsPassSDKValidator — the core declarations satisfy the same validator
// that judges a third-party artifact. [mustBuild] panics on a violation, so a failure here
// would normally surface as a panic at init; the test states the invariant by name and
// prints every finding rather than only the first.
func TestCoreDeclarationsPassSDKValidator(t *testing.T) {
	for _, i := range schema.Validate(validationDocument()) {
		if i.Level == schema.LevelError {
			t.Errorf("%s: %s", i.Path, i)
		}
	}
}

// sortedDocument is [validationDocument] with modules in name order, so the live document
// and the golden are comparable without depending on the declaration order of
// [coreModules] (which is grouped Soul-side first, ADR-015 order).
func sortedDocument() schema.Document {
	doc := validationDocument()
	doc.Modules = append([]schema.Module(nil), doc.Modules...)
	sort.Slice(doc.Modules, func(i, j int) bool { return doc.Modules[i].Name < doc.Modules[j].Name })
	return doc
}

func decodeJSON(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep number literals exact; a float round-trip would hide a default drift
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return v
}

// keyModulesByName rewrites the `modules` array into an object keyed by module name, so a
// module missing on one side is reported as that module rather than as a shift of every
// following array index.
func keyModulesByName(t *testing.T, side string, doc any) any {
	t.Helper()
	obj, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("%s: document root is %T, want object", side, doc)
	}
	mods, ok := obj["modules"].([]any)
	if !ok {
		t.Fatalf("%s: modules is %T, want array", side, obj["modules"])
	}
	byName := make(map[string]any, len(mods))
	for _, m := range mods {
		entry, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("%s: module entry is %T, want object", side, m)
		}
		name, _ := entry["name"].(string)
		if name == "" {
			t.Fatalf("%s: module entry without a name: %v", side, entry)
		}
		if _, dup := byName[name]; dup {
			t.Fatalf("%s: module %q declared twice", side, name)
		}
		byName[name] = entry
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	out["modules"] = byName
	return out
}

// diffJSON walks two decoded JSON trees and records every difference as a path plus what
// each side holds. It reports in both directions: a key only in the golden is a dropped
// field, a key only in the live schema is an addition the golden never authorized.
func diffJSON(path string, want, got any, out *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: golden has an object, live schema has %s", path, render(got)))
			return
		}
		for _, k := range sortedKeysOf(w) {
			gv, present := g[k]
			if !present {
				*out = append(*out, fmt.Sprintf("%s.%s: DROPPED — golden has %s, live schema has nothing", path, k, render(w[k])))
				continue
			}
			diffJSON(path+"."+k, w[k], gv, out)
		}
		for _, k := range sortedKeysOf(g) {
			if _, present := w[k]; !present {
				*out = append(*out, fmt.Sprintf("%s.%s: ADDED — live schema has %s, golden has nothing", path, k, render(g[k])))
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: golden has an array, live schema has %s", path, render(got)))
			return
		}
		if len(w) != len(g) {
			*out = append(*out, fmt.Sprintf("%s: golden has %d element(s) %s, live schema has %d %s",
				path, len(w), render(want), len(g), render(got)))
			return
		}
		for i := range w {
			diffJSON(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], out)
		}
	default:
		if !reflect.DeepEqual(want, got) {
			*out = append(*out, fmt.Sprintf("%s: golden has %s, live schema has %s", path, render(want), render(got)))
		}
	}
}

func render(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func sortedKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
