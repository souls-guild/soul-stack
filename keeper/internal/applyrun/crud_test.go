package applyrun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB is an ExecQueryRower stub for unit tests. Captures the last SQL and
// args, returns a configurable Row / CommandTag. Pattern matches
// keeper/internal/incarnation/crud_test.go.
type fakeDB struct {
	execCalls  int
	execSQL    string
	execArgs   []any
	execErr    error
	execTag    pgconn.CommandTag
	execTagSet bool

	queryRowCalls int
	queryRowSQL   string
	queryRowArgs  []any
	queryRowFunc  func(sql string) pgx.Row

	queryCalls int
	querySQL   string
	queryArgs  []any
	queryFunc  func(sql string) (pgx.Rows, error)
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.execSQL = sql
	f.execArgs = args
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	if f.execTagSet {
		return f.execTag, nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queryRowCalls++
	f.queryRowSQL = sql
	f.queryRowArgs = args
	if f.queryRowFunc != nil {
		return f.queryRowFunc(sql)
	}
	return errRow{err: pgx.ErrNoRows}
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.queryCalls++
	f.querySQL = sql
	f.queryArgs = args
	if f.queryFunc != nil {
		return f.queryFunc(sql)
	}
	return nil, errors.New("fakeDB: Query not configured")
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

type staticRow struct {
	values []any
	err    error
}

func (r staticRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("staticRow: len mismatch")
	}
	for i, d := range dest {
		assign(d, r.values[i])
	}
	return nil
}

func assign(dest, src any) {
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *int:
		*d = src.(int)
	case *int32:
		*d = src.(int32)
	case *bool:
		*d = src.(bool)
	case *time.Time:
		*d = src.(time.Time)
	case *[]byte:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	case **int:
		if src == nil {
			*d = nil
		} else {
			v := src.(int)
			*d = &v
		}
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case **time.Time:
		if src == nil {
			*d = nil
		} else {
			tt := src.(time.Time)
			*d = &tt
		}
	default:
		panic("staticRow.assign: unsupported dest type")
	}
}

func intp(v int) *int       { return &v }
func strp(v string) *string { return &v }

// --- Insert -----------------------------------------------------------

func TestInsert_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{now}}
		},
	}
	aid := "archon-alice"
	run := &ApplyRun{
		ApplyID:         "01HAPPLY0000000000000000",
		SID:             "host.example.com",
		IncarnationName: "redis-prod",
		Scenario:        "create",
		Status:          StatusRunning,
		StartedByAID:    &aid,
	}
	if err := Insert(context.Background(), f, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !run.StartedAt.Equal(now) {
		t.Errorf("RETURNING started_at not assigned: %v", run.StartedAt)
	}
	if f.queryRowCalls != 1 {
		t.Errorf("queryRowCalls = %d, want 1", f.queryRowCalls)
	}
	if !strings.Contains(f.queryRowSQL, "INSERT INTO apply_runs") {
		t.Errorf("SQL = %q", f.queryRowSQL)
	}
	if len(f.queryRowArgs) != 9 {
		t.Fatalf("args len = %d, want 9", len(f.queryRowArgs))
	}
	if f.queryRowArgs[8] != 0 {
		t.Errorf("args[8] passage = %v, want 0 (default Passage)", f.queryRowArgs[8])
	}
	if f.queryRowArgs[0] != "01HAPPLY0000000000000000" {
		t.Errorf("args[0] apply_id = %v", f.queryRowArgs[0])
	}
	if f.queryRowArgs[1] != "host.example.com" {
		t.Errorf("args[1] sid = %v", f.queryRowArgs[1])
	}
	if f.queryRowArgs[2] != "redis-prod" {
		t.Errorf("args[2] incarnation_name = %v", f.queryRowArgs[2])
	}
	if f.queryRowArgs[5] != "running" {
		t.Errorf("args[5] status = %v", f.queryRowArgs[5])
	}
	if f.queryRowArgs[7] != "archon-alice" {
		t.Errorf("args[7] started_by_aid = %v", f.queryRowArgs[7])
	}
}

