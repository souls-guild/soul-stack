package cadence

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

// fakeDB is an ExecQueryRower stub (parity with voyage::fakeDB, without CopyFrom).
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
	queryRows  pgx.Rows
	queryErr   error
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
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.queryRows != nil {
		return f.queryRows, nil
	}
	return nil, errors.New("fakeDB: Query not configured")
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// timestampsRow is RETURNING (created_at, updated_at) for Insert/Update.
type timestampsRow struct {
	created time.Time
	updated time.Time
	err     error
}

func (r timestampsRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 2 {
		return errors.New("timestampsRow: expected 2 dest")
	}
	cp, ok := dest[0].(*time.Time)
	if !ok {
		return errors.New("timestampsRow: dest[0] not *time.Time")
	}
	up, ok := dest[1].(*time.Time)
	if !ok {
		return errors.New("timestampsRow: dest[1] not *time.Time")
	}
	*cp, *up = r.created, r.updated
	return nil
}

func strptr(s string) *string { return &s }
func intptr(i int) *int       { return &i }

func intervalCadence() *Cadence {
	return &Cadence{
		ID:              "01H000000000000000000CAD00",
		Name:            "nightly-converge",
		Enabled:         true,
		ScheduleKind:    ScheduleKindInterval,
		IntervalSeconds: intptr(300),
		OverlapPolicy:   OverlapPolicySkip,
		Kind:            KindScenario,
		ScenarioName:    strptr("converge"),
		Target:          json.RawMessage(`{"coven":"prod-eu"}`),
		Input:           []byte(`{"force":true}`),
		CreatedByAID:    "archon-alice",
	}
}

func cronCadence() *Cadence {
	return &Cadence{
		ID:            "01H000000000000000000CAD01",
		Name:          "catalog-refresh",
		Enabled:       true,
		ScheduleKind:  ScheduleKindCron,
		CronExpr:      strptr("0 */6 * * *"),
		OverlapPolicy: OverlapPolicyQueue,
		Kind:          KindCommand,
		Module:        strptr("core.cmd.shell"),
		Target:        json.RawMessage(`["a.example","b.example"]`),
		CreatedByAID:  "archon-bob",
	}
}

// TestValidateIntervalFloor covers the floor limit for interval-Cadence periods
// (ADR-046 Pass B). floor=0 means disabled; cron/non-interval are unaffected;
// boundary 29/30.
func TestValidateIntervalFloor(t *testing.T) {
	t.Parallel()
	mk := func(sec int) *Cadence {
		c := intervalCadence()
		c.IntervalSeconds = intptr(sec)
		return c
	}
	cases := []struct {
		name      string
		c         *Cadence
		floor     int
		wantError bool
	}{
		{"floor=0 disabled (interval 5)", mk(5), 0, false},
		{"interval 5 < floor 30 → reject", mk(5), 30, true},
		{"interval 29 (boundary) < 30 → reject", mk(29), 30, true},
		{"interval 30 (boundary) == 30 → ok", mk(30), 30, false},
		{"interval 300 >= 30 → ok", mk(300), 30, false},
		{"cron-Cadence unaffected", cronCadence(), 30, false},
		{"nil cadence → ok", nil, 30, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIntervalFloor(tc.c, tc.floor)
			if tc.wantError && err == nil {
				t.Fatal("expected floor error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantError && !strings.Contains(err.Error(), "Beacons") {
				t.Errorf("floor error should suggest Beacons; got %v", err)
			}
		})
	}
}

func TestValidScheduleKind(t *testing.T) {
	t.Parallel()
	for _, k := range []ScheduleKind{ScheduleKindInterval, ScheduleKindCron} {
		if !ValidScheduleKind(k) {
			t.Errorf("ValidScheduleKind(%q) = false", k)
		}
	}
	for _, k := range []ScheduleKind{"", "Interval", "rate"} {
		if ValidScheduleKind(k) {
			t.Errorf("ValidScheduleKind(%q) = true", k)
		}
	}
}

