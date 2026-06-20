//go:build integration

// Integration-тесты check-drift (ADR-031 Slice B). Использует тот же noop-service-
// репо + testcontainers PG, что и прочие scenario-тесты.

package scenario

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// noopServiceRepoWithConverge — расширение noopServiceRepo: тот же service.yml,
// scenario/create/main.yml + scenario/converge/main.yml. converge несёт одну
// core.file-задачу для маркер-файла (минимальный валидный converge для теста).
//
// extraFiles: дополнительные файлы (rel→content), записываются в репо до
// commit-а — для тестов, где converge надо ЗАМЕНИТЬ на свою форму (например
// невалидный YAML, отсутствие converge).
func noopServiceRepoWithConverge(t *testing.T, extraFiles map[string]string) string {
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
description: noop service for check-drift integration test
state_schema:
  type: object
  properties: {}
`)
	write("scenario/create/main.yml", `name: create
description: smoke
state_changes: {}
tasks:
  - name: Echo hello
    module: core.exec.run
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`)
	write("scenario/converge/main.yml", `name: converge
description: drift-converge smoke
state_changes: {}
tasks:
  - name: Ensure marker file
    module: core.file.present
    params:
      path: /var/lib/soul-stack/noop-converged
      content: "noop converged\n"
      mode: "0644"
`)
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
	if _, err := wt.Commit("init noop with converge", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// newDriftRunner собирает Runner с AcolyteEnabled — обязательное условие
// CheckDrift (inline-путь не пробрасывает DryRun, см. checkdrift.go).
func newDriftRunner(t *testing.T) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       fakeDispatcher{},
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-drift-test",
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     30 * time.Second,
	})
}

// finalizeHostsAsCheckDriftAcolyte эмулирует работу Acolyte: переводит planned-
// строки прогона в `success` с зашитым register_data per task_idx — нужно для
// сборки DriftReport (assembleDriftReport читает apply_task_register).
// changedByTaskIdx — set task_idx с changed=true; остальные → changed=false.
func finalizeHostsAsCheckDriftAcolyte(t *testing.T, applyID string, sids []string, taskCount int, changedByTaskIdx map[int]bool) {
	t.Helper()
	ctx := context.Background()
	for _, sid := range sids {
		for idx := 0; idx < taskCount; idx++ {
			data := map[string]any{
				"changed":   changedByTaskIdx[idx],
				"failed":    false,
				"timed_out": false,
				"skipped":   false,
			}
			if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
				ApplyID:      applyID,
				SID:          sid,
				TaskIdx:      idx,
				RegisterData: data,
			}); err != nil {
				t.Fatalf("UpsertTaskRegister(%s,%d): %v", sid, idx, err)
			}
		}
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, 0, applyrun.StatusSuccess, nil); err != nil {
			t.Fatalf("UpdateStatus(%s,success): %v", sid, err)
		}
	}
}

// finalizeHostsAsUnsupported — эмуляция Soul, у которого `plan.unsupported`-
// task (community-модуль без PlanReadSafe): apply_runs.status=failed +
// error_summary с маркером кода `plan.unsupported`.
func finalizeHostsAsUnsupported(t *testing.T, applyID, sid string, taskIdx int) {
	t.Helper()
	summary := fmt.Sprintf("task %d core.custom.example: plan.unsupported", taskIdx)
	// failure-таск тоже несёт apply_task_register со skipped/failed-фактами;
	// но для unsupported-классификации hand-classifier-у достаточно
	// error_summary. Эмулируем минимум: пишем failure через RecordTaskFailure.
	// N=1 unsupported-сценарий: локальный task_idx == глобальный plan_index.
	if err := applyrun.RecordTaskFailure(context.Background(), integrationPool, applyID, sid, 0, taskIdx, taskIdx, summary); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if err := applyrun.UpdateStatus(context.Background(), integrationPool, applyID, sid, 0, applyrun.StatusFailed, &summary); err != nil {
		t.Fatalf("UpdateStatus(failed): %v", err)
	}
}

// runCheckDriftInBackground запускает CheckDrift в горутине и возвращает chan с
// результатом — тест должен сначала запустить, потом эмулировать Acolyte
// (finalize), потом дождаться канала. Иначе CheckDrift зависает в barrier до
// runTimeout.
func runCheckDriftInBackground(t *testing.T, r *Runner, spec CheckDriftSpec) <-chan struct {
	report *DriftReport
	err    error
} {
	t.Helper()
	out := make(chan struct {
		report *DriftReport
		err    error
	}, 1)
	go func() {
		report, err := r.CheckDrift(context.Background(), spec)
		out <- struct {
			report *DriftReport
			err    error
		}{report, err}
	}()
	// дать CheckDrift время сделать render+InsertPlanned (~200ms — render+PG-IO)
	time.Sleep(200 * time.Millisecond)
	return out
}

func waitDriftResult(t *testing.T, ch <-chan struct {
	report *DriftReport
	err    error
}) (*DriftReport, error) {
	t.Helper()
	select {
	case r := <-ch:
		return r.report, r.err
	case <-time.After(15 * time.Second):
		t.Fatal("CheckDrift не вернулся за 15s")
		return nil, nil
	}
}

// TestIntegration_CheckDrift_HappyPath_Clean — два хоста, оба apply_runs success
// с register changed=false → DriftReport clean.
func TestIntegration_CheckDrift_HappyPath_Clean(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepoWithConverge(t, nil)

	r := newDriftRunner(t)
	applyID := audit.NewULID()
	ch := runCheckDriftInBackground(t, r, CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	finalizeHostsAsCheckDriftAcolyte(t, applyID,
		[]string{"host-a.example.com", "host-b.example.com"},
		1, // converge несёт 1 task
		map[int]bool{ /* all false */ })

	report, err := waitDriftResult(t, ch)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if report.Summary.HostsClean != 2 || report.Summary.HostsDrifted != 0 {
		t.Errorf("summary clean/drifted = %d/%d, want 2/0", report.Summary.HostsClean, report.Summary.HostsDrifted)
	}
	if len(report.Hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(report.Hosts))
	}
	for _, h := range report.Hosts {
		if h.Status != DriftStatusClean {
			t.Errorf("host %s status = %s, want clean", h.SID, h.Status)
		}
		if len(h.Tasks) != 1 || h.Tasks[0].Changed {
			t.Errorf("host %s tasks = %+v, want one clean task", h.SID, h.Tasks)
		}
	}

	// incarnation.status НЕ становится drift (нет расхождений), и MarkDriftStatus
	// явно дёрнут API-handler-ом — здесь проверяем сам поток. Просто убеждаемся
	// что строка осталась в ready.
	inc, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if inc.Status != incarnation.StatusReady {
		t.Errorf("incarnation.status = %s, want ready", inc.Status)
	}
}

// TestIntegration_CheckDrift_Drifted — host-a register changed=true → drifted.
func TestIntegration_CheckDrift_Drifted(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepoWithConverge(t, nil)

	r := newDriftRunner(t)
	applyID := audit.NewULID()
	ch := runCheckDriftInBackground(t, r, CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	// host-a — drift на task 0; host-b — clean
	finalizeHostsAsCheckDriftAcolyte(t, applyID,
		[]string{"host-a.example.com"}, 1, map[int]bool{0: true})
	finalizeHostsAsCheckDriftAcolyte(t, applyID,
		[]string{"host-b.example.com"}, 1, map[int]bool{})

	report, err := waitDriftResult(t, ch)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if report.Summary.HostsDrifted != 1 || report.Summary.HostsClean != 1 {
		t.Errorf("summary drifted/clean = %d/%d, want 1/1",
			report.Summary.HostsDrifted, report.Summary.HostsClean)
	}

	driftedSID := ""
	for _, h := range report.Hosts {
		if h.Status == DriftStatusDrifted {
			driftedSID = h.SID
		}
	}
	if driftedSID != "host-a.example.com" {
		t.Errorf("drifted host = %q, want host-a.example.com", driftedSID)
	}
}

// TestIntegration_CheckDrift_Unsupported — Soul возвращает FAILED с
// plan.unsupported → DriftStatusUnsupported (не failed).
func TestIntegration_CheckDrift_Unsupported(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepoWithConverge(t, nil)

	r := newDriftRunner(t)
	applyID := audit.NewULID()
	ch := runCheckDriftInBackground(t, r, CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	finalizeHostsAsUnsupported(t, applyID, "host-a.example.com", 0)

	report, err := waitDriftResult(t, ch)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if report.Summary.HostsUnsupported != 1 {
		t.Errorf("hosts_unsupported = %d, want 1", report.Summary.HostsUnsupported)
	}
	if report.Hosts[0].Status != DriftStatusUnsupported {
		t.Errorf("status = %s, want unsupported", report.Hosts[0].Status)
	}
}

// TestIntegration_CheckDrift_ConvergeMissing — сервис без scenario/converge/
// → ErrConvergeMissing.
func TestIntegration_CheckDrift_ConvergeMissing(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t) // БЕЗ converge

	r := newDriftRunner(t)
	_, err := r.CheckDrift(context.Background(), CheckDriftSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	if !errors.Is(err, ErrConvergeMissing) {
		t.Fatalf("CheckDrift: %v, want ErrConvergeMissing", err)
	}
}

// TestIntegration_CheckDrift_AcolyteRequired — runner без AcolyteEnabled
// отказывается делать check-drift (inline-путь не пробрасывает dry_run).
func TestIntegration_CheckDrift_AcolyteRequired(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepoWithConverge(t, nil)

	r := newRunner(t, mockSuccessDispatcher(t), gitURL) // AcolyteEnabled=false
	_, err := r.CheckDrift(context.Background(), CheckDriftSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("CheckDrift на runner без AcolyteEnabled прошёл, want отказ")
	}
}

// mockSuccessDispatcher — простой stub-Outbound для CheckDrift_AcolyteRequired:
// сам тест не должен дойти до SendApply (отказ ранее), достаточно non-nil.
func mockSuccessDispatcher(t *testing.T) ApplyDispatcher {
	t.Helper()
	return fakeDispatcher{}
}
