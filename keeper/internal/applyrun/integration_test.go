//go:build integration

// Integration tests for the apply_runs CRUD via testcontainers-go. The pattern
// matches keeper/internal/incarnation/integration_test.go.

package applyrun

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("applyrun integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("applyrun integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetAll(t *testing.T) {
	t.Helper()
	// CASCADE: apply_runs → incarnation → operators (FK chain).
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE apply_runs, state_history, incarnation, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{
		AID:         aid,
		DisplayName: aid,
		AuthMethod:  operator.AuthMethodJWT,
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func seedIncarnation(t *testing.T, name, aid string) {
	t.Helper()
	creator := aid
	inc := &incarnation.Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: incarnation.StatusReady, CreatedByAID: &creator,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnation(%s): %v", name, err)
	}
}

func TestIntegration_Insert_AndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	aid := "archon-alice"
	run := &ApplyRun{
		ApplyID:         "01HAPPLY0000000000000000",
		SID:             "host.example.com",
		IncarnationName: "redis-prod",
		Scenario:        "create",
		TaskIdx:         intp(0),
		Status:          StatusRunning,
		StartedByAID:    &aid,
	}
	if err := Insert(ctx, integrationPool, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if run.StartedAt.IsZero() {
		t.Errorf("StartedAt zero — RETURNING did not fill")
	}

	got, err := SelectByApplyID(ctx, integrationPool, "01HAPPLY0000000000000000", "host.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.IncarnationName != "redis-prod" || got.Scenario != "create" {
		t.Errorf("got = %+v", got)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.TaskIdx == nil || *got.TaskIdx != 0 {
		t.Errorf("task_idx = %v, want 0", got.TaskIdx)
	}
	if got.FinishedAt != nil {
		t.Errorf("finished_at = %v, want nil for running", got.FinishedAt)
	}
	if got.StartedByAID == nil || *got.StartedByAID != "archon-alice" {
		t.Errorf("started_by_aid = %v", got.StartedByAID)
	}
}

func TestIntegration_Insert_NullableNils(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}
	if err := Insert(ctx, integrationPool, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.TaskIdx != nil || got.ErrorSummary != nil || got.StartedByAID != nil {
		t.Errorf("nullable not nil: %+v", got)
	}
}

func TestIntegration_Insert_DuplicateKey(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}
	if err := Insert(ctx, integrationPool, run); err != nil {
		t.Fatalf("Insert#1: %v", err)
	}
	err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	})
	if !errors.Is(err, ErrApplyRunAlreadyExists) {
		t.Fatalf("err = %v, want ErrApplyRunAlreadyExists", err)
	}
}

func TestIntegration_Insert_SameApplyDifferentSID(t *testing.T) {
	// apply_id model A: one apply_id, different sid → two independent rows.
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b"} {
		run := &ApplyRun{
			ApplyID: "01HSAMEAPPLY", SID: sid, IncarnationName: "redis-prod",
			Scenario: "scale", Status: StatusRunning,
		}
		if err := Insert(ctx, integrationPool, run); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE apply_id = '01HSAMEAPPLY'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("rows for shared apply_id = %d, want 2", n)
	}
}

func TestIntegration_Insert_FKViolation(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// The incarnation doesn't exist → FK violation.
	err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "ghost", Scenario: "create",
		Status: StatusRunning,
	})
	if err == nil {
		t.Fatal("Insert with non-existent incarnation: expected error")
	}
	if errors.Is(err, ErrApplyRunAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrApplyRunAlreadyExists; err = %v", err)
	}
}

func TestIntegration_Insert_CHECKViolation_BadStatus(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	// Bypass Go validation with a direct Exec, exercising the SQL-side CHECK.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
		 VALUES ('a', 's', 'redis-prod', 'create', 'error_locked')`)
	if err == nil {
		t.Fatal("expected CHECK violation for status='error_locked'")
	}
}

func TestIntegration_UpdateStatus_SetsFinishedAt(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := UpdateStatus(ctx, integrationPool, "a", "s", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("status = %q, want success", got.Status)
	}
	if got.FinishedAt == nil {
		t.Errorf("finished_at nil after terminal status; want set")
	}
}

// TestIntegration_UpdateStatus_ErrorSummaryCoalesce — the error_summary COALESCE
// semantics on a LEGITIMATE running→terminal transition: the summary is written on
// running→failed. The append-only guard (ADR-027(j), S-P2.4) forbids a
// terminal→terminal overwrite — a repeat UpdateStatus(failed→failed) is now
// rejected with [ErrApplyRunAlreadyTerminal] (instead of running another COALESCE).
// The test reflects the new invariant rather than working around it: the first
// terminal committer wins, the second is a no-op rejection.
func TestIntegration_UpdateStatus_ErrorSummaryCoalesce(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// running→failed: the summary is written (COALESCE on a legitimate transition).
	if err := UpdateStatus(ctx, integrationPool, "a", "s", 0, StatusFailed, strp("boom")); err != nil {
		t.Fatalf("UpdateStatus#1 (running→failed): %v", err)
	}

	// The repeat terminal write (failed→failed) is rejected by the append-only guard
	// BEFORE any write — the caller treats it as a no-op (the first committer won).
	err := UpdateStatus(ctx, integrationPool, "a", "s", 0, StatusFailed, nil)
	if !errors.Is(err, ErrApplyRunAlreadyTerminal) {
		t.Fatalf("UpdateStatus#2 (failed→failed): err = %v, want ErrApplyRunAlreadyTerminal", err)
	}

	// The rejected second write didn't touch error_summary: it stayed "boom".
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.ErrorSummary == nil || *got.ErrorSummary != "boom" {
		t.Errorf("error_summary = %v, want preserved \"boom\"", got.ErrorSummary)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed (first terminal fixed)", got.Status)
	}
}

func TestIntegration_UpdateStatus_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	err := UpdateStatus(ctx, integrationPool, "ghost", "s", 0, StatusSuccess, nil)
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

func TestIntegration_SelectIncarnationByApplyID(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "scale",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	name, scenario, attempt, err := SelectIncarnationByApplyID(ctx, integrationPool, "a", "s", 0)
	if err != nil {
		t.Fatalf("SelectIncarnationByApplyID: %v", err)
	}
	if name != "redis-prod" || scenario != "scale" {
		t.Errorf("got (%q, %q), want (redis-prod, scale)", name, scenario)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0 (fresh Insert without claim, DEFAULT 0)", attempt)
	}
}

func TestIntegration_SelectIncarnationByApplyID_NotFound(t *testing.T) {
	resetAll(t)
	_, _, _, err := SelectIncarnationByApplyID(context.Background(), integrationPool, "ghost", "s", 0)
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

func TestIntegration_ApplyRuns_FK_OnIncarnationDelete(t *testing.T) {
	// CASCADE: deleting the incarnation makes apply_runs rows disappear.
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM incarnation WHERE name = 'redis-prod'`); err != nil {
		t.Fatalf("DELETE incarnation: %v", err)
	}
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE incarnation_name = 'redis-prod'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("apply_runs rows after CASCADE = %d, want 0", n)
	}
}

