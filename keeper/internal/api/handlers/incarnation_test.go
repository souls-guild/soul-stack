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

// fakeIncDB — мок [IncarnationDB] (ExecQueryRower + TxBeginner). Минимальный
// set, нужный endpoint-ам Create / Get / List / History / Run / Unlock.
type fakeIncDB struct {
	// Create-path
	insertRow   func() pgx.Row
	insertCalls int
	// insertArgs — последние аргументы INSERT INTO incarnation (spec=$5, traits=$11)
	// для проверки прокидки spec.traits на create-пути (ADR-060 amend R1).
	insertArgs []any
	// updateTraitsArg — jsonb-арг $2 UPDATE incarnation SET traits (PUT .../traits,
	// ADR-060 amend R1): целостная замена incarnation.traits.
	updateTraitsArg []byte

	// Get/History/Run existence-probe + Unlock SELECT FOR UPDATE
	selectByNameRow func(name string) pgx.Row

	// List
	countRow func(sql string) pgx.Row
	listRows func() (pgx.Rows, error)
	// captureListSQL — hook на текст list-SELECT (ORDER BY pushdown-проверка).
	captureListSQL func(sql string)
	// lastCountArgs / lastListArgs — bind-args COUNT/SELECT list-запросов
	// (scope-pushdown проверка S3b-3). listCalled — был ли вызван SelectAll
	// (fail-closed: при пустом scope SelectAll не должен вызываться).
	lastCountArgs []any
	lastListArgs  []any
	listCalled    bool

	// Unlock-path: SELECT FOR UPDATE (state, status) + Exec-учёт.
	unlockSelectRow func(name string) pgx.Row
	execCalls       []string

	// Rerun-last-path: last-run probe в UnlockForRerun
	// `SELECT scenario, apply_id FROM state_history ... ORDER BY history_id DESC LIMIT 1`.
	// nil → дефолт [create, <applyID>] (create-путь: последний упавший = create).
	lastScenarioRow func(name string) pgx.Row
	// Rerun-last (операционный путь): recipe probe `SELECT recipe FROM apply_runs WHERE
	// apply_id = $1 AND recipe IS NOT NULL LIMIT 1`. nil → ErrNoRows (fail-closed:
	// recipe недоступен). Задать для happy-path.
	recipeRow func(applyID string) pgx.Row

	// Upgrade-path: SELECT FOR UPDATE (state, state_schema_version, status).
	upgradeSelectRow func(name string) pgx.Row

	// Destroy-path: RowsAffected для single-winner `DELETE FROM incarnation`
	// (DeleteAfterTeardown). zero-value → "DELETE 1" (строка снесена); задать
	// "DELETE 0" для no-op-гонки. archive-INSERT-ы возвращают пустой тег.
	deleteTag pgconn.CommandTag

	// UpdateHosts-path: SELECT FROM souls WHERE sid = ANY($1) — набор SID-ов,
	// которые «существуют» в реестре `souls`. nil → ни одного (для теста
	// UnknownSID). UpdateHosts SQL `UPDATE incarnation SET spec = ...` ловится
	// общим Exec — фиксирует факт записи в execCalls.
	soulsExisting map[string]struct{}

	// Runs read-view (GET .../runs[/{apply_id}]): count-строка списка прогонов
	// (COUNT(DISTINCT apply_id) FROM apply_runs) и rows list/detail (SELECT ...
	// FROM apply_runs). nil → пустой список / 0 (для scope-gate/empty-тестов).
	applyRunsCountRow func(sql string) pgx.Row
	applyRunsRows     func() (pgx.Rows, error)

	// Run-tasks read-view (GET .../runs/{apply_id}/tasks, NIM-37): EXISTS-probe
	// принадлежности прогона инкарнации (runExistsRow, nil → true = «принадлежит»)
	// и строки плана apply_run_plan (runPlanRows, nil → пустой план).
	runExistsRow func(applyID, name string) pgx.Row
	runPlanRows  func() (pgx.Rows, error)

	// Global runs read-view (GET /v1/runs[/stats]): COUNT(*) по свёртке apply_runs.
	// runsCalled/lastRunsSQL/lastRunsArgs — факт и содержимое count-запроса
	// (fail-closed- и scope-pushdown-проверки). nil runsCountRow → 0.
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
		// Дефолт: последний упавший = create (create-путь), apply_id — заглушка.
		return staticRow{values: []any{"create", "01HFAILEDRUN00000000000000"}}
	}
	if strings.Contains(sql, "FROM apply_runs") && strings.Contains(sql, "recipe IS NOT NULL") {
		if f.recipeRow != nil {
			return f.recipeRow(args[0].(string))
		}
		return errRow{err: pgx.ErrNoRows}
	}
	// UpdateHosts: UPDATE incarnation SET spec = ... RETURNING updated_at.
	// Этот UPDATE-with-RETURNING приходит ДО общего match-а "WHERE name = $1"
	// (тот же предикат стоит и здесь), поэтому обрабатывается отдельной веткой
	// и возвращает свежий timestamp на Scan(*time.Time). UpdateTraits (SET traits)
	// — тот же RETURNING updated_at, фиксируем его jsonb-арг $2 в updateTraitsArg.
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
	// Run-tasks scope-probe: EXISTS(SELECT 1 FROM apply_runs ...) — принадлежит ли
	// прогон инкарнации (RunExistsForIncarnation, NIM-37). nil hook → true.
	if strings.Contains(sql, "EXISTS(SELECT 1") && strings.Contains(sql, "FROM apply_runs") {
		if f.runExistsRow != nil {
			return f.runExistsRow(args[0].(string), args[1].(string))
		}
		return staticRow{values: []any{true}}
	}
	// Runs read-view (GET .../runs): COUNT(DISTINCT apply_id) FROM apply_runs.
	// applyRunsCountRow контролирует значение; nil → 0 (пустой список прогонов).
	if strings.Contains(sql, "COUNT(DISTINCT apply_id) FROM apply_runs") {
		if f.applyRunsCountRow != nil {
			return f.applyRunsCountRow(sql)
		}
		return staticRow{values: []any{int(0)}}
	}
	// Global runs read-view (GET /v1/runs): COUNT(*) по свёртке apply_runs.
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
	// UpdateHosts: SELECT sid FROM souls WHERE sid = ANY($1) — отдаём
	// существующие SID-ы из soulsExisting (контролирует тест).
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
	// Run-tasks: SELECT ... FROM apply_run_plan (план задач прогона, NIM-37). Стоит
	// ПЕРЕД apply_runs-веткой: "FROM apply_run_plan" не содержит "FROM apply_runs",
	// но держим порядок явным. nil hook → пустой план.
	if strings.Contains(sql, "FROM apply_run_plan") {
		if f.runPlanRows != nil {
			return f.runPlanRows()
		}
		return &emptyRows{}, nil
	}
	// Runs read-view: SELECT ... FROM apply_runs (list прогонов / detail per-host).
	// Отдельный hook от listRows (тот — incarnation-list), nil → пустой набор.
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

