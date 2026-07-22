//go:build integration

// End-to-end integration test tying together S4 (reclaim-only-claimed /
// re-claim) and S1/S5 (RunResult.attempt epoch-check in correlateRunResult)
// on a LIVE PG.
//
// Major coverage gap (qa ae10): existing tests assert each link in
// isolation — TestIntegration_ClaimNext_AttemptIncrements drives attempt up
// to 2 and stops there; TestCorrelateRunResult_StaleAttemptDropped feeds a
// stale result against a SYNTHETIC rowAttempt=5 via a fake DB. The
// end-to-end seam (attempt actually advances through a re-claim →
// correlateRunResult reads that same row and rejects the stale attempt) had
// never been asserted in one system. This test runs the full chain on a
// single PG pool through real applyrun CRUD and the real
// eventStreamHandler.correlateRunResult.

package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// resetRecoveryE2E clears the tables used by the end-to-end recovery test.
// The grpc-package resetAll truncates onboarding tables but not apply_runs /
// incarnation / state_history — here we need exactly the apply-lifecycle set.
func resetRecoveryE2E(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE apply_runs, state_history, incarnation, operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE recovery-e2e: %v", err)
	}
}

// seedRecoveryIncarnation creates an operator and an incarnation with a
// known state — a control for "correlateRunResult doesn't touch
// incarnation.state".
func seedRecoveryIncarnation(t *testing.T, name string, state map[string]any) {
	t.Helper()
	ctx := context.Background()
	aid := "archon-alice"
	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := aid
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, State: state,
		Status: incarnation.StatusReady, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("incarnation.Create: %v", err)
	}
}

// newRecoveryHandler — a handler over a live PG pool with registered
// keeper_grpc_* metrics (scrape keeper_runresult_stale_total) and a
// recordingAudit. SeedDB isn't needed (correlateRunResult only touches
// ApplyRunDB), but validate() requires SeedDB+AuditWriter — we supply
// fake/recording ones.
func newRecoveryHandler(t *testing.T) (*eventStreamHandler, *recordingAudit, *obs.Registry) {
	t.Helper()
	reg := obs.NewRegistry()
	aw := &recordingAudit{}
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		ApplyRunDB:  integrationPool,
		Metrics:     RegisterGRPCMetrics(reg),
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardIntegrationLogger()), aw, reg
}

// readApplyStatus reads the status of the `(applyID, sid)` row directly
// (bypassing CRUD guards).
func readApplyStatus(t *testing.T, ctx context.Context, applyID, sid string) string {
	t.Helper()
	var st string
	if err := integrationPool.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid).Scan(&st); err != nil {
		t.Fatalf("read apply_runs status: %v", err)
	}
	return st
}

// readIncarnationState reads incarnation.state as text (for the "untouched"
// comparison).
func readIncarnationState(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	var s string
	if err := integrationPool.QueryRow(ctx,
		`SELECT state::text FROM incarnation WHERE name = $1`, name).Scan(&s); err != nil {
		t.Fatalf("read incarnation.state: %v", err)
	}
	return s
}

