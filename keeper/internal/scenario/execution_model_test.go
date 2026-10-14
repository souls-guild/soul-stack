package scenario

import (
	"context"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ GUARD (NIM-952). A Runner built without a model runs linear, and only the
// staged constant brings the rollback model back.
//
// Mutation: return ExecutionStaged for anything but ExecutionLinear in
// executionModel → the zero and the unknown value run staged.
func TestRunner_ExecutionModelIsLinearUnlessStaged(t *testing.T) {
	for model, want := range map[ExecutionModel]ExecutionModel{
		"":              ExecutionLinear,
		ExecutionStaged: ExecutionStaged,
		ExecutionLinear: ExecutionLinear,
		"anything-else": ExecutionLinear,
	} {
		r := &Runner{deps: Deps{ExecutionModel: model}}
		if got := r.executionModel(); got != want {
			t.Errorf("Deps.ExecutionModel %q → %q, want %q", model, got, want)
		}
	}
}

// keeper.yml::scenario.execution_model reaches the Runner as a function read at
// every run start, so a reload takes the next run without a restart and wins over
// the static field.
//
// Mutation: read only Deps.ExecutionModel → the configured staged rollback runs
// linear.
func TestRunner_ExecutionModelFnIsReadPerRun(t *testing.T) {
	model := ExecutionStaged
	r := &Runner{deps: Deps{ExecutionModel: ExecutionLinear, ExecutionModelFn: func() ExecutionModel { return model }}}
	if got := r.executionModel(); got != ExecutionStaged {
		t.Fatalf("configured staged → %q, want staged", got)
	}
	model = ExecutionLinear
	if got := r.executionModel(); got != ExecutionLinear {
		t.Fatalf("after reload to linear → %q, want linear", got)
	}
	model = "staeged"
	if got := r.executionModel(); got != ExecutionLinear {
		t.Fatalf("unknown model %q → %q, want linear", model, got)
	}
}

// The event carries the model the run decided at its start; a spec that never
// went through run() carries no key rather than a made-up one.
func TestEmitRunCompleted_CarriesExecutionModel(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)
	spec := runCompletedSpec()
	spec.executionModel = ExecutionLinear
	r.emitRunCompleted(context.Background(), spec, runCompletedStatusFailed, nil, nil, nil, slog.New(slog.DiscardHandler))
	r.emitRunCompleted(context.Background(), runCompletedSpec(), runCompletedStatusFailed, nil, nil, nil, slog.New(slog.DiscardHandler))
	if len(aw.events) != 2 {
		t.Fatalf("events = %d, want 2", len(aw.events))
	}
	if got := aw.events[0].Payload["execution_model"]; got != "linear" {
		t.Errorf("execution_model = %v, want linear", got)
	}
	if got, ok := aw.events[1].Payload["execution_model"]; ok {
		t.Errorf("execution_model = %v on a spec run() never set, want no key", got)
	}
}

// ★ GUARD (ADR-0091, decision 1). With linear numbering the Passage
// selection the dispatcher already does sends a default block's children in two
// messages, one per step, and a group in one; under staged numbering the default
// block is one message.
//
// Mutation: drop the per-child stamp in walkBlockChildren → b, rendered at its
// step, reads Passage 0, and step 1 carries nothing.
func TestLinearSteps_DefaultBlockChildrenTravelAlone(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	mod := func(name string) config.Task {
		return config.Task{Name: name, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}}
	}
	pair := func(group bool) config.Task {
		task := config.Task{Name: "pair", Block: &config.BlockTask{Block: []config.Task{mod("a"), mod("b")}}}
		if group {
			task.Serial = 1
			task.Settings = &config.ScenarioSettings{SerialScope: new(config.SerialScopeBlock)}
		}
		return task
	}
	hosts := []*topology.HostFacts{{SID: "a.example.com", Coven: []string{"svc"}}}

	carried := func(task config.Task, model ExecutionModel) [][]string {
		m := &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}}
		in := render.RenderInput{Scenario: m, Incarnation: render.IncarnationMeta{ID: "svc"}, Hosts: hosts}
		count := 1
		if model == ExecutionLinear {
			plan, steps := config.LinearPassage(m.Tasks, m.Settings)
			in.TaskPassage, in.Steps, in.HeldTargets, count = plan.TaskPassage, steps, render.NewHeldTargets(), plan.Count
		}
		var out [][]string
		for p := 0; p < count; p++ {
			in.ActivePassage = p
			tasks, plans, err := render.NewPipeline(nil, engine, nil, nil).Render(context.Background(), in)
			if err != nil {
				t.Fatalf("Render at %d: %v", p, err)
			}
			pt, pp := tasksForPassage(tasks, plans, p)
			var names []string
			for _, rt := range groupByHost(pt, pp)["a.example.com"] {
				names = append(names, rt.Name)
			}
			out = append(out, names)
		}
		return out
	}

	if got := carried(pair(false), ExecutionLinear); len(got) != 2 || len(got[0]) != 1 || got[0][0] != "a" || len(got[1]) != 1 || got[1][0] != "b" {
		t.Errorf("linear default block messages = %v, want [[a] [b]]", got)
	}
	if got := carried(pair(true), ExecutionLinear); len(got) != 1 || len(got[0]) != 2 {
		t.Errorf("linear group messages = %v, want [[a b]]", got)
	}
	if got := carried(pair(false), ExecutionStaged); len(got) != 1 || len(got[0]) != 2 {
		t.Errorf("staged default block messages = %v, want [[a b]]", got)
	}
}
