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

// computeScn wraps a task list in a manifest declaring `names` in `compute:`.
// Declaring them is what separates the two rules in a test: with the name present,
// a diagnostic can only be about scope.
func computeScn(tasks []config.Task, names ...string) *config.ScenarioManifest {
	block := make(config.ComputeBlock, 0, len(names))
	for _, n := range names {
		block = append(block, config.ComputeVar{Name: n, Value: "${ 1 + 2 }"})
	}
	return &config.ScenarioManifest{Name: "t", Compute: block, Tasks: tasks}
}

// codesOf returns "<code> <yaml-path>" per diagnostic, in order — the two things a
// caller acts on, and the order they are printed in.
func codesOf(t *testing.T, scn *config.ScenarioManifest) []string {
	t.Helper()
	var out []string
	for _, d := range computeDiagnostics("scn.yml", scn, true) {
		out = append(out, d.Code+" "+d.YAMLPath)
	}
	return out
}

func execTask(name string) config.Task {
	return config.Task{
		Name:   name,
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "/usr/bin/true"}},
	}
}

// TestComputeScope_FlaggedContexts — the contexts that read without the namespace,
// each reported at the YAML path the author has to go and edit. The name is
// declared in every case, so nothing here can be the name rule in disguise.
//
// The flow-control four (when/changed_when/failed_when/until) are here and not in
// the name test on purpose: Keeper never renders them — they are copied into the
// RenderedTask verbatim (pipeline.renderKeeperTask) and evaluated by Soul in
// cel.NewFlowControl, whose env has no `compute` at all. Checking a NAME there
// would tell the author "the namespace exists here, this name does not" about a
// namespace that does not exist there, which is the exact inversion NIM-619 was
// filed to remove.
func TestComputeScope_FlaggedContexts(t *testing.T) {
	cases := []struct {
		name string
		task config.Task
		want string
	}{
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
		{
			name: "when",
			task: config.Task{
				Name:   "use",
				When:   "compute.node_count > 1",
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].when",
		},
		{
			name: "changed_when",
			task: config.Task{
				Name:        "use",
				ChangedWhen: "compute.node_count > 1",
				Module:      &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].changed_when",
		},
		{
			name: "failed_when",
			task: config.Task{
				Name:       "use",
				FailedWhen: "compute.node_count > 1",
				Module:     &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].failed_when",
		},
		{
			name: "retry until",
			task: config.Task{
				Name:   "use",
				Retry:  &config.RetrySpec{Count: 2, Until: "compute.node_count > 1"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].retry.until",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codesOf(t, computeScn([]config.Task{tc.task}, "node_count"))
			want := "compute_out_of_scope " + tc.want
			if len(got) != 1 || got[0] != want {
				t.Fatalf("diagnostics = %v, want exactly [%s]", got, want)
			}
		})
	}
}

// TestComputeScope_KeeperTaskIsInScope is the reversal this ticket landed on
// (variant B): `on: keeper` renders in the run-level context, which is exactly
// where compute was resolved, so reading it there is correct and the linter must
// say nothing. The counterpart at the render layer — that the task actually SEES
// the value — is render.TestComputeScope_KeeperTaskReadsCompute.
func TestComputeScope_KeeperTaskIsInScope(t *testing.T) {
	tasks := []config.Task{{
		Name: "seed",
		On:   config.KeeperTarget,
		Vars: map[string]any{"n": "${ compute.node_count }"},
		Module: &config.ModuleTask{
			Module: "core.soul.registered",
			Params: map[string]any{"sid": "node-${ compute.node_count }.example.com"},
		},
	}}
	if got := codesOf(t, computeScn(tasks, "node_count")); len(got) != 0 {
		t.Fatalf("a keeper task reading a declared name was flagged: %v", got)
	}
}