func TestInsert_NilTaskIdxAndAID(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now()}}
		},
	}
	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "i", Scenario: "sc",
		Status: StatusRunning,
	}
	if err := Insert(context.Background(), f, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// task_idx (idx 4), error_summary (idx 6), started_by_aid (idx 7) → nil.
	if f.queryRowArgs[4] != nil {
		t.Errorf("args[4] task_idx = %v, want nil", f.queryRowArgs[4])
	}
	if f.queryRowArgs[6] != nil {
		t.Errorf("args[6] error_summary = %v, want nil", f.queryRowArgs[6])
	}
	if f.queryRowArgs[7] != nil {
		t.Errorf("args[7] started_by_aid = %v, want nil", f.queryRowArgs[7])
	}
}

func TestInsert_TaskIdxPropagated(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now()}}
		},
	}
	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "i", Scenario: "sc",
		Status: StatusRunning, TaskIdx: intp(3),
	}
	if err := Insert(context.Background(), f, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if f.queryRowArgs[4] != 3 {
		t.Errorf("args[4] task_idx = %v, want 3", f.queryRowArgs[4])
	}
}

func TestInsert_RejectsNil(t *testing.T) {
	f := &fakeDB{}
	if err := Insert(context.Background(), f, nil); err == nil {
		t.Fatal("Insert(nil) returned nil")
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d, want 0", f.queryRowCalls)
	}
}

func TestInsert_RejectsEmptyFields(t *testing.T) {
	cases := []*ApplyRun{
		{ApplyID: "", SID: "s", IncarnationName: "i", Scenario: "sc", Status: StatusRunning},
		{ApplyID: "a", SID: "", IncarnationName: "i", Scenario: "sc", Status: StatusRunning},
		{ApplyID: "a", SID: "s", IncarnationName: "", Scenario: "sc", Status: StatusRunning},
		{ApplyID: "a", SID: "s", IncarnationName: "i", Scenario: "", Status: StatusRunning},
	}
	for i, run := range cases {
		f := &fakeDB{}
		if err := Insert(context.Background(), f, run); err == nil {
			t.Errorf("case %d: Insert returned nil for empty field", i)
		}
		if f.queryRowCalls != 0 {
			t.Errorf("case %d: queryRowCalls = %d, want 0", i, f.queryRowCalls)
		}
	}
}

func TestInsert_RejectsInvalidStatus(t *testing.T) {
	f := &fakeDB{}
	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "i", Scenario: "sc",
		Status: Status("frobnicated"),
	}
	if err := Insert(context.Background(), f, run); err == nil {
		t.Fatal("Insert with invalid status returned nil")
	}
}

func TestInsert_MapsUniqueViolation(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "apply_runs_pkey",
			}}
		},
	}
	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "i", Scenario: "sc",
		Status: StatusRunning,
	}
	err := Insert(context.Background(), f, run)
	if !errors.Is(err, ErrApplyRunAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrApplyRunAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "apply_runs_pkey") {
		t.Errorf("err = %v; expected constraint name", err)
	}
}

func TestInsert_MapsFKViolation(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "apply_runs_incarnation_fk",
			}}
		},
	}
	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "ghost", Scenario: "sc",
		Status: StatusRunning,
	}
	err := Insert(context.Background(), f, run)
	if err == nil {
		t.Fatal("FK-violation returned nil")
	}
	if errors.Is(err, ErrApplyRunAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrApplyRunAlreadyExists; err = %v", err)
	}
	if !strings.Contains(err.Error(), "FK violation") {
		t.Errorf("err = %v; expected \"FK violation\"", err)
	}
}

// --- UpdateStatus -----------------------------------------------------

func TestUpdateStatus_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1"), execTagSet: true}
	err := UpdateStatus(context.Background(), f, "a", "s", 0, StatusSuccess, nil)
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if f.execCalls != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.execSQL, "UPDATE apply_runs") {
		t.Errorf("SQL = %q", f.execSQL)
	}
	if !strings.Contains(f.execSQL, "finished_at") {
		t.Errorf("SQL must touch finished_at: %q", f.execSQL)
	}
	if len(f.execArgs) != 5 {
		t.Fatalf("args len = %d, want 5", len(f.execArgs))
	}
	if f.execArgs[4] != 0 {
		t.Errorf("args[4] passage = %v, want 0", f.execArgs[4])
	}
	if f.execArgs[2] != "success" {
		t.Errorf("args[2] status = %v, want success", f.execArgs[2])
	}
	if f.execArgs[3] != nil {
		t.Errorf("args[3] error_summary = %v, want nil", f.execArgs[3])
	}
}

