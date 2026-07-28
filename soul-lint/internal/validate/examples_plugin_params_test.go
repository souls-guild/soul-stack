package validate

// Corpus guard on plugin params (NIM-206): every `params:` key a scenario or
// destiny in examples/ passes to a plugin module must be declared in that
// plugin's manifest.
//
// Keeper's static check (shared/config.validateModuleParams) covers namespace
// `core` only — a plugin manifest normally lives on disk next to its binary and
// is not resolvable while linting. In this repo both halves ARE checked in, so
// the audit can run, and it has to: since NIM-204 the runtime gate is enforced
// for plugins too (ADR-0076(t)), so an undeclared key here is a live
// module.unknown_param on the host, not a log line. The
// per-plugin table guard lives with each plugin (see
// examples/module/soul-mod-community-redis/manifest_test.go); this one catches
// the other direction — a scenario reaching for a param no manifest declares.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/plugin"
)

const examplesRoot = "../../../examples"

func TestExamples_PluginTaskParamsAreDeclared(t *testing.T) {
	manifests := loadExampleManifests(t)
	if len(manifests) == 0 {
		t.Fatal("no soul_module manifests found under examples/module")
	}

	files := exampleCorpusFiles(t)
	if len(files) == 0 {
		t.Fatal("no YAML found under examples/ outside examples/module")
	}

	seen := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc any
		if err := yaml.Unmarshal(src, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, _ := filepath.Rel(examplesRoot, path)

		for _, task := range findModuleTasks(doc) {
			ns, name, state, ok := splitAddress(task.module)
			if !ok {
				continue
			}
			m, known := manifests[ns+"."+name]
			if !known {
				continue // core module, or a plugin whose manifest is not in this repo
			}
			def, hasState := m.Spec.States[state]
			if !hasState {
				t.Errorf("%s: task addresses %s, but the manifest has no such state", rel, task.module)
				continue
			}
			seen++
			for _, key := range sortedParamKeys(task.params) {
				if _, declared := def.Input[key]; !declared {
					t.Errorf("%s: %s receives param %q, which the manifest does not declare "+
						"— this task FAILS on a host with module.unknown_param (ADR-0076(t))",
						rel, task.module, key)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no plugin-module tasks found in examples/ — the audit checked nothing")
	}
	t.Logf("audited %d plugin-module tasks across %d files", seen, len(files))
}

type moduleTask struct {
	module string
	params map[string]any
}

// findModuleTasks walks arbitrary YAML and returns every mapping that carries a
// string `module:` alongside a `params:` mapping. Walking the raw tree rather
// than the scenario parser keeps the audit indifferent to where a task sits —
// top-level, inside a block, behind an include, in a destiny.
func findModuleTasks(node any) []moduleTask {
	var out []moduleTask
	switch v := node.(type) {
	case map[string]any:
		if mod, ok := v["module"].(string); ok {
			params, _ := v["params"].(map[string]any)
			out = append(out, moduleTask{module: mod, params: params})
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic order of reported findings
		for _, k := range keys {
			out = append(out, findModuleTasks(v[k])...)
		}
	case []any:
		for _, child := range v {
			out = append(out, findModuleTasks(child)...)
		}
	}
	return out
}

// splitAddress splits `<namespace>.<name>.<state>`. A state may not contain a
// dot, a namespace and a name may not either, so the split is unambiguous.
func splitAddress(addr string) (ns, name, state string, ok bool) {
	parts := strings.Split(addr, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func sortedParamKeys(params map[string]any) []string {
	out := make([]string, 0, len(params))
	for k := range params {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func loadExampleManifests(t *testing.T) map[string]*plugin.Manifest {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(examplesRoot, "module", "*", "manifest.yaml"))
	if err != nil {
		t.Fatalf("glob manifests: %v", err)
	}
	out := make(map[string]*plugin.Manifest, len(paths))
	for _, p := range paths {
		m, diags, err := plugin.Load(p)
		if err != nil {
			t.Fatalf("load %s: %v", p, err)
		}
		if m == nil {
			t.Fatalf("load %s: unparseable (%v)", p, diags)
		}
		if m.Kind != "soul_module" {
			continue
		}
		out[fmt.Sprintf("%s.%s", m.Namespace, m.Name)] = m
	}
	return out
}

// exampleCorpusFiles lists the YAML of examples/ minus examples/module (a
// manifest is the contract, not a caller of it).
func exampleCorpusFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	moduleDir := filepath.Join(examplesRoot, "module")
	err := filepath.WalkDir(examplesRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == moduleDir {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext == ".yml" || ext == ".yaml" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples: %v", err)
	}
	return out
}
