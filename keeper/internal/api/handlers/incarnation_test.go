package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeIncDB — a mock [IncarnationDB] (ExecQueryRower + TxBeginner). The minimal
// set needed by the Create / Get / List / History / Run / Unlock endpoints.
type fakeIncDB struct {
	// Create-path
	insertRow   func() pgx.Row
	insertCalls int
	// insertArgs — the last arguments of INSERT INTO incarnation (spec=$5, traits=$11)
	// to verify threading of spec.traits on the create path (ADR-060 amend R1).
	insertArgs []any
	// updateTraitsArg — jsonb arg $2 of UPDATE incarnation SET traits (PUT .../traits,
	// ADR-060 amend R1): a wholesale replacement of incarnation.traits.
	updateTraitsArg []byte

	// Get/History/Run existence-probe + Unlock SELECT FOR UPDATE
	selectByNameRow func(name string) pgx.Row

	// List
	countRow func(sql string) pgx.Row
	listRows func() (pgx.Rows, error)
	// captureListSQL — a hook on the list-SELECT text (ORDER BY pushdown check).
	captureListSQL func(sql string)
	// lastCountArgs / lastListArgs — bind-args of the COUNT/SELECT list queries
	// (scope-pushdown check S3b-3). listCalled — whether SelectAll was called
	// (fail-closed: on an empty scope SelectAll must not be called).
	lastCountArgs []any
	lastListArgs  []any
	listCalled    bool

	// Unlock path: SELECT FOR UPDATE (state, status) + Exec accounting.
	unlockSelectRow func(name string) pgx.Row
	execCalls       []string

	// Rerun-last path: the last-run probe in UnlockForRerun
	// `SELECT scenario, apply_id FROM state_history ... ORDER BY history_id DESC LIMIT 1`.
	// nil → default [create, <applyID>] (create path: the last failed = create).
	lastScenarioRow func(name string) pgx.Row
	// Rerun-last day-2 path: the recipe probe `SELECT recipe FROM apply_runs WHERE
	// apply_id = $1 AND recipe IS NOT NULL LIMIT 1`. nil → ErrNoRows (fail-closed:
	// recipe unavailable). Set for the day-2 happy path.
	recipeRow func(applyID string) pgx.Row

	// Upgrade path: SELECT FOR UPDATE (state, state_schema_version, status).
	upgradeSelectRow func(name string) pgx.Row

	// Destroy path: RowsAffected for the single-winner `DELETE FROM incarnation`
	// (DeleteAfterTeardown). zero-value → "DELETE 1" (row deleted); set
	// "DELETE 0" for a no-op race. archive INSERTs return an empty tag.
	deleteTag pgconn.CommandTag

	// UpdateHosts path: SELECT FROM souls WHERE sid = ANY($1) — the set of SIDs
	// that "exist" in the `souls` registry. nil → none (for the
	// UnknownSID test). The UpdateHosts SQL `UPDATE incarnation SET spec = ...` is caught
	// by the generic Exec — recording the write in execCalls.
	soulsExisting map[string]struct{}

	// Runs read-view (GET .../runs[/{apply_id}]): the count row of the runs list
	// (COUNT(DISTINCT apply_id) FROM apply_runs) and the list/detail rows (SELECT ...
	// FROM apply_runs). nil → empty list / 0 (for scope-gate/empty tests).
	applyRunsCountRow func(sql string) pgx.Row
	applyRunsRows     func() (pgx.Rows, error)

	// Run-tasks read-view (GET .../runs/{apply_id}/tasks, NIM-37): the EXISTS probe
	// of whether the run belongs to the incarnation (runExistsRow, nil → true = "belongs")
	// and the apply_run_plan plan rows (runPlanRows, nil → empty plan).
	runExistsRow func(applyID, name string) pgx.Row
	runPlanRows  func() (pgx.Rows, error)

	// Global runs read-view (GET /v1/runs[/stats]): COUNT(*) over the apply_runs rollup.
	// runsCalled/lastRunsSQL/lastRunsArgs — the fact and contents of the count query
	// (fail-closed and scope-pushdown checks). nil runsCountRow → 0.
	runsCountRow func(sql string) pgx.Row
	lastRunsSQL  string
	lastRunsArgs []any
	runsCalled   bool
}

func (f *fakeIncDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.execCalls = append(f.execCalls, sql)
	if strings.Contains(sql, "DELETE FROM incarnation") {
		if f.deleteTag.String() == "" {
			return pgconn.NewCommandTag("DELETE 1"), nil
		}
		return f.deleteTag, nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeIncDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO incarnation") {
		f.insertCalls++
		f.insertArgs = args
		if f.insertRow != nil {
			return f.insertRow()
		}
		return staticRow{values: []any{time.Now(), time.Now()}}
	}
	if strings.Contains(sql, "SELECT state, state_schema_version, status") && strings.Contains(sql, "FOR UPDATE") {
		if f.upgradeSelectRow != nil {
			return f.upgradeSelectRow(args[0].(string))
		}
		return errRow{err: pgx.ErrNoRows}
	}
	if strings.Contains(sql, "SELECT state, status") && strings.Contains(sql, "FOR UPDATE") {
		if f.unlockSelectRow != nil {
			return f.unlockSelectRow(args[0].(string))
		}
		return errRow{err: pgx.ErrNoRows}
	}
	if strings.Contains(sql, "SELECT scenario") && strings.Contains(sql, "FROM state_history") {
		if f.lastScenarioRow != nil {
			return f.lastScenarioRow(args[0].(string))
		}
		// Default: the last failed = create (create path), apply_id is a stub.
		return staticRow{values: []any{"create", "01HFAILEDRUN00000000000000"}}
	}
	if strings.Contains(sql, "FROM apply_runs") && strings.Contains(sql, "recipe IS NOT NULL") {
		if f.recipeRow != nil {
			return f.recipeRow(args[0].(string))
		}
		return errRow{err: pgx.ErrNoRows}
	}
	// UpdateHosts: UPDATE incarnation SET spec = ... RETURNING updated_at.
	// This UPDATE-with-RETURNING arrives BEFORE the generic "WHERE name = $1" match
	// (the same predicate is here too), so it is handled by a separate branch
	// and returns a fresh timestamp to Scan(*time.Time). UpdateTraits (SET traits)
	// — the same RETURNING updated_at; we record its jsonb arg $2 in updateTraitsArg.
	if strings.Contains(sql, "UPDATE incarnation") && strings.Contains(sql, "RETURNING updated_at") {
		if strings.Contains(sql, "SET traits") && len(args) >= 2 {
			f.updateTraitsArg, _ = args[1].([]byte)
		}
		return staticRow{values: []any{time.Now().UTC()}}
	}
	if strings.Contains(sql, "FROM incarnation\nWHERE name") || strings.Contains(sql, "WHERE name = $1") {
		if f.selectByNameRow != nil {
			return f.selectByNameRow(args[0].(string))
		}
		return errRow{err: pgx.ErrNoRows}
	}
	if strings.Contains(sql, "COUNT(*) FROM incarnation") || strings.Contains(sql, "COUNT(*) FROM state_history") {
		if strings.Contains(sql, "COUNT(*) FROM incarnation") {
			f.listCalled = true
			f.lastCountArgs = args
		}
		if f.countRow != nil {
			return f.countRow(sql)
		}
		return staticRow{values: []any{int(0)}}
	}
	// Run-tasks scope-probe: EXISTS(SELECT 1 FROM apply_runs ...) — whether the
	// run belongs to the incarnation (RunExistsForIncarnation, NIM-37). nil hook → true.
	if strings.Contains(sql, "EXISTS(SELECT 1") && strings.Contains(sql, "FROM apply_runs") {
		if f.runExistsRow != nil {
			return f.runExistsRow(args[0].(string), args[1].(string))
		}
		return staticRow{values: []any{true}}
	}
	// Runs read-view (GET .../runs): COUNT(DISTINCT apply_id) FROM apply_runs.
	// applyRunsCountRow controls the value; nil → 0 (empty runs list).
	if strings.Contains(sql, "COUNT(DISTINCT apply_id) FROM apply_runs") {
		if f.applyRunsCountRow != nil {
			return f.applyRunsCountRow(sql)
		}
		return staticRow{values: []any{int(0)}}
	}
	// Global runs read-view (GET /v1/runs): COUNT(*) over the apply_runs rollup.
	if strings.Contains(sql, "COUNT(*)") && strings.Contains(sql, "FROM apply_runs") {
		f.runsCalled = true
		f.lastRunsSQL = sql
		f.lastRunsArgs = args
		if f.runsCountRow != nil {
			return f.runsCountRow(sql)
		}
		return staticRow{values: []any{int(0)}}
	}
	return errRow{err: errors.New("fakeIncDB.QueryRow: unexpected SQL: " + sql)}
}

func (f *fakeIncDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	// UpdateHosts: SELECT sid FROM souls WHERE sid = ANY($1) — return the
	// existing SIDs from soulsExisting (the test controls it).
	if strings.Contains(sql, "FROM souls WHERE sid = ANY") {
		sids, _ := args[0].([]string)
		var found []string
		for _, sid := range sids {
			if _, ok := f.soulsExisting[sid]; ok {
				found = append(found, sid)
			}
		}
		return &stringRows{values: found}, nil
	}
	// Run-tasks: SELECT ... FROM apply_run_plan (the run's task plan, NIM-37). It stands
	// BEFORE the apply_runs branch: "FROM apply_run_plan" does not contain "FROM apply_runs",
	// but we keep the order explicit. nil hook → empty plan.
	if strings.Contains(sql, "FROM apply_run_plan") {
		if f.runPlanRows != nil {
			return f.runPlanRows()
		}
		return &emptyRows{}, nil
	}
	// Runs read-view: SELECT ... FROM apply_runs (runs list / per-host detail).
	// A separate hook from listRows (that one is the incarnation list), nil → empty set.
	if strings.Contains(sql, "FROM apply_runs") {
		if f.applyRunsRows != nil {
			return f.applyRunsRows()
		}
		return &emptyRows{}, nil
	}
	if strings.Contains(sql, "FROM incarnation") {
		f.lastListArgs = args
		if f.captureListSQL != nil {
			f.captureListSQL(sql)
		}
	}
	if f.listRows != nil {
		return f.listRows()
	}
	return &emptyRows{}, nil
}

// BeginTx returns a fakeIncTx proxying back into fakeIncDB. The Tx methods
// Commit/Rollback are no-ops (the unit test does not check PG transactional
// semantics, only the handler → CRUD route).
func (f *fakeIncDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeIncTx{db: f}, nil
}

// fakeIncTx — a pgx.Tx wrapper over fakeIncDB. Delegates Exec/Query/QueryRow;
// Commit/Rollback are no-ops; the other pgx.Tx methods panic when called
// (Unlock does not use them).
type fakeIncTx struct{ db *fakeIncDB }

func (t *fakeIncTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.db.BeginTx(ctx, pgx.TxOptions{})
}
func (t *fakeIncTx) BeginFunc(_ context.Context, fn func(pgx.Tx) error) error { return fn(t) }
func (t *fakeIncTx) Commit(_ context.Context) error                           { return nil }
func (t *fakeIncTx) Rollback(_ context.Context) error                         { return nil }
func (t *fakeIncTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("fakeIncTx.CopyFrom: unexpected")
}
func (t *fakeIncTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("fakeIncTx.SendBatch: unexpected")
}
func (t *fakeIncTx) LargeObjects() pgx.LargeObjects { panic("fakeIncTx.LargeObjects: unexpected") }
func (t *fakeIncTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("fakeIncTx.Prepare: unexpected")
}
func (t *fakeIncTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *fakeIncTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *fakeIncTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (t *fakeIncTx) Conn() *pgx.Conn { return nil }

// emptyRows — a pgx.Rows stub with no values.
type emptyRows struct{}

func (r *emptyRows) Next() bool                                   { return false }
func (r *emptyRows) Scan(_ ...any) error                          { return nil }
func (r *emptyRows) Err() error                                   { return nil }
func (r *emptyRows) Close()                                       {}
func (r *emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *emptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *emptyRows) RawValues() [][]byte                          { return nil }
func (r *emptyRows) Conn() *pgx.Conn                              { return nil }

// makeIncarnationRow builds a pgx.Row stub for SelectByName with
// preset fields. Used for the Get/History existence-probe test.
func makeIncarnationRow(name string) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits (ADR-060 amend R1)
		any(nil), []byte(nil), // last_drift_check_at, last_drift_summary (ADR-031 Slice C)
		"create", // created_scenario (migration 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// --- Create -----------------------------------------------------------

func TestIncarnation_Create_202(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","input":{"replicas":3}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Decode into a map to verify the ABSENCE of the `status` field in the JSON
	// (createIncarnationResponse does not declare it, but a check via a
	// raw map catches a regression if the field is accidentally returned).
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw["incarnation"] != "redis-prod" {
		t.Errorf("incarnation = %v", raw["incarnation"])
	}
	if _, hasStatus := raw["status"]; hasStatus {
		t.Errorf("response contains 'status' field (OpenAPI schema drift): %v", raw)
	}
	applyID, _ := raw["apply_id"].(string)
	if len(applyID) != 26 {
		t.Errorf("apply_id len = %d, want 26 (ULID)", len(applyID))
	}
	if db.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", db.insertCalls)
	}
}

// TestIncarnation_Create_Covens_Accepted — covens in the body is accepted (not an
// unknown-field for the strict decoder) and reaches insert. Threading covens to
// INSERT arg $10 is covered by the domain test TestCreate_CovensPassedThrough.
func TestIncarnation_Create_Covens_Accepted(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","covens":["prod","dc1"]}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", db.insertCalls)
	}
}

func TestIncarnation_Create_InvalidCoven_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","covens":["Bad_Coven"]}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (reject before insert)", db.insertCalls)
	}
}

func TestIncarnation_Create_InvalidName_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"Bad_Name","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422", rec.Code)
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", db.insertCalls)
	}
}

