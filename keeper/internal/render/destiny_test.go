package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// stubDestinyResolver is an in-memory resolver for apply:destiny unit tests.
type stubDestinyResolver struct {
	resolved *ResolvedDestiny
	err      error
	gotName  string
}

func (s *stubDestinyResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	s.gotName = name
	if s.err != nil {
		return nil, s.err
	}
	return s.resolved, nil
}

// flatDestiny is a flat test destiny made of two module tasks that read their own input.
func flatDestiny() *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "pilot-flat",
		Input: config.InputSchemaMap{
			"marker_file":    {Type: "string", Required: true},
			"marker_payload": {Type: "string", Required: true},
			"marker_mode":    {Type: "string", Default: "0644"},
		},
		Tasks: []config.Task{
			{
				Name: "Lay down the marker file",
				Module: &config.ModuleTask{
					Module: "core.file.present",
					Params: map[string]any{
						"path":    "${ input.marker_file }",
						"content": "${ input.marker_payload }",
						"mode":    "${ input.marker_mode }",
					},
				},
			},
			{
				Name:        "Record placement",
				ChangedWhen: "false",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ input.marker_file }"},
				},
			},
		},
	}
}

// applyScenario is a scenario with a single apply:destiny task.
func applyScenario(destiny string, applyInput map[string]any) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{
				Name:  "Apply destiny",
				Apply: &config.ApplyTask{Destiny: destiny, Input: applyInput},
			},
		},
	}
}

// TestRender_ApplyDestiny_Expands — apply:destiny expands into destiny tasks
// with plan-wide indexes; apply.input resolves params; defaults are filled in.
func TestRender_ApplyDestiny_Expands(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "${ input.path }", "marker_payload": "${ input.content }"}),
		Input:       map[string]any{"path": "/etc/marker", "content": "ok"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.gotName != "pilot-flat" {
		t.Errorf("resolver got name %q, want pilot-flat", res.gotName)
	}
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d plans=%d, want 2/2", len(tasks), len(plans))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1 (contiguous)", tasks[0].Index, tasks[1].Index)
	}
	if tasks[0].Module != "core.file.present" {
		t.Errorf("task0 module = %q", tasks[0].Module)
	}
	f0 := tasks[0].Params.GetFields()
	if got := f0["path"].GetStringValue(); got != "/etc/marker" {
		t.Errorf("path = %q, want /etc/marker (from apply.input <- scenario.input.path)", got)
	}
	if got := f0["content"].GetStringValue(); got != "ok" {
		t.Errorf("content = %q, want ok", got)
	}
	if got := f0["mode"].GetStringValue(); got != "0644" {
		t.Errorf("mode = %q, want 0644 (picked up from default destiny)", got)
	}
	cmd := tasks[1].Params.GetFields()["cmd"].GetStringValue()
	if cmd != "echo /etc/marker" {
		t.Errorf("command = %q, want echo /etc/marker", cmd)
	}
}

// TestRender_ApplyDestiny_Isolation — destiny does NOT see the scenario scope:
// a reference to scenario input not passed through apply.input fails (no such
// key) rather than silently picking up the parent's value.
func TestRender_ApplyDestiny_Isolation(t *testing.T) {
	leaky := flatDestiny()
	// The destiny task references input.secret_from_scenario, which is NOT in
	// apply.input — it must not be present in the isolated env.
	leaky.Tasks[0].Module.Params["content"] = "${ input.secret_from_scenario }"
	res := &stubDestinyResolver{resolved: leaky}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		// scenario scope contains secret_from_scenario — destiny must NOT see it.
		Input:       map[string]any{"secret_from_scenario": "LEAK"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error - destiny must not see scenario-input (isolation ADR-009)")
	}
}

// TestRender_ApplyDestiny_StateIsolation — destiny does NOT see
// incarnation.state: Variant A (incarnation.state in scenario-render) doesn't
// leak into the destiny pass. State lives in RenderInput (not IncarnationMeta),
// and renderApplyDestiny copies only parentIn.Incarnation (meta) and does NOT
// pass State through → destiny's `incarnation.state` is empty. A destiny task
// reading incarnation.state.x in non-optional form fails with an eval error
// (no such key) — isolation, same as for scenario-input (orchestration.md
// §4.1: state only via apply.input).
func TestRender_ApplyDestiny_StateIsolation(t *testing.T) {
	leaky := flatDestiny()
	leaky.Tasks[0].Module.Params["content"] = "${ incarnation.state.redis_users }"
	res := &stubDestinyResolver{resolved: leaky}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		// scenario scope carries state — destiny must NOT see it.
		State:   map[string]any{"redis_users": map[string]any{"alice": "x"}},
		Hosts:   []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny: res,
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error - destiny must not see incarnation.state (isolation Variant A)")
	}
}

