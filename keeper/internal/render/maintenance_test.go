package render

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Monday 2026-11-02 10:00 Moscow; the window below opens at 02:00 Moscow.
var maintenanceAt = time.Date(2026, time.November, 2, 7, 0, 0, 0, time.UTC)

func mondayWindow(t *testing.T) *maintenance.Calculator {
	t.Helper()
	c, err := maintenance.NewCalculator([]maintenance.Window{{
		ID: "w1", Days: []string{"mon"}, StartTime: "02:00", DurationMinutes: 180, Timezone: "Europe/Moscow",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ★ GUARD "no windows → true" (ADR-0093 §8). An incarnation with no windows and no
// freeze is open, with no text, and its next start is the instant asked about.
//
// Mutations: MaintenanceAt's nil branch answering Open: false; Calculator.OpenAt
// dropping `len(c.windows) == 0 ||`; MaintenanceAt passing Text() through without
// HasWindows (CEL would read "any time").
func TestMaintenanceAt_NoWindowsIsOpen(t *testing.T) {
	none, err := maintenance.NewCalculator(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := maintenanceAt.Add(1500 * time.Millisecond)
	want := MaintenanceWindow{Open: true, NextStart: maintenanceAt.Add(time.Second)}
	for name, c := range map[string]MaintenanceCalculator{"nil": nil, "calculator": none} {
		if got := MaintenanceAt(c, at); got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}

	closed := MaintenanceAt(mondayWindow(t), maintenanceAt)
	if closed.Open || closed.Text == "" || !closed.NextStart.Equal(time.Date(2026, time.November, 8, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("a closed window: got %+v", closed)
	}
}

const maintenanceStep = `
name: linear
tasks:
  - name: restart
    module: core.exec.run
    when: in_maintenance_window()
    params: { cmd: restart }
  - name: restart a
    module: core.exec.run
    when: in_maintenance_window() && soulprint.self.sid == "a.example.com"
    params: { cmd: restart }
  - name: note
    module: core.exec.run
    params: { cmd: "echo ${ incarnation.maintenance_window.text }|${ incarnation.maintenance_window.next_start }" }
`

func maintenancePass(t *testing.T, src string, mw *MaintenanceWindow, soulWhen bool, active int) ([]*RenderedTask, []DispatchPlan, error) {
	t.Helper()
	m := loadStagedManifest(t, src)
	in := RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		Maintenance: mw, SoulEvaluatesWhen: soulWhen,
	}
	if !soulWhen {
		plan, steps := config.LinearPassage(m.Tasks, m.Settings)
		in.TaskPassage, in.ActivePassage, in.Steps, in.HeldTargets = plan.TaskPassage, active, steps, NewHeldTargets()
	}
	return NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
}

func runsOn(t *testing.T, tasks []*RenderedTask, plans []DispatchPlan, name string) []string {
	t.Helper()
	rt, pl := renderedByName(t, tasks, plans, name)
	var out []string
	for _, sid := range pl.TargetSIDs {
		hf := rt.FlowBySID[sid]
		if hf.Failure != nil {
			t.Fatalf("%s on %s failed: %+v", name, sid, hf.Failure)
		}
		if !hf.Skip {
			out = append(out, sid)
		}
	}
	return out
}

// Under linear dispatch the Keeper decides a step's `when:`, so the function is
// answered there, with the pass's one answer on every host.
//
// Mutation: settleHostFlow evaluating `when:` in the Soul's engine → the step fails on
// every host instead of running.
func TestMaintenance_LinearWhenIsDecidedByTheKeeper(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	closed := MaintenanceAt(mondayWindow(t), maintenanceAt)
	for _, tc := range []struct {
		name   string
		step   int
		mw     *MaintenanceWindow
		runsOn []string
	}{
		{"restart", 0, &open, []string{sidA, sidB}},
		{"restart a", 1, &open, []string{sidA}},
		{"restart", 0, &closed, nil},
		{"restart a", 1, &closed, nil},
	} {
		tasks, plans, err := maintenancePass(t, maintenanceStep, tc.mw, false, tc.step)
		if err != nil {
			t.Fatalf("Render step %d: %v", tc.step, err)
		}
		if got := runsOn(t, tasks, plans, tc.name); strings.Join(got, ",") != strings.Join(tc.runsOn, ",") {
			t.Errorf("open=%v: %s runs on %v, want %v", tc.mw.Open, tc.name, got, tc.runsOn)
		}
	}

	tasks, plans, err := maintenancePass(t, maintenanceStep, &closed, false, 2)
	if err != nil {
		t.Fatalf("Render step 2: %v", err)
	}
	note, _ := renderedByName(t, tasks, plans, "note")
	if cmd := note.Params.GetFields()["cmd"].GetStringValue(); cmd != "echo mon 02:00 for 3h (Europe/Moscow)|2026-11-08T23:00:00Z" {
		t.Errorf("note cmd = %q", cmd)
	}
}

// A post-module predicate runs after the module, later than the moment the window was
// read, so the render refuses it at the first step — a later step's included, which
// renders as a placeholder carrying no predicate.
//
// Mutation: refuseLateMaintenanceReads reading the rendered tasks instead of the plan's
// text → pass 0 renders, and the run fails at step 1 after step 0 was sent.
func TestMaintenance_PostModulePredicateRefusedBeforeTheFirstStep(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	_, _, err := maintenancePass(t, `
name: linear
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: restart
    module: core.exec.run
    failed_when: register.self.rc != 0 && in_maintenance_window()
    params: { cmd: restart }
`, &open, false, 0)
	var unsupported *cel.ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Feature != cel.MaintenanceSoulSideFeature || !strings.Contains(err.Error(), `task "restart": failed_when`) {
		t.Fatalf("want the late-read refusal naming restart's failed_when at pass 0, got %v", err)
	}

	// Inside a block too.
	_, _, err = maintenancePass(t, `
name: linear
tasks:
  - name: blk
    block:
      - name: first
        module: core.exec.run
        params: { cmd: first }
      - name: inner
        module: core.exec.run
        retry: { count: 2, until: in_maintenance_window() }
        params: { cmd: inner }
`, &open, false, 0)
	if err == nil || !strings.Contains(err.Error(), `task "inner": until`) {
		t.Fatalf("a block child's retry.until: got %v, want the refusal", err)
	}
}

// Under staged dispatch the Soul evaluates a host task's `when:`, so the render
// refuses the call before anything is sent — the static and the dynamic one alike.
//
// Mutation: refuseLateMaintenanceReads not reading `when:` under SoulEvaluatesWhen.
func TestMaintenance_StagedWhenIsRefusedAtRender(t *testing.T) {
	// A later passage's task, the case a rendered placeholder hides.
	open0 := MaintenanceAt(nil, maintenanceAt)
	m := loadStagedManifest(t, "\nname: staged\ntasks:\n  - name: probe\n    module: core.exec.run\n    register: probe\n    params: { cmd: probe }\n  - name: restart\n    module: core.exec.run\n    when: in_maintenance_window()\n    params: { cmd: \"${ register.probe.stdout }\" }\n")
	passage, perr := Stratify(m.Tasks)
	if perr != nil || passage.Count < 2 {
		t.Fatalf("stratify: %v, %d passages", perr, passage.Count)
	}
	_, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		TaskPassage: passage.TaskPassage, Maintenance: &open0, SoulEvaluatesWhen: true,
	})
	if err == nil || !strings.Contains(err.Error(), cel.MaintenanceSoulSideFeature) {
		t.Errorf("a later passage's when: at passage 0: got %v, want the refusal", err)
	}

	open := MaintenanceAt(nil, maintenanceAt)
	for _, when := range []string{"in_maintenance_window()", "in_maintenance_window() && register.probe.ok"} {
		_, _, err := maintenancePass(t, "\nname: staged\ntasks:\n  - name: probe\n    module: core.exec.run\n    register: probe\n    params: { cmd: probe }\n  - name: restart\n    module: core.exec.run\n    when: '"+when+"'\n    params: { cmd: restart }\n", &open, true, 0)
		var unsupported *cel.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Feature != cel.MaintenanceSoulSideFeature {
			t.Errorf("when %q: want the Soul-side refusal, got %v", when, err)
		}
	}
}

// A destiny is part of the step that applies it: its params read the step's answer,
// and its `when:`, which the Soul evaluates, is refused.
//
// Mutations: drop `Maintenance: parentIn.Maintenance` from the destiny pass → its params
// fail with no state; the destiny pass's refuseLateMaintenanceReads not reading `when:`.
func TestMaintenance_DestinyReadsTheStepAnswer(t *testing.T) {
	closed := MaintenanceAt(mondayWindow(t), maintenanceAt)
	d := flatDestiny()
	d.Tasks[0].Module.Params["content"] = "${ in_maintenance_window() }|${ incarnation.maintenance_window.next_start }"
	render := func(d *ResolvedDestiny) ([]*RenderedTask, error) {
		scn := applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"})
		tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
			Scenario: scn, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
			Steps: mustSteps(scn), Destiny: &stubDestinyResolver{resolved: d}, Maintenance: &closed,
		})
		return tasks, err
	}
	tasks, err := render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["content"].GetStringValue(); got != "false|2026-11-08T23:00:00Z" {
		t.Errorf("destiny content = %q", got)
	}

	d = flatDestiny()
	d.Tasks[1].When = "in_maintenance_window()"
	if _, err := render(d); err == nil || !strings.Contains(err.Error(), cel.MaintenanceSoulSideFeature) {
		t.Errorf("a destiny's when: calling the function: got %v, want the Soul-side refusal", err)
	}
}

// A pass without a window fails only the cell that reads it.
func TestMaintenance_NoStateFailsOnlyTheReader(t *testing.T) {
	if _, _, err := maintenancePass(t, "\nname: linear\ntasks:\n  - name: plain\n    module: core.exec.run\n    params: { cmd: x }\n", nil, false, 0); err != nil {
		t.Fatalf("a pass reading no window: %v", err)
	}
	_, _, err := maintenancePass(t, "\nname: linear\ntasks:\n  - name: reader\n    module: core.exec.run\n    params: { cmd: \"${ in_maintenance_window() }\" }\n", nil, false, 0)
	if err == nil || !strings.Contains(err.Error(), "no maintenance-window state") {
		t.Fatalf("a cell calling the function with no window: got %v", err)
	}
}

// ★ GUARD (plan shape across passes). A pass re-renders the steps before it, and a
// window that closed since must not re-decide what they were: here an include group
// gated on the window, steps 0 and 1, kept while open. Pass 2 renders with the window
// closed and the earlier steps' answers in MaintenanceByStep, and the plan keeps its
// numbering — the registers already collected are matched to tasks by index.
//
// Mutation: maintenanceFor answering with the pass's window for every step → the
// group is dropped at pass 2 and `after` moves from index 2 to 0.
func TestMaintenance_EarlierStepsKeepTheirWindow(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	closed := MaintenanceAt(mondayWindow(t), maintenanceAt)
	exec := func(name string) config.Task {
		return config.Task{Name: name, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": name}}}
	}
	grouped := func(name string) config.Task {
		t := exec(name)
		t.IncludeGroupID, t.IncludeWhen = 1, "in_maintenance_window()"
		return t
	}
	m := &config.ScenarioManifest{Name: "linear", Tasks: []config.Task{grouped("a"), grouped("b"), exec("after")}}
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	render := func(active int, byStep map[int]*MaintenanceWindow) []*RenderedTask {
		tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
			Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
			TaskPassage: plan.TaskPassage, ActivePassage: active, Steps: steps, HeldTargets: NewHeldTargets(),
			Maintenance: &closed, MaintenanceByStep: byStep,
		})
		if err != nil {
			t.Fatalf("Render pass %d: %v", active, err)
		}
		return tasks
	}
	tasks := render(2, map[int]*MaintenanceWindow{0: &open, 1: &open})
	if len(tasks) != 3 || tasks[2].Name != "after" || tasks[2].Index != 2 {
		t.Fatalf("pass 2 with the steps' own windows: %d tasks, last %q at %d; want the group kept and after at 2", len(tasks), tasks[len(tasks)-1].Name, tasks[len(tasks)-1].Index)
	}
	// The group's own step decided with a closed window drops it: the answer is
	// the step's, not a constant.
	if tasks := render(0, nil); len(tasks) != 1 || tasks[0].Name != "after" {
		t.Fatalf("pass 0 with a closed window: want the group dropped, got %d tasks", len(tasks))
	}
}