// BeginTx возвращает fakeIncTx, проксирующую обратно в fakeIncDB. Tx-методы
// Commit/Rollback — no-op (unit-тест не проверяет транзакционную семантику
// PG, только маршрут handler → CRUD).
func (f *fakeIncDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeIncTx{db: f}, nil
}

// fakeIncTx — pgx.Tx-обёртка над fakeIncDB. Делегирует Exec/Query/QueryRow;
// Commit/Rollback — no-op; прочие методы pgx.Tx panic-ают при обращении
// (Unlock их не использует).
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

// emptyRows — pgx.Rows-stub без значений.
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

// makeIncarnationRow конструирует pgx.Row-stub под SelectByName с
// преднастроенными полями. Используется для теста Get/History
// existence-probe.
func makeIncarnationRow(name string) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits (ADR-060 amend R1)
		any(nil), []byte(nil), // last_drift_check_at, last_drift_summary (ADR-031 Slice C)
		"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
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
	// Декодируем в map, чтобы проверить ОТСУТСТВИЕ поля `status` в JSON
	// (createIncarnationResponse его не объявляет, но проверка через
	// raw-map ловит регресс, если поле случайно вернут).
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

// TestIncarnation_Create_Covens_Accepted — covens в теле принимается (не
// unknown-field strict-декодером) и доходит до insert. Прокидывание covens до
// INSERT-арга $10 покрыто domain-тестом TestCreate_CovensPassedThrough.
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

// --- Secret masking on GET output (вариант D) -------------------------

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

	// Хранимый incarnation не мутирован — маскируется только ответ.
	if inc.State["admin_token"] != pwd {
		t.Errorf("исходный inc.State мутирован: %v", inc.State["admin_token"])
	}
	if inc.Spec["input"].(map[string]any)["db_password"] != pwd {
		t.Errorf("исходный inc.Spec мутирован")
	}
}

// TestToIncarnationGetView_ProjectsTraitsAndCreatedScenario — handler-проекция
// читает traits (ADR-060 operator-set метки) и created_scenario (механизм нескольких
// create) из доменной incarnation-строки. Источник bug-а: оба поля не отдавались в
// GET → UI traits-modal открывался без prefill, оператор не видел стартовый сценарий.
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

	// Пустые домен-значения проходят как есть (omitempty опускает их в wire-проекции).
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
	// Хранимый snapshot не мутирован.
	if e.StateBefore["admin_token"] != "old" {
		t.Errorf("исходный StateBefore мутирован")
	}
}

