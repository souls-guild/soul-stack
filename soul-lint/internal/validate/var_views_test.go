package validate

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/servicevars"
)

func varCodes(ds []diag.Diagnostic) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, d := range ds {
		switch d.Code {
		case codeVarCycle, codeVarUnknownRef, codeVarInvalid:
			out = append(out, d)
		}
	}
	return out
}

// TestServiceView_FollowsTheStack — the static walk stacks a service's layers as the
// Keeper does: in `_stack.yaml` step order, a composed `file:` at its step's
// position, not in the lexical order of the files' paths. Self-reference and deep
// merge depend on that order, so a lexical walk reports a cycle the Keeper never
// raises, and misses one it does. The second case gates its step on a coven the stub
// incarnation lacks, so only the static walk can see its cycle.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars.EveryLayer
// take the lexical branch whatever the stack — "step order over lexical order" goes
// red; or walk every file under vars/ sorted by path, as before NIM-1014's fix
// round — "a foreach step after the defaults" goes red.
func TestServiceView_FollowsTheStack(t *testing.T) {
	t.Run("a foreach step after the defaults", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/_stack.yaml": "stack:\n  - file: defaults.yaml\n" +
				"  - foreach: \"${ incarnation.covens }\"\n    as: coven\n    file: \"coven/${ coven }.yaml\"\n    optional: true\n",
			"vars/defaults.yaml":   "redis: {port: 6379}\n",
			"vars/coven/prod.yaml": "redis: {url: \"redis://localhost:${ vars.redis.port }\"}\n",
		})
		layers, err := servicevars.Layers(os.DirFS(root), servicevars.IncarnationContext{ID: "x", Service: "redis", Covens: []string{"prod"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := cel.NewVarView(nil, layers...).Lookup("redis")
		if err != nil || got.(map[string]any)["url"] != "redis://localhost:6379" {
			t.Fatalf("the Keeper's prod view: redis = %#v, %v", got, err)
		}
		if ds := varCodes(serviceDiags(t, root, "redis")); len(ds) != 0 {
			t.Errorf("no var finding, got %+v", ds)
		}
	})
	t.Run("step order over lexical order", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/_stack.yaml":  "stack:\n  - file: 20-wb.yaml\n    when: \"'prod' in incarnation.covens\"\n  - file: 10-base.yaml\n",
			"vars/20-wb.yaml":   "redis: {url: \"${ vars.redis.port }\"}\n",
			"vars/10-base.yaml": "redis: {port: 6379}\n",
		})
		layers, err := servicevars.Layers(os.DirFS(root), servicevars.IncarnationContext{ID: "x", Service: "redis", Covens: []string{"prod"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := keeperCycleText(t, cel.NewVarView(nil, layers...), "redis")
		got := withCode(serviceDiags(t, root, "redis"), codeVarCycle)
		if len(got) != 1 || got[0].Message != want || got[0].File != filepath.Join(root, "vars", "20-wb.yaml") {
			t.Fatalf("want one %q at vars/20-wb.yaml, got %+v", want, got)
		}
	})
}

// TestServiceView_ForeachInBindingOrder — a `foreach:` over a constant list stacks
// its files in binding order, as the Keeper does, not in their lexical order, and
// for every incarnation. "forder": the Keeper stacks l/b.yaml under l/a.yaml, so
// x = 1 and y = 1, while the lexical order would close x → y → x. "foreachorder":
// l/a.yaml's self-reference reads l/b.yaml below it.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars.EveryLayer
// skip fixedFiles (`fixed := false`), or mark every stack layer Conditional — both
// go red.
func TestServiceView_ForeachInBindingOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  map[string]any
	}{
		"forder": {
			files: map[string]string{"vars/l/a.yaml": "x: 1\ny: \"${ vars.x }\"\n", "vars/l/b.yaml": "x: \"${ vars.y }\"\n"},
			want:  map[string]any{"x": uint64(1), "y": uint64(1)},
		},
		"foreachorder": {
			files: map[string]string{"vars/l/a.yaml": "x: \"${ vars.x }-a\"\n", "vars/l/b.yaml": "x: base\n"},
			want:  map[string]any{"x": "base-a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc.files["vars/_stack.yaml"] = "stack:\n  - foreach: \"${ ['b', 'a'] }\"\n    as: f\n    file: \"l/${ f }.yaml\"\n"
			root := writeServiceTree(t, tc.files)
			got, err := keeperServiceView(t, root).Materialize()
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the Keeper's view = %#v, %v; want %#v", got, err, tc.want)
			}
			if ds := varCodes(serviceDiags(t, root, "redis")); len(ds) != 0 {
				t.Errorf("no var finding, got %+v", ds)
			}
		})
	}
}