// compute: resolves once per pass for every step, the steps that already ran
// included, so it has no window to read.
func TestMaintenance_ComputeIsRefused(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	for _, expr := range []string{"${ in_maintenance_window() }", "${ incarnation.maintenance_window.text }"} {
		_, _, err := maintenancePass(t, "\nname: linear\ncompute:\n  open: \""+expr+"\"\ntasks:\n  - name: plain\n    module: core.exec.run\n    params: { cmd: x }\n", &open, false, 0)
		var unsupported *cel.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Feature != computeMaintenanceFeature {
			t.Errorf("compute %q: want the compute refusal, got %v", expr, err)
		}
	}

	// A whole read of `incarnation` names no field, so it is not refused, and it finds
	// no window there either.
	tasks, _, err := maintenancePass(t, "\nname: linear\ncompute:\n  has_window: \"${ 'maintenance_window' in incarnation }\"\ntasks:\n  - name: plain\n    module: core.exec.run\n    params: { cmd: \"w=${ compute.has_window }\" }\n", &open, false, 0)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if cmd := tasks[0].Params.GetFields()["cmd"].GetStringValue(); cmd != "w=false" {
		t.Errorf("compute saw a window: cmd = %q", cmd)
	}
}

// A keeper task's, an assert's and an applier's `when:` never leave the Keeper, so they
// are answered under staged dispatch too.
//
// Mutation: refuseLateMaintenanceReads reading any of their `when:`s as the Soul's.
func TestMaintenance_KeeperTaskWhenUnderStagedDispatch(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	m := &config.ScenarioManifest{Name: "staged", Tasks: []config.Task{{
		Name: "register", When: "in_maintenance_window()",
		Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{}},
	}}}
	_, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		Maintenance: &open, SoulEvaluatesWhen: true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(plans) != 1 || !plans[0].Keeper {
		t.Fatalf("plans = %+v, want one keeper plan", plans)
	}
}

