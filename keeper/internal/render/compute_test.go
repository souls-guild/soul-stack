package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// compute: — scenario-level computed vars (ADR-009 amendment 2026-06-23):
// resolved ONCE in a run-level context (without soulprint), the result
// `compute.<name>` is visible in apply.input and state_changes BIT-FOR-BIT (drift
// eliminated).

// resolveCompute: a chain (compute references an earlier compute) plus a
// run-level input/essence context. Declaration order matters.
func TestResolveCompute_ChainAndContext(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "base", Value: "${ merge(essence.defaults, default(input.over, {})) }"},
			{Name: "full", Value: "${ merge(compute.base, { 'extra': 'yes' }) }"},
			{Name: "n", Value: int64(7)},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Essence:  map[string]any{"defaults": map[string]any{"a": "1", "b": "2"}},
		Input:    map[string]any{"over": map[string]any{"b": "9"}},
		Hosts:    []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:      context.Background(),
	}
	got, err := p.resolveCompute(in)
	if err != nil {
		t.Fatalf("resolveCompute: %v", err)
	}
	base, _ := got["base"].(map[string]any)
	if base["a"] != "1" || base["b"] != "9" {
		t.Fatalf("compute.base = %#v, want {a:1, b:9} (input.over.b бьёт essence)", got["base"])
	}
	full, _ := got["full"].(map[string]any)
	if full["a"] != "1" || full["b"] != "9" || full["extra"] != "yes" {
		t.Fatalf("compute.full = %#v, want base + extra:yes (ссылка на ранний compute)", got["full"])
	}
	if got["n"] != int64(7) {
		t.Fatalf("compute.n = %#v, want литерал 7", got["n"])
	}
}

// ★ Isolation barrier #2: a compute expression referencing soulprint.self fails
// with no-such-key — compute's resolve context is run-level (no soulprint), so
// compute is host-invariant and safe in state_changes.
func TestResolveCompute_BarrierSoulprint(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "ip", Value: "${ soulprint.self.network.primary_ip }"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Hosts: []*topology.HostFacts{host("h1", []string{"create"},
			map[string]any{"network": map[string]any{"primary_ip": "10.0.0.1"}})},
		Ctx: context.Background(),
	}
	_, err := p.resolveCompute(in)
	if err == nil {
		t.Fatalf("★ ожидалась ошибка: compute не должен видеть soulprint (барьер host-инвариантности)")
	}
	if !strings.Contains(err.Error(), "compute.ip") {
		t.Fatalf("ошибка должна указывать на compute.ip, получено: %v", err)
	}
}

