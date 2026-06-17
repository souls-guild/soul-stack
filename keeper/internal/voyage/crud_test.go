package voyage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB — ExecQueryRower-stub (parity errandrun::fakeDB).
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

	// CopyFrom-перехват (InsertTargets, S-med-3): фиксируем таблицу/колонки и
	// прочитанные из источника строки; copyErr подменяет результат.
	copyCalls   int
	copyTable   pgx.Identifier
	copyColumns []string
	copyValues  [][]any
	copyErr     error
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
	return nil, errors.New("fakeDB: Query not configured")
}

// CopyFrom прокручивает источник до конца (как реальный pgx-COPY читает все
// строки), запоминая значения каждой строки и таблицу/колонки для ассертов.
func (f *fakeDB) CopyFrom(_ context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error) {
	f.copyCalls++
	f.copyTable = table
	f.copyColumns = columns
	f.copyValues = nil
	for src.Next() {
		vals, err := src.Values()
		if err != nil {
			return int64(len(f.copyValues)), err
		}
		f.copyValues = append(f.copyValues, vals)
	}
	if err := src.Err(); err != nil {
		return int64(len(f.copyValues)), err
	}
	if f.copyErr != nil {
		return 0, f.copyErr
	}
	return int64(len(f.copyValues)), nil
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

type fixedTimeRow struct{ t time.Time }

func (r fixedTimeRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("fixedTimeRow: expected 1 dest")
	}
	tp, ok := dest[0].(*time.Time)
	if !ok {
		return errors.New("fixedTimeRow: dest is not *time.Time")
	}
	*tp = r.t
	return nil
}

func strptr(s string) *string { return &s }

func scenarioVoyage() *Voyage {
	return &Voyage{
		VoyageID:       "01H000000000000000000VOY00",
		Kind:           KindScenario,
		ScenarioName:   strptr("restart"),
		Input:          []byte(`{"force":true}`),
		TargetResolved: json.RawMessage(`["service-redis","service-pg"]`),
		Status:         StatusPending,
		StartedByAID:   "archon-alice",
	}
}

func commandVoyage() *Voyage {
	return &Voyage{
		VoyageID:       "01H000000000000000000VOY01",
		Kind:           KindCommand,
		Module:         strptr("core.cmd.shell"),
		Input:          []byte(`{"cmd":"uptime"}`),
		TargetResolved: json.RawMessage(`["a.example","b.example"]`),
		Status:         StatusPending,
		StartedByAID:   "archon-bob",
	}
}

func TestValidKind(t *testing.T) {
	t.Parallel()
	for _, k := range []Kind{KindScenario, KindCommand} {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false", k)
		}
	}
	for _, k := range []Kind{"", "Scenario", "exec"} {
		if ValidKind(k) {
			t.Errorf("ValidKind(%q) = true", k)
		}
	}
}

func TestValidStatus(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{StatusScheduled, StatusPending, StatusRunning, StatusSucceeded, StatusFailed, StatusPartialFailed, StatusCancelled} {
		if !ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = false", s)
		}
	}
	for _, s := range []Status{"", "unknown", "Pending"} {
		if ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = true", s)
		}
	}
}

func TestValidTargetKind(t *testing.T) {
	t.Parallel()
	for _, k := range []TargetKind{TargetKindIncarnation, TargetKindSID} {
		if !ValidTargetKind(k) {
			t.Errorf("ValidTargetKind(%q) = false", k)
		}
	}
	if ValidTargetKind("host") {
		t.Error("ValidTargetKind(host) = true")
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{StatusSucceeded, StatusFailed, StatusPartialFailed, StatusCancelled} {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = false", s)
		}
	}
	for _, s := range []Status{StatusScheduled, StatusPending, StatusRunning} {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = true", s)
		}
	}
}