func TestIncarnation_Get_200_StateMasked(t *testing.T) {
	// End-to-end через handler: state с секретом → в JSON-ответе замаскирован,
	// несекретное поле — как есть.
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
				"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
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

// TestIncarnation_List_CovenFilter_PassesToSQL — coven query-param доходит
// до SQL-арга (а не отсекается на client-side). Аргументы COUNT(*) — это
// готовый args[] для filter.Coven (см. buildListWhere → `$1 = ANY(covens)`).
func TestIncarnation_List_CovenFilter_PassesToSQL(t *testing.T) {
	var capturedArgs []any
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows: func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
	// Перехватываем countRow с аргументами — у тестового fake нет hook-а для
	// args[]; матчер по SQL содержит COUNT(*) FROM incarnation, а сам args
	// доходит как параметр в QueryRow. Подменяем countRow на closure с
	// побочным эффектом.
	db.countRow = func(sql string) pgx.Row {
		// SQL содержит WHERE-предикат — но самих args здесь нет. Достаточно
		// проверить, что SQL включает $1 = ANY(covens) (фильтр сработал).
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

// TestIncarnation_List_InvalidCoven_422 — невалидная coven-метка отсекается
// до SQL (kebab-case-формат).
func TestIncarnation_List_InvalidCoven_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?coven=DEV_UPPER", nil)
	rec := incList(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// listSQLCapture перехватывает SQL списка (Query) и COUNT (countRow) —
// нужно убедиться, что query-param долетел до WHERE/ORDER BY pushdown.
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

// TestIncarnation_List_StateFilter_PassesToSQL — query `state.redis_version=8.0`
// долетает до jsonb-pushdown (->>) в COUNT-SQL.
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

// TestIncarnation_List_StateFilter_NumericOp — query `state.memory_mb=gt:1000`
// разбирается в числовое сравнение (->>)::numeric.
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

// TestIncarnation_List_StateFilter_InjectionPath_422 — спецсимволы/SQL в
// state-path отбиваются форматной валидацией ([a-z0-9_]) до запроса в БД.
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

// TestIncarnation_List_StateFilter_NumericOp_NonNumericValue_422 — нечисловое
// значение при числовом операторе (`state.memory_mb=gt:abc`) → 422
// (опечатка оператора), а не 500 от PG cast-ошибки 22P02.
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

// TestIncarnation_List_SortStateField_PassesToSQL — sort=state.<field> уходит
// в jsonb ORDER BY (перехват list-SQL через captureListSQL).
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

// TestIncarnation_List_BadSortField_422 — неизвестное sort-поле → 422.
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

// TestIncarnation_List_BadSortDir_422 — неизвестное направление → 422.
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
	// Existence-probe не находит → 404 без захода в HistorySelectByName.
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
	// Валидный ULID-фильтр → 200, existence-probe + COUNT + SELECT.
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
	// Не-ULID: lowercase, неверная длина.
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
	// Пустой `apply_id` (e.g. `?apply_id=`) — фильтр игнорируется,
	// поведение как без query-param (backward-compat).
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

// Capture-fake — перехватывает args INSERT-а, чтобы тест видел, какой spec
// ушёл в БД. Имитирует только INSERT-path (Create); остальные SQL-сценарии
// падают в errRow.
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
	// Body без `input` — spec должен пойти в БД как `{}`, не `{"input": null}`.
	db := &captureInsertDB{fakeIncDB: &fakeIncDB{}}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	// args[4] (1-based $5) — specBytes. См. insertSQL в crud.go.
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
	// COUNT возвращает 7, listRows — пусто (имитация offset=100 при total=7).
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

// --- List/Get scoped-видимость (ADR-047 S3b-3) ------------------------

// incRows — pgx.Rows-stub, прогоняющий staticRow за staticRow (multi-row list-
// результат для scanIncarnation). Аналог incarnation-пакетного fakeRows; в
// handlers-тестах был только emptyRows/stringRows.
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

// incListRow конструирует staticRow под SelectAll-list (15 колонок порядка
// scanIncarnation) с заданными name/covens/state.
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
		"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// doIncList выполняет List под scoper с claims=archon-alice.
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

// scopeArgHas — есть ли среди bind-args []string-аргумент, равный want (порядок
// учитывается; scope-covens/state-names биндятся как []string).
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

// TestIncarnation_List_EmptyPurview_FailClosed — ГЛАВНЫЙ security-инвариант
// (ADR-047): оператор с пустым Purview (default-deny, нет coven/state измерений)
// видит ПУСТОЙ список, НЕ все incarnation. fakeIncDB отдал бы строки — handler
// обязан вернуть 0 и НЕ обратиться к SelectAll. Регресс = оператор видит чужие
// incarnation.
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

// TestIncarnation_List_NoClaims_FailClosed — нет claims (защитный инвариант,
// route под RequireJWT) → пустой список.
func TestIncarnation_List_NoClaims_FailClosed(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(3)}} },
		listRows: func() (pgx.Rows, error) {
			return &incRows{rows: []staticRow{incListRow("secret-inc", nil, nil)}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, unrestrictedScoper(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations", nil) // БЕЗ claims
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

// incListRowBare — как incListRow, но created_scenario = NULL (bare-инкарнация,
// миграция 090). scanIncarnation проецирует 16-ю колонку в **string → nil.
func incListRowBare(name string) staticRow {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		any(nil), // created_scenario = NULL (bare, миграция 090)
		any(nil), // applying_apply_id (ADR-068 §A1, bare → NULL)
	}}
}

// TestIncarnation_List_BareIncarnation_NoPanic — GUARD Фаза 2 (handler-level,
// реальная NULL-проекция scanIncarnation в list-пути): list со строкой bare-
// инкарнации (created_scenario IS NULL) → 200, без паники, элемент доезжает.
// scanIncarnation читает 16-ю колонку в **string → nil; регресс = NULL ломает
// list-проекцию (паника/ошибка scan). omitempty-семантику created_scenario на
// реальной NULL-строке проверяет TestHumaIncarnation_Get_BareIncarnation_OmitsCreatedScenario
// (huma-reply несёт это поле; тест-шим list его не проецирует — на нём omitempty
// тривиально и неинформативно).
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

// TestIncarnation_List_NilScoper_FailClosed — scoper не сконфигурирован → пустой
// список (защита от мис-wire-up-а, не раскрывать все incarnation).
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

// TestIncarnation_List_Unrestricted_All — `*`/bare-без-default Purview → весь
// список без scope-предиката (scope-args в SQL не добавляются).
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
	// Unrestricted → НЕ должно быть scope-args ([]string) в COUNT.
	for _, a := range db.lastCountArgs {
		if _, ok := a.([]string); ok {
			t.Errorf("unrestricted scope добавил scope-args в SQL: %v", db.lastCountArgs)
		}
	}
}

// TestIncarnation_List_CovenScope_ReachesSQL — coven-scoped оператор: covens
// доходят до SQL как []string-аргумент coven∪{name}-pushdown-а.
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
	// coven∪{name}: SQL обязан содержать оба плеча (covens && + name = ANY).
	if !strings.Contains(listSQL, "covens &&") || !strings.Contains(listSQL, "name = ANY") {
		t.Errorf("coven∪{name} SQL неполон (нужны covens && И name = ANY): %q", listSQL)
	}
}

// TestIncarnation_List_StateScope_ResolvedNamesReachSQL — state-измерение Purview
// (StateExprs): handler предрезолвит имена incarnation-ов через statepredicate и
// прокинет их как `name = ANY($n)`-pushdown. fakeIncDB: state-резолв-pass (page-
// lister) находит redis-a (redis_version==8.0) → его имя приходит в scope.
func TestIncarnation_List_StateScope_ResolvedNamesReachSQL(t *testing.T) {
	// Один fakeIncDB обслуживает И state-резолв (page-lister SelectAll), И
	// финальный list-SelectAll. Оба идут через Query/QueryRow по тем же SQL-
	// сигнатурам; state-lister отдаёт реальные строки (для CEL-eval), финальный
	// list — пусто (нам важны лишь scope-args).
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
			// Первый Query — page-lister state-резолва: отдаём строки для CEL.
			return &incRows{rows: resolveRows}, nil
		}
		// Последующие — финальный list: пусто (нас интересуют scope-args).
		return &emptyRows{}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Имя redis-a (state-matched) должно прийти в финальный list как scope-арг.
	if !scopeArgHas(db.lastCountArgs, []string{"redis-a"}) {
		t.Errorf("state-резолв: имя redis-a не дошло до финального list-SQL как scope-name: %v", db.lastCountArgs)
	}
}

