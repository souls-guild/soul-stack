package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- fakes ---

// fakeCadenceStore — mock [CadenceStore]. SQL router by substring (parity
// fakeVoyageStore). Counts write calls; selectByID/list are configurable.
// voyages queries (FROM voyages) serve `/runs`.
type fakeCadenceStore struct {
	mu sync.Mutex

	insertCalls    int
	insertArgs     []any // last positional INSERT args (for batch/percent-column guards)
	updateCalls    int
	updateArgs     []any // last positional UPDATE args
	setEnabledArgs []bool
	deleteCalls    int

	// next return value of write ops.
	insertErr       error // QueryRow Insert → this err in Scan (nil → timestamps)
	updateNoRows    bool  // Update → pgx.ErrNoRows (not-found)
	setEnabledNoRow bool  // SetEnabled → RowsAffected 0 (not-found)
	deleteNoRow     bool  // Delete → RowsAffected 0 (not-found)

	selectByID func(id string) pgx.Row // selectByIDSQL
	listRows   func() (pgx.Rows, error)
	listCount  int

	// runs: voyages list/count.
	voyageListRows  func() (pgx.Rows, error)
	voyageListCount int

	// notify (ADR-052 §m): tx aspect of Create with a notify block.
	committed         bool     // tx.Commit called
	rolledBack        bool     // tx.Rollback called
	insertTidingCalls int      // INSERT INTO tidings (persistent notify rules)
	insertTidingArgs  [][]any  // positional args of each InsertTiding (order)
	insertTidingErr   error    // next INSERT INTO tidings → this err (atomicity)
	heraldKnown       []string // existing heralds (prepareNotify existence check); nil → any exists
}

func (f *fakeCadenceStore) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "UPDATE cadences SET\n    enabled"):
		f.mu.Lock()
		if len(args) >= 2 {
			if b, ok := args[1].(bool); ok {
				f.setEnabledArgs = append(f.setEnabledArgs, b)
			}
		}
		noRow := f.setEnabledNoRow
		f.mu.Unlock()
		if noRow {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "DELETE FROM cadences"):
		f.mu.Lock()
		f.deleteCalls++
		noRow := f.deleteNoRow
		f.mu.Unlock()
		if noRow {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, errors.New("fakeCadenceStore.Exec: unexpected SQL: " + sql)
}

func (f *fakeCadenceStore) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO cadences"):
		f.mu.Lock()
		f.insertCalls++
		f.insertArgs = args
		f.mu.Unlock()
		if f.insertErr != nil {
			return cadenceErrRow{err: f.insertErr}
		}
		return cadenceScalarRow{vals: []any{time.Now().UTC(), time.Now().UTC()}}
	case strings.Contains(sql, "UPDATE cadences SET") && strings.Contains(sql, "RETURNING created_at, updated_at"):
		f.mu.Lock()
		f.updateCalls++
		f.updateArgs = args
		noRows := f.updateNoRows
		f.mu.Unlock()
		if noRows {
			return cadenceErrRow{err: pgx.ErrNoRows}
		}
		return cadenceScalarRow{vals: []any{time.Now().UTC(), time.Now().UTC()}}
	case strings.Contains(sql, "FROM cadences\nWHERE id = $1"):
		if f.selectByID != nil {
			return f.selectByID(args[0].(string))
		}
		return cadenceErrRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "INSERT INTO tidings"):
		// Persistent Cadence notify rule (ADR-052 §m): RETURNING created_at,
		// updated_at. Record order/args; insertTidingErr simulates an FK/collision
		// for the atomicity test (rollback of the whole tx).
		f.mu.Lock()
		f.insertTidingCalls++
		argsCopy := append([]any(nil), args...)
		f.insertTidingArgs = append(f.insertTidingArgs, argsCopy)
		err := f.insertTidingErr
		f.mu.Unlock()
		if err != nil {
			return cadenceErrRow{err: err}
		}
		return cadenceScalarRow{vals: []any{time.Now().UTC(), time.Now().UTC()}}
	case strings.Contains(sql, "FROM heralds"):
		// Channel existence check in prepareNotify (SelectHeraldByName). heraldKnown=nil
		// → any herald exists (minimal Herald row). Otherwise match by name.
		name, _ := args[0].(string)
		if f.heraldKnown != nil {
			found := false
			for _, h := range f.heraldKnown {
				if h == name {
					found = true
					break
				}
			}
			if !found {
				return cadenceErrRow{err: pgx.ErrNoRows}
			}
		}
		// scanHerald: name, type, config, secret_ref, enabled, created_at, updated_at, created_by_aid.
		return cadenceScalarRow{vals: []any{name, "webhook", []byte(`{}`), nil, true, time.Now().UTC(), time.Now().UTC(), nil}}
	case strings.Contains(sql, "SELECT COUNT(*) FROM cadences"):
		return cadenceScalarRow{vals: []any{f.listCount}}
	case strings.Contains(sql, "SELECT COUNT(*) FROM voyages"):
		return cadenceScalarRow{vals: []any{f.voyageListCount}}
	}
	return cadenceErrRow{err: errors.New("fakeCadenceStore.QueryRow: unexpected SQL: " + sql)}
}

// BeginTx returns a tx wrapper routing Exec/QueryRow back into the store
// (ADR-052 §m Create-tx). Commit/Rollback set flags for the atomicity
// guard test.
func (f *fakeCadenceStore) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeCadenceTx{store: f}, nil
}