func TestInsert_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*Voyage)
		want string
	}{
		{"nil voyage", func(*Voyage) {}, "nil voyage"},
		{"empty id", func(v *Voyage) { v.VoyageID = "" }, "empty voyage_id"},
		{"empty aid", func(v *Voyage) { v.StartedByAID = "" }, "empty started_by_aid"},
		{"invalid kind", func(v *Voyage) { v.Kind = "bogus" }, "invalid kind"},
		{"scenario without name", func(v *Voyage) { v.ScenarioName = nil }, "kind=scenario требует"},
		{"scenario with module", func(v *Voyage) { v.Module = strptr("core.cmd.shell") }, "не должен нести module"},
		{"empty target", func(v *Voyage) { v.TargetResolved = nil }, "empty target_resolved"},
		{"batch_size zero", func(v *Voyage) { z := 0; v.BatchSize = &z }, "batch_size must be > 0"},
		{"concurrency negative", func(v *Voyage) { n := -1; v.Concurrency = &n }, "concurrency must be > 0"},
		{"invalid on_failure", func(v *Voyage) { f := OnFailure("noop"); v.OnFailure = &f }, "invalid on_failure"},
		{"non-pending status", func(v *Voyage) { v.Status = StatusRunning }, "Insert требует status=pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fdb := &fakeDB{}
			var arg *Voyage
			if tc.name != "nil voyage" {
				arg = scenarioVoyage()
				tc.mut(arg)
			}
			err := Insert(ctx, fdb, arg)
			if err == nil {
				t.Fatal("Insert: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Insert err = %v, want substring %q", err, tc.want)
			}
			if fdb.queryRowCalls != 0 {
				t.Errorf("QueryRow called %d times, want 0 (validation before SQL)", fdb.queryRowCalls)
			}
		})
	}
}

func TestInsert_CommandValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*Voyage)
		want string
	}{
		{"command without module", func(v *Voyage) { v.Module = nil }, "kind=command требует"},
		{"command with scenario", func(v *Voyage) { v.ScenarioName = strptr("restart") }, "не должен нести scenario_name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := commandVoyage()
			tc.mut(v)
			err := Insert(ctx, &fakeDB{}, v)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Insert err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInsert_HappyPath_Scenario(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC)
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: want} }}

	v := scenarioVoyage()
	v.TargetOrigin = json.RawMessage(`{"coven":"prod-eu"}`)
	f := OnFailureContinue
	v.OnFailure = &f

	if err := Insert(ctx, fdb, v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !v.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", v.CreatedAt, want)
	}
	if fdb.queryRowCalls != 1 {
		t.Errorf("queryRowCalls = %d, want 1", fdb.queryRowCalls)
	}
	if !strings.Contains(fdb.queryRowSQL, "INSERT INTO voyages") {
		t.Errorf("unexpected SQL: %.200s", fdb.queryRowSQL)
	}
	if got := len(fdb.queryRowArgs); got != 22 {
		t.Errorf("queryRowArgs len = %d, want 22", got)
	}
}

func TestInsert_HappyPath_Command(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
	if err := Insert(ctx, fdb, commandVoyage()); err != nil {
		t.Fatalf("Insert command: %v", err)
	}
}

func TestInsert_AutoFillStatusPendingAndInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
	v := scenarioVoyage()
	v.Status = ""
	v.Input = nil // должен подставиться `{}`
	if err := Insert(ctx, fdb, v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if v.Status != StatusPending {
		t.Errorf("Status = %q, want pending (auto-fill)", v.Status)
	}
	// Аргумент input (5-й позиционный) — `{}`.
	if got, ok := fdb.queryRowArgs[4].([]byte); !ok || string(got) != "{}" {
		t.Errorf("input arg = %v, want []byte(`{}`)", fdb.queryRowArgs[4])
	}
}

// TestInsert_ScheduleAtFuture_StatusScheduled — schedule_at в будущем (+ пустой
// Status) → автоветка scheduled, в БД уходит status='scheduled' (arg #16).
func TestInsert_ScheduleAtFuture_StatusScheduled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
	v := scenarioVoyage()
	v.Status = ""
	future := time.Now().Add(time.Hour)
	v.ScheduleAt = &future
	if err := Insert(ctx, fdb, v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if v.Status != StatusScheduled {
		t.Errorf("Status = %q, want scheduled (future schedule_at)", v.Status)
	}
	if got, ok := fdb.queryRowArgs[15].(string); !ok || got != string(StatusScheduled) {
		t.Errorf("status arg = %v, want %q", fdb.queryRowArgs[15], StatusScheduled)
	}
}

// TestInsert_ScheduleAtPast_StatusPending — schedule_at в прошлом → ветка
// pending (наступившее «отложенное» = немедленно подбираемое).
func TestInsert_ScheduleAtPast_StatusPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
	v := scenarioVoyage()
	v.Status = ""
	past := time.Now().Add(-time.Hour)
	v.ScheduleAt = &past
	if err := Insert(ctx, fdb, v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if v.Status != StatusPending {
		t.Errorf("Status = %q, want pending (past schedule_at)", v.Status)
	}
}

