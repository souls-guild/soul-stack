package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// loopDestiny is a destiny with one loop: task over input.changes (mirrors
// service/redis scenario update_acl → destiny update_acls). Each item expands
// into a separate RenderedTask. input.changes is passed through the parent's
// apply.input.
func loopDestiny() *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "pilot-loop",
		Input: config.InputSchemaMap{
			"changes": {Type: "array", Required: true},
		},
		Tasks: []config.Task{
			{
				Name: "Apply ACL patch per user",
				Loop: &config.LoopSpec{
					Items:   "${ input.changes }",
					As:      "change",
					IndexAs: "username",
				},
				Module: &config.ModuleTask{
					Module: "core.cmd.shell",
					Params: map[string]any{
						"cmd": "redis-cli ACL SETUSER ${ username } ${ change.acl }",
					},
				},
			},
		},
	}
}

// TestRender_ApplyDestiny_Loop_Expands proves loop INSIDE a destiny (slice E
// removed): one loop task expands into N RenderedTask per input.changes
// element, item binding (<as>/<index_as>) is correct, indices are contiguous.
// Mirrors scenario add_acl_user, but the loop lives in the destiny task.
func TestRender_ApplyDestiny_Loop_Expands(t *testing.T) {
	res := &stubDestinyResolver{resolved: loopDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": map[string]any{
			"alice": map[string]any{"acl": "~* +@all"},
			"bob":   map[string]any{"acl": "~foo +get"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// object → 2 iterations, alphabetical order by key (alice, bob).
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d plans=%d, want 2/2 (loop in destiny expanded)", len(tasks), len(plans))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1 (continuous through loop in destiny)", tasks[0].Index, tasks[1].Index)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "redis-cli ACL SETUSER alice ~* +@all" {
		t.Errorf("tasks[0].cmd = %q (binding username/change.acl of first iteration)", got)
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "redis-cli ACL SETUSER bob ~foo +get" {
		t.Errorf("tasks[1].cmd = %q (binding of second iteration)", got)
	}
	if plans[0].TaskIndex != 0 || plans[1].TaskIndex != 1 {
		t.Errorf("plans indices = %d,%d, want 0,1", plans[0].TaskIndex, plans[1].TaskIndex)
	}
}

// TestRender_ApplyDestiny_Loop_MixedPlan proves a module task BEFORE loop:destiny
// and AFTER: contiguous indices continue across the destiny-loop boundary (as
// they do across apply:destiny).
func TestRender_ApplyDestiny_Loop_MixedPlan(t *testing.T) {
	res := &stubDestinyResolver{resolved: loopDestiny()}
	scn := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "pre", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo pre"}}},
			{Name: "apply", Apply: &config.ApplyTask{Destiny: "pilot-loop", Input: map[string]any{"changes": "${ input.changes }"}}},
			{Name: "post", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo post"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: scn,
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
			map[string]any{"acl": "~b"},
			map[string]any{"acl": "~c"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// pre(0) + loop×3 (1,2,3) + post(4) = 5 tasks, contiguous indices.
	if len(tasks) != 5 {
		t.Fatalf("len(tasks) = %d, want 5 (pre + loop×3 + post)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (continuous through destiny-loop)", i, rt.Index, i)
		}
	}
	if got := tasks[4].Params.GetFields()["cmd"].GetStringValue(); got != "echo post" {
		t.Errorf("post after destiny-loop rendered incorrectly: %q", got)
	}
}

// TestRender_ApplyDestiny_Loop_Isolation is a REVERSE-GUARD on isolation:
// loop: items in a destiny reference soulprint.hosts (scenario-only roster,
// unreachable from an isolated destiny) → isolation error (AllowHosts=false
// under destinyIsolated). Catches an isolation leak if loopInvariantVars stops
// honoring destinyIsolated.
func TestRender_ApplyDestiny_Loop_Isolation(t *testing.T) {
	leaky := loopDestiny()
	leaky.Tasks[0].Loop.Items = `${ soulprint.hosts.where("role == 'master'") }`
	res := &stubDestinyResolver{resolved: leaky}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected isolation error - destiny-loop must not see soulprint.hosts (AllowHosts=false)")
	}
}

// TestRender_ApplyDestiny_Loop_RegisterIsolation is a REVERSE-GUARD: loop:
// items in a destiny reference register (scenario-scope, empty in an isolated
// destiny) → no-such-key. A destiny's register is empty before the barrier
// (isolation, ADR-009); cross-scope register doesn't leak into loop.items.
func TestRender_ApplyDestiny_Loop_RegisterIsolation(t *testing.T) {
	leaky := loopDestiny()
	leaky.Tasks[0].Loop.Items = "${ register.probe.stdout_lines }"
	res := &stubDestinyResolver{resolved: leaky}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		// scenario-scope register exists, but must not be visible in the destiny env.
		Register: map[string]any{"probe": map[string]any{"stdout_lines": []any{"x"}}},
		Hosts:    []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:  res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected error - destiny-loop must not see scenario register (isolation)")
	}
}

// TestRender_ApplyDestiny_Loop_OnChanges proves onchanges on a destiny-loop
// task: contiguous Index stays intact and the register name resolves to an
// Index when there's loop fan-out inside a destiny (inherited via
// resolveOnChanges' final pass).
//
// register+loop semantics (shared with scenario, registerIndex.go): N
// iterations share ONE register name → the register-name→Index map keeps the
// last iteration. onchanges on a loop register resolves to the loop's last
// iteration Index. This is existing renderLoopTask behavior (not
// destiny-specific); this test confirms destiny-loop INHERITS it without
// surprises and indices stay intact.
func TestRender_ApplyDestiny_Loop_OnChanges(t *testing.T) {
	d := loopDestiny()
	d.Tasks[0].Register = "acl_patch"
	d.Tasks = append(d.Tasks, config.Task{
		Name:      "Notify after patch",
		OnChanges: []string{"acl_patch"},
		Module:    &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo reloaded"}},
	})
	res := &stubDestinyResolver{resolved: d}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
			map[string]any{"acl": "~b"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// loop×2 (Index 0,1) + consumer (Index 2).
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 (loop×2 + consumer)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d", i, rt.Index, i)
		}
	}
	// onchanges register name acl_patch resolves to the Index of the LAST loop
	// iteration (registerIndex: one register → last Index; semantics shared with scenario).
	consumer := tasks[2]
	if len(consumer.OnChangesIdx) != 1 {
		t.Fatalf("OnChangesIdx = %v, want 1 (register loop-task -> Index of last iteration)", consumer.OnChangesIdx)
	}
	if consumer.OnChangesIdx[0] != 1 {
		t.Errorf("OnChangesIdx = %v, want [1] (remap register-name to Index of last loop-iteration)", consumer.OnChangesIdx)
	}
}