func TestUpdateStatus_WithErrorSummary(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1"), execTagSet: true}
	err := UpdateStatus(context.Background(), f, "a", "s", 0, StatusFailed, strp("task 0 failed: policy_violation"))
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if f.execArgs[3] != "task 0 failed: policy_violation" {
		t.Errorf("args[3] error_summary = %v", f.execArgs[3])
	}
}

func TestUpdateStatus_NotFound(t *testing.T) {
	// UPDATE 0 rows + probe → ErrNoRows (row doesn't exist at all) → ErrApplyRunNotFound.
	f := &fakeDB{
		execTag:    pgconn.NewCommandTag("UPDATE 0"),
		execTagSet: true,
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: pgx.ErrNoRows}
		},
	}
	err := UpdateStatus(context.Background(), f, "a", "s", 0, StatusSuccess, nil)
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

// Append-only single-winner (ADR-027(j)): the SQL carries a source-status
// guard, a terminal is not overwritten by another terminal.
func TestUpdateStatus_AppendOnlyGuardInSQL(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1"), execTagSet: true}
	if err := UpdateStatus(context.Background(), f, "a", "s", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if !strings.Contains(f.execSQL, "status IN ('planned', 'claimed', 'running', 'dispatched')") {
		t.Errorf("UPDATE without append-only guard of source status: %q", f.execSQL)
	}
}

// dispatched → terminal goes through (UPDATE 1): after MarkDispatched a row
// is exactly dispatched by the time RunResult arrives, and the terminal
// must commit from that state.
func TestUpdateStatus_DispatchedToTerminal_OK(t *testing.T) {
	for _, term := range []Status{StatusSuccess, StatusFailed, StatusCancelled} {
		f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1"), execTagSet: true}
		if err := UpdateStatus(context.Background(), f, "a", "s", 0, term, nil); err != nil {
			t.Fatalf("dispatched → %s: %v", term, err)
		}
		if f.queryRowCalls != 0 {
			t.Errorf("%s: queryRowCalls = %d, want 0 (UPDATE won without probe)", term, f.queryRowCalls)
		}
		if !strings.Contains(f.execSQL, "'dispatched'") {
			t.Errorf("%s: guard does not allow dispatched as source status: %q", term, f.execSQL)
		}
	}
}

// Non-terminal → terminal: the transition goes through (UPDATE 1), probe is
// not triggered.
func TestUpdateStatus_NonTerminalToTerminal_OK(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1"), execTagSet: true}
	if err := UpdateStatus(context.Background(), f, "a", "s", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("non-terminal → terminal: %v", err)
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d, want 0 (UPDATE won without probe)", f.queryRowCalls)
	}
}

// Terminal → terminal: UPDATE 0 rows (guard blocked it), probe sees a
// terminal status → ErrApplyRunAlreadyTerminal (no-op, first one won, NOT an
// error).
func TestUpdateStatus_TerminalToTerminal_AlreadyTerminal(t *testing.T) {
	for _, st := range []string{"success", "failed", "cancelled"} {
		f := &fakeDB{
			execTag:    pgconn.NewCommandTag("UPDATE 0"),
			execTagSet: true,
			queryRowFunc: func(_ string) pgx.Row {
				return staticRow{values: []any{st}}
			},
		}
		err := UpdateStatus(context.Background(), f, "a", "s", 0, StatusFailed, strp("recovery boom"))
		if !errors.Is(err, ErrApplyRunAlreadyTerminal) {
			t.Errorf("status=%s: err = %v, want ErrApplyRunAlreadyTerminal", st, err)
		}
		if f.queryRowCalls != 1 {
			t.Errorf("status=%s: queryRowCalls = %d, want 1 (probe after UPDATE 0)", st, f.queryRowCalls)
		}
	}
}

func TestUpdateStatus_RejectsEmptyKey(t *testing.T) {
	f := &fakeDB{}
	if err := UpdateStatus(context.Background(), f, "", "s", 0, StatusSuccess, nil); err == nil {
		t.Error("empty apply_id returned nil")
	}
	if err := UpdateStatus(context.Background(), f, "a", "", 0, StatusSuccess, nil); err == nil {
		t.Error("empty sid returned nil")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d on validation failure, want 0", f.execCalls)
	}
}

func TestUpdateStatus_RejectsInvalidStatus(t *testing.T) {
	f := &fakeDB{}
	if err := UpdateStatus(context.Background(), f, "a", "s", 0, Status("hax"), nil); err == nil {
		t.Fatal("invalid status returned nil")
	}
}

// --- RecordTaskFailure ------------------------------------------------

func TestRecordTaskFailure_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1"), execTagSet: true}
	// taskIdx=2 (local), planIndex=7 (global) differ — pins down that both
	// travel as separate arguments (ADR-056 §S1 fix Variant B).
	err := RecordTaskFailure(context.Background(), f, "a", "s", 0, 2, 7, "task 7 core.pkg.installed: E: Version not found")
	if err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if f.execCalls != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.execSQL, "COALESCE(task_idx") || !strings.Contains(f.execSQL, "COALESCE(error_summary") ||
		!strings.Contains(f.execSQL, "COALESCE(failed_plan_index") {
		t.Errorf("SQL must COALESCE first-failure-wins (task_idx/error_summary/failed_plan_index): %q", f.execSQL)
	}
	if len(f.execArgs) != 6 {
		t.Fatalf("args len = %d, want 6", len(f.execArgs))
	}
	if f.execArgs[4] != 0 {
		t.Errorf("args[4] passage = %v, want 0", f.execArgs[4])
	}
	if f.execArgs[2] != 2 {
		t.Errorf("args[2] task_idx (local) = %v, want 2", f.execArgs[2])
	}
	if f.execArgs[5] != 7 {
		t.Errorf("args[5] failed_plan_index (global) = %v, want 7", f.execArgs[5])
	}
	if f.execArgs[3] != "task 7 core.pkg.installed: E: Version not found" {
		t.Errorf("args[3] summary = %v", f.execArgs[3])
	}
}