// TestComputeUnknownName_FlaggedWhereTheNamespaceExists — the inverted rule. Every
// context here HAS the namespace, so the only thing that can be wrong is the name,
// and that is the mistake the runtime's "no such key" was always describing.
func TestComputeUnknownName_FlaggedWhereTheNamespaceExists(t *testing.T) {
	cases := []struct {
		name string
		task config.Task
		want string
	}{
		{
			name: "soul-side params",
			task: config.Task{
				Name:   "use",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.node_cont }"}},
			},
			want: "$.tasks[0].params.cmd",
		},
		{
			name: "keeper params",
			task: config.Task{
				Name:   "seed",
				On:     config.KeeperTarget,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "${ compute.node_cont }"}},
			},
			want: "$.tasks[0].params.sid",
		},
		{
			name: "task vars",
			task: config.Task{
				Name:   "use",
				Vars:   map[string]any{"n": "${ compute.node_cont }"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].vars.n",
		},
		{
			name: "where",
			task: config.Task{
				Name:   "use",
				Where:  "compute.node_cont > 1",
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "$.tasks[0].where",
		},
		{
			name: "assert that",
			task: config.Task{
				Name:   "check",
				Assert: &config.AssertSpec{That: []string{"compute.node_cont > 1"}},
			},
			want: "$.tasks[0].assert.that[0]",
		},
		{
			name: "apply input",
			task: config.Task{
				Name:  "hand off",
				Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"n": "${ compute.node_cont }"}},
			},
			want: "$.tasks[0].apply.input.n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codesOf(t, computeScn([]config.Task{tc.task}, "node_count"))
			want := "compute_unknown_name " + tc.want
			if len(got) != 1 || got[0] != want {
				t.Fatalf("diagnostics = %v, want exactly [%s]", got, want)
			}
		})
	}
}

// TestComputeScope_FlowControlSaysWhatToWriteInstead — a rule that only refuses is
// half a rule here, because the way out is not obvious: flow_context carries
// `vars`, and a task's vars: ARE rendered with the namespace. So the fix is one
// line, and the diagnostic has to say so. The paired render-side proof that the
// detour actually works is render.TestComputeScope_KeeperTaskVarsReadCompute.
func TestComputeScope_FlowControlSaysWhatToWriteInstead(t *testing.T) {
	diags := computeDiagnostics("scn.yml", computeScn([]config.Task{{
		Name:   "use",
		When:   "compute.node_count > 1",
		Module: &config.ModuleTask{Module: "core.exec.run"},
	}}, "node_count"), true)
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want exactly one", diags)
	}
	if !strings.Contains(diags[0].Message, "flow-control") {
		t.Fatalf("message does not name the context: %s", diags[0].Message)
	}
	if !strings.Contains(diags[0].Hint, "vars") {
		t.Fatalf("hint does not point at the detour through vars:: %s", diags[0].Hint)
	}

	// And the detour itself is silent: the value is read where the namespace is,
	// the predicate reads the binding.
	clean := []config.Task{{
		Name:   "use",
		Vars:   map[string]any{"n": "${ compute.node_count }"},
		When:   "vars.n > 1",
		Module: &config.ModuleTask{Module: "core.exec.run"},
	}}
	if got := codesOf(t, computeScn(clean, "node_count")); len(got) != 0 {
		t.Fatalf("the documented detour was flagged: %v", got)
	}
}

