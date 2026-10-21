package validate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeServiceTree lays down the smallest service a lint run will accept, plus
// whatever extra files the case needs. Keys are paths relative to the service
// root; a nil value means "make this directory".
func writeServiceTree(t *testing.T, files map[string]string, dirs ...string) string {
	t.Helper()
	root := t.TempDir()

	base := "name: redis\nstate_schema: {}\n"
	if _, given := files["service.yml"]; !given {
		files["service.yml"] = base
	}
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return root
}

// lintService runs the service validator and returns its human output.
func lintService(t *testing.T, root string) string {
	t.Helper()
	var out bytes.Buffer
	_ = Run(Options{Path: filepath.Join(root, "service.yml"), Kind: KindService}, &out, &out)
	return out.String()
}

// TestServiceVars_StackStepInvalid — a `_stack.yaml` the keeper would refuse is
// refused offline, by the same parser, before the service is ever registered.
//
// Each case is a shape whose runtime symptom is a WRONG RESOLUTION rather than a
// failure: an unknown key means a gate that silently did not gate, an empty
// `stack:` means every var of the service quietly gone, a selector over `vars.*`
// means a layer chosen from a value the final merge then changes. `inline:` is
// removed (ADR-0082 amendment 2026-10-15), so it is refused with the hint that
// says where its values go.
func TestServiceVars_StackStepInvalid(t *testing.T) {
	for name, tc := range map[string]struct{ stack, want string }{
		"unknown key": {"stack:\n  - file: 00-base.yaml\n    whn: incarnation.traits.env == 'prd'\n", ""},
		"no steps":    {"steps:\n  - file: 00-base.yaml\n", ""},
		"inline":      {"stack:\n  - file: 00-base.yaml\n    inline: {a: 1}\n", "put the values in a file under vars/ and name it with file:"},
		"foreach without as": {"stack:\n  - foreach: \"${ incarnation.covens }\"\n" +
			"    file: \"coven-x.yaml\"\n", ""},
		"as shadows context": {"stack:\n  - foreach: \"${ incarnation.covens }\"\n" +
			"    as: vars\n    file: \"coven-x.yaml\"\n", ""},
		"bad strategy":    {"stack:\n  - file: 00-base.yaml\n    strategy: shallow\n", ""},
		"when reads vars": {"stack:\n  - file: 00-base.yaml\n  - file: coven-x.yaml\n    when: vars.env == 'dev'\n", "step 2"},
		"file reads vars": {"stack:\n  - file: \"coven-${ vars.tier }.yaml\"\n", "file: reads vars.tier"},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeServiceTree(t, map[string]string{
				"vars/00-base.yaml": "port: 6379\n",
				"vars/_stack.yaml":  tc.stack,
				"vars/coven-x.yaml": "tier: gold\n",
			})
			got := lintService(t, root)
			if !strings.Contains(got, "stack_step_invalid") {
				t.Errorf("expected stack_step_invalid, got: %s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected %q in: %s", tc.want, got)
			}
		})
	}
}

// TestServiceVars_ValidStackIsQuiet — the other half, and the one that makes the
// test above mean something. A well-formed stack raises nothing, so a passing
// `stack_step_invalid` assertion is about the malformed file and not about the
// check firing on everything.
func TestServiceVars_ValidStackIsQuiet(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\nenv: dev\n",
		"vars/coven-x.yaml": "tier: gold\n",
		"vars/90-dev.yaml":  "maxmemory_percent: 60\n",
		"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n" +
			"  - foreach: \"${ incarnation.covens }\"\n    as: coven\n" +
			"    file: \"coven-${ coven }.yaml\"\n    optional: true\n" +
			"  - file: 90-dev.yaml\n    when: incarnation.traits.env == 'dev'\n" +
			"    strategy: replace\n",
	})
	if got := lintService(t, root); strings.Contains(got, "stack_step_invalid") {
		t.Errorf("false stack_step_invalid on a valid pipeline: %s", got)
	}
}

// TestServiceVars_MisspeltStackFile — `_stack.yml` is refused rather than
// ignored. Ignored is the dangerous answer: the file sits there looking like a
// pipeline while the directory resolves lexically, so the author's conditions
// never run and nothing says so.
func TestServiceVars_MisspeltStackFile(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"vars/_stack.yml":   "stack:\n  - file: 00-base.yaml\n",
	})
	if got := lintService(t, root); !strings.Contains(got, "stack_step_invalid") {
		t.Errorf("expected stack_step_invalid for the .yml spelling, got: %s", got)
	}
}