func TestRecordTaskFailure_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0"), execTagSet: true}
	err := RecordTaskFailure(context.Background(), f, "a", "s", 0, 0, 0, "boom")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

func TestRecordTaskFailure_RejectsBadInput(t *testing.T) {
	f := &fakeDB{}
	if err := RecordTaskFailure(context.Background(), f, "", "s", 0, 0, 0, "x"); err == nil {
		t.Error("empty apply_id returned nil")
	}
	if err := RecordTaskFailure(context.Background(), f, "a", "", 0, 0, 0, "x"); err == nil {
		t.Error("empty sid returned nil")
	}
	if err := RecordTaskFailure(context.Background(), f, "a", "s", 0, -1, 0, "x"); err == nil {
		t.Error("negative task_idx returned nil")
	}
	if err := RecordTaskFailure(context.Background(), f, "a", "s", 0, 0, -1, "x"); err == nil {
		t.Error("negative plan_index returned nil")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d on validation failure, want 0", f.execCalls)
	}
}

// --- SelectByApplyID --------------------------------------------------

func TestSelectByApplyID_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	fin := now.Add(time.Minute)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{
				"01HAPPLY", "host.example.com", "redis-prod", "create",
				2, // task_idx (**int dest, plain int src)
				"success",
				"", // error_summary (**string dest, plain string src)
				now,
				fin,            // finished_at (**time.Time dest)
				"archon-alice", // started_by_aid (**string dest)
				nil,            // claim_by_kid (**string)
				nil,            // claim_at (**time.Time)
				nil,            // claim_expires_at (**time.Time)
				0,              // attempt (*int)
				nil,            // recipe (*[]byte) NULL → nil Recipe
			}}
		},
	}
	run, err := SelectByApplyID(context.Background(), f, "01HAPPLY", "host.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if run.ApplyID != "01HAPPLY" || run.SID != "host.example.com" {
		t.Errorf("key mismatch: %+v", run)
	}
	if run.Status != StatusSuccess {
		t.Errorf("status = %q, want success", run.Status)
	}
	if run.TaskIdx == nil || *run.TaskIdx != 2 {
		t.Errorf("task_idx = %v, want 2", run.TaskIdx)
	}
	if run.FinishedAt == nil || !run.FinishedAt.Equal(fin) {
		t.Errorf("finished_at = %v, want %v", run.FinishedAt, fin)
	}
	if run.StartedByAID == nil || *run.StartedByAID != "archon-alice" {
		t.Errorf("started_by_aid = %v", run.StartedByAID)
	}
}

