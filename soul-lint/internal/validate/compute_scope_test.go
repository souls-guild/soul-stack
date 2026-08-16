package validate

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The offline half of NIM-619. The runtime guard's own tests live in
// shared/cel/scope_test.go and keeper/internal/render/compute_scope_guard_test.go;
// what these pin is where the linter looks, where it deliberately does NOT, and
// that it agrees with the engine on what counts as a reference at all.

func computeScopeCodes(t *testing.T, tasks []config.Task) []string {
	t.Helper()
	var out []string
	for _, d := range computeScopeDiagnostics("scn.yml", tasks) {
		if d.Code == "compute_out_of_scope" {
			out = append(out, d.YAMLPath)
		}
	}
	return out
}

func execTask(name string) config.Task {
	return config.Task{
		Name:   name,
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "/usr/bin/true"}},
	}
}

// TestComputeScope_FlaggedContexts — the three contexts the ticket names, each
// reported at the YAML path the author has to go and edit.
func TestComputeScope_FlaggedContexts(t *testing.T) {
	cases := []struct {
		name string
		task config.Task
		want string
	}{
		{
			name: "keeper params",
			task: config.Task{
				Name: "seed",
				On:   config.KeeperTarget,
				Module: &config.ModuleTask{
					Module: "core.soul.registered",
					Params: map[string]any{"sid": "node-${ compute.node_count }.example.com"},
				},
			},
			want: "$.tasks[0].params.sid",
		},
		{
			name: "keeper params nested in a map",
			task: config.Task{
				Name: "seed",
				On:   config.KeeperTarget,
				Module: &config.ModuleTask{
					Module: "core.soul.registered",
					Params: map[string]any{"meta": map[string]any{"n": "${ compute.node_count }"}},
				},
			},
			want: "$.tasks[0].params.meta.n",
		},
		{
			name: "keeper params nested in a list",
			task: config.Task{
				Name: "seed",
				On:   config.KeeperTarget,
				Module: &config.ModuleTask{
					Module: "core.soul.registered",
					Params: map[string]any{"coven": []any{"web", "${ compute.node_count }"}},
				},
			},
			want: "$.tasks[0].params.coven[1]",
		},
		{
			// vars: layer onto the very same keeper context (resolveTaskVars), so
			// routing the reference through them is not a way around the rule.
			name: "keeper vars",
			task: config.Task{
				Name:   "seed",
				On:     config.KeeperTarget,
				Vars:   map[string]any{"n": "${ compute.node_count }"},
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "${ vars.n }"}},
			},
			want: "$.tasks[0].vars.n",
		},
		{
			name: "loop items",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: "${ compute.node_count }", As: "i"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].loop.items",
		},
		{
			name: "loop items as a list element",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: []any{"a", "${ compute.node_count }"}, As: "i"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].loop.items[1]",
		},
		{
			// An expression key: the whole string is CEL, with no `${ }` wrapper.
			name: "loop when",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: []any{"a"}, As: "i", When: "compute.node_count > 1"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].loop.when",
		},
		{
			name: "on covens",
			task: config.Task{
				Name:   "roll",
				On:     []any{"${ 'web-' + string(compute.node_count) }"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].on[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeScopeCodes(t, []config.Task{tc.task})
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("yaml paths = %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}

// TestComputeScope_NotFlagged — the contexts that DO have the namespace, and the
// strings that only look like a reference. Every entry here is something a regex
// rule would get wrong, which is why the rule asks shared/cel instead.
func TestComputeScope_NotFlagged(t *testing.T) {
	cases := []struct {
		name  string
		tasks []config.Task
	}{
		{
			name: "soul-side params — the namespace is in scope",
			tasks: []config.Task{{
				Name:   "use",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.node_count }"}},
			}},
		},
		{
			name: "soul-side where: — in scope",
			tasks: []config.Task{{
				Name:   "use",
				Where:  "compute.node_count > 1",
				Module: &config.ModuleTask{Module: "core.exec.run"},
			}},
		},
		{
			name: "prose in a keeper param",
			tasks: []config.Task{{
				Name:   "seed",
				On:     config.KeeperTarget,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"note": "compute the digest first"}},
			}},
		},
		{
			name: "a coven label that contains the word",
			tasks: []config.Task{{
				Name:   "roll",
				On:     []any{"compute-cluster"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			}},
		},
		{
			name: "an input field whose name starts with it",
			tasks: []config.Task{{
				Name:   "seed",
				On:     config.KeeperTarget,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "${ input.compute_timeout }"}},
			}},
		},
		{
			name: "the word inside a CEL string literal is a host field",
			tasks: []config.Task{{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: `${ soulprint.hosts.where("compute == 4") }`, As: "h"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			}},
		},
		{
			// The linter must not invent diagnostics out of text the compiler will
			// reject anyway, with a position soul-lint cannot produce here.
			name: "unparseable interpolation is the compiler's to report",
			tasks: []config.Task{{
				Name:   "seed",
				On:     config.KeeperTarget,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "${ compute. }"}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeScopeCodes(t, tc.tasks); len(got) != 0 {
				t.Fatalf("flagged %v, expected nothing", got)
			}
		})
	}
}