// TestInsert_NoScheduleAt_StatusPending — без schedule_at → pending (S1-S3
// поведение не меняется).
func TestInsert_NoScheduleAt_StatusPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
	v := scenarioVoyage()
	v.Status = ""
	v.ScheduleAt = nil
	if err := Insert(ctx, fdb, v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if v.Status != StatusPending {
		t.Errorf("Status = %q, want pending (no schedule_at)", v.Status)
	}
}

// TestInsert_ExplicitScheduledStatus_Accepted — caller может явно задать
// scheduled (Insert больше не отвергает не-pending, если это scheduled).
func TestInsert_ExplicitScheduledStatus_Accepted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
	v := scenarioVoyage()
	v.Status = StatusScheduled
	future := time.Now().Add(time.Hour)
	v.ScheduleAt = &future
	if err := Insert(ctx, fdb, v); err != nil {
		t.Fatalf("Insert with explicit scheduled: %v", err)
	}
}

func TestInsert_UniqueViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return errRow{err: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "voyages_pkey"}}
	}}
	err := Insert(ctx, fdb, scenarioVoyage())
	if !errors.Is(err, ErrVoyageExists) {
		t.Errorf("err = %v, want ErrVoyageExists", err)
	}
}

func TestInsert_FKViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return errRow{err: &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "voyages_started_by_aid_fk"}}
	}}
	err := Insert(ctx, fdb, scenarioVoyage())
	if err == nil || !strings.Contains(err.Error(), "FK violation") {
		t.Errorf("err = %v, want FK violation", err)
	}
}

func TestSelectByID_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	_, err := SelectByID(ctx, fdb, "01H")
	if !errors.Is(err, ErrVoyageNotFound) {
		t.Errorf("err = %v, want ErrVoyageNotFound", err)
	}
}

func TestSelectByID_EmptyID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := SelectByID(ctx, &fakeDB{}, "")
	if err == nil || !strings.Contains(err.Error(), "empty voyage_id") {
		t.Errorf("err = %v, want empty voyage_id", err)
	}
}

func TestFinalize_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	if err := Finalize(ctx, fdb, "", "kid", StatusSucceeded, nil); err == nil || !strings.Contains(err.Error(), "empty voyage_id") {
		t.Errorf("empty id: %v", err)
	}
	if err := Finalize(ctx, fdb, "01H", "", StatusSucceeded, nil); err == nil || !strings.Contains(err.Error(), "empty kid") {
		t.Errorf("empty kid: %v", err)
	}
	if err := Finalize(ctx, fdb, "01H", "kid", "weird", nil); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("invalid status: %v", err)
	}
	if err := Finalize(ctx, fdb, "01H", "kid", StatusPending, nil); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("non-terminal (pending): %v", err)
	}
	if err := Finalize(ctx, fdb, "01H", "kid", StatusRunning, nil); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("non-terminal (running): %v", err)
	}
	if fdb.execCalls != 0 {
		t.Errorf("Exec called on validation error: %d", fdb.execCalls)
	}
}

func TestFinalize_LeaseLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0"), execTagSet: true}
	err := Finalize(ctx, fdb, "01H", "kid", StatusSucceeded, nil)
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("err = %v, want ErrLeaseLost", err)
	}
}

func TestFinalize_HappyPath_WithSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	summary := &Summary{Total: 2, Succeeded: 1, Failed: 1}
	if err := Finalize(ctx, fdb, "01H", "kid-1", StatusPartialFailed, summary); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !strings.Contains(fdb.execSQL, "claimed_by_kid = $2") {
		t.Errorf("SQL без ownership-guard: %.200s", fdb.execSQL)
	}
	if !strings.Contains(fdb.execSQL, "status         = 'running'") {
		t.Errorf("SQL без status='running'-guard: %.200s", fdb.execSQL)
	}
}

func TestInsertTargets_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := InsertTargets(ctx, &fakeDB{}, "", []VoyageTarget{{TargetKind: TargetKindSID, TargetID: "a"}}); err == nil || !strings.Contains(err.Error(), "empty voyage_id") {
		t.Errorf("empty voyage_id: %v", err)
	}
	if err := InsertTargets(ctx, &fakeDB{}, "v1", nil); err == nil || !strings.Contains(err.Error(), "empty targets") {
		t.Errorf("empty targets: %v", err)
	}
	if err := InsertTargets(ctx, &fakeDB{}, "v1", []VoyageTarget{{TargetKind: "bad", TargetID: "a"}}); err == nil || !strings.Contains(err.Error(), "invalid target_kind") {
		t.Errorf("invalid kind: %v", err)
	}
	if err := InsertTargets(ctx, &fakeDB{}, "v1", []VoyageTarget{{TargetKind: TargetKindSID}}); err == nil || !strings.Contains(err.Error(), "empty target_id") {
		t.Errorf("empty target_id: %v", err)
	}
}