// TestRender_ApplyDestiny_MissingRequired — a required destiny input isn't
// passed via apply.input and has no default → contract error.
func TestRender_ApplyDestiny_MissingRequired(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		// marker_payload (required, no default) isn't passed.
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error for a missing required destiny input")
	}
}

// TestRender_ApplyDestiny_NilResolver — apply:destiny without a resolver → ErrUnsupportedDSL.
func TestRender_ApplyDestiny_NilResolver(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", nil),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		// Destiny: nil
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL", err)
	}
}

// TestRender_ApplyDestiny_UnexpandedInclude — within-destiny include
// expansion happens before render (in the loader/fixture resolver). If a
// resolver returns a ResolvedDestiny with an unexpanded include, render
// catches it as ErrUnexpandedInclude (an expansion bug, not "outside pilot").
func TestRender_ApplyDestiny_UnexpandedInclude(t *testing.T) {
	nested := flatDestiny()
	nested.Tasks = append(nested.Tasks, config.Task{
		Name:    "nested include",
		Include: &config.IncludeTask{Include: "more.yml"},
	})
	res := &stubDestinyResolver{resolved: nested}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnexpandedInclude) {
		t.Fatalf("err = %v, want ErrUnexpandedInclude (unexpanded include in destiny)", err)
	}
}

// TestRender_ApplyDestiny_RejectsSerial — a task inside destiny can't carry
// the scenario-only orchestration key serial: (guardDestinyTask, destiny.go).
// A scenario-level serial: is inherited by destiny through the
// renderApplyDestiny parameter, not through a destiny task field → a
// destiny task's own serial: → ErrUnsupportedDSL.
func TestRender_ApplyDestiny_RejectsSerial(t *testing.T) {
	d := flatDestiny()
	d.Tasks[0].Serial = 1
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (serial: on a destiny task)", err)
	}
}

// TestRender_ApplyDestiny_RejectsRunOnce — mirrors serial:: a task inside
// destiny can't carry run_once: (guardDestinyTask, destiny.go) → ErrUnsupportedDSL.
func TestRender_ApplyDestiny_RejectsRunOnce(t *testing.T) {
	d := flatDestiny()
	d.Tasks[1].RunOnce = true
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (run_once: on a destiny task)", err)
	}
}

// TestRender_ApplyDestiny_ResolverError — a resolver error is propagated.
func TestRender_ApplyDestiny_ResolverError(t *testing.T) {
	res := &stubDestinyResolver{err: errors.New("not found in registry")}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("ghost", nil),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected a propagated resolver error")
	}
}

// TestRender_ApplyDestiny_MixedPlan — a scenario with a module task BEFORE
// apply:destiny: plan-wide indexes continue across the apply boundary.
func TestRender_ApplyDestiny_MixedPlan(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	scn := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "pre", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo pre"}}},
			{Name: "apply", Apply: &config.ApplyTask{Destiny: "pilot-flat", Input: map[string]any{"marker_file": "/m", "marker_payload": "p"}}},
			{Name: "post", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo post"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    scn,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// pre(0) + destiny(1,2) + post(3) = 4 tasks with plan-wide indexes.
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4", len(tasks))
	}
	wantIdx := []int{0, 1, 2, 3}
	for i, rt := range tasks {
		if rt.Index != wantIdx[i] {
			t.Errorf("tasks[%d].Index = %d, want %d", i, rt.Index, wantIdx[i])
		}
	}
	if tasks[3].Module != "core.exec.run" || tasks[3].Params.GetFields()["cmd"].GetStringValue() != "echo post" {
		t.Errorf("post-task after destiny rendered incorrectly: %+v", tasks[3])
	}
}

// TestRender_ApplyDestiny_RejectsNestedApply — a nested apply: inside destiny
// (apply:destiny → the destiny task itself carries apply:) → ErrUnsupportedDSL
// (guardDestinyTask, case task.Apply != nil). apply:destiny is a single-level
// expansion (V2, ADR-009); recursive apply nesting is outside pilot scope.
// Existing destiny-guard tests cover serial/run_once/include inside destiny
// (loop is now SUPPORTED — slice E dropped), but not nested apply — the one
// remaining branch of guardDestinyTask without a test.
func TestRender_ApplyDestiny_RejectsNestedApply(t *testing.T) {
	d := flatDestiny()
	d.Tasks = append(d.Tasks, config.Task{
		Name:  "nested apply",
		Apply: &config.ApplyTask{Destiny: "another", Input: map[string]any{}},
	})
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (nested apply: in destiny)", err)
	}
}

// block: inside destiny is now SUPPORTED (ADR-009 amendment 2026-06-24) —
// mechanism guard tests are in destiny_block_test.go.
