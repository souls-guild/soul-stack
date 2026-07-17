//go:build integration

// Integration tests for the teardown run mode (S-D2b): scenario `destroy` via
// TerminalMode=TerminalDestroy. Verify run()'s final branching:
//
//   - success teardown → incarnation stays in `destroying` (NOT ready), state is
//     untouched, ready is NOT committed (physical row removal — S-D3);
//   - teardown failure (a host fails, barrier fail-closed) → `destroy_failed`
//     (NOT error_locked), status_details carries the reason;
//   - lockRun gate for teardown: only starts from `destroying`.
//
// Infrastructure (testcontainers PG, mock Outbound, local-fs git) is shared with
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

// seedDestroyingIncarnation creates an incarnation directly in `destroying` status
// (as after S-D1 Destroy) with a non-empty state — so the test can verify that
// teardown does NOT touch the state graph. Coven label = name (roster by
// incarnation.name).
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

// destroyServiceRepo creates a local-fs git repo for the service-noop with a
// `destroy` scenario (one core.exec.run teardown step). file:// URL for the loader.
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

// waitStatusInc polls incarnation status until it reaches want (or times out) and
// returns the row for follow-up asserts (state / status_details). Unlike
// waitStatus (gap4_test.go) it returns the incarnation; unlike waitRunDone it
// doesn't require a state_history snapshot — teardown-success in S-D2b does NOT
// write a snapshot (state isn't committed), status stays destroying.
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
	t.Fatalf("incarnation %s did not reach status %q within 10s", name, want)
	return nil
}

// TestIntegration_Destroy_Teardown_Success — teardown succeeded on all hosts →
// S-D3 physical removal: after a successful barrier, DeleteAfterTeardown archives
// and DELETEs the incarnation row in one tx; the V3 cascade removes live
// state_history / apply_runs / register. So the observable success terminal is the
// ABSENCE of the incarnation row (not "stays in destroying" — that was the
// pre-S-D3 semantics).
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

	// teardown success terminal — the incarnation row is physically removed (S-D3).
	waitIncarnationGone(t, "noop-prod")

	// dispatch happened exactly once (one host teardown).
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (one host teardown)", disp.calls)
	}

	// V3 cascade: the run's apply_runs are removed along with the incarnation row.
	st, serr := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if serr != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", serr)
	}
	if len(st) != 0 {
		t.Errorf("apply_runs of the run = %d, want 0 (cascade V3 removed it along with the incarnation)", len(st))
	}
}

// waitIncarnationGone polls until the incarnation row physically disappears
// (S-D3 success: DeleteAfterTeardown in the run goroutine after the barrier).
// Before removal SelectByName returns the row; after — ErrIncarnationNotFound.
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
	t.Fatalf("incarnation %s not removed within 10s (teardown-success should delete the row)", name)
}

// TestIntegration_Destroy_Teardown_Fail — a host failed (RunResult failed) →
// barrier fail-closed → incarnation goes to `destroy_failed` (NOT error_locked),
// status_details carries the reason. state is untouched.
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

	// A teardown failure transitions to destroy_failed (NOT error_locked).
	inc := waitStatusInc(t, "noop-prod", incarnation.StatusDestroyFailed)
	if inc.Status == incarnation.StatusErrorLocked {
		t.Fatal("status = error_locked - a failed teardown should yield destroy_failed, NOT error_locked")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details = %+v, want reason=dispatch_failed", inc.StatusDetails)
	}
	// state is untouched on failure (last known-good).
	if inc.State["leader"] != "host-a" {
		t.Errorf("state.leader = %v, want host-a (state does not change on failure)", inc.State["leader"])
	}

	// A failure writes a state_history snapshot (zero-diff, records the fact of the run).
	hist, total, herr := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if herr != nil {
		t.Fatalf("HistorySelectByName: %v", herr)
	}
	if total != 1 {
		t.Fatalf("state_history snapshots = %d, want 1 (failure records the fact of the run)", total)
	}
	if hist[0].Scenario != DestroyScenarioName {
		t.Errorf("history scenario = %q, want %q", hist[0].Scenario, DestroyScenarioName)
	}
}

// TestIntegration_Destroy_Teardown_RejectsNonDestroying — teardown starts strictly
// from `destroying`: an incarnation in ready → StartDestroy is rejected by the
// lockRun gate (ErrNotRunnable), status doesn't change, no dispatch happens.
func TestIntegration_Destroy_Teardown_RejectsNonDestroying(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod") // ready, not destroying
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

	// The run is rejected inside the run goroutine (lockRun → ErrNotRunnable);
	// status stays ready, no dispatch happens.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disp.calls > 0 {
			t.Fatalf("SendApply called for teardown from the ready state")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if got.Status != incarnation.StatusReady {
		t.Errorf("status = %q, want ready (teardown from ready is rejected)", got.Status)
	}
}
