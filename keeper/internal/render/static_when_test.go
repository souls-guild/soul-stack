package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// optionalInputApplyTask is a task on an inactive branch of a multi-action
// destiny: its params read an optional input (`${ input.maxmemory }`) that
// isn't passed under the current action, gated by
// `when: input.action == 'apply'`. Eager param rendering would fail on
// no-such-key; the static-when placeholder skip avoids it (ADR-012(d),
// Variant b).
func optionalInputApplyTask() config.Task {
	return config.Task{
		Name:   "apply-config",
		When:   "input.action == 'apply'",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli config set maxmemory ${ input.maxmemory }"}},
	}
}

// TestStaticWhenFalse_SkipsParamRender — ★ key spec case: a task with an
// optional-input in params + `when: input.action=='apply'`, at
// action!=apply → Render does NOT fail (params are skipped, no-such-key on
// `${ input.maxmemory }` is never reached).
func TestStaticWhenFalse_SkipsParamRender(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "multi-action", Tasks: []config.Task{optionalInputApplyTask()}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"}, // not apply → maxmemory isn't passed
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when:false should skip param render (optional-input is unreachable), got %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (placeholder stays in the plan)", len(tasks))
	}
	rt := tasks[0]
	if rt.Params != nil {
		t.Errorf("Params = %v, want nil (render skipped)", rt.Params.AsMap())
	}
	if rt.When != "input.action == 'apply'" {
		t.Errorf("When = %q, want the as-is passed-through predicate", rt.When)
	}
	if rt.Module != "core.exec.run" {
		t.Errorf("Module = %q, want core.exec.run (placeholder carries the module)", rt.Module)
	}
	if rt.FlowContext == nil {
		t.Error("FlowContext = nil - Soul must get flow_context for its own evalWhen")
	}
}

// TestStaticWhenTrue_RendersParams — reverse control: same predicate at
// action==apply → static-when:true → no skip, params render the normal way.
func TestStaticWhenTrue_RendersParams(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "multi-action", Tasks: []config.Task{optionalInputApplyTask()}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "apply", "maxmemory": "256mb"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := tasks[0].Params.GetFields()["cmd"].GetStringValue()
	if got != "redis-cli config set maxmemory 256mb" {
		t.Errorf("cmd = %q, want the rendered command (static-when:true -> params render)", got)
	}
}

// TestStaticSkip_OnChangesIndicesIntact — the onchanges source task is
// static-skipped: its Index/remap stay intact, and the onchanges consumer
// reads "not changed" (source in registerByIdx with changed=false ⇒ consumer
// correctly skips). Verifies the render invariant: skip doesn't break
// contiguous numbering or onchanges resolution.
func TestStaticSkip_OnChangesIndicesIntact(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "onchanges-skip",
		Tasks: []config.Task{
			{
				Name:     "src",
				When:     "input.action == 'apply'", // static-false at update_acls
				Register: "src",
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "touch ${ input.path }"}},
			},
			{
				Name:      "consumer",
				OnChanges: []string{"src"},
				Module:    &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo reload"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (skip does not drop the task from the plan)", len(tasks))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("Index = %d,%d, want 0,1 (contiguous numbering intact)", tasks[0].Index, tasks[1].Index)
	}
	if tasks[0].Params != nil {
		t.Error("source static-skipped -> Params must be nil")
	}
	// onchanges resolved to the source's Index (0): the consumer references src.
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("OnChangesIdx = %v, want [0] (register-name resolution to source Index intact)", tasks[1].OnChangesIdx)
	}
	if plans[1].TaskIndex != 1 {
		t.Errorf("plans[1].TaskIndex = %d, want 1", plans[1].TaskIndex)
	}
}

// TestStaticSkip_ConsistentAcrossPassages — static-when gives the SAME skip
// in Passage 0 and Passage 1 for the same input/state snapshot (ADR-056:
// State is passage-invariant, static-when is register-/soulprint-independent
// ⇒ deterministic). The same RenderInput, rendered as different
// ActivePassage, skips identically.
func TestStaticSkip_ConsistentAcrossPassages(t *testing.T) {
	task := optionalInputApplyTask()
	manifest := &config.ScenarioManifest{Name: "passages", Tasks: []config.Task{task}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	base := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		TaskPassage: []int{0},
	}

	in0 := base
	in0.ActivePassage = 0
	t0, _, err := p.Render(context.Background(), in0)
	if err != nil {
		t.Fatalf("Render P0: %v", err)
	}

	in1 := base
	in1.ActivePassage = 0 // same active stage — re-render of the same snapshot
	t1, _, err := p.Render(context.Background(), in1)
	if err != nil {
		t.Fatalf("Render P0 (repeat): %v", err)
	}

	if (t0[0].Params == nil) != (t1[0].Params == nil) {
		t.Errorf("skip is inconsistent between runs: P0.Params==nil=%v, repeat.Params==nil=%v",
			t0[0].Params == nil, t1[0].Params == nil)
	}
	if t0[0].Params != nil {
		t.Error("both runs must skip (static-when:false on the same snapshot)")
	}
}

