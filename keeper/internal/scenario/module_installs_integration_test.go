//go:build integration

// Integration-guard синтеза install-шагов core.module.installed из
// service.yml::modules[] (ADR-065, NIM-8): прогон с modules[] несёт в
// ApplyRequest синтез-RenderedTask с params {name, ref} ПЕРЕД потребителем;
// явный шаг оператора (takeover) дубля не даёт; drift-план симметричен.

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

// moduleServiceRepo — local-fs git-репо сервиса с modules[] в манифесте:
// service.yml (community.echo v1.2.0) + scenario/create/main.yml из mainTasks
// + опц. converge.
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

// taskCaptureDispatcher — mockDispatcher, дополнительно снимающий полный
// список RenderedTask отправленного ApplyRequest.
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

// installIndexes — индексы задач core.module.installed в плане.
func installIndexes(tasks []*keeperv1.RenderedTask) []int {
	var out []int
	for i, rt := range tasks {
		if rt.GetModule() == "core.module.installed" {
			out = append(out, i)
		}
	}
	return out
}

// TestIntegration_ModuleInstallSynthesis — прогон сценария БЕЗ явного
// install-шага: ApplyRequest несёт синтез-RenderedTask core.module.installed с
// params {name: community.echo, ref: v1.2.0} ПЕРЕД потребителем.
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
		t.Fatalf("dispatched tasks = %d, want 2 (синтез-install + потребитель)", len(tasks))
	}
	install := tasks[0]
	if install.GetModule() != "core.module.installed" {
		t.Fatalf("task[0].module = %q, want core.module.installed (синтез ПЕРЕД потребителем)", install.GetModule())
	}
	fields := install.GetParams().GetFields()
	if got := fields["name"].GetStringValue(); got != "community.echo" {
		t.Errorf("install params.name = %q, want community.echo", got)
	}
	if got := fields["ref"].GetStringValue(); got != "v1.2.0" {
		t.Errorf("install params.ref = %q, want v1.2.0 (ref записи modules[])", got)
	}
	if tasks[1].GetModule() != "community.echo.run" {
		t.Errorf("task[1].module = %q, want community.echo.run", tasks[1].GetModule())
	}
}

// TestIntegration_ModuleInstallTakeover_NoDuplicate — явный install-шаг с
// литеральным params.name подавляет синтез: в плане РОВНО ОДИН
// core.module.installed (операторский, без ref).
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
		t.Fatalf("dispatched tasks = %d, want 2 (takeover: без синтез-дубля)", len(tasks))
	}
	idx := installIndexes(tasks)
	if len(idx) != 1 || idx[0] != 0 {
		t.Fatalf("core.module.installed indexes = %v, want [0] (ровно один — операторский)", idx)
	}
	if _, hasRef := tasks[0].GetParams().GetFields()["ref"]; hasRef {
		t.Errorf("takeover-шаг несёт ref — в плане синтез-шаг вместо операторского")
	}
}

// TestIntegration_RenderForHost_SynthesisParity — guard claim-пути (ADR-027):
// RenderForHost по persisted-рецепту обязан воспроизвести РОВНО план
// run-goroutine, включая синтез-install из modules[] (ADR-065). Без синтеза в
// RenderForHost Acolyte-хост молча не получил бы install-шаг, а корреляция
// TaskEvent↔RenderedTask (plan_index) съехала бы.
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

	// Эталон — dispatched-план run-пути (тот же service-снапшот).
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
		t.Fatalf("run-путь dispatched tasks = %d, want 2", len(dispatched))
	}

	// Claim-путь: воспроизведение плана Acolyte-ом по рецепту.
	tasks, _, err := RenderForHost(context.Background(), r.deps, &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	}, "noop-prod", audit.NewULID(), "host-a.example.com")
	if err != nil {
		t.Fatalf("RenderForHost: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("RenderForHost tasks = %d, want 2 (синтез-install + потребитель)", len(tasks))
	}
	if tasks[0].Module != "core.module.installed" {
		t.Fatalf("claim task[0].module = %q, want core.module.installed (синтез на claim-пути)", tasks[0].Module)
	}
	fields := tasks[0].Params.GetFields()
	if got := fields["name"].GetStringValue(); got != "community.echo" {
		t.Errorf("claim install params.name = %q, want community.echo", got)
	}
	if got := fields["ref"].GetStringValue(); got != "v1.2.0" {
		t.Errorf("claim install params.ref = %q, want v1.2.0", got)
	}

	// Parity: последовательность модулей claim-плана ≡ dispatched-плану run-пути.
	for i := range tasks {
		if tasks[i].Module != dispatched[i].GetModule() {
			t.Errorf("plan parity: task[%d] claim=%q run=%q — планы Acolyte↔run разошлись", i, tasks[i].Module, dispatched[i].GetModule())
		}
	}
}

// TestIntegration_CheckDrift_SynthesizedInstallInPlan — drift-план симметричен
// apply-плану: converge с потребителем community.echo → DriftReport несёт
// task с module core.module.installed (синтез из modules[]).
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
	// drift-план = синтез-install (plan_index 0) + converge-потребитель (1).
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
		t.Fatalf("drift tasks = %d, want 2 (синтез-install + потребитель): %+v", len(tasks), tasks)
	}
	if tasks[0].Module != "core.module.installed" {
		t.Errorf("drift task[0].module = %q, want core.module.installed (синтез в drift-плане)", tasks[0].Module)
	}
	if tasks[1].Module != "community.echo.run" {
		t.Errorf("drift task[1].module = %q, want community.echo.run", tasks[1].Module)
	}
}

// finalizeDriftHostWithPlanIndexes — эмуляция Acolyte для drift-прогона из
// taskCount задач: register на КАЖДЫЙ plan_index (N=1 passage: локальный
// task_idx == глобальному) + терминал success.
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