// TestComputeUnknownName_DynamicReferenceSilencesTheCell — a reference whose name
// the source does not carry makes the whole cell unjudgeable, not just itself.
//
// The macro case is the one that would bite: names are extracted with macros OFF
// (Engine.parseNoMacro), so in `input.hosts.map(compute, compute.role)` — legal CEL,
// where `compute` is the comprehension variable and shadows the namespace — the
// walk reads `compute.role` as a namespace selection and would report `role` as an
// undeclared compute name on a scenario that is fine. What both cases share is an
// unpaired `compute` identifier, which is what the dynamic flag reports.
func TestComputeUnknownName_DynamicReferenceSilencesTheCell(t *testing.T) {
	for _, expr := range []string{
		"echo ${ compute[input.key] }",                // index by a non-literal
		"${ size(compute) }",                          // the bare namespace
		"${ input.hosts.map(compute, compute.role) }", // comprehension variable, shadows
		"${ compute.node_cont + size(compute) }",      // a real typo, but not judgeable here
	} {
		tasks := []config.Task{{
			Name:   "use",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": expr}},
		}}
		if got := codesOf(t, computeScn(tasks, "node_count")); len(got) != 0 {
			t.Fatalf("%s: got %v, want none (the name is not in the source)", expr, got)
		}
	}
}

// TestComputeUnknownName_DeclaredNamesArePassed — the same expressions with the
// name spelled right. A rule that flagged these would make `compute:` unusable.
func TestComputeUnknownName_DeclaredNamesArePassed(t *testing.T) {
	tasks := []config.Task{{
		Name:   "use",
		Where:  "compute.node_count > 1",
		Vars:   map[string]any{"n": "${ compute.node_count }"},
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.region }"}},
	}}
	if got := codesOf(t, computeScn(tasks, "node_count", "region")); len(got) != 0 {
		t.Fatalf("declared names were flagged: %v", got)
	}
}

// TestComputeUnknownName_DynamicReferenceIsLeftAlone — a reference whose name is
// not in the source. The rule reports what the author wrote; inventing "input.k"
// as a compute name would be a diagnostic about text that does not exist.
func TestComputeUnknownName_DynamicReferenceIsLeftAlone(t *testing.T) {
	for _, expr := range []string{"${ compute[input.k] }", "${ size(compute) }", "${ has(compute.node_count) }"} {
		tasks := []config.Task{{
			Name:   "use",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": expr}},
		}}
		if got := codesOf(t, computeScn(tasks, "node_count")); len(got) != 0 {
			t.Fatalf("%s: flagged %v, expected nothing", expr, got)
		}
	}
}

// TestComputeUnknownName_ForwardReferenceInTheBlock — entries resolve in
// declaration order (render.resolveCompute accumulates), so an entry reading one
// declared below it fails at run time with the same "no such key" as a typo.
func TestComputeUnknownName_ForwardReferenceInTheBlock(t *testing.T) {
	scn := &config.ScenarioManifest{
		Name: "t",
		Compute: config.ComputeBlock{
			{Name: "first", Value: "${ compute.second + 1 }"},
			{Name: "second", Value: "${ 1 + 2 }"},
			{Name: "third", Value: "${ compute.first + compute.second }"}, // both behind it: fine
		},
	}
	got := codesOf(t, scn)
	want := "compute_unknown_name $.compute.first"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("diagnostics = %v, want exactly [%s]", got, want)
	}
	d := computeDiagnostics("scn.yml", scn, true)[0]
	if !strings.Contains(d.Message, "declared LATER") {
		t.Fatalf("the message does not say the name comes later: %s", d.Message)
	}
	// Its own name is not in scope for an entry either — that is a self-reference,
	// and the index rule catches it the same way.
	self := &config.ScenarioManifest{Name: "t", Compute: config.ComputeBlock{
		{Name: "loop", Value: "${ compute.loop }"},
	}}
	if got := codesOf(t, self); len(got) != 1 {
		t.Fatalf("a self-referencing entry: %v, want one diagnostic", got)
	}
}

