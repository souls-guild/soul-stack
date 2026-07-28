package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// asyncScenario — the canonical ADR-0075 shape parsed from YAML (so the test
// covers the config grammar too, not just the struct): two fire-and-forget
// probes, then a task holding an explicit barrier on both.
const asyncScenario = `
name: async-pilot
description: two async probes joined by an explicit require
state_changes: {}
tasks:
  - name: collect cpu
    module: core.exec.run
    register: collect_cpu
    async: true
    params:
      cmd: collect-cpu
  - name: collect memory
    module: core.exec.run
    register: collect_memory
    async: true
    params:
      cmd: collect-memory
  - name: send report
    module: core.exec.run
    require: [collect_cpu, collect_memory]
    params:
      cmd: report-send
`

func asyncRenderInput(m *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
}

// TestRender_AsyncAndRequireReachTheWire is the acceptance test of NIM-150: the
// ADR-0075 construct parses, survives render, and lands in RenderedTask's wire
// form with require: resolved from register names into LOCAL task indices. The
// guards that used to reject the key (guardPilotDSL) no longer fire.
func TestRender_AsyncAndRequireReachTheWire(t *testing.T) {
	m := loadStagedManifest(t, asyncScenario)
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), asyncRenderInput(m))
	if err != nil {
		t.Fatalf("Render: %v (the async: guard must be lifted, ADR-0075)", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("rendered tasks = %d, want 3", len(tasks))
	}
	if !tasks[0].Async || !tasks[1].Async {
		t.Errorf("Async = %v/%v, want true/true (async: threaded from config.Task)", tasks[0].Async, tasks[1].Async)
	}
	if tasks[2].Async {
		t.Error("the barrier task must NOT be async — async: is a per-task flag, not a group")
	}
	if got := tasks[2].RequireIdx; len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("RequireIdx = %v, want [0 1] (register names resolved to indices, Variant A)", got)
	}
	if tasks[2].RequireAll {
		t.Error("RequireAll = true on the list form — the two forms are mutually exclusive")
	}

	pt := ToProtoTasks(tasks)
	if !pt[0].GetAsync() || !pt[1].GetAsync() {
		t.Errorf("proto async = %v/%v, want true/true", pt[0].GetAsync(), pt[1].GetAsync())
	}
	got := pt[2].GetRequireIdx()
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("proto require_idx = %v, want [0 1] (remapped global→local)", got)
	}
	if pt[2].GetRequireAll() {
		t.Error("proto require_all = true on the list form")
	}
}

// TestRender_RequireAllReachesTheWire — the scalar form `require: all` sets
// require_all and names no source, so require_idx stays empty.
func TestRender_RequireAllReachesTheWire(t *testing.T) {
	m := loadStagedManifest(t, `
name: async-all
description: a global barrier over every async task
state_changes: {}
tasks:
  - name: collect cpu
    module: core.exec.run
    register: collect_cpu
    async: true
    params:
      cmd: collect-cpu
  - name: final consistency check
    module: core.exec.run
    require: all
    params:
      cmd: check-cluster-state
`)
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), asyncRenderInput(m))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !tasks[1].RequireAll {
		t.Error("RequireAll = false, want true (`require: all` scalar form)")
	}
	if tasks[1].RequireIdx != nil {
		t.Errorf("RequireIdx = %v, want nil (the all form names no source)", tasks[1].RequireIdx)
	}
	pt := ToProtoTasks(tasks)
	if !pt[1].GetRequireAll() || pt[1].GetRequireIdx() != nil {
		t.Errorf("proto require_all/require_idx = %v/%v, want true/nil", pt[1].GetRequireAll(), pt[1].GetRequireIdx())
	}
}

