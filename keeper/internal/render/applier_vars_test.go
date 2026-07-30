package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// An applier's task-level `vars:` reach the env that renders `apply.input`
// (NIM-336). There are TWO entrances: the applier's own key, and a `block:`
// above it whose `vars:` mergeBlockInheritance merges into every descendant,
// an apply: one included. The second is why refusing the key was never enough
// — it is written on the block, where §6.5 explicitly allows it, so no offline
// validator can see it.
//
// ★ What this is NOT: a wrong value. The scenario pass has no file-vars base,
// so before the fix `${ vars.x }` in apply.input failed the render with "no
// such key" rather than resolving to something else. The gap is that a block
// documented as passing `vars:` to its descendants did not pass them to one
// kind of descendant. TestApplier_NoVarsLayerWithoutTaskVars pins that.

// varsDestiny takes one input and echoes it into a command, so a test can read
// back exactly what crossed the boundary.
func varsDestiny() *ResolvedDestiny {
	return &ResolvedDestiny{
		Name:  "d",
		Input: config.InputSchemaMap{"pause": {Type: "string", Required: true}},
		Tasks: []config.Task{{
			Name:   "echo the pause",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "sleep ${ input.pause }"}},
		}},
	}
}

// renderWithVars runs a one-host scenario made of the given tasks and returns
// the rendered plan.
func renderWithVars(t *testing.T, tasks []config.Task) ([]*RenderedTask, error) {
	t.Helper()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	out, _, err := p.Render(context.Background(), RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: tasks},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     staticResolver{varsDestiny()},
	})
	return out, err
}

// Entrance 1 — the applier's own `vars:`. Before NIM-336 resolveApplyInput
// built its env with hostVars alone and never called resolveTaskVars, so the
// key was accepted and dropped.
func TestApplier_OwnVarsReachApplyInput(t *testing.T) {
	tasks, err := renderWithVars(t, []config.Task{{
		Name:  "apply-step",
		Vars:  map[string]any{"pause": "5"},
		Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"pause": "${ vars.pause }"}},
	}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got, want := cmdOf(t, tasks[0]), "sleep 5"; got != want {
		t.Errorf("cmd = %q, want %q — the applier's own vars: did not reach apply.input", got, want)
	}
}

// Entrance 2 — inherited from a `block:`. mergeBlockInheritance merges the
// block's vars into the apply descendant; this pins that they survive the trip
// into the destiny's input.
func TestApplier_BlockVarsReachApplyInput(t *testing.T) {
	tasks, err := renderWithVars(t, []config.Task{{
		Name: "grp",
		Vars: map[string]any{"pause": "7"},
		Block: &config.BlockTask{Block: []config.Task{{
			Name:  "apply-step",
			Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"pause": "${ vars.pause }"}},
		}}},
	}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got, want := cmdOf(t, tasks[0]), "sleep 7"; got != want {
		t.Errorf("cmd = %q, want %q — a block's vars: did not reach its apply descendant's input", got, want)
	}
}

// ★ Pre-fix behaviour, pinned so nobody re-derives it wrongly: the scenario
// pass has NO file-vars base (fileVarsForHost is destiny-only, Variant A of
// vars.md), so an applier that declares no `vars:` has no `vars.*` layer at
// all. Before this fix that was true of EVERY applier — `${ vars.x }` in
// apply.input failed with "no such key". The loss was loud, not silent: this
// ticket is about a capability the DSL documents and did not deliver, not about
// a wrong value being used.
func TestApplier_NoVarsLayerWithoutTaskVars(t *testing.T) {
	_, err := renderWithVars(t, []config.Task{{
		Name:  "apply-step",
		Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"pause": "${ vars.pause }"}},
	}})
	if err == nil {
		t.Fatal("Render succeeded; an applier with no vars: has no vars.* layer on the scenario pass")
	}
	if !strings.Contains(err.Error(), "pause") {
		t.Errorf("error = %v, want it to name the unresolvable key", err)
	}
}

// A local task-var wins over a same-named one from the block above it (Variant
// A, vars.md: the more local scope wins). mergeVars decides that before render;
// the applier path must not quietly reverse it.
func TestApplier_OwnVarsShadowBlockVars(t *testing.T) {
	tasks, err := renderWithVars(t, []config.Task{{
		Name: "grp",
		Vars: map[string]any{"pause": "99"},
		Block: &config.BlockTask{Block: []config.Task{{
			Name:  "apply-step",
			Vars:  map[string]any{"pause": "5"},
			Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"pause": "${ vars.pause }"}},
		}}},
	}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cmdOf(t, tasks[0]); got != "sleep 5" {
		t.Errorf("cmd = %q, want %q — the block's vars.pause answered instead of the applier's own", got, "sleep 5")
	}
}

// An unknown `vars.<name>` must still fail loudly rather than render empty —
// the fix must not turn a typo into a silent empty string.
func TestApplier_UnknownVarRefStillFails(t *testing.T) {
	_, err := renderWithVars(t, []config.Task{{
		Name:  "apply-step",
		Vars:  map[string]any{"pause": "5"},
		Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"pause": "${ vars.nope }"}},
	}})
	if err == nil {
		t.Fatal("Render succeeded; want an error for an unknown vars.nope in apply.input")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want it to name the unknown var", err)
	}
}

// A var-to-var cycle inside the applier's own layer is reported, not spun on.
func TestApplier_VarCycleReported(t *testing.T) {
	_, err := renderWithVars(t, []config.Task{{
		Name:  "apply-step",
		Vars:  map[string]any{"a": "${ vars.b }", "b": "${ vars.a }"},
		Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{"pause": "${ vars.a }"}},
	}})
	if err == nil {
		t.Fatal("Render succeeded; want an error for a vars cycle on the applier")
	}
	if !strings.Contains(err.Error(), "apply-step") {
		t.Errorf("error = %v, want it to name the applier task", err)
	}
}