func TestInsertTargets_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	targets := []VoyageTarget{
		{TargetKind: TargetKindIncarnation, TargetID: "service-redis", BatchIndex: 0},
		{TargetKind: TargetKindIncarnation, TargetID: "service-pg", BatchIndex: 0},
		{TargetKind: TargetKindIncarnation, TargetID: "service-app", BatchIndex: 1},
	}
	if err := InsertTargets(ctx, fdb, "v1", targets); err != nil {
		t.Fatalf("InsertTargets: %v", err)
	}
	// S-med-3: вставка идёт одним CopyFrom, а не циклом per-row Exec.
	if fdb.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (InsertTargets использует CopyFrom, не Exec)", fdb.execCalls)
	}
	if fdb.copyCalls != 1 {
		t.Errorf("copyCalls = %d, want 1", fdb.copyCalls)
	}
	if got := fdb.copyTable.Sanitize(); got != `"voyage_targets"` {
		t.Errorf("CopyFrom table = %q, want voyage_targets", got)
	}
	if len(fdb.copyValues) != len(targets) {
		t.Errorf("CopyFrom rows = %d, want %d", len(fdb.copyValues), len(targets))
	}
}

// TestInsertTargets_CopyColumnsMatchSchema059 — guard от рассинхрона колонок
// CopyFrom со схемой voyage_targets (миграция 059): набор и порядок ДОЛЖНЫ быть
// ровно (voyage_id, target_kind, target_id, batch_index, status). Эталон выписан
// вручную из 059 (строки 127-131). Именно этот guard поймал бы BLOCKER, где
// CopyFrom объявлял несуществующие target_sid / target_incarnation / attempt.
// created_at в набор не входит — у колонки DEFAULT now() в схеме.
func TestInsertTargets_CopyColumnsMatchSchema059(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	targets := []VoyageTarget{
		{TargetKind: TargetKindIncarnation, TargetID: "service-redis", BatchIndex: 0},
		{TargetKind: TargetKindSID, TargetID: "host-a.example.com", BatchIndex: 1},
	}
	if err := InsertTargets(ctx, fdb, "v1", targets); err != nil {
		t.Fatalf("InsertTargets: %v", err)
	}

	wantCols := []string{"voyage_id", "target_kind", "target_id", "batch_index", "status"}
	if len(fdb.copyColumns) != len(wantCols) {
		t.Fatalf("CopyFrom columns = %v, want %v (схема 059)", fdb.copyColumns, wantCols)
	}
	for i, c := range wantCols {
		if fdb.copyColumns[i] != c {
			t.Errorf("CopyFrom column[%d] = %q, want %q", i, fdb.copyColumns[i], c)
		}
	}
	// Значения каждой строки идут СТРОГО в порядке колонок: voyage_id (override),
	// target_kind, target_id, batch_index, status=awaiting (auto-fill).
	if len(fdb.copyValues) != 2 {
		t.Fatalf("CopyFrom rows = %d, want 2", len(fdb.copyValues))
	}
	row0 := fdb.copyValues[0]
	want0 := []any{"v1", string(TargetKindIncarnation), "service-redis", 0, string(TargetStatusAwaiting)}
	for i := range want0 {
		if row0[i] != want0[i] {
			t.Errorf("row0 value[%d] = %v (%T), want %v", i, row0[i], row0[i], want0[i])
		}
	}
	row1 := fdb.copyValues[1]
	want1 := []any{"v1", string(TargetKindSID), "host-a.example.com", 1, string(TargetStatusAwaiting)}
	for i := range want1 {
		if row1[i] != want1[i] {
			t.Errorf("row1 value[%d] = %v (%T), want %v", i, row1[i], row1[i], want1[i])
		}
	}
}