func TestValidOverlapPolicy(t *testing.T) {
	t.Parallel()
	for _, p := range []OverlapPolicy{OverlapPolicySkip, OverlapPolicyQueue, OverlapPolicyParallel} {
		if !ValidOverlapPolicy(p) {
			t.Errorf("ValidOverlapPolicy(%q) = false", p)
		}
	}
	for _, p := range []OverlapPolicy{"", "Skip", "drop"} {
		if ValidOverlapPolicy(p) {
			t.Errorf("ValidOverlapPolicy(%q) = true", p)
		}
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

func TestInsert_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*Cadence)
		want string
	}{
		{"nil cadence", nil, "nil cadence"},
		{"empty id", func(c *Cadence) { c.ID = "" }, "empty id"},
		{"empty name", func(c *Cadence) { c.Name = "" }, "empty name"},
		{"empty aid", func(c *Cadence) { c.CreatedByAID = "" }, "empty created_by_aid"},
		{"invalid schedule_kind", func(c *Cadence) { c.ScheduleKind = "rate" }, "invalid schedule_kind"},
		{"interval without seconds", func(c *Cadence) { c.IntervalSeconds = nil }, "schedule_kind=interval requires"},
		{"interval seconds zero", func(c *Cadence) { c.IntervalSeconds = intptr(0) }, "interval_seconds must be > 0"},
		{"interval with cron", func(c *Cadence) { c.CronExpr = strptr("* * * * *") }, "must not carry cron_expr"},
		{"invalid overlap", func(c *Cadence) { c.OverlapPolicy = "drop" }, "invalid overlap_policy"},
		{"invalid kind", func(c *Cadence) { c.Kind = "bogus" }, "invalid kind"},
		{"scenario without name", func(c *Cadence) { c.ScenarioName = nil }, "kind=scenario requires"},
		{"scenario with module", func(c *Cadence) { c.Module = strptr("core.cmd.shell") }, "must not carry module"},
		{"empty target", func(c *Cadence) { c.Target = nil }, "empty target"},
		{"invalid batch_mode", func(c *Cadence) { m := BatchMode("turbo"); c.BatchMode = &m }, "invalid batch_mode"},
		{"batch_size zero", func(c *Cadence) { c.BatchSize = intptr(0) }, "batch_size must be > 0"},
		{"batch_percent out of range", func(c *Cadence) { c.BatchPercent = intptr(101) }, "batch_percent must be in [1, 100]"},
		{"concurrency negative", func(c *Cadence) { c.Concurrency = intptr(-1) }, "concurrency must be > 0"},
		{"fail_threshold zero", func(c *Cadence) { c.FailThreshold = intptr(0) }, "fail_threshold must be > 0"},
		{"fail_threshold_percent out of range high", func(c *Cadence) { c.FailThresholdPercent = intptr(101) }, "fail_threshold_percent must be in [1, 100]"},
		{"fail_threshold_percent out of range low", func(c *Cadence) { c.FailThresholdPercent = intptr(0) }, "fail_threshold_percent must be in [1, 100]"},
		{"fail_threshold + percent XOR", func(c *Cadence) { c.FailThreshold = intptr(2); c.FailThresholdPercent = intptr(50) }, "fail_threshold and fail_threshold_percent are mutually exclusive"},
		{"invalid on_failure", func(c *Cadence) { f := OnFailure("noop"); c.OnFailure = &f }, "invalid on_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fdb := &fakeDB{}
			var arg *Cadence
			if tc.mut != nil {
				arg = intervalCadence()
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

func TestInsert_CronValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*Cadence)
		want string
	}{
		{"cron without expr", func(c *Cadence) { c.CronExpr = nil }, "schedule_kind=cron requires"},
		{"cron empty expr", func(c *Cadence) { c.CronExpr = strptr("") }, "schedule_kind=cron requires"},
		{"cron with interval", func(c *Cadence) { c.IntervalSeconds = intptr(60) }, "must not carry interval_seconds"},
		{"command without module", func(c *Cadence) { c.Module = nil }, "kind=command requires"},
		{"command with scenario", func(c *Cadence) { c.ScenarioName = strptr("converge") }, "must not carry scenario_name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cronCadence()
			tc.mut(c)
			err := Insert(ctx, &fakeDB{}, c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Insert err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInsert_HappyPath_Interval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	created := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return timestampsRow{created: created, updated: created}
	}}

	c := intervalCadence()
	if err := Insert(ctx, fdb, c); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !c.CreatedAt.Equal(created) || !c.UpdatedAt.Equal(created) {
		t.Errorf("timestamps = (%v, %v), want both %v", c.CreatedAt, c.UpdatedAt, created)
	}
	if fdb.queryRowCalls != 1 {
		t.Errorf("queryRowCalls = %d, want 1", fdb.queryRowCalls)
	}
	if !strings.Contains(fdb.queryRowSQL, "INSERT INTO cadences") {
		t.Errorf("unexpected SQL: %.200s", fdb.queryRowSQL)
	}
	// id + 24 recipe/schedule arguments = 25 (in sync with insertSQL $1..$25).
	if got := len(fdb.queryRowArgs); got != 25 {
		t.Errorf("queryRowArgs len = %d, want 25", got)
	}
	if fdb.queryRowArgs[0] != c.ID {
		t.Errorf("arg[0] = %v, want id %q", fdb.queryRowArgs[0], c.ID)
	}
}

func TestInsert_HappyPath_Cron(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return timestampsRow{created: time.Now().UTC(), updated: time.Now().UTC()}
	}}
	if err := Insert(ctx, fdb, cronCadence()); err != nil {
		t.Fatalf("Insert cron: %v", err)
	}
}