func TestSelectByApplyID_NullableNils(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{
				"a", "s", "i", "sc",
				nil, // task_idx NULL
				"running",
				nil, // error_summary NULL
				now,
				nil, // finished_at NULL
				nil, // started_by_aid NULL
				nil, // claim_by_kid NULL
				nil, // claim_at NULL
				nil, // claim_expires_at NULL
				0,   // attempt
				nil, // recipe NULL
			}}
		},
	}
	run, err := SelectByApplyID(context.Background(), f, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if run.TaskIdx != nil || run.ErrorSummary != nil || run.FinishedAt != nil || run.StartedByAID != nil {
		t.Errorf("nullable not nil: %+v", run)
	}
	if run.Status != StatusRunning {
		t.Errorf("status = %q, want running", run.Status)
	}
}

func TestSelectByApplyID_NotFound(t *testing.T) {
	f := &fakeDB{} // default → ErrNoRows
	_, err := SelectByApplyID(context.Background(), f, "missing", "s")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

// --- SelectIncarnationByApplyID ---------------------------------------

func TestSelectIncarnationByApplyID_HappyPath(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{"redis-prod", "scale", int32(3)}}
		},
	}
	name, scenario, attempt, err := SelectIncarnationByApplyID(context.Background(), f, "a", "s", 0)
	if err != nil {
		t.Fatalf("SelectIncarnationByApplyID: %v", err)
	}
	if name != "redis-prod" || scenario != "scale" {
		t.Errorf("got (%q, %q), want (redis-prod, scale)", name, scenario)
	}
	if attempt != 3 {
		t.Errorf("attempt = %d, want 3 (fencing-epoch of row)", attempt)
	}
	if len(f.queryRowArgs) != 3 || f.queryRowArgs[0] != "a" || f.queryRowArgs[1] != "s" || f.queryRowArgs[2] != 0 {
		t.Errorf("args = %v", f.queryRowArgs)
	}
}

func TestSelectIncarnationByApplyID_NotFound(t *testing.T) {
	f := &fakeDB{} // default → ErrNoRows
	_, _, _, err := SelectIncarnationByApplyID(context.Background(), f, "missing", "s", 0)
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

// --- RequestCancel ----------------------------------------------------

func TestRequestCancel_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 2"), execTagSet: true}
	affected, err := RequestCancel(context.Background(), f, "01HCANCEL")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 2 {
		t.Errorf("affected = %d, want 2", affected)
	}
	if f.execCalls != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.execSQL, "cancel_requested = true") {
		t.Errorf("SQL must set cancel_requested: %q", f.execSQL)
	}
	// Non-terminal rows (planned/claimed/running) — ADR-027 cutover: a Cancel
	// in the planned/claimed window must reach the row, terminal rows are
	// excluded (Cancel on a finished run is a no-op).
	if !strings.Contains(f.execSQL, "status IN ('planned', 'claimed', 'running')") {
		t.Errorf("SQL must filter status IN (planned,claimed,running): %q", f.execSQL)
	}
	if len(f.execArgs) != 1 || f.execArgs[0] != "01HCANCEL" {
		t.Errorf("args = %v, want [01HCANCEL]", f.execArgs)
	}
}

func TestRequestCancel_NoRunningRows_NoOp(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0"), execTagSet: true}
	affected, err := RequestCancel(context.Background(), f, "01HDONE")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (terminal/unknown run)", affected)
	}
}