// TestIncarnation_List_OR_CovenAndState_Union — OR измерений: coven ∪ state =
// union. coven=prod + state redis8 → в финальный list уходят И scope-covens [prod],
// И state-резолвнутые имена. Оба плеча присутствуют в args.
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

// TestIncarnation_Get_EmptyPurview_404 — fail-closed: пустой Purview → 404 (не
// палим существование чужой incarnation), хотя строка в БД есть.
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

// TestIncarnation_Get_NilScoper_404 — nil-scoper → 404 (fail-closed мис-wire-up).
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

// TestIncarnation_Get_CovenMatch_200 — incarnation в scope-ковене (по covens[]) →
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

// TestIncarnation_Get_NameMatch_200 — coven∪{name}: incarnation БЕЗ совпадающих
// covens[], но её ИМЯ = scope-coven (ADR-008 корневая метка) → 200. Регресс =
// оператор со scope coven=redis-prod не видит incarnation redis-prod.
func TestIncarnation_Get_NameMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incListRow(name, []string{"other-coven"}, nil) // covens НЕ содержат redis-prod
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"redis-prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (coven∪{name}: name=redis-prod матчит scope coven=redis-prod)", rec.Code)
	}
}

// TestIncarnation_Get_CovenMismatch_404 — incarnation вне scope-ковенов и имя не
// совпадает → 404.
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

// TestIncarnation_Get_StateMatch_200 — state-измерение: incarnation, чей state
// удовлетворяет StateExpr scope → 200 (без coven-совпадения).
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

// TestIncarnation_Get_StateMismatch_404 — state не удовлетворяет ни одному
// StateExpr и нет coven-совпадения → 404.
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

// doIncHistory — handler-direct History с claims (зеркало doIncGet). Возвращает
// recorder; db должна отдать selectByNameRow (existence-probe + scope), count/list.
func doIncHistory(t *testing.T, h *IncarnationHandler, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := withClaims(newChiRequest(http.MethodGet, "/v1/incarnations/"+name+"/history", nil, "name", name), "archon-alice")
	rec := incHistory(h, req)
	return rec
}

// fakeIncHistoryDB — fakeIncDB для History scope-тестов: existence-probe несёт
// covens/state (через incListRow), COUNT=0 + пустой list (scope-ветка проверяется
// до раскрытия timeline, наполнять историю незачем).
func fakeIncHistoryDB(name string, covens []string, state map[string]any) *fakeIncDB {
	row := incListRow(name, covens, state)
	return &fakeIncDB{
		selectByNameRow: func(string) pgx.Row { return row },
		countRow:        func(string) pgx.Row { return staticRow{values: []any{int(0)}} },
		listRows:        func() (pgx.Rows, error) { return &emptyRows{}, nil },
	}
}

// TestIncarnation_History_StateMatch_200 — History gate переведён на existence-
// only RequireAction (ADR-047 §г): state-scoped оператор достигает handler-а,
// сужение через getInScope("history"). state матчит StateExpr → 200. Регресс
// (до Фикс 2) = state-scoped оператор ловил 403 на route-gate Multi (state-
// измерение не резолвится в incarnation-context → deny ДО handler-а).
func TestIncarnation_History_StateMatch_200(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"staging"}, map[string]any{"redis_version": "8.0"})
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (state-scoped видит историю своей incarnation)", rec.Code)
	}
}

// TestIncarnation_History_StateMismatch_404 — state не матчит ни StateExpr, нет
// coven-совпадения → 404 (история чужой incarnation не раскрывается).
func TestIncarnation_History_StateMismatch_404(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"staging"}, map[string]any{"redis_version": "7.2"})
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{stateExprs: []string{`state.redis_version == "8.0"`}}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (state не матчит — история скрыта)", rec.Code)
	}
}

// TestIncarnation_History_CovenMatch_200 — coven-scoped оператор видит историю
// incarnation своего coven (паритет Get; coven-scope раньше матчил и через Multi-
// gate, теперь — через getInScope("history")).
func TestIncarnation_History_CovenMatch_200(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"prod"}, nil)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (coven-match)", rec.Code)
	}
}

// TestIncarnation_History_EmptyPurview_404 — fail-closed: пустой Purview → 404
// (история существующей-но-чужой incarnation не раскрывается).
func TestIncarnation_History_EmptyPurview_404(t *testing.T) {
	db := fakeIncHistoryDB("redis-prod", []string{"prod"}, nil)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, fakeIncScoper{empty: true}, nil)
	rec := doIncHistory(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (empty-purview fail-closed)", rec.Code)
	}
}

