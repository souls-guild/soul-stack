//go:build integration

// Integration guard for synthesizing core.module.installed install steps
// from service.yml::modules[] (ADR-065, NIM-8): a run with modules[] carries
// a synthesized RenderedTask with params {name, ref} in the ApplyRequest
// BEFORE the consumer; an explicit operator step (takeover) suppresses the
// duplicate; the drift plan is symmetric.

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// moduleServiceRepo is a local-fs git repo of a service with modules[] in the
// manifest: service.yml (community.echo v1.2.0) + scenario/create/main.yml
// from mainTasks + optional converge.
func moduleServiceRepo(t *testing.T, mainTasks string, extraFiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	write("service.yml", `name: noop
state_schema_version: 1
description: service with modules[] for install-synthesis integration test
state_schema:
  type: object
  properties: {}
modules:
  - { name: community.echo, ref: v1.2.0 }
`)
	write("scenario/create/main.yml", mainTasks)
	for rel, content := range extraFiles {
		write(rel, content)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init modules service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// taskCaptureDispatcher is a mockDispatcher that additionally captures the
// full list of RenderedTasks from the sent ApplyRequest.
type taskCaptureDispatcher struct {
	t      *testing.T
	mu     sync.Mutex
	result applyrun.Status
	tasks  []*keeperv1.RenderedTask
}

func (d *taskCaptureDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	d.tasks = req.GetTasks()
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, d.result, nil); err != nil {
		d.t.Errorf("taskCaptureDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *taskCaptureDispatcher) captured() []*keeperv1.RenderedTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tasks
}

// installIndexes returns the indexes of core.module.installed tasks in the plan.
func installIndexes(tasks []*keeperv1.RenderedTask) []int {
	var out []int
	for i, rt := range tasks {
		if rt.GetModule() == "core.module.installed" {
			out = append(out, i)
		}
	}
	return out
}

// TestIntegration_ModuleInstallSynthesis runs a scenario WITHOUT an explicit
// install step: the ApplyRequest carries a synthesized
// core.module.installed RenderedTask with params
// {name: community.echo, ref: v1.2.0} BEFORE the consumer.
func TestIntegration_ModuleInstallSynthesis(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := moduleServiceRepo(t, `name: create
state_changes: {}
tasks:
  - name: Use the echo plugin
    module: community.echo.run
    params:
      message: hello
`, nil)

	disp := &taskCaptureDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	tasks := disp.captured()
	if len(tasks) != 2 {
		t.Fatalf("dispatched tasks = %d, want 2 (synth-install + consumer)", len(tasks))
	}
	install := tasks[0]
	if install.GetModule() != "core.module.installed" {
		t.Fatalf("task[0].module = %q, want core.module.installed (synth BEFORE the consumer)", install.GetModule())
	}
	fields := install.GetParams().GetFields()
	if got := fields["name"].GetStringValue(); got != "community.echo" {
		t.Errorf("install params.name = %q, want community.echo", got)
	}
	if got := fields["ref"].GetStringValue(); got != "v1.2.0" {
		t.Errorf("install params.ref = %q, want v1.2.0 (ref of the modules[] entry)", got)
	}
	if tasks[1].GetModule() != "community.echo.run" {
		t.Errorf("task[1].module = %q, want community.echo.run", tasks[1].GetModule())
	}
}

// TestIntegration_ModuleInstallTakeover_NoDuplicate: an explicit install step
// with a literal params.name suppresses synthesis — the plan has EXACTLY ONE
// core.module.installed (the operator's, without ref).
func TestIntegration_ModuleInstallTakeover_NoDuplicate(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := moduleServiceRepo(t, `name: create
state_changes: {}
tasks:
  - name: Operator installs the plugin explicitly
    module: core.module.installed
    params:
      name: community.echo
  - name: Use the echo plugin
    module: community.echo.run
    params:
      message: hello
`, nil)

	disp := &taskCaptureDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	tasks := disp.captured()
	if len(tasks) != 2 {
		t.Fatalf("dispatched tasks = %d, want 2 (takeover: no synth duplicate)", len(tasks))
	}
	idx := installIndexes(tasks)
	if len(idx) != 1 || idx[0] != 0 {
		t.Fatalf("core.module.installed indexes = %v, want [0] (exactly one - the operator's)", idx)
	}
	if _, hasRef := tasks[0].GetParams().GetFields()["ref"]; hasRef {
		t.Errorf("takeover step carries ref - the plan has a synth step instead of the operator's")
	}
}

// TestIntegration_RenderForHost_SynthesisParity guards the claim path
// (ADR-027): RenderForHost from a persisted recipe must reproduce EXACTLY
// the run-goroutine's plan, including install synthesis from modules[]
// (ADR-065). Without synthesis in RenderForHost, an Acolyte host would
// silently miss the install step, and the TaskEvent↔RenderedTask
// (plan_index) correlation would drift.
func TestIntegration_RenderForHost_SynthesisParity(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := moduleServiceRepo(t, `name: create
state_changes: {}
tasks:
  - name: Use the echo plugin
    module: community.echo.run
    params:
      message: hello
`, nil)

	// Reference: the dispatched plan of the run path (same service snapshot).
	disp := &taskCaptureDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	dispatched := disp.captured()
	if len(dispatched) != 2 {
		t.Fatalf("run path dispatched tasks = %d, want 2", len(dispatched))
	}

	// Claim path: the Acolyte reproduces the plan from the recipe.
	tasks, _, err := RenderForHost(context.Background(), r.deps, &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	}, "noop-prod", audit.NewULID(), "host-a.example.com")
	if err != nil {
		t.Fatalf("RenderForHost: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("RenderForHost tasks = %d, want 2 (synth-install + consumer)", len(tasks))
	}
	if tasks[0].Module != "core.module.installed" {
		t.Fatalf("claim task[0].module = %q, want core.module.installed (synth on the claim path)", tasks[0].Module)
	}
	fields := tasks[0].Params.GetFields()
	if got := fields["name"].GetStringValue(); got != "community.echo" {
		t.Errorf("claim install params.name = %q, want community.echo", got)
	}
	if got := fields["ref"].GetStringValue(); got != "v1.2.0" {
		t.Errorf("claim install params.ref = %q, want v1.2.0", got)
	}

	// Parity: the claim plan's module sequence ≡ the run path's dispatched plan.
	for i := range tasks {
		if tasks[i].Module != dispatched[i].GetModule() {
			t.Errorf("plan parity: task[%d] claim=%q run=%q - Acolyte<->run plans diverged", i, tasks[i].Module, dispatched[i].GetModule())
		}
	}
}

// TestIntegration_CheckDrift_SynthesizedInstallInPlan: the drift plan is
// symmetric with the apply plan — converge with the community.echo consumer
// → DriftReport carries a task with module core.module.installed (synthesized
// from modules[]).
func TestIntegration_CheckDrift_SynthesizedInstallInPlan(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := moduleServiceRepo(t, `name: create
state_changes: {}
tasks:
  - name: Use the echo plugin
    module: community.echo.run
    params:
      message: hello
`, map[string]string{
		"scenario/converge/main.yml": `name: converge
state_changes: {}
tasks:
  - name: Converge via the echo plugin
    module: community.echo.run
    params:
      message: hello
`,
	})

	r := newDriftRunner(t)
	applyID := audit.NewULID()
	ch := runCheckDriftInBackground(t, r, CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	// drift plan = synthesized install (plan_index 0) + converge consumer (1).
	finalizeDriftHostWithPlanIndexes(t, applyID, "host-a.example.com", 2)

	report, err := waitDriftResult(t, ch)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(report.Hosts) != 1 {
		t.Fatalf("hosts = %d, want 1", len(report.Hosts))
	}
	tasks := report.Hosts[0].Tasks
	if len(tasks) != 2 {
		t.Fatalf("drift tasks = %d, want 2 (synth-install + consumer): %+v", len(tasks), tasks)
	}
	if tasks[0].Module != "core.module.installed" {
		t.Errorf("drift task[0].module = %q, want core.module.installed (synth in the drift plan)", tasks[0].Module)
	}
	if tasks[1].Module != "community.echo.run" {
		t.Errorf("drift task[1].module = %q, want community.echo.run", tasks[1].Module)
	}
}

// finalizeDriftHostWithPlanIndexes emulates the Acolyte for a drift run of
// taskCount tasks: register on EVERY plan_index (N=1 passage: local
// task_idx == global) + a success terminal.
func finalizeDriftHostWithPlanIndexes(t *testing.T, applyID, sid string, taskCount int) {
	t.Helper()
	ctx := context.Background()
	for idx := 0; idx < taskCount; idx++ {
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID:   applyID,
			SID:       sid,
			PlanIndex: idx,
			TaskIdx:   idx,
			RegisterData: map[string]any{
				"changed": false, "failed": false, "timed_out": false, "skipped": false,
			},
		}); err != nil {
			t.Fatalf("UpsertTaskRegister(%d): %v", idx, err)
		}
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, 0, applyrun.StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus(success): %v", err)
	}
}

// TestIntegration_RenderForHost_FromUpgradeLoadsUpgradeDir GUARDS the render
// path (ADR-0068): with recipe.FromUpgrade=true, the Acolyte renders
// upgrade/<slug>/main.yml, NOT scenario/<slug>/. Both dirs carry a scenario
// with the same name but different consumer modules → the plan's source is
// unambiguous. Catches a regression in render_host.go (threading
// recipe.FromUpgrade through parseScenarioFromArtifact).
func TestIntegration_RenderForHost_FromUpgradeLoadsUpgradeDir(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := moduleServiceRepo(t, `name: create
state_changes: {}
tasks:
  - name: noop create
    module: core.exec.run
    params:
      cmd: "true"
`, map[string]string{
		"scenario/to_v2/main.yml": `name: to_v2
state_changes: {}
tasks:
  - name: from scenario dir
    module: core.file.absent
    params:
      path: /tmp/from-scenario
`,
		"upgrade/to_v2/main.yml": `name: to_v2
from: ["v1"]
state_changes: {}
tasks:
  - name: from upgrade dir
    module: core.file.present
    params:
      path: /tmp/from-upgrade
      content: x
`,
	})

	disp := &taskCaptureDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	tasks, _, err := RenderForHost(context.Background(), r.deps, &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "to_v2",
		FromUpgrade:  true,
	}, "noop-prod", audit.NewULID(), "host-a.example.com")
	if err != nil {
		t.Fatalf("RenderForHost FromUpgrade: %v", err)
	}

	var sawUpgradeTask bool
	for _, rt := range tasks {
		switch rt.Module {
		case "core.file.present":
			sawUpgradeTask = true
		case "core.file.absent":
			t.Errorf("RenderForHost read scenario/to_v2 (core.file.absent) instead of upgrade/to_v2 - recipe.FromUpgrade was not honored")
		}
	}
	if !sawUpgradeTask {
		t.Fatalf("RenderForHost FromUpgrade=true did not render the upgrade/to_v2 task (core.file.present); got %d tasks", len(tasks))
	}
}