// TestServiceVars_DirNested — a layer file in a subdirectory of vars/ is never
// read by the resolver, which does not descend. This is the shape the retired
// `essence/coven/` layout had, so it is the mistake a migrating author is most
// likely to make, and its symptom is a value that is simply absent.
func TestServiceVars_DirNested(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":    "port: 6379\n",
		"vars/coven/prod.yaml": "tier: gold\n",
	})
	got := lintService(t, root)
	if !strings.Contains(got, "vars_dir_nested") {
		t.Fatalf("expected vars_dir_nested, got: %s", got)
	}
	if strings.Count(got, "vars_dir_nested") != 1 {
		t.Errorf("want exactly one diagnostic per subdirectory, got: %s", got)
	}
}

// TestServiceVars_FlatLayoutIsQuiet — files directly inside vars/ are what the
// resolver reads, and a non-YAML file in a subdirectory is not a layer anyone
// expected to be read.
func TestServiceVars_FlatLayoutIsQuiet(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"vars/10-tls.yaml":  "tls_port: 6380\n",
		"vars/README.md":    "notes\n",
		"vars/docs/why.md":  "prose\n",
	})
	if got := lintService(t, root); strings.Contains(got, "vars_dir_nested") {
		t.Errorf("false vars_dir_nested on a flat layout: %s", got)
	}
}

// TestServiceVars_RetiredLayout — the retired directory still in place. Half a
// migration is worse than none: `vars/` resolves to nothing, and every
// `default(vars.X, y)` in the repo takes its fallback without complaint.
func TestServiceVars_RetiredLayout(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"essence/_default.yaml": "port: 6379\n",
	})
	if got := lintService(t, root); !strings.Contains(got, "vars_retired_layout") {
		t.Errorf("expected vars_retired_layout, got: %s", got)
	}
}

// TestServiceVars_NoVarsDirIsQuiet — a service may legitimately declare no
// defaults. Absence must not read as a defect, or the check becomes noise every
// author learns to ignore.
func TestServiceVars_NoVarsDirIsQuiet(t *testing.T) {
	root := writeServiceTree(t, map[string]string{})
	got := lintService(t, root)
	for _, code := range []string{"vars_dir_nested", "stack_step_invalid", "vars_retired_layout"} {
		if strings.Contains(got, code) {
			t.Errorf("false %s on a service with no vars/: %s", code, got)
		}
	}
}

// lintScenario runs the scenario validator over `<root>/scenario/<name>/main.yml`.
func lintScenario(t *testing.T, root, scenario string) string {
	t.Helper()
	var out bytes.Buffer
	_ = Run(Options{
		Path: filepath.Join(root, "scenario", scenario, "main.yml"),
		Kind: KindScenario,
	}, &out, &out)
	return out.String()
}

// TestServiceVarShadow — a local `vars:` taking over a service var's name.
//
// The one cost of merging the two namespaces. Before ADR-0082 a service's
// parameters were `essence.X` and a local was `vars.X` — two spellings, no way
// to collide. Now a local silently wins, and every later `vars.port` in that
// scenario means the local one with nothing to say so.
func TestServiceVarShadow(t *testing.T) {
	const scn = `name: create
input: {}
tasks:
  - name: write config
    module: core.file.present
    vars:
      port: "9999"
    params:
      path: /etc/redis/redis.conf
      content: "port ${ vars.port }"
`
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":        "port: 6379\nconf_dir: /etc/redis\n",
		"scenario/create/main.yml": scn,
	})
	got := lintScenario(t, root, "create")
	if !strings.Contains(got, "vars_shadows_service_var") {
		t.Fatalf("expected vars_shadows_service_var, got: %s", got)
	}
	if !strings.Contains(got, "vars.port") {
		t.Errorf("the diagnostic must name the shadowed var, got: %s", got)
	}
	if strings.Contains(got, "conf_dir") {
		t.Errorf("conf_dir is not shadowed by anything; got: %s", got)
	}
}

