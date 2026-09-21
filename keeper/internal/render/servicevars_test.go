package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestRender_InterpolatesServiceVars — `${ vars.* }` in params renders from
// RenderInput.ServiceVars (slice E2 passthrough). Proves service vars reaches the
// per-host CEL phase through the whole Render pipeline.
func TestRender_InterpolatesServiceVars(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "cfg",
		Tasks: []config.Task{
			{
				Name:   "write conn",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "connect ${ vars.db.host }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		ServiceVars: map[string]any{"db": map[string]any{"host": "pg-primary"}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "connect pg-primary" {
		t.Errorf("command = %q, want %q", got, "connect pg-primary")
	}
}

// TestRender_ServiceVarsInWhere — service vars is available in the expression-key where:.
func TestRender_ServiceVarsInWhere(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "gated",
		Tasks: []config.Task{
			{
				Name:   "feature gate",
				Where:  "vars.feature.enabled",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "go"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	enabled := RenderInput{
		Scenario:    manifest,
		ServiceVars: map[string]any{"feature": map[string]any{"enabled": true}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, plans, err := p.Render(context.Background(), enabled)
	if err != nil {
		t.Fatalf("Render(enabled): %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 1 {
		t.Errorf("enabled: TargetSIDs = %v, want 1 host", got)
	}

	disabled := enabled
	disabled.ServiceVars = map[string]any{"feature": map[string]any{"enabled": false}}
	_, plans, err = p.Render(context.Background(), disabled)
	if err != nil {
		t.Fatalf("Render(disabled): %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 0 {
		t.Errorf("disabled: TargetSIDs = %v, want 0 hosts (where filtered)", got)
	}
}

// TestRender_LoopOverServiceVars — `items: ${ vars.users }` expands over service vars
// (a host-invariant source for the loop axis).
func TestRender_LoopOverServiceVars(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ vars.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		ServiceVars: map[string]any{"users": []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "bob"},
		}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if cmdOf(t, tasks[0]) != "useradd alice" || cmdOf(t, tasks[1]) != "useradd bob" {
		t.Errorf("loop commands = %q, %q", cmdOf(t, tasks[0]), cmdOf(t, tasks[1]))
	}
}

// TestRender_EmptyServiceVarsNoLeak — a missing ServiceVars in RenderInput doesn't
// break a run that doesn't touch service vars (no panic, no leak into env).
func TestRender_EmptyServiceVarsNoLeak(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "plain",
		Tasks: []config.Task{
			{Name: "noop", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo hi"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cmdOf(t, tasks[0]); got != "echo hi" {
		t.Errorf("command = %q, want %q", got, "echo hi")
	}
}

// TestRender_ApplyDestiny_ServiceVarsNotLeaked — destiny isolation (slice A):
// destiny does NOT see service vars directly. Despite a non-empty ServiceVars in
// scenario scope, a `${ vars.* }` reference inside a destiny task gives an
// eval error (no-such-key), because renderApplyDestiny builds the destiny
// RenderInput with an empty ServiceVars.
func TestRender_ApplyDestiny_ServiceVarsNotLeaked(t *testing.T) {
	leaky := &ResolvedDestiny{
		Name: "leaky",
		Tasks: []config.Task{
			{
				Name:   "peek service vars",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ vars.secret }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("leaky", nil),
		ServiceVars: map[string]any{"secret": "topsecret"},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: leaky},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("expected an eval error: service vars should not be visible in the destiny pass")
	}
	if !strings.Contains(err.Error(), "no such key: secret") {
		t.Errorf("error is not about service vars: %v", err)
	}
}

// TestRender_ApplyDestiny_ServiceVarsViaInput — the correct path for service vars into
// destiny: via apply: input:. service vars resolves in the scenario env (parent),
// and the value is passed through into destiny's input, which sees only the
// result.
func TestRender_ApplyDestiny_ServiceVarsViaInput(t *testing.T) {
	dst := &ResolvedDestiny{
		Name:  "via-input",
		Input: config.InputSchemaMap{"db_host": {Type: "string", Required: true}},
		Tasks: []config.Task{
			{
				Name:   "use host",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "connect ${ input.db_host }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("via-input", map[string]any{"db_host": "${ vars.db.host }"}),
		ServiceVars: map[string]any{"db": map[string]any{"host": "pg-primary"}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: dst},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "connect pg-primary" {
		t.Errorf("command = %q, want %q (service vars via apply:input)", got, "connect pg-primary")
	}
}