// TestRender_ApplyDestiny_Loop_StaticWhenSkip proves a destiny-loop task with
// when: statically evaluating to false (input.action != ...) yields N
// skip-placeholders (parity with scenario static-when+loop): tasks stay in the
// plan (Params==nil), but params are NOT rendered — so a broken/unreachable
// ${...} in params doesn't fail the render. Mirrors scenario's static-when
// placeholder-skip, for a loop inside a destiny.
func TestRender_ApplyDestiny_Loop_StaticWhenSkip(t *testing.T) {
	d := &ResolvedDestiny{
		Name: "pilot-loop-when",
		Input: config.InputSchemaMap{
			"action":  {Type: "string", Required: true},
			"changes": {Type: "array", Required: true},
		},
		Tasks: []config.Task{
			{
				Name: "Apply ACL patch (gated by action)",
				When: "input.action == 'update_acls'", // static-false when action=create
				Loop: &config.LoopSpec{Items: "${ input.changes }", As: "change", IndexAs: "username"},
				Module: &config.ModuleTask{
					Module: "core.cmd.shell",
					// missing_var is deliberately absent — rendering params would fail if not for the skip.
					Params: map[string]any{"cmd": "redis-cli ACL SETUSER ${ username } ${ change.missing_var }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop-when", map[string]any{
			"action":  "${ input.action }",
			"changes": "${ input.changes }",
		}),
		Input: map[string]any{"action": "create", "changes": []any{
			map[string]any{"acl": "~a"},
			map[string]any{"acl": "~b"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v (static-when:false must skip params, not fail on missing_var)", err)
	}
	// 2 iterations → 2 skip-placeholders, Params==nil (never rendered).
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (placeholder for each loop-iteration)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Params != nil {
			t.Errorf("tasks[%d].Params != nil - static-when:false must skip rendering params", i)
		}
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (continuous even when skipped)", i, rt.Index, i)
		}
	}
}
