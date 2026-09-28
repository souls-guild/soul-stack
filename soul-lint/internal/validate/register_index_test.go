package validate

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// The offline half of NIM-909. The runtime guard's own tests live in
// shared/cel/register_index_test.go; what these pin is that the linter sees the same
// cells the compute rules do, reports the YAML path the author has to edit, and does
// NOT report an index that reaches into a register's payload.

// TestRegisterIndexForm_ReportedAtEveryCell — ★ GUARD: every expression-bearing cell
// of a scenario, whatever its compute scope. The flow-control keys are here and not
// excluded (as they are for the name half of the compute rule) because the compile-time
// refusal is unconditional — and the `when:` row is the one that repays it most: there
// the index form is mis-classified as a static predicate and fails the whole RUN at
// render (see the header of shared/cel/register_index.go, key by key).
//
// Mutation: move the registerIndex call in [computeChecker.check] below the
// `scope != cel.ComputeAvailable` branch's return — the flow-control, loop-axis and
// coven-list rows then go unreported.
func TestRegisterIndexForm_ReportedAtEveryCell(t *testing.T) {
	cases := []struct {
		name string
		task config.Task
		want string
	}{
		{
			name: "params",
			task: config.Task{
				Name:   "act",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": `echo ${ register["probe"].stdout }`}},
			},
			want: "register_index_form $.tasks[0].params.cmd",
		},
		{
			name: "where",
			task: config.Task{
				Name:   "act",
				Where:  `register["probe"].stdout == "master"`,
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "register_index_form $.tasks[0].where",
		},
		{
			name: "when (flow control)",
			task: config.Task{
				Name:   "act",
				When:   `register["probe"].changed`,
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "register_index_form $.tasks[0].when",
		},
		{
			name: "loop items",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: `${ register["probe"].lines }`, As: "i"},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "register_index_form $.tasks[0].loop.items",
		},
		{
			name: "task vars",
			task: config.Task{
				Name:   "act",
				Vars:   map[string]any{"role": `${ register["probe"].stdout }`},
				Module: &config.ModuleTask{Module: "core.exec.run"},
			},
			want: "register_index_form $.tasks[0].vars.role",
		},
		{
			name: "apply input",
			task: config.Task{
				Name:  "roll",
				Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"addr": `${ register["probe"].stdout }`}},
			},
			want: "register_index_form $.tasks[0].apply.input.addr",
		},
		{
			name: "the per-host map, name indexed",
			task: config.Task{
				Name:   "capture",
				On:     config.KeeperTarget,
				Module: &config.ModuleTask{Module: "core.state.set", Params: map[string]any{"value": `${ register.hosts["addr"] }`}},
			},
			want: "register_index_form $.tasks[0].params.value",
		},
		{
			name: "inside a block child",
			task: config.Task{
				Name: "wrap",
				Block: &config.BlockTask{Block: []config.Task{{
					Name:   "act",
					Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": `echo ${ register["probe"].stdout }`}},
				}}},
			},
			want: "register_index_form $.tasks[0].block[0].params.cmd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codesOf(t, computeScn([]config.Task{tc.task}))
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestRegisterIndexForm_IndexIntoAPayloadIsSilent — ★ GUARD on the rule's width,
// offline half. Both of these name the register first, so the reference is extractable
// and the run is ordered by it; the index reaches the register's own payload
// (`register.<name>["key"]`) or the SID of the per-host map
// (`register.hosts.<name>["<sid>"]`, NIM-711).
//
// Mutation: in [cel.Engine.ExpressionIndexesRegisterName] / its interpolation sibling,
// answer from the coarse pre-test (`register` + `[`) instead of the AST.
func TestRegisterIndexForm_IndexIntoAPayloadIsSilent(t *testing.T) {
	tasks := []config.Task{
		{
			Name:   "act",
			Where:  `register.probe["stdout"] == "master"`,
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": `echo ${ register.probe["sha256"] }`}},
		},
		{
			Name:   "capture",
			On:     config.KeeperTarget,
			Module: &config.ModuleTask{Module: "core.state.set", Params: map[string]any{"value": `${ register.hosts.addr["node-a"] }`}},
		},
	}
	if got := codesOf(t, computeScn(tasks)); len(got) != 0 {
		t.Fatalf("got %v, want no diagnostics", got)
	}
}

// TestRegisterIndexForm_TheCaptureMatchHintDoesNotSendTheAuthorInCircles — ★ GUARD: a
// capture's bare `match:` predicate is re-evaluated per collection element against
// elem/value and nothing else, so the register root is absent there whichever spelling
// is used. The ordinary hint ("write `register.<name>`") would buy the author a second
// `no such key` — the exact inversion NIM-619 was filed about, re-committed in a new
// rule. The finding still fires; only the hint changes.
//
// Mutation: drop the ComputeOutOfScopeStateMatch arm of [registerIndexHint], or pass a
// constant scope from [computeChecker.check].
func TestRegisterIndexForm_TheCaptureMatchHintDoesNotSendTheAuthorInCircles(t *testing.T) {
	capture := config.Task{
		Name: "record",
		On:   config.KeeperTarget,
		Module: &config.ModuleTask{Module: "core.state.add", Params: map[string]any{
			"path":  "$.users",
			"match": `register["probe"].sid == elem.sid`,
		}},
	}
	diags := computeDiagnostics("scn.yml", computeScn([]config.Task{capture}), true)
	if len(diags) != 1 || diags[0].Code != "register_index_form" {
		t.Fatalf("got %+v, want one register_index_form", diags)
	}
	if strings.Contains(diags[0].Hint, "invisible to reference extraction") {
		t.Fatalf("hint = %q, want the match:-specific advice, not the generic rewrite", diags[0].Hint)
	}
	if !strings.Contains(diags[0].Hint, "${ register.<name>.<field> }") {
		t.Fatalf("hint = %q, want it to name the route that works here", diags[0].Hint)
	}
}

// TestRegisterIndexForm_ReportedWithAnUnresolvedComputeBlock — the compute BLOCK's own
// cells are walked even when the declared set is incomplete (a covenant fragment that
// failed to resolve): the compute name rule is what an incomplete set would make lie,
// and this rule does not read the set at all.
//
// Mutation: restore the `if c.declared == nil { return }` early return in
// [computeChecker.computeBlock].
func TestRegisterIndexForm_ReportedWithAnUnresolvedComputeBlock(t *testing.T) {
	scn := &config.ScenarioManifest{
		Name:    "t",
		Compute: config.ComputeBlock{{Name: "role", Value: `${ register["probe"].stdout }`}},
	}
	diags := computeDiagnostics("scn.yml", scn, false /* namesResolved */)
	if len(diags) != 1 || diags[0].Code != "register_index_form" || diags[0].YAMLPath != "$.compute.role" {
		t.Fatalf("got %+v, want one register_index_form at $.compute.role", diags)
	}
}