// fakeCadenceTx — pgx.Tx wrapper over fakeCadenceStore (parity fakeVoyageTx).
type fakeCadenceTx struct{ store *fakeCadenceStore }

func (t *fakeCadenceTx) Begin(context.Context) (pgx.Tx, error)                    { return t, nil }
func (t *fakeCadenceTx) BeginFunc(_ context.Context, fn func(pgx.Tx) error) error { return fn(t) }
func (t *fakeCadenceTx) Commit(context.Context) error {
	t.store.mu.Lock()
	t.store.committed = true
	t.store.mu.Unlock()
	return nil
}
func (t *fakeCadenceTx) Rollback(context.Context) error {
	t.store.mu.Lock()
	t.store.rolledBack = true
	t.store.mu.Unlock()
	return nil
}
func (t *fakeCadenceTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("fakeCadenceTx.CopyFrom: unexpected")
}
func (t *fakeCadenceTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { panic("unexpected") }
func (t *fakeCadenceTx) LargeObjects() pgx.LargeObjects                         { panic("unexpected") }
func (t *fakeCadenceTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected")
}
func (t *fakeCadenceTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.store.Exec(ctx, sql, args...)
}
func (t *fakeCadenceTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.store.Query(ctx, sql, args...)
}
func (t *fakeCadenceTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.store.QueryRow(ctx, sql, args...)
}
func (t *fakeCadenceTx) Conn() *pgx.Conn { return nil }

func (f *fakeCadenceStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM voyages"):
		if f.voyageListRows != nil {
			return f.voyageListRows()
		}
		return &emptyRows{}, nil
	case strings.Contains(sql, "FROM cadences"):
		if f.listRows != nil {
			return f.listRows()
		}
		return &emptyRows{}, nil
	}
	return &emptyRows{}, nil
}

// CopyFrom — voyage.ExecQueryRower requirement; not called on the read-only `/runs` path.
func (f *fakeCadenceStore) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("fakeCadenceStore.CopyFrom: unexpected (cadence CRUD does not use CopyFrom)")
}

type cadenceErrRow struct{ err error }

func (r cadenceErrRow) Scan(...any) error { return r.err }

type cadenceScalarRow struct{ vals []any }

func (r cadenceScalarRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch p := d.(type) {
		case *time.Time:
			*p = r.vals[i].(time.Time)
		case *int:
			*p = r.vals[i].(int)
		}
	}
	return nil
}

// cadenceFullRow — Row for cadence.scanCadence (26 selectColumns in
// scanCadence order). Minimal set for a GET/list/PATCH round-trip.
type cadenceFullRow struct {
	id           string
	name         string
	enabled      bool
	scheduleKind string
	overlap      string
	kind         string
	scenarioName *string
	module       *string
	intervalSecs *int
	// Counterpart fields of mutually-exclusive pairs (value stored in DB) — for
	// PATCH format-switch tests (percent↔absolute). nil → column NULL.
	batchSize            *int
	batchPercent         *int
	failThreshold        *int
	failThresholdPercent *int
}

func (r cadenceFullRow) Scan(dest ...any) error {
	if len(dest) != 27 {
		return errors.New("cadenceFullRow: expected 27 dest")
	}
	now := time.Now().UTC()
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.name
	*dest[2].(*bool) = r.enabled
	*dest[3].(*string) = r.scheduleKind
	*dest[4].(**int) = r.intervalSecs
	*dest[5].(**string) = nil // cron_expr
	*dest[6].(*string) = r.overlap
	*dest[7].(*string) = r.kind
	*dest[8].(**string) = r.scenarioName
	*dest[9].(**string) = r.module
	*dest[10].(*json.RawMessage) = json.RawMessage(`{"service":"web"}`)
	*dest[11].(*[]byte) = []byte(`{}`)
	*dest[12].(**string) = nil                 // batch_mode
	*dest[13].(**int) = r.batchSize            // batch_size
	*dest[14].(**int) = r.batchPercent         // batch_percent
	*dest[15].(**int) = nil                    // concurrency
	*dest[16].(**int) = r.failThreshold        // fail_threshold
	*dest[17].(**int) = r.failThresholdPercent // fail_threshold_percent
	*dest[18].(**float64) = nil
	*dest[19].(**float64) = nil
	*dest[20].(**bool) = nil   // require_alive
	*dest[21].(**string) = nil // on_failure
	*dest[22].(**time.Time) = nil
	*dest[23].(**time.Time) = nil
	*dest[24].(*string) = "archon-alice"
	*dest[25].(*time.Time) = now
	*dest[26].(*time.Time) = now
	return nil
}

// --- helpers ---

// newCadenceHandler — handler with a non-empty default scenario resolve (one
// incarnation) so scenario-OK tests pass the empty-target guard
// (cadence_empty_target). incReader=nil → the per-incarnation scope loop is
// skipped (parity Voyage). Empty-scope tests build a handler with an explicit
// out:nil resolver.
func newCadenceHandler(store *fakeCadenceStore, enf apimiddleware.PermissionChecker) *CadenceHandler {
	// pollFloorSeconds=0 → floor check disabled: existing CRUD tests don't hit
	// the floor limit (checked separately by newCadenceHandlerFloor).
	// tidingInvalidator=nil → notify invalidation is a no-op (CRUD tests without notify).
	return NewCadenceHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, nil, enf, nil, nil, 0, nil)
}

