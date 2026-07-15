//go:build integration

// Integration tests for GAP#4: closing scenario-run + unlock coverage gaps
// found in QA. Complements integration_test.go (same rig: testcontainers
// PG, local-fs git service-noop, mock Outbound):
//
//  1. scenario-run against NON-EMPTY state — merge, not overwrite (real
//     DB round-trip of incarnation.state).
//  2. Full lifecycle in one test: create → error_locked → run (lock-gate
//     rejection) → unlock → run again → success.
//  3. Concurrency lock-gate: parallel runs against ONE incarnation —
//     exactly one starts apply, the rest are rejected (lockRun under FOR UPDATE).

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// seedIncarnationWithState creates a ready incarnation with the given initial
// state (seedIncarnation in integration_test.go starts from empty state —
// merge-into-existing isn't covered by it).
func seedIncarnationWithState(t *testing.T, name string, state map[string]any) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name: name, Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusReady,
		State: state,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnationWithState: %v", err)
	}
}

// setBServiceRepo — service-noop whose `bump` scenario carries state_changes.sets
// setting ONLY field b (a static literal). Other state fields aren't
// mentioned — merge must preserve them.
func setBServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepoScenario(t, "bump", `name: bump
description: set only field b
state_changes:
  sets:
    b: "20"
tasks:
  - name: No-op step
    module: core.exec.run
    params:
      cmd: echo
      args: ["bump"]
    changed_when: "false"
`)
}

