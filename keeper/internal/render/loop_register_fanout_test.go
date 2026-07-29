package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// A `loop:` writes every iteration into ONE `register:` (destiny/tasks.md §7),
// so a requisite naming it means all of them. While the name→index map kept
// only the last writer, `onchanges:` gated on the final iteration and a
// `require:` barrier released while its siblings were still running (NIM-246).

// fanoutPlan renders a 3-iteration loop task registered as "rendered",
// followed by one consumer built by the caller.
func fanoutPlan(t *testing.T, consumer config.Task) []*RenderedTask {
	t.Helper()
	fan := moduleTask("fan", "core.exec.run")
	fan.Register = "rendered"
	fan.Async = true
	fan.Loop = &config.LoopSpec{Items: "${ ['a','b','c'] }", As: "item"}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{fan, consumer}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4 (3 iterations + consumer)", len(tasks))
	}
	return tasks
}

func TestLoopFanout_RequireWaitsForEveryIteration(t *testing.T) {
	consumer := moduleTask("consumer", "core.service.restarted")
	consumer.Require = []string{"rendered"}
	tasks := fanoutPlan(t, consumer)

	got := tasks[3].RequireIdx
	if len(got) != 3 {
		t.Fatalf("consumer.RequireIdx = %v, want all 3 iterations", got)
	}
	for i, idx := range got {
		if idx != i {
			t.Errorf("consumer.RequireIdx = %v, want [0 1 2] in plan order", got)
			break
		}
	}
}

func TestLoopFanout_OnChangesSeesEveryIteration(t *testing.T) {
	consumer := moduleTask("consumer", "core.service.restarted")
	consumer.OnChanges = []string{"rendered"}
	tasks := fanoutPlan(t, consumer)

	if got := tasks[3].OnChangesIdx; len(got) != 3 {
		t.Fatalf("consumer.OnChangesIdx = %v, want all 3 iterations (any change runs the task)", got)
	}
}

func TestLoopFanout_OnFailSeesEveryIteration(t *testing.T) {
	consumer := moduleTask("consumer", "core.service.restarted")
	consumer.OnFail = []string{"rendered"}
	tasks := fanoutPlan(t, consumer)

	if got := tasks[3].OnFailIdx; len(got) != 3 {
		t.Fatalf("consumer.OnFailIdx = %v, want all 3 iterations (any failure runs the rescue)", got)
	}
}

// Negative — a register that did NOT fan out still resolves to exactly one
// index. The fix must not turn every requisite into a set.
func TestLoopFanout_PlainRegisterStillResolvesToOne(t *testing.T) {
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "cfg"
	consumer := moduleTask("consumer", "core.service.restarted")
	consumer.OnChanges = []string{"cfg"}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{probe, consumer}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[1].OnChangesIdx; len(got) != 1 || got[0] != 0 {
		t.Errorf("consumer.OnChangesIdx = %v, want [0]", got)
	}
}

// Two names, one of them fanned out: the flattened list must hold every index
// of both, and the cross-passage check must still report the right NAME (it
// used to pair indexes onto names by position, which a fan-out breaks).
func TestLoopFanout_MixedNamesFlattenInOrder(t *testing.T) {
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "cfg"
	fan := moduleTask("fan", "core.exec.run")
	fan.Register = "rendered"
	fan.Loop = &config.LoopSpec{Items: "${ ['a','b'] }", As: "item"}
	consumer := moduleTask("consumer", "core.service.restarted")
	consumer.Require = []string{"cfg", "rendered"}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{probe, fan, consumer}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// 0=probe(cfg) 1,2=fan iterations 3=consumer.
	got := tasks[3].RequireIdx
	if len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Errorf("consumer.RequireIdx = %v, want [0 1 2] (cfg, then both iterations)", got)
	}
}