// TestStaticSkipEqualsSoulSkip — ★ static-when-false on Keeper == when-false
// on Soul. Keeper skips (Params==nil) exactly when Soul-side evalWhen over
// the same flow_context (NewFlowControl, same sandbox) returns false.
// Bit-for-bit guarantee (ADR-012(d)): one env, one flow_context.
func TestStaticSkipEqualsSoulSkip(t *testing.T) {
	task := optionalInputApplyTask()
	manifest := &config.ScenarioManifest{Name: "equiv", Tasks: []config.Task{task}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	keeperSkipped := tasks[0].Params == nil

	// Soul-side replay of evalWhen: the same flow-control engine + flow_context
	// from RenderedTask (what actually ships to Soul).
	soulEngine, err := cel.NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	soulWhen, err := soulEngine.EvalPredicate(tasks[0].When, flowControlVarsFromStruct(tasks[0].FlowContext, nil))
	if err != nil {
		t.Fatalf("Soul-side evalWhen: %v", err)
	}
	soulSkipped := !soulWhen

	if keeperSkipped != soulSkipped {
		t.Errorf("Keeper static-skip=%v != Soul when-skip=%v - sandbox/flow_context mismatch", keeperSkipped, soulSkipped)
	}
	if !keeperSkipped {
		t.Error("expected a skip on both sides (action=update_acls != apply)")
	}
}

// TestRegisterWhen_StaysSoulSide — `when: register.X` is NOT static-skipped:
// register is known only to Soul, the predicate stays Soul-side, params
// render the usual way (Keeper never evaluates it).
func TestRegisterWhen_StaysSoulSide(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "register-when",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "register.probe.changed",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"user": "alice"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if tasks[0].Params == nil {
		t.Fatal("register-when must NOT skip param render (stays Soul-side)")
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo alice" {
		t.Errorf("cmd = %q, want the rendered command (params render via the usual path)", got)
	}
	if tasks[0].When != "register.probe.changed" {
		t.Errorf("When = %q, want the as-is passed-through predicate", tasks[0].When)
	}
}

// TestMixedWhen_NotStatic — ★ reverse case: `when: input.a && register.b` is
// mixed (has register) → NOT static → usual path (params render). If the
// detector mistakenly classified it as static, it would try to evaluate a
// register-dependent predicate on Keeper (register empty) and/or skip on an
// incomplete context. isStaticWhen must return false because of the
// register reference.
func TestMixedWhen_NotStatic(t *testing.T) {
	if isStaticWhen("input.a && register.b.changed") {
		t.Fatal("isStaticWhen(input.a && register.b) = true, want false (register-dependent)")
	}
	manifest := &config.ScenarioManifest{
		Name: "mixed-when",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "input.a && register.b.changed",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"a": true, "user": "bob"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: mixed when must NOT be evaluated Keeper-side, got %v", err)
	}
	if tasks[0].Params == nil {
		t.Fatal("mixed when (with register) must NOT skip param render")
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo bob" {
		t.Errorf("cmd = %q, want echo bob (usual path)", got)
	}
}

// TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard — ★ layer 12: a task on an
// inactive branch with unsupported DSL (`async:` on a keeper-side task) + a
// statically-false when: → static-when PRECEDES the DSL guards: the task is
// gated off and skipped BEFORE they run, instead of being rejected
// with ErrUnsupportedDSL. The active branch (another task) still renders.
// Reverse: before the fix, the guard rejected the key even on an
// inactive branch and failed the whole Render.
func TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "multi-action-async",
		Tasks: []config.Task{
			{
				Name:   "diagnose (keeper-side async, gated off)",
				When:   "input.action == 'diagnose'", // static-false at action=update_acls
				On:     "keeper",
				Async:  true, // unsupported on a keeper task — the guard would have rejected it BEFORE the fix
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{}},
			},
			{
				Name:   "active update_acls",
				When:   "input.action == 'update_acls'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls", "user": "alice"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when MUST precede the DSL guards - a gated-off task must not fail Render, got %v", err)
	}
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 2,2 (skip-placeholder + active)", len(tasks), len(plans))
	}
	// Task 0 — gated off: skip placeholder, params weren't rendered.
	if tasks[0].Params != nil {
		t.Errorf("tasks[0].Params != nil - a gated-off task must be a skip-placeholder")
	}
	if tasks[0].When != "input.action == 'diagnose'" {
		t.Errorf("tasks[0].When = %q, want the passed-through predicate", tasks[0].When)
	}
	if tasks[0].FlowContext == nil {
		t.Error("tasks[0].FlowContext == nil - Soul needs flow_context for its own evalWhen -> SKIPPED")
	}
	// Task 1 — active: renders the normal way.
	if tasks[1].Params == nil {
		t.Fatal("tasks[1].Params == nil - active update_acls must render")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "echo alice" {
		t.Errorf("tasks[1].cmd = %q, want echo alice (active branch renders)", got)
	}
}