// fakeTidingInvalidator — mock [TidingInvalidator]; counts calls and records the
// argument (ADR-052 §m: invalidation after commit with notify).
type fakeTidingInvalidator struct {
	calls int
	names []string
}

func (f *fakeTidingInvalidator) InvalidateTidings(_ context.Context, name string) {
	f.calls++
	f.names = append(f.names, name)
}

// newCadenceHandlerNotify — handler with a tidingInvalidator for notify tests
// (ADR-052 §m). bare scenario resolve (one incarnation), incReader=nil.
func newCadenceHandlerNotify(store *fakeCadenceStore, enf apimiddleware.PermissionChecker, inv TidingInvalidator) *CadenceHandler {
	return NewCadenceHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, nil, enf, nil, inv, 0, nil)
}

// newCadenceHandlerFloor — handler with a floor limit (ADR-046 Pass B): interval <
// floorSeconds → 422 on Create/Patch. bare-check-only (scenarioResolver/incReader
// as in newCadenceHandler).
func newCadenceHandlerFloor(store *fakeCadenceStore, enf apimiddleware.PermissionChecker, floorSeconds int) *CadenceHandler {
	return NewCadenceHandler(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, nil, enf, nil, nil, floorSeconds, nil)
}

// newCadenceHandlerScoped — handler with scenarioResolver + incReader for
// per-target coven-scope-check tests (ADR-046 §7). parity newVoyageHandler with a
// non-empty incReader.
func newCadenceHandlerScoped(store *fakeCadenceStore, sc VoyageScenarioResolver, incReader IncarnationContextReader, enf apimiddleware.PermissionChecker) *CadenceHandler {
	return NewCadenceHandler(store, sc, incReader, enf, nil, nil, 0, nil)
}

func cadenceReq(method, url, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, url, http.NoBody)
	} else {
		r = httptest.NewRequest(method, url, strings.NewReader(body))
	}
	return r.WithContext(apimiddleware.InjectClaimsForTest(r.Context(), &keeperjwt.Claims{Subject: "archon-alice"}))
}