func TestIncarnation_Create_MissingService_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

func TestIncarnation_Create_DuplicateName_409(t *testing.T) {
	db := &fakeIncDB{
		insertRow: func() pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code: "23505", ConstraintName: "incarnation_pkey",
			}}
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("Code = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeIncarnationExists {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeIncarnationExists)
	}
}

// --- Get --------------------------------------------------------------

func TestIncarnation_Get_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(newChiRequest(http.MethodGet, "/v1/incarnations/redis-prod", nil, "name", "redis-prod"), "archon-alice")
	rec := incGet(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto incDTOJSON
	if err := json.NewDecoder(rec.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Name != "redis-prod" {
		t.Errorf("Name = %q", dto.Name)
	}
	if dto.Status != "ready" {
		t.Errorf("Status = %q", dto.Status)
	}
}

func TestIncarnation_Get_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodGet, "/v1/incarnations/ghost", nil, "name", "ghost")
	rec := incGet(h, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404", rec.Code)
	}
}

func TestIncarnation_Get_InvalidName_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodGet, "/v1/incarnations/Bad_Name", nil, "name", "Bad_Name")
	rec := incGet(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// --- Secret masking on GET output (variant D) -------------------------

func TestToDTO_MasksSecretsInStateAndSpec(t *testing.T) {
	pwd := "s3cr3t"
	inc := &incarnation.Incarnation{
		Name:   "redis-prod",
		Status: incarnation.StatusReady,
		Spec: map[string]any{
			"input":         map[string]any{"db_password": pwd},
			"public_option": "visible",
		},
		State: map[string]any{
			"admin_token": pwd,
			"replicas":    float64(3),
			"vault_ref":   "vault:secret/redis/admin",
		},
	}
	dto := toIncarnationGetView(inc, nil)

	if got := dto.State["admin_token"]; got != "***MASKED***" {
		t.Errorf("state.admin_token = %v, want masked", got)
	}
	if got := dto.State["vault_ref"]; got != "***MASKED***" {
		t.Errorf("state.vault_ref = %v, want masked", got)
	}
	if got := dto.State["replicas"]; got != float64(3) {
		t.Errorf("state.replicas = %v, want 3 (несекретное — без маскировки)", got)
	}
	specInput := dto.Spec["input"].(map[string]any)
	if got := specInput["db_password"]; got != "***MASKED***" {
		t.Errorf("spec.input.db_password = %v, want masked", got)
	}
	if got := dto.Spec["public_option"]; got != "visible" {
		t.Errorf("spec.public_option = %v, want visible", got)
	}

	// The stored incarnation is not mutated — only the response is masked.
	if inc.State["admin_token"] != pwd {
		t.Errorf("исходный inc.State мутирован: %v", inc.State["admin_token"])
	}
	if inc.Spec["input"].(map[string]any)["db_password"] != pwd {
		t.Errorf("исходный inc.Spec мутирован")
	}
}

// TestToIncarnationGetView_ProjectsTraitsAndCreatedScenario — the handler projection
// reads traits (ADR-060 operator-set labels) and created_scenario (the multi-create
// mechanism) from the domain incarnation row. Bug source: both fields were not returned in
// GET → the UI traits-modal opened without prefill, the operator didn't see the start scenario.
func TestToIncarnationGetView_ProjectsTraitsAndCreatedScenario(t *testing.T) {
	cs := "create_cluster"
	inc := &incarnation.Incarnation{
		Name:            "redis-prod",
		Status:          incarnation.StatusReady,
		CreatedScenario: &cs,
		Traits:          map[string]any{"env": "prod", "az": []any{"a", "b"}},
	}
	view := toIncarnationGetView(inc, nil)

	if view.CreatedScenario != "create_cluster" {
		t.Errorf("CreatedScenario = %q, want create_cluster", view.CreatedScenario)
	}
	if got := view.Traits["env"]; got != "prod" {
		t.Errorf("Traits[env] = %v, want prod", got)
	}
	if got, ok := view.Traits["az"].([]any); !ok || len(got) != 2 {
		t.Errorf("Traits[az] = %v, want list len 2 (Trait полиморфен)", view.Traits["az"])
	}

	// Empty domain values pass through as-is (omitempty drops them in the wire projection).
	empty := &incarnation.Incarnation{Name: "x", Status: incarnation.StatusReady, Traits: map[string]any{}}
	emptyView := toIncarnationGetView(empty, nil)
	if emptyView.CreatedScenario != "" {
		t.Errorf("CreatedScenario = %q, want empty", emptyView.CreatedScenario)
	}
	if len(emptyView.Traits) != 0 {
		t.Errorf("Traits = %v, want empty", emptyView.Traits)
	}
}

func TestToHistoryDTO_MasksSecretsInStateSnapshots(t *testing.T) {
	e := &incarnation.HistoryEntry{
		HistoryID:   "01HX",
		Scenario:    "rotate",
		StateBefore: map[string]any{"admin_token": "old", "replicas": float64(1)},
		StateAfter:  map[string]any{"admin_token": "new", "replicas": float64(1)},
	}
	dto := toStateHistoryView(e, nil)

	if got := dto.StateBefore["admin_token"]; got != "***MASKED***" {
		t.Errorf("state_before.admin_token = %v, want masked", got)
	}
	if got := dto.StateAfter["admin_token"]; got != "***MASKED***" {
		t.Errorf("state_after.admin_token = %v, want masked", got)
	}
	if got := dto.StateBefore["replicas"]; got != float64(1) {
		t.Errorf("state_before.replicas = %v, want 1 (несекретное)", got)
	}
	// The stored snapshot is not mutated.
	if e.StateBefore["admin_token"] != "old" {
		t.Errorf("исходный StateBefore мутирован")
	}
}

func TestIncarnation_Get_200_StateMasked(t *testing.T) {
	// End-to-end through the handler: a state with a secret → masked in the JSON response,
	// a non-secret field — as-is.
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			now := time.Now()
			return staticRow{values: []any{
				name, "redis", "v1", int(1),
				[]byte("{}"),
				[]byte(`{"admin_token":"s3cr3t","replicas":3}`),
				"ready",
				[]byte(nil), any(nil),
				now, now, []string(nil),
				[]byte("{}"),          // traits
				any(nil), []byte(nil), // ADR-031 Slice C
				"create", // created_scenario (migration 089, NOT NULL DEFAULT)
				any(nil), // applying_apply_id (ADR-068 §A1)
			}}
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(newChiRequest(http.MethodGet, "/v1/incarnations/redis-prod", nil, "name", "redis-prod"), "archon-alice")
	rec := incGet(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto incDTOJSON
	if err := json.NewDecoder(rec.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := dto.State["admin_token"]; got != "***MASKED***" {
		t.Errorf("state.admin_token = %v, want masked в JSON-ответе", got)
	}
	if got := dto.State["replicas"]; got != float64(3) {
		t.Errorf("state.replicas = %v, want 3 (несекретное отдаётся как есть)", got)
	}
}

// --- List -------------------------------------------------------------

func TestIncarnation_List_200_Empty(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["total"] != float64(0) {
		t.Errorf("total = %v, want 0", out["total"])
	}
	if out["limit"] != float64(50) {
		t.Errorf("limit = %v, want 50 (default)", out["limit"])
	}
}

func TestIncarnation_List_BadLimit_400(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?limit=99999", nil)
	rec := incList(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("Code = %d, want 400", rec.Code)
	}
}

func TestIncarnation_List_BadStatusFilter_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?status=destroyed", nil)
	rec := incList(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// TestIncarnation_List_CovenFilter_PassesToSQL — the coven query-param reaches
// the SQL arg (not dropped client-side). The COUNT(*) arguments are the
// ready args[] for filter.Coven (see buildListWhere → `$1 = ANY(covens)`).
func TestIncarnation_List_CovenFilter_PassesToSQL(t *testing.T) {
	var capturedArgs []any
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	// Intercept countRow with arguments — the test fake has no hook for
	// args[]; the SQL matcher contains COUNT(*) FROM incarnation, while args itself
	// arrives as a parameter to QueryRow. Replace countRow with a closure that has
	// a side effect.
	db.countRow = func(sql string) pgx.Row {
		// The SQL contains the WHERE predicate — but args itself is not here. It's enough
		// to check that the SQL includes $1 = ANY(covens) (the filter fired).
		capturedArgs = append(capturedArgs, sql)
		return staticRow{values: []any{int(0)}}
	}

	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?coven=dev", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(capturedArgs) == 0 {
		t.Fatal("countRow не был вызван")
	}
	sql, _ := capturedArgs[0].(string)
	if !strings.Contains(sql, "ANY(covens)") {
		t.Errorf("count SQL не содержит ANY(covens)-предикат:\n%s", sql)
	}
}

// TestIncarnation_List_InvalidCoven_422 — an invalid coven label is rejected
// before SQL (kebab-case format).
func TestIncarnation_List_InvalidCoven_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?coven=DEV_UPPER", nil)
	rec := incList(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// listSQLCapture intercepts the list SQL (Query) and COUNT (countRow) —
// to confirm that the query-param reached the WHERE/ORDER BY pushdown.
func listSQLCapture() (*fakeIncDB, *string) {
	var captured string
	db := &fakeIncDB{
		countRow: func(sql string) pgx.Row {
			captured = sql
			return staticRow{values: []any{int(0)}}
		},
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	return db, &captured
}

// TestIncarnation_List_StateFilter_PassesToSQL — the query `state.redis_version=8.0`
// reaches the jsonb pushdown (->>) in the COUNT SQL.
func TestIncarnation_List_StateFilter_PassesToSQL(t *testing.T) {
	db, captured := listSQLCapture()
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?state.redis_version=8.0", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "state->>'redis_version'") {
		t.Errorf("state-фильтр не долетел до SQL:\n%s", *captured)
	}
}

// TestIncarnation_List_StateFilter_NumericOp — the query `state.memory_mb=gt:1000`
// parses into a numeric comparison (->>)::numeric.
func TestIncarnation_List_StateFilter_NumericOp(t *testing.T) {
	db, captured := listSQLCapture()
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?state.memory_mb=gt:1000", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*captured, "(state->>'memory_mb')::numeric >") {
		t.Errorf("числовой state-фильтр не долетел до SQL:\n%s", *captured)
	}
}

// TestIncarnation_List_StateFilter_InjectionPath_422 — special chars/SQL in the
// state-path are rejected by format validation ([a-z0-9_]) before the DB query.
func TestIncarnation_List_StateFilter_InjectionPath_422(t *testing.T) {
	for _, raw := range []string{
		"state.redis_version'%20OR%201=1=x",
		"state.DROP=1",
		"state.with-dash=1",
	} {
		db := &fakeIncDB{}
		h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
		req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?"+raw, nil)
		rec := incList(h, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("query %q: Code = %d, want 422", raw, rec.Code)
		}
	}
}

// TestIncarnation_List_StateFilter_NumericOp_NonNumericValue_422 — a non-numeric
// value with a numeric operator (`state.memory_mb=gt:abc`) → 422
// (operator typo), not a 500 from a PG cast error 22P02.
func TestIncarnation_List_StateFilter_NumericOp_NonNumericValue_422(t *testing.T) {
	for _, raw := range []string{
		"state.memory_mb=gt:abc",
		"state.memory_mb=gte:1.2.3",
		"state.memory_mb=lt:",
		"state.memory_mb=lte:10x",
	} {
		db := &fakeIncDB{}
		h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
		req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?"+raw, nil), "archon-alice")
		rec := incList(h, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("query %q: Code = %d, want 422; body=%s", raw, rec.Code, rec.Body.String())
		}
	}
}

// TestIncarnation_List_SortStateField_PassesToSQL — sort=state.<field> goes
// into a jsonb ORDER BY (list-SQL intercepted via captureListSQL).
func TestIncarnation_List_SortStateField_PassesToSQL(t *testing.T) {
	var listSQL string
	db := &fakeIncDB{
		countRow:       func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows:       func() (pgx.Rows, error) { return &emptyRows{}, nil },
		captureListSQL: func(sql string) { listSQL = sql },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?sort=state.redis_version&sort_dir=desc", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(listSQL, "ORDER BY state->>'redis_version' DESC") {
		t.Errorf("sort по state-полю не долетел до ORDER BY:\n%s", listSQL)
	}
}

// TestIncarnation_List_BadSortField_422 — an unknown sort field → 422.
func TestIncarnation_List_BadSortField_422(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?sort=spec", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// TestIncarnation_List_BadSortDir_422 — an unknown direction → 422.
func TestIncarnation_List_BadSortDir_422(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?sort=name&sort_dir=sideways", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// --- History ----------------------------------------------------------

func TestIncarnation_History_200_Empty(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		countRow:        func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows:        func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(newChiRequest(http.MethodGet, "/v1/incarnations/redis-prod/history", nil, "name", "redis-prod"), "archon-alice")
	rec := incHistory(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestIncarnation_History_NotFound_404(t *testing.T) {
	// The existence-probe finds nothing → 404 without entering HistorySelectByName.
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodGet, "/v1/incarnations/ghost/history", nil, "name", "ghost")
	rec := incHistory(h, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404", rec.Code)
	}
}

func TestIncarnation_History_InvalidName_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodGet, "/v1/incarnations/Bad_Name/history", nil, "name", "Bad_Name")
	rec := incHistory(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

func TestIncarnation_History_ApplyIDFilter_200(t *testing.T) {
	// A valid ULID filter → 200, existence-probe + COUNT + SELECT.
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		countRow:        func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows:        func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(newChiRequest(http.MethodGet,
		"/v1/incarnations/redis-prod/history?apply_id=01HABCDEFGHJKMNPQRSTVWXYZ0", nil,
		"name", "redis-prod"), "archon-alice")
	rec := incHistory(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["total"] != float64(0) {
		t.Errorf("total = %v, want 0 (no matching apply_id)", out["total"])
	}
}

func TestIncarnation_History_ApplyIDInvalid_400(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	// Non-ULID: lowercase, wrong length.
	req := newChiRequest(http.MethodGet,
		"/v1/incarnations/redis-prod/history?apply_id=not-a-ulid", nil,
		"name", "redis-prod")
	rec := incHistory(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Code = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeMalformedRequest {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeMalformedRequest)
	}
}

func TestIncarnation_History_ApplyIDEmpty_OK(t *testing.T) {
	// Empty `apply_id` (e.g. `?apply_id=`) — the filter is ignored,
	// behavior as without the query-param (backward-compat).
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		countRow:        func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows:        func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(newChiRequest(http.MethodGet,
		"/v1/incarnations/redis-prod/history?apply_id=", nil,
		"name", "redis-prod"), "archon-alice")
	rec := incHistory(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200 (empty apply_id = no filter)", rec.Code)
	}
}

func TestIncarnation_Create_InvalidService_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"Bad/Service"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422", rec.Code)
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (validation must short-circuit)", db.insertCalls)
	}
}

// Capture-fake — intercepts the INSERT args so the test sees which spec
// went to the DB. Mimics only the INSERT path (Create); the other SQL scenarios
// fall into errRow.
type captureInsertDB struct {
	*fakeIncDB
	gotArgs []any
}

func (f *captureInsertDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO incarnation") {
		f.gotArgs = args
	}
	return f.fakeIncDB.QueryRow(ctx, sql, args...)
}

func TestIncarnation_Create_NilInput_SpecEmpty(t *testing.T) {
	// Body without `input` — spec must go to the DB as `{}`, not `{"input": null}`.
	db := &captureInsertDB{fakeIncDB: &fakeIncDB{}}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	// args[4] (1-based $5) — specBytes. See insertSQL in crud.go.
	if len(db.gotArgs) < 5 {
		t.Fatalf("insert args len = %d, want >= 5", len(db.gotArgs))
	}
	specBytes, ok := db.gotArgs[4].([]byte)
	if !ok {
		t.Fatalf("spec arg type = %T, want []byte", db.gotArgs[4])
	}
	if string(specBytes) != "{}" {
		t.Errorf("spec = %q, want \"{}\" (input not provided → no key)", string(specBytes))
	}
}

func TestIncarnation_List_OffsetBeyondTotal_200_EmptyItems(t *testing.T) {
	// COUNT returns 7, listRows — empty (mimics offset=100 at total=7).
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(7)}} },
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/incarnations?offset=100&limit=10", nil), "archon-alice")
	rec := incList(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items  []map[string]any `json:"items"`
		Total  int              `json:"total"`
		Offset int              `json:"offset"`
		Limit  int              `json:"limit"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 7 {
		t.Errorf("total = %d, want 7", out.Total)
	}
	if len(out.Items) != 0 {
		t.Errorf("items len = %d, want 0", len(out.Items))
	}
}

// --- List/Get scoped visibility (ADR-047 S3b-3) ------------------------

// incRows — a pgx.Rows stub iterating staticRow after staticRow (a multi-row list
// result for scanIncarnation). Analogous to the incarnation package's fakeRows; the
// handlers tests had only emptyRows/stringRows.
type incRows struct {
	rows []staticRow
	idx  int
}

func (r *incRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}
func (r *incRows) Scan(dest ...any) error                       { return r.rows[r.idx-1].Scan(dest...) }
func (r *incRows) Err() error                                   { return nil }
func (r *incRows) Close()                                       {}
func (r *incRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *incRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *incRows) Values() ([]any, error)                       { return nil, nil }
func (r *incRows) RawValues() [][]byte                          { return nil }
func (r *incRows) Conn() *pgx.Conn                              { return nil }

// incListRow builds a staticRow for the SelectAll list (15 columns in scanIncarnation
// order) with the given name/covens/state.
func incListRow(name string, covens []string, state map[string]any) staticRow {
	now := time.Now()
	stateBytes := []byte("{}")
	if state != nil {
		b, _ := json.Marshal(state)
		stateBytes = b
	}
	var covenArg any = []string(nil)
	if covens != nil {
		covenArg = covens
	}
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), stateBytes, "ready",
		[]byte(nil), any(nil),
		now, now, covenArg,
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		"create", // created_scenario (migration 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// doIncList runs List under the scoper with claims=archon-alice.
func doIncList(t *testing.T, h *IncarnationHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v1/incarnations"
	if query != "" {
		url += "?" + query
	}
	req := withClaims(httptest.NewRequest(http.MethodGet, url, nil), "archon-alice")
	rec := incList(h, req)
	return rec
}

// scopeArgHas — whether the bind-args include a []string argument equal to want (order
// matters; scope-covens/state-names bind as []string).
func scopeArgHas(args []any, want []string) bool {
	for _, a := range args {
		if got, ok := a.([]string); ok && len(got) == len(want) {
			eq := true
			for i := range got {
				if got[i] != want[i] {
					eq = false
					break
				}
			}
			if eq {
				return true
			}
		}
	}
	return false
}

// TestIncarnation_List_EmptyPurview_FailClosed — the MAIN security invariant
// (ADR-047): an operator with an empty Purview (default-deny, no coven/state dimensions)
// sees an EMPTY list, NOT all incarnations. fakeIncDB would return rows — the handler
// must return 0 and NOT touch SelectAll. Regress = the operator sees others'
// incarnations.
func TestIncarnation_List_EmptyPurview_FailClosed(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(5)}} },
		listRows: func() (pgx.Rows, error) {
			return &incRows{rows: []staticRow{incListRow("secret-inc", []string{"secret"}, nil)}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, fakeIncScoper{empty: true}, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("empty-purview total/len = %d/%d, want 0/0 (fail-closed, НЕ все incarnation)", out.Total, len(out.Items))
	}
	if db.listCalled {
		t.Errorf("fail-closed обязан НЕ ходить в SelectAll, но SelectAll вызван (args=%v)", db.lastCountArgs)
	}
}

// TestIncarnation_List_NoClaims_FailClosed — no claims (a defensive invariant,
// the route is under RequireJWT) → empty list.
func TestIncarnation_List_NoClaims_FailClosed(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(3)}} },
		listRows: func() (pgx.Rows, error) {
			return &incRows{rows: []staticRow{incListRow("secret-inc", nil, nil)}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations", nil) // no claims
	rec := incList(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Total int `json:"total"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Total != 0 || db.listCalled {
		t.Fatalf("no-claims: total=%d listCalled=%v, want 0/false (fail-closed)", out.Total, db.listCalled)
	}
}

// incListRowBare — like incListRow, but created_scenario = NULL (a bare incarnation,
// migration 090). scanIncarnation projects the 16th column into **string → nil.
func incListRowBare(name string) staticRow {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		any(nil), // created_scenario = NULL (bare, migration 090)
		any(nil), // applying_apply_id (ADR-068 §A1, bare → NULL)
	}}
}

