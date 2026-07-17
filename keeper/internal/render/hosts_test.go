package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// hostWithRole builds a HostFacts with SID/coven/role + soulprint (network/os)
// for soulprint.hosts projection tests.
func hostWithRole(sid, role string, coven []string, network, os map[string]any) *topology.HostFacts {
	return &topology.HostFacts{
		SID:       sid,
		Coven:     coven,
		Role:      role,
		Soulprint: map[string]any{"network": network, "os": os},
	}
}

func hostsRunInput(manifest *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("web-1.example.com", "primary", []string{"prod", "web"},
				map[string]any{"primary_ip": "10.0.0.1"}, map[string]any{"family": "debian"}),
			hostWithRole("web-2.example.com", "replica", []string{"prod", "web"},
				map[string]any{"primary_ip": "10.0.0.2"}, map[string]any{"family": "debian"}),
			hostWithRole("db-1.example.com", "replica", []string{"prod", "db"},
				map[string]any{"primary_ip": "10.0.0.3"}, map[string]any{"family": "rhel"}),
		},
	}
}

// moduleScenario builds a scenario from a single module task with the given params.
func moduleScenario(module string, params map[string]any) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "t", Module: &config.ModuleTask{Module: module, Params: params}},
		},
	}
}

// TestRender_SoulprintHosts_Projection — soulprint.hosts is projected from
// in.Hosts in the scenario pass: list size equals the run's host count.
func TestRender_SoulprintHosts_Projection(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": "echo ${ soulprint.hosts.size() }",
	}))
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo 3" {
		t.Fatalf("command = %q, want echo 3", got)
	}
}

// TestRender_SoulprintHostsWhere_FirstByIndex — filter + [0]: cross-host
// primary discovery (declared role), first element via [0].
func TestRender_SoulprintHostsWhere_FirstByIndex(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `redis-cli replicaof ${ soulprint.hosts.where("role == 'primary'")[0].network.primary_ip } 6379`,
	}))
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "redis-cli replicaof 10.0.0.1 6379" {
		t.Fatalf("command = %q", got)
	}
}

// TestRender_SoulprintWhere_Synonym — soulprint.where(...) ≡ soulprint.hosts.where(...).
func TestRender_SoulprintWhere_Synonym(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `count=${ size(soulprint.where("'web' in covens")) }`,
	}))
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "count=2" {
		t.Fatalf("command = %q, want count=2", got)
	}
}

// TestRender_SoulprintHostsChoirs_CrossHost — soulprint.hosts[].choirs is
// visible cross-host (ADR-044, S-T4): filters by choir membership among the
// run's hosts.
func TestRender_SoulprintHostsChoirs_CrossHost(t *testing.T) {
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `count=${ size(soulprint.hosts.where("'voters' in choirs")) }`,
	}))
	in.Hosts[0].Choirs = []string{"primaries", "voters"}
	in.Hosts[1].Choirs = []string{"voters"}
	// in.Hosts[2] — no choir memberships.

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "count=2" {
		t.Fatalf("command = %q, want count=2", got)
	}
}

// TestRender_LoopOverSoulprintHostsWhere — loop.items over
// soulprint.hosts.where(...): expands into N tasks over the run's filtered
// hosts (declared role).
func TestRender_LoopOverSoulprintHostsWhere(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{
				Name: "per replica",
				Loop: &config.LoopSpec{
					Items: `${ soulprint.hosts.where("role == 'replica'") }`,
					As:    "replica",
				},
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ replica.network.primary_ip }"},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(manifest)
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (two replicas)", len(tasks))
	}
	got := []string{
		tasks[0].Params.GetFields()["cmd"].GetStringValue(),
		tasks[1].Params.GetFields()["cmd"].GetStringValue(),
	}
	want := map[string]bool{"echo 10.0.0.2": false, "echo 10.0.0.3": false}
	for _, g := range got {
		if _, ok := want[g]; !ok {
			t.Fatalf("unexpected command %q (got %v)", g, got)
		}
		want[g] = true
	}
	for cmd, seen := range want {
		if !seen {
			t.Fatalf("expected command %q among %v", cmd, got)
		}
	}
}

// TestRender_DestinyIsolation_HostsForbidden — soulprint.hosts in a destiny
// pass → validation error (isolation, orchestration.md §4.1), not a silent
// empty list.
func TestRender_DestinyIsolation_HostsForbidden(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "leaky",
		Input: config.InputSchemaMap{},
		Tasks: []config.Task{
			{
				Name: "leak hosts",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ soulprint.hosts.size() }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	in := hostsRunInput(applyScenario("leaky", map[string]any{}))
	in.Destiny = res

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("expected isolation error for soulprint.hosts in destiny")
	}
	if !strings.Contains(err.Error(), "soulprint.hosts") {
		t.Fatalf("expected message about soulprint.hosts, got: %v", err)
	}
}