// --- RBAC scope selectors (ADR-008 amendment a) -----------------------

// hasCovenCtx — есть ли в наборе контекст с заданным coven (+ service, если
// задан); incarnation-ключ проверяется на совпадение с name.
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
	// covens ∪ {name} = {prod, dc1, redis-prod}, service во всех.
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
	// name уже в covens → не дублируется.
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
		// service=redis, covens=[prod] (см. makeIncStatusRow service=redis;
		// дополняем covens вручную).
		now := time.Now()
		return staticRow{values: []any{
			name, "redis", "v1", int(1),
			[]byte("{}"), []byte("{}"), "ready",
			[]byte(nil), any(nil),
			now, now, []string{"prod"},
			[]byte("{}"),          // traits
			any(nil), []byte(nil), // ADR-031 Slice C
			"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
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
	// Тело восстановлено для handler-а.
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

// fakeStarter — мок [ScenarioStarter]. Перехватывает RunSpec и возвращает
// заданную ошибку (nil → успех).
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

// fakeResolver — мок [ServiceResolver]. ok=false → сервис не зарегистрирован.
type fakeResolver struct {
	ok bool
}

func (f *fakeResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{Name: service, Ref: "v1"}, f.ok
}

// fakeIncScoper — мок [PurviewResolver] для scoped List/Get-тестов (ADR-047
// S3b-3). Поля мапятся в [rbac.Purview]: covens → Covens, stateExprs → StateExprs,
// traitExprs → TraitExprs (ADR-060 п.7 slice 1, `key:value`-пары).
// empty=true → Purview{} (fail-closed). Симметрично soul_test.fakeScoper.
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

// unrestrictedScoper — типовой Unrestricted-scoper для существующих List/Get-
// тестов, которые проверяют не scope, а filter/sort SQL-путь (scope снят →
// поведение без изменений). Помогает не плодить литерал в каждом тесте.
func unrestrictedScoper() fakeIncScoper { return fakeIncScoper{unrestricted: true} }

// newChiRequestScenario строит request с двумя chi-URL-params (name +
// scenario) для Run-handler-а — он читает оба через chi.URLParam.
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

// makeIncStatusRow конструирует staticRow под SelectByName с заданным
// статусом (для Run-probe error_locked).
func makeIncStatusRow(name, status string) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits
		any(nil), []byte(nil), // ADR-031 Slice C
		"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// makeUnlockSelectRow конструирует staticRow под FOR UPDATE-select unlock-семейства.
// Несёт 4 колонки (state, status, created_scenario, spec): plain Unlock / Destroy
// сканируют первые две, UnlockForRerun — все четыре.
func makeUnlockSelectRow(status string) pgx.Row {
	return staticRow{values: []any{[]byte("{}"), status, "create", []byte("{}")}}
}

// makeUnlockSelectRowSpec — как makeUnlockSelectRow, но spec несёт заданный jsonb
// (проверка проброса spec.input в RunSpec.Input на create-пути).
func makeUnlockSelectRowSpec(status string, specJSON []byte) pgx.Row {
	return staticRow{values: []any{[]byte("{}"), status, "create", specJSON}}
}

// makeUnlockSelectRowBare — как makeUnlockSelectRow, но created_scenario = NULL
// (bare-инкарнация): 3-й scan-dest **string получит nil.
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
	// Scenario без input: пустое тело → 202 (input опционален).
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

// makeIncStatusRowBare — как makeIncStatusRow, но created_scenario = NULL (bare-
// инкарнация, миграция 090: создана без bootstrap-сценария). scanIncarnation читает
// 16-ю колонку в **string → nil.
func makeIncStatusRowBare(name, status string) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits
		any(nil), []byte(nil), // ADR-031 Slice C
		any(nil), // created_scenario = NULL (bare, миграция 090)
		any(nil), // applying_apply_id (ADR-068 §A1, bare → NULL)
	}}
}