// TestVarInvalid_StubSkipsAFileNamedByItsIdentity — a required step `file:
// "inc/${ incarnation.id }.yaml"` names one file per incarnation, and the stub's id
// is made up, so the stub not finding inc/stub-incarnation.yaml is no finding; the
// static walk still lints inc/redis-prod.yaml. So is a composed file over a
// `foreach:` of the incarnation's covens. A missing file whose path is the same for
// every incarnation — one composed from a constant `foreach:` — stays var_invalid,
// the stub's. A literal one is var_invalid at its step, once, whatever its `when:` or
// `foreach:`: the stub never selects a coven-gated step, and a `foreach:` over the
// incarnation's id stacks it for every incarnation, which the Keeper refuses.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars stackLayers
// drop the `a.stub && …` clause — "per-id file" goes red; skip every missing file
// under the stub (`a.stub ||`) — "file of a constant foreach list" goes red; drop the
// missingStepFileDiags call from serviceVarChecks — the literal rows a stub does not
// select go red; or drop the StepFileMissingError test of serviceStubDiags — "literal
// file" goes red on a second finding.
func TestVarInvalid_StubSkipsAFileNamedByItsIdentity(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"per-id file": {
			"vars/_stack.yaml":         "stack:\n  - file: 00-base.yaml\n  - file: \"inc/${ incarnation.id }.yaml\"\n",
			"vars/inc/redis-prod.yaml": "port: 2\nt: \"${ vars.nope }\"\n",
		},
		"per-coven file": {
			"vars/_stack.yaml":         "stack:\n  - file: 00-base.yaml\n  - foreach: \"${ incarnation.covens + ['zz'] }\"\n    as: c\n    file: \"inc/${ c }.yaml\"\n",
			"vars/inc/redis-prod.yaml": "port: 2\nt: \"${ vars.nope }\"\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			files["vars/00-base.yaml"] = "port: 1\n"
			root := writeServiceTree(t, files)
			got := varCodes(serviceDiags(t, root, "redis"))
			if len(got) != 1 || got[0].Code != codeVarUnknownRef || got[0].File != filepath.Join(root, "vars", "inc", "redis-prod.yaml") {
				t.Errorf("want only the walk's var_unknown_ref in inc/redis-prod.yaml, got %+v", got)
			}
		})
	}
	t.Run("file of a constant foreach list", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - foreach: \"${ ['missing'] }\"\n    as: f\n    file: \"10-${ f }.yaml\"\n",
			"vars/00-base.yaml": "port: 1\n",
		})
		got := varCodes(serviceDiags(t, root, "redis"))
		if len(got) != 1 || got[0].Code != codeVarInvalid || !strings.Contains(got[0].Message, `"10-missing.yaml" not found`) {
			t.Errorf("want one var_invalid on 10-missing.yaml, got %+v", got)
		}
	})
	for name, step := range map[string]string{
		"literal file":                      "  - file: 10-missing.yaml\n",
		"literal file of a gated step":      "  - file: 10-missing.yaml\n    when: \"'a' in incarnation.covens\"\n",
		"literal file over the covens":      "  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: 10-missing.yaml\n",
		"literal file over the incarnation": "  - foreach: \"${ [incarnation.id] }\"\n    as: i\n    file: 10-missing.yaml\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeServiceTree(t, map[string]string{
				"vars/_stack.yaml":  "stack:\n  - file: 00-base.yaml\n" + step,
				"vars/00-base.yaml": "port: 1\n",
			})
			got := varCodes(serviceDiags(t, root, "redis"))
			want := `vars/_stack.yaml step 2: file "10-missing.yaml" not found (add optional: true to tolerate it)`
			if len(got) != 1 || got[0].Code != codeVarInvalid || got[0].Message != want ||
				got[0].File != filepath.Join(root, "vars", "_stack.yaml") || got[0].YAMLPath != "$.stack[1].file" || got[0].Line == 0 {
				t.Errorf("want one var_invalid %q at the step's file:, got %+v", want, got)
			}
		})
	}
}