// TestRender_DestinyIsolation_SelfStillWorks — soulprint.self stays
// available in a destiny (per-host facts are a stable layer); isolation
// only concerns hosts.
func TestRender_DestinyIsolation_SelfStillWorks(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "ok",
		Input: config.InputSchemaMap{},
		Tasks: []config.Task{
			{
				Name: "use self",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ soulprint.self.os.family }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	in := RenderInput{
		Scenario:    applyScenario("ok", map[string]any{}),
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("web-1.example.com", "primary", []string{"prod"},
				map[string]any{"primary_ip": "10.0.0.1"}, map[string]any{"family": "debian"}),
		},
		Destiny: res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo debian" {
		t.Fatalf("command = %q, want echo debian", got)
	}
}

// TestRender_DestinyIsolation_SelfArch — guard for the invariant relaxation
// (ADR-009/010 amendment): destiny CEL reads the target host's stable
// self-fact soulprint.self.os.arch — a synthetic arm64 is substituted into
// destiny params. Symmetric with .tmpl render_context (ADR-012(d)):
// self-facts are available in CEL .yml too.
func TestRender_DestinyIsolation_SelfArch(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "arch-aware",
		Input: config.InputSchemaMap{},
		Tasks: []config.Task{
			{
				Name: "fetch by arch",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "install --arch ${ soulprint.self.os.arch }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	in := RenderInput{
		Scenario:    applyScenario("arch-aware", map[string]any{}),
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("arm-1.example.com", "primary", []string{"prod"},
				map[string]any{"primary_ip": "10.0.0.1"},
				map[string]any{"family": "debian", "arch": "arm64"}),
		},
		Destiny: res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "install --arch arm64" {
		t.Fatalf("command = %q, want install --arch arm64", got)
	}
}

// TestRender_DestinyIsolation_WhereForbidden — soulprint.where(...) in a
// destiny pass → isolation error (run topology stays scenario-only), mirrors
// TestRender_DestinyIsolation_HostsForbidden for the synonym accessor.
func TestRender_DestinyIsolation_WhereForbidden(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "leaky-where",
		Input: config.InputSchemaMap{},
		Tasks: []config.Task{
			{
				Name: "leak where",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": `echo ${ size(soulprint.where("'prod' in covens")) }`},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	in := hostsRunInput(applyScenario("leaky-where", map[string]any{}))
	in.Destiny = res

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("expected isolation error for soulprint.where in destiny")
	}
	if !strings.Contains(err.Error(), "soulprint.hosts") {
		t.Fatalf("expected message about soulprint.hosts isolation, got: %v", err)
	}
}

// TestRender_ScenarioSelfArch — the scenario pass isn't broken by the
// relaxation: the same self-fact soulprint.self.os.arch reads directly in a
// scenario module task.
func TestRender_ScenarioSelfArch(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: moduleScenario("core.exec.run", map[string]any{
			"cmd": "install --arch ${ soulprint.self.os.arch }",
		}),
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("arm-1.example.com", "primary", []string{"prod"},
				map[string]any{"primary_ip": "10.0.0.1"},
				map[string]any{"family": "debian", "arch": "arm64"}),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "install --arch arm64" {
		t.Fatalf("command = %q, want install --arch arm64", got)
	}
}

// TestRender_ApplyDestiny_InputArchCompat — apply:input stays a valid
// channel after the relaxation (backward compat): scenario renders arch from
// soulprint.self and passes it into the destiny via apply:input; the
// destiny reads input.arch (not soulprint.self directly) — the old idiom
// isn't broken.
func TestRender_ApplyDestiny_InputArchCompat(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "via-input",
		Input: config.InputSchemaMap{"arch": {Type: "string", Required: true}},
		Tasks: []config.Task{
			{
				Name: "use input arch",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "install --arch ${ input.arch }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	in := RenderInput{
		// scenario computes arch from the target host's self and passes it into the destiny.
		Scenario:    applyScenario("via-input", map[string]any{"arch": "${ soulprint.self.os.arch }"}),
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("arm-1.example.com", "primary", []string{"prod"},
				map[string]any{"primary_ip": "10.0.0.1"},
				map[string]any{"family": "debian", "arch": "arm64"}),
		},
		Destiny: res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "install --arch arm64" {
		t.Fatalf("command = %q, want install --arch arm64", got)
	}
}

// TestRender_EmptyFilterIndex0_StepError — where selected no one → [0] over
// an empty list → a render step error (clear, not a panic). "primary not
// found" is a step error, not a silent null.
func TestRender_EmptyFilterIndex0_StepError(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `replicaof ${ soulprint.hosts.where("role == 'no-such'")[0].network.primary_ip }`,
	}))
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("expected render error for [0] over an empty filter")
	}
}

// TestRender_HostMissingFactSection — qa: when a host's Soulprint is missing
// a section (network/os), render substitutes an empty map, so a field access
// gives "no such key" on the field itself (family/primary_ip), not the
// section (os/network). Pins down the actual behavior: a clear render error,
// not a panic.
func TestRender_HostMissingFactSection(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `fam=${ soulprint.hosts.where("os.family == 'debian'").size() }`,
	}))
	// Host without an os section: Soulprint with no os key → soulprintSection returns {}.
	in.Hosts = []*topology.HostFacts{
		{SID: "x.example.com", Coven: []string{"prod"}, Role: "primary", Soulprint: map[string]any{}},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("expected render error for where over a host without os fact")
	}
	if !strings.Contains(err.Error(), "family") {
		t.Fatalf("expected no-such-key for field family (os section = empty map), got: %v", err)
	}
}
