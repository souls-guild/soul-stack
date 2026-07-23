package handlers

// Guard tests for the incarnation runs read view at the handler boundary (RunsTyped /
// RunDetailTyped): scope gate (deny/nil-scoper/cross-incarnation → 404), input
// validation (bad name → 422, bad apply_id → 400) and happy-path (empty list + per-host
// projection store→View). The exact per-task mapping from a real apply_runs row
// (task_idx/plan_index/error) is covered by the integration test applyrun.SelectRunDetail —
// here we test the handler layer specifically: domain function + inScope predicate + projection.
//
// The domain functions take the inScope predicate DIRECTLY (the same one the huma layer
// assembles via GetInScopeFor(claims, "history")); we test them without the HTTP wrapper.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// allowScope / denyScope — inScope predicates for the scope gate (replacing GetInScopeFor
// in a direct domain-function call). Unrestricted operator → allow, out-of-scope →
// deny (the handler returns 404, does not leak existence).
func allowScope(*incarnation.Incarnation) bool { return true }
func denyScope(*incarnation.Incarnation) bool  { return false }

// validApplyID — a syntactically valid ULID (26 Crockford-base32 characters,
// alphabet 0-9A-HJKMNP-TV-Z without I/L/O/U) for paths where apply_id passes IsValidULID.
const validApplyID = "01HZZZ00000000000000000000"

// --- RunsTyped (list of runs) --------------------------------------

func TestRunsTyped_BadName_422(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "Bad_Name", 0, 50, allowScope)
	requireProblemStatus(t, err, 422)
}

func TestRunsTyped_BadLimit_400(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "redis-prod", 0, 99999, allowScope)
	requireProblemStatus(t, err, 400)
}

// TestRunsTyped_OutOfScope_404 — the incarnation exists, but inScope denies →
// 404 (the existence probe does not leak someone else's incarnation as 403).
func TestRunsTyped_OutOfScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "redis-prod", 0, 50, denyScope)
	requireProblemStatus(t, err, 404)
}

// TestRunsTyped_NilScope_404 — nil inScope (fail-closed mis-wire-up) → 404, store
// untouched.
func TestRunsTyped_NilScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "redis-prod", 0, 50, nil)
	requireProblemStatus(t, err, 404)
}

// TestRunsTyped_IncarnationNotFound_404 — the incarnation is absent (SelectByName → ErrNoRows)
// → 404 already at the existence probe, before touching apply_runs.
func TestRunsTyped_IncarnationNotFound_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "ghost", 0, 50, allowScope)
	requireProblemStatus(t, err, 404)
}

// TestRunsTyped_Empty_OK — the incarnation is in scope, no runs: success (nil error),
// empty list, Total=0. Proves that after the scope gate RunsTyped reaches the
// store and correctly projects an empty set.
func TestRunsTyped_Empty_OK(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		// COUNT(DISTINCT apply_id)→0 + apply_runs Query→emptyRows (default fake).
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	reply, err := h.RunsTyped(context.Background(), "redis-prod", 0, 50, allowScope)
	if err != nil {
		t.Fatalf("RunsTyped: %v", err)
	}
	if reply.Total != 0 {
		t.Errorf("Total = %d, want 0", reply.Total)
	}
	if len(reply.Items) != 0 {
		t.Errorf("len(Items) = %d, want 0", len(reply.Items))
	}
	if reply.Limit != 50 {
		t.Errorf("Limit = %d, want 50", reply.Limit)
	}
}

// --- RunDetailTyped (run detail) ----------------------------------

func TestRunDetailTyped_BadName_422(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "Bad_Name", validApplyID, allowScope)
	requireProblemStatus(t, err, 422)
}

// TestRunDetailTyped_BadApplyID_400 — a non-ULID apply_id is rejected with 400 BEFORE
// the existence probe (validation before the store).
func TestRunDetailTyped_BadApplyID_400(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "redis-prod", "not-a-ulid", allowScope)
	requireProblemStatus(t, err, 400)
}

// TestRunDetailTyped_OutOfScope_404 — the incarnation exists, inScope denies → 404
// (store untouched, the existence probe rejected it).
func TestRunDetailTyped_OutOfScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "redis-prod", validApplyID, denyScope)
	requireProblemStatus(t, err, 404)
}

// TestRunDetailTyped_RunNotFound_404 — the incarnation is in scope, but apply_id does not
// belong to it (SelectRunDetail → 0 rows → ErrApplyRunNotFound): 404. Mirrors the
// cross-incarnation isolation of the store layer at the handler boundary.
func TestRunDetailTyped_RunNotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		applyRunsRows:   func() (pgx.Rows, error) { return &emptyRows{}, nil }, // 0 host rows
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	requireProblemStatus(t, err, 404)
}

