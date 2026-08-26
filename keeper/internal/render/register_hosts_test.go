package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	celpkg "github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// captureScenario — a probe on every host plus ONE keeper capture reading the
// register across hosts. The shape NIM-711 exists for; every test below renders it.
func captureScenario(value string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "per-host-capture",
		Tasks: []config.Task{
			{Name: "record", On: "keeper", Module: &config.ModuleTask{
				Module: "core.state.set",
				Params: map[string]any{"field": "node_ids", "value": value},
			}},
		},
	}
}

// TestRegisterHosts_KeeperCaptureSeesEveryHost — ★ GUARD for the invariant NIM-711
// adds: inside an `on: keeper` task, `register.hosts.<name>` is that register
// across the run's hosts, keyed by SID, with each host's OWN value.
//
// It is the only route a per-host value has into `incarnation.state`: the capture
// is a keeper task, so it binds no soulprint and reads only the keeper bucket
// ([ADR-056] slice 2) — `register.node_id` there is not "some host's value", it is
// nothing. Losing the inversion does not fail loudly: the capture would write a
// map with the wrong shape, or nothing, and the state is only read back later.
//
// The two hosts carry DIFFERENT payloads on purpose — the L0 corpus case
// (examples/service/state-verbs/scenario/per-host-capture) applies one mock
// register to every host, so only Go can pin that the values are not smeared.
//
// Mutation: in [registerHosts], key the inner map by `name` instead of `sid` (or
// return `in.Register`) and this fails on the read-back below.
func TestRegisterHosts_KeeperCaptureSeesEveryHost(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    captureScenario("${ register.hosts.node_id }"),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("node-a", []string{"svc"}, nil),
			host("node-b", []string{"svc"}, nil),
		},
		RegisterByHost: map[string]map[string]any{
			"node-a": {"node_id": map[string]any{"stdout": "aaa"}},
			"node-b": {"node_id": map[string]any{"stdout": "bbb"}},
		},
		Ctx: context.Background(),
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	byHost := tasks[0].Params.GetFields()["value"].GetStructValue().GetFields()
	if len(byHost) != 2 {
		t.Fatalf("value has %d hosts, want 2: %v", len(byHost), byHost)
	}
	for sid, want := range map[string]string{"node-a": "aaa", "node-b": "bbb"} {
		got := byHost[sid].GetStructValue().GetFields()["stdout"].GetStringValue()
		if got != want {
			t.Fatalf("value[%q].stdout = %q, want %q (each host's own register)", sid, got, want)
		}
	}
}

// TestRegisterHosts_ExcludesKeeperBucket — ★ GUARD: the synthetic keeper SID is
// not a host and must not appear in register.hosts.
//
// [RenderInput.RegisterByHost] carries the keeper bucket under
// [KeeperTargetSID] (see keeper/internal/scenario/keeper_dispatch.go
// keeperRegisterBucket), so the inversion walks over it. Including it would put a
// `"keeper"` entry into a map the author iterates as hosts — a `foreach` in a
// migration, a size() against incarnation.host_count — and nothing downstream can
// tell that entry from a host.
//
// Mutation: delete the `sid == KeeperTargetSID` skip in [registerHosts].
func TestRegisterHosts_ExcludesKeeperBucket(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    captureScenario("${ register.hosts.node_id }"),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("node-a", []string{"svc"}, nil)},
		RegisterByHost: map[string]map[string]any{
			"node-a":        {"node_id": map[string]any{"stdout": "aaa"}},
			KeeperTargetSID: {"node_id": map[string]any{"stdout": "not-a-host"}},
		},
		Ctx: context.Background(),
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	byHost := tasks[0].Params.GetFields()["value"].GetStructValue().GetFields()
	if _, ok := byHost[KeeperTargetSID]; ok {
		t.Fatalf("value carries the synthetic %q bucket as a host: %v", KeeperTargetSID, byHost)
	}
	if len(byHost) != 1 {
		t.Fatalf("value has %d entries, want 1 (the single real host)", len(byHost))
	}
}