// TestServiceView_ADeadFileIsNotLinted — a nested file no step names is never read
// by the Keeper, so none of its definitions is a var finding; vars_dir_nested is
// what reports it.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars.EveryLayer
// walk every file under vars/ — both go red.
func TestServiceView_ADeadFileIsNotLinted(t *testing.T) {
	for name, stack := range map[string]string{
		"no stack":                      "",
		"a stack that does not name it": "stack:\n  - file: 00-base.yaml\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"vars/00-base.yaml": "port: 6379\n",
				"vars/coven/x.yaml": "a: \"${ vars.a }\"\nb: \"${ vars.nope }\"\nc: \"${ soulprint.self.x }\"\n",
			}
			if stack != "" {
				files["vars/_stack.yaml"] = stack
			}
			ds := serviceDiags(t, writeServiceTree(t, files), "redis")
			if got := varCodes(ds); len(got) != 0 {
				t.Errorf("a file the Keeper never reads has no var findings, got %+v", got)
			}
			if got := withCode(ds, "vars_dir_nested"); len(got) != 1 {
				t.Errorf("want vars_dir_nested, got %+v", got)
			}
		})
	}
}

// TestServiceView_AnUnreadableLayerIsAFinding — a layer that does not parse stops
// the static walk, and that is reported rather than leaving every check silently
// off: in validate-service even for a file only a coven-gated step reads, which the
// stub evaluation never selects, and in validate-scenario, whose views carry the
// service rung.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in serviceVarViewDiags and
// scenarioVarViewDiags drop the error of serviceVarLayers again — both go red.
func TestServiceView_AnUnreadableLayerIsAFinding(t *testing.T) {
	t.Run("validate-service", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n" +
				"  - foreach: \"${ incarnation.covens }\"\n    as: coven\n    file: \"coven/${ coven }.yaml\"\n    optional: true\n",
			"vars/00-base.yaml":    "port: 6379\n",
			"vars/coven/prod.yaml": "a: [\n",
		})
		got := withCode(serviceDiags(t, root, "redis"), codeVarInvalid)
		if len(got) != 1 || !strings.Contains(got[0].Message, "vars/coven/prod.yaml") || got[0].Level != diag.LevelError {
			t.Errorf("want one var_invalid ERROR naming vars/coven/prod.yaml, got %+v", got)
		}
	})
	t.Run("validate-scenario", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "port: 6379\n",
			"vars/10-bad.yaml":         "a: [\n",
			"scenario/create/main.yml": scenarioWithTaskVars("", "      x: \"${ vars.port }\"\n"),
		})
		got := withCode(scenarioDiags(t, root, "create"), codeVarInvalid)
		if len(got) != 1 || !strings.Contains(got[0].Message, "vars/10-bad.yaml") {
			t.Errorf("want one var_invalid naming vars/10-bad.yaml, got %+v", got)
		}
	})
}

// TestValidateDestiny_RunsTheVarChecks — validate-destiny judges a destiny's
// vars.yml: the pilot-vars-cycle fixture, the one known cycle in the corpus, is its
// var_cycle in the Keeper's text.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: delete the
// destinyVarViewDiags call from diagnose — this goes red.
func TestValidateDestiny_RunsTheVarChecks(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "service", "pilot-destiny", "pilot-vars-cycle", "destiny.yml")
	got := withCode(lintJSON(t, Options{Path: path, Kind: KindDestiny}), codeVarCycle)
	if len(got) != 1 || got[0].Message != "var_cycle: a → b → c → a" || filepath.Base(got[0].File) != "vars.yml" {
		t.Errorf("want one var_cycle a → b → c → a in vars.yml, got %+v", got)
	}
}

// scenarioTasks is a scenario carrying the given task list (YAML, indented under
// `tasks:`) and scenario vars.
func scenarioTasks(scenarioVars, tasks string) string {
	s := "name: create\ninput: {}\n"
	if scenarioVars != "" {
		s += "vars:\n" + scenarioVars
	}
	return s + "tasks:\n" + tasks
}

// TestScenarioView_BlockVarsReachTheTask — a block's vars are merged into each child
// task's rung, as render merges them, so a cycle between a block var and a task var
// exists only in the merged view.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in walkTaskVars hand the
// block's children `inherited` instead of `vars` — this goes red.
func TestScenarioView_BlockVarsReachTheTask(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"scenario/create/main.yml": scenarioTasks("", "  - name: group\n    vars:\n      a: \"${ vars.b }\"\n    block:\n"+
			"      - name: write\n        module: core.file.present\n        vars:\n          b: \"${ vars.a }\"\n"+
			"        params:\n          path: /tmp/x\n          content: x\n"),
	})
	got := withCode(scenarioDiags(t, root, "create"), codeVarCycle)
	if len(got) != 1 || got[0].Message != "var_cycle: a → b → a" {
		t.Errorf("want var_cycle a → b → a, got %+v", got)
	}
}