// TestRunDetailTyped_PerHostMapping_OK — happy-path of the store→View projection at
// the handler boundary: two run hosts, host-a failed (task_idx/plan_index/error
// filled in), host-b succeeded (nil details). We check that RunHostStatusView carries
// per-host fields and the aggregate status is failed. The real per-task Scan from PG —
// in integration TestIntegration_SelectRunDetail; here it's the handler projection.
func TestRunDetailTyped_PerHostMapping_OK(t *testing.T) {
	failedIdx, failedPlan := 2, 5
	errSummary := "task 2 core.pkg.installed: boom"
	now := time.Now().UTC()
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		applyRunsRows: func() (pgx.Rows, error) {
			return &applyRunsHostRows{rows: []applyRunHostRow{
				{ // host-a: failed
					sid: "host-a", status: "failed", passage: 0,
					taskIdx: &failedIdx, failedPlan: &failedPlan, errorSummary: &errSummary,
					attempt: 1, cancelRequested: false,
					scenario: "scale", startedAt: now, finishedAt: &now, startedBy: strp("archon-alice"),
					input: []byte(`{"version":"7.2","db_password":"***MASKED***"}`),
				},
				{ // host-b: succeeded
					sid: "host-b", status: "success", passage: 0,
					attempt: 1, cancelRequested: false,
					scenario: "scale", startedAt: now, finishedAt: &now, startedBy: strp("archon-alice"),
				},
			}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	d, err := h.RunDetailTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunDetailTyped: %v", err)
	}
	if d.Scenario != "scale" {
		t.Errorf("Scenario = %q, want scale", d.Scenario)
	}
	if d.Status != "failed" {
		t.Errorf("Status = %q, want failed (host-a failed)", d.Status)
	}
	if len(d.Hosts) != 2 {
		t.Fatalf("len(Hosts) = %d, want 2", len(d.Hosts))
	}
	// host-a: carries the address of the failed task.
	ha := d.Hosts[0]
	if ha.SID != "host-a" || ha.Status != "failed" {
		t.Errorf("Hosts[0] = {%q,%q}, want {host-a,failed}", ha.SID, ha.Status)
	}
	if ha.FailedTaskIdx == nil || *ha.FailedTaskIdx != 2 {
		t.Errorf("Hosts[0].FailedTaskIdx = %v, want 2", ha.FailedTaskIdx)
	}
	if ha.FailedPlanIndex == nil || *ha.FailedPlanIndex != 5 {
		t.Errorf("Hosts[0].FailedPlanIndex = %v, want 5", ha.FailedPlanIndex)
	}
	if ha.ErrorSummary == nil || *ha.ErrorSummary != errSummary {
		t.Errorf("Hosts[0].ErrorSummary = %v, want %q", ha.ErrorSummary, errSummary)
	}
	// host-b: success, failed-task details nil.
	hb := d.Hosts[1]
	if hb.SID != "host-b" || hb.Status != "success" {
		t.Errorf("Hosts[1] = {%q,%q}, want {host-b,success}", hb.SID, hb.Status)
	}
	if hb.FailedTaskIdx != nil || hb.FailedPlanIndex != nil || hb.ErrorSummary != nil {
		t.Errorf("Hosts[1] carries failed-task details on success: %+v", hb)
	}
	// masked run input snapshot projects through (run-invariant, first non-null).
	if d.Input == nil {
		t.Fatal("RunDetailView.Input nil, want masked snapshot")
	}
	if d.Input["version"] != "7.2" {
		t.Errorf("Input[version] = %v, want 7.2 (non-secret intact)", d.Input["version"])
	}
	if d.Input["db_password"] != "***MASKED***" {
		t.Errorf("Input[db_password] = %v, want ***MASKED***", d.Input["db_password"])
	}
}

// requireProblemStatus checks that err is a domain *problemError with the expected
// HTTP status (problem-type → code mapping). t.Helper for a readable stack.
func requireProblemStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with status %d, got nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error is not *problemError: %v", err)
	}
	if d.Status != want {
		t.Errorf("problem status = %d, want %d (%v)", d.Status, want, err)
	}
}

// applyRunHostRow — one apply_runs host row for the detail-rows-stub (column order and
// types of selectRunHostsSQL: sid/status/passage/task_idx/failed_plan_index/
// error_summary/attempt/cancel_requested/scenario/started_at/finished_at/started_by/input).
type applyRunHostRow struct {
	sid             string
	status          string
	passage         int
	taskIdx         *int
	failedPlan      *int
	errorSummary    *string
	attempt         int32
	cancelRequested bool
	scenario        string
	startedAt       time.Time
	finishedAt      *time.Time
	startedBy       *string
	input           []byte
}

// applyRunsHostRows — a pgx.Rows stub over a set of apply_runs host rows. Scan
// supports exactly the column types of selectRunHostsSQL (incl. *int32/*bool/**int/
// **time.Time/**string, which the generic staticRow does not cover).
type applyRunsHostRows struct {
	rows []applyRunHostRow
	idx  int
}

func (r *applyRunsHostRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *applyRunsHostRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	vals := []any{
		row.sid, row.status, row.passage, row.taskIdx, row.failedPlan, row.errorSummary,
		row.attempt, row.cancelRequested, row.scenario, row.startedAt, row.finishedAt, row.startedBy,
		row.input,
	}
	for i, d := range dest {
		if err := scanApplyRunCol(d, vals[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *applyRunsHostRows) Err() error                                   { return nil }
func (r *applyRunsHostRows) Close()                                       {}
func (r *applyRunsHostRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *applyRunsHostRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *applyRunsHostRows) Values() ([]any, error)                       { return nil, nil }
func (r *applyRunsHostRows) RawValues() [][]byte                          { return nil }
func (r *applyRunsHostRows) Conn() *pgx.Conn                              { return nil }

// scanApplyRunCol assigns v to a dest pointer of the right type (a narrow set of apply_runs
// columns; an unknown type → error, so schema drift is visible).
func scanApplyRunCol(dest, v any) error {
	switch d := dest.(type) {
	case *string:
		*d = v.(string)
	case *int:
		*d = v.(int)
	case *int32:
		*d = v.(int32)
	case *bool:
		*d = v.(bool)
	case *time.Time:
		*d = v.(time.Time)
	case **int:
		*d = v.(*int)
	case **string:
		*d = v.(*string)
	case **time.Time:
		*d = v.(*time.Time)
	case *[]byte:
		if v == nil {
			*d = nil
		} else {
			*d = v.([]byte)
		}
	default:
		return errors.New("applyRunsHostRows.Scan: unsupported dest type")
	}
	return nil
}