func TestIntegration_ApplyRuns_FK_OnOperatorDelete_SetsNull(t *testing.T) {
	// SET NULL: deleting the operator clears started_by_aid.
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	aid := "archon-alice"
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning, StartedByAID: &aid,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// The incarnation was created by archon-alice → first clear its FK there, so
	// the operator can be deleted without a conflict (incarnation.created_by_aid is also SET NULL).
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM operators WHERE aid = 'archon-alice'`); err != nil {
		t.Fatalf("DELETE operator: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.StartedByAID != nil {
		t.Errorf("started_by_aid = %v after operator delete, want nil (SET NULL)", got.StartedByAID)
	}
}

func TestIntegration_SelectStatusesByApplyID(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// Three hosts of one run (apply_id model A), plus a noise row of
	// another apply_id — it must not end up in the selection.
	for _, sid := range []string{"host-c", "host-a", "host-b"} {
		if err := Insert(ctx, integrationPool, &ApplyRun{
			ApplyID: "01HBARRIER", SID: sid, IncarnationName: "redis-prod",
			Scenario: "restart", Status: StatusRunning,
		}); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HOTHER", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "restart", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert noise: %v", err)
	}

	// Move one host to success, the other to failed with a summary.
	if err := UpdateStatus(ctx, integrationPool, "01HBARRIER", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus host-a: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "01HBARRIER", "host-b", 0, StatusFailed, strp("boom")); err != nil {
		t.Fatalf("UpdateStatus host-b: %v", err)
	}

	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HBARRIER")
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (noise apply_id excluded)", len(got))
	}
	// ORDER BY sid ASC.
	wantSIDs := []string{"host-a", "host-b", "host-c"}
	for i, hs := range got {
		if hs.SID != wantSIDs[i] {
			t.Errorf("got[%d].SID = %q, want %q (sorted)", i, hs.SID, wantSIDs[i])
		}
	}
	if got[0].Status != StatusSuccess {
		t.Errorf("host-a status = %q, want success", got[0].Status)
	}
	if got[1].Status != StatusFailed {
		t.Errorf("host-b status = %q, want failed", got[1].Status)
	}
	if got[1].ErrorSummary == nil || *got[1].ErrorSummary != "boom" {
		t.Errorf("host-b error_summary = %v, want boom", got[1].ErrorSummary)
	}
	if got[2].Status != StatusRunning {
		t.Errorf("host-c status = %q, want running", got[2].Status)
	}
}

func TestIntegration_SelectStatusesByApplyID_Empty(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HNONE")
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 for unknown apply_id", len(got))
	}
}

// TestIntegration_RequestCancel_FlagsRunningHosts — cluster-wide Cancel (G1):
// RequestCancel sets cancel_requested on all running rows of the run; the flag
// is read back via SelectStatusesByApplyID (the same path the run goroutine's
// barrier polling uses to see it).
func TestIntegration_RequestCancel_FlagsRunningHosts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b"} {
		if err := Insert(ctx, integrationPool, &ApplyRun{
			ApplyID: "01HCANCEL", SID: sid, IncarnationName: "redis-prod",
			Scenario: "restart", Status: StatusRunning,
		}); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	// A noise run — its cancel_requested must not be touched.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HKEEP", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "restart", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert noise: %v", err)
	}

	// Before cancellation the flag isn't set anywhere.
	before, err := SelectStatusesByApplyID(ctx, integrationPool, "01HCANCEL")
	if err != nil {
		t.Fatalf("SelectStatuses before: %v", err)
	}
	for _, hs := range before {
		if hs.CancelRequested {
			t.Fatalf("host %s: cancel_requested=true before RequestCancel", hs.SID)
		}
	}

	affected, err := RequestCancel(ctx, integrationPool, "01HCANCEL")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 2 {
		t.Errorf("affected = %d, want 2 (both running hosts of the run)", affected)
	}

	after, err := SelectStatusesByApplyID(ctx, integrationPool, "01HCANCEL")
	if err != nil {
		t.Fatalf("SelectStatuses after: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("len = %d, want 2", len(after))
	}
	for _, hs := range after {
		if !hs.CancelRequested {
			t.Errorf("host %s: cancel_requested=false after RequestCancel", hs.SID)
		}
	}

	// The noise run is unaffected.
	noise, err := SelectStatusesByApplyID(ctx, integrationPool, "01HKEEP")
	if err != nil {
		t.Fatalf("SelectStatuses noise: %v", err)
	}
	if len(noise) != 1 || noise[0].CancelRequested {
		t.Errorf("noise run was touched by RequestCancel: %+v", noise)
	}
}

// TestIntegration_RequestCancel_TerminalNoOp — Cancel of an already-finished
// run (all rows terminal) doesn't set the flag: affected=0, a no-op.
func TestIntegration_RequestCancel_TerminalNoOp(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HDONE", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "restart", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "01HDONE", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	affected, err := RequestCancel(ctx, integrationPool, "01HDONE")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (terminal run is no-op)", affected)
	}
	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HDONE")
	if err != nil {
		t.Fatalf("SelectStatuses: %v", err)
	}
	if len(got) != 1 || got[0].CancelRequested {
		t.Errorf("terminal run got cancel_requested: %+v", got)
	}
}

// TestIntegration_RequestCancel_Idempotent — a repeat RequestCancel on a
// running run is idempotent (the flag stays true→true, no error).
func TestIntegration_RequestCancel_Idempotent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedApplyRun(t, "01HTWICE", "host-a")

	first, err := RequestCancel(ctx, integrationPool, "01HTWICE")
	if err != nil {
		t.Fatalf("RequestCancel first: %v", err)
	}
	second, err := RequestCancel(ctx, integrationPool, "01HTWICE")
	if err != nil {
		t.Fatalf("RequestCancel second: %v", err)
	}
	if first != 1 || second != 1 {
		t.Errorf("affected first=%d second=%d, want 1 and 1 (idempotent)", first, second)
	}
}

// TestIntegration_RequestCancel_UnknownApplyID — cancelling a non-existent
// run: affected=0, no error.
func TestIntegration_RequestCancel_UnknownApplyID(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	affected, err := RequestCancel(ctx, integrationPool, "01HGHOST")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 for unknown apply_id", affected)
	}
}

// TestIntegration_RequestCancel_PartialRunning — a run with mixed hosts: one
// is already success (terminal), the other is still running. The status='running'
// filter in RequestCancel sets the flag ONLY on the running row (affected=1, not 2);
// that's enough for the barrier ([cancelRequested] sees true on any row).
// The boundary between all-running ([TestIntegration_RequestCancel_FlagsRunningHosts])
// and all-terminal ([TestIntegration_RequestCancel_TerminalNoOp]): cancelling a run
// where part of the hosts have already finished.
func TestIntegration_RequestCancel_PartialRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b"} {
		if err := Insert(ctx, integrationPool, &ApplyRun{
			ApplyID: "01HMIXED", SID: sid, IncarnationName: "redis-prod",
			Scenario: "restart", Status: StatusRunning,
		}); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	// host-a already finished with success — it must not get cancel_requested.
	if err := UpdateStatus(ctx, integrationPool, "01HMIXED", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus host-a: %v", err)
	}

	affected, err := RequestCancel(ctx, integrationPool, "01HMIXED")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1 (only still-running host-b)", affected)
	}

	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HMIXED")
	if err != nil {
		t.Fatalf("SelectStatuses: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	byID := map[string]HostStatus{got[0].SID: got[0], got[1].SID: got[1]}
	if byID["host-a"].CancelRequested {
		t.Error("host-a (success terminal) got cancel_requested: status='running' filter is broken")
	}
	if !byID["host-b"].CancelRequested {
		t.Error("host-b (running) did not get cancel_requested")
	}
	// The barrier will see the flag on any row of the run — a partial flag is enough.
	if !cancelRequestedAny(got) {
		t.Error("no row carries cancel_requested: barrier will not cancel the run")
	}
}

// cancelRequestedAny — a test mirror of scenario.cancelRequested: the barrier
// only needs the flag on any row of the run. We duplicate the scenario package's
// private helper here so the applyrun test doesn't pull in an import of scenario.
func cancelRequestedAny(statuses []HostStatus) bool {
	for i := range statuses {
		if statuses[i].CancelRequested {
			return true
		}
	}
	return false
}

func TestIntegration_RecordTaskFailure_FirstFailureWins(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedApplyRun(t, "01HFAIL", "host-a")

	// The first failed task pins task_idx (local 1) + failed_plan_index
	// (global 4 — staged/per-host-where, local ≠ global) + the summary.
	if err := RecordTaskFailure(ctx, integrationPool, "01HFAIL", "host-a", 0, 1, 4,
		"task 4 core.pkg.installed: E: Version '7.2.4' not found"); err != nil {
		t.Fatalf("RecordTaskFailure first: %v", err)
	}
	// The second failed task does NOT overwrite (COALESCE first-failure-wins) —
	// neither task_idx, nor failed_plan_index, nor the summary.
	if err := RecordTaskFailure(ctx, integrationPool, "01HFAIL", "host-a", 0, 3, 9, "task 9 later boom"); err != nil {
		t.Fatalf("RecordTaskFailure second: %v", err)
	}

	got, err := SelectByApplyID(ctx, integrationPool, "01HFAIL", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.TaskIdx == nil || *got.TaskIdx != 1 {
		t.Errorf("task_idx = %v, want 1 (first failed task, local)", got.TaskIdx)
	}
	if got.ErrorSummary == nil || *got.ErrorSummary != "task 4 core.pkg.installed: E: Version '7.2.4' not found" {
		t.Errorf("error_summary = %v, want first task", got.ErrorSummary)
	}
	// The status stays running until RunResult.
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running (RecordTaskFailure does not touch status)", got.Status)
	}

	// failed_plan_index is read via the HostStatus projection (SelectByApplyID doesn't
	// carry it). The global index of the first failed task = 4 (first-failure-wins).
	statuses, err := SelectStatusesByApplyID(ctx, integrationPool, "01HFAIL")
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses len = %d, want 1", len(statuses))
	}
	hs := statuses[0]
	if hs.FailedPlanIndex == nil || *hs.FailedPlanIndex != 4 {
		t.Errorf("failed_plan_index = %v, want 4 (global, first failed task not overwritten by second with 9)", hs.FailedPlanIndex)
	}
	if hs.TaskIdx == nil || *hs.TaskIdx != 1 {
		t.Errorf("HostStatus.TaskIdx = %v, want 1 (local)", hs.TaskIdx)
	}
}

func TestIntegration_RecordTaskFailure_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	err := RecordTaskFailure(ctx, integrationPool, "01HMISSING", "host-x", 0, 0, 0, "boom")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

// TestIntegration_WardClaimColumns_Phase0 — the ADR-027 Phase 0 acceptance
// criterion: migration 025 is applied to the container schema; an existing
// apply_runs row from the old path (Insert) coexists with the new Ward-claim
// columns, which get a DEFAULT (attempt=0, claim_* NULL) and are written by
// no one; the status CHECK accepts planned/claimed and keeps
// running/success/failed/cancelled; the claim-scan partial index is created.
// The 025 down/up reversibility is covered by the migrate package
// (TestIntegration_MigrateApply_DownThenUp runs a full down→up) + a sanity
// check on the down.sql content (migrations_test).
func TestIntegration_WardClaimColumns_Phase0(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// An old-path row via plain Insert — the CRUD layer knows nothing about Ward-claim.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HWARD", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert legacy row: %v", err)
	}

	// The Ward-claim columns exist and carry DEFAULT values on the old-path row.
	var (
		attempt        int
		claimByKID     *string
		claimAt        *time.Time
		claimExpiresAt *time.Time
	)
	if err := integrationPool.QueryRow(ctx, `
		SELECT attempt, claim_by_kid, claim_at, claim_expires_at
		FROM apply_runs WHERE apply_id = '01HWARD' AND sid = 'host-a'
	`).Scan(&attempt, &claimByKID, &claimAt, &claimExpiresAt); err != nil {
		t.Fatalf("select ward-claim columns: %v", err)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0 (DEFAULT for old-path row)", attempt)
	}
	if claimByKID != nil || claimAt != nil || claimExpiresAt != nil {
		t.Errorf("claim_* must be NULL on old-path row: %v / %v / %v",
			claimByKID, claimAt, claimExpiresAt)
	}

	// CHECK accepts planned/claimed (raw SQL: ValidStatus intentionally doesn't
	// pass them in Phase 0, so we insert bypassing CRUD).
	for _, st := range []string{"planned", "claimed"} {
		if _, err := integrationPool.Exec(ctx, `
			INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
			VALUES ($1, 'host-x', 'redis-prod', 'create', $2)
		`, "01HWARD-"+st, st); err != nil {
			t.Errorf("status %q rejected by CHECK after 025: %v", st, err)
		}
	}
	// Old values are preserved: cancelled (024 era) is still valid.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
		VALUES ('01HWARD-cancelled', 'host-y', 'redis-prod', 'create', 'cancelled')
	`); err != nil {
		t.Errorf("status 'cancelled' rejected after 025 (regression): %v", err)
	}
	// Invalid status is still rejected.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
		VALUES ('01HWARD-bad', 'host-z', 'redis-prod', 'create', 'bogus')
	`); err == nil {
		t.Error("status 'bogus' accepted, expected CHECK violation")
	}

	// Partial index for claim scan exists.
	var idxExists bool
	if err := integrationPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = 'apply_runs_claim_scan_idx'
		)
	`).Scan(&idxExists); err != nil {
		t.Fatalf("pg_indexes claim_scan: %v", err)
	}
	if !idxExists {
		t.Error("apply_runs_claim_scan_idx is absent after 025")
	}

	// Current path is intact: Insert/SelectByApplyID work as before, and the
	// existing row is not affected by Ward-claim columns.
	got, err := SelectByApplyID(ctx, integrationPool, "01HWARD", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID after 025: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running (current path unchanged)", got.Status)
	}
}