// TestRender_AsyncInDestiny — guardDestinyTask no longer rejects async: either
// (ADR-0075 lifts BOTH render guards): a destiny task runs Soul-side inside the
// same ApplyRequest, so asynchrony is as meaningful there as in a scenario.
func TestRender_AsyncInDestiny(t *testing.T) {
	d := &ResolvedDestiny{
		Name: "warm-caches",
		Tasks: []config.Task{
			{
				Name:     "warm",
				Async:    true,
				Register: "warm",
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "warm-cache"}},
			},
			{
				Name:    "verify",
				Require: []string{"warm"},
				Module:  &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "verify-cache"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("warm-caches", map[string]any{}),
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: d},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v (async: inside a destiny must render, ADR-0075)", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("rendered tasks = %d, want 2", len(tasks))
	}
	if !tasks[0].Async {
		t.Error("destiny task Async = false, want true")
	}
	if got := tasks[1].RequireIdx; len(got) != 1 || got[0] != 0 {
		t.Errorf("destiny RequireIdx = %v, want [0]", got)
	}
}

// TestRender_LoopAsyncMarksEveryIteration — `loop:` + `async: true` (§7): the
// loop fans out into N RenderedTask at render, and EVERY iteration carries the
// flag, so each runs in its own flow. A threading regression here would silently
// make only some iterations async.
func TestRender_LoopAsyncMarksEveryIteration(t *testing.T) {
	m := loadStagedManifest(t, `
name: loop-async
description: every iteration of an async loop runs in its own flow
state_changes: {}
tasks:
  - name: fetch
    module: core.exec.run
    async: true
    loop:
      items: "${ ['a', 'b', 'c'] }"
    params:
      cmd: "fetch ${ item }"
`)
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), asyncRenderInput(m))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("rendered tasks = %d, want 3 (one per loop item)", len(tasks))
	}
	for i, tk := range tasks {
		if !tk.Async {
			t.Errorf("iteration %d: Async = false, want true", i)
		}
	}
}

