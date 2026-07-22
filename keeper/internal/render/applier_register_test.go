package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Applier-register materialization (orchestration.md §2.1.1, Variant B): an
// applier task (`apply:`+`register:`) emits a synthetic TERMINAL
// core.noop.run after its child destiny tasks, with Register=applier-register
// and AggregateOf=indices of all children. Tests below cover the keeper-side
// half of the invariant (terminal emitted / onchanges resolves / passage
// stratification); the Soul-side changed/failed/timed_out aggregation lives
// in soul/internal/runtime.

// applierRegisterScenario — a scenario with one apply:destiny task carrying
// register:, plus an external consumer reacting via onchanges:[<applier>].
func applierRegisterScenario(destiny, register string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{
				Name:     "Apply destiny",
				Register: register,
				Apply:    &config.ApplyTask{Destiny: destiny, Input: map[string]any{"marker_file": "/m", "marker_payload": "p"}},
			},
			{
				Name:      "React to destiny change",
				OnChanges: []string{register},
				Module: &config.ModuleTask{
					Module: "core.service.restarted",
					Params: map[string]any{"name": "svc"},
				},
			},
		},
	}
}

// TestRender_ApplierRegister_TerminalEmitted — an applier with register:
// emits a terminal core.noop.run AFTER its child destiny tasks, with
// Register=applier-register and AggregateOf=indices of all children. Without
// register:, no terminal is emitted (only an applier WITH register bumps the
// running Index by 1).
func TestRender_ApplierRegister_TerminalEmitted(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	// applyScenario carries NO register: → 2 children, no terminal (bit-for-bit).
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (no register): %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("applier without register: len(tasks)=%d, want 2 (terminal is NOT emitted)", len(tasks))
	}

	// Same applier WITH register: → 2 children + 1 terminal.
	in.Scenario.Tasks[0].Register = "redis_destiny"
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (with register): %v", err)
	}
	if len(tasks) != 3 || len(plans) != 3 {
		t.Fatalf("applier with register: len(tasks)=%d plans=%d, want 3/3 (+terminal)", len(tasks), len(plans))
	}
	term := tasks[2]
	if term.Module != "core.noop.run" {
		t.Errorf("terminal module = %q, want core.noop.run", term.Module)
	}
	if term.Register != "redis_destiny" {
		t.Errorf("terminal Register = %q, want redis_destiny (= applier-register)", term.Register)
	}
	if term.Index != 2 {
		t.Errorf("terminal Index = %d, want 2 (through-numbered after children 0,1)", term.Index)
	}
	if len(term.AggregateOf) != 2 || term.AggregateOf[0] != 0 || term.AggregateOf[1] != 1 {
		t.Errorf("terminal AggregateOf = %v, want [0 1] (Index of all children)", term.AggregateOf)
	}
	if term.Params == nil {
		t.Errorf("terminal Params = nil - must be an empty Struct (ApplyRequest assembly)")
	}
}

// TestRender_ApplierRegister_OnChangesResolves — ★ guard (b): an external
// onchanges:[<applier>] RESOLVES to the Index of the terminal core.noop.run
// (previously failed with ErrOnChangesUnknownRegister — the applier's
// register wasn't in registerIndex).
func TestRender_ApplierRegister_OnChangesResolves(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applierRegisterScenario("pilot-flat", "redis_destiny"),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v (used to fail with ErrOnChangesUnknownRegister - the applier register was not materialized)", err)
	}
	// 2 children (0,1) + terminal (2) + consumer (3).
	if len(tasks) != 4 {
		t.Fatalf("len(tasks)=%d, want 4 (2 children + terminal + consumer)", len(tasks))
	}
	consumer := tasks[3]
	if consumer.Module != "core.service.restarted" {
		t.Fatalf("consumer[3] module = %q, want core.service.restarted", consumer.Module)
	}
	// onchanges:[redis_destiny] must resolve to the terminal's Index (2), NOT
	// to a child or to a nonexistent register.
	if len(consumer.OnChangesIdx) != 1 || consumer.OnChangesIdx[0] != 2 {
		t.Fatalf("consumer OnChangesIdx = %v, want [2] (Index of terminal core.noop.run)", consumer.OnChangesIdx)
	}
}

// TestRender_ApplierRegister_PassageStratification — ★ guard (d): a consumer of
// the applier-register lands in Passage+1 relative to the applier. The
// applier's register is a passage-determining emitter (taskEmittedRegisters →
// t.Register); a consumer reading register.<applier> in where: gets split
// into the next Passage by Stratify.
func TestRender_ApplierRegister_PassageStratification(t *testing.T) {
	tasks := []config.Task{
		{
			Name:     "Apply destiny",
			Register: "redis_destiny",
			Apply:    &config.ApplyTask{Destiny: "pilot-flat"},
		},
		{
			// where: reads register.redis_destiny — a passage-determining source
			// → the consumer must land in the Passage AFTER the applier.
			Name:  "Conditional restart",
			Where: "register.redis_destiny.changed",
			Module: &config.ModuleTask{
				Module: "core.service.restarted",
				Params: map[string]any{"name": "svc"},
			},
		},
	}
	plan, err := Stratify(tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if plan.Count != 2 {
		t.Fatalf("Passage.Count = %d, want 2 (applier-register emitter -> consumer in Passage+1)", plan.Count)
	}
	if plan.TaskPassage[0] != 0 {
		t.Errorf("applier Passage = %d, want 0", plan.TaskPassage[0])
	}
	if plan.TaskPassage[1] != 1 {
		t.Errorf("consumer Passage = %d, want 1 (moved to Passage+1 via register.redis_destiny)", plan.TaskPassage[1])
	}
}

// TestToProtoTasks_AggregateOfRemap — AggregateOf (global child Index
// values) is REMAPPED global→local when building an ApplyRequest, like
// onchanges/onfail: Soul aggregates by local position in registerByIdx. A
// missing source maps to a sentinel (-1, contributes nothing to the OR).
func TestToProtoTasks_AggregateOfRemap(t *testing.T) {
	// Local slice: Index 1 was filtered out by where: → absent. Local
	// positions: [0]=Index0, [1]=Index2 (terminal). Terminal's AggregateOf =
	// [0,1] (global): 0 is present (local 0), 1 is absent → sentinel.
	tasks := []*RenderedTask{
		{Index: 0, Name: "child0", Module: "core.file.present"},
		{Index: 2, Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int{0, 1}},
	}
	got := ToProtoTasks(tasks)
	agg := got[1].GetAggregateOf()
	if len(agg) != 2 {
		t.Fatalf("aggregate_of len = %d (%v), want 2 (a missing source is encoded as a sentinel, NOT dropped)", len(agg), agg)
	}
	if agg[0] != 0 {
		t.Errorf("aggregate_of[0] = %d, want 0 (Index 0 -> local 0)", agg[0])
	}
	if agg[1] != outOfRangeRequisite {
		t.Errorf("aggregate_of[1] = %d, want %d (sentinel of the filtered-out child)", agg[1], outOfRangeRequisite)
	}
}