func TestInsert_AutoFillInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return timestampsRow{created: time.Now().UTC(), updated: time.Now().UTC()}
	}}
	c := intervalCadence()
	c.Input = nil // should be filled with `{}`
	if err := Insert(ctx, fdb, c); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// input argument is the 11th positional one (index 11: id, name, enabled, sk, iv,
	// cron, overlap, kind, scenario, module, target, input).
	if got, ok := fdb.queryRowArgs[11].([]byte); !ok || string(got) != "{}" {
		t.Errorf("input arg = %v, want []byte(`{}`)", fdb.queryRowArgs[11])
	}
}

func TestInsert_UniqueViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return errRow{err: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "cadences_pkey"}}
	}}
	err := Insert(ctx, fdb, intervalCadence())
	if !errors.Is(err, ErrCadenceExists) {
		t.Errorf("err = %v, want ErrCadenceExists", err)
	}
}

func TestInsert_FKViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row {
		return errRow{err: &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "cadences_created_by_aid_fk"}}
	}}
	err := Insert(ctx, fdb, intervalCadence())
	if err == nil || !strings.Contains(err.Error(), "FK violation") {
		t.Errorf("err = %v, want FK violation", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	_, err := Get(ctx, fdb, "01H")
	if !errors.Is(err, ErrCadenceNotFound) {
		t.Errorf("err = %v, want ErrCadenceNotFound", err)
	}
}

func TestGet_EmptyID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := Get(ctx, &fakeDB{}, "")
	if err == nil || !strings.Contains(err.Error(), "empty id") {
		t.Errorf("err = %v, want empty id", err)
	}
}

// fullCadenceRow is a pgx.Row returning the full set of 26 selectColumns
// columns in scanCadence order. It lets us verify Insert form -> scan
// round-trip without a real PG. interBatchSecs/interUnitSecs are float seconds
// (as EXTRACT EPOCH).
type fullCadenceRow struct {
	id                   string
	name                 string
	enabled              bool
	scheduleKind         string
	intervalSeconds      *int
	cronExpr             *string
	overlapPolicy        string
	kind                 string
	scenarioName         *string
	module               *string
	target               []byte
	input                []byte
	batchMode            *string
	batchSize            *int
	batchPercent         *int
	concurrency          *int
	failThreshold        *int
	failThresholdPercent *int
	interBatchSecs       *float64
	interUnitSecs        *float64
	requireAlive         *bool
	onFailure            *string
	nextRunAt            *time.Time
	lastRunAt            *time.Time
	createdByAID         string
	createdAt            time.Time
	updatedAt            time.Time
}

func (r fullCadenceRow) Scan(dest ...any) error {
	if len(dest) != 27 {
		return errors.New("fullCadenceRow: expected 27 dest")
	}
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.name
	*dest[2].(*bool) = r.enabled
	*dest[3].(*string) = r.scheduleKind
	*dest[4].(**int) = r.intervalSeconds
	*dest[5].(**string) = r.cronExpr
	*dest[6].(*string) = r.overlapPolicy
	*dest[7].(*string) = r.kind
	*dest[8].(**string) = r.scenarioName
	*dest[9].(**string) = r.module
	*dest[10].(*json.RawMessage) = json.RawMessage(r.target)
	*dest[11].(*[]byte) = r.input
	*dest[12].(**string) = r.batchMode
	*dest[13].(**int) = r.batchSize
	*dest[14].(**int) = r.batchPercent
	*dest[15].(**int) = r.concurrency
	*dest[16].(**int) = r.failThreshold
	*dest[17].(**int) = r.failThresholdPercent
	*dest[18].(**float64) = r.interBatchSecs
	*dest[19].(**float64) = r.interUnitSecs
	*dest[20].(**bool) = r.requireAlive
	*dest[21].(**string) = r.onFailure
	*dest[22].(**time.Time) = r.nextRunAt
	*dest[23].(**time.Time) = r.lastRunAt
	*dest[24].(*string) = r.createdByAID
	*dest[25].(*time.Time) = r.createdAt
	*dest[26].(*time.Time) = r.updatedAt
	return nil
}