// writeServiceRepoScenario — a constructor for a local-fs git repo service-noop with
// scenario/<name>/main.yml = scenarioMain (a generalization of writeServiceRepo, which
// hard-codes scenario/create/). Factored into this file to avoid touching
// integration_test.go.
func writeServiceRepoScenario(t *testing.T, scenarioName, scenarioMain string) string {
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
description: noop service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/"+scenarioName+"/main.yml", scenarioMain)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_RunMergesIntoExistingState — GAP#4 #1: incarnation with
// state {a:1, b:2}; scenario bump sets ONLY b. After the run (real
// DB round-trip): a is preserved (merge didn't overwrite the whole state), b is updated to 20.
func TestIntegration_RunMergesIntoExistingState(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithState(t, "noop-prod", map[string]any{"a": 1, "b": 2})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := setBServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "bump",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// a isn't mentioned in sets → must be preserved (JSON round-trip from JSONB → float64).
	if inc.State["a"] != float64(1) {
		t.Errorf("state.a = %v (%T), want 1 (merge должен сохранить непереписанные поля)", inc.State["a"], inc.State["a"])
	}
	// b is mentioned in sets → overwritten with the literal "20".
	if inc.State["b"] != "20" {
		t.Errorf("state.b = %v (%T), want \"20\" (sets обновил поле)", inc.State["b"], inc.State["b"])
	}

	// Real DB round-trip: read state again directly from the DB to
	// confirm the merge is committed, not just visible in the in-memory snapshot.
	fromDB, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if fromDB.State["a"] != float64(1) {
		t.Errorf("DB state.a = %v, want 1", fromDB.State["a"])
	}
	if fromDB.State["b"] != "20" {
		t.Errorf("DB state.b = %v, want \"20\"", fromDB.State["b"])
	}

	// state_history snapshot: state_before carries the original b=2, state_after — b="20".
	hist, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 || len(hist) != 1 {
		t.Fatalf("history entries = %d, want 1", total)
	}
	if hist[0].StateBefore["b"] != float64(2) {
		t.Errorf("history state_before.b = %v, want 2", hist[0].StateBefore["b"])
	}
	if hist[0].StateAfter["a"] != float64(1) {
		t.Errorf("history state_after.a = %v, want 1 (a сохранён в snapshot)", hist[0].StateAfter["a"])
	}
	if hist[0].StateAfter["b"] != "20" {
		t.Errorf("history state_after.b = %v, want \"20\"", hist[0].StateAfter["b"])
	}
}

// TestIntegration_Lifecycle_LockUnlockRerun — GAP#4 #2: a full lifecycle
// in one test.
//
//	create (ready, state {seeded:true})
//	  → run #1 fails on the host → error_locked (state NOT touched)
//	  → run #2 against error_locked → lock-gate rejects it (ErrLocked,
//	    dispatch doesn't happen, status stays error_locked) — this is the backend's 409
//	  → Unlock(reason) → ready, status_details cleared, state preserved
//	  → run #3 → goes through to success.
func TestIntegration_Lifecycle_LockUnlockRerun(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithState(t, "noop-prod", map[string]any{"seeded": true})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ref := artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"}

	// --- run #1: fail → error_locked --------------------------------
	summary := "module failed"
	failDisp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	rFail := newRunner(t, failDisp, gitURL)

	apply1 := audit.NewULID()
	if err := rFail.Start(context.Background(), RunSpec{
		ApplyID: apply1, IncarnationName: "noop-prod", ServiceRef: ref,
		ScenarioName: "create", StartedByAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Start #1: %v", err)
	}
	locked := waitRunDone(t, "noop-prod", apply1, incarnation.StatusErrorLocked)
	if locked.StatusDetails == nil || locked.StatusDetails["reason"] != "dispatch_failed" {
		t.Fatalf("после fail: status_details = %v, want reason dispatch_failed", locked.StatusDetails)
	}
	if locked.State["seeded"] != true {
		t.Errorf("state.seeded = %v, want true (fail не трогает state)", locked.State["seeded"])
	}

	// --- run #2: against error_locked → lock-gate rejects it (backend 409) ---
	gateDisp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	rGate := newRunner(t, gateDisp, gitURL)
	apply2 := audit.NewULID()
	if err := rGate.Start(context.Background(), RunSpec{
		ApplyID: apply2, IncarnationName: "noop-prod", ServiceRef: ref,
		ScenarioName: "create", StartedByAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Start #2: %v", err)
	}
	// lockRun → ErrLocked inside the goroutine: dispatch doesn't happen, status
	// stays error_locked. This is the same check under FOR UPDATE that the handler
	// surfaces as a 409 incarnation-locked.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gateDisp.calls > 0 {
			t.Fatalf("прогон #2: SendApply вызван при error_locked-incarnation (lock-gate не сработал)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stillLocked, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName после #2: %v", err)
	}
	if stillLocked.Status != incarnation.StatusErrorLocked {
		t.Errorf("после отбитого #2: status = %q, want error_locked", stillLocked.Status)
	}

	// --- unlock with reason → ready --------------------------------------
	unlockRes, err := incarnation.Unlock(context.Background(), integrationPool,
		"noop-prod", "triaged: host fixed manually", "archon-alice", audit.NewULID())
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if unlockRes.PreviousStatus != incarnation.StatusErrorLocked {
		t.Errorf("unlock previous_status = %q, want error_locked", unlockRes.PreviousStatus)
	}
	unlocked, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName после unlock: %v", err)
	}
	if unlocked.Status != incarnation.StatusReady {
		t.Fatalf("после unlock: status = %q, want ready", unlocked.Status)
	}
	if unlocked.StatusDetails != nil {
		t.Errorf("после unlock: status_details = %v, want nil (сброшены)", unlocked.StatusDetails)
	}
	if unlocked.State["seeded"] != true {
		t.Errorf("после unlock: state.seeded = %v, want true (unlock не трогает state)", unlocked.State["seeded"])
	}

	// --- run #3: ready again → goes through to success ------------------
	okDisp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	rOK := newRunner(t, okDisp, gitURL)
	apply3 := audit.NewULID()
	if err := rOK.Start(context.Background(), RunSpec{
		ApplyID: apply3, IncarnationName: "noop-prod", ServiceRef: ref,
		ScenarioName: "create", StartedByAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Start #3: %v", err)
	}
	final := waitRunDone(t, "noop-prod", apply3, incarnation.StatusReady)
	if final.StatusDetails != nil {
		t.Errorf("после #3: status_details = %v, want nil on success", final.StatusDetails)
	}
	if okDisp.calls != 1 {
		t.Errorf("прогон #3: SendApply calls = %d, want 1 (после unlock прогон проходит)", okDisp.calls)
	}
	if final.State["seeded"] != true {
		t.Errorf("после #3: state.seeded = %v, want true", final.State["seeded"])
	}
}

// blockingDispatcher simulates Soul holding an apply open until the release
// signal (so the winning run keeps the incarnation in applying while
// competitors try to start). On SendApply it counts calls, waits for release,
// then writes a terminal success status.
type blockingDispatcher struct {
	t       *testing.T
	calls   atomic.Int32
	release chan struct{}
}

func (d *blockingDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.calls.Add(1)
	select {
	case <-d.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("blockingDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

// TestIntegration_Concurrency_LockGate_RunVsRun — GAP#4 #3: N parallel
// runs (different apply_id) against ONE incarnation. lockRun under FOR UPDATE
// serializes run-vs-run: EXACTLY ONE moves the incarnation to applying and reaches
// dispatch (SendApply), the rest see applying on selectForUpdate →
// ErrAlreadyRunning and are rejected without dispatch.
//
// Run under go test -race. The winner is held in applying via
// blockingDispatcher until all competitors have bounced off the lock-gate; then
// release finishes the winning run.
func TestIntegration_Concurrency_LockGate_RunVsRun(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ref := artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"}

	disp := &blockingDispatcher{t: t, release: make(chan struct{})}
	r := newRunner(t, disp, gitURL)

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			// Different apply_id: the in-memory active-gate (keyed by apply_id) is NOT involved —
			// only lockRun under FOR UPDATE provides serialization.
			if err := r.Start(context.Background(), RunSpec{
				ApplyID: audit.NewULID(), IncarnationName: "noop-prod", ServiceRef: ref,
				ScenarioName: "create", StartedByAID: "archon-alice",
			}); err != nil {
				t.Errorf("Start: %v", err)
			}
		}()
	}
	wg.Wait()

	// Wait until exactly one run reaches SendApply (the winner locked the
	// incarnation into applying and blocked on release). The rest should
	// bounce off the lock-gate during this time.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if disp.calls.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := disp.calls.Load(); got != 1 {
		t.Fatalf("SendApply calls = %d, want ровно 1 (lock-gate сериализует run-vs-run)", got)
	}

	// Give the losers time to confirm they won't trigger dispatch later.
	time.Sleep(300 * time.Millisecond)
	if got := disp.calls.Load(); got != 1 {
		t.Fatalf("SendApply calls = %d после паузы, want 1 (конкуренты не должны стартовать apply)", got)
	}

	// apply_runs: only the winner created a running row. Release the winner and
	// wait for the incarnation to return to ready (success terminal of the run).
	close(disp.release)
	waitStatus(t, "noop-prod", incarnation.StatusReady)

	var applyRunRows int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM apply_runs`).Scan(&applyRunRows); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if applyRunRows != 1 {
		t.Errorf("apply_runs rows = %d, want 1 (только победитель завёл строку прогона)", applyRunRows)
	}
}

// waitStatus polls incarnation status until it reaches want (or timeout).
// Separate from waitRunDone: in the concurrency test the winner's apply_id isn't
// known ahead of time (generated inside each goroutine), so we wait on status instead.
func waitStatus(t *testing.T, name string, want incarnation.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inc, err := incarnation.SelectByName(context.Background(), integrationPool, name)
		if err != nil {
			t.Fatalf("waitStatus SelectByName: %v", err)
		}
		if inc.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("incarnation %s не достиг статуса %q за 10s", name, want)
}