// A capture's bare match: is evaluated per element at merge, after the step's render.
func TestMaintenance_CaptureMatchIsRefusedAtMerge(t *testing.T) {
	match, _ := NewPipeline(nil, newEngine(t), nil, nil).StateOpEvaluators(context.Background(), "svc")
	_, err := match("in_maintenance_window() && elem == value", "a", "a")
	var unsupported *cel.ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Feature != cel.MaintenanceSoulSideFeature {
		t.Fatalf("want the late-read refusal, got %v", err)
	}
}

// A default block's children are steps of their own, so each takes its own step's
// window: at the second child's pass the first re-renders with its answer and the
// second, the active one, with the pass's.
//
// Mutation: drop the per-child maintenanceFor in walkBlockChildren → the second child
// renders with the block's first step's window, open.
func TestMaintenance_BlockChildTakesItsOwnStepWindow(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	closed := MaintenanceAt(mondayWindow(t), maintenanceAt)
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: blk
    block:
      - name: first
        module: core.exec.run
        params: { cmd: first }
      - name: second
        module: core.exec.run
        params: { cmd: "w=${ in_maintenance_window() }" }
`)
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		TaskPassage: plan.TaskPassage, ActivePassage: 1, Steps: steps, HeldTargets: NewHeldTargets(),
		Maintenance: &closed, MaintenanceByStep: map[int]*MaintenanceWindow{0: &open},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, _ := renderedByName(t, tasks, plans, "second")
	if cmd := second.Params.GetFields()["cmd"].GetStringValue(); cmd != "w=false" {
		t.Errorf("second cmd = %q with the window closed at its step, want w=false", cmd)
	}
}

// The child's targeting is its step's too: its `where:` reads the window it renders
// with, not the block's first step's.
//
// Mutation: blockChildTargets taking renderBlockTask's in rather than childIn(pos).
func TestMaintenance_BlockChildWhereTakesItsOwnStepWindow(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	closed := MaintenanceAt(mondayWindow(t), maintenanceAt)
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: blk
    block:
      - name: first
        module: core.exec.run
        params: { cmd: first }
      - name: second
        module: core.exec.run
        where: in_maintenance_window()
        params: { cmd: second }
`)
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		TaskPassage: plan.TaskPassage, ActivePassage: 1, Steps: steps, HeldTargets: NewHeldTargets(),
		Maintenance: &closed, MaintenanceByStep: map[int]*MaintenanceWindow{0: &open},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, pl := renderedByName(t, tasks, plans, "second"); len(pl.TargetSIDs) != 0 {
		t.Errorf("second targets %v with the window closed at its step, want none", pl.TargetSIDs)
	}
}