// TestComputeUnknownName_SilentWhenTheCovenantDidNotResolve — mergeComputeSections
// prepends a covenant fragment's entries, so an unresolved fragment leaves
// scn.Compute short of names the scenario legitimately reads. Reporting them as
// typos would be a linter blaming the author for a file it failed to open.
func TestComputeUnknownName_SilentWhenTheCovenantDidNotResolve(t *testing.T) {
	tasks := []config.Task{{
		Name:   "use",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.from_the_fragment }"}},
	}}
	scn := computeScn(tasks)
	scn.Extends = "covenant"

	if diags := computeDiagnostics("scn.yml", scn, false); len(diags) != 0 {
		t.Fatalf("names reported with an unresolved covenant: %v", diags)
	}
	// The scope rule does not depend on names, so it keeps working.
	scn.Tasks = append(scn.Tasks, config.Task{
		Name:   "fan",
		Loop:   &config.LoopSpec{Items: "${ compute.anything }", As: "i"},
		Module: &config.ModuleTask{Module: "core.exec.run"},
	})
	diags := computeDiagnostics("scn.yml", scn, false)
	if len(diags) != 1 || diags[0].Code != "compute_out_of_scope" {
		t.Fatalf("scope rule with an unresolved covenant: %v", diags)
	}
}

// TestComputeScope_NotFlagged — the strings that only look like a reference. Every
// entry here is something a regex rule would get wrong, which is why both rules ask
// shared/cel instead.
func TestComputeScope_NotFlagged(t *testing.T) {
	cases := []struct {
		name  string
		tasks []config.Task
	}{
		{
			name: "prose in a param",
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
			if got := codesOf(t, computeScn(tc.tasks, "node_count")); len(got) != 0 {
				t.Fatalf("flagged %v, expected nothing", got)
			}
		})
	}
}

// TestComputeScope_BlockChildDoesNotInheritOn is the case that decides the shape of
// the walk. `on:` is NOT inherited by a block's children — mergeBlockInheritance
// carries when/where/vars/requisites and never On. With keeper now in scope the
// stake is the loop axis: a child of a block is not on the axis either.
func TestComputeScope_BlockChildDoesNotInheritOn(t *testing.T) {
	tasks := []config.Task{{
		Name: "group",
		Loop: &config.LoopSpec{Items: []any{"a"}, As: "i"},
		Block: &config.BlockTask{Block: []config.Task{{
			Name:   "child",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.node_count }"}},
		}}},
	}}
	if got := codesOf(t, computeScn(tasks, "node_count")); len(got) != 0 {
		t.Fatalf("a block child was flagged for its parent's context at %v", got)
	}
}

// TestComputeScope_BlockChildIsWalked — the other half: the walk does recurse, so a
// child's own loop axis is flagged at its nested path.
func TestComputeScope_BlockChildIsWalked(t *testing.T) {
	tasks := []config.Task{{
		Name: "group",
		Block: &config.BlockTask{Block: []config.Task{
			execTask("first"),
			{
				Name:   "child",
				Loop:   &config.LoopSpec{Items: "${ compute.node_count }", As: "i"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
		}},
	}}
	got := codesOf(t, computeScn(tasks, "node_count"))
	want := "compute_out_of_scope $.tasks[0].block[1].loop.items"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("diagnostics = %v, want exactly [%s]", got, want)
	}
}

// TestComputeScope_DiagnosticNamesTheNamespace — the message is the deliverable.
// It has to say the namespace is absent, name the context, and carry the way out;
// "no such key" is the wording this ticket exists to stop producing.
func TestComputeScope_DiagnosticNamesTheNamespace(t *testing.T) {
	diags := computeDiagnostics("scn.yml", computeScn([]config.Task{{
		Name:   "fan",
		Loop:   &config.LoopSpec{Items: "${ compute.node_count }", As: "i"},
		Module: &config.ModuleTask{Module: "core.exec.run"},
	}}, "node_count"), true)
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
	for _, want := range []string{"compute", "namespace", "loop.items"} {
		if !strings.Contains(d.Message, want) {
			t.Fatalf("the message does not mention %q: %s", want, d.Message)
		}
	}
	if d.Hint == "" {
		t.Fatal("no hint — the author is told what is wrong but not what to write instead")
	}
	// The wording comes from shared/cel, so the offline rule and the runtime guard
	// cannot drift apart on the first rewording of either.
	wantCtx, wantHint := cel.ComputeOutOfScopeLoopAxis.Describe()
	if !strings.Contains(d.Message, wantCtx) || d.Hint != wantHint {
		t.Fatalf("diagnostic does not reuse cel.ComputeScope.Describe():\n message=%s\n hint=%s", d.Message, d.Hint)
	}
}

// TestComputeUnknownName_DiagnosticCarriesTheAlternatives — a misspelling is worth
// reporting only if the author can see what they meant to type.
func TestComputeUnknownName_DiagnosticCarriesTheAlternatives(t *testing.T) {
	diags := computeDiagnostics("scn.yml", computeScn([]config.Task{{
		Name:   "use",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.node_cont }"}},
	}}, "node_count", "region"), true)
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %d, want 1", len(diags))
	}
	d := diags[0]
	if d.Code != "compute_unknown_name" {
		t.Fatalf("code = %q", d.Code)
	}
	if !strings.Contains(d.Message, "node_cont") {
		t.Fatalf("the message does not name what was written: %s", d.Message)
	}
	for _, want := range []string{"node_count", "region"} {
		if !strings.Contains(d.Hint, want) {
			t.Fatalf("the hint does not list %q: %s", want, d.Hint)
		}
	}

	// No block at all: the hint has to say THAT, not list an empty set.
	empty := computeDiagnostics("scn.yml", computeScn([]config.Task{{
		Name:   "use",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ compute.anything }"}},
	}}), true)
	if len(empty) != 1 || !strings.Contains(empty[0].Hint, "no compute: block") {
		t.Fatalf("hint for a scenario without compute:: %v", empty)
	}
}