// TestScenarioView_LoopBindingCompiles — a task's own vars may read its loop binding,
// named by `as:`/`index_as:` or the default `item`.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make withLoop return scope
// unchanged — this goes red.
func TestScenarioView_LoopBindingCompiles(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"scenario/create/main.yml": scenarioTasks("",
			"  - name: named\n    module: core.file.present\n    loop:\n      items: [1, 2]\n      as: n\n      index_as: i\n"+
				"    vars:\n      p: \"${ n }-${ i }\"\n    params:\n      path: /tmp/x\n      content: \"${ vars.p }\"\n"+
				"  - name: default\n    module: core.file.present\n    loop:\n      items: [1]\n"+
				"    vars:\n      q: \"${ item }\"\n    params:\n      path: /tmp/y\n      content: \"${ vars.q }\"\n"),
	})
	if got := varCodes(scenarioDiags(t, root, "create")); len(got) != 0 {
		t.Errorf("a loop binding is in scope for the task's vars, got %+v", got)
	}
}

// TestScenarioView_KeeperTaskVarsAreHostFree — a keeper-side task's vars compile in
// the context render gives them, host-free: soulprint.self there is var_invalid,
// while the same text in a host task's vars compiles.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make scenarioTaskScope
// return the host-task scope for every task — this goes red.
func TestScenarioView_KeeperTaskVarsAreHostFree(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"scenario/create/main.yml": scenarioTasks("",
			"  - name: on keeper\n    module: core.soul.registered\n    vars:\n      kh: \"${ soulprint.self.hostname }\"\n    params: {}\n"+
				"  - name: on hosts\n    module: core.file.present\n    vars:\n      hh: \"${ soulprint.self.hostname }\"\n"+
				"    params:\n      path: /tmp/x\n      content: x\n"),
	})
	got := withCode(scenarioDiags(t, root, "create"), codeVarInvalid)
	if len(got) != 1 || !strings.HasPrefix(got[0].Message, "vars.kh (task vars:)") {
		t.Errorf("want one var_invalid on the keeper task's kh, got %+v", got)
	}
}

// TestVarInvalid_ScenarioRungRefusesHostRootsAtAnyDepth — every root of point 6's
// closed list is refused in a scenario `vars:` definition when it sits in a string
// nested inside a map, not only at the top.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in compile() check a name's
// definition only when it is itself a string — this goes red.
func TestVarInvalid_ScenarioRungRefusesHostRootsAtAnyDepth(t *testing.T) {
	keys := map[string]string{
		"v":  "${ vault('secret/x#y') }",
		"g":  "${ generate_secret({}) }",
		"h":  "${ soulprint.hosts.size() }",
		"w":  "${ soulprint.hosts.where('true') }",
		"rh": "${ register.hosts.p }",
		"rs": "${ register.self.x }",
	}
	body := "  n:\n"
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		body += "    " + k + ": \"" + keys[k] + "\"\n"
	}
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":        "port: 6379\n",
		"scenario/create/main.yml": scenarioWithTaskVars(body, ""),
	})
	got := map[string]bool{}
	for _, d := range withCode(scenarioDiags(t, root, "create"), codeVarInvalid) {
		got[d.YAMLPath] = true
	}
	for k := range keys {
		if !got["$.vars.n."+k] {
			t.Errorf("want var_invalid at $.vars.n.%s, got %v", k, got)
		}
	}
	if len(got) != len(keys) {
		t.Errorf("want exactly %d findings, got %v", len(keys), got)
	}
}

// TestVarChecks_AFindingIsReportedOnce — a cycle of the scenario rung is in the
// run-level view and in every task view stacked on it; it is one finding.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the c.seen check from
// emit — this goes red.
func TestVarChecks_AFindingIsReportedOnce(t *testing.T) {
	task := func(name string) string {
		return "  - name: " + name + "\n    module: core.file.present\n    vars:\n      own: \"" + name + "\"\n" +
			"    params:\n      path: /tmp/" + name + "\n      content: \"${ vars.own }\"\n"
	}
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":        "port: 6379\n",
		"scenario/create/main.yml": scenarioTasks("  a: \"${ vars.b }\"\n  b: \"${ vars.a }\"\n", task("one")+task("two")),
	})
	if got := withCode(scenarioDiags(t, root, "create"), codeVarCycle); len(got) != 1 {
		t.Errorf("want the scenario rung's cycle once, got %+v", got)
	}
}