// compute is available in apply.input (through the params/where render) AND in
// state_changes, the same value (the drift guard is eliminated by compute itself).
// Run through Render + RenderStateOps on one RenderInput.
func TestCompute_SameValueInTasksAndStateChanges(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "cfg", Value: "${ merge(essence.base, { 'maxmemory': string(int(input.mb)) + 'mb' }) }"},
		},
		StateChanges: &config.StateChanges{
			IsList: true,
			Ops: []config.StateChange{
				{Verb: config.VerbSet, Field: "redis_config", Value: "${ compute.cfg }"},
			},
		},
		Tasks: []config.Task{
			{
				Name: "render",
				Module: &config.ModuleTask{
					Module: "core.noop.run",
					Params: map[string]any{"config": "${ compute.cfg }"},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Essence:     map[string]any{"base": map[string]any{"appendonly": "yes"}},
		Input:       map[string]any{"mb": int64(512)},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:         context.Background(),
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	taskCfg := tasks[0].Params.GetFields()["config"].GetStructValue().GetFields()
	if taskCfg["appendonly"].GetStringValue() != "yes" || taskCfg["maxmemory"].GetStringValue() != "512mb" {
		t.Fatalf("apply.input.config = %v, want {appendonly:yes, maxmemory:512mb}", taskCfg)
	}

	ops, err := p.RenderStateOps(in)
	if err != nil {
		t.Fatalf("RenderStateOps: %v", err)
	}
	if len(ops) != 1 || ops[0].Field != "redis_config" {
		t.Fatalf("ops = %+v, want one set redis_config", ops)
	}
	stateCfg, _ := ops[0].Value.(map[string]any)
	if stateCfg["appendonly"] != "yes" || stateCfg["maxmemory"] != "512mb" {
		t.Fatalf("state_changes.redis_config = %#v, want {appendonly:yes, maxmemory:512mb} (== apply.input.config)", ops[0].Value)
	}
}

// ★ Isolation barrier #1: compute does NOT leak into the isolated destiny pass.
// Structural: destinyIn (destiny.go:99-107) carries no Compute field, so
// resolveCompute for a destiny input yields nil — `compute.<name>` inside destiny
// is a plain no-such-key.
func TestCompute_NotLeakingIntoDestiny(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	destinyIn := RenderInput{
		Scenario:        &config.ScenarioManifest{Name: "redis"},
		destinyIsolated: true,
		Ctx:             context.Background(),
	}
	got, err := p.resolveCompute(destinyIn)
	if err != nil {
		t.Fatalf("resolveCompute(destiny): %v", err)
	}
	if got != nil {
		t.Fatalf("★ destiny compute = %#v, want nil (изоляция: compute не протекает в destiny)", got)
	}
}

// ★ Isolation barrier #1 (POSITIVE, end-to-end): a scenario with a NON-empty
// compute: plus apply:destiny, whose destiny step references ${ compute.x } — a
// full Render fails with no-such-key (destinyIn.Compute isn't forwarded,
// destiny.go:99-107). Proves structural isolation on a REAL pass (not just
// resolveCompute in a vacuum): the parent resolves compute (apply.input composes
// the value), but inside destiny `compute.*` is unavailable — the value does NOT
// reach it.
func TestCompute_NotLeakingIntoDestiny_RenderThrough(t *testing.T) {
	// destiny references compute.cfg, which doesn't exist in the isolated env.
	leaky := flatDestiny()
	leaky.Tasks[0].Module.Params["content"] = "${ compute.cfg }"
	res := &stubDestinyResolver{resolved: leaky}

	scenario := applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"})
	// A NON-empty compute: on the parent — it resolves in scenario-scope, but is
	// NOT forwarded into destiny.
	scenario.Compute = config.ComputeBlock{
		{Name: "cfg", Value: "${ merge(essence.base, {}) }"},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    scenario,
		Essence:     map[string]any{"base": map[string]any{"appendonly": "yes"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("★ Render: ожидалась ошибка — destiny не должна видеть compute (изоляция, compute.* недоступен в destiny-проходе)")
	}
}

// ★ Forward-reference forbidden: compute[i] references compute[j] declared LATER
// → no-such-key (resolution follows declaration order strictly, acc only
// accumulates what's already computed). Proves that a forward-ref doesn't
// silently "pick up" the later value, but fails honestly.
func TestResolveCompute_ForwardReferenceIsNoSuchKey(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			// early references late, declared below — at the time early is resolved,
			// it's not yet in acc.
			{Name: "early", Value: "${ compute.late }"},
			{Name: "late", Value: "ready"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Hosts:    []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:      context.Background(),
	}
	_, err := p.resolveCompute(in)
	if err == nil {
		t.Fatal("★ ожидалась ошибка: forward-reference compute.late из compute.early (объявлен позже)")
	}
	if !strings.Contains(err.Error(), "compute.early") {
		t.Fatalf("ошибка должна указывать на compute.early, получено: %v", err)
	}
}

// ★ A broken CEL expression inside compute → the error is wrapped as
// render: compute.<name>: (not a panic/silent skip). Proves the expression's
// syntax error is attributed to the specific compute entry.
func TestResolveCompute_BrokenCELWrapped(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "bad", Value: "${ 1 + + }"}, // syntactically invalid CEL
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Hosts:    []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:      context.Background(),
	}
	_, err := p.resolveCompute(in)
	if err == nil {
		t.Fatal("★ ожидалась ошибка на битом CEL внутри compute.bad")
	}
	if !strings.Contains(err.Error(), "render: compute.bad") {
		t.Fatalf("ошибка должна быть обёрнута как 'render: compute.bad: ...', получено: %v", err)
	}
}