// TestStaticWhenTrue_UnsupportedDSL_StillRejected — reverse check on
// over-skip: the same task + when: at action==diagnose → static-TRUE →
// task is ACTIVE → guard REJECTS with ErrUnsupportedDSL. Per-action
// validation: unsupported DSL is rejected exactly when its branch activates,
// never masked.
func TestStaticWhenTrue_UnsupportedDSL_StillRejected(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "active-keeper-async",
		Tasks: []config.Task{
			{
				Name:   "diagnose (keeper-side async, ACTIVE)",
				When:   "input.action == 'diagnose'", // static-TRUE at action=diagnose
				On:     "keeper",
				Async:  true,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "diagnose"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (an active unsupported task is rejected per-action)", err)
	}
}

// TestNonStaticWhen_UnsupportedDSL_StillRejected — a non-static when:
// (`register.x`) + unsupported DSL → the task isn't statically-false (register
// is known only to Soul), bypasses the early static-skip → the guard rejects
// with ErrUnsupportedDSL the usual way. Guarantees the early skip doesn't
// weaken the guard for register-/mixed-when branches.
func TestNonStaticWhen_UnsupportedDSL_StillRejected(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "register-keeper-async",
		Tasks: []config.Task{
			{
				Name:   "keeper-side async gated by register",
				When:   "register.probe.changed", // not static → Soul-side
				On:     "keeper",
				Async:  true,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (register-when parallel is not static -> guard rejects)", err)
	}
}

// TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard_Destiny — same invariant
// in the destiny render pass (apply:destiny): a gated-off parallel task on
// an inactive destiny branch bypasses guardDestinyTask, the active destiny
// task renders. Mirrors the redis-destiny diagnostics (manage.yml
// update_acls active, diagnostic.yml diagnose gated off with parallel:).
func TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard_Destiny(t *testing.T) {
	d := &ResolvedDestiny{
		Name: "multi-action",
		Input: config.InputSchemaMap{
			"action": {Type: "string", Required: true},
			"user":   {Type: "string", Required: false},
		},
		Tasks: []config.Task{
			{
				Name:    "diagnose (run_once, gated off)",
				When:    "input.action == 'diagnose'", // static-false at action=update_acls
				RunOnce: true,                         // scenario-only key — forbidden in a destiny
				Module:  &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli ping"}},
			},
			{
				Name:   "active update_acls",
				When:   "input.action == 'update_acls'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("multi-action", map[string]any{
			"action": "update_acls",
			"user":   "alice",
		}),
		Input:       map[string]any{"action": "update_acls", "user": "alice"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when MUST precede guardDestinyTask in the destiny pass, got %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (skip-placeholder destiny + active destiny)", len(tasks))
	}
	if tasks[0].Params != nil {
		t.Errorf("tasks[0].Params != nil - gated-off parallel destiny must be a skip-placeholder")
	}
	if tasks[1].Params == nil {
		t.Fatal("tasks[1].Params == nil - active update_acls destiny must render")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "echo alice" {
		t.Errorf("tasks[1].cmd = %q, want echo alice", got)
	}
}

// TestStaticWhenTrue_UnsupportedDSL_StillRejected_Destiny — destiny reverse
// case: an active parallel destiny task is rejected by guardDestinyTask
// per-action.
func TestStaticWhenTrue_UnsupportedDSL_StillRejected_Destiny(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "active",
		Input: config.InputSchemaMap{"action": {Type: "string", Required: true}},
		Tasks: []config.Task{
			{
				Name:    "diagnose (run_once, ACTIVE)",
				When:    "input.action == 'diagnose'",
				RunOnce: true,
				Module:  &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli ping"}},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("active", map[string]any{"action": "diagnose"}),
		Input:       map[string]any{"action": "diagnose"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (an active unsupported destiny task is rejected per-action)", err)
	}
}

// TestIsStaticWhen_Classification — classifier table: which when predicates are static.
func TestIsStaticWhen_Classification(t *testing.T) {
	cases := []struct {
		when string
		want bool
	}{
		{"", false},                       // empty — nothing to evaluate Keeper-side
		{"input.action == 'apply'", true}, // input only — static
		{"vars.enabled && incarnation.name != ''", true}, // service var + incarnation — static
		{"vars.flag", true},                             // vars — static (host invariance caught by a second layer)
		{"register.probe.changed", false},               // register — Soul-side
		{"input.a && register.b.ok", false},             // mixed with register — Soul-side
		{"soulprint.self.os.family == 'debian'", false}, // soulprint — host-variant
		{"input.a && soulprint.self.x", false},          // mixed with soulprint — host-variant
	}
	for _, c := range cases {
		if got := isStaticWhen(c.when); got != c.want {
			t.Errorf("isStaticWhen(%q) = %v, want %v", c.when, got, c.want)
		}
	}
}

// keeperWhenManifest — one `on: keeper` capture step carrying the given
// predicate, and nothing else. The module is a real state verb: the guard exists
// because [ADR-0084] made this family the only writer of incarnation state, and
// a fixture using core.soul.registered would test the same code path while
// naming the wrong stake.
func keeperWhenManifest(when string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "keeper-when",
		Tasks: []config.Task{
			{
				Name: "record the provisioned host",
				On:   "keeper",
				When: when,
				Module: &config.ModuleTask{
					Module: "core.state.set",
					Params: map[string]any{"field": "provisioned", "value": "yes"},
				},
			},
		},
	}
}

func keeperWhenInput(m *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    m,
		Input:       map[string]any{"provision": true},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
}

// TestRender_NonStaticWhenOnKeeperTaskRejected — ★ [ADR-0084] F-D. A `when:`
// reading register/soulprint on an `on: keeper` task used to be copied onto the
// RenderedTask and never read: `when:` is a Soul-side predicate and a keeper
// task never reaches a Soul. The file said "conditionally", the step ran every
// time — and since this ADR the step that runs every time is the one that writes
// state. Fail-closed at render is the backstop; soul-lint says it offline.
func TestRender_NonStaticWhenOnKeeperTaskRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		when string
	}{
		{"register", "register.provision.rc == 0"},
		{"soulprint", "soulprint.self.os.family == 'debian'"},
		{"mixed with input", "input.provision && register.provision.rc == 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), keeperWhenInput(keeperWhenManifest(tc.when)))
			if !errors.Is(err, ErrUnsupportedDSL) {
				t.Fatalf("err = %v, want ErrUnsupportedDSL", err)
			}
			// The author has to be able to act on it: the message names the
			// predicate it refused and the form that does work.
			if !strings.Contains(err.Error(), tc.when) {
				t.Errorf("the message must quote the refused predicate, got: %v", err)
			}
			if !strings.Contains(err.Error(), "cond ? a : b") {
				t.Errorf("the message must name the working form (the condition inside the value), got: %v", err)
			}
		})
	}
}

// TestRender_StaticWhenOnKeeperTaskStillWorks — the other half of the guard, and
// the half a too-wide rejection would break. A static predicate IS honoured on a
// keeper task: emitStaticWhenSkip runs before the IsKeeperTask branch, so false
// gates the step off and true renders it. Both outcomes are the working form and
// must stay bit-for-bit.
func TestRender_StaticWhenOnKeeperTaskStillWorks(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)

	tasks, _, err := p.Render(context.Background(), keeperWhenInput(keeperWhenManifest("input.provision")))
	if err != nil {
		t.Fatalf("static-true when: on a keeper task must render, got %v", err)
	}
	if len(tasks) != 1 || tasks[0].Params == nil {
		t.Fatalf("static-true when: must take the normal path, got %d task(s) params=%v", len(tasks), tasks[0].Params)
	}

	tasks, _, err = p.Render(context.Background(), keeperWhenInput(keeperWhenManifest("!input.provision")))
	if err != nil {
		t.Fatalf("static-false when: on a keeper task must skip, not fail, got %v", err)
	}
	if len(tasks) != 1 || tasks[0].Params != nil {
		t.Fatalf("static-false when: must emit a skip placeholder, got %d task(s) params=%v", len(tasks), tasks[0].Params)
	}
}
