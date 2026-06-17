//go:build integration

// Integration-тесты teardown-режима прогона (S-D2b): scenario `destroy` через
// TerminalMode=TerminalDestroy. Проверяют ветвление финала run():
//
//   - success teardown → incarnation остаётся в `destroying` (НЕ ready), state
//     не тронут, ready НЕ коммитится (физический снос строки — S-D3);
//   - провал teardown (хост упал, barrier fail-closed) → `destroy_failed`
//     (НЕ error_locked), status_details несёт причину;
//   - lockRun-gate teardown: стартует только из `destroying`.
//
// Инфраструктура (testcontainers PG, mock Outbound, local-fs git) общая с
// integration_test.go.

package scenario

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"os"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// seedDestroyingIncarnation создаёт incarnation сразу в статусе `destroying`
// (как после S-D1 Destroy) с непустым state — чтобы тест проверил, что teardown
// НЕ трогает state-граф. Coven-метка = name (roster по incarnation.name).
func seedDestroyingIncarnation(t *testing.T, name string, state map[string]any) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name: name, Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusDestroying,
		State: state,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedDestroyingIncarnation: %v", err)
	}
}

// destroyServiceRepo создаёт local-fs git-репо service-noop со scenario
// `destroy` (один core.exec.run-шаг teardown-а). file://-URL для loader-а.
func destroyServiceRepo(t *testing.T) string {
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
description: noop service with destroy teardown
state_schema:
  type: object
  properties: {}
`)
	write("scenario/destroy/main.yml", `name: destroy
description: teardown step
state_changes: {}
tasks:
  - name: Tear down on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["teardown"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init destroy service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// waitStatusInc поллит статус incarnation до достижения want (или таймаута) и
// возвращает строку для последующих assert-ов (state / status_details). В
// отличие от waitStatus (gap4_test.go) возвращает incarnation; в отличие от
// waitRunDone не требует state_history-snapshot-а — teardown-success в S-D2b НЕ
// пишет snapshot (state не коммитится), статус остаётся destroying.
func waitStatusInc(t *testing.T, name string, want incarnation.Status) *incarnation.Incarnation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inc, err := incarnation.SelectByName(context.Background(), integrationPool, name)
		if err != nil {
			t.Fatalf("SelectByName: %v", err)
		}
		if inc.Status == want {
			return inc
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("incarnation %s не достиг статуса %q за 10s", name, want)
	return nil
}

// TestIntegration_Destroy_Teardown_Success — teardown прошёл на всех хостах →
// S-D3 физический снос: после успешного barrier-а DeleteAfterTeardown архивирует
// и DELETE-ит строку incarnation одной tx; каскад V3 сносит live state_history /
// apply_runs / register. Поэтому наблюдаемый терминал успеха — ОТСУТСТВИЕ строки
// incarnation (а не «остаётся в destroying» — то была пред-S-D3 семантика).
func TestIntegration_Destroy_Teardown_Success(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedDestroyingIncarnation(t, "noop-prod", map[string]any{"leader": "host-a"})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := destroyServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("StartDestroy: %v", err)
	}

	// Терминал успеха teardown — строка incarnation физически снесена (S-D3).
	waitIncarnationGone(t, "noop-prod")

	// dispatch состоялся ровно один раз (один хост teardown).
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (один хост teardown)", disp.calls)
	}

	// Каскад V3: apply_runs прогона снесены вместе со строкой incarnation.
	st, serr := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if serr != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", serr)
	}
	if len(st) != 0 {
		t.Errorf("apply_runs прогона = %d, want 0 (каскад V3 снёс вместе с incarnation)", len(st))
	}
}

// waitIncarnationGone поллит, пока строка incarnation физически не исчезнет
// (S-D3 success: DeleteAfterTeardown в run-goroutine после barrier-а). До
// удаления SelectByName отдаёт строку; после — ErrIncarnationNotFound.
func waitIncarnationGone(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, err := incarnation.SelectByName(context.Background(), integrationPool, name)
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("SelectByName: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("incarnation %s не снесена за 10s (teardown-success должен удалить строку)", name)
}

// TestIntegration_Destroy_Teardown_Fail — хост упал (RunResult failed) →
// barrier fail-closed → incarnation в `destroy_failed` (НЕ error_locked),
// status_details несёт причину. state не тронут.
func TestIntegration_Destroy_Teardown_Fail(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedDestroyingIncarnation(t, "noop-prod", map[string]any{"leader": "host-a"})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := destroyServiceRepo(t)

	summary := "teardown step failed"
	disp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("StartDestroy: %v", err)
	}

	// Провал teardown переводит в destroy_failed (НЕ error_locked).
	inc := waitStatusInc(t, "noop-prod", incarnation.StatusDestroyFailed)
	if inc.Status == incarnation.StatusErrorLocked {
		t.Fatal("status = error_locked — провал teardown должен давать destroy_failed, НЕ error_locked")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details = %+v, want reason=dispatch_failed", inc.StatusDetails)
	}
	// state не тронут при фейле (last known-good).
	if inc.State["leader"] != "host-a" {
		t.Errorf("state.leader = %v, want host-a (state не меняется при фейле)", inc.State["leader"])
	}

	// Провал пишет state_history-snapshot (zero-diff, фиксирует факт прогона).
	hist, total, herr := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if herr != nil {
		t.Fatalf("HistorySelectByName: %v", herr)
	}
	if total != 1 {
		t.Fatalf("state_history snapshots = %d, want 1 (фейл фиксирует факт прогона)", total)
	}
	if hist[0].Scenario != DestroyScenarioName {
		t.Errorf("history scenario = %q, want %q", hist[0].Scenario, DestroyScenarioName)
	}
}

// TestIntegration_Destroy_Teardown_RejectsNonDestroying — teardown стартует
// строго из `destroying`: incarnation в ready → StartDestroy отвергается
// lockRun-gate-ом (ErrNotRunnable), статус не меняется, dispatch не происходит.
func TestIntegration_Destroy_Teardown_RejectsNonDestroying(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod") // ready, не destroying
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := destroyServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	if err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("StartDestroy: %v", err)
	}

	// Прогон отклоняется внутри run-goroutine (lockRun → ErrNotRunnable);
	// статус остаётся ready, dispatch не происходит.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disp.calls > 0 {
			t.Fatalf("SendApply вызван для teardown из ready-статуса")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if got.Status != incarnation.StatusReady {
		t.Errorf("status = %q, want ready (teardown из ready отвергнут)", got.Status)
	}
}