// TestGet_ScanRoundTrip checks that a full row passes through scanCadence:
// enums, nullable fields, INTERVAL float->Duration, and jsonb bytes are restored
// into Cadence.
func TestGet_ScanRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	bm := string(BatchModeWindow)
	of := string(OnFailureAbort)
	next := created.Add(time.Hour)
	row := fullCadenceRow{
		id:                   "01H000000000000000000CAD00",
		name:                 "nightly-converge",
		enabled:              true,
		scheduleKind:         string(ScheduleKindInterval),
		intervalSeconds:      intptr(300),
		overlapPolicy:        string(OverlapPolicySkip),
		kind:                 string(KindScenario),
		scenarioName:         strptr("converge"),
		target:               []byte(`{"coven":"prod-eu"}`),
		input:                []byte(`{"force":true}`),
		batchMode:            &bm,
		batchPercent:         intptr(25),
		concurrency:          intptr(4),
		failThresholdPercent: intptr(40),
		interBatchSecs:       f64ptr(30),
		interUnitSecs:        f64ptr(2),
		requireAlive:         boolptr(true),
		onFailure:            &of,
		nextRunAt:            &next,
		createdByAID:         "archon-alice",
		createdAt:            created,
		updatedAt:            created,
	}
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return row }}

	c, err := Get(ctx, fdb, row.id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.ScheduleKind != ScheduleKindInterval || c.IntervalSeconds == nil || *c.IntervalSeconds != 300 {
		t.Errorf("schedule = %q/%v, want interval/300", c.ScheduleKind, c.IntervalSeconds)
	}
	if c.OverlapPolicy != OverlapPolicySkip || c.Kind != KindScenario {
		t.Errorf("overlap/kind = %q/%q", c.OverlapPolicy, c.Kind)
	}
	if c.BatchMode == nil || *c.BatchMode != BatchModeWindow {
		t.Errorf("batch_mode = %v, want window", c.BatchMode)
	}
	if c.FailThresholdPercent == nil || *c.FailThresholdPercent != 40 {
		t.Errorf("fail_threshold_percent = %v, want 40 (column round-trip)", c.FailThresholdPercent)
	}
	if c.InterBatchInterval == nil || *c.InterBatchInterval != 30*time.Second {
		t.Errorf("inter_batch_interval = %v, want 30s", c.InterBatchInterval)
	}
	if c.InterUnitInterval == nil || *c.InterUnitInterval != 2*time.Second {
		t.Errorf("inter_unit_interval = %v, want 2s", c.InterUnitInterval)
	}
	if c.OnFailure == nil || *c.OnFailure != OnFailureAbort {
		t.Errorf("on_failure = %v, want abort", c.OnFailure)
	}
	if c.NextRunAt == nil || !c.NextRunAt.Equal(next) {
		t.Errorf("next_run_at = %v, want %v", c.NextRunAt, next)
	}
	if string(c.Target) != `{"coven":"prod-eu"}` {
		t.Errorf("target = %s", c.Target)
	}
}