// TestRender_AsyncOnKeeperTaskRejected — `async:` on an `on: keeper` task is
// fail-closed. The flag rides RenderedTask to a Soul runner and a keeper task
// never reaches one, so honouring it would be a silent no-op. Same shape as the
// existing apply:/loop: rejections on a keeper task.
func TestRender_AsyncOnKeeperTaskRejected(t *testing.T) {
	m := &config.ScenarioManifest{
		Name: "keeper-async",
		Tasks: []config.Task{
			{
				Name:   "register the soul",
				On:     "keeper",
				Async:  true,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	if _, _, err := p.Render(context.Background(), asyncRenderInput(m)); !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (async: on a keeper-side task)", err)
	}
}

// TestRender_RequireUnknownRegister — a typo'd barrier is an error, not a
// silent "wait for nothing". Mirrors ErrOnChangesUnknownRegister: the ordering
// the author declared would otherwise never happen and nothing would say so.
func TestRender_RequireUnknownRegister(t *testing.T) {
	m := &config.ScenarioManifest{
		Name: "typo",
		Tasks: []config.Task{
			{
				Name:     "probe",
				Register: "probe",
				Async:    true,
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "probe"}},
			},
			{
				Name:    "consumer",
				Require: []string{"prob"}, // typo
				Module:  &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "go"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	if _, _, err := p.Render(context.Background(), asyncRenderInput(m)); !errors.Is(err, ErrRequireUnknownRegister) {
		t.Fatalf("err = %v, want ErrRequireUnknownRegister", err)
	}
}

// crossPassageRequireScenario — a `require:` pointing FORWARD across a Passage
// boundary. `where: register.role.*` is passage-defining (ADR-056), so the
// source of the barrier lands in Passage 1, while the task awaiting it stays in
// Passage 0 (`require:` is deliberately NOT passage-defining). The two would
// then travel in different ApplyRequests with the consumer dispatched first.
const crossPassageRequireScenario = `
name: cross-passage-require
description: a barrier whose source lands in the next Passage
state_changes: {}
tasks:
  - name: probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: act on master only
    module: core.exec.run
    register: acted
    where: "register.role.stdout == 'master'"
    params:
      cmd: promote
  - name: waits for a task it can never see
    module: core.exec.run
    require: [acted]
    params:
      cmd: report
`

// TestRender_RequireCrossPassageForwardRejected — the Passage invariant of
// ADR-0075(d): an async flow never outlives its Passage, so a barrier can only
// await a task in its own Passage or an earlier one. A source in a LATER Passage
// is unrepresentable on the wire (the remap sentinel means "nothing to wait
// for"), and shipping it would silently reverse the declared order — reject at
// render instead.
func TestRender_RequireCrossPassageForwardRejected(t *testing.T) {
	m := loadStagedManifest(t, crossPassageRequireScenario)
	plan, err := Stratify(m.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if plan.Count != 2 {
		t.Fatalf("Passage.Count = %d, want 2 (probe→where consumer)", plan.Count)
	}
	if plan.TaskPassage[2] >= plan.TaskPassage[1] {
		t.Fatalf("TaskPassage = %v, want the require-consumer BEHIND its source (require: is not passage-defining)", plan.TaskPassage)
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := asyncRenderInput(m)
	in.TaskPassage = plan.TaskPassage
	if _, _, err := p.Render(context.Background(), in); !errors.Is(err, ErrRequireCrossPassage) {
		t.Fatalf("err = %v, want ErrRequireCrossPassage", err)
	}
}

// TestResolveRequire_EarlierPassageAllowed — the opposite direction is legal and
// must NOT be rejected: a Passage is closed on every host before the next one is
// dispatched, so a source in an earlier Passage is already finalized and the
// barrier is satisfied before the consumer's ApplyRequest is even assembled.
func TestResolveRequire_EarlierPassageAllowed(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "probe", Register: "probe", Passage: 0, Async: true},
		{Index: 1, Name: "consumer", Passage: 1, requireNames: []string{"probe"}},
	}
	if err := resolveRequire(tasks); err != nil {
		t.Fatalf("resolveRequire: %v (an earlier-Passage source is already finalized)", err)
	}
	if got := tasks[1].RequireIdx; len(got) != 1 || got[0] != 0 {
		t.Errorf("RequireIdx = %v, want [0]", got)
	}
}

// TestToProtoTasks_RequireSentinelForAbsentSource — a source that is not in THIS
// ApplyRequest (filtered out by where: on this host, or in an already-closed
// Passage) is encoded as the -1 sentinel rather than dropped: dropping would
// shift the remaining indices. Soul reads -1 as "nothing to wait for" — the
// inverse of what the same sentinel means for onchanges/onfail.
func TestToProtoTasks_RequireSentinelForAbsentSource(t *testing.T) {
	// The slice handed to ToProtoTasks holds only the consumer: its source
	// (global Index 0) did not survive per-host filtering.
	tasks := []*RenderedTask{
		{Index: 1, Name: "consumer", Module: "core.exec.run", RequireIdx: []int{0}},
	}
	got := ToProtoTasks(tasks)[0].GetRequireIdx()
	if len(got) != 1 || got[0] != outOfRangeRequisite {
		t.Errorf("require_idx = %v, want [%d] (absent source → sentinel, not dropped)", got, outOfRangeRequisite)
	}
}

// TestToProtoTasks_ConcurrencyZeroValue — the only-add contract (ADR-012(c)): a
// plan that uses neither key leaves all three fields at their zero value, so an
// old Soul that has never heard of them decodes the message unchanged and runs
// the plan sequentially — a correct execution, only serial.
func TestToProtoTasks_ConcurrencyZeroValue(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "plain", Module: "core.exec.run"},
	}
	pt := ToProtoTasks(tasks)[0]
	if pt.GetAsync() {
		t.Error("async = true on a task that never declared it")
	}
	if pt.GetRequireIdx() != nil {
		t.Errorf("require_idx = %v, want nil", pt.GetRequireIdx())
	}
	if pt.GetRequireAll() {
		t.Error("require_all = true on a task that never declared it")
	}
}
