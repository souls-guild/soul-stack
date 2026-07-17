package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Conditional-include group-drop INSIDE apply:destiny (ADR-009 amendment,
// parity with the scenario path). renderApplyDestiny mirrors the scenario loop
// (pipeline.go): tasks expanded from an include under a static `when:` carry
// through Task.IncludeWhen/IncludeGroupID. include-when is evaluated ONCE per
// group in an ISOLATED destiny env (input = resolved apply.input +
// schema-defaults, NOT scenario-scope) and on false drops ALL tasks of the
// group FOR REAL — no RenderedTask emitted, no idx++. The includeGroupKeep
// cache is per-pass, separate from scenario.
//
// Tests build carry-through fields directly (via includeGroup from
// include_group_drop_test.go) — the focus is the render invariant of
// group-drop in destiny.

// applyDestinyScenario wraps a destiny with a conditional-include in a
// scenario with a single apply:destiny task, forwarding applyInput into
// destiny-input.
func applyDestinyScenario(destiny string, applyInput map[string]any) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "Apply destiny", Apply: &config.ApplyTask{Destiny: destiny, Input: applyInput}},
		},
	}
}

// TestDestinyIncludeGroupDrop_WhenFalse_TasksAbsent — ★ dropping a conditional
// include group inside destiny on non-matching input: the group's tasks are
// physically ABSENT from the plan (a real drop), tail indices stay continuous
// with no gaps. include-when references input.topology — resolved against
// destiny-input (apply.input), NOT scenario-input, and must NOT fail
// no-such-key (input.topology is passed via apply.input).
func TestDestinyIncludeGroupDrop_WhenFalse_TasksAbsent(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, cmdTask("head", "head"))
	dtasks = append(dtasks, includeGroup("input.topology == 'sentinel'", 1,
		cmdTask("sentinel-a", "sa"),
		cmdTask("sentinel-b", "sb"),
	)...)
	dtasks = append(dtasks, cmdTask("tail", "tail"))

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "cond-destiny",
		Input: config.InputSchemaMap{"topology": {Type: "string", Required: true}},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyDestinyScenario("cond-destiny", map[string]any{"topology": "standalone"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: dropping the sentinel group on non-sentinel input must NOT fail (no-such-key is not allowed - input.topology is in apply.input): %v", err)
	}
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d plans=%d, want 2/2 (head + tail; group dropped WITHOUT a placeholder)", len(tasks), len(plans))
	}
	for _, rt := range tasks {
		if rt.Name == "sentinel-a" || rt.Name == "sentinel-b" {
			t.Fatalf("task %q of the dropped sentinel group is present in the destiny plan", rt.Name)
		}
	}
	if tasks[0].Name != "head" || tasks[1].Name != "tail" {
		t.Errorf("plan = [%q,%q], want [head,tail]", tasks[0].Name, tasks[1].Name)
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("Index = %d,%d, want 0,1 (contiguous without gaps - drop does not reserve idx)", tasks[0].Index, tasks[1].Index)
	}
}

// TestDestinyIncludeGroupDrop_WhenTrue_TasksPresent — keep on matching input:
// the group's tasks are present and render the normal way (carry-through
// fields don't affect rendering), indices stay continuous.
func TestDestinyIncludeGroupDrop_WhenTrue_TasksPresent(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, cmdTask("head", "head"))
	dtasks = append(dtasks, includeGroup("input.topology == 'sentinel'", 1,
		cmdTask("sentinel-a", "sa"),
		cmdTask("sentinel-b", "sb"),
	)...)
	dtasks = append(dtasks, cmdTask("tail", "tail"))

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "cond-destiny",
		Input: config.InputSchemaMap{"topology": {Type: "string", Required: true}},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyDestinyScenario("cond-destiny", map[string]any{"topology": "sentinel"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("len(tasks)=%d, want 4 (head + 2 groups + tail)", len(tasks))
	}
	want := []string{"head", "sentinel-a", "sentinel-b", "tail"}
	for i, w := range want {
		if tasks[i].Name != w {
			t.Errorf("tasks[%d].Name = %q, want %q", i, tasks[i].Name, w)
		}
		if tasks[i].Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (contiguous numbering)", i, tasks[i].Index, i)
		}
	}
	if tasks[1].Params == nil {
		t.Error("sentinel-a.Params == nil - include-when:true should render the group the normal way")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "sa" {
		t.Errorf("sentinel-a.cmd = %q, want sa", got)
	}
}