// TestGet_ScanRoundTrip_Nulls covers a cron case with NULL in all nullable
// fields: interval_seconds=NULL (cron does not carry interval),
// batch_*/inter_*/require_alive/on_failure/next_run_at/last_run_at=NULL, so the
// corresponding *fields in Cadence remain nil (fragile nil pointer mapping in
// scanCadence).
func TestGet_ScanRoundTrip_Nulls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	row := fullCadenceRow{
		id:            "01H000000000000000000CAD01",
		name:          "catalog-refresh",
		enabled:       true,
		scheduleKind:  string(ScheduleKindCron),
		cronExpr:      strptr("0 */6 * * *"),
		overlapPolicy: string(OverlapPolicyQueue),
		kind:          string(KindCommand),
		module:        strptr("core.cmd.shell"),
		target:        []byte(`["a.example"]`),
		input:         []byte(`{}`),
		// all nullable fields are left nil (structure fields' zero values).
		createdByAID: "archon-bob",
		createdAt:    created,
		updatedAt:    created,
	}
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return row }}

	c, err := Get(ctx, fdb, row.id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.ScheduleKind != ScheduleKindCron || c.CronExpr == nil || *c.CronExpr != "0 */6 * * *" {
		t.Errorf("schedule = %q/%v, want cron/expr", c.ScheduleKind, c.CronExpr)
	}
	if c.IntervalSeconds != nil {
		t.Errorf("IntervalSeconds = %v, want nil (cron case)", c.IntervalSeconds)
	}
	if c.BatchMode != nil || c.BatchSize != nil || c.BatchPercent != nil ||
		c.Concurrency != nil || c.FailThreshold != nil {
		t.Errorf("batch_* = %v/%v/%v/%v/%v, want all nil",
			c.BatchMode, c.BatchSize, c.BatchPercent, c.Concurrency, c.FailThreshold)
	}
	if c.InterBatchInterval != nil || c.InterUnitInterval != nil {
		t.Errorf("inter_* = %v/%v, want nil", c.InterBatchInterval, c.InterUnitInterval)
	}
	if c.RequireAlive != nil {
		t.Errorf("RequireAlive = %v, want nil", c.RequireAlive)
	}
	if c.OnFailure != nil {
		t.Errorf("OnFailure = %v, want nil", c.OnFailure)
	}
	if c.NextRunAt != nil || c.LastRunAt != nil {
		t.Errorf("next/last_run_at = %v/%v, want nil", c.NextRunAt, c.LastRunAt)
	}
}

func f64ptr(f float64) *float64 { return &f }
func boolptr(b bool) *bool      { return &b }

func TestUpdate_Validation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := intervalCadence()
	c.OverlapPolicy = "drop"
	err := Update(ctx, &fakeDB{}, c)
	if err == nil || !strings.Contains(err.Error(), "invalid overlap_policy") {
		t.Errorf("Update err = %v, want invalid overlap_policy", err)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	err := Update(ctx, fdb, intervalCadence())
	if !errors.Is(err, ErrCadenceNotFound) {
		t.Errorf("err = %v, want ErrCadenceNotFound", err)
	}
}

func TestUpdate_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return timestampsRow{created: now, updated: now} }}
	c := intervalCadence()
	c.Enabled = false
	if err := Update(ctx, fdb, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.Contains(fdb.queryRowSQL, "UPDATE cadences") {
		t.Errorf("unexpected SQL: %.200s", fdb.queryRowSQL)
	}
	// UPDATE does not write created_by_aid: id + 23 recipe/schedule args = 24 ($1..$24).
	if got := len(fdb.queryRowArgs); got != 24 {
		t.Errorf("queryRowArgs len = %d, want 24 (id + 23)", got)
	}
}

func TestSetEnabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("empty id", func(t *testing.T) {
		if err := SetEnabled(ctx, &fakeDB{}, "", true); err == nil || !strings.Contains(err.Error(), "empty id") {
			t.Errorf("err = %v, want empty id", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		fdb := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0"), execTagSet: true}
		if err := SetEnabled(ctx, fdb, "01H", false); !errors.Is(err, ErrCadenceNotFound) {
			t.Errorf("err = %v, want ErrCadenceNotFound", err)
		}
	})
	t.Run("happy", func(t *testing.T) {
		fdb := &fakeDB{}
		if err := SetEnabled(ctx, fdb, "01H", false); err != nil {
			t.Fatalf("SetEnabled: %v", err)
		}
		if !strings.Contains(fdb.execSQL, "enabled    = $2") {
			t.Errorf("SQL without enabled-set: %.200s", fdb.execSQL)
		}
		if fdb.execArgs[1] != false {
			t.Errorf("enabled arg = %v, want false", fdb.execArgs[1])
		}
	})
}

func TestDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("empty id", func(t *testing.T) {
		if err := Delete(ctx, &fakeDB{}, ""); err == nil || !strings.Contains(err.Error(), "empty id") {
			t.Errorf("err = %v, want empty id", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		fdb := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0"), execTagSet: true}
		if err := Delete(ctx, fdb, "01H"); !errors.Is(err, ErrCadenceNotFound) {
			t.Errorf("err = %v, want ErrCadenceNotFound", err)
		}
	})
	t.Run("happy", func(t *testing.T) {
		fdb := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1"), execTagSet: true}
		if err := Delete(ctx, fdb, "01H"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if !strings.Contains(fdb.execSQL, "DELETE FROM cadences") {
			t.Errorf("unexpected SQL: %.200s", fdb.execSQL)
		}
	})
}

func TestList_CountError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return errRow{err: errors.New("count boom")} }}
	_, _, err := List(ctx, fdb, ListFilter{}, 0, 10)
	if err == nil || !strings.Contains(err.Error(), "count") {
		t.Errorf("err = %v, want count error", err)
	}
}