func TestRequestCancel_RejectsEmptyApplyID(t *testing.T) {
	f := &fakeDB{}
	if _, err := RequestCancel(context.Background(), f, ""); err == nil {
		t.Error("empty apply_id returned nil")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d on validation failure, want 0", f.execCalls)
	}
}

// TestRequestCancel_PGError — a PG update failure on the flag is wrapped
// into an error (RequestCancel is the only write path for cluster-wide
// Cancel; caller Runner.RequestCancel propagates it up to the admin API).
// affected is meaningless on error — the caller must check err first.
func TestRequestCancel_PGError(t *testing.T) {
	boom := errors.New("connection reset by peer")
	f := &fakeDB{execErr: boom}
	affected, err := RequestCancel(context.Background(), f, "01HCANCEL")
	if err == nil {
		t.Fatal("RequestCancel on Exec error returned nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrapped Exec error", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d on error, want 0", affected)
	}
}

// --- SelectCancelRequested --------------------------------------------

// TestSelectCancelRequested_True — the Acolyte reads the fresh flag before
// SendApply; true → the apply is not sent (claim moves the task to
// cancelled).
func TestSelectCancelRequested_True(t *testing.T) {
	f := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return staticRow{values: []any{true}}
	}}
	got, err := SelectCancelRequested(context.Background(), f, "01HCANCEL", "host-a")
	if err != nil {
		t.Fatalf("SelectCancelRequested: %v", err)
	}
	if !got {
		t.Errorf("cancel_requested = false, want true")
	}
	if !strings.Contains(f.queryRowSQL, "cancel_requested") {
		t.Errorf("SQL must select cancel_requested: %q", f.queryRowSQL)
	}
	if len(f.queryRowArgs) != 2 || f.queryRowArgs[0] != "01HCANCEL" || f.queryRowArgs[1] != "host-a" {
		t.Errorf("args = %v, want [01HCANCEL host-a]", f.queryRowArgs)
	}
}

func TestSelectCancelRequested_False(t *testing.T) {
	f := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return staticRow{values: []any{false}}
	}}
	got, err := SelectCancelRequested(context.Background(), f, "01HRUN", "host-a")
	if err != nil {
		t.Fatalf("SelectCancelRequested: %v", err)
	}
	if got {
		t.Errorf("cancel_requested = true, want false")
	}
}

func TestSelectCancelRequested_NotFound(t *testing.T) {
	f := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return errRow{err: pgx.ErrNoRows}
	}}
	_, err := SelectCancelRequested(context.Background(), f, "01HGHOST", "host-a")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Errorf("err = %v, want ErrApplyRunNotFound", err)
	}
}

// --- ValidStatus ------------------------------------------------------

func TestValidStatus(t *testing.T) {
	// Phase 1 (ADR-027): planned/claimed are now valid for the CRUD layer;
	// the old running/success/failed/cancelled are preserved.
	good := []Status{
		StatusPlanned, StatusClaimed,
		StatusRunning, StatusSuccess, StatusFailed, StatusCancelled,
	}
	bad := []Status{"", "RUNNING", "done", "error_locked"}
	for _, s := range good {
		if !ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = true, want false", s)
		}
	}
}

// --- ClaimNext (validation + query shape, no DB) --------------------