// TestDestinyIncludeGroupDrop_Nested — ★ parity with scenario: nested
// conditional-include in destiny. Outer group keeps, inner (a different
// group-id) drops — outer tasks remain, inner ones vanish. Each group's
// include-when is evaluated in an isolated destiny env; group caches are kept
// separate by IncludeGroupID.
func TestDestinyIncludeGroupDrop_Nested(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, cmdTask("head", "head"))
	// Outer group (id=1) — keep when tls=='on'.
	dtasks = append(dtasks, includeGroup("input.tls == 'on'", 1,
		cmdTask("outer-a", "oa"),
	)...)
	// Nested group (id=2) — drop when ha=='off' (a different group-id, its own condition).
	dtasks = append(dtasks, includeGroup("input.ha == 'on'", 2,
		cmdTask("inner-a", "ia"),
		cmdTask("inner-b", "ib"),
	)...)
	dtasks = append(dtasks, cmdTask("tail", "tail"))

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name: "nested-destiny",
		Input: config.InputSchemaMap{
			"tls": {Type: "string", Required: true},
			"ha":  {Type: "string", Required: true},
		},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyDestinyScenario("nested-destiny", map[string]any{"tls": "on", "ha": "off"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// head + outer-a(keep) + tail = 3; the inner group (ha=off) is dropped.
	if len(tasks) != 3 {
		t.Fatalf("len(tasks)=%d, want 3 (head + outer-a + tail; inner group dropped)", len(tasks))
	}
	names := map[string]*RenderedTask{}
	for _, rt := range tasks {
		names[rt.Name] = rt
	}
	if _, ok := names["inner-a"]; ok {
		t.Error("inner-a is present - nested include group (ha=off) should be dropped")
	}
	if _, ok := names["inner-b"]; ok {
		t.Error("inner-b is present - nested include group (ha=off) should be dropped")
	}
	if _, ok := names["outer-a"]; !ok {
		t.Error("outer-a is missing - outer include group (tls=on) should remain")
	}
	if tail := names["tail"]; tail == nil || tail.Index != 2 {
		t.Errorf("tail.Index = %v, want 2 (contiguous after head=0, outer-a=1; inner-drop does not reserve idx)", tail)
	}
}

// TestDestinyIncludeGroupDrop_IsolatedEnv — ★ include-when resolves against
// ISOLATED destiny-input (apply.input + defaults), NOT scenario-scope.
// scenario-input carries topology=='sentinel', but apply.input passes
// standalone → the group MUST drop (if include-when looked at parentIn, it
// would stay).
func TestDestinyIncludeGroupDrop_IsolatedEnv(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, includeGroup("input.topology == 'sentinel'", 1,
		cmdTask("sentinel-only", "so"),
	)...)

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "iso-destiny",
		Input: config.InputSchemaMap{"topology": {Type: "string", Required: true}},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		// apply.input.topology = literal standalone (NOT from scenario-input).
		Scenario: applyDestinyScenario("iso-destiny", map[string]any{"topology": "standalone"}),
		// scenario-scope carries topology=sentinel — destiny must NOT see it.
		Input:       map[string]any{"topology": "sentinel"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks)=%d, want 0 - include-when should read destiny-input (standalone -> drop), NOT scenario-input (sentinel)", len(tasks))
	}
}