// TestIntegration_RecoveryReclaim_StaleRunResultDropped_LiveAttempt2Commits —
// END-TO-END S4↔S1/S5 on a live PG:
//
//	InsertPlanned → ClaimNext(attempt=1) → MarkDispatched(claimed→dispatched)
//	  → emulate owner death + lease expiry + reclaim (dispatched can't be
//	    reclaimed, so we first "roll back" to claimed as an
//	    under-delivered render, then ReclaimApplyRuns on the expired lease
//	    returns it to planned, attempt is preserved)
//	  → ClaimNext(attempt=2)
//	  → correlateRunResult(RunResult{attempt=1}) = stale attempt:
//	    stale-drop + keeper_runresult_stale_total++ + row stays
//	    non-terminal + incarnation.state untouched
//	  → correlateRunResult(RunResult{attempt=2}) = current attempt:
//	    commit (row terminates as success).
//
// Asserts that attempt really advances through a re-claim, and that
// correlateRunResult reads that same row and applies the epoch-check to it.
func TestIntegration_RecoveryReclaim_StaleRunResultDropped_LiveAttempt2Commits(t *testing.T) {
	resetRecoveryE2E(t)
	const (
		incName = "redis-prod"
		applyID = "01HRECOVERYE2E0000000000"
		sid     = "host.example.com"
	)
	knownState := map[string]any{"replicas": float64(3), "version": "7.2"}
	seedRecoveryIncarnation(t, incName, knownState)
	ctx := context.Background()
	aid := "archon-alice"

	// 1) InsertPlanned — a planned job for Acolyte claim.
	if err := applyrun.InsertPlanned(ctx, integrationPool, &applyrun.ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: incName, Scenario: "scale",
		StartedByAID: &aid,
		Recipe: &applyrun.Recipe{
			ScenarioName: "scale",
			Input:        map[string]any{"replicas": float64(5)},
			StartedByAID: &aid,
		},
	}); err != nil {
		t.Fatalf("InsertPlanned: %v", err)
	}

	// 2) ClaimNext → attempt 0→1; MarkDispatched → claimed→dispatched.
	claimed, err := applyrun.ClaimNext(ctx, integrationPool, "keeper-dead", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#1: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Attempt != 1 {
		t.Fatalf("first claim: len=%d attempt=%v, want 1/1", len(claimed), claimed)
	}
	if err := applyrun.MarkDispatched(ctx, integrationPool, applyID, sid); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusDispatched) {
		t.Fatalf("after MarkDispatched status=%q, want dispatched", got)
	}

	// 3) Emulate owner death BEFORE handoff + lease expiry + re-claim.
	// In the Acolyte flow, reclaim is narrowed to status='claimed' (S4):
	// dispatched is NOT reclaimed by design. Here we model "the owner died
	// before finishing the render/handoff" — the row is treated as an
	// under-delivered claimed row with an expired lease. First we move the
	// row to claimed with an already-expired claim_expires_at (as if
	// dispatched never got set), then the recovery reclaim predicate
	// (status='claimed' AND claim_expires_at < NOW() → planned, attempt
	// PRESERVED) returns it to the queue. The SQL mirrors
	// reaper.reclaimApplyRunsSQL — pulling in the reaper package for one
	// UPDATE isn't worth it, so the predicate is reproduced literally.
	if _, err := integrationPool.Exec(ctx, `
		UPDATE apply_runs
		SET status='claimed', claim_by_kid='keeper-dead', claim_at=NOW() - INTERVAL '2 hours',
		    claim_expires_at=NOW() - INTERVAL '1 hour'
		WHERE apply_id=$1 AND sid=$2`, applyID, sid); err != nil {
		t.Fatalf("emulating expired claimed: %v", err)
	}
	tag, err := integrationPool.Exec(ctx, `
		UPDATE apply_runs
		SET status='planned', claim_by_kid=NULL, claim_at=NULL, claim_expires_at=NULL
		WHERE status='claimed' AND claim_expires_at < NOW()`)
	if err != nil {
		t.Fatalf("reclaim of stale claimed: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("reclaim affected %d rows, want 1 (stale claimed → planned)", tag.RowsAffected())
	}
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusPlanned) {
		t.Fatalf("after reclaim status=%q, want planned", got)
	}

	// 4) ClaimNext again → attempt 1→2 (fencing epoch increased: new owner).
	reclaim, err := applyrun.ClaimNext(ctx, integrationPool, "keeper-live", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#2: %v", err)
	}
	if len(reclaim) != 1 {
		t.Fatalf("repeat claim len=%d, want 1", len(reclaim))
	}
	if reclaim[0].Attempt != 2 {
		t.Fatalf("repeat claim attempt=%d, want 2 (1→2 via re-claim)", reclaim[0].Attempt)
	}
	// Drive it to dispatched, as a live Acolyte would before SendApply.
	if err := applyrun.MarkDispatched(ctx, integrationPool, applyID, sid); err != nil {
		t.Fatalf("MarkDispatched#2: %v", err)
	}

	h, aw, reg := newRecoveryHandler(t)
	stateBefore := readIncarnationState(t, ctx, incName)

	// 5) RunResult from the FIRST (stale) attempt=1 — via the real handler.
	h.handleRunResult(ctx, sid, "session-stale", &keeperv1.RunResult{
		ApplyId: applyID, Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Attempt: 1,
	})

	// ASSERT: stale metric +1.
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("keeper_runresult_stale_total != 1 after stale RunResult; got=\n%s", body)
	}
	// ASSERT: the row remains NON-terminal (dispatched from the 2nd claim).
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusDispatched) {
		t.Errorf("after stale RunResult status=%q, want dispatched (not terminal)", got)
	}
	// ASSERT: incarnation.state is unchanged (correlateRunResult doesn't touch it).
	if got := readIncarnationState(t, ctx, incName); got != stateBefore {
		t.Errorf("incarnation.state changed by a stale result: %q → %q", stateBefore, got)
	}
	// audit run.completed is written BEFORE correlate — the fact of receipt is recorded even for stale results.
	if len(aw.snapshot()) != 1 {
		t.Errorf("audit events = %d, want 1 (run.completed before correlate)", len(aw.snapshot()))
	}

	// 6) Contrast: RunResult from the CURRENT attempt=2 → commit (terminal).
	h.handleRunResult(ctx, sid, "session-live", &keeperv1.RunResult{
		ApplyId: applyID, Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Attempt: 2,
	})
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusSuccess) {
		t.Errorf("after a fresh RunResult status=%q, want success (commit)", got)
	}
	// The stale metric did NOT increase again: the current attempt isn't stale.
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_runresult_stale_total 2") {
		t.Errorf("keeper_runresult_stale_total grew on a fresh attempt; got=\n%s", body)
	}
}