func cadenceReqID(method, url, id, body string) *http.Request {
	r := cadenceReq(method, url, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// scenarioRow / scenarioStore — helper for selectByID returning a scenario Cadence.
func scenarioStore() *fakeCadenceStore {
	return &fakeCadenceStore{
		selectByID: func(id string) pgx.Row {
			return cadenceFullRow{
				id: id, name: "nightly", enabled: true,
				scheduleKind: "interval", intervalSecs: intp(300),
				overlap: "skip", kind: "scenario", scenarioName: strp("converge"),
			}
		},
	}
}

func intp(n int) *int       { return &n }
func strp(s string) *string { return &s }

// --- tests: create ---

func TestCadenceCreate_OK_Interval(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"nightly","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"}}`))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var reply cadenceCreateReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Name != "nightly" || !reply.Enabled {
		t.Errorf("reply = %+v, want name=nightly enabled=true", reply)
	}
	if reply.NextRunAt == nil {
		t.Error("next_run_at must be computed on creation (interval)")
	}
	if store.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", store.insertCalls)
	}
}

func TestCadenceCreate_OK_Cron(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"hourly","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var reply cadenceCreateReply
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.NextRunAt == nil {
		t.Error("next_run_at must be computed on creation (cron)")
	}
}

// --- tests: recipe string batch fields (ADR-043 amendment 2026-06-09, S3) ---

// Positions of batch/percent columns in insertSQL args (id=args[0]; see recipeArgs
// order: batch_size $14, batch_percent $15, fail_threshold $17,
// fail_threshold_percent $18 → 0-indexed args respectively).
const (
	cadenceArgBatchSize            = 13
	cadenceArgBatchPercent         = 14
	cadenceArgFailThreshold        = 16
	cadenceArgFailThresholdPercent = 17
)

// argInt extracts an int argument of a positional INSERT/UPDATE (recipeArgs stores
// a dereferenced int or a nil interface for NULL).
func argInt(t *testing.T, args []any, idx int) (val int, isNull bool) {
	t.Helper()
	if idx >= len(args) {
		t.Fatalf("arg[%d] out of range (len=%d)", idx, len(args))
	}
	if args[idx] == nil {
		return 0, true
	}
	n, ok := args[idx].(int)
	if !ok {
		t.Fatalf("arg[%d] = %T, want int/nil", idx, args[idx])
	}
	return n, false
}

// TestCadenceCreate_BatchPercentString — `batch:"20%"` → column batch_percent=20,
// batch_size NULL. Spawn-scope resolve already reaches it via the existing path
// (BuildVoyage effectiveBatchSize).
func TestCadenceCreate_BatchPercentString(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"batch":"20%"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.insertArgs, cadenceArgBatchPercent); isNull || v != 20 {
		t.Errorf("batch_percent column = %v (null=%v), want 20", v, isNull)
	}
	if _, isNull := argInt(t, store.insertArgs, cadenceArgBatchSize); !isNull {
		t.Errorf("batch_size must be NULL when batch=20%%")
	}
}

// TestCadenceCreate_BatchHostsString — `batch:"5"` → column batch_size=5,
// batch_percent NULL.
func TestCadenceCreate_BatchHostsString(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"batch":"5"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.insertArgs, cadenceArgBatchSize); isNull || v != 5 {
		t.Errorf("batch_size column = %v (null=%v), want 5", v, isNull)
	}
	if _, isNull := argInt(t, store.insertArgs, cadenceArgBatchPercent); !isNull {
		t.Errorf("batch_percent must be NULL when batch=5")
	}
}

// TestCadenceCreate_MaxFailuresPercentString — `max_failures:"25%"` → NEW column
// fail_threshold_percent=25, fail_threshold NULL. Resolve to absolute happens at
// spawn-scope (BuildVoyage); here only the percent column write is checked.
func TestCadenceCreate_MaxFailuresPercentString(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"max_failures":"25%"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.insertArgs, cadenceArgFailThresholdPercent); isNull || v != 25 {
		t.Errorf("fail_threshold_percent column = %v (null=%v), want 25", v, isNull)
	}
	if _, isNull := argInt(t, store.insertArgs, cadenceArgFailThreshold); !isNull {
		t.Errorf("fail_threshold must be NULL when max_failures=25%%")
	}
}

// TestCadenceCreate_MaxFailuresAbsoluteString — `max_failures:"3"` → column
// fail_threshold=3, fail_threshold_percent NULL.
func TestCadenceCreate_MaxFailuresAbsoluteString(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"max_failures":"3"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.insertArgs, cadenceArgFailThreshold); isNull || v != 3 {
		t.Errorf("fail_threshold column = %v (null=%v), want 3", v, isNull)
	}
	if _, isNull := argInt(t, store.insertArgs, cadenceArgFailThresholdPercent); !isNull {
		t.Errorf("fail_threshold_percent must be NULL when max_failures=3")
	}
}

// TestCadenceCreate_BatchConflict422 — `batch` + numeric batch_percent → 422
// (voyage_batch_spec_conflict), Insert not called.
func TestCadenceCreate_BatchConflict422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"batch":"20%","batch_percent":30}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_spec_conflict") {
		t.Errorf("body should mention voyage_batch_spec_conflict: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// TestCadenceCreate_MaxFailuresConflict422 — `max_failures` + numeric
// fail_threshold → 422, Insert not called.
func TestCadenceCreate_MaxFailuresConflict422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"max_failures":"25%","fail_threshold":2}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "voyage_batch_spec_conflict") {
		t.Errorf("body should mention voyage_batch_spec_conflict: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// TestCadenceCreate_BatchMalformed422 — garbage `batch` → 422, Insert not called.
func TestCadenceCreate_BatchMalformed422(t *testing.T) {
	for _, bad := range []string{`"2x"`, `"abc"`, `"50%%"`, `"-1"`, `"101%"`} {
		store := &fakeCadenceStore{}
		h := newCadenceHandler(store, allowAll())
		rec := httptest.NewRecorder()
		h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
			`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"batch":`+bad+`}`))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("batch=%s: status = %d, want 422; body=%s", bad, rec.Code, rec.Body.String())
		}
		if store.insertCalls != 0 {
			t.Errorf("batch=%s: insertCalls = %d, want 0", bad, store.insertCalls)
		}
	}
}

// TestCadenceCreate_MaxFailuresMalformed422 — garbage `max_failures` → 422.
func TestCadenceCreate_MaxFailuresMalformed422(t *testing.T) {
	for _, bad := range []string{`"3x"`, `"abc"`, `"0%"`, `"101%"`} {
		store := &fakeCadenceStore{}
		h := newCadenceHandler(store, allowAll())
		rec := httptest.NewRecorder()
		h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
			`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"max_failures":`+bad+`}`))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("max_failures=%s: status = %d, want 422; body=%s", bad, rec.Code, rec.Body.String())
		}
		if store.insertCalls != 0 {
			t.Errorf("max_failures=%s: insertCalls = %d, want 0", bad, store.insertCalls)
		}
	}
}

// TestCadenceCreate_BackcompatNumericFields — legacy numeric batch_size/
// fail_threshold (without string fields) work as before → correct columns.
func TestCadenceCreate_BackcompatNumericFields(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"batch_size":4,"fail_threshold":2}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.insertArgs, cadenceArgBatchSize); isNull || v != 4 {
		t.Errorf("batch_size = %v (null=%v), want 4", v, isNull)
	}
	if v, isNull := argInt(t, store.insertArgs, cadenceArgFailThreshold); isNull || v != 2 {
		t.Errorf("fail_threshold = %v (null=%v), want 2", v, isNull)
	}
	if _, isNull := argInt(t, store.insertArgs, cadenceArgFailThresholdPercent); !isNull {
		t.Errorf("fail_threshold_percent must be NULL (backcompat without percent)")
	}
}

// TestCadencePatch_MaxFailuresPercentString — PATCH `max_failures:"25%"` writes
// column fail_threshold_percent=25 (Update args).
func TestCadencePatch_MaxFailuresPercentString(t *testing.T) {
	store := scenarioStore()
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"max_failures":"25%"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.updateArgs, cadenceArgFailThresholdPercent); isNull || v != 25 {
		t.Errorf("PATCH fail_threshold_percent column = %v (null=%v), want 25", v, isNull)
	}
}

// TestCadencePatch_BatchConflict422 — PATCH `batch` + numeric batch_size in one
// body → 422, Update not called.
func TestCadencePatch_BatchConflict422(t *testing.T) {
	store := scenarioStore()
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"batch":"20%","batch_size":3}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0", store.updateCalls)
	}
}

// scenarioStoreWith — scenario Cadence with given stored batch/fail fields
// (for PATCH format-switch tests of a mutually-exclusive pair, Batch S3 review).
func scenarioStoreWith(row cadenceFullRow) *fakeCadenceStore {
	return &fakeCadenceStore{
		selectByID: func(id string) pgx.Row {
			row.id = id
			row.name = "nightly"
			row.enabled = true
			row.scheduleKind = "interval"
			row.intervalSecs = intp(300)
			row.overlap = "skip"
			row.kind = "scenario"
			row.scenarioName = strp("converge")
			return row
		},
	}
}

// TestCadencePatch_MaxFailuresPercentToAbsolute — guard (Batch S3 review): PATCH
// `max_failures:"3"` over a STORED fail_threshold_percent=25 switches to
// absolute — clears the counterpart percent field, doesn't hit XOR-validate 422.
func TestCadencePatch_MaxFailuresPercentToAbsolute(t *testing.T) {
	store := scenarioStoreWith(cadenceFullRow{failThresholdPercent: intp(25)})
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"max_failures":"3"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (switching percent->absolute must not hit XOR-422); body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.updateArgs, cadenceArgFailThreshold); isNull || v != 3 {
		t.Errorf("PATCH fail_threshold = %v (null=%v), want 3", v, isNull)
	}
	if _, isNull := argInt(t, store.updateArgs, cadenceArgFailThresholdPercent); !isNull {
		t.Errorf("counterpart fail_threshold_percent must be cleared when setting absolute")
	}
}

// TestCadencePatch_MaxFailuresAbsoluteToPercent — reverse direction: PATCH
// `max_failures:"25%"` over a STORED fail_threshold=3 switches to percent,
// clearing the counterpart absolute field.
func TestCadencePatch_MaxFailuresAbsoluteToPercent(t *testing.T) {
	store := scenarioStoreWith(cadenceFullRow{failThreshold: intp(3)})
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"max_failures":"25%"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.updateArgs, cadenceArgFailThresholdPercent); isNull || v != 25 {
		t.Errorf("PATCH fail_threshold_percent = %v (null=%v), want 25", v, isNull)
	}
	if _, isNull := argInt(t, store.updateArgs, cadenceArgFailThreshold); !isNull {
		t.Errorf("counterpart fail_threshold must be cleared when setting percent")
	}
}

// TestCadencePatch_BatchPercentToHosts — PATCH `batch:"5"` over a stored
// batch_percent=20 switches to absolute size, clearing the counterpart percent.
func TestCadencePatch_BatchPercentToHosts(t *testing.T) {
	store := scenarioStoreWith(cadenceFullRow{batchPercent: intp(20)})
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"batch":"5"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.updateArgs, cadenceArgBatchSize); isNull || v != 5 {
		t.Errorf("PATCH batch_size = %v (null=%v), want 5", v, isNull)
	}
	if _, isNull := argInt(t, store.updateArgs, cadenceArgBatchPercent); !isNull {
		t.Errorf("counterpart batch_percent must be cleared when setting batch_size")
	}
}

// TestCadencePatch_BatchHostsToPercent — reverse: PATCH `batch:"20%"` over a
// stored batch_size=5 switches to percent, clearing the counterpart size.
func TestCadencePatch_BatchHostsToPercent(t *testing.T) {
	store := scenarioStoreWith(cadenceFullRow{batchSize: intp(5)})
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"batch":"20%"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.updateArgs, cadenceArgBatchPercent); isNull || v != 20 {
		t.Errorf("PATCH batch_percent = %v (null=%v), want 20", v, isNull)
	}
	if _, isNull := argInt(t, store.updateArgs, cadenceArgBatchSize); !isNull {
		t.Errorf("counterpart batch_size must be cleared when setting batch_percent")
	}
}

// TestCadencePatch_MaxFailuresKeepsUntouchedPair — guard for "nil-req touches
// nothing": PATCH without max_failures/fail_threshold does NOT clear the stored
// fail_threshold_percent (only a name change).
func TestCadencePatch_MaxFailuresKeepsUntouchedPair(t *testing.T) {
	store := scenarioStoreWith(cadenceFullRow{failThresholdPercent: intp(25)})
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"name":"renamed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if v, isNull := argInt(t, store.updateArgs, cadenceArgFailThresholdPercent); isNull || v != 25 {
		t.Errorf("stored fail_threshold_percent = %v (null=%v) must remain 25 (PATCH did not touch the pair)", v, isNull)
	}
}

// --- tests: period floor limit (ADR-046 Pass B) ---

// TestCadenceCreate_IntervalBelowFloor422 — create an interval Cadence with
// interval_seconds < poll_floor (30) → 422, Insert NOT called (reject before SQL).
// Boundary 29/30: 29 reject, 30 ok.
func TestCadenceCreate_IntervalBelowFloor422(t *testing.T) {
	cases := []struct {
		name     string
		interval int
		want     int
		insert   int
	}{
		{"interval 5 → 422", 5, http.StatusUnprocessableEntity, 0},
		{"interval 29 (boundary) -> 422", 29, http.StatusUnprocessableEntity, 0},
		{"interval 30 (boundary) -> 201", 30, http.StatusCreated, 1},
		{"interval 300 → 201", 300, http.StatusCreated, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCadenceStore{}
			h := newCadenceHandlerFloor(store, allowAll(), 30)
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(
				`{"name":"x","schedule_kind":"interval","interval_seconds":%d,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"}}`,
				tc.interval)
			h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", body))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if store.insertCalls != tc.insert {
				t.Errorf("insertCalls = %d, want %d", store.insertCalls, tc.insert)
			}
			if tc.want == http.StatusUnprocessableEntity && !strings.Contains(rec.Body.String(), "Beacons") {
				t.Errorf("floor-422 should hint at Beacons; body=%s", rec.Body.String())
			}
		})
	}
}