// TestComputeUnknownName_ReportedOncePerName — one value naming the same missing
// entry twice is one mistake, and a linter that says it twice trains its reader to
// skim.
func TestComputeUnknownName_ReportedOncePerName(t *testing.T) {
	got := codesOf(t, computeScn([]config.Task{{
		Name:   "use",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "${ compute.gone }-${ compute.gone }"}},
	}}, "node_count"))
	if len(got) != 1 {
		t.Fatalf("diagnostics = %v, want exactly one", got)
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
		"compute_unknown_name $.tasks[0].params.alpha",
		"compute_unknown_name $.tasks[0].params.beta",
		"compute_unknown_name $.tasks[0].params.delta",
		"compute_unknown_name $.tasks[0].params.gamma",
	}
	for i := 0; i < 8; i++ {
		got := codesOf(t, computeScn(tasks))
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

// stateScn builds a scenario whose only content is a state_changes list.
func stateScn(ops []config.StateChange, names ...string) *config.ScenarioManifest {
	scn := computeScn(nil, names...)
	scn.StateChanges = &config.StateChanges{IsList: true, Ops: ops}
	return scn
}

// TestComputeUnknownName_StateChangesAreWalked — state_changes renders over the
// scenario context (stateChangesVars render-side, stateOpVars merge-time), so the
// namespace is there and a misspelling is the author's mistake, not a scope one.
// Every key here is a separate CEL cell with its own evaluator.
func TestComputeUnknownName_StateChangesAreWalked(t *testing.T) {
	cases := []struct {
		name string
		op   config.StateChange
		want string
	}{
		{
			name: "set value",
			op:   config.StateChange{Verb: config.VerbSet, Field: "cfg", Value: "${ compute.node_cont }"},
			want: "compute_unknown_name $.state_changes[0].value",
		},
		{
			name: "nested value",
			op:   config.StateChange{Verb: config.VerbSet, Field: "cfg", Value: map[string]any{"a": []any{"${ compute.node_cont }"}}},
			want: "compute_unknown_name $.state_changes[0].value.a[0]",
		},
		{
			name: "map add key",
			op:   config.StateChange{Verb: config.VerbAdd, Field: "users", Key: "${ compute.node_cont }", Value: "x"},
			want: "compute_unknown_name $.state_changes[0].key",
		},
		{
			name: "modify patch",
			op:   config.StateChange{Verb: config.VerbModify, Field: "users", Patch: map[string]any{"acl": "${ compute.node_cont }"}},
			want: "compute_unknown_name $.state_changes[0].patch.acl",
		},
		{
			name: "modify match sees the context",
			op:   config.StateChange{Verb: config.VerbModify, Field: "users", Match: "elem.n == compute.node_cont"},
			want: "compute_unknown_name $.state_changes[0].match",
		},
		{
			name: "foreach collection",
			op:   config.StateChange{Verb: config.VerbForeach, In: "${ compute.node_cont }", As: "r"},
			want: "compute_unknown_name $.state_changes[0].in",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codesOf(t, stateScn([]config.StateChange{tc.op}, "node_count"))
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestComputeScope_AddMatchIsOutOfScope — the one state_changes cell that does NOT
// see the run context: an `add:` identity predicate is a pure function of elem and
// value (render.EvalStateMatch). A modify `match:` on the same field IS in scope,
// which is why the verb decides and not the key.
func TestComputeScope_AddMatchIsOutOfScope(t *testing.T) {
	got := codesOf(t, stateScn([]config.StateChange{
		{Verb: config.VerbAdd, Field: "replicas", Value: "x", Match: "elem == compute.node_count"},
	}, "node_count"))
	want := "compute_out_of_scope $.state_changes[0].match"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want [%s]", got, want)
	}

	// Same predicate, declared name, verb modify — nothing to report.
	if got := codesOf(t, stateScn([]config.StateChange{
		{Verb: config.VerbModify, Field: "replicas", Match: "elem == compute.node_count"},
	}, "node_count")); len(got) != 0 {
		t.Fatalf("modify match: got %v, want none", got)
	}
}

// TestComputeUnknownName_ForeachDoIsWalked — a foreach's `do:` holds the operations
// that actually touch state; stopping the walk at the foreach would leave the most
// common place for a typo unchecked.
func TestComputeUnknownName_ForeachDoIsWalked(t *testing.T) {
	got := codesOf(t, stateScn([]config.StateChange{{
		Verb: config.VerbForeach, In: "${ compute.node_count }", As: "r",
		Do: []config.StateChange{
			{Verb: config.VerbSet, Field: "cfg", Value: "${ compute.node_cont }"},
		},
	}}, "node_count"))
	want := "compute_unknown_name $.state_changes[0].do[0].value"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want [%s]", got, want)
	}
}

// TestComputeUnknownName_LegacyStateChangesMapForm — the deprecated map form is
// still accepted by the parser, so a scenario written that way must not silently
// skip the check.
func TestComputeUnknownName_LegacyStateChangesMapForm(t *testing.T) {
	scn := computeScn(nil, "node_count")
	scn.StateChanges = &config.StateChanges{Sets: map[string]string{"cfg": "${ compute.node_cont }"}}
	got := codesOf(t, scn)
	want := "compute_unknown_name $.state_changes.sets.cfg"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want [%s]", got, want)
	}
}

// TestComputeScope_WiredIntoScenarioValidation — the rules can be perfect and never
// be called. These fixtures run through the public entry point: the broken ones must
// report their code, the golden one must stay silent.
func TestComputeScope_WiredIntoScenarioValidation(t *testing.T) {
	runExpectJSONSubset(t, "../../testdata/scenario-broken/scenario-compute-out-of-scope.yml",
		KindScenario, ExitHasErrors, []string{"compute_out_of_scope"})
	runExpectJSONSubset(t, "../../testdata/scenario-broken/scenario-compute-unknown-name.yml",
		KindScenario, ExitHasErrors, []string{"compute_unknown_name"})
	runExpect(t, "../../testdata/scenario-golden/compute-in-scope.yml", KindScenario, false, ExitOK, nil)
}