// TestIncarnation_List_BareIncarnation_NoPanic — GUARD Phase 2 (handler-level,
// the real NULL projection of scanIncarnation on the list path): a list with a bare
// incarnation row (created_scenario IS NULL) → 200, no panic, the element arrives.
// scanIncarnation reads the 16th column into **string → nil; regress = NULL breaks
// the list projection (panic/scan error). The omitempty semantics of created_scenario on
// a real NULL row are checked by TestHumaIncarnation_Get_BareIncarnation_OmitsCreatedScenario
// (the huma-reply carries this field; the list test-shim does not project it — omitempty there
// is trivial and uninformative).
func TestIncarnation_List_BareIncarnation_NoPanic(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(1)}} },
		listRows: func() (pgx.Rows, error) {
			return &incRows{rows: []staticRow{incListRowBare("redis-bare")}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("total/len = %d/%d, want 1/1", out.Total, len(out.Items))
	}
	if out.Items[0]["name"] != "redis-bare" {
		t.Errorf("item name = %v, want redis-bare", out.Items[0]["name"])
	}
}

// TestIncarnation_List_NilScoper_FailClosed — the scoper is not configured → empty
// list (protection against mis-wire-up, don't expose all incarnations).
func TestIncarnation_List_NilScoper_FailClosed(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(3)}} },
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if db.listCalled {
		t.Errorf("nil-scoper обязан fail-closed (НЕ вызывать SelectAll)")
	}
}

// TestIncarnation_List_Unrestricted_All — `*`/bare-without-default Purview → the whole
// list without a scope predicate (scope-args are not added to the SQL).
func TestIncarnation_List_Unrestricted_All(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(2)}} },
		listRows: func() (pgx.Rows, error) {
			return &incRows{rows: []staticRow{
				incListRow("a", nil, nil), incListRow("b", nil, nil),
			}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("unrestricted total/len = %d/%d, want 2/2", out.Total, len(out.Items))
	}
	// Unrestricted → there must be NO scope-args ([]string) in COUNT.
	for _, a := range db.lastCountArgs {
		if _, ok := a.([]string); ok {
			t.Errorf("unrestricted scope добавил scope-args в SQL: %v", db.lastCountArgs)
		}
	}
}

// TestIncarnation_List_CovenScope_ReachesSQL — a coven-scoped operator: covens
// reach the SQL as the []string argument of the coven∪{name} pushdown.
func TestIncarnation_List_CovenScope_ReachesSQL(t *testing.T) {
	var listSQL string
	db := &fakeIncDB{
		countRow:       func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows:       func() (pgx.Rows, error) { return &emptyRows{}, nil },
		captureListSQL: func(sql string) { listSQL = sql },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"redis-prod"}}, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !scopeArgHas(db.lastCountArgs, []string{"redis-prod"}) {
		t.Errorf("coven-scope [redis-prod] не дошёл до SQL-args: %v", db.lastCountArgs)
	}
	// coven∪{name}: the SQL must contain both arms (covens && + name = ANY).
	if !strings.Contains(listSQL, "covens &&") || !strings.Contains(listSQL, "name = ANY") {
		t.Errorf("coven∪{name} SQL неполон (нужны covens && И name = ANY): %q", listSQL)
	}
}

// TestIncarnation_List_StateScope_ResolvedNamesReachSQL — the state dimension of Purview
// (StateExprs): the handler pre-resolves incarnation names via statepredicate and
// threads them as a `name = ANY($n)` pushdown. fakeIncDB: the state-resolve pass (page
// lister) finds redis-a (redis_version==8.0) → its name reaches scope.
func TestIncarnation_List_StateScope_ResolvedNamesReachSQL(t *testing.T) {
	// One fakeIncDB serves BOTH the state-resolve (page-lister SelectAll) AND
	// the final list-SelectAll. Both go through Query/QueryRow on the same SQL
	// signatures; the state-lister returns real rows (for CEL eval), the final
	// list — empty (we only care about scope-args).
	resolveRows := []staticRow{
		incListRow("redis-a", nil, map[string]any{"redis_version": "8.0"}),
		incListRow("redis-b", nil, map[string]any{"redis_version": "7.2"}),
	}
	var queryCall int
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(len(resolveRows))}} },
	}
	db.listRows = func() (pgx.Rows, error) {
		queryCall++
		if queryCall == 1 {
			// The first Query — the page-lister state-resolve: return rows for CEL.
			return &incRows{rows: resolveRows}, nil
		}
		// The rest — the final list: empty (we care about scope-args).
		return &emptyRows{}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The name redis-a (state-matched) must reach the final list as a scope-arg.
	if !scopeArgHas(db.lastCountArgs, []string{"redis-a"}) {
		t.Errorf("state-резолв: имя redis-a не дошло до финального list-SQL как scope-name: %v", db.lastCountArgs)
	}
}

// TestIncarnation_List_OR_CovenAndState_Union — OR of dimensions: coven ∪ state =
// union. coven=prod + state redis8 → the final list receives BOTH scope-covens [prod]
// AND the state-resolved names. Both arms are present in args.
func TestIncarnation_List_OR_CovenAndState_Union(t *testing.T) {
	resolveRows := []staticRow{incListRow("redis-8", nil, map[string]any{"redis_version": "8.0"})}
	var queryCall int
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(1)}} },
	}
	db.listRows = func() (pgx.Rows, error) {
		queryCall++
		if queryCall == 1 {
			return &incRows{rows: resolveRows}, nil
		}
		return &emptyRows{}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}, stateExprs: []string{`state.redis_version == "8.0"`}}, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !scopeArgHas(db.lastCountArgs, []string{"prod"}) {
		t.Errorf("OR-union: coven-плечо [prod] не дошло до SQL: %v", db.lastCountArgs)
	}
	if !scopeArgHas(db.lastCountArgs, []string{"redis-8"}) {
		t.Errorf("OR-union: state-плечо [redis-8] не дошло до SQL: %v", db.lastCountArgs)
	}
}

// --- Get scoped (ADR-047 S3b-3) ---------------------------------------

func doIncGet(t *testing.T, h *IncarnationHandler, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := withClaims(newChiRequest(http.MethodGet, "/v1/incarnations/"+name, nil, "name", name), "archon-alice")
	rec := incGet(h, req)
	return rec
}

// TestIncarnation_Get_EmptyPurview_404 — fail-closed: an empty Purview → 404 (don't
// leak the existence of another's incarnation), even though the row exists in the DB.
func TestIncarnation_Get_EmptyPurview_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, fakeIncScoper{empty: true}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (out-of-scope не палим как 403)", rec.Code)
	}
}

// TestIncarnation_Get_NilScoper_404 — nil-scoper → 404 (fail-closed mis-wire-up).
func TestIncarnation_Get_NilScoper_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (nil-scoper fail-closed)", rec.Code)
	}
}

// TestIncarnation_Get_Unrestricted_200 — Unrestricted → 200.
func TestIncarnation_Get_Unrestricted_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (unrestricted)", rec.Code)
	}
}

// TestIncarnation_Get_CovenMatch_200 — an incarnation in a scope-coven (by covens[]) →
// 200.
func TestIncarnation_Get_CovenMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incListRow(name, []string{"prod"}, nil)
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (coven-match по covens[])", rec.Code)
	}
}