// TestServiceVarShadow_NonOverlappingIsQuiet — the false-positive side. A local
// whose name no service var uses is the ordinary case and must stay silent, or
// the warning is worthless.
func TestServiceVarShadow_NonOverlappingIsQuiet(t *testing.T) {
	const scn = `name: create
input: {}
tasks:
  - name: write config
    module: core.file.present
    vars:
      rendered_at: "now"
    params:
      path: /etc/redis/redis.conf
      content: "# ${ vars.rendered_at }"
`
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":        "port: 6379\n",
		"scenario/create/main.yml": scn,
	})
	if got := lintScenario(t, root, "create"); strings.Contains(got, "vars_shadows_service_var") {
		t.Errorf("false vars_shadows_service_var on non-overlapping names: %s", got)
	}
}

// TestStackLegacyRoot_IsWalked — `vars/_stack.yaml` is the THIRD CEL environment
// carrying the `incarnation` root ([ADR-0085], NIM-730) and the one no scenario
// rule can reach: it is not a scenario. A step written against the retired
// spelling resolves today through the window's alias and, when the alias goes,
// fails BEFORE the render — so the whole run refuses to start rather than one
// task failing, with nothing having warned.
//
// All three selectors are covered, each with its own address. (`inline:`, the
// fourth evaluated cell, is refused at parse since the ADR-0082 amendment of
// 2026-10-15.)
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: delete the
// `stackLegacyRootDiags(...)` call from stackFileDiags. The stack still parses,
// every other service check stays green, and this test goes red.
func TestStackLegacyRoot_IsWalked(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"vars/_stack.yaml": `stack:
  - file: "00-base.yaml"
  - file: "tier-${ incarnation.name }.yaml"
    optional: true
  - when: "incarnation.name != ''"
    file: "00-base.yaml"
  - foreach: "${ [incarnation.name] }"
    as: n
    file: "who-${ n }.yaml"
    optional: true
`,
	})

	got := lintService(t, root)
	for _, want := range []string{
		"incarnation_name_legacy_root",
		"_stack.yaml:3:5", // file:
		"_stack.yaml:5:5", // when:
		"_stack.yaml:7:5", // foreach:
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// WARNING, not error: an error would close the window it exists to keep open.
	// Checked on the rule's own lines, since the shared fixture manifest carries an
	// unrelated error of its own.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "incarnation_name_legacy_root") && !strings.Contains(line, "warning:") {
			t.Errorf("the retired root in _stack.yaml must WARN, not error: %s", line)
		}
	}
}

// TestLayerLegacyRoot_IsWalked — a layer file's definitions are evaluated since
// the ADR-0082 amendment of 2026-10-15, so `${ incarnation.name }` in one is a
// real read leaning on the alias window, exactly as in a selector. Every YAML file
// under vars/ is walked, a subdirectory included — a `_stack.yaml` `file:` can
// name one — and each finding cites its own file and line. The current spelling
// and prose naming the root stay silent, and the stack itself is not walked twice.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: delete the
// `layerLegacyRootDiags(varsDir)` call from serviceVarsDiags. The stack and the
// layers still lint, and this test goes red.
func TestLayerLegacyRoot_IsWalked(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\nowner: \"${ incarnation.name }\"\n" +
			"note: \"composed from incarnation.name at create\"\n",
		"vars/coven/prod.yml": "tls:\n  cn: \"${ incarnation.name }.svc\"\n  sans:\n" +
			"    - \"${ incarnation.id }\"\n    - \"x-${ incarnation.name }\"\n",
		"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n  - file: coven/prod.yml\n",
	})

	got := lintService(t, root)
	for _, want := range []string{
		filepath.Join("vars", "00-base.yaml") + ":2:1",
		filepath.Join("vars", "coven", "prod.yml") + ":2:3",
		filepath.Join("vars", "coven", "prod.yml") + ":5:7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "incarnation_name_legacy_root"); n != 3 {
		t.Errorf("want exactly 3 findings (owner, tls.cn, tls.sans[1]), got %d:\n%s", n, got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "incarnation_name_legacy_root") && !strings.Contains(line, "warning:") {
			t.Errorf("the retired root in a layer file must WARN, not error: %s", line)
		}
	}
}

// TestStackLegacyRoot_NewSpellingIsSilent — the migrated stack says nothing, so
// the warning cannot become noise a converted repository reads past.
func TestStackLegacyRoot_NewSpellingIsSilent(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"vars/_stack.yaml": `stack:
  - file: "00-base.yaml"
  - when: "incarnation.id != ''"
    file: "owner-${ incarnation.id }.yaml"
    optional: true
`,
	})
	if got := lintService(t, root); strings.Contains(got, "incarnation_name_legacy_root") {
		t.Errorf("the current spelling was flagged:\n%s", got)
	}
}