// TestCadenceCreate_CronUnaffectedByFloor — a cron Cadence (interval_seconds NULL)
// is unaffected by the floor limit even at `*/1` (minute granularity ≥ floor).
func TestCadenceCreate_CronUnaffectedByFloor(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandlerFloor(store, allowAll(), 30)
	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"freq","schedule_kind":"cron","cron_expr":"* * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("cron should not fail on floor; status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1 (cron passed floor)", store.insertCalls)
	}
}

// TestCadencePatch_IntervalBelowFloor422 — PATCH changes interval_seconds to sub-floor
// (10) → 422, Update NOT called. Current row (scenarioStore) is interval 300.
func TestCadencePatch_IntervalBelowFloor422(t *testing.T) {
	store := scenarioStore()
	h := newCadenceHandlerFloor(store, allowAll(), 30)
	id := audit.NewULID()
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id,
		`{"interval_seconds":10}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0 (floor-reject before Update)", store.updateCalls)
	}
}

// interval + cron together → 422 (XOR, cadence.validate).
func TestCadenceCreate_IntervalAndCron422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"cron_expr":"0 * * * *","overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (validate before SQL)", store.insertCalls)
	}
}

// invalid overlap_policy → 422.
func TestCadenceCreate_BadOverlap422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"bogus","kind":"scenario","scenario_name":"converge","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// kind=scenario without scenario_name → 422.
func TestCadenceCreate_ScenarioMissingName422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"scenario","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// kind=command carrying scenario_name → 422.
func TestCadenceCreate_CommandWithScenarioName422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"command","module":"core.cmd.shell","scenario_name":"nope","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// invalid kind → 422 (before validate, in checkKindPermission).
func TestCadenceCreate_InvalidKind422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"frobnicate","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// scenario-target resolving to an empty scope (coven with no incarnations) → 422
// before Insert (parity TestVoyageCreate_EmptyTarget422 / voyage_empty_target).
// Without the guard an empty resolve ran the scope loop zero times → a silent 201
// on a "dead" recipe. allowAll() passes kind-guard+bare-check; resolver explicitly out=nil.
func TestCadenceCreate_EmptyTarget422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandlerScoped(store, &fakeVoyageScenarioResolver{out: nil}, nil, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cadence_empty_target") {
		t.Errorf("body should mention cadence_empty_target: %s", rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (empty-target before Insert)", store.insertCalls)
	}
}

// malformed JSON → 400.
func TestCadenceCreate_BadJSON400(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", `{"kind":}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: two-tier RBAC kind-guard ---

// kind=scenario without incarnation.run → 403 (two-tier guard, ADR-046 §7).
func TestCadenceCreate_ScenarioKindGuardDenied403(t *testing.T) {
	store := &fakeCadenceStore{}
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"errand.run": true}} // no incarnation.run
	h := newCadenceHandler(store, enf)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (kind-guard before Insert)", store.insertCalls)
	}
}