// TestIncarnation_Get_NameMatch_200 — coven∪{name}: an incarnation WITHOUT matching
// covens[], but whose NAME = a scope-coven (ADR-008 root label) → 200. Regress =
// an operator with scope coven=redis-prod does not see incarnation redis-prod.
func TestIncarnation_Get_NameMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incListRow(name, []string{"other-coven"}, nil) // covens do NOT contain redis-prod
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"redis-prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (coven∪{name}: name=redis-prod матчит scope coven=redis-prod)", rec.Code)
	}
}

// TestIncarnation_Get_CovenMismatch_404 — an incarnation outside the scope-covens and whose name doesn't
// match → 404.
func TestIncarnation_Get_CovenMismatch_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incListRow(name, []string{"staging"}, nil)
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (incarnation вне scope)", rec.Code)
	}
}

// TestIncarnation_Get_StateMatch_200 — the state dimension: an incarnation whose state
// satisfies a scope StateExpr → 200 (without a coven match).
func TestIncarnation_Get_StateMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incListRow(name, []string{"staging"}, map[string]any{"redis_version": "8.0"})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (state-match по StateExpr)", rec.Code)
	}
}

// TestIncarnation_Get_StateMismatch_404 — state satisfies no
// StateExpr and there is no coven match → 404.
func TestIncarnation_Get_StateMismatch_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incListRow(name, []string{"staging"}, map[string]any{"redis_version": "7.2"})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (state не матчит)", rec.Code)
	}
}

// doIncHistory — handler-direct History with claims (mirror of doIncGet). Returns
// a recorder; db must provide selectByNameRow (existence-probe + scope), count/list.
func doIncHistory(t *testing.T, h *IncarnationHandler, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := withClaims(newChiRequest(http.MethodGet, "/v1/incarnations/"+name+"/history", nil, "name", name), "archon-alice")
	rec := incHistory(h, req)
	return rec
}

// fakeIncHistoryDB — a fakeIncDB for History scope tests: the existence-probe carries
// covens/state (via incListRow), COUNT=0 + empty list (the scope branch is checked
// before revealing the timeline, so filling history is pointless).
func fakeIncHistoryDB(name string, covens []string, state map[string]any) *fakeIncDB {
	row := incListRow(name, covens, state)
	return &fakeIncDB{
		selectByNameRow: func(string) pgx.Row { return row },
		countRow:        func(string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows:        func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
}

// TestIncarnation_History_StateMatch_200 — the History gate moved to existence-
// only RequireAction (ADR-047 §d): a state-scoped operator reaches the handler,
// narrowing via getInScope("history"). state matches a StateExpr → 200. Regress
// (before Fix 2) = a state-scoped operator got 403 at the Multi route-gate (the state
// dimension does not resolve in the incarnation context → deny BEFORE the handler).
func TestIncarnation_History_StateMatch_200(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"staging"}, map[string]any{"redis_version": "8.0"})
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (state-scoped видит историю своей incarnation)", rec.Code)
	}
}

// TestIncarnation_History_StateMismatch_404 — state matches no StateExpr, no
// coven match → 404 (another's incarnation history is not revealed).
func TestIncarnation_History_StateMismatch_404(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"staging"}, map[string]any{"redis_version": "7.2"})
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (state не матчит — история скрыта)", rec.Code)
	}
}

// TestIncarnation_History_CovenMatch_200 — a coven-scoped operator sees the history of
// an incarnation in its coven (parity with Get; coven-scope used to match via the Multi
// gate too, now — via getInScope("history")).
func TestIncarnation_History_CovenMatch_200(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"prod"}, nil)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (coven-match)", rec.Code)
	}
}

// TestIncarnation_History_EmptyPurview_404 — fail-closed: an empty Purview → 404
// (the history of an existing-but-foreign incarnation is not revealed).
func TestIncarnation_History_EmptyPurview_404(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"prod"}, nil)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, fakeIncScoper{empty: true}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (empty-purview fail-closed)", rec.Code)
	}
}

// --- RBAC scope selectors (ADR-008 amendment a) -----------------------

// hasCovenCtx — whether the set contains a context with the given coven (+ service, if
// set); the incarnation key is checked to match name.
func hasCovenCtx(ctxs []map[string]string, name, service, coven string) bool {
	for _, c := range ctxs {
		if c["incarnation"] == name && c["coven"] == coven && c["service"] == service {
			return true
		}
	}
	return false
}

func TestIncarnationCovenContexts_DeclaredPlusName(t *testing.T) {
	ctxs := incarnationCovenContexts("redis-prod", "redis", []string{"prod", "dc1"})
	// covens ∪ {name} = {prod, dc1, redis-prod}, service in all.
	if len(ctxs) != 3 {
		t.Fatalf("len = %d, want 3: %v", len(ctxs), ctxs)
	}
	for _, coven := range []string{"prod", "dc1", "redis-prod"} {
		if !hasCovenCtx(ctxs, "redis-prod", "redis", coven) {
			t.Errorf("missing context for coven=%q: %v", coven, ctxs)
		}
	}
}

func TestIncarnationCovenContexts_NameDedup(t *testing.T) {
	// name is already in covens → not duplicated.
	ctxs := incarnationCovenContexts("prod", "redis", []string{"prod"})
	if len(ctxs) != 1 {
		t.Fatalf("len = %d, want 1 (dedup): %v", len(ctxs), ctxs)
	}
}

func TestIncarnationCovenContexts_EmptyCovens_NameOnly(t *testing.T) {
	ctxs := incarnationCovenContexts("foo", "redis", nil)
	if len(ctxs) != 1 || !hasCovenCtx(ctxs, "foo", "redis", "foo") {
		t.Fatalf("want single name-as-coven context, got %v", ctxs)
	}
}

func TestIncarnationCovenContexts_EmptyName_Nil(t *testing.T) {
	if got := incarnationCovenContexts("", "redis", []string{"prod"}); got != nil {
		t.Errorf("got = %v, want nil for empty name", got)
	}
}

func TestIncarnationScopeSelector_ReadsRow(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		// service=redis, covens=[prod] (see makeIncStatusRow service=redis;
		// we add covens manually).
		now := time.Now()
		return staticRow{values: []any{
			name, "redis", "v1", int(1),
			[]byte("{}"), []byte("{}"), "ready",
			[]byte(nil), any(nil),
			now, now, []string{"prod"},
			[]byte("{}"),          // traits
			any(nil), []byte(nil), // ADR-031 Slice C
			"create", // created_scenario (migration 089, NOT NULL DEFAULT)
			any(nil), // applying_apply_id (ADR-068 §A1)
		}}
	}}
	sel := IncarnationScopeSelector(db)
	req := newChiRequest(http.MethodGet, "/v1/incarnations/redis-prod", nil, "name", "redis-prod")
	ctxs := sel(req)
	for _, coven := range []string{"prod", "redis-prod"} {
		if !hasCovenCtx(ctxs, "redis-prod", "redis", coven) {
			t.Errorf("missing coven=%q in %v", coven, ctxs)
		}
	}
}

func TestIncarnationScopeSelector_NotFound_Nil(t *testing.T) {
	db := &fakeIncDB{} // SelectByName → ErrNoRows
	sel := IncarnationScopeSelector(db)
	req := newChiRequest(http.MethodGet, "/v1/incarnations/missing", nil, "name", "missing")
	if got := sel(req); got != nil {
		t.Errorf("got = %v, want nil (fail-closed) for not-found", got)
	}
}

func TestIncarnationScopeSelector_InvalidName_Nil(t *testing.T) {
	db := &fakeIncDB{}
	sel := IncarnationScopeSelector(db)
	req := newChiRequest(http.MethodGet, "/v1/incarnations/Bad_Name", nil, "name", "Bad_Name")
	if got := sel(req); got != nil {
		t.Errorf("got = %v, want nil for invalid name", got)
	}
}

func TestIncarnationCreateScopeSelector_FromBody(t *testing.T) {
	body := bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","covens":["prod"]}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", body)
	ctxs := IncarnationCreateScopeSelector(req)
	for _, coven := range []string{"prod", "redis-prod"} {
		if !hasCovenCtx(ctxs, "redis-prod", "redis", coven) {
			t.Errorf("missing coven=%q in %v", coven, ctxs)
		}
	}
	// The body is restored for the handler.
	rest, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(rest), "redis-prod") {
		t.Errorf("body not restored for handler: %q", rest)
	}
}

