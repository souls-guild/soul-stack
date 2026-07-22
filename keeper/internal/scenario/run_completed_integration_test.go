//go:build integration

// Integration guards for the incarnation.run_completed terminal event on run
// FAILURE (T4 foundation, ADR-052 §k). Verify the abort() branch of run():
//
//   - late abort (dispatch_failed: tasks/plans already rendered) → event
//     status=failed with partial changed_tasks;
//   - early abort (no_hosts: render never reached, tasks/plans nil) → event
//     status=failed with empty changed_tasks (no panic);
//   - TerminalDestroy failure (teardown failed) → does NOT emit run_completed
//     (its own destroy_failed terminal);
//   - single-winner signal from lockIncarnation (finalized) on an
//     already-finalized incarnation.
//
// Infrastructure (testcontainers PG, mock Outbound, local-fs git) shared with
// integration_test.go. Runner is built with a real auditpg.Writer/Reader
// (unlike newRunner, which doesn't wire up Audit) — the event actually reaches
// audit_log.

package scenario

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// newRunnerWithAudit builds a Runner with a real auditpg.Writer/Reader —
// otherwise emitRunCompleted (Audit==nil) silently no-ops and the event never
// reaches audit_log.
func newRunnerWithAudit(t *testing.T, disp ApplyDispatcher) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:       artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:     topology.NewResolver(integrationPool, nil, nil),
		Essence:      essence.NewResolver(nil),
		Render:       render.NewPipeline(nil, engine, nil, nil),
		Outbound:     disp,
		DB:           integrationPool,
		Audit:        auditpg.NewWriter(integrationPool),
		AuditReader:  auditpg.NewReader(integrationPool),
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// runCompletedEvents reads all incarnation.run_completed events for run applyID
// (correlation_id = apply_id) from audit_log and returns their payloads.
func runCompletedEvents(t *testing.T, applyID string) []map[string]any {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(),
		`SELECT payload FROM audit_log
		   WHERE event_type = $1 AND correlation_id = $2
		   ORDER BY created_at`,
		string(audit.EventIncarnationRunCompleted), applyID)
	if err != nil {
		t.Fatalf("query run_completed events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var p map[string]any
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// waitRunCompletedEvents polls until exactly one incarnation.run_completed
// event for run applyID appears and returns its payload. The event in the
// abort()/success branch is written under a detached ctx AFTER the incarnation
// status is finalized, so waiting on status (waitRunDone) isn't enough — we
// need to wait for the actual audit_log write.
func waitRunCompletedEvents(t *testing.T, applyID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if evs := runCompletedEvents(t, applyID); len(evs) > 0 {
			if len(evs) > 1 {
				t.Fatalf("run_completed events = %d, want exactly 1 (one event per run)", len(evs))
			}
			return evs[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("incarnation.run_completed for apply_id=%s did not appear within 10s", applyID)
	return nil
}

// TestIntegration_RunCompletedFailed_LateAbort — late abort (dispatch_failed:
// SendApply returned a failed terminal after render) → incarnation.run_completed
// status=failed is emitted exactly once, changed_tasks is partial (no changes
// here, changed_when:false, so it's empty, but the EVENT exists). Mirrors
// TestIntegration_FailPath, with an added event check.
func TestIntegration_RunCompletedFailed_LateAbort(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	summary := "module failed"
	disp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	r := newRunnerWithAudit(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	ev := waitRunCompletedEvents(t, applyID)
	if ev["status"] != "failed" {
		t.Errorf("event status = %v, want failed", ev["status"])
	}
	if ev["incarnation"] != "noop-prod" {
		t.Errorf("event incarnation = %v, want noop-prod", ev["incarnation"])
	}
	if _, hasCadence := ev["cadence_id"]; hasCadence {
		t.Errorf("manual run should not carry cadence_id, got %v", ev["cadence_id"])
	}
}

// TestIntegration_RunCompletedFailed_EarlyAbort — early abort (no_hosts: render
// never reached, tasks/plans nil) → event status=failed with empty
// changed_tasks, no panic on buildChangedTasks' nil input.
func TestIntegration_RunCompletedFailed_EarlyAbort(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No connected hosts → no_hosts (abort before render).
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithAudit(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "no_hosts" {
		t.Fatalf("reason = %v, want no_hosts", inc.StatusDetails["reason"])
	}

	ev := waitRunCompletedEvents(t, applyID)
	if ev["status"] != "failed" {
		t.Errorf("event status = %v, want failed", ev["status"])
	}
	ct, ok := ev["changed_tasks"].([]any)
	if !ok {
		t.Fatalf("changed_tasks type = %T, want []any (JSONB array)", ev["changed_tasks"])
	}
	if len(ct) != 0 {
		t.Errorf("changed_tasks len = %d, want 0 (early abort, tasks=nil)", len(ct))
	}
}

// TestIntegration_RunCompletedFailed_CadenceIDPresent — a run with CadenceID != nil
// (simulating a Voyage-schedule child run) → the failure event carries cadence_id.
func TestIntegration_RunCompletedFailed_CadenceIDPresent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithAudit(t, disp)

	cadenceID := "cad-77"
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		CadenceID:       &cadenceID,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// no_hosts → failed terminal; we care about cadence_id in the payload.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	ev := waitRunCompletedEvents(t, applyID)
	if ev["cadence_id"] != "cad-77" {
		t.Errorf("event cadence_id = %v, want cad-77 (child Voyage of the schedule)", ev["cadence_id"])
	}
}

// TestIntegration_RunCompleted_VoyageIDPresent — a run with VoyageID != nil
// (simulating a spawn via the Voyage orchestrator) → the incarnation.run_completed
// event carries voyage_id in the payload (ADR-052 amend §k, Voyage detail
// visibility fetch). Verified through a real run() (failed path, mirrors the
// CadenceID test) and that the same event is found by filtering on
// payload->>'voyage_id' (Voyage detail).
func TestIntegration_RunCompleted_VoyageIDPresent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithAudit(t, disp)

	voyageID := "voy-77"
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		VoyageID:        &voyageID,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// no_hosts → failed terminal; we care about voyage_id in the payload.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	ev := waitRunCompletedEvents(t, applyID)
	if ev["voyage_id"] != "voy-77" {
		t.Errorf("event voyage_id = %v, want voy-77 (run via Voyage)", ev["voyage_id"])
	}
	if _, hasCadence := ev["cadence_id"]; hasCadence {
		t.Errorf("voyage without cadence should not carry cadence_id, got %v", ev["cadence_id"])
	}

	// Voyage detail fetches the voyage's run events by filtering on
	// payload->>'voyage_id' (correlation_id on a per-incarnation event =
	// apply_id, not voyage_id).
	var byVoyage int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE event_type = $1 AND payload->>'voyage_id' = $2`,
		string(audit.EventIncarnationRunCompleted), voyageID).Scan(&byVoyage); err != nil {
		t.Fatalf("count by voyage_id: %v", err)
	}
	if byVoyage != 1 {
		t.Errorf("voyage events by payload->>'voyage_id' = %d, want 1", byVoyage)
	}
}

// TestIntegration_DestroyFailed_NoRunCompleted — a teardown failure
// (TerminalDestroy) goes to its own destroy_failed terminal and does NOT emit
// incarnation.run_completed (the TerminalMode != TerminalDestroy gate in abort).
func TestIntegration_DestroyFailed_NoRunCompleted(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedDestroyingIncarnation(t, "noop-prod", map[string]any{"leader": "host-a"})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := destroyServiceRepo(t)

	summary := "teardown failed"
	disp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	r := newRunnerWithAudit(t, disp)

	applyID := audit.NewULID()
	if err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("StartDestroy: %v", err)
	}

	waitStatusInc(t, "noop-prod", incarnation.StatusDestroyFailed)

	// destroy_failed event — the destroy terminal, must appear. Filtering by
	// correlation_id is standard (writeDestroyFailedAudit sets
	// correlation_id=apply_id, like run_completed). Poll until it shows up — the
	// event is written under a detached ctx after the status change.
	deadline := time.Now().Add(10 * time.Second)
	var destroyFailed int
	for time.Now().Before(deadline) {
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_log WHERE event_type = $1 AND correlation_id = $2`,
			string(audit.EventIncarnationDestroyFailed), applyID).Scan(&destroyFailed); err != nil {
			t.Fatalf("count destroy_failed: %v", err)
		}
		if destroyFailed > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if destroyFailed != 1 {
		t.Errorf("destroy_failed events = %d, want 1 (its own destroy terminal)", destroyFailed)
	}

	// run_completed is NOT emitted for a destroy failure (the TerminalDestroy
	// gate in abort). destroy_failed is already recorded above → if
	// run_completed were emitted, it would have shown up too; we check after
	// its appearance.
	if evs := runCompletedEvents(t, applyID); len(evs) != 0 {
		t.Errorf("run_completed events = %d, want 0 (destroy failure emits destroy_failed, NOT run_completed)", len(evs))
	}
}

// TestIntegration_LockIncarnation_SingleWinnerSignal — the single-winner signal
// from lockIncarnation: on an incarnation that ANOTHER committer has already
// moved out of applying (UpdateStateFromRun → ErrAlreadyFinalized),
// lockIncarnation returns finalized=false. abort() with finalized=false does
// NOT emit a failure event (protects against a duplicate on a recovery
// takeover). This tests the signal itself — the `&& finalized` gate in abort is
// read explicitly and covered by this invariant.
func TestIntegration_LockIncarnation_SingleWinnerSignal(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// incarnation in applying — the state lockIncarnation finalizes from.
	inc := &incarnation.Incarnation{
		Name: "noop-prod", Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusApplying,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("Create applying incarnation: %v", err)
	}

	r := newRunnerWithAudit(t, &mockDispatcher{t: t, result: applyrun.StatusSuccess})
	log := slog.New(slog.DiscardHandler)
	spec := RunSpec{ApplyID: audit.NewULID(), IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// First committer (us) — actually finalizes applying → error_locked.
	if finalized := r.lockIncarnation(context.Background(), spec, nil, incarnation.StatusErrorLocked, "dispatch_failed", nil, nil, log); !finalized {
		t.Fatalf("first lockIncarnation: finalized=false, want true (real finalizer)")
	}

	// Second call (simulating a recovery loser: incarnation is no longer in
	// applying) → ErrAlreadyFinalized internally → finalized=false: the failure
	// event is emitted by the winner, not this instance.
	spec2 := RunSpec{ApplyID: audit.NewULID(), IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}
	if finalized := r.lockIncarnation(context.Background(), spec2, nil, incarnation.StatusErrorLocked, "dispatch_failed", nil, nil, log); finalized {
		t.Errorf("second lockIncarnation on an already-finalized incarnation: finalized=true, want false (single-winner loser)")
	}
}