// kind=command without errand.run → 403.
func TestCadenceCreate_CommandKindGuardDenied403(t *testing.T) {
	store := &fakeCadenceStore{}
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"incarnation.run": true}} // no errand.run
	h := newCadenceHandler(store, enf)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", store.insertCalls)
	}
}

// --- tests: per-target coven-scope (ADR-046 §7, security-fix S4) ---

// fakeCadenceScopedEnforcer models a coven-scoped Archon (parity the real
// rbac.Permission.Matches of CadenceHandler's two-tier guard): the bare-check
// (nil context, [checkKindPermission]) for `incarnation.run` PASSES, but the
// per-incarnation OR-context-check ([checkTargetScope] → allowedAnyContext)
// allows only contexts whose `coven` is in allowedCovens. So the scope
// discriminator is the scope loop itself (security intent: bare passes, coven=B
// is cut by the scope check), full parity VoyageHandler.createScenario.
type fakeCadenceScopedEnforcer struct {
	allowedCovens map[string]bool // coven labels that hold incarnation.run
}

func (e *fakeCadenceScopedEnforcer) Check(_ string, resource, action string, ctx map[string]string) error {
	if resource != "incarnation" || action != "run" {
		return rbac.ErrPermissionDenied
	}
	// Bare-check (nil/no coven) — passes (the Archon holds incarnation.run; the
	// per-context scope loop decides which covens it's limited to).
	if len(ctx) == 0 {
		return nil
	}
	if e.allowedCovens[ctx["coven"]] {
		return nil
	}
	return rbac.ErrPermissionDenied
}

