package render

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Rescue units in render (ADR-0091 amendment NIM-993, rule 1): every task carries
// the key of the outermost unit holding it, and a linear run learns where each
// scenario unit can reach.

func runTask(name string, extra ...func(*config.Task)) config.Task {
	t := config.Task{Name: name, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": name}}}
	for _, f := range extra {
		f(&t)
	}
	return t
}

func onfail(names ...string) func(*config.Task) {
	return func(t *config.Task) { t.OnFail = names }
}

func registers(name string) func(*config.Task) {
	return func(t *config.Task) { t.Register = name }
}

// unitDestiny is a destiny with units of its own: undo (position 1), the revert block
// (position 2), off, which a static `when:` skips (position 4), and the `loop:` again
// (position 5) all rescue its step.
func unitDestiny(prefix string) *ResolvedDestiny {
	return &ResolvedDestiny{Name: prefix, Tasks: []config.Task{
		runTask(prefix+".step", registers("step")),
		runTask(prefix+".undo", onfail("step")),
		{Name: prefix + ".revert", OnFail: []string{"step"}, Block: &config.BlockTask{Block: []config.Task{
			runTask(prefix + ".revert.one"),
		}}},
		runTask(prefix + ".tail"),
		runTask(prefix+".off", onfail("step"), func(t *config.Task) { t.When = "false" }),
		runTask(prefix+".again", onfail("step"), func(t *config.Task) {
			t.Loop = &config.LoopSpec{Items: "${ ['x', 'y'] }"}
			t.Module.Params = map[string]any{"cmd": "${ item }"}
		}),
	}}
}

type destinyByName map[string]*ResolvedDestiny

func (d destinyByName) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	return d[name], nil
}

// unitPlan is a linear plan with every place a rescue unit key comes from: a block
// unit holding a unit of another source and a child a static `when:` skips, a unit
// inside a block that is none, an applier inside a unit and one outside every unit,
// a `loop:` unit, a unit a static `when:` skips and a top-level task unit.
func unitPlan() *config.ScenarioManifest {
	return &config.ScenarioManifest{Name: "linear", Tasks: []config.Task{
		runTask("deploy", registers("deploy")),
		runTask("probe", registers("probe")),
		runTask("plain"),
		{Name: "roll", OnFail: []string{"deploy"}, Block: &config.BlockTask{Block: []config.Task{
			runTask("restore"),
			runTask("nested", onfail("probe")),
			runTask("restore_off", func(t *config.Task) { t.When = "false" }),
		}}},
		{Name: "wrap", Block: &config.BlockTask{Block: []config.Task{
			runTask("inner", onfail("probe")),
			{Name: "unit_apply", OnFail: []string{"deploy"}, Register: "unit_applied", Apply: &config.ApplyTask{Destiny: "u"}},
		}}},
		{Name: "plain_apply", Register: "plain_applied", Apply: &config.ApplyTask{Destiny: "p"}},
		runTask("each", onfail("deploy"), func(t *config.Task) {
			t.Loop = &config.LoopSpec{Items: "${ ['x', 'y'] }"}
			t.Module.Params = map[string]any{"cmd": "${ item }"}
		}),
		runTask("skipped", onfail("deploy"), func(t *config.Task) { t.When = "false" }),
		runTask("rollback", onfail("deploy")),
	}}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 1 and 6). Every task carries the key of
// the outermost unit holding it, on every pass, rendered or a placeholder:
//   - a block unit's children its key, the one naming another source too (nested,
//     `onfail: [probe]` inside `roll`) — the outer unit decides every source it holds;
//   - a unit inside a block that is none, its own position;
//   - an applier inside a unit, that unit's key on every task of its destiny, the
//     destiny's own units included;
//   - an applier in no unit, its destiny's own units as "<applier>@<destiny position>"
//     and nothing on the destiny's other tasks;
//   - an applier's terminal, none: it is no task of its unit;
//   - every iteration of a `loop:` unit and a unit a static `when:` skips, the unit's,
//     in a block, at the top level and in a destiny alike;
//   - a placeholder past the end of a run a failure ended — a rescue reaching no
//     host, at the top level or in a block, and any other task — the unit's of its
//     task.
//
// Mutations: take the innermost unit holding a task (the last match in keyAt) →
// nested reads 3/1; stamp an applier child's whole range in walkBlockChildren → the
// terminal of unit_apply reads 4/1; leave a destiny pass's outer key unset → u.undo
// reads 4/1@1; leave out any one stampRescueUnit call → the task it stamps reads ""
// on some pass (restore_off, u.off, u.again, rollback, and restore and each past the
// end among them).
func TestRender_RescueUnitKeysTheOutermostUnit(t *testing.T) {
	want := map[string]string{
		"deploy": "", "probe": "", "plain": "",
		"roll": "3", "restore": "3", "nested": "3", "restore_off": "3",
		"wrap":       "",
		"inner":      "4/0",
		"unit_apply": "4/1", "u.step": "4/1", "u.undo": "4/1", "u.revert.one": "4/1", "u.tail": "4/1",
		"u.off": "4/1", "u.again": "4/1",
		"applier-register unit_applied": "",
		"plain_apply":                   "", "p.step": "", "p.undo": "5@1", "p.revert.one": "5@2", "p.tail": "",
		"p.off": "5@4", "p.again": "5@5",
		"applier-register plain_applied": "",
		"each":                           "6",
		"skipped":                        "7",
		"rollback":                       "8",
	}
	m := unitPlan()
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	render := func(active int, rescue *Rescue) ([]*RenderedTask, []DispatchPlan) {
		t.Helper()
		tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
			Scenario:      m,
			Input:         map[string]any{},
			Incarnation:   IncarnationMeta{ID: "svc"},
			Hosts:         linearHosts(),
			TaskPassage:   plan.TaskPassage,
			ActivePassage: active,
			Steps:         steps,
			HeldTargets:   NewHeldTargets(),
			Rescue:        rescue,
			Destiny:       destinyByName{"u": unitDestiny("u"), "p": unitDestiny("p")},
		})
		if err != nil {
			t.Fatalf("Render at step %d (rescue %+v): %v", active, rescue, err)
		}
		return tasks, plans
	}
	seen := map[string]bool{}
	check := func(pass string, tasks []*RenderedTask) {
		t.Helper()
		for _, rt := range tasks {
			key, ok := want[rt.Name]
			if !ok {
				t.Fatalf("%s rendered %q, which the table does not know", pass, rt.Name)
			}
			seen[rt.Name] = true
			if rt.RescueUnit != key {
				t.Errorf("%s: %s carries rescue unit %q, want %q", pass, rt.Name, rt.RescueUnit, key)
			}
		}
	}
	for active := 0; active < plan.Count; active++ {
		tasks, _ := render(active, nil)
		check(fmt.Sprintf("step %d", active), tasks)
	}
	// Ended at step 0 with nothing failed: every rescue past it reaches no host.
	pastEnd, pastPlans := render(plan.Count-1, &Rescue{Ended: true, EndedAt: 0})
	for _, name := range []string{"restore", "nested", "inner", "unit_apply", "each"} {
		if rt, plan := renderedByName(t, pastEnd, pastPlans, name); rt.Params != nil || len(plan.TargetSIDs) != 0 {
			t.Fatalf("past the end, %s rendered for %v - the pass no longer reaches its placeholder", name, plan.TargetSIDs)
		}
	}
	check("past the end", pastEnd)
	for name := range want {
		if !seen[name] {
			t.Errorf("%s was never rendered", name)
		}
	}
}