// TestComputeScope_BlockChildDoesNotInheritOn is the case that decides the shape of
// the walk. `on:` is NOT inherited by a block's children — mergeBlockInheritance
// carries when/where/vars/requisites and never On, so a child of an `on: keeper`
// block renders in the ordinary per-host context, where compute IS readable.
// Propagating it here would flag a scenario that works.
func TestComputeScope_BlockChildDoesNotInheritOn(t *testing.T) {
	tasks := []config.Task{{
		Name: "group",
		On:   config.KeeperTarget,
		Block: &config.BlockTask{Block: []config.Task{{
			Name:   "child",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.node_count }"}},
		}}},
	}}
	if got := computeScopeCodes(t, tasks); len(got) != 0 {
		t.Fatalf("a block child inherited its parent's on: and got flagged at %v", got)
	}
}

// TestComputeScope_BlockChildWithItsOwnOn — the other half: the walk does recurse,
// so a child that declares `on: keeper` itself is flagged at its nested path.
func TestComputeScope_BlockChildWithItsOwnOn(t *testing.T) {
	tasks := []config.Task{{
		Name: "group",
		Block: &config.BlockTask{Block: []config.Task{
			execTask("first"),
			{
				Name:   "child",
				On:     config.KeeperTarget,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "${ compute.node_count }"}},
			},
		}},
	}}
	got := computeScopeCodes(t, tasks)
	if len(got) != 1 || got[0] != "$.tasks[0].block[1].params.sid" {
		t.Fatalf("yaml paths = %v, want exactly [$.tasks[0].block[1].params.sid]", got)
	}
}

// TestComputeScope_DiagnosticNamesTheNamespace — the message is the deliverable.
// It has to say the namespace is absent, name the context, and carry the way out;
// "no such key" is the wording this ticket exists to stop producing.
func TestComputeScope_DiagnosticNamesTheNamespace(t *testing.T) {
	diags := computeScopeDiagnostics("scn.yml", []config.Task{{
		Name:   "seed",
		On:     config.KeeperTarget,
		Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "${ compute.node_count }"}},
	}})
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %d, want 1", len(diags))
	}
	d := diags[0]
	if d.Code != "compute_out_of_scope" {
		t.Fatalf("code = %q", d.Code)
	}
	if strings.Contains(d.Message, "no such key") {
		t.Fatalf("the message blames a key: %s", d.Message)
	}
	for _, want := range []string{"compute", "namespace", "on: keeper"} {
		if !strings.Contains(d.Message, want) {
			t.Fatalf("the message does not mention %q: %s", want, d.Message)
		}
	}
	if d.Hint == "" {
		t.Fatal("no hint — the author is told what is wrong but not what to write instead")
	}
	// The wording comes from shared/cel, so the offline rule and the runtime guard
	// cannot drift apart on the first rewording of either.
	wantCtx, wantHint := cel.ComputeOutOfScopeKeeperTask.Describe()
	if !strings.Contains(d.Message, wantCtx) || d.Hint != wantHint {
		t.Fatalf("diagnostic does not reuse cel.ComputeScope.Describe():\n message=%s\n hint=%s", d.Message, d.Hint)
	}
}

// TestComputeScope_StableOrder — params: decodes into a map, and a map's range
// order is randomised per run. Without the sort, the same scenario would report its
// findings in a different order every invocation.
func TestComputeScope_StableOrder(t *testing.T) {
	tasks := []config.Task{{
		Name: "seed",
		On:   config.KeeperTarget,
		Module: &config.ModuleTask{
			Module: "core.soul.registered",
			Params: map[string]any{
				"alpha": "${ compute.a }",
				"beta":  "${ compute.b }",
				"gamma": "${ compute.c }",
				"delta": "${ compute.d }",
			},
		},
	}}
	want := []string{
		"$.tasks[0].params.alpha",
		"$.tasks[0].params.beta",
		"$.tasks[0].params.delta",
		"$.tasks[0].params.gamma",
	}
	for i := 0; i < 8; i++ {
		got := computeScopeCodes(t, tasks)
		if len(got) != len(want) {
			t.Fatalf("run %d: got %v, want %v", i, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("run %d: order = %v, want %v", i, got, want)
			}
		}
	}
}

// TestComputeScope_WiredIntoScenarioValidation — the rule can be perfect and never
// be called. These two fixtures run through the public entry point: the broken one
// must report the code, the golden one must stay silent.
func TestComputeScope_WiredIntoScenarioValidation(t *testing.T) {
	runExpectJSONSubset(t, "../../testdata/scenario-broken/scenario-compute-out-of-scope.yml",
		KindScenario, ExitHasErrors, []string{"compute_out_of_scope"})
	runExpect(t, "../../testdata/scenario-golden/compute-in-scope.yml", KindScenario, false, ExitOK, nil)
}