// TestRegisterHosts_UnavailableOnHostTask — ★ GUARD for the isolation half: a
// Soul-side task must NOT reach other hosts' registers through this root.
// `register.<name>` on a host is deliberately that host's own value ([ADR-0083]
// §5); register.hosts would be a second, cross-host channel into a context that
// has no business seeing one.
//
// The cut-off is at COMPILE time, not "resolves to an empty map": an empty map is
// a silent wrong answer (`.size() == 0`, an empty foreach), an ErrUnsupported is a
// stopped render.
//
// Mutation: set AllowRegisterHosts: true in [hostVars] and this stops erroring.
func TestRegisterHosts_UnavailableOnHostTask(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "per-host-capture",
		Tasks: []config.Task{
			{Name: "echo", Module: &config.ModuleTask{
				Module: "core.exec.run",
				Params: map[string]any{"cmd": "echo", "args": []any{"${ register.hosts.node_id }"}},
			}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:       manifest,
		Incarnation:    IncarnationMeta{Name: "svc"},
		Hosts:          []*topology.HostFacts{host("node-a", []string{"svc"}, nil)},
		RegisterByHost: map[string]map[string]any{"node-a": {"node_id": map[string]any{"stdout": "aaa"}}},
		Ctx:            context.Background(),
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render succeeded: a host task reached register.hosts")
	}
	var unsupported *celpkg.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *cel.ErrUnsupported (compile-time isolation, not an eval miss)", err)
	}
	if !strings.Contains(unsupported.Feature, "register.hosts") {
		t.Fatalf("feature = %q, want it to name register.hosts", unsupported.Feature)
	}
}

// TestRegisterHosts_UnknownNameNamesTheRegister — the accessor exists in a keeper
// task even with nothing registered yet, so a misspelt register fails as
// `no such key: <name>` — the author's actual mistake. Placing `hosts` in the
// activation only when non-empty would report `no such key: hosts`, which reads as
// "this accessor is not available here" and sends the author after the wrong bug.
//
// Mutation: return early from [cel.Vars.registerRoot] when RegisterHosts is empty.
func TestRegisterHosts_UnknownNameNamesTheRegister(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    captureScenario("${ register.hosts.typo }"),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("node-a", []string{"svc"}, nil)},
		Ctx:         context.Background(),
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render succeeded on an unregistered name")
	}
	if !strings.Contains(err.Error(), "no such key: typo") {
		t.Fatalf("err = %v, want `no such key: typo` (the register name, not the accessor)", err)
	}
}

// TestRegisterHosts_UnavailableOnTemplatePath — ★ GUARD for the one cross-context
// call: [Pipeline.resolveTemplateUsesInput] resolves a HOST task's `template:`
// param in the KEEPER context (the path is host-invariant per the pilot
// contract), which would otherwise hand a host task the keeper-only accessor and
// let a cross-host value choose which .tmpl file gets read. The per-host pass
// would reject the same expression a moment later, so the leak is narrow — but it
// makes the isolation depend on pass ordering rather than on the flag.
//
// A non-string `template:` is what reaches the CEL branch (a plain `${ … }` cell
// is already a string and is taken literally).
//
// Mutation: pass keeperVars(in) unchanged in [Pipeline.resolveTemplateUsesInput]
// and the error becomes "must resolve to a non-empty path string" — the render
// still fails, but for the wrong reason and after rendering the value.
func TestRegisterHosts_UnavailableOnTemplatePath(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "per-host-capture",
		Tasks: []config.Task{
			{Name: "render", Module: &config.ModuleTask{
				Module: moduleFileRendered,
				Params: map[string]any{
					paramTemplate: map[string]any{"path": "${ register.hosts.node_id }"},
					"dest":        "/etc/app.conf",
				},
			}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:       manifest,
		Incarnation:    IncarnationMeta{Name: "svc"},
		Hosts:          []*topology.HostFacts{host("node-a", []string{"svc"}, nil)},
		RegisterByHost: map[string]map[string]any{"node-a": {"node_id": map[string]any{"stdout": "aaa"}}},
		Ctx:            context.Background(),
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render succeeded: a host task's template path reached register.hosts")
	}
	var unsupported *celpkg.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *cel.ErrUnsupported (the keeper context must not open the accessor here)", err)
	}
	if !strings.Contains(unsupported.Feature, "register.hosts") {
		t.Fatalf("feature = %q, want it to name register.hosts", unsupported.Feature)
	}
}