// coverPlan holds a top-level unit of two sources, a unit inside a block below
// another `on:` holding a unit of another source before its last step, a unit in an
// include group the pass keeps and two in one it drops — one of them inside a block
// of that group.
func coverPlan(t *testing.T) *config.ScenarioManifest {
	t.Helper()
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: migrate }
  - name: rollback
    module: core.exec.run
    onfail: [deploy, migrate]
    params: { cmd: rollback }
  - name: web
    on: [web]
    block:
      - name: inner
        on: [eu]
        onfail: [deploy]
        block:
          - name: nested
            module: core.exec.run
            onfail: [migrate]
            params: { cmd: nested }
          - name: two
            module: core.exec.run
            params: { cmd: two }
`)
	m.Tasks = append(m.Tasks, includeGroup("false", 1,
		runTask("gone", onfail("deploy")),
		config.Task{Name: "gone_block", Block: &config.BlockTask{Block: []config.Task{runTask("gone_child", onfail("deploy"))}}},
	)...)
	m.Tasks = append(m.Tasks, includeGroup("true", 2, runTask("kept", onfail("migrate")))...)
	return m
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 1-2). A unit's cover is its steps and the
// labels a host must carry: its own `on:` and every enclosing construct's, ANDed. A
// construct naming two sources is one cover; a unit in an include group the pass
// drops is none. A unit inside a unit of another source is decided by the outermost
// one (rule 1): its cover names that unit's key, the one its tasks carry, and that
// unit's last step.
//
// Mutations: read the unit's own `on:` alone → inner covers [eu]; skip the
// include-group check → gone and gone_child get covers; take the cover's own path as
// its deciding unit → nested reads 3/0/0 decided at 3.
func TestRescueCovers_StepsAndTheOnChain(t *testing.T) {
	m := coverPlan(t)
	_, steps := config.LinearPassage(m.Tasks, m.Settings)
	covers, err := NewPipeline(nil, newEngine(t), nil, nil).RescueCovers(RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       linearHosts(),
		Steps:       steps,
	})
	if err != nil {
		t.Fatalf("RescueCovers: %v", err)
	}
	want := []RescueCover{
		{Path: "2", Sources: []string{"deploy", "migrate"}, First: 2, Last: 2, RescueUnit: "2", DecidedAt: 2},
		{Path: "3/0", Sources: []string{"deploy"}, First: 3, Last: 4, Covens: []string{"web", "eu"}, RescueUnit: "3/0", DecidedAt: 4},
		{Path: "3/0/0", Sources: []string{"migrate"}, First: 3, Last: 3, Covens: []string{"web", "eu"}, RescueUnit: "3/0", DecidedAt: 4},
		{Path: "6", Sources: []string{"migrate"}, First: 7, Last: 7, RescueUnit: "6", DecidedAt: 7},
	}
	if !reflect.DeepEqual(covers, want) {
		t.Errorf("RescueCovers =\n%+v\nwant\n%+v", covers, want)
	}

	if covers, err := NewPipeline(nil, newEngine(t), nil, nil).RescueCovers(RenderInput{Scenario: m}); err != nil || covers != nil {
		t.Errorf("RescueCovers without steps = %v, %v; want nothing", covers, err)
	}

	web := host(sidA, []string{"svc", "web", "eu"}, nil)
	for _, tc := range []struct {
		h      *topology.HostFacts
		covens []string
		want   bool
	}{
		{web, []string{"web", "eu"}, true},
		{host(sidB, []string{"svc", "web"}, nil), []string{"web", "eu"}, false},
		{host(sidB, nil, nil), nil, true},
	} {
		if got := HostCovered(tc.h, tc.covens); got != tc.want {
			t.Errorf("HostCovered(%v, %v) = %v, want %v", tc.h.Coven, tc.covens, got, tc.want)
		}
	}
}