// scopedIncReader — incReader returning incarnations with fixed covens by
// name (for the per-incarnation scope loop). incA→coven-a, incB→coven-b.
func scopedIncReader() *fakeIncDB {
	return &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		coven := "coven-b"
		if name == "inc-a" {
			coven = "coven-a"
		}
		now := time.Now()
		return staticRow{values: []any{
			name, "redis", "v1", int(1),
			[]byte("{}"), []byte("{}"), "ready",
			[]byte(nil), any(nil),
			now, now, []string{coven},
			[]byte("{}"), // traits
			any(nil), []byte(nil),
			"create", // created_scenario (migration 089, NOT NULL DEFAULT)
			any(nil), // applying_apply_id (ADR-068 §A1)
		}}
	}}
}

// (a) a scoped Archon "incarnation.run on coven=A" can NOT create a Cadence whose
// target resolves to an incarnation outside scope (coven=B) → 403 fail-closed.
// Parity TestVoyageCreate_ScenarioRBACDenied / scope-loop createScenario.
func TestCadenceCreate_ScopeDenied_CovenB_403(t *testing.T) {
	store := &fakeCadenceStore{}
	enf := &fakeCadenceScopedEnforcer{allowedCovens: map[string]bool{"coven-a": true}}
	h := newCadenceHandlerScoped(store, &fakeVoyageScenarioResolver{out: []string{"inc-b"}}, scopedIncReader(), enf)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"coven":["coven-b"]}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (scope-check before Insert)", store.insertCalls)
	}
}

// (b) the same Archon CAN create a Cadence on coven=A (in scope) → 201.
func TestCadenceCreate_ScopeAllowed_CovenA_201(t *testing.T) {
	store := &fakeCadenceStore{}
	enf := &fakeCadenceScopedEnforcer{allowedCovens: map[string]bool{"coven-a": true}}
	h := newCadenceHandlerScoped(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, scopedIncReader(), enf)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"coven":["coven-a"]}}`))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", store.insertCalls)
	}
}

// (c) PATCH target A→B by a scoped Archon → 403 (second hole: PATCH changes target
// without kind/scope check). Loaded row is a scenario on coven-a (scopeStore
// returns target {"service":"web"}, but we mock the resolver to inc-a; PATCH moves
// target to coven-b → resolver returns inc-b outside scope).
func TestCadencePatch_ScopeDenied_RetargetCovenB_403(t *testing.T) {
	store := scenarioStore()
	enf := &fakeCadenceScopedEnforcer{allowedCovens: map[string]bool{"coven-a": true}}
	// Resolver by post-patch target: PATCH sets coven-b → incarnation inc-b.
	h := newCadenceHandlerScoped(store, &fakeVoyageScenarioResolver{out: []string{"inc-b"}}, scopedIncReader(), enf)
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id,
		`{"target":{"coven":["coven-b"]}}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if store.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0 (scope-check before Update)", store.updateCalls)
	}
}

// (c') PATCH target within scope (coven=A) → 200 (PATCH guard doesn't false-cut).
func TestCadencePatch_ScopeAllowed_RetargetCovenA_200(t *testing.T) {
	store := scenarioStore()
	enf := &fakeCadenceScopedEnforcer{allowedCovens: map[string]bool{"coven-a": true}}
	h := newCadenceHandlerScoped(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, scopedIncReader(), enf)
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id,
		`{"target":{"coven":["coven-a"]}}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1", store.updateCalls)
	}
}

// (c") PATCH without incarnation.run at all (no kind-permission) → 403. Closes
// the "PATCH without kind-permission check" hole (second hole, kind-guard on PATCH).
func TestCadencePatch_NoKindPermission_403(t *testing.T) {
	store := scenarioStore()
	enf := &fakeVoyageEnforcer{allow: map[string]bool{"errand.run": true}} // no incarnation.run
	h := newCadenceHandler(store, enf)
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"name":"renamed"}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if store.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0 (kind-guard before Update)", store.updateCalls)
	}
}

// --- tests: read ---

func TestCadenceGet_OK(t *testing.T) {
	store := scenarioStore()
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Get(rec, cadenceReqID(http.MethodGet, "/v1/cadences/"+id, id, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto cadenceDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	if dto.CadenceID != id || dto.Kind != "scenario" || dto.ScheduleKind != "interval" {
		t.Errorf("dto = %+v, want id=%s scenario interval", dto, id)
	}
}

func TestCadenceGet_NotFound404(t *testing.T) {
	store := &fakeCadenceStore{selectByID: func(string) pgx.Row { return cadenceErrRow{err: pgx.ErrNoRows} }}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Get(rec, cadenceReqID(http.MethodGet, "/v1/cadences/"+id, id, ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cadence_not_found") {
		t.Errorf("body should mention cadence_not_found: %s", rec.Body.String())
	}
}

func TestCadenceGet_BadULID422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Get(rec, cadenceReqID(http.MethodGet, "/v1/cadences/not-a-ulid", "not-a-ulid", ""))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCadenceList_OK(t *testing.T) {
	store := &fakeCadenceStore{
		listCount: 1,
		listRows: func() (pgx.Rows, error) {
			return &cadenceRowsIter{rows: []cadenceFullRow{{
				id: audit.NewULID(), name: "nightly", enabled: true,
				scheduleKind: "interval", intervalSecs: intp(300),
				overlap: "skip", kind: "scenario", scenarioName: strp("converge"),
			}}}, nil
		},
	}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.List(rec, cadenceReq(http.MethodGet, "/v1/cadences?enabled=true&kind=scenario", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Items []cadenceDTO `json:"items"`
		Total int          `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.Total != 1 || len(reply.Items) != 1 || reply.Items[0].Kind != "scenario" {
		t.Errorf("reply = %+v, want total=1 one scenario item", reply)
	}
}