// The up-front capability survey cannot decide what a step decides with its own
// window: a gated `when:` counts as one that may run, and a task whose targeting reads
// the window is left to its step.
//
// Mutations: drop predicateReadsMaintenanceWindow from the survey's off → gated_when
// is missing with the window closed; from targetingKnown, or the on: check → by_where
// or by_on appears with the window open.
func TestMaintenance_SurveyLeavesTheWindowToTheStep(t *testing.T) {
	m := loadStagedManifest(t, `
name: needs
tasks:
  - name: gated_when
    module: core.exec.run
    when: in_maintenance_window()
    params: { cmd: x }
  - name: by_where
    module: core.exec.run
    where: in_maintenance_window()
    params: { cmd: x }
  - name: by_on
    module: core.exec.run
    on: ["${ in_maintenance_window() ? 'db' : 'none' }"]
    params: { cmd: x }
  - name: plain
    module: core.exec.run
    params: { cmd: x }
`)
	for _, tc := range []struct {
		mw   MaintenanceWindow
		want []string
	}{
		{MaintenanceAt(mondayWindow(t), maintenanceAt), []string{"gated_when", "plain"}},
		{MaintenanceAt(nil, maintenanceAt), []string{"gated_when", "plain"}},
	} {
		in := surveyInput(m, needHosts())
		in.Maintenance = &tc.mw
		var names []string
		for name := range surveyNeeds(t, in) {
			names = append(names, name)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Errorf("open=%v: surveyed %v, want %v", tc.mw.Open, names, tc.want)
		}
	}
}

func TestMaintenance_AssertAndApplierWhenUnderStagedDispatch(t *testing.T) {
	open := MaintenanceAt(nil, maintenanceAt)
	scn := applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"})
	scn.Tasks[0].When = "in_maintenance_window()"
	scn.Tasks = append(scn.Tasks, config.Task{Name: "check", When: "in_maintenance_window()", Assert: &config.AssertSpec{That: []string{"true"}}})
	if _, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: scn, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		Destiny: &stubDestinyResolver{resolved: flatDestiny()}, Maintenance: &open, SoulEvaluatesWhen: true,
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
}