// TestIncarnation_Run_BareIncarnation_Ops_202 — GUARD Фаза 2: bare-инкарнация
// (created_scenario IS NULL) запускает ОБЫЧНЫЙ operational-сценарий через
// RunTyped штатно — 202, прогон стартует. RunTyped резолвит инкарнацию по
// SelectByName и НЕ читает created_scenario для операционного запуска (он нужен только
// rerun-last на create-пути). Регресс = операционный путь начинает требовать created_scenario не-NULL
// (или паникует на NULL-проекции) → bare-инкарнации лишаются эксплуатационных операций.
func TestIncarnation_Run_BareIncarnation_Ops_202(t *testing.T) {
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
		t.Fatalf("Code = %d, want 202 (bare operational run), body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Fatalf("starter.calls = %d, want 1 (bare инкарнация допускает операционный сценарий)", starter.calls)
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
	// Unlock пишет state_history INSERT + UPDATE → 2 Exec-а.
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

// TestIncarnation_Unlock_ReasonAtMax_200 — reason ровно ReasonMaxLen символов
// проходит (граница включительно): unlock выполняется, 200 + 2 Exec-а.
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

// TestIncarnation_Unlock_ReasonOverMax_422 — reason длиннее ReasonMaxLen → 422
// ДО транзакции (верхняя граница reason, поведенческий инвариант).
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

// fakeLoader — мок [ServiceSnapshotLoader]. Load возвращает снапшот с
// заданной target schema-версией; LoadMigrationChain отдаёт chain/err из
// преднастроенных полей. git-стек не поднимается.
type fakeLoader struct {
	targetSchema int
	loadErr      error

	chain    statemigrate.Chain
	chainErr error

	// destroy pre-check (ReadFile): hasDestroyScenario=true → scenario `destroy`
	// «есть» в снапшоте; false → os.ErrNotExist. readErr перекрывает (I/O-сбой).
	hasDestroyScenario bool
	readErr            error

	// scenarioYAML — для sync input-валидации (scenario.ValidateInput):
	// непустое → ReadFile отдаёт этот YAML как scenario/<name>/main.yml.
	// Перекрывает hasDestroyScenario-ветку.
	scenarioYAML string

	// localDir — корень реального снапшота на диске (temp). Непустое → Load
	// проставляет его в ServiceArtifact.LocalDir, а ReadFile читает запрошенный
	// файл с диска (path-aware). Нужно для механизма нескольких create-сценариев:
	// scenario.ResolveCreateScenarios сканирует art.LocalDir (artifact.ListScenarios),
	// а scenario.ValidateInput читает scenario/<chosen>/main.yml — обе фазы должны
	// видеть один и тот же снапшот, поэтому per-path-ответ обязателен. Зеркало
	// dirInputLoader из scenario/validate_input_types_test.go. Перекрывает
	// scenarioYAML/hasDestroyScenario-ветки в ReadFile.
	localDir string

	// lifecycle — блок lifecycle манифеста снапшота (S3: auto_create/auto_destroy).
	// nil → манифест без lifecycle (оба флага дефолтно true, backcompat).
	lifecycle *config.LifecycleConfig

	// stateSchema — flat state_schema манифеста снапшота (seal read-path:
	// secretSchemaForIncarnation обходит его на secret:true). nil → без state_schema.
	stateSchema map[string]any

	// revealableSecrets — секция revealable_secrets манифеста снапшота (NIM-74):
	// revealableSecretsFor читает её на reveal-эндпоинте. nil → без reveal-деклараций.
	revealableSecrets []config.RevealableSecret

	loadCalls     int
	chainCalls    int
	readFileCalls int
	upgradesCalls int

	// loadedRefs фиксирует ref.Ref каждого Load (порядок вызовов) — guard-тесты
	// version-pin сверяют, на какой версии сервиса материализовался снапшот.
	loadedRefs []string

	// upgrades — результат ListUpgrades (ADR-0068): непустой список с FromVersions,
	// матчащий текущий пин, → found-ветвь UpgradeTyped. nil → legacy.
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

// ReadFile — для destroy PrepareDestroy pre-check (наличие scenario `destroy`)
// и sync input-валидации. localDir (если задан) — path-aware чтение с диска
// (read scenario/<name>/main.yml реального снапшота); иначе scenarioYAML/
// hasDestroyScenario-заглушки. hasDestroyScenario=true → файл «есть»; false →
// os.ErrNotExist (нет scenario). readErr (если задан) перекрывает всё — для теста I/O-сбоя.
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

// makeIncRowVer конструирует staticRow под SelectByName с заданными
// service_version и state_schema_version (для Upgrade resolve `from`).
func makeIncRowVer(name, serviceVersion string, schema int) pgx.Row {
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", serviceVersion, schema,
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"),          // traits
		any(nil), []byte(nil), // ADR-031 Slice C
		"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// makeUpgradeSelectRow конструирует staticRow под Upgrade SELECT FOR UPDATE
// (state, state_schema_version, status).
func makeUpgradeSelectRow(schema int, status string) pgx.Row {
	return staticRow{values: []any{[]byte("{}"), schema, status}}
}

func newUpgradeHandler(db *fakeIncDB, loader *fakeLoader) *IncarnationHandler {
	return NewIncarnationHandler(db, &fakeStarter{}, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
}

func TestIncarnation_Upgrade_202(t *testing.T) {
	// Реальный апгрейд v1→v2, схема 1→2: chain c одной миграцией. Happy path
	// (статус ready) → 202 + apply_id.
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

// TestIncarnation_Upgrade_FoundAutostart_202 — found-ветвь (ADR-0068 §5): для
// перехода v1→v2 есть upgrade-сценарий → 202 несёт run_apply_id=R (≠ M), Runner
// получил RunSpec{FromUpgrade:true, FromLocked:true, ApplyID:R, ScenarioName:slug,
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
	// ADR-0068 §7 инвариант: upgrade-сценарий работает со state, НЕ с input —
	// input НЕ мигрируется. RunSpec.Input обязан быть nil (регресс, если кто-то
	// начнёт протаскивать spec.input в upgrade-прогон).
	if sp.Input != nil {
		t.Errorf("RunSpec.Input = %v, want nil (input НЕ мигрируется, ADR-0068 §7)", sp.Input)
	}
}

// TestIncarnation_Upgrade_FoundNilRunner_500 — found-ветвь (upgrade-сценарий есть),
// но runner не сконфигурирован → 500 ДО резервирования applying (анти-зомби, ADR-0068
// §5: инкарнация не должна зависнуть в applying без Runner-прогона).
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
	// runner=nil (2-й арг) при найденном upgrade-сценарии.
	h := NewIncarnationHandler(db, nil, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	req := newChiRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade",
		bytes.NewReader([]byte(`{"to_version":"v2"}`)), "name", "redis-prod")
	req = withClaims(req, "archon-alice")
	rec := incUpgrade(h, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Code = %d, want 500 (found + runner=nil); body=%s", rec.Code, rec.Body.String())
	}
	// Анти-зомби: applying НЕ резервировался (UpgradeStateSchema SELECT FOR UPDATE
	// не вызван, никакой tx не открыт) — проверка runner=nil ДО смены статуса.
	if selectCalled {
		t.Error("UpgradeStateSchema SELECT FOR UPDATE вызван — applying зарезервирован ДО проверки runner (анти-зомби нарушен)")
	}
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %v, want пусто (никакого tx до 500)", db.execCalls)
	}
}

// TestIncarnation_Upgrade_LegacyNoRun_202 — legacy (upgrade-сценарий не найден):
// 202 БЕЗ run_apply_id, Runner НЕ вызван (drift + WARN, host-раскатка вручную).
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
	// Смена ref при той же схеме (target == current, to_version != текущего
	// service_version) — легитимный ref-bump: пустой chain, 202.
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
	// Полное совпадение: to_version == текущий service_version И схема та же
	// → 422 (апгрейдить нечего). Не доходит до LoadMigrationChain.
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
	// Downgrade через git-ref: incarnation на схеме 3, целевой ref несёт
	// схему 2 (target < current). Ранний guard в handler-е возвращает 409
	// (forward-only, ADR-019) ДО вызова LoadMigrationChain — реальный путь,
	// который раньше падал в 500 (загрузчик на from>to отдаёт обычную ошибку,
	// не ErrMigrationChainBroken). Отличается от downgrade-кейса в
	// SentinelMapping: там SelectByName видит совместимую схему, а downgrade
	// детектится позже под FOR UPDATE (защита гонки).
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

// upgradeSentinelCase — table-параметризация маппинга sentinel-ошибок
// incarnation.UpgradeStateSchema на HTTP-коды. Sentinel приходит из
// upgradeSelectRow (gate статуса) или downgrade (target < current schema).
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
			// FOR UPDATE видит схему 3, target 2 < 3 → downgrade.
			name:         "downgrade→409",
			upgradeRow:   func(_ string) pgx.Row { return makeUpgradeSelectRow(3, "ready") },
			targetSchema: 2,
			toVersion:    "v2",
			wantCode:     http.StatusConflict,
			wantType:     problem.TypeIncarnationLocked,
		},
		{
			// SelectByName видит схему 1, FOR UPDATE видит 5 (гонка resolve↔lock)
			// при пустом chain → schema-mismatch.
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

// chi-routing helper для unit-тестов — newChiRequest определён в
// operator_test.go и переиспользуется здесь напрямую (один пакет
// handlers, общая видимость).

// --- CheckDrift -------------------------------------------------------

// fakeDriftChecker — мок [DriftChecker] (CheckDrift + MarkDriftStatus).
// Фиксирует переданный spec, число вызовов и аргументы MarkDriftStatus; report
// / err — что вернуть из CheckDrift; markErr — что вернуть из MarkDriftStatus.
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

// sampleDriftReportH — образец отчёта с одним drifted-хостом для проверки
// тела ответа и aggregate-summary.
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

	// MarkDriftStatus вызван с hasDrift=true (есть drifted-хост) — паритет
	// с MCP-handler-ом.
	if !drift.marked {
		t.Fatal("MarkDriftStatus не вызван")
	}
	if drift.markName != "redis-prod" || !drift.markHasDrift {
		t.Errorf("MarkDriftStatus state = (%q, %v), want (redis-prod, true)",
			drift.markName, drift.markHasDrift)
	}

	// Audit-trail: EventIncarnationDriftChecked с correlation_id=apply_id и
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
	// drift=nil → endpoint не сконфигурирован, симметрично Run/Upgrade/Destroy.
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
	// Чистый отчёт (нет drifted/failed) → MarkDriftStatus(name, false): handler
	// сбрасывает incarnation в ready (если была в drift). Паритет с
	// информационной семантикой ADR-031(d).
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

// makeIncRowWithHosts — staticRow для SelectByName / FOR UPDATE с заданными
// hosts в spec.hosts[]. Зеркалит [makeIncarnationRow], но даёт контроль над
// jsonb-spec.
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
		"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
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
	// Spec в DTO должен содержать новые hosts.
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
		// Для remove всё равно проверяется существование SID.
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
		// b.example отсутствует в souls.
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

// TestUpdateHosts_EmptyReplace_Clears — replace с пустым hosts[] = осознанная
// очистка declared-spec (документированное решение, см. doc-comment UpdateHosts).
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

// TestUpdateHosts_Role_Boundaries — role kebab-case или пустая: пустая → ok,
// валидная kebab → ok, 63 символа → ok, 64 → 422, uppercase → 422.
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

// TestUpdateHosts_EmptySID_422 — hosts[].sid non-empty: пустой SID → 422.
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

// TestUpdateHosts_AuditEmitted — на 200-исход handler пишет
// `incarnation.hosts_updated` с правильным source / archon / payload.
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

// recordingAuditWriter — простой in-memory audit.Writer для проверки payload.
type recordingAuditWriter struct{ events []*audit.Event }

func (a *recordingAuditWriter) Write(_ context.Context, e *audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

// --- Sync input-validation (fix: required-input dyra) ------------------

// scenarioCreateRequiredInput — scenario `create` с required-полем `name`
// (string, без default) и опциональным `replicas` (integer, default 1).
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

// writeCreateScenarioDir пишет scenario/create/main.yml (yaml) в новый temp-корень
// и возвращает его. Фаза 2: ResolveCreateScenarios сканирует localDir, поэтому
// create-сценарий обязан лежать на диске. Если yaml не несёт `create: true` —
// префиксуем флагом (чтобы он попал в create-набор), сохраняя имя строки `create`.
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

// newCreateHandlerWithSchema — Create-handler с runner+services+loader
// (production-конфигурация), где loader отдаёт scenario `create` (yaml) с диска.
// Фаза 2: тесты-callers передают create_scenario=create (выбор обязателен при
// наличии create-сценариев).
func newCreateHandlerWithSchema(t *testing.T, db *fakeIncDB, yaml string) (*IncarnationHandler, *fakeStarter) {
	t.Helper()
	starter := &fakeStarter{}
	loader := &fakeLoader{localDir: writeCreateScenarioDir(t, yaml)}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	return h, starter
}

// TestIncarnation_Create_RequiredInputMissing_422 — ROOT-CAUSE баг "ba":
// create без required-поля теперь отвергается СИНХРОННО (422), incarnation-
// строка НЕ создаётся, scenario НЕ запускается.
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

// TestIncarnation_Create_RequiredInputMissing_NilInput_422 — отсутствие ключа
// `input` вовсе (nil) даёт тот же 422, что и пустой `{}`.
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

// TestIncarnation_Create_RequiredInputProvided_202 — required-поле передано →
// проходит, incarnation создаётся, scenario запускается.
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

// TestIncarnation_Create_TypeMismatch_422 — required передан, но опциональное
// поле неверного типа → 422 до мутации.
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

// TestIncarnation_Create_NoSchema_202 — scenario `create` без `input:` блока:
// любой input проходит (как у сервиса без обязательных полей).
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

// boolPtr — helper для *bool lifecycle-флагов в тестах.
func boolPtr(b bool) *bool { return &b }

// TestIncarnation_Create_AutoCreateFalse_NoRun — lifecycle.auto_create=false:
// инкарнация создаётся (insert), НО scenario `create` НЕ запускается; ответ 202
// БЕЗ apply_id (omitted), status остаётся ready.
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
	// created_scenario при auto_create=false НЕ NULL: bootstrap-сценарий есть (create),
	// прогон лишь отложен (отличие от bare). $12 = create.
	if got, _ := db.insertArgs[11].(string); got != "create" {
		t.Errorf("INSERT created_scenario ($12) = %q, want create (auto_create=false ≠ bare)", got)
	}
	// apply_id отсутствует в JSON (nullable, omitempty).
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
// (явно) → прогон стартует, apply_id присутствует (паритет default).
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

// TestIncarnation_Create_NoLifecycleBlock_Run — манифест без lifecycle-блока
// (nil) → backcompat: прогон стартует (auto_create дефолтно true).
func TestIncarnation_Create_NoLifecycleBlock_Run(t *testing.T) {
	db := &fakeIncDB{}
	// lifecycle=nil в fakeLoader (манифест без блока) → auto_create дефолтно true.
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

// newRunHandlerWithSchema — Run-handler с loader, отдающим scenario с
// required-input (production-конфигурация Run).
func newRunHandlerWithSchema(db *fakeIncDB, yaml string) (*IncarnationHandler, *fakeStarter) {
	starter := &fakeStarter{}
	loader := &fakeLoader{scenarioYAML: yaml}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	return h, starter
}

// TestIncarnation_Run_RequiredInputMissing_422 — scenario-run без required-
// поля отвергается sync (422), прогон НЕ стартует.
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

// TestIncarnation_Run_RequiredInputProvided_202 — required передан → прогон
// стартует.
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

// --- Golden wire-guard (oapi-миграция, байт-в-байт) -------------------

// TestIncarnationGetReply_GoldenNullFields — wire-инвариант после миграции на
// сгенерированные oapi-типы: nullable+required поля (created_by_aid / state /
// spec / status_details) при nil ОБЯЗАНЫ присутствовать в JSON со значением
// `null` (НЕ опускаться). Ловит регресс, если на любое из них вернётся
// omitempty (тогда ключ исчезнет и контракт сломается — клиент/UI ждёт null).
func TestIncarnationGetReply_GoldenNullFields(t *testing.T) {
	inc := &incarnation.Incarnation{
		Name:               "redis-prod",
		Service:            "redis",
		ServiceVersion:     "v1",
		StateSchemaVersion: 1,
		Status:             incarnation.StatusReady,
		// Spec / State / StatusDetails / CreatedByAID — все nil.
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

// TestIncarnationGetReply_DriftSummaryTyped — wire-инвариант typed
// last_drift_summary (уход от opaque-passthrough): заполненная колонка едет на
// проволоку typed-объектом с counts-ключами (integer, НЕ float-строки) и
// scanned_at в RFC3339Nano. Ловит регресс возврата к map-passthrough или
// потерю/переименование полей DriftScanSummary.
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
	// Round-trip через wire-форму — counts/scanned_at читаются обратно typed без потерь.
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

// TestIncarnationGetReply_DriftSummaryOmittedWhenNil — NULL-колонка
// (incarnation ни разу не сканировалась): ключ last_drift_summary ОТСУТСТВУЕТ
// на wire (omit, не null) — прежняя omit-семантика сохранена после типизации.
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

// TestStateHistoryEntry_GoldenChangedByAIDOmitted — симметричный инвариант для
// changed_by_aid: при пустом (nil) значении ключ ОБЯЗАН ОТСУТСТВОВАТЬ (omit, не
// null). Текущий wire = omit при пустом; регресс на nullable-без-omitempty
// добавил бы `"changed_by_aid":null` и сломал байт-в-байт.
func TestStateHistoryEntry_GoldenChangedByAIDOmitted(t *testing.T) {
	e := &incarnation.HistoryEntry{
		HistoryID: "01HX",
		Scenario:  "rotate",
		// StateBefore / StateAfter — nil (присутствуют как null), ChangedByAID — nil.
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
	// state_before/state_after — наоборот, ПРИСУТСТВУЮТ как null (required+nullable).
	for _, want := range []string{`"state_before":null`, `"state_after":null`} {
		if !strings.Contains(got, want) {
			t.Errorf("wire НЕ содержит %s (omitempty-регресс?)\nwire: %s", want, got)
		}
	}
}