func TestClaimNext_Validation(t *testing.T) {
	f := &fakeDB{}
	cases := []struct {
		name  string
		kid   string
		lease time.Duration
		batch int
	}{
		{"empty kid", "", time.Second, 1},
		{"zero lease", "keeper-1", 0, 1},
		{"negative lease", "keeper-1", -time.Second, 1},
		{"zero batch", "keeper-1", time.Second, 0},
		{"negative batch", "keeper-1", time.Second, -5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ClaimNext(context.Background(), f, tc.kid, tc.lease, tc.batch); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d on validation failure, want 0 (don't go to DB)", f.queryCalls)
	}
}

func TestClaimNext_QueryShape(t *testing.T) {
	f := &fakeDB{
		queryFunc: func(_ string) (pgx.Rows, error) {
			return nil, errors.New("stop after capturing args")
		},
	}
	_, _ = ClaimNext(context.Background(), f, "keeper-1", 30*time.Second, 7)
	if f.queryCalls != 1 {
		t.Fatalf("queryCalls = %d, want 1", f.queryCalls)
	}
	if !strings.Contains(f.querySQL, "FOR UPDATE SKIP LOCKED") {
		t.Errorf("SQL does not contain FOR UPDATE SKIP LOCKED: %q", f.querySQL)
	}
	if !strings.Contains(f.querySQL, "attempt          = r.attempt + 1") {
		t.Errorf("SQL does not increment attempt: %q", f.querySQL)
	}
	if len(f.queryArgs) != 3 {
		t.Fatalf("args len = %d, want 3", len(f.queryArgs))
	}
	if f.queryArgs[0] != "keeper-1" {
		t.Errorf("args[0] kid = %v, want keeper-1", f.queryArgs[0])
	}
	if f.queryArgs[2] != 7 {
		t.Errorf("args[2] batch = %v, want 7", f.queryArgs[2])
	}
}

// --- MarkDispatched (validation + guard shape, no DB) -----------------

func TestMarkDispatched_Validation(t *testing.T) {
	f := &fakeDB{}
	if err := MarkDispatched(context.Background(), f, "", "s"); err == nil {
		t.Error("empty apply_id: expected error")
	}
	if err := MarkDispatched(context.Background(), f, "a", ""); err == nil {
		t.Error("empty sid: expected error")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d on validation failure, want 0", f.execCalls)
	}
}

func TestMarkDispatched_GuardFilter(t *testing.T) {
	// Exec touched the row → ok, no probe SELECT needed.
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1"), execTagSet: true}
	if err := MarkDispatched(context.Background(), f, "a", "s"); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	if !strings.Contains(f.execSQL, "status = 'claimed'") {
		t.Errorf("guard filter status='claimed' missing from SQL: %q", f.execSQL)
	}
	if !strings.Contains(f.execSQL, "status = 'dispatched'") {
		t.Errorf("target status dispatched missing from SQL: %q", f.execSQL)
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d, want 0 (successful UPDATE doesn't add status)", f.queryRowCalls)
	}
}

func TestMarkDispatched_ZeroRows_NotFound(t *testing.T) {
	// Exec touched 0 rows + the probe SELECT returns ErrNoRows → ErrApplyRunNotFound.
	f := &fakeDB{
		execTag:    pgconn.NewCommandTag("UPDATE 0"),
		execTagSet: true,
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: pgx.ErrNoRows}
		},
	}
	if err := MarkDispatched(context.Background(), f, "a", "s"); !errors.Is(err, ErrApplyRunNotFound) {
		t.Errorf("err = %v, want ErrApplyRunNotFound", err)
	}
}

func TestMarkDispatched_ZeroRows_NotClaimed(t *testing.T) {
	// Exec touched 0 rows + the probe SELECT found the row in dispatched → NotClaimed.
	f := &fakeDB{
		execTag:    pgconn.NewCommandTag("UPDATE 0"),
		execTagSet: true,
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{"dispatched"}}
		},
	}
	if err := MarkDispatched(context.Background(), f, "a", "s"); !errors.Is(err, ErrApplyRunNotClaimed) {
		t.Errorf("err = %v, want ErrApplyRunNotClaimed", err)
	}
}

// --- OrphanDispatched (Soul-reconcile, ADR-027(g), S6) ---