func TestIncarnationCreateScopeSelector_NoName_Nil(t *testing.T) {
	body := bytes.NewReader([]byte(`{"service":"redis"}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", body)
	if got := IncarnationCreateScopeSelector(req); got != nil {
		t.Errorf("got = %v, want nil for missing name", got)
	}
}

func TestIncarnationCreateScopeSelector_BadJSON_Nil(t *testing.T) {
	body := bytes.NewReader([]byte(`{bad`))
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", body)
	if got := IncarnationCreateScopeSelector(req); got != nil {
		t.Errorf("got = %v, want nil for bad JSON", got)
	}
}

// --- Run / Unlock fakes -----------------------------------------------

// fakeStarter — a mock [ScenarioStarter]. Captures RunSpec and returns
// the given error (nil → success).
type fakeStarter struct {
	gotSpec scenario.RunSpec
	calls   int
	err     error
}

func (f *fakeStarter) Start(_ context.Context, spec scenario.RunSpec) error {
	f.calls++
	f.gotSpec = spec
	return f.err
}

// fakeResolver — a mock [ServiceResolver]. ok=false → service not registered.
type fakeResolver struct {
	ok bool
}

func (f *fakeResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{Name: service, Ref: "v1"}, f.ok
}

// fakeIncScoper — a mock [PurviewResolver] for scoped List/Get tests (ADR-047
// S3b-3). The fields map into [rbac.Purview]: covens → Covens, stateExprs → StateExprs,
// traitExprs → TraitExprs (ADR-060 §7 slice 1, `key:value` pairs).
// empty=true → Purview{} (fail-closed). Symmetric with soul_test.fakeScoper.
type fakeIncScoper struct {
	covens       []string
	stateExprs   []string
	traitExprs   []string
	unrestricted bool
	empty        bool
}

func (s fakeIncScoper) ResolvePurview(_, _, _ string) rbac.Purview {
	if s.empty {
		return rbac.Purview{}
	}
	return rbac.Purview{
		Covens:       s.covens,
		StateExprs:   s.stateExprs,
		TraitExprs:   s.traitExprs,
		Unrestricted: s.unrestricted,
	}
}

// unrestrictedScoper — a typical Unrestricted scoper for existing List/Get
// tests that check not scope but the filter/sort SQL path (scope removed →
// unchanged behavior). Avoids duplicating the literal in every test.
func unrestrictedScoper() fakeIncScoper { return fakeIncScoper{unrestricted: true} }

// newChiRequestScenario builds a request with two chi URL params (name +
// scenario) for the Run handler — it reads both via chi.URLParam.
func newChiRequestScenario(method, path string, body *bytes.Reader, name, scenarioName string) *http.Request {
	var b *bytes.Reader
	if body != nil {
		b = body
	} else {
		b = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, b)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	rctx.URLParams.Add("scenario", scenarioName)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	return r
}

// makeIncStatusRow builds a staticRow for SelectByName with a given
// status (for the Run-probe error_locked).
func makeIncStatusRow(name, status string) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits
		any(nil), []byte(nil), // ADR-031 Slice C
		"create", // created_scenario (migration 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// makeUnlockSelectRow builds a staticRow for the FOR UPDATE select of the unlock family.
// Carries 4 columns (state, status, created_scenario, spec): plain Unlock / Destroy
// scan the first two, UnlockForRerun — all four.
func makeUnlockSelectRow(status string) pgx.Row {
	return staticRow{values: []any{[]byte("{}"), status, "create", []byte("{}")}}
}

// makeUnlockSelectRowSpec — like makeUnlockSelectRow, but spec carries the given jsonb
// (checks threading spec.input into RunSpec.Input on the create path).
func makeUnlockSelectRowSpec(status string, specJSON []byte) pgx.Row {
	return staticRow{values: []any{[]byte("{}"), status, "create", specJSON}}
}

// makeUnlockSelectRowBare — like makeUnlockSelectRow, but created_scenario = NULL
// (a bare incarnation): the 3rd scan-dest **string gets nil.
func makeUnlockSelectRowBare(status string) pgx.Row {
	return staticRow{values: []any{[]byte("{}"), status, any(nil), []byte("{}")}}
}

func newRunHandler(db *fakeIncDB, starter *fakeStarter, resolver *fakeResolver) *IncarnationHandler {
	return NewIncarnationHandler(db, starter, nil, nil, resolver, nil, nil, nil, nil)
}

// --- Run --------------------------------------------------------------

func TestIncarnation_Run_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	starter := &fakeStarter{}
	h := newRunHandler(db, starter, &fakeResolver{ok: true})
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/redis-prod/scenarios/add_user",
		bytes.NewReader([]byte(`{"input":{"user":"bob"}}`)),
		"redis-prod", "add_user")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw["incarnation"] != "redis-prod" || raw["scenario"] != "add_user" {
		t.Errorf("reply echo wrong: %v", raw)
	}
	if applyID, _ := raw["apply_id"].(string); len(applyID) != 26 {
		t.Errorf("apply_id len = %d, want 26", len(applyID))
	}
	if starter.calls != 1 {
		t.Fatalf("starter.calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "add_user" {
		t.Errorf("RunSpec.ScenarioName = %q, want add_user", starter.gotSpec.ScenarioName)
	}
	if starter.gotSpec.IncarnationName != "redis-prod" {
		t.Errorf("RunSpec.IncarnationName = %q", starter.gotSpec.IncarnationName)
	}
	if starter.gotSpec.StartedByAID != "archon-alice" {
		t.Errorf("RunSpec.StartedByAID = %q", starter.gotSpec.StartedByAID)
	}
}

func TestIncarnation_Run_MissingIncarnation_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	starter := &fakeStarter{}
	h := newRunHandler(db, starter, &fakeResolver{ok: true})
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/ghost/scenarios/add_user", nil, "ghost", "add_user")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Code = %d, want 404", rec.Code)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (must not start for missing)", starter.calls)
	}
}

func TestIncarnation_Run_ErrorLocked_409(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "error_locked") },
	}
	starter := &fakeStarter{}
	h := newRunHandler(db, starter, &fakeResolver{ok: true})
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/redis-prod/scenarios/add_user", nil, "redis-prod", "add_user")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("Code = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeIncarnationLocked {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeIncarnationLocked)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (locked must not start)", starter.calls)
	}
}

func TestIncarnation_Run_UnknownService_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	starter := &fakeStarter{}
	h := newRunHandler(db, starter, &fakeResolver{ok: false})
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/redis-prod/scenarios/add_user", nil, "redis-prod", "add_user")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422", rec.Code)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0", starter.calls)
	}
}

func TestIncarnation_Run_NoBody_202(t *testing.T) {
	// Scenario without input: empty body → 202 (input is optional).
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	starter := &fakeStarter{}
	h := newRunHandler(db, starter, &fakeResolver{ok: true})
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/redis-prod/scenarios/restart", nil, "redis-prod", "restart")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if starter.gotSpec.Input != nil {
		t.Errorf("RunSpec.Input = %v, want nil (no body)", starter.gotSpec.Input)
	}
}

func TestIncarnation_Run_NoRunner_500(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/redis-prod/scenarios/add_user", nil, "redis-prod", "add_user")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500 (runner not configured)", rec.Code)
	}
}

// makeIncStatusRowBare — like makeIncStatusRow, but created_scenario = NULL (a bare
// incarnation, migration 090: created without a bootstrap scenario). scanIncarnation reads
// the 16th column into **string → nil.
func makeIncStatusRowBare(name, status string) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits
		any(nil), []byte(nil), // ADR-031 Slice C
		any(nil), // created_scenario = NULL (bare, migration 090)
		any(nil), // applying_apply_id (ADR-068 §A1, bare → NULL)
	}}
}

// TestIncarnation_Run_BareIncarnation_Day2_202 — GUARD Phase 2: a bare incarnation
// (created_scenario IS NULL) runs an ORDINARY operational scenario (day-2) via
// RunTyped normally — 202, the run starts. RunTyped resolves the incarnation by
// SelectByName and does NOT read created_scenario for a day-2 run (it is needed only for
// rerun-last on the create path). Regress = the day-2 path starts requiring created_scenario non-NULL
// (or panics on the NULL projection) → bare incarnations lose day-2 operations.
func TestIncarnation_Run_BareIncarnation_Day2_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRowBare(name, "ready") },
	}
	starter := &fakeStarter{}
	h := newRunHandler(db, starter, &fakeResolver{ok: true})
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/redis-bare/scenarios/add_user",
		bytes.NewReader([]byte(`{"input":{"user":"bob"}}`)),
		"redis-bare", "add_user")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202 (bare day-2 run), body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Fatalf("starter.calls = %d, want 1 (bare инкарнация допускает day-2 operational-сценарий)", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "add_user" {
		t.Errorf("RunSpec.ScenarioName = %q, want add_user", starter.gotSpec.ScenarioName)
	}
}

// --- Unlock -----------------------------------------------------------

func TestIncarnation_Unlock_200(t *testing.T) {
	db := &fakeIncDB{
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow("error_locked") },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock",
		bytes.NewReader([]byte(`{"reason":"manual cleanup verified"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUnlock(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Name           string `json:"name"`
		PreviousStatus string `json:"previous_status"`
		Status         string `json:"status"`
		UnlockedByAID  string `json:"unlocked_by_aid"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PreviousStatus != "error_locked" {
		t.Errorf("previous_status = %q, want error_locked", out.PreviousStatus)
	}
	if out.Status != "ready" {
		t.Errorf("status = %q, want ready", out.Status)
	}
	if out.UnlockedByAID != "archon-alice" {
		t.Errorf("unlocked_by_aid = %q", out.UnlockedByAID)
	}
	if out.Name != "redis-prod" {
		t.Errorf("name = %q", out.Name)
	}
	// Unlock writes a state_history INSERT + UPDATE → 2 Execs.
	if len(db.execCalls) != 2 {
		t.Errorf("execCalls = %d, want 2 (history INSERT + status UPDATE)", len(db.execCalls))
	}
}

func TestIncarnation_Unlock_NotLocked_409(t *testing.T) {
	db := &fakeIncDB{
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow("ready") },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock",
		bytes.NewReader([]byte(`{"reason":"x"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUnlock(h, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("Code = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeIncarnationLocked {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeIncarnationLocked)
	}
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %d, want 0 (not-locked must not mutate)", len(db.execCalls))
	}
}

func TestIncarnation_Unlock_MissingIncarnation_404(t *testing.T) {
	db := &fakeIncDB{
		unlockSelectRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/ghost/unlock",
		bytes.NewReader([]byte(`{"reason":"x"}`)), "name", "ghost")
	req = withClaims(req, "archon-alice")
	rec := incUnlock(h, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404", rec.Code)
	}
}

func TestIncarnation_Unlock_NoReason_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock",
		bytes.NewReader([]byte(`{}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUnlock(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %d, want 0 (no reason → no tx)", len(db.execCalls))
	}
}

// TestIncarnation_Unlock_ReasonAtMax_200 — a reason of exactly ReasonMaxLen characters
// passes (inclusive boundary): unlock proceeds, 200 + 2 Execs.
func TestIncarnation_Unlock_ReasonAtMax_200(t *testing.T) {
	db := &fakeIncDB{
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow("error_locked") },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	reason := strings.Repeat("a", incarnation.ReasonMaxLen)
	body, _ := json.Marshal(map[string]string{"reason": reason})
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock",
		bytes.NewReader(body), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUnlock(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200 (reason ровно %d допустим), body=%s",
			rec.Code, incarnation.ReasonMaxLen, rec.Body.String())
	}
	if len(db.execCalls) != 2 {
		t.Errorf("execCalls = %d, want 2 (history INSERT + status UPDATE)", len(db.execCalls))
	}
}

// TestIncarnation_Unlock_ReasonOverMax_422 — a reason longer than ReasonMaxLen → 422
// BEFORE the transaction (the upper reason boundary, a behavioral invariant).
func TestIncarnation_Unlock_ReasonOverMax_422(t *testing.T) {
	db := &fakeIncDB{
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow("error_locked") },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	reason := strings.Repeat("a", incarnation.ReasonMaxLen+1)
	body, _ := json.Marshal(map[string]string{"reason": reason})
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock",
		bytes.NewReader(body), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUnlock(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422 (reason %d > max), body=%s",
			rec.Code, incarnation.ReasonMaxLen+1, rec.Body.String())
	}
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %d, want 0 (reason over max → no tx)", len(db.execCalls))
	}
}

// --- Upgrade ----------------------------------------------------------

// fakeLoader — a mock [ServiceSnapshotLoader]. Load returns a snapshot with
// the given target schema version; LoadMigrationChain returns chain/err from
// preset fields. The git stack is not brought up.
type fakeLoader struct {
	targetSchema int
	loadErr      error

	chain    statemigrate.Chain
	chainErr error

	// destroy pre-check (ReadFile): hasDestroyScenario=true → scenario `destroy`
	// "exists" in the snapshot; false → os.ErrNotExist. readErr overrides (I/O failure).
	hasDestroyScenario bool
	readErr            error

	// scenarioYAML — for sync input validation (scenario.ValidateInput):
	// non-empty → ReadFile returns this YAML as scenario/<name>/main.yml.
	// Overrides the hasDestroyScenario branch.
	scenarioYAML string

	// localDir — the root of a real on-disk snapshot (temp). Non-empty → Load
	// sets it in ServiceArtifact.LocalDir, and ReadFile reads the requested
	// file from disk (path-aware). Needed for the multi-create-scenario mechanism:
	// scenario.ResolveCreateScenarios scans art.LocalDir (artifact.ListScenarios),
	// and scenario.ValidateInput reads scenario/<chosen>/main.yml — both phases must
	// see the same snapshot, so a per-path response is required. Mirrors
	// dirInputLoader from scenario/validate_input_types_test.go. Overrides the
	// scenarioYAML/hasDestroyScenario branches in ReadFile.
	localDir string

	// lifecycle — the lifecycle block of the snapshot manifest (S3: auto_create/auto_destroy).
	// nil → a manifest without lifecycle (both flags default to true, backcompat).
	lifecycle *config.LifecycleConfig

	// stateSchema — the flat state_schema of the snapshot manifest (seal read-path:
	// secretSchemaForIncarnation walks it for secret:true). nil → no state_schema.
	stateSchema map[string]any

	// revealableSecrets — the revealable_secrets section of the snapshot manifest (NIM-74):
	// revealableSecretsFor reads it on the reveal endpoint. nil → no reveal declarations.
	revealableSecrets []config.RevealableSecret

	loadCalls     int
	chainCalls    int
	readFileCalls int
	upgradesCalls int

	// loadedRefs records the ref.Ref of each Load (call order) — the version-pin
	// guard tests verify which service version the snapshot materialized on.
	loadedRefs []string

	// upgrades — the result of ListUpgrades (ADR-0068): a non-empty list with FromVersions
	// matching the current pin → the found branch of UpgradeTyped. nil → legacy.
	upgrades []artifact.Scenario
}

func (f *fakeLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	f.loadCalls++
	f.loadedRefs = append(f.loadedRefs, ref.Ref)
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return &artifact.ServiceArtifact{
		Ref:      ref,
		LocalDir: f.localDir,
		Manifest: &config.ServiceManifest{StateSchemaVersion: f.targetSchema, Lifecycle: f.lifecycle, StateSchema: f.stateSchema, RevealableSecrets: f.revealableSecrets},
	}, nil
}

func (f *fakeLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	f.chainCalls++
	if f.chainErr != nil {
		return nil, f.chainErr
	}
	return f.chain, nil
}

func (f *fakeLoader) ListUpgrades(_ *artifact.ServiceArtifact) ([]artifact.Scenario, error) {
	f.upgradesCalls++
	return f.upgrades, nil
}

// ReadFile — for the destroy PrepareDestroy pre-check (presence of scenario `destroy`)
// and sync input validation. localDir (if set) — path-aware read from disk
// (reads scenario/<name>/main.yml of the real snapshot); otherwise the scenarioYAML/
// hasDestroyScenario stubs. hasDestroyScenario=true → the file "exists"; false →
// os.ErrNotExist (no scenario). readErr (if set) overrides everything — for the I/O-failure test.
func (f *fakeLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	f.readFileCalls++
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.localDir != "" {
		return os.ReadFile(filepath.Join(f.localDir, filepath.FromSlash(file)))
	}
	if f.scenarioYAML != "" {
		return []byte(f.scenarioYAML), nil
	}
	if f.hasDestroyScenario {
		return []byte("tasks: []\n"), nil
	}
	return nil, os.ErrNotExist
}

// makeIncRowVer builds a staticRow for SelectByName with given
// service_version and state_schema_version (for the Upgrade `from` resolve).
func makeIncRowVer(name, serviceVersion string, schema int) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", serviceVersion, schema,
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits
		any(nil), []byte(nil), // ADR-031 Slice C
		"create", // created_scenario (migration 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// makeUpgradeSelectRow builds a staticRow for the Upgrade SELECT FOR UPDATE
// (state, state_schema_version, status).
func makeUpgradeSelectRow(schema int, status string) pgx.Row {
	return staticRow{values: []any{[]byte("{}"), schema, status}}
}

func newUpgradeHandler(db *fakeIncDB, loader *fakeLoader) *IncarnationHandler {
	return NewIncarnationHandler(db, &fakeStarter{}, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
}

func TestIncarnation_Upgrade_202(t *testing.T) {
	// A real upgrade v1→v2, schema 1→2: a chain with one migration. Happy path
	// (status ready) → 202 + apply_id.
	mig, err := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	db := &fakeIncDB{
		selectByNameRow:  func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
		upgradeSelectRow: func(_ string) pgx.Row { return makeUpgradeSelectRow(1, "ready") },
	}
	loader := &fakeLoader{targetSchema: 2, chain: statemigrate.Chain{mig}}
	h := newUpgradeHandler(db, loader)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	applyID, _ := raw["apply_id"].(string)
	if len(applyID) != 26 {
		t.Errorf("apply_id len = %d, want 26 (ULID)", len(applyID))
	}
	if loader.loadCalls != 1 || loader.chainCalls != 1 {
		t.Errorf("loadCalls=%d chainCalls=%d, want 1/1", loader.loadCalls, loader.chainCalls)
	}
}

// TestIncarnation_Upgrade_FoundAutostart_202 — the found branch (ADR-0068 §5): for
// the v1→v2 transition there is an upgrade scenario → 202 carries run_apply_id=R (≠ M), the Runner
// got RunSpec{FromUpgrade:true, FromLocked:true, ApplyID:R, ScenarioName:slug,
// ServiceRef.Ref:to_version}.
func TestIncarnation_Upgrade_FoundAutostart_202(t *testing.T) {
	mig, err := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	db := &fakeIncDB{
		selectByNameRow:  func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
		upgradeSelectRow: func(_ string) pgx.Row { return makeUpgradeSelectRow(1, "ready") },
	}
	loader := &fakeLoader{
		targetSchema: 2,
		chain:        statemigrate.Chain{mig},
		upgrades:     []artifact.Scenario{{Name: "to_v2", FromVersions: []string{"v1"}}},
	}
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	applyID, _ := raw["apply_id"].(string)
	runApplyID, _ := raw["run_apply_id"].(string)
	if len(runApplyID) != 26 {
		t.Errorf("run_apply_id = %q, want 26-char ULID (found-ветвь)", runApplyID)
	}
	if runApplyID == applyID {
		t.Errorf("run_apply_id == apply_id (%q) — R и M обязаны отличаться", applyID)
	}
	if starter.calls != 1 {
		t.Fatalf("runner.Start calls = %d, want 1 (автозапуск upgrade-сценария)", starter.calls)
	}
	sp := starter.gotSpec
	if !sp.FromUpgrade || !sp.FromLocked {
		t.Errorf("RunSpec FromUpgrade=%v FromLocked=%v, want both true", sp.FromUpgrade, sp.FromLocked)
	}
	if sp.ScenarioName != "to_v2" {
		t.Errorf("RunSpec.ScenarioName = %q, want to_v2 (slug)", sp.ScenarioName)
	}
	if sp.ApplyID != runApplyID {
		t.Errorf("RunSpec.ApplyID = %q, want run_apply_id %q (R)", sp.ApplyID, runApplyID)
	}
	if sp.ServiceRef.Ref != "v2" {
		t.Errorf("RunSpec.ServiceRef.Ref = %q, want v2 (пин цели)", sp.ServiceRef.Ref)
	}
	if sp.StartedByAID != "archon-alice" {
		t.Errorf("RunSpec.StartedByAID = %q, want archon-alice", sp.StartedByAID)
	}
	// ADR-0068 §7 invariant: the upgrade scenario works with state, NOT input —
	// input is NOT migrated. RunSpec.Input must be nil (regress if someone
	// starts smuggling spec.input into the upgrade run).
	if sp.Input != nil {
		t.Errorf("RunSpec.Input = %v, want nil (input НЕ мигрируется, ADR-0068 §7)", sp.Input)
	}
}

// TestIncarnation_Upgrade_FoundNilRunner_500 — the found branch (an upgrade scenario exists),
// but the runner is not configured → 500 BEFORE reserving applying (anti-zombie, ADR-0068
// §5: the incarnation must not hang in applying without a Runner run).
func TestIncarnation_Upgrade_FoundNilRunner_500(t *testing.T) {
	mig, err := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	selectCalled := false
	db := &fakeIncDB{
		selectByNameRow:  func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
		upgradeSelectRow: func(_ string) pgx.Row { selectCalled = true; return makeUpgradeSelectRow(1, "ready") },
	}
	loader := &fakeLoader{
		targetSchema: 2,
		chain:        statemigrate.Chain{mig},
		upgrades:     []artifact.Scenario{{Name: "to_v2", FromVersions: []string{"v1"}}},
	}
	// runner=nil (2nd arg) with an upgrade scenario found.
	h := NewIncarnationHandler(db, nil, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Code = %d, want 500 (found + runner=nil); body=%s", rec.Code, rec.Body.String())
	}
	// Anti-zombie: applying was NOT reserved (UpgradeStateSchema SELECT FOR UPDATE
	// not called, no tx opened) — the runner=nil check is BEFORE the status change.
	if selectCalled {
		t.Error("UpgradeStateSchema SELECT FOR UPDATE вызван — applying зарезервирован ДО проверки runner (анти-зомби нарушен)")
	}
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %v, want пусто (никакого tx до 500)", db.execCalls)
	}
}

// TestIncarnation_Upgrade_LegacyNoRun_202 — legacy (no upgrade scenario found):
// 202 WITHOUT run_apply_id, the Runner is NOT called (drift + WARN, host rollout is manual).
func TestIncarnation_Upgrade_LegacyNoRun_202(t *testing.T) {
	mig, err := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	db := &fakeIncDB{
		selectByNameRow:  func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
		upgradeSelectRow: func(_ string) pgx.Row { return makeUpgradeSelectRow(1, "ready") },
	}
	loader := &fakeLoader{targetSchema: 2, chain: statemigrate.Chain{mig}} // upgrades nil → legacy
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["run_apply_id"]; present {
		t.Errorf("legacy reply несёт run_apply_id (%v) — должно быть опущено (omitempty)", raw["run_apply_id"])
	}
	if starter.calls != 0 {
		t.Errorf("runner.Start calls = %d, want 0 (legacy — без автозапуска)", starter.calls)
	}
}

func TestIncarnation_Upgrade_RefBump_202(t *testing.T) {
	// A ref change at the same schema (target == current, to_version != the current
	// service_version) — a legitimate ref-bump: empty chain, 202.
	db := &fakeIncDB{
		selectByNameRow:  func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
		upgradeSelectRow: func(_ string) pgx.Row { return makeUpgradeSelectRow(1, "ready") },
	}
	loader := &fakeLoader{targetSchema: 1, chain: statemigrate.Chain{}}
	h := newUpgradeHandler(db, loader)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v1-hotfix"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202 (ref-bump), body=%s", rec.Code, rec.Body.String())
	}
}

func TestIncarnation_Upgrade_NoOpSameVersion_422(t *testing.T) {
	// Full match: to_version == the current service_version AND the same schema
	// → 422 (nothing to upgrade). Does not reach LoadMigrationChain.
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowVer(name, "v2", 2) },
	}
	loader := &fakeLoader{targetSchema: 2}
	h := newUpgradeHandler(db, loader)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if loader.chainCalls != 0 {
		t.Errorf("chainCalls = %d, want 0 (no-op must short-circuit)", loader.chainCalls)
	}
}

func TestIncarnation_Upgrade_DowngradeViaRef_409(t *testing.T) {
	// Downgrade via git-ref: the incarnation is on schema 3, the target ref carries
	// schema 2 (target < current). An early guard in the handler returns 409
	// (forward-only, ADR-019) BEFORE calling LoadMigrationChain — the real path,
	// which used to fall into 500 (the loader on from>to returns a plain error,
	// not ErrMigrationChainBroken). Differs from the downgrade case in
	// SentinelMapping: there SelectByName sees a compatible schema, and downgrade
	// is detected later under FOR UPDATE (race protection).
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowVer(name, "v3", 3) },
	}
	loader := &fakeLoader{targetSchema: 2, chain: statemigrate.Chain{}}
	h := newUpgradeHandler(db, loader)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("Code = %d, want 409 (downgrade via ref), body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeIncarnationLocked {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeIncarnationLocked)
	}
	if loader.chainCalls != 0 {
		t.Errorf("chainCalls = %d, want 0 (downgrade guard must short-circuit before loader)", loader.chainCalls)
	}
}

func TestIncarnation_Upgrade_MissingIncarnation_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := newUpgradeHandler(db, &fakeLoader{targetSchema: 2})
	req := newChiRequest(http.MethodPost, "/v1/incarnations/ghost/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "ghost")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404", rec.Code)
	}
}

func TestIncarnation_Upgrade_NoToVersion_422(t *testing.T) {
	db := &fakeIncDB{}
	h := newUpgradeHandler(db, &fakeLoader{targetSchema: 2})
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
}

func TestIncarnation_Upgrade_NoLoader_500(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500 (loader not configured)", rec.Code)
	}
}

func TestIncarnation_Upgrade_ChainBroken_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
	}
	loader := &fakeLoader{targetSchema: 3, chainErr: artifact.ErrMigrationChainBroken}
	h := newUpgradeHandler(db, loader)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v3"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
}

func TestIncarnation_Upgrade_LoadFailed_500(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
	}
	loader := &fakeLoader{loadErr: errors.New("git: ref not found")}
	h := newUpgradeHandler(db, loader)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v99"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500", rec.Code)
	}
}

// upgradeSentinelCase — table parameterization of the mapping of sentinel errors from
// incarnation.UpgradeStateSchema to HTTP codes. The sentinel comes from
// upgradeSelectRow (the status gate) or downgrade (target < current schema).
func TestIncarnation_Upgrade_SentinelMapping(t *testing.T) {
	cases := []struct {
		name         string
		upgradeRow   func(string) pgx.Row
		targetSchema int
		toVersion    string
		wantCode     int
		wantType     string
	}{
		{
			name:         "applying→409 (busy)",
			upgradeRow:   func(_ string) pgx.Row { return makeUpgradeSelectRow(1, "applying") },
			targetSchema: 2,
			toVersion:    "v2",
			wantCode:     http.StatusConflict,
			wantType:     problem.TypeIncarnationLocked,
		},
		{
			name:         "error_locked→409",
			upgradeRow:   func(_ string) pgx.Row { return makeUpgradeSelectRow(1, "error_locked") },
			targetSchema: 2,
			toVersion:    "v2",
			wantCode:     http.StatusConflict,
			wantType:     problem.TypeIncarnationLocked,
		},
		{
			name:         "migration_failed→409",
			upgradeRow:   func(_ string) pgx.Row { return makeUpgradeSelectRow(1, "migration_failed") },
			targetSchema: 2,
			toVersion:    "v2",
			wantCode:     http.StatusConflict,
			wantType:     problem.TypeIncarnationLocked,
		},
		{
			// FOR UPDATE sees schema 3, target 2 < 3 → downgrade.
			name:         "downgrade→409",
			upgradeRow:   func(_ string) pgx.Row { return makeUpgradeSelectRow(3, "ready") },
			targetSchema: 2,
			toVersion:    "v2",
			wantCode:     http.StatusConflict,
			wantType:     problem.TypeIncarnationLocked,
		},
		{
			// SelectByName sees schema 1, FOR UPDATE sees 5 (a resolve↔lock race)
			// with an empty chain → schema-mismatch.
			name:         "schema_mismatch→409",
			upgradeRow:   func(_ string) pgx.Row { return makeUpgradeSelectRow(5, "ready") },
			targetSchema: 1,
			toVersion:    "v1-other",
			wantCode:     http.StatusConflict,
			wantType:     problem.TypeIncarnationLocked,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeIncDB{
				selectByNameRow:  func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) },
				upgradeSelectRow: tc.upgradeRow,
			}
			loader := &fakeLoader{targetSchema: tc.targetSchema, chain: statemigrate.Chain{}}
			h := newUpgradeHandler(db, loader)
			req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
				bytes.NewReader([]byte(`{"to_version":"`+tc.toVersion+`"}`)), "name", "redis-prod")
			req = withClaims(req, "archon-alice")
			rec := incUpgrade(h, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("Code = %d, want %d, body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
			var p problem.Details
			_ = json.NewDecoder(rec.Body).Decode(&p)
			if p.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", p.Type, tc.wantType)
			}
		})
	}
}

// chi-routing helper for the unit tests — newChiRequest is defined in
// operator_test.go and reused here directly (one package
// handlers, shared visibility).

// --- CheckDrift -------------------------------------------------------

// fakeDriftChecker — a mock [DriftChecker] (CheckDrift + MarkDriftStatus).
// Records the passed spec, the call count and the MarkDriftStatus arguments; report
// / err — what to return from CheckDrift; markErr — what to return from MarkDriftStatus.
type fakeDriftChecker struct {
	gotSpec      scenario.CheckDriftSpec
	calls        int
	report       *scenario.DriftReport
	err          error
	marked       bool
	markName     string
	markHasDrift bool
	markErr      error
}

func (f *fakeDriftChecker) CheckDrift(_ context.Context, spec scenario.CheckDriftSpec) (*scenario.DriftReport, error) {
	f.calls++
	f.gotSpec = spec
	return f.report, f.err
}

func (f *fakeDriftChecker) MarkDriftStatus(_ context.Context, name string, hasDrift bool) error {
	f.marked = true
	f.markName = name
	f.markHasDrift = hasDrift
	return f.markErr
}

// sampleDriftReportH — a sample report with one drifted host to check the
// response body and the aggregate summary.
func sampleDriftReportH() *scenario.DriftReport {
	return &scenario.DriftReport{
		CheckedAt:       time.Now().UTC(),
		IncarnationName: "redis-prod",
		ScenarioRef:     scenario.ConvergeScenarioName,
		Hosts: []scenario.DriftHostReport{
			{
				SID:    "host-a.example.com",
				Status: scenario.DriftStatusDrifted,
				Tasks: []scenario.DriftTaskResult{
					{Idx: 0, Module: "core.file.present", Changed: true},
				},
			},
		},
		Summary: scenario.DriftSummary{HostsDrifted: 1, HostsClean: 0},
	}
}

func newDriftHandler(db *fakeIncDB, drift *fakeDriftChecker, aw *fakeAuditWriter) *IncarnationHandler {
	return NewIncarnationHandler(db, nil, nil, drift, &fakeResolver{ok: true}, nil, aw, nil, nil)
}

func newDriftRequest(name, aid string, body []byte) *http.Request {
	r := newChiRequest(http.MethodPost, "/v1/incarnations/"+name+"/check-drift",
		bytes.NewReader(body), "name", name)
	return withClaims(r, aid)
}

func TestIncarnation_CheckDrift_Success_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	drift := &fakeDriftChecker{report: sampleDriftReportH()}
	aw := &fakeAuditWriter{}
	h := newDriftHandler(db, drift, aw)

	rec := incCheckDrift(h, newDriftRequest("redis-prod", "archon-alice", []byte(`{}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got scenario.DriftReport
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.IncarnationName != "redis-prod" {
		t.Errorf("incarnation = %q, want redis-prod", got.IncarnationName)
	}
	if got.ScenarioRef != scenario.ConvergeScenarioName {
		t.Errorf("scenario_ref = %q, want converge", got.ScenarioRef)
	}
	if got.Summary.HostsDrifted != 1 {
		t.Errorf("hosts_drifted = %d, want 1", got.Summary.HostsDrifted)
	}
	if len(got.Hosts) != 1 || got.Hosts[0].Status != scenario.DriftStatusDrifted {
		t.Errorf("hosts = %+v, want one drifted host", got.Hosts)
	}

	if drift.calls != 1 {
		t.Errorf("CheckDrift calls = %d, want 1", drift.calls)
	}
	if drift.gotSpec.IncarnationName != "redis-prod" || drift.gotSpec.StartedByAID != "archon-alice" {
		t.Errorf("spec = %+v", drift.gotSpec)
	}
	if len(drift.gotSpec.ApplyID) != 26 {
		t.Errorf("apply_id len = %d, want 26 (ULID)", len(drift.gotSpec.ApplyID))
	}

	// MarkDriftStatus called with hasDrift=true (there is a drifted host) — parity
	// with the MCP handler.
	if !drift.marked {
		t.Fatal("MarkDriftStatus не вызван")
	}
	if drift.markName != "redis-prod" || !drift.markHasDrift {
		t.Errorf("MarkDriftStatus state = (%q, %v), want (redis-prod, true)",
			drift.markName, drift.markHasDrift)
	}

	// Audit-trail: EventIncarnationDriftChecked with correlation_id=apply_id and
	// source=api.
	if !hasEvent(aw, audit.EventIncarnationDriftChecked) {
		t.Fatal("audit: incarnation.drift_checked не записан")
	}
	for _, ev := range aw.events {
		if ev.EventType != audit.EventIncarnationDriftChecked {
			continue
		}
		if ev.Source != audit.SourceAPI {
			t.Errorf("audit source = %q, want api", ev.Source)
		}
		if ev.CorrelationID != drift.gotSpec.ApplyID {
			t.Errorf("audit correlation_id = %q, want %q (apply_id)", ev.CorrelationID, drift.gotSpec.ApplyID)
		}
		if ev.ArchonAID != "archon-alice" {
			t.Errorf("audit archon_aid = %q", ev.ArchonAID)
		}
		summary, _ := ev.Payload["drift_summary"].(map[string]any)
		if summary == nil {
			t.Fatalf("audit drift_summary отсутствует: %+v", ev.Payload)
		}
		if summary["hosts_drifted"] != 1 {
			t.Errorf("audit hosts_drifted = %v, want 1", summary["hosts_drifted"])
		}
	}
}

func TestIncarnation_CheckDrift_ConvergeMissing_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	drift := &fakeDriftChecker{err: scenario.ErrConvergeMissing}
	h := newDriftHandler(db, drift, &fakeAuditWriter{})

	rec := incCheckDrift(h, newDriftRequest("redis-prod", "archon-alice", []byte(`{}`)))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
	if drift.marked {
		t.Error("MarkDriftStatus не должен вызываться при ErrConvergeMissing")
	}
}

func TestIncarnation_CheckDrift_InputMissing_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	drift := &fakeDriftChecker{err: scenario.ErrDriftInputMissing}
	h := newDriftHandler(db, drift, &fakeAuditWriter{})

	rec := incCheckDrift(h, newDriftRequest("redis-prod", "archon-alice", []byte(`{}`)))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
}

func TestIncarnation_CheckDrift_NotConfigured_500(t *testing.T) {
	// drift=nil → the endpoint is not configured, symmetric with Run/Upgrade/Destroy.
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)

	rec := incCheckDrift(h, newDriftRequest("redis-prod", "archon-alice", []byte(`{}`)))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500", rec.Code)
	}
}

func TestIncarnation_CheckDrift_NotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	drift := &fakeDriftChecker{}
	h := newDriftHandler(db, drift, &fakeAuditWriter{})

	rec := incCheckDrift(h, newDriftRequest("ghost", "archon-alice", []byte(`{}`)))

	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404", rec.Code)
	}
	if drift.calls != 0 {
		t.Errorf("CheckDrift calls = %d, want 0 (404 до CheckDrift)", drift.calls)
	}
}

func TestIncarnation_CheckDrift_InvalidName_422(t *testing.T) {
	db := &fakeIncDB{}
	drift := &fakeDriftChecker{}
	h := newDriftHandler(db, drift, &fakeAuditWriter{})

	rec := incCheckDrift(h, newDriftRequest("Bad_Name", "archon-alice", []byte(`{}`)))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
	if drift.calls != 0 {
		t.Errorf("CheckDrift calls = %d, want 0 (validation до CheckDrift)", drift.calls)
	}
}

func TestIncarnation_CheckDrift_NoDrift_MarksReady(t *testing.T) {
	// A clean report (no drifted/failed) → MarkDriftStatus(name, false): the handler
	// resets the incarnation to ready (if it was in drift). Parity with the
	// informational semantics of ADR-031(d).
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	drift := &fakeDriftChecker{report: &scenario.DriftReport{
		CheckedAt:       time.Now().UTC(),
		IncarnationName: "redis-prod",
		ScenarioRef:     scenario.ConvergeScenarioName,
		Hosts: []scenario.DriftHostReport{
			{SID: "host-a.example.com", Status: scenario.DriftStatusClean},
		},
		Summary: scenario.DriftSummary{HostsClean: 1},
	}}
	h := newDriftHandler(db, drift, &fakeAuditWriter{})

	rec := incCheckDrift(h, newDriftRequest("redis-prod", "archon-alice", []byte(`{}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !drift.marked || drift.markHasDrift {
		t.Errorf("MarkDriftStatus state = (marked=%v, hasDrift=%v), want (true, false)",
			drift.marked, drift.markHasDrift)
	}
}

// --- UpdateHosts (PATCH /v1/incarnations/{name}/hosts) -----------------

// makeIncRowWithHosts — a staticRow for SelectByName / FOR UPDATE with given
// hosts in spec.hosts[]. Mirrors [makeIncarnationRow], but gives control over
// the jsonb-spec.
func makeIncRowWithHosts(name, status string, hosts []map[string]any) pgx.Row {
	specMap := map[string]any{}
	if hosts != nil {
		arr := make([]any, 0, len(hosts))
		for _, h := range hosts {
			arr = append(arr, h)
		}
		specMap["hosts"] = arr
	}
	specBytes, _ := json.Marshal(specMap)
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		specBytes, []byte("{}"), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		"create", // created_scenario (migration 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

func TestUpdateHosts_200_Replace(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return makeIncRowWithHosts(name, "ready", []map[string]any{
				{"sid": "old.example", "role": "master"},
			})
		},
		soulsExisting: map[string]struct{}{
			"a.example": {}, "b.example": {},
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[{"sid":"a.example","role":"master"},{"sid":"b.example","role":"replica"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto incDTOJSON
	if err := json.NewDecoder(rec.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Name != "redis-prod" {
		t.Errorf("Name = %q", dto.Name)
	}
	// Spec in the DTO must contain the new hosts.
	hostsRaw, _ := dto.Spec["hosts"].([]any)
	if len(hostsRaw) != 2 {
		t.Errorf("dto.spec.hosts len = %d, want 2 (raw=%v)", len(hostsRaw), dto.Spec["hosts"])
	}
}

func TestUpdateHosts_200_Append(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return makeIncRowWithHosts(name, "ready", []map[string]any{
				{"sid": "a.example", "role": "master"},
			})
		},
		soulsExisting: map[string]struct{}{"b.example": {}},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"append","hosts":[{"sid":"b.example","role":"replica"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto incDTOJSON
	_ = json.NewDecoder(rec.Body).Decode(&dto)
	hostsRaw, _ := dto.Spec["hosts"].([]any)
	if len(hostsRaw) != 2 {
		t.Errorf("dto.spec.hosts len = %d, want 2 (append добавляет к existing)", len(hostsRaw))
	}
}

func TestUpdateHosts_200_Remove(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return makeIncRowWithHosts(name, "ready", []map[string]any{
				{"sid": "a.example", "role": "master"},
				{"sid": "b.example"},
			})
		},
		// For remove, SID existence is still checked.
		soulsExisting: map[string]struct{}{"b.example": {}},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"remove","hosts":[{"sid":"b.example"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto incDTOJSON
	_ = json.NewDecoder(rec.Body).Decode(&dto)
	hostsRaw, _ := dto.Spec["hosts"].([]any)
	if len(hostsRaw) != 1 {
		t.Errorf("dto.spec.hosts len = %d, want 1 (b удалён)", len(hostsRaw))
	}
}

func TestUpdateHosts_UnknownSID_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowWithHosts(name, "ready", nil) },
		// b.example is absent from souls.
		soulsExisting: map[string]struct{}{"a.example": {}},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[{"sid":"a.example"},{"sid":"b.example"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
	if !strings.Contains(p.Detail, "b.example") {
		t.Errorf("Detail = %q, expected missing SID in message", p.Detail)
	}
}

func TestUpdateHosts_DestroyingStatus_409(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return makeIncRowWithHosts(name, "destroying", nil)
		},
		soulsExisting: map[string]struct{}{"a.example": {}},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[{"sid":"a.example"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("Code = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeIncarnationLocked {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeIncarnationLocked)
	}
}

func TestUpdateHosts_NotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/ghost/hosts", body, "name", "ghost")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404", rec.Code)
	}
}

func TestUpdateHosts_InvalidName_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/Bad_Name/hosts", body, "name", "Bad_Name")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

func TestUpdateHosts_InvalidMode_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowWithHosts(name, "ready", nil) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"upsert","hosts":[]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// TestUpdateHosts_Mode_Boundaries — enum replace|append|remove: valid → 200,
// unknown → 422, empty → 422.
func TestUpdateHosts_Mode_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want int
	}{
		{"replace → 200", "replace", http.StatusOK},
		{"append → 200", "append", http.StatusOK},
		{"remove → 200", "remove", http.StatusOK},
		{"unknown → 422", "upsert", http.StatusUnprocessableEntity},
		{"empty → 422", "", http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeIncDB{
				selectByNameRow: func(name string) pgx.Row { return makeIncRowWithHosts(name, "ready", nil) },
				soulsExisting:   map[string]struct{}{"a.example": {}},
			}
			h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
			body, _ := json.Marshal(map[string]any{"mode": c.mode, "hosts": []map[string]any{{"sid": "a.example"}}})
			req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", bytes.NewReader(body), "name", "redis-prod")
			req = withClaims(req, "archon-alice")
			rec := incUpdateHosts(h, req)
			if rec.Code != c.want {
				t.Errorf("mode=%q → code %d, want %d (body=%s)", c.mode, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestUpdateHosts_EmptyReplace_Clears — replace with an empty hosts[] = a deliberate
// clear of the declared-spec (a documented decision, see the UpdateHosts doc-comment).
func TestUpdateHosts_EmptyReplace_Clears(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return makeIncRowWithHosts(name, "ready", []map[string]any{
				{"sid": "a.example", "role": "master"},
			})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200 (replace-clear легитимен), body=%s", rec.Code, rec.Body.String())
	}
	var dto incDTOJSON
	_ = json.NewDecoder(rec.Body).Decode(&dto)
	hostsRaw, _ := dto.Spec["hosts"].([]any)
	if len(hostsRaw) != 0 {
		t.Errorf("dto.spec.hosts len = %d, want 0 (очищено)", len(hostsRaw))
	}
}

// TestUpdateHosts_Role_Boundaries — role kebab-case or empty: empty → ok,
// valid kebab → ok, 63 chars → ok, 64 → 422, uppercase → 422.
func TestUpdateHosts_Role_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		role string
		want int
	}{
		{"empty → ok", "", http.StatusOK},
		{"kebab → ok", "db-master", http.StatusOK},
		{"len 63 → ok", strings.Repeat("a", 63), http.StatusOK},
		{"len 64 → 422", strings.Repeat("a", 64), http.StatusUnprocessableEntity},
		{"uppercase → 422", "Master", http.StatusUnprocessableEntity},
		{"underscore → 422", "db_master", http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeIncDB{
				selectByNameRow: func(name string) pgx.Row { return makeIncRowWithHosts(name, "ready", nil) },
				soulsExisting:   map[string]struct{}{"a.example": {}},
			}
			h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
			host := map[string]any{"sid": "a.example"}
			if c.role != "" {
				host["role"] = c.role
			}
			body, _ := json.Marshal(map[string]any{"mode": "replace", "hosts": []map[string]any{host}})
			req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", bytes.NewReader(body), "name", "redis-prod")
			req = withClaims(req, "archon-alice")
			rec := incUpdateHosts(h, req)
			if rec.Code != c.want {
				t.Errorf("role=%q (len %d) → code %d, want %d (body=%s)", c.role, len(c.role), rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestUpdateHosts_EmptySID_422 — hosts[].sid non-empty: an empty SID → 422.
func TestUpdateHosts_EmptySID_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowWithHosts(name, "ready", nil) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[{"sid":""}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422 (пустой sid)", rec.Code)
	}
}

func TestUpdateHosts_InvalidSID_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowWithHosts(name, "ready", nil) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[{"sid":"BAD_SID"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

func TestUpdateHosts_InvalidRole_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[{"sid":"a.example","role":"Bad_Role"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// TestUpdateHosts_AuditEmitted — on a 200 outcome the handler writes
// `incarnation.hosts_updated` with the correct source / archon / payload.
func TestUpdateHosts_AuditEmitted(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowWithHosts(name, "ready", nil) },
		soulsExisting:   map[string]struct{}{"a.example": {}},
	}
	aw := &recordingAuditWriter{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, aw, nil, nil)
	body := bytes.NewReader([]byte(`{"mode":"replace","hosts":[{"sid":"a.example","role":"master"}]}`))
	req := newChiRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", body, "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpdateHosts(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationHostsUpdated {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventIncarnationHostsUpdated)
	}
	if ev.Source != audit.SourceAPI {
		t.Errorf("Source = %q, want api", ev.Source)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("ArchonAID = %q, want archon-alice", ev.ArchonAID)
	}
	if ev.Payload["mode"] != "replace" {
		t.Errorf("payload.mode = %v", ev.Payload["mode"])
	}
}

// recordingAuditWriter — a simple in-memory audit.Writer for checking the payload.
type recordingAuditWriter struct{ events []*audit.Event }

func (a *recordingAuditWriter) Write(_ context.Context, e *audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

// --- Sync input validation (fix: required-input gap) ------------------

// scenarioCreateRequiredInput — scenario `create` with a required field `name`
// (string, no default) and an optional `replicas` (integer, default 1).
const scenarioCreateRequiredInput = `name: create
create: true
state_changes: {}
input:
  name:
    type: string
    required: true
  replicas:
    type: integer
    default: 1
tasks:
  - name: noop
    module: core.exec.run
    params:
      cmd: echo
    changed_when: "false"
`

// writeCreateScenarioDir writes scenario/create/main.yml (yaml) into a new temp root
// and returns it. Phase 2: ResolveCreateScenarios scans localDir, so the
// create scenario must live on disk. If the yaml does not carry `create: true` —
// we prefix the flag (so it lands in the create set), keeping the string name `create`.
func writeCreateScenarioDir(t *testing.T, yaml string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "scenario", "create")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if !strings.Contains(yaml, "create: true") {
		yaml = "create: true\n" + yaml
	}
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write create/main.yml: %v", err)
	}
	return root
}

// newCreateHandlerWithSchema — a Create handler with runner+services+loader
// (production config), where the loader returns scenario `create` (yaml) from disk.
// Phase 2: the test callers pass create_scenario=create (the choice is required when
// create scenarios are present).
func newCreateHandlerWithSchema(t *testing.T, db *fakeIncDB, yaml string) (*IncarnationHandler, *fakeStarter) {
	t.Helper()
	starter := &fakeStarter{}
	loader := &fakeLoader{localDir: writeCreateScenarioDir(t, yaml)}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	return h, starter
}

// TestIncarnation_Create_RequiredInputMissing_422 — ROOT-CAUSE bug "ba":
// create without a required field is now rejected SYNCHRONOUSLY (422), the incarnation
// row is NOT created, the scenario is NOT launched.
func TestIncarnation_Create_RequiredInputMissing_422(t *testing.T) {
	db := &fakeIncDB{}
	h, starter := newCreateHandlerWithSchema(t, db, scenarioCreateRequiredInput)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create","input":{}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (reject before insert)", db.insertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (scenario not launched)", starter.calls)
	}
}

// TestIncarnation_Create_RequiredInputMissing_NilInput_422 — a missing `input` key
// entirely (nil) yields the same 422 as an empty `{}`.
func TestIncarnation_Create_RequiredInputMissing_NilInput_422(t *testing.T) {
	db := &fakeIncDB{}
	h, starter := newCreateHandlerWithSchema(t, db, scenarioCreateRequiredInput)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insert=%d start=%d, want 0/0", db.insertCalls, starter.calls)
	}
}

// TestIncarnation_Create_RequiredInputProvided_202 — the required field is provided →
// passes, the incarnation is created, the scenario is launched.
func TestIncarnation_Create_RequiredInputProvided_202(t *testing.T) {
	db := &fakeIncDB{}
	h, starter := newCreateHandlerWithSchema(t, db, scenarioCreateRequiredInput)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create","input":{"name":"alice"}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", db.insertCalls)
	}
	if starter.calls != 1 {
		t.Errorf("starter.calls = %d, want 1", starter.calls)
	}
}

// TestIncarnation_Create_TypeMismatch_422 — required is provided, but an optional
// field of the wrong type → 422 before mutation.
func TestIncarnation_Create_TypeMismatch_422(t *testing.T) {
	db := &fakeIncDB{}
	h, _ := newCreateHandlerWithSchema(t, db, scenarioCreateRequiredInput)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create","input":{"name":"alice","replicas":"x"}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", db.insertCalls)
	}
}

// TestIncarnation_Create_NoSchema_202 — scenario `create` without an `input:` block:
// any input passes (like a service with no required fields).
func TestIncarnation_Create_NoSchema_202(t *testing.T) {
	db := &fakeIncDB{}
	h, _ := newCreateHandlerWithSchema(t, db, "name: create\nstate_changes: {}\ntasks: []\n")
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", db.insertCalls)
	}
}

// boolPtr — a helper for *bool lifecycle flags in tests.
func boolPtr(b bool) *bool { return &b }

// TestIncarnation_Create_AutoCreateFalse_NoRun — lifecycle.auto_create=false:
// the incarnation is created (insert), BUT scenario `create` is NOT launched; the response is 202
// WITHOUT apply_id (omitted), status stays ready.
func TestIncarnation_Create_AutoCreateFalse_NoRun(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	loader := &fakeLoader{
		localDir:  writeCreateScenarioDir(t, "name: create\nstate_changes: {}\ntasks: []\n"),
		lifecycle: &config.LifecycleConfig{AutoCreate: boolPtr(false)},
	}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)

	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create"}`))), "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1 (инкарнация создаётся)", db.insertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (auto_create=false → прогона нет)", starter.calls)
	}
	// created_scenario at auto_create=false is NOT NULL: the bootstrap scenario exists (create),
	// the run is merely deferred (unlike bare). $12 = create.
	if got, _ := db.insertArgs[11].(string); got != "create" {
		t.Errorf("INSERT created_scenario ($12) = %q, want create (auto_create=false ≠ bare)", got)
	}
	// apply_id is absent from the JSON (nullable, omitempty).
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, has := raw["apply_id"]; has {
		t.Errorf("apply_id присутствует при auto_create=false: %v", raw)
	}
	if raw["incarnation"] != "ba" {
		t.Errorf("incarnation = %v", raw["incarnation"])
	}
}