func TestList_FilterArgs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// COUNT returns total through queryRowFunc; Query itself returns an error
	// (rows are not configured), which is enough to verify passed filter args.
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return countRow{n: 0} }}
	_, _, _ = List(ctx, fdb, ListFilter{EnabledOnly: true, Kind: KindCommand}, 5, 20)
	if fdb.queryArgs[0] != true {
		t.Errorf("count/list arg[0] (enabledOnly) = %v, want true", fdb.queryArgs[0])
	}
	if fdb.queryArgs[1] != string(KindCommand) {
		t.Errorf("list arg[1] (kind) = %v, want command", fdb.queryArgs[1])
	}
	if fdb.queryArgs[2] != 20 || fdb.queryArgs[3] != 5 {
		t.Errorf("list limit/offset = %v/%v, want 20/5", fdb.queryArgs[2], fdb.queryArgs[3])
	}
}

type countRow struct{ n int }

func (r countRow) Scan(dest ...any) error {
	*dest[0].(*int) = r.n
	return nil
}

// cadenceScanColumns is the reference column order for a `cadences` row as
// scanCadence assigns row.Scan arguments (crud.go). This is the only source of
// truth for the guard below: if a column is added/reordered in
// insertSQL/selectColumns/scanCadence without syncing this list, the test fails
// (fragile positional pattern, parity guard-059 voyage_targets).
//
// insertSQL writes all columns except DB-managed created_at/updated_at
// (RETURNING), in the same relative order, so we check it as a prefix of the
// reference.
var cadenceScanColumns = []string{
	"id", "name", "enabled",
	"schedule_kind", "interval_seconds", "cron_expr", "overlap_policy",
	"kind", "scenario_name", "module", "target", "input",
	"batch_mode", "batch_size", "batch_percent", "concurrency", "fail_threshold", "fail_threshold_percent",
	"inter_batch_interval", "inter_unit_interval", "require_alive", "on_failure",
	"next_run_at", "last_run_at",
	"created_by_aid", "created_at", "updated_at",
}

// TestColumnsMatchScanOrder guards against positional column ORDER drift. The
// existing len(dest)!=26 only catches the count; this test catches reordering:
// it compares column names from selectColumns (full set) and insertSQL (prefix
// without RETURNING columns) against [cadenceScanColumns] by content AND order.
func TestColumnsMatchScanOrder(t *testing.T) {
	t.Parallel()

	sel := parseColumnList(selectColumns)
	if len(sel) != len(cadenceScanColumns) {
		t.Fatalf("selectColumns = %d columns %v, want %d %v", len(sel), sel, len(cadenceScanColumns), cadenceScanColumns)
	}
	for i, want := range cadenceScanColumns {
		if sel[i] != want {
			t.Errorf("selectColumns[%d] = %q, want %q (drift with scanCadence)", i, sel[i], want)
		}
	}

	ins := parseInsertColumns(insertSQL)
	if len(ins) != len(cadenceScanColumns)-2 { // minus created_at, updated_at (RETURNING)
		t.Fatalf("insertSQL = %d columns %v, want %d", len(ins), ins, len(cadenceScanColumns)-2)
	}
	for i, c := range ins {
		if c != cadenceScanColumns[i] {
			t.Errorf("insertSQL column[%d] = %q, want %q (drift with selectColumns/scanCadence)", i, c, cadenceScanColumns[i])
		}
	}
}

// parseColumnList parses a SELECT column list (the selectColumns constant) into
// names in order: split by commas + unpack `EXTRACT(EPOCH FROM x)::float8` -> x.
// Other expressions/casts are not expected; if they appear, the guard will catch
// the noise.
func parseColumnList(list string) []string {
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name := normalizeColumn(p)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// parseInsertColumns extracts column names from INSERT INTO t (...) VALUES (...):
// it takes the contents of the first pair of parentheses (column-list) and
// parses it as a list.
func parseInsertColumns(sql string) []string {
	open := strings.Index(sql, "(")
	close := strings.Index(sql, ")")
	if open < 0 || close < 0 || close < open {
		return nil
	}
	return parseColumnList(sql[open+1 : close])
}

// normalizeColumn converts one column-list item to the bare name:
// EXTRACT(EPOCH FROM x)::float8 -> x; otherwise trim and drop trailing ::cast.
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