// TestOrphanDispatched_SweepSQL — the SQL terminates dispatched rows into orphaned by SID
// with an epoch-fenced filter (`status='dispatched'` + `apply_id != ALL($2)`), and carries
// finished_at=NOW() and a fixed error_summary.
func TestOrphanDispatched_SweepSQL(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 2"), execTagSet: true}
	n, err := OrphanDispatched(context.Background(), f, "host-1", []*ActiveApply{
		{ApplyID: "apply-live", Attempt: 3},
	})
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 2 {
		t.Errorf("RowsAffected = %d, want 2", n)
	}
	if !strings.Contains(f.execSQL, "status        = 'orphaned'") {
		t.Errorf("SQL не ставит status=orphaned: %q", f.execSQL)
	}
	if !strings.Contains(f.execSQL, "status = 'dispatched'") {
		t.Errorf("SQL без single-winner-фильтра status='dispatched': %q", f.execSQL)
	}
	if !strings.Contains(f.execSQL, "apply_id != ALL($2)") {
		t.Errorf("SQL без epoch-fenced-фильтра known-набора: %q", f.execSQL)
	}
	if !strings.Contains(f.execSQL, "finished_at   = NOW()") {
		t.Errorf("SQL не проставляет finished_at: %q", f.execSQL)
	}
	// args: $1=sid, $2=known apply_ids, $3=error_summary.
	if len(f.execArgs) != 3 {
		t.Fatalf("execArgs len = %d, want 3", len(f.execArgs))
	}
	if f.execArgs[0] != "host-1" {
		t.Errorf("$1 = %v, want host-1", f.execArgs[0])
	}
	known, ok := f.execArgs[1].([]string)
	if !ok || len(known) != 1 || known[0] != "apply-live" {
		t.Errorf("$2 = %v, want [apply-live]", f.execArgs[1])
	}
	if f.execArgs[2] != orphanDispatchedErrorSummary {
		t.Errorf("$3 = %v, want фиксированный error_summary", f.execArgs[2])
	}
}

// TestOrphanDispatched_EmptyKnown_OrphansAll — an empty WardRoster (Soul restart):
// known=nil → `apply_id != ALL('{}')` is true for all → all dispatched rows of the
// SID are terminated (an empty slice is passed, not a skip of the sweep).
func TestOrphanDispatched_EmptyKnown_OrphansAll(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 5"), execTagSet: true}
	n, err := OrphanDispatched(context.Background(), f, "host-1", nil)
	if err != nil {
		t.Fatalf("OrphanDispatched(nil): %v", err)
	}
	if n != 5 {
		t.Errorf("RowsAffected = %d, want 5 (осиротили все)", n)
	}
	known, ok := f.execArgs[1].([]string)
	if !ok || len(known) != 0 {
		t.Errorf("$2 = %v, want пустой []string (не nil-маркер)", f.execArgs[1])
	}
}

// TestOrphanDispatched_KnownFiltersEmptyIDs — nil entries and empty apply_id in
// the set are ignored (defensive), they don't land in the known slice.
func TestOrphanDispatched_KnownFiltersEmptyIDs(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0"), execTagSet: true}
	_, err := OrphanDispatched(context.Background(), f, "host-1", []*ActiveApply{
		nil,
		{ApplyID: ""},
		{ApplyID: "apply-real", Attempt: 1},
	})
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	known := f.execArgs[1].([]string)
	if len(known) != 1 || known[0] != "apply-real" {
		t.Errorf("known = %v, want [apply-real] (мусор отфильтрован)", known)
	}
}

// TestOrphanDispatched_NoRows_NotError — 0 rows affected (everything is already
// terminal, or everything was declared alive) — not an error.
func TestOrphanDispatched_NoRows_NotError(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0"), execTagSet: true}
	n, err := OrphanDispatched(context.Background(), f, "host-1", []*ActiveApply{{ApplyID: "x"}})
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 0 {
		t.Errorf("RowsAffected = %d, want 0", n)
	}
}

// TestOrphanDispatched_RejectsEmptySID — an empty SID is rejected before Exec.
func TestOrphanDispatched_RejectsEmptySID(t *testing.T) {
	f := &fakeDB{}
	if _, err := OrphanDispatched(context.Background(), f, "", nil); err == nil {
		t.Fatal("OrphanDispatched with empty SID returned nil-error")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (validation before Exec)", f.execCalls)
	}
}

// TestOrphanDispatched_PGError — an Exec error is wrapped, RowsAffected=0.
func TestOrphanDispatched_PGError(t *testing.T) {
	f := &fakeDB{execErr: errors.New("boom")}
	n, err := OrphanDispatched(context.Background(), f, "host-1", nil)
	if err == nil {
		t.Fatal("OrphanDispatched on Exec error returned nil-error")
	}
	if n != 0 {
		t.Errorf("RowsAffected = %d, want 0 on error", n)
	}
}