// TestIncarnation_Create_AutoCreateTrueExplicit_Run — lifecycle.auto_create=true
// (explicit) → the run starts, apply_id is present (parity with default).
func TestIncarnation_Create_AutoCreateTrueExplicit_Run(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	loader := &fakeLoader{
		localDir:  writeCreateScenarioDir(t, "name: create\nstate_changes: {}\ntasks: []\n"),
		lifecycle: &config.LifecycleConfig{AutoCreate: boolPtr(true)},
	}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)

	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create"}`))), "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Errorf("starter.calls = %d, want 1 (auto_create=true)", starter.calls)
	}
	var raw map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&raw)
	if _, has := raw["apply_id"]; !has {
		t.Errorf("apply_id отсутствует при auto_create=true: %v", raw)
	}
}

// TestIncarnation_Create_NoLifecycleBlock_Run — a manifest without a lifecycle block
// (nil) → backcompat: the run starts (auto_create defaults to true).
func TestIncarnation_Create_NoLifecycleBlock_Run(t *testing.T) {
	db := &fakeIncDB{}
	// lifecycle=nil in fakeLoader (a manifest without the block) → auto_create defaults to true.
	h, starter := newCreateHandlerWithSchema(t, db, "name: create\nstate_changes: {}\ntasks: []\n")
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"ba","service":"redis","create_scenario":"create"}`))), "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Errorf("starter.calls = %d, want 1 (backcompat: lifecycle nil → auto_create true)", starter.calls)
	}
}

// newRunHandlerWithSchema — a Run handler with a loader that returns a scenario with
// required-input (production Run config).
func newRunHandlerWithSchema(db *fakeIncDB, yaml string) (*IncarnationHandler, *fakeStarter) {
	starter := &fakeStarter{}
	loader := &fakeLoader{scenarioYAML: yaml}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	return h, starter
}

// TestIncarnation_Run_RequiredInputMissing_422 — a scenario run without a required
// field is rejected sync (422), the run does NOT start.
func TestIncarnation_Run_RequiredInputMissing_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	h, starter := newRunHandlerWithSchema(db, scenarioCreateRequiredInput)
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/ba/scenarios/create",
		bytes.NewReader([]byte(`{"input":{}}`)),
		"ba", "create")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (run not launched)", starter.calls)
	}
}

// TestIncarnation_Run_RequiredInputProvided_202 — required is provided → the run
// starts.
func TestIncarnation_Run_RequiredInputProvided_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncStatusRow(name, "ready") },
	}
	h, starter := newRunHandlerWithSchema(db, scenarioCreateRequiredInput)
	req := newChiRequestScenario(http.MethodPost,
		"/v1/incarnations/ba/scenarios/create",
		bytes.NewReader([]byte(`{"input":{"name":"alice"}}`)),
		"ba", "create")
	req = withClaims(req, "archon-alice")
	rec := incRun(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Errorf("starter.calls = %d, want 1", starter.calls)
	}
}

// --- Golden wire-guard (oapi migration, byte-for-byte) -------------------

// TestIncarnationGetReply_GoldenNullFields — the wire invariant after migrating to
// generated oapi types: nullable+required fields (created_by_aid / state /
// spec / status_details) at nil MUST be present in the JSON with the value
// `null` (NOT omitted). Catches a regression if omitempty comes back on any of
// them (the key would then disappear and the contract would break — the client/UI expects null).
func TestIncarnationGetReply_GoldenNullFields(t *testing.T) {
	inc := &incarnation.Incarnation{
		Name:               "redis-prod",
		Service:            "redis",
		ServiceVersion:     "v1",
		StateSchemaVersion: 1,
		Status:             incarnation.StatusReady,
		// Spec / State / StatusDetails / CreatedByAID — all nil.
		CreatedAt: time.Unix(0, 0).UTC(),
		UpdatedAt: time.Unix(0, 0).UTC(),
	}
	b, err := json.Marshal(shimGetReplyJSON(toIncarnationGetView(inc, nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"created_by_aid":null`,
		`"state":null`,
		`"spec":null`,
		`"status_details":null`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wire НЕ содержит %s (omitempty-регресс?)\nwire: %s", want, got)
		}
	}
}