// TestInsert_CadenceIDBackLink — спавн от Cadence (ADR-046 §2): CadenceID на
// Voyage уходит последним позиционным аргументом insertSQL (arg #22). Ручной
// прогон (CadenceID=nil) шлёт NULL (nil-интерфейс).
func TestInsert_CadenceIDBackLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("populated", func(t *testing.T) {
		fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
		v := scenarioVoyage()
		v.CadenceID = strptr("01H000000000000000000CAD00")
		if err := Insert(ctx, fdb, v); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if got, ok := fdb.queryRowArgs[21].(string); !ok || got != "01H000000000000000000CAD00" {
			t.Errorf("cadence_id arg = %v, want CAD00", fdb.queryRowArgs[21])
		}
	})

	t.Run("manual nil → NULL", func(t *testing.T) {
		fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return fixedTimeRow{t: time.Now().UTC()} }}
		v := scenarioVoyage()
		v.CadenceID = nil
		if err := Insert(ctx, fdb, v); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if fdb.queryRowArgs[21] != nil {
			t.Errorf("cadence_id arg = %v, want nil (NULL)", fdb.queryRowArgs[21])
		}
	})
}

// TestScanVoyage_CadenceIDRoundTrip — back-link cadence_id читается из строки в
// Voyage.CadenceID (последняя колонка selectColumns). Полная строка собирается
// через voyageRow, чтобы пройти весь scanVoyage с populated cadence_id.
func TestScanVoyage_CadenceIDRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	row := newVoyageRow()
	row.cadenceID = strptr("01H000000000000000000CAD42")
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return row }}

	v, err := SelectByID(ctx, fdb, "01H")
	if err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	if v.CadenceID == nil || *v.CadenceID != "01H000000000000000000CAD42" {
		t.Errorf("CadenceID = %v, want CAD42", v.CadenceID)
	}

	// Ручной прогон: cadence_id NULL → nil.
	row.cadenceID = nil
	v2, err := SelectByID(ctx, fdb, "01H")
	if err != nil {
		t.Fatalf("SelectByID nil: %v", err)
	}
	if v2.CadenceID != nil {
		t.Errorf("CadenceID = %v, want nil (NULL)", v2.CadenceID)
	}
}

// voyageRow — pgx.Row, отдающий все 31 колонку selectColumns в порядке scanVoyage.
// Минимальный набор валидных значений + изменяемый cadenceID для back-link-теста.
type voyageRow struct {
	cadenceID *string
}

func newVoyageRow() *voyageRow { return &voyageRow{} }

func (r *voyageRow) Scan(dest ...any) error {
	if len(dest) != 31 {
		return errors.New("voyageRow: expected 31 dest")
	}
	*dest[0].(*string) = "01H"                             // voyage_id
	*dest[1].(*string) = string(KindScenario)              // kind
	*dest[2].(**string) = strptr("restart")                // scenario_name
	*dest[3].(**string) = nil                              // module
	*dest[4].(*[]byte) = []byte(`{}`)                      // input
	*dest[5].(*json.RawMessage) = json.RawMessage(`["a"]`) // target_resolved
	*dest[6].(*[]byte) = nil                               // target_origin
	*dest[7].(**int) = nil                                 // batch_size
	*dest[8].(**int) = nil                                 // concurrency
	*dest[9].(**string) = nil                              // batch_mode
	*dest[10].(*bool) = false                              // dry_run
	*dest[11].(**time.Time) = nil                          // schedule_at
	*dest[12].(**float64) = nil                            // inter_batch_interval (epoch)
	*dest[13].(**string) = nil                             // on_failure
	*dest[14].(*int) = 0                                   // total_batches
	*dest[15].(*int) = 0                                   // current_batch_index
	*dest[16].(*string) = string(StatusPending)            // status
	*dest[17].(**string) = nil                             // claimed_by_kid
	*dest[18].(**time.Time) = nil                          // last_renewed_at
	*dest[19].(**time.Time) = nil                          // claim_expires_at
	*dest[20].(*int) = 0                                   // attempt
	*dest[21].(*string) = "archon-alice"                   // started_by_aid
	*dest[22].(*time.Time) = time.Now().UTC()              // created_at
	*dest[23].(**time.Time) = nil                          // started_at
	*dest[24].(**time.Time) = nil                          // finished_at
	*dest[25].(*[]byte) = nil                              // summary
	*dest[26].(**int) = nil                                // batch_percent
	*dest[27].(**int) = nil                                // fail_threshold
	*dest[28].(**float64) = nil                            // inter_unit_interval (epoch)
	*dest[29].(**bool) = nil                               // require_alive
	*dest[30].(**string) = r.cadenceID                     // cadence_id
	return nil
}