func TestCadenceList_BadEnabled422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.List(rec, cadenceReq(http.MethodGet, "/v1/cadences?enabled=maybe", ""))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCadenceList_BadKind422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.List(rec, cadenceReq(http.MethodGet, "/v1/cadences?kind=bogus", ""))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: patch ---

// PATCH changes name and enabled (no schedule change → next_run untouched).
func TestCadencePatch_OK(t *testing.T) {
	store := scenarioStore()
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id,
		`{"name":"renamed","enabled":false}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1", store.updateCalls)
	}
	var dto cadenceDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	if dto.Name != "renamed" || dto.Enabled {
		t.Errorf("dto = %+v, want name=renamed enabled=false", dto)
	}
}

// PATCH with a schedule change (interval → cron) recomputes next_run_at.
func TestCadencePatch_ScheduleChange_RecomputesNextRun(t *testing.T) {
	store := scenarioStore()
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id,
		`{"schedule_kind":"cron","cron_expr":"0 0 * * *"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto cadenceDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	if dto.ScheduleKind != "cron" || dto.CronExpr != "0 0 * * *" {
		t.Errorf("dto schedule = %q/%q, want cron/'0 0 * * *'", dto.ScheduleKind, dto.CronExpr)
	}
	if dto.NextRunAt == nil {
		t.Error("next_run_at must be recomputed when schedule changes")
	}
	// interval_seconds cleared on switch to cron (validate invariant).
	if dto.IntervalSeconds != nil {
		t.Errorf("interval_seconds = %v, want nil (cleared on cron)", dto.IntervalSeconds)
	}
}

func TestCadencePatch_NotFound404(t *testing.T) {
	store := &fakeCadenceStore{selectByID: func(string) pgx.Row { return cadenceErrRow{err: pgx.ErrNoRows} }}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"name":"x"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// PATCH with a broken schedule (interval_seconds=0) → 422 (validate in Update).
func TestCadencePatch_InvalidInterval422(t *testing.T) {
	store := scenarioStore()
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"interval_seconds":0}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: enable/disable ---

func TestCadenceEnable_OK(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Enable(rec, cadenceReqID(http.MethodPost, "/v1/cadences/"+id+"/enable", id, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.setEnabledArgs) != 1 || !store.setEnabledArgs[0] {
		t.Errorf("setEnabledArgs = %v, want [true]", store.setEnabledArgs)
	}
}

func TestCadenceDisable_OK(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Disable(rec, cadenceReqID(http.MethodPost, "/v1/cadences/"+id+"/disable", id, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.setEnabledArgs) != 1 || store.setEnabledArgs[0] {
		t.Errorf("setEnabledArgs = %v, want [false]", store.setEnabledArgs)
	}
}

func TestCadenceEnable_NotFound404(t *testing.T) {
	store := &fakeCadenceStore{setEnabledNoRow: true} // 0 rows → not-found
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Enable(rec, cadenceReqID(http.MethodPost, "/v1/cadences/"+id+"/enable", id, ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: delete ---

func TestCadenceDelete_OK(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Delete(rec, cadenceReqID(http.MethodDelete, "/v1/cadences/"+id, id, ""))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if store.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", store.deleteCalls)
	}
}

func TestCadenceDelete_NotFound404(t *testing.T) {
	store := &fakeCadenceStore{deleteNoRow: true}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Delete(rec, cadenceReqID(http.MethodDelete, "/v1/cadences/"+id, id, ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- tests: runs ---

func TestCadenceRuns_OK(t *testing.T) {
	store := scenarioStore()
	store.voyageListCount = 1
	store.voyageListRows = func() (pgx.Rows, error) {
		return &voyageRowsIter{rows: [][]any{voyageRowVals(audit.NewULID(), voyage.KindScenario, voyage.StatusSucceeded)}}, nil
	}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Runs(rec, cadenceReqID(http.MethodGet, "/v1/cadences/"+id+"/runs", id, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Items []voyageDTO `json:"items"`
		Total int         `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.Total != 1 || len(reply.Items) != 1 {
		t.Errorf("reply = %+v, want total=1 one child Voyage", reply)
	}
}

func TestCadenceRuns_CadenceNotFound404(t *testing.T) {
	store := &fakeCadenceStore{selectByID: func(string) pgx.Row { return cadenceErrRow{err: pgx.ErrNoRows} }}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Runs(rec, cadenceReqID(http.MethodGet, "/v1/cadences/"+id+"/runs", id, ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- row iterator ---

type cadenceRowsIter struct {
	rows []cadenceFullRow
	idx  int
}

func (r *cadenceRowsIter) Next() bool {
	r.idx++
	return r.idx <= len(r.rows)
}
func (r *cadenceRowsIter) Scan(dest ...any) error        { return r.rows[r.idx-1].Scan(dest...) }
func (r *cadenceRowsIter) Err() error                    { return nil }
func (r *cadenceRowsIter) Close()                        {}
func (r *cadenceRowsIter) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *cadenceRowsIter) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *cadenceRowsIter) Values() ([]any, error) { return nil, nil }
func (r *cadenceRowsIter) RawValues() [][]byte    { return nil }
func (r *cadenceRowsIter) Conn() *pgx.Conn        { return nil }