// seedApplyRun inserts a minimal apply_runs row (FK parent for apply_task_register).
func seedApplyRun(t *testing.T, applyID, sid string) {
	t.Helper()
	if err := Insert(context.Background(), integrationPool, &ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("seedApplyRun(%s,%s): %v", applyID, sid, err)
	}
}

func TestIntegration_TaskRegister_UpsertAndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedApplyRun(t, "01HREG", "host-a")
	seedApplyRun(t, "01HREG", "host-b")

	// PlanIndex is the correlation key (PK component, migration 079); N=1 -> ==TaskIdx.
	rows := []*TaskRegister{
		{ApplyID: "01HREG", SID: "host-a", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "a0", "rc": float64(0)}},
		{ApplyID: "01HREG", SID: "host-a", PlanIndex: 2, TaskIdx: 2, RegisterData: map[string]any{"stdout": "a2"}},
		{ApplyID: "01HREG", SID: "host-b", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "b0"}},
	}
	for _, r := range rows {
		if err := UpsertTaskRegister(ctx, integrationPool, r); err != nil {
			t.Fatalf("UpsertTaskRegister(%s,%d): %v", r.SID, r.PlanIndex, err)
		}
	}

	got, err := SelectTaskRegistersByApplyID(ctx, integrationPool, "01HREG")
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Sort order (sid, plan_index): host-a/0, host-a/2, host-b/0.
	if got[0].SID != "host-a" || got[0].PlanIndex != 0 {
		t.Errorf("got[0] = %s/%d, want host-a/0", got[0].SID, got[0].PlanIndex)
	}
	if got[1].SID != "host-a" || got[1].PlanIndex != 2 {
		t.Errorf("got[1] = %s/%d, want host-a/2", got[1].SID, got[1].PlanIndex)
	}
	if got[2].SID != "host-b" || got[2].PlanIndex != 0 {
		t.Errorf("got[2] = %s/%d, want host-b/0", got[2].SID, got[2].PlanIndex)
	}
	if got[0].RegisterData["stdout"] != "a0" {
		t.Errorf("got[0].stdout = %v, want a0", got[0].RegisterData["stdout"])
	}
	// jsonb number is read as float64.
	if got[0].RegisterData["rc"] != float64(0) {
		t.Errorf("got[0].rc = %T(%v), want float64(0)", got[0].RegisterData["rc"], got[0].RegisterData["rc"])
	}
}

