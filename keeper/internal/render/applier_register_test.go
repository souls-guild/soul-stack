package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Материализация applier-register (orchestration.md §2.1.1, Вариант B): applier-
// задача (`apply:`+`register:`) после своих дочерних destiny-задач эмитит
// синтетическую ТЕРМИНАЛЬНУЮ core.noop.run с Register=applier-register и
// AggregateOf=индексы всех дочерних. Тесты ниже — keeper-side половина инварианта
// (терминал эмитится / onchanges резолвится / passage-стратификация); Soul-side
// агрегат changed/failed/timed_out — soul/internal/runtime.

// applierRegisterScenario — сценарий с одной apply:destiny-задачей с register: и
// внешним потребителем, который реагирует onchanges:[<applier>].
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

// TestRender_ApplierRegister_TerminalEmitted — applier с register: эмитит
// терминальную core.noop.run ПОСЛЕ дочерних destiny-задач, с Register=applier-
// register и AggregateOf=индексы всех дочерних. Без register: терминал не эмитится
// (impact на сквозной Index: +1 только за applier С register).
func TestRender_ApplierRegister_TerminalEmitted(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	// applyScenario НЕ несёт register: → 2 дочерних, без терминала (БИТ-В-БИТ).
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (no register): %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("applier без register: len(tasks)=%d, want 2 (терминал НЕ эмитится)", len(tasks))
	}

	// Тот же applier С register: → 2 дочерних + 1 терминал.
	in.Scenario.Tasks[0].Register = "redis_destiny"
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (with register): %v", err)
	}
	if len(tasks) != 3 || len(plans) != 3 {
		t.Fatalf("applier с register: len(tasks)=%d plans=%d, want 3/3 (+терминал)", len(tasks), len(plans))
	}
	term := tasks[2]
	if term.Module != "core.noop.run" {
		t.Errorf("терминал module = %q, want core.noop.run", term.Module)
	}
	if term.Register != "redis_destiny" {
		t.Errorf("терминал Register = %q, want redis_destiny (= applier-register)", term.Register)
	}
	if term.Index != 2 {
		t.Errorf("терминал Index = %d, want 2 (сквозной за дочерними 0,1)", term.Index)
	}
	if len(term.AggregateOf) != 2 || term.AggregateOf[0] != 0 || term.AggregateOf[1] != 1 {
		t.Errorf("терминал AggregateOf = %v, want [0 1] (Index всех дочерних)", term.AggregateOf)
	}
	if term.Params == nil {
		t.Errorf("терминал Params = nil — должен быть пустой Struct (сборка ApplyRequest)")
	}
}

// TestRender_ApplierRegister_OnChangesResolves — ★ guard (b): внешний
// onchanges:[<applier>] РЕЗОЛВИТСЯ в Index терминальной core.noop.run (раньше
// падало ErrOnChangesUnknownRegister — register applier-а не был в registerIndex).
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
		t.Fatalf("Render: %v (раньше ErrOnChangesUnknownRegister — register applier-а не материализовался)", err)
	}
	// 2 дочерних (0,1) + терминал (2) + потребитель (3).
	if len(tasks) != 4 {
		t.Fatalf("len(tasks)=%d, want 4 (2 дочерних + терминал + потребитель)", len(tasks))
	}
	consumer := tasks[3]
	if consumer.Module != "core.service.restarted" {
		t.Fatalf("потребитель[3] module = %q, want core.service.restarted", consumer.Module)
	}
	// onchanges:[redis_destiny] обязан резолвиться в Index терминала (2), НЕ в
	// дочерние и НЕ в несуществующий register.
	if len(consumer.OnChangesIdx) != 1 || consumer.OnChangesIdx[0] != 2 {
		t.Fatalf("потребитель OnChangesIdx = %v, want [2] (Index терминала core.noop.run)", consumer.OnChangesIdx)
	}
}

// TestRender_ApplierRegister_PassageStratification — ★ guard (d): потребитель
// applier-register уезжает в Passage+1 относительно applier. register applier-а —
// passage-определяющий эмиттер (taskEmittedRegisters → t.Register), потребитель
// читает register.<applier> в where: → Stratify разводит их по Passage.
func TestRender_ApplierRegister_PassageStratification(t *testing.T) {
	tasks := []config.Task{
		{
			Name:     "Apply destiny",
			Register: "redis_destiny",
			Apply:    &config.ApplyTask{Destiny: "pilot-flat"},
		},
		{
			// where: читает register.redis_destiny — passage-определяющий источник
			// → потребитель обязан уехать в Passage ПОСЛЕ applier.
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
		t.Fatalf("Passage.Count = %d, want 2 (applier-register-эмиттер → потребитель в Passage+1)", plan.Count)
	}
	if plan.TaskPassage[0] != 0 {
		t.Errorf("applier Passage = %d, want 0", plan.TaskPassage[0])
	}
	if plan.TaskPassage[1] != 1 {
		t.Errorf("потребитель Passage = %d, want 1 (уехал в Passage+1 по register.redis_destiny)", plan.TaskPassage[1])
	}
}

// TestToProtoTasks_AggregateOfRemap — AggregateOf (глобальные Index дочерних)
// РЕМАПИТСЯ global→local при сборке ApplyRequest, как onchanges/onfail: Soul
// агрегирует по локальной позиции в registerByIdx. Отсутствующий источник →
// sentinel (-1, нулевой вклад в OR).
func TestToProtoTasks_AggregateOfRemap(t *testing.T) {
	// Локальный срез: Index 1 отфильтрован where: → не попал. Локальные позиции:
	// [0]=Index0, [1]=Index2(терминал). AggregateOf терминала = [0,1] (global): 0
	// присутствует (локальная 0), 1 отсутствует → sentinel.
	tasks := []*RenderedTask{
		{Index: 0, Name: "child0", Module: "core.file.present"},
		{Index: 2, Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int{0, 1}},
	}
	got := ToProtoTasks(tasks)
	agg := got[1].GetAggregateOf()
	if len(agg) != 2 {
		t.Fatalf("aggregate_of len = %d (%v), want 2 (отсутствующий источник кодируется sentinel-ом, НЕ выкидывается)", len(agg), agg)
	}
	if agg[0] != 0 {
		t.Errorf("aggregate_of[0] = %d, want 0 (Index 0 → локальная 0)", agg[0])
	}
	if agg[1] != outOfRangeRequisite {
		t.Errorf("aggregate_of[1] = %d, want %d (sentinel отфильтрованного дочернего)", agg[1], outOfRangeRequisite)
	}
}