// TestIncarnationGetReply_DriftSummaryTyped — the wire invariant for typed
// last_drift_summary (moving away from opaque passthrough): a populated column goes onto
// the wire as a typed object with counts keys (integer, NOT float strings) and
// scanned_at in RFC3339Nano. Catches a regression back to map-passthrough or
// loss/rename of DriftScanSummary fields.
func TestIncarnationGetReply_DriftSummaryTyped(t *testing.T) {
	scannedAt := time.Date(2026, 5, 26, 12, 0, 0, 123456789, time.UTC)
	inc := &incarnation.Incarnation{
		Name:               "redis-prod",
		Service:            "redis",
		ServiceVersion:     "v1",
		StateSchemaVersion: 1,
		Status:             incarnation.StatusReady,
		CreatedAt:          time.Unix(0, 0).UTC(),
		UpdatedAt:          time.Unix(0, 0).UTC(),
		LastDriftCheckAt:   &scannedAt,
		LastDriftSummary: &incarnation.DriftScanSummary{
			HostsDrifted: 1, HostsClean: 2, HostsUnsupported: 0, HostsFailed: 0,
			TotalHosts: 3, ScannedAt: scannedAt,
		},
	}
	b, err := json.Marshal(shimGetReplyJSON(toIncarnationGetView(inc, nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"hosts_drifted":1`,
		`"hosts_clean":2`,
		`"hosts_unsupported":0`,
		`"hosts_failed":0`,
		`"total_hosts":3`,
		`"scanned_at":"2026-05-26T12:00:00.123456789Z"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wire НЕ содержит %s (typed-drift-регресс?)\nwire: %s", want, got)
		}
	}
	// Round-trip through the wire form — counts/scanned_at read back typed without loss.
	var reply struct {
		LastDriftSummary *struct {
			HostsDrifted int       `json:"hosts_drifted"`
			TotalHosts   int       `json:"total_hosts"`
			ScannedAt    time.Time `json:"scanned_at"`
		} `json:"last_drift_summary"`
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if reply.LastDriftSummary == nil {
		t.Fatalf("LastDriftSummary опущен на wire при заполненной колонке")
	}
	if reply.LastDriftSummary.HostsDrifted != 1 || reply.LastDriftSummary.TotalHosts != 3 {
		t.Errorf("round-trip counts разъехались: %+v", *reply.LastDriftSummary)
	}
	if !reply.LastDriftSummary.ScannedAt.Equal(scannedAt) {
		t.Errorf("round-trip scanned_at = %v, want %v", reply.LastDriftSummary.ScannedAt, scannedAt)
	}
}

// TestIncarnationGetReply_DriftSummaryOmittedWhenNil — a NULL column
// (the incarnation was never scanned): the last_drift_summary key is ABSENT
// on the wire (omit, not null) — the former omit semantics preserved after typing.
func TestIncarnationGetReply_DriftSummaryOmittedWhenNil(t *testing.T) {
	inc := &incarnation.Incarnation{
		Name:               "redis-prod",
		Service:            "redis",
		ServiceVersion:     "v1",
		StateSchemaVersion: 1,
		Status:             incarnation.StatusReady,
		CreatedAt:          time.Unix(0, 0).UTC(),
		UpdatedAt:          time.Unix(0, 0).UTC(),
		// LastDriftSummary / LastDriftCheckAt — nil.
	}
	b, err := json.Marshal(shimGetReplyJSON(toIncarnationGetView(inc, nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); strings.Contains(got, "last_drift_summary") {
		t.Errorf("wire содержит last_drift_summary при NULL-колонке (должен быть опущен)\nwire: %s", got)
	}
}

// TestStateHistoryEntry_GoldenChangedByAIDOmitted — the symmetric invariant for
// changed_by_aid: at an empty (nil) value the key MUST be ABSENT (omit, not
// null). The current wire = omit when empty; a regression to nullable-without-omitempty
// would add `"changed_by_aid":null` and break byte-for-byte.
func TestStateHistoryEntry_GoldenChangedByAIDOmitted(t *testing.T) {
	e := &incarnation.HistoryEntry{
		HistoryID: "01HX",
		Scenario:  "rotate",
		// StateBefore / StateAfter — nil (present as null), ChangedByAID — nil.
		ApplyID: "01HABCDEFGHJKMNPQRSTVWXYZ0",
		At:      time.Unix(0, 0).UTC(),
	}
	b, err := json.Marshal(shimHistoryEntryJSON(toStateHistoryView(e, nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "changed_by_aid") {
		t.Errorf("wire содержит changed_by_aid при пустом значении (должен быть опущен)\nwire: %s", got)
	}
	// state_before/state_after — conversely, PRESENT as null (required+nullable).
	for _, want := range []string{`"state_before":null`, `"state_after":null`} {
		if !strings.Contains(got, want) {
			t.Errorf("wire НЕ содержит %s (omitempty-регресс?)\nwire: %s", want, got)
		}
	}
}