func TestIntegration_TaskRegister_UpsertOverwrites(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	seedApplyRun(t, "01HREG", "host-a")

	if err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HREG", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "first"},
	}); err != nil {
		t.Fatalf("Upsert#1: %v", err)
	}
	if err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HREG", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "second"},
	}); err != nil {
		t.Fatalf("Upsert#2: %v", err)
	}

	got, err := SelectTaskRegistersByApplyID(ctx, integrationPool, "01HREG")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (upsert by PK)", len(got))
	}
	if got[0].RegisterData["stdout"] != "second" {
		t.Errorf("stdout = %v, want second (last wins)", got[0].RegisterData["stdout"])
	}
}

func TestIntegration_TaskRegister_FKCascadeOnApplyRunDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	seedApplyRun(t, "01HREG", "host-a")

	if err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HREG", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "x"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Deleting the parent apply_runs row cascades and clears register.
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM apply_runs WHERE apply_id = '01HREG' AND sid = 'host-a'`); err != nil {
		t.Fatalf("DELETE apply_runs: %v", err)
	}
	got, err := SelectTaskRegistersByApplyID(ctx, integrationPool, "01HREG")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 after parent CASCADE delete", len(got))
	}
}

func TestIntegration_TaskRegister_FKViolation_NoApplyRun(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// No apply_runs parent: FK violation.
	err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HGHOST", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "x"},
	})
	if err == nil {
		t.Fatal("UpsertTaskRegister without apply_runs parent: expected FK violation")
	}
}

// seedPlanned inserts an apply_runs row in planned status (work-queue input,
// ADR-027): scenario-runner writes planned on dispatch, and Acolyte claims it via
// ClaimNext.
func seedPlanned(t *testing.T, applyID, sid string) {
	t.Helper()
	if err := Insert(context.Background(), integrationPool, &ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusPlanned,
	}); err != nil {
		t.Fatalf("seedPlanned(%s,%s): %v", applyID, sid, err)
	}
}

// TestIntegration_ValidStatus_PlannedClaimedNowValid — Phase 1: planned/claimed
// became valid for the CRUD layer ([Insert]/[UpdateStatus]); however the old path
// (direct Insert(running)) is not broken.
func TestIntegration_ValidStatus_PlannedClaimedNowValid(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// planned now passes Insert Go validation.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HVS", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusPlanned,
	}); err != nil {
		t.Fatalf("Insert(planned): %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HVS", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusPlanned {
		t.Errorf("status = %q, want planned", got.Status)
	}

	// Old path is untouched: Insert(running) still works.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HVS", SID: "host-b", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert(running) legacy path broken: %v", err)
	}
	gotB, err := SelectByApplyID(ctx, integrationPool, "01HVS", "host-b")
	if err != nil {
		t.Fatalf("SelectByApplyID host-b: %v", err)
	}
	if gotB.Status != StatusRunning {
		t.Errorf("status = %q, want running (legacy path)", gotB.Status)
	}
}

// TestIntegration_ClaimNext_PlannedToClaimed verifies basic claim: planned ->
// claimed, claim_by_kid/claim_at/claim_expires_at set, attempt 0->1.
func TestIntegration_ClaimNext_PlannedToClaimed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedPlanned(t, "01HCLAIM", "host-a")

	claimed, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed len = %d, want 1", len(claimed))
	}
	c := claimed[0]
	if c.Status != StatusClaimed {
		t.Errorf("status = %q, want claimed", c.Status)
	}
	if c.ClaimByKID == nil || *c.ClaimByKID != "keeper-1" {
		t.Errorf("claim_by_kid = %v, want keeper-1", c.ClaimByKID)
	}
	if c.ClaimAt == nil {
		t.Errorf("claim_at nil, want set")
	}
	if c.ClaimExpiresAt == nil {
		t.Errorf("claim_expires_at nil, want set")
	}
	if c.ClaimAt != nil && c.ClaimExpiresAt != nil && !c.ClaimExpiresAt.After(*c.ClaimAt) {
		t.Errorf("claim_expires_at %v is not after claim_at %v", c.ClaimExpiresAt, c.ClaimAt)
	}
	if c.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (0→1)", c.Attempt)
	}

	// Persistence: repeat SELECT sees claimed.
	got, err := SelectByApplyID(ctx, integrationPool, "01HCLAIM", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusClaimed {
		t.Errorf("persisted status = %q, want claimed", got.Status)
	}
}

// TestIntegration_ClaimNext_NoPlanned verifies no planned jobs -> empty slice, no error.
func TestIntegration_ClaimNext_NoPlanned(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// Noise running row must not be claimed (planned only).
	seedApplyRun(t, "01HRUN", "host-a")

	claimed, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed len = %d, want 0 (no planned)", len(claimed))
	}
}

// TestIntegration_ClaimNext_BatchLimit verifies claiming no more than batch rows.
func TestIntegration_ClaimNext_BatchLimit(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b", "host-c", "host-d", "host-e"} {
		seedPlanned(t, "01HBATCH", sid)
	}

	first, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ClaimNext#1: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first batch len = %d, want 2 (limit)", len(first))
	}

	// Remainder (3 planned) is claimed by next claims with the same limit 2: 2, then 1.
	second, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ClaimNext#2: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("second batch len = %d, want 2", len(second))
	}
	third, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ClaimNext#3: %v", err)
	}
	if len(third) != 1 {
		t.Fatalf("third batch len = %d, want 1 (remainder)", len(third))
	}
}

// TestIntegration_ClaimNext_Concurrent is the key correctness test: two parallel
// ClaimNext calls (different Acolytes / KIDs) over a shared planned set must not
// receive the same row. FOR UPDATE SKIP LOCKED guarantees this. Union of claimed
// sets equals the full set, intersection is empty.
func TestIntegration_ClaimNext_Concurrent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	const n = 40
	for i := 0; i < n; i++ {
		seedPlanned(t, "01HCONC", "host-"+strconv.Itoa(i))
	}

	type res struct {
		runs []*ApplyRun
		err  error
	}
	var (
		wg      sync.WaitGroup
		results [2]res
	)
	kids := [2]string{"keeper-1", "keeper-2"}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Each worker claims in batches until planned rows run out.
			for {
				batch, err := ClaimNext(ctx, integrationPool, kids[idx], 60*time.Second, 5)
				if err != nil {
					results[idx].err = err
					return
				}
				if len(batch) == 0 {
					return
				}
				results[idx].runs = append(results[idx].runs, batch...)
			}
		}(w)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("worker %d: %v", i, r.err)
		}
	}

	// No row is claimed twice; union covers the whole set.
	seen := make(map[string]string) // sid -> kid that claimed it
	total := 0
	for i, r := range results {
		for _, run := range r.runs {
			total++
			if prev, dup := seen[run.SID]; dup {
				t.Fatalf("row sid=%s claimed twice: kid=%s and kid=%s (FOR UPDATE SKIP LOCKED broken)",
					run.SID, prev, kids[i])
			}
			seen[run.SID] = kids[i]
			// Claimed row carries its KID and attempt=1.
			if run.ClaimByKID == nil || *run.ClaimByKID != kids[i] {
				t.Errorf("sid=%s claim_by_kid=%v, want %s", run.SID, run.ClaimByKID, kids[i])
			}
			if run.Attempt != 1 {
				t.Errorf("sid=%s attempt=%d, want 1", run.SID, run.Attempt)
			}
		}
	}
	if total != n {
		t.Errorf("total claimed %d, want %d (some planned rows lost)", total, n)
	}
	if len(seen) != n {
		t.Errorf("unique claimed rows %d, want %d", len(seen), n)
	}
}

// TestIntegration_ClaimNext_AttemptIncrements tests that attempt increments on
// repeat claim. Emulate recovery manually: claimed -> planned reset, then claim
// again -> attempt 1->2.
func TestIntegration_ClaimNext_AttemptIncrements(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedPlanned(t, "01HEPOCH", "host-a")

	first, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#1: %v", err)
	}
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("first claim attempt = %v, want 1", first)
	}

	// Recovery emulation: expired Ward is returned to planned (claim_* reset,
	// attempt preserved; Reaper recovery scan does not touch attempt, ADR-027(i)).
	if _, err := integrationPool.Exec(ctx, `
		UPDATE apply_runs
		SET status='planned', claim_by_kid=NULL, claim_at=NULL, claim_expires_at=NULL
		WHERE apply_id='01HEPOCH' AND sid='host-a'`); err != nil {
		t.Fatalf("recovery reset: %v", err)
	}

	second, err := ClaimNext(ctx, integrationPool, "keeper-2", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#2: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second claim len = %d, want 1", len(second))
	}
	if second[0].Attempt != 2 {
		t.Errorf("attempt = %d, want 2 (1→2 on reclaim)", second[0].Attempt)
	}
	if second[0].ClaimByKID == nil || *second[0].ClaimByKID != "keeper-2" {
		t.Errorf("claim_by_kid = %v, want keeper-2 (new owner after recovery)", second[0].ClaimByKID)
	}
}

// TestIntegration_ClaimNext_Validation verifies empty kid / non-positive lease/batch
// are rejected before going to DB.
func TestIntegration_ClaimNext_Validation(t *testing.T) {
	ctx := context.Background()
	if _, err := ClaimNext(ctx, integrationPool, "", time.Second, 1); err == nil {
		t.Error("empty kid: expected error")
	}
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 0, 1); err == nil {
		t.Error("zero lease: expected error")
	}
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", time.Second, 0); err == nil {
		t.Error("zero batch: expected error")
	}
}

// TestIntegration_MarkDispatched_Ok verifies claimed -> dispatched succeeds.
func TestIntegration_MarkDispatched_Ok(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedPlanned(t, "01HMARK", "host-a")
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if err := MarkDispatched(ctx, integrationPool, "01HMARK", "host-a"); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HMARK", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched", got.Status)
	}
}

// TestIntegration_MarkDispatched_GuardRejectsNonClaimed verifies the guard:
// transition is possible only from claimed. running->dispatched and
// planned->dispatched are rejected (ErrApplyRunNotClaimed), absent row ->
// ErrApplyRunNotFound.
func TestIntegration_MarkDispatched_GuardRejectsNonClaimed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// running -> dispatched: guard (running outside claimed).
	seedApplyRun(t, "01HG", "host-running") // Inserts StatusRunning.
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-running"); !errors.Is(err, ErrApplyRunNotClaimed) {
		t.Errorf("running→dispatched: err = %v, want ErrApplyRunNotClaimed", err)
	}

	// planned → dispatched: guard.
	seedPlanned(t, "01HG", "host-planned")
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-planned"); !errors.Is(err, ErrApplyRunNotClaimed) {
		t.Errorf("planned→dispatched: err = %v, want ErrApplyRunNotClaimed", err)
	}

	// Absent row -> NotFound.
	if err := MarkDispatched(ctx, integrationPool, "01HG", "ghost"); !errors.Is(err, ErrApplyRunNotFound) {
		t.Errorf("ghost: err = %v, want ErrApplyRunNotFound", err)
	}

	// Repeated MarkDispatched after a successful transition (already dispatched)
	// is also guarded: idempotency means the second call does not reconfirm it.
	seedPlanned(t, "01HG", "host-twice")
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-twice"); err != nil {
		t.Fatalf("MarkDispatched#1: %v", err)
	}
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-twice"); !errors.Is(err, ErrApplyRunNotClaimed) {
		t.Errorf("repeated MarkDispatched: err = %v, want ErrApplyRunNotClaimed", err)
	}
}

// TestIntegration_InsertPlanned_WithRecipe verifies Phase 1.4.2: InsertPlanned
// writes a status=planned row with persisted recipe (column 029), attempt=0
// DEFAULT, Ward columns NULL. Invariant A: recipe.Input carries vault-ref as string.
func TestIntegration_InsertPlanned_WithRecipe(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	aid := "archon-alice"
	recipe := &Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "redis", Git: "https://example.test/redis.git", Ref: "main"},
		ScenarioName: "create",
		Input:        map[string]any{"db_password": "vault:secret/db-creds#password"},
		StartedByAID: &aid,
	}
	run := &ApplyRun{
		ApplyID: "01HPLAN", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", StartedByAID: &aid, Recipe: recipe,
	}
	if err := InsertPlanned(ctx, integrationPool, run); err != nil {
		t.Fatalf("InsertPlanned: %v", err)
	}
	if run.Status != StatusPlanned {
		t.Errorf("run.Status = %q, want planned", run.Status)
	}
	if run.StartedAt.IsZero() {
		t.Errorf("StartedAt not filled by RETURNING")
	}

	got, err := SelectByApplyID(ctx, integrationPool, "01HPLAN", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusPlanned {
		t.Errorf("persisted status = %q, want planned", got.Status)
	}
	if got.Attempt != 0 {
		t.Errorf("attempt = %d, want 0 (DEFAULT, incremented by claim)", got.Attempt)
	}
	if got.ClaimByKID != nil {
		t.Errorf("claim_by_kid = %v, want NULL before claim", got.ClaimByKID)
	}

	// recipe travels through claim: ClaimNext.RETURNING carries recipe -> run.Recipe.
	claimed, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed len = %d, want 1", len(claimed))
	}
	c := claimed[0]
	if c.Recipe == nil {
		t.Fatalf("claimed.Recipe nil: recipe did not travel through ClaimNext")
	}
	if c.Recipe.ScenarioName != "create" {
		t.Errorf("recipe.ScenarioName = %q, want create", c.Recipe.ScenarioName)
	}
	// Invariant A: vault-ref in persisted data is a string; secret is not revealed.
	if c.Recipe.Input["db_password"] != "vault:secret/db-creds#password" {
		t.Errorf("recipe.Input db_password = %v, want vault-ref as-is", c.Recipe.Input["db_password"])
	}
	if c.Recipe.StartedByAID == nil || *c.Recipe.StartedByAID != aid {
		t.Errorf("recipe.StartedByAID = %v, want %q", c.Recipe.StartedByAID, aid)
	}
}

// TestIntegration_InsertPlanned_RejectsNilRecipe verifies that a planned job
// without recipe cannot be rendered by Acolyte, so InsertPlanned rejects nil recipe.
func TestIntegration_InsertPlanned_RejectsNilRecipe(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	err := InsertPlanned(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HNIL", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", // Recipe is not set.
	})
	if err == nil {
		t.Fatalf("InsertPlanned with nil recipe succeeded, want error")
	}
}

// dispatchRow is a helper: planned -> claimed -> dispatched for (applyID, sid).
// Returns the claimed row attempt (fencing epoch after ClaimNext).
func dispatchRow(t *testing.T, applyID, sid string) int {
	t.Helper()
	ctx := context.Background()
	seedPlanned(t, applyID, sid)
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10); err != nil {
		t.Fatalf("ClaimNext(%s/%s): %v", applyID, sid, err)
	}
	if err := MarkDispatched(ctx, integrationPool, applyID, sid); err != nil {
		t.Fatalf("MarkDispatched(%s/%s): %v", applyID, sid, err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, applyID, sid)
	if err != nil {
		t.Fatalf("SelectByApplyID(%s/%s): %v", applyID, sid, err)
	}
	return got.Attempt
}

// TestIntegration_OrphanDispatched_SweepsAbsent - Soul-reconcile (ADR-027(g),
// S6) on real PG: a dispatched row for a SID whose apply_id is NOT in WardRoster
// becomes orphaned (with finished_at + error_summary); an in-set row is NOT
// touched; a row for another SID is not touched (per-SID sweep).
func TestIntegration_OrphanDispatched_SweepsAbsent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	dispatchRow(t, "01HLIVE", "host-a")  // declared live -> leave untouched
	dispatchRow(t, "01HDEAD", "host-a")  // outside the set -> orphaned
	dispatchRow(t, "01HOTHER", "host-b") // another SID -> leave untouched

	n, err := OrphanDispatched(ctx, integrationPool, "host-a", []*ActiveApply{{ApplyID: "01HLIVE"}})
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 1 {
		t.Fatalf("orphaned count = %d, want 1 (only 01HDEAD/host-a)", n)
	}

	dead, err := SelectByApplyID(ctx, integrationPool, "01HDEAD", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID dead: %v", err)
	}
	if dead.Status != StatusOrphaned {
		t.Errorf("01HDEAD status = %q, want orphaned", dead.Status)
	}
	if dead.FinishedAt == nil {
		t.Error("01HDEAD finished_at not set")
	}
	if dead.ErrorSummary == nil || *dead.ErrorSummary != orphanDispatchedErrorSummary {
		t.Errorf("01HDEAD error_summary = %v, want fixed orphaned marker", dead.ErrorSummary)
	}

	// Declared live - stays dispatched.
	live, err := SelectByApplyID(ctx, integrationPool, "01HLIVE", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID live: %v", err)
	}
	if live.Status != StatusDispatched {
		t.Errorf("01HLIVE status = %q, want dispatched (in the set)", live.Status)
	}

	// Another SID - not touched (per-SID sweep).
	other, err := SelectByApplyID(ctx, integrationPool, "01HOTHER", "host-b")
	if err != nil {
		t.Fatalf("SelectByApplyID other: %v", err)
	}
	if other.Status != StatusDispatched {
		t.Errorf("01HOTHER status = %q, want dispatched (another SID)", other.Status)
	}
}

// TestIntegration_OrphanDispatched_EpochDivergence_NotOrphaned - attempt divergence:
// the set carries the same apply_id as the dispatched row, but with a DIFFERENT
// attempt (reclaim in flight). The row is NOT terminalized - presence of apply_id
// in the set (with any attempt) protects it from orphan (epoch-fenced: safer not to orphan).
func TestIntegration_OrphanDispatched_EpochDivergence_NotOrphaned(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	attempt := dispatchRow(t, "01HEPOCH", "host-a")

	// Soul reported the same apply_id, but with a higher attempt (re-claim in flight).
	n, err := OrphanDispatched(ctx, integrationPool, "host-a", []*ActiveApply{
		{ApplyID: "01HEPOCH", Attempt: int32(attempt + 5)},
	})
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 0 {
		t.Fatalf("orphaned count = %d, want 0 (apply_id in the set protects from orphan)", n)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HEPOCH", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched (attempt divergence does not terminalize)", got.Status)
	}
}

// TestIntegration_OrphanDispatched_EmptySet_OrphansAll - empty WardRoster
// (Soul restart): all dispatched rows for the SID are terminalized.
func TestIntegration_OrphanDispatched_EmptySet_OrphansAll(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	dispatchRow(t, "01HA", "host-a")
	dispatchRow(t, "01HB", "host-a")

	n, err := OrphanDispatched(ctx, integrationPool, "host-a", nil)
	if err != nil {
		t.Fatalf("OrphanDispatched(nil): %v", err)
	}
	if n != 2 {
		t.Fatalf("orphaned count = %d, want 2 (empty set -> all dispatched)", n)
	}
	for _, id := range []string{"01HA", "01HB"} {
		got, err := SelectByApplyID(ctx, integrationPool, id, "host-a")
		if err != nil {
			t.Fatalf("SelectByApplyID %s: %v", id, err)
		}
		if got.Status != StatusOrphaned {
			t.Errorf("%s status = %q, want orphaned", id, got.Status)
		}
	}
}

// TestIntegration_OrphanDispatched_SingleWinnerVsRunResult - sweep <-> RunResult race
// through single-winner: terminal result from RunResult (UpdateStatus dispatched->success)
// won -> subsequent sweep for the same row returns 0 (it is no longer dispatched).
func TestIntegration_OrphanDispatched_SingleWinnerVsRunResult(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	dispatchRow(t, "01HRACE", "host-a")

	// RunResult arrived first: dispatched → success.
	if err := UpdateStatus(ctx, integrationPool, "01HRACE", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus(success): %v", err)
	}

	// Sweep with an empty set does NOT overwrite an already-terminal row (the
	// status='dispatched' filter excludes it) - 0 affected.
	n, err := OrphanDispatched(ctx, integrationPool, "host-a", nil)
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 0 {
		t.Fatalf("orphaned count = %d, want 0 (RunResult terminal won, single-winner)", n)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HRACE", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("status = %q, want success (not overwritten by orphaned)", got.Status)
	}
}

// TestIntegration_OrphanedStatus_CHECKAllows - migration 044: CHECK constraint
// apply_runs_status_valid allows orphaned (direct Insert succeeds). Symmetric to
// TestIntegration_ValidStatus_PlannedClaimedNowValid for 040.
func TestIntegration_OrphanedStatus_CHECKAllows(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HORPH", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusOrphaned,
	}); err != nil {
		t.Fatalf("Insert(orphaned) - CHECK does not allow orphaned after 044: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HORPH", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusOrphaned {
		t.Errorf("status = %q, want orphaned", got.Status)
	}
}

// TestIntegration_NoMatchStatus_CHECKAllows - migration 045 (FINDING-01 variant
// (b)): apply_runs_status_valid allows no_match (direct Insert succeeds).
// Symmetric to TestIntegration_OrphanedStatus_CHECKAllows for 044.
func TestIntegration_NoMatchStatus_CHECKAllows(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HNOMATCH", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusNoMatch,
	}); err != nil {
		t.Fatalf("Insert(no_match) - CHECK does not allow no_match after 045: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HNOMATCH", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusNoMatch {
		t.Errorf("status = %q, want no_match", got.Status)
	}
}

// TestIntegration_UpdateStatus_NoMatchSetsFinishedAt - claim no-op path for FINDING-01
// (variant (b)): a non-target roster host is moved planned/claimed -> no_match
// through UpdateStatus, which sets finished_at (no_match is terminal, not
// running). Without finished_at, no_match rows would not match purge_apply_runs
// and would accumulate forever.
func TestIntegration_UpdateStatus_NoMatchSetsFinishedAt(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// planned row (as dispatchPlanned writes it for EACH roster host).
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HNM", SID: "host-x", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusPlanned,
	}); err != nil {
		t.Fatalf("Insert(planned): %v", err)
	}
	// claim no-op: on:/where: filtered everything -> no_match (NOT success).
	if err := UpdateStatus(ctx, integrationPool, "01HNM", "host-x", 0, StatusNoMatch, nil); err != nil {
		t.Fatalf("UpdateStatus(no_match): %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HNM", "host-x")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusNoMatch {
		t.Errorf("status = %q, want no_match", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("finished_at nil after transition to no_match; want set (otherwise the row is not purged)")
	}
}

// --- run read-view (GET /v1/incarnations/{name}/runs[/{apply_id}]) ---

// TestIntegration_ListRunsByIncarnation - folds apply_runs by apply_id: list of
// incarnation runs with aggregate status, time bounds, and exclusion of runs from
// ANOTHER incarnation.
func TestIntegration_ListRunsByIncarnation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"

	// Run A: two hosts, both success -> success, finished_at set.
	for _, sid := range []string{"host-a", "host-b"} {
		mustInsertRun(t, ctx, "01HRUNA", sid, "redis-prod", "create", StatusPlanned, &aid)
		if err := UpdateStatus(ctx, integrationPool, "01HRUNA", sid, 0, StatusSuccess, nil); err != nil {
			t.Fatalf("UpdateStatus A/%s: %v", sid, err)
		}
	}
	// Run B: one host success, the other still running -> applying, finished_at NULL.
	mustInsertRun(t, ctx, "01HRUNB", "host-a", "redis-prod", "restart", StatusRunning, &aid)
	mustInsertRun(t, ctx, "01HRUNB", "host-b", "redis-prod", "restart", StatusRunning, &aid)
	if err := UpdateStatus(ctx, integrationPool, "01HRUNB", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus B/host-a: %v", err)
	}
	// Run C: another incarnation - excluded from the redis-prod selection.
	mustInsertRun(t, ctx, "01HRUNC", "host-z", "redis-staging", "create", StatusSuccess, &aid)

	runs, total, err := ListRunsByIncarnation(ctx, integrationPool, "redis-prod", 0, 50)
	if err != nil {
		t.Fatalf("ListRunsByIncarnation: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2 (other incarnation excluded)", total)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	// ORDER BY MIN(started_at) DESC - B (inserted later) first.
	byID := map[string]RunSummary{runs[0].ApplyID: runs[0], runs[1].ApplyID: runs[1]}
	a, okA := byID["01HRUNA"]
	b, okB := byID["01HRUNB"]
	if !okA || !okB {
		t.Fatalf("expected apply_id 01HRUNA and 01HRUNB; got %+v", runs)
	}
	if a.Status != RunStatusSuccess {
		t.Errorf("A.Status = %q, want success", a.Status)
	}
	if a.Scenario != "create" {
		t.Errorf("A.Scenario = %q, want create", a.Scenario)
	}
	if a.FinishedAt == nil {
		t.Error("A.FinishedAt nil, want set (all hosts finished)")
	}
	if a.StartedByAID == nil || *a.StartedByAID != aid {
		t.Errorf("A.StartedByAID = %v, want %q", a.StartedByAID, aid)
	}
	if b.Status != RunStatusApplying {
		t.Errorf("B.Status = %q, want applying (host-b is still running)", b.Status)
	}
	if b.FinishedAt != nil {
		t.Errorf("B.FinishedAt = %v, want nil (not all hosts finished)", b.FinishedAt)
	}
}

// TestIntegration_ListRunsByIncarnation_Empty - incarnation without runs.
func TestIntegration_ListRunsByIncarnation_Empty(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	runs, total, err := ListRunsByIncarnation(ctx, integrationPool, "redis-prod", 0, 50)
	if err != nil {
		t.Fatalf("ListRunsByIncarnation: %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("total=%d len=%d, want 0/0", total, len(runs))
	}
}

// TestIntegration_ListRunsByIncarnation_Paging - total counts ALL runs,
// page is limited by limit/offset.
func TestIntegration_ListRunsByIncarnation_Paging(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"

	for _, id := range []string{"01HR01", "01HR02", "01HR03"} {
		mustInsertRun(t, ctx, id, "host-a", "redis-prod", "create", StatusSuccess, &aid)
	}
	runs, total, err := ListRunsByIncarnation(ctx, integrationPool, "redis-prod", 0, 2)
	if err != nil {
		t.Fatalf("ListRunsByIncarnation: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(runs) != 2 {
		t.Errorf("len(runs) = %d, want 2 (limit)", len(runs))
	}
}

// TestIntegration_SelectRunDetail - details of a single run: per-host rows,
// failed task address, aggregate failed status.
func TestIntegration_SelectRunDetail(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"

	mustInsertRun(t, ctx, "01HDET", "host-a", "redis-prod", "scale", StatusRunning, &aid)
	mustInsertRun(t, ctx, "01HDET", "host-b", "redis-prod", "scale", StatusRunning, &aid)
	// host-a: failed task (task_idx=2 locally, plan_index=5 globally).
	if err := RecordTaskFailure(ctx, integrationPool, "01HDET", "host-a", 0, 2, 5, "task 2 core.pkg.installed: boom"); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "01HDET", "host-a", 0, StatusFailed, strp("task 2 core.pkg.installed: boom")); err != nil {
		t.Fatalf("UpdateStatus host-a: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "01HDET", "host-b", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus host-b: %v", err)
	}

	d, err := SelectRunDetail(ctx, integrationPool, "01HDET", "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if d.ApplyID != "01HDET" || d.Scenario != "scale" {
		t.Errorf("d = {%q, %q}, want {01HDET, scale}", d.ApplyID, d.Scenario)
	}
	if d.Status != RunStatusFailed {
		t.Errorf("d.Status = %q, want failed", d.Status)
	}
	if d.FinishedAt == nil {
		t.Error("d.FinishedAt nil, want set (both hosts are terminal)")
	}
	if len(d.Hosts) != 2 {
		t.Fatalf("len(Hosts) = %d, want 2", len(d.Hosts))
	}
	// ORDER BY sid ASC -> host-a first.
	ha := d.Hosts[0]
	if ha.SID != "host-a" || ha.Status != StatusFailed {
		t.Errorf("Hosts[0] = {%q, %q}, want {host-a, failed}", ha.SID, ha.Status)
	}
	if ha.FailedTaskIdx == nil || *ha.FailedTaskIdx != 2 {
		t.Errorf("Hosts[0].FailedTaskIdx = %v, want 2", ha.FailedTaskIdx)
	}
	if ha.FailedPlanIndex == nil || *ha.FailedPlanIndex != 5 {
		t.Errorf("Hosts[0].FailedPlanIndex = %v, want 5", ha.FailedPlanIndex)
	}
	if ha.ErrorSummary == nil || *ha.ErrorSummary != "task 2 core.pkg.installed: boom" {
		t.Errorf("Hosts[0].ErrorSummary = %v", ha.ErrorSummary)
	}
	hb := d.Hosts[1]
	if hb.SID != "host-b" || hb.Status != StatusSuccess {
		t.Errorf("Hosts[1] = {%q, %q}, want {host-b, success}", hb.SID, hb.Status)
	}
	if hb.FailedTaskIdx != nil {
		t.Errorf("Hosts[1].FailedTaskIdx = %v, want nil (success)", hb.FailedTaskIdx)
	}
}

// TestIntegration_SelectRunDetail_CrossIncarnationIsolation - apply_id that lives in
// ANOTHER incarnation is unavailable through the first one's detail (scope invariant:
// apply_id is not resolved around the incarnation predicate).
func TestIntegration_SelectRunDetail_CrossIncarnationIsolation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"

	mustInsertRun(t, ctx, "01HXINC", "host-a", "redis-staging", "create", StatusSuccess, &aid)

	// Run exists, but belongs to redis-staging — from redis-prod not-found.
	_, err := SelectRunDetail(ctx, integrationPool, "01HXINC", "redis-prod")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound (cross-incarnation)", err)
	}
	// Available from its own incarnation.
	if _, err := SelectRunDetail(ctx, integrationPool, "01HXINC", "redis-staging"); err != nil {
		t.Fatalf("SelectRunDetail(own): %v", err)
	}
}

// TestIntegration_SelectRunDetail_NotFound - unknown apply_id.
func TestIntegration_SelectRunDetail_NotFound(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	_, err := SelectRunDetail(ctx, integrationPool, "01HGHOST", "redis-prod")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

// mustInsertRun is a helper: Insert one host row of a run, fatal on error.
func mustInsertRun(t *testing.T, ctx context.Context, applyID, sid, inc, scenario string, status Status, startedBy *string) {
	t.Helper()
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: inc,
		Scenario: scenario, Status: status, StartedByAID: startedBy,
	}); err != nil {
		t.Fatalf("Insert(%s/%s): %v", applyID, sid, err)
	}
}