// voyageScanColumns — эталонный порядок колонок строки `voyages` в том виде, в
// каком scanVoyage присваивает row.Scan-аргументы (crud.go). Единственный источник
// правды для guard-а: при добавлении/перестановке колонки в insertSQL/
// selectColumns/scanVoyage без синхронизации списка — тест падает. Давно хрупкий
// (6 мест на cadence_id, ADR-046 §2); parity guard-059 voyage_targets.
//
// insertSQL пишет лишь подмножество (без DB-managed current_batch_index/
// claimed_by_kid/... и RETURNING created_at) — проверяем как ПОДпоследовательность
// эталона в том же относительном порядке, не как префикс.
var voyageScanColumns = []string{
	"voyage_id", "kind", "scenario_name", "module", "input",
	"target_resolved", "target_origin",
	"batch_size", "concurrency", "batch_mode", "dry_run",
	"schedule_at", "inter_batch_interval", "on_failure",
	"total_batches", "current_batch_index", "status",
	"claimed_by_kid", "last_renewed_at", "claim_expires_at", "attempt",
	"started_by_aid", "created_at", "started_at", "finished_at", "summary",
	"batch_percent", "fail_threshold", "inter_unit_interval", "require_alive",
	"cadence_id",
}

// TestColumnsMatchScanOrder — guard от рассинхрона ПОРЯДКА позиционных колонок.
// len(dest)!=31 ловит только число; этот тест ловит перестановку: selectColumns
// сверяется с [voyageScanColumns] по составу и порядку, insertSQL — что его
// колонки идут подпоследовательностью эталона (без DB-managed/RETURNING).
func TestColumnsMatchScanOrder(t *testing.T) {
	t.Parallel()

	sel := parseColumnList(selectColumns)
	if len(sel) != len(voyageScanColumns) {
		t.Fatalf("selectColumns = %d колонок %v, want %d %v", len(sel), sel, len(voyageScanColumns), voyageScanColumns)
	}
	for i, want := range voyageScanColumns {
		if sel[i] != want {
			t.Errorf("selectColumns[%d] = %q, want %q (рассинхрон со scanVoyage)", i, sel[i], want)
		}
	}

	ins := parseInsertColumns(insertSQL)
	if !isSubsequence(ins, voyageScanColumns) {
		t.Errorf("insertSQL колонки %v не являются упорядоченной подпоследовательностью эталона %v (рассинхрон порядка)", ins, voyageScanColumns)
	}
}

// isSubsequence сообщает, входят ли все элементы sub в seq в том же относительном
// порядке (каждый элемент sub должен встретиться при одном проходе по seq).
func isSubsequence(sub, seq []string) bool {
	i := 0
	for _, s := range seq {
		if i < len(sub) && sub[i] == s {
			i++
		}
	}
	return i == len(sub)
}

// parseColumnList разбирает SELECT-список колонок в имена по порядку: split по
// запятым + распаковка `EXTRACT(EPOCH FROM x)::float8` → x.
func parseColumnList(list string) []string {
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if name := normalizeColumn(p); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// parseInsertColumns вытаскивает имена колонок из INSERT INTO t (...) VALUES (...):
// берёт содержимое первой пары скобок (column-list) и разбирает как список.
func parseInsertColumns(sql string) []string {
	open := strings.Index(sql, "(")
	closeParen := strings.Index(sql, ")")
	if open < 0 || closeParen < 0 || closeParen < open {
		return nil
	}
	return parseColumnList(sql[open+1 : closeParen])
}

// normalizeColumn приводит элемент column-списка к голому имени:
// EXTRACT(EPOCH FROM x)::float8 → x; иначе trim + отброс хвостовых ::cast.
func normalizeColumn(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(strings.ToUpper(s), "EXTRACT(EPOCH FROM "); i >= 0 {
		inner := s[i+len("EXTRACT(EPOCH FROM "):]
		if j := strings.Index(inner, ")"); j >= 0 {
			inner = inner[:j]
		}
		return strings.TrimSpace(inner)
	}
	if i := strings.Index(s, "::"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// TestInsertTargets_CopyFromError — PG-сбой на CopyFrom оборачивается, не паникует.
func TestInsertTargets_CopyFromError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{copyErr: errors.New("copy boom")}
	targets := []VoyageTarget{{TargetKind: TargetKindSID, TargetID: "a", BatchIndex: 0}}
	err := InsertTargets(ctx, fdb, "v1", targets)
	if err == nil || !strings.Contains(err.Error(), "copy targets") {
		t.Fatalf("want copy targets error, got %v", err)
	}
}
