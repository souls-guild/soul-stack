package handlers

// Guard-тесты глобального read-view прогонов на handler-границе (AllRunsTyped /
// RunsStatsTyped): валидация входа (limit-cap 100 / статус / имя инкарнации),
// fail-closed Purview (nil claims / nil scoper / пустой scope → пустой ответ БЕЗ
// захода в store), scope-pushdown (подзапрос по incarnation в SQL + bind-args) и
// проекция store→View. Реальная SQL-семантика свёртки — в integration-тестах
// applyrun (runsglobal_integration_test.go); здесь именно handler-слой.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

func runsClaims() *keeperjwt.Claims { return &keeperjwt.Claims{Subject: "archon-alice"} }

// newRunsHandler — handler с fake-БД и заданным scoper-ом (только db/scoper нужны
// глобальному read-view).
func newRunsHandler(db *fakeIncDB, scoper PurviewResolver) *IncarnationHandler {
	return NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, scoper, nil)
}

// --- AllRunsTyped: валидация входа -------------------------------------

func TestAllRunsTyped_BadLimit_400(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(), AllRunsInput{Offset: 0, Limit: 0})
	requireProblemStatus(t, err, 400)
}

// TestAllRunsTyped_LimitOver100_400 — спец-cap /v1/runs: limit ≤ 100 (уже общего
// MaxPageLimit=1000).
func TestAllRunsTyped_LimitOver100_400(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(), AllRunsInput{Offset: 0, Limit: 101})
	requireProblemStatus(t, err, 400)
}

func TestAllRunsTyped_BadStatus_422(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Status: "exploded", Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
}

func TestAllRunsTyped_BadIncarnationName_422(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Incarnation: "Bad_Name", Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
}

// TestAllRunsTyped_BadSort_422 — не-whitelist поле сортировки → 422 (sentinel из
// store, ADR-068 §B1).
func TestAllRunsTyped_BadSort_422(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Sort: "created_at; DROP TABLE apply_runs", Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
}

// TestAllRunsTyped_BadSortDir_422 — не-asc/desc направление → 422.
func TestAllRunsTyped_BadSortDir_422(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{SortDir: "sideways", Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
}

// TestAllRunsTyped_ValidSort_OK — валидные sort/sort_dir прокидываются в store и
// не роняют путь (реальный порядок — в integration-тестах applyrun).
func TestAllRunsTyped_ValidSort_OK(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Sort: "status", SortDir: "asc", Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("AllRunsTyped(valid sort): %v", err)
	}
	if !db.runsCalled {
		t.Error("валидный sort не дошёл до store")
	}
}

// TestAllRunsTyped_ServiceLongName_NoValidation422 — service-фильтр — точный
// bound-match, НЕ доменный валидатор: легитимное имя сервиса длиной ≥64 (валидно
// для реестра ^[a-z][a-z0-9-]*$, миграция 034 без cap длины; incarnation.service
// без формат-CHECK, 005) не даёт 422, а доходит до store bind-аргом.
func TestAllRunsTyped_ServiceLongName_NoValidation422(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	longSvc := strings.Repeat("a", 80) // валиден для реестра, но длиннее старого cap ValidName=63
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Service: longSvc, Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("AllRunsTyped(long service): неожиданно 422/ошибка на валидном сервисе: %v", err)
	}
	if !db.runsCalled {
		t.Error("длинное валидное имя сервиса не дошло до store (ошибочно отсечено валидатором)")
	}
	if !argsHasString(db.lastRunsArgs, longSvc) {
		t.Errorf("service не пришёл bind-аргом: %v", db.lastRunsArgs)
	}
}

// TestAllRunsTyped_ServiceGarbage_SafeBind — мусорный/инъекционный service —
// bound bind-арг (не конкатенация): не 422 и не 500, доходит до store как
// значение (на реальном PG exact-match даст пустую выдачу).
func TestAllRunsTyped_ServiceGarbage_SafeBind(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	garbage := "no-such'; DROP TABLE incarnation;--"
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Service: garbage, Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("AllRunsTyped(garbage service): want без ошибки (bound-фильтр), got %v", err)
	}
	if !db.runsCalled || !argsHasString(db.lastRunsArgs, garbage) {
		t.Errorf("garbage service должен уйти bound bind-аргом (инъекционно безопасно): args=%v", db.lastRunsArgs)
	}
}

// TestAllRunsTyped_FilterCapByRunes — cap service/q считается в РУНАХ, не байтах:
// 128 кириллических символов (256 байт) проходят, 129 — 422 (оба поля).
func TestAllRunsTyped_FilterCapByRunes(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	okRunes := strings.Repeat("я", 128) // 256 байт — в байтовом cap отсеклось бы
	tooLong := strings.Repeat("я", 129) // 129 символов — за границей

	if _, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Q: okRunes, Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("q=128 рун: неожиданно 422 (cap в байтах?): %v", err)
	}
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Service: okRunes, Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("service=128 рун: неожиданно 422 (cap в байтах?): %v", err)
	}
	_, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Q: tooLong, Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
	_, err = h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Service: tooLong, Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
}

// TestAllRunsTyped_BadStartedAfter_422 — малформед started_after (не RFC3339) → 422.
func TestAllRunsTyped_BadStartedAfter_422(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{StartedAfter: "2026-99-99", Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
}

// TestAllRunsTyped_BadStartedBefore_422 — малформед started_before → 422.
func TestAllRunsTyped_BadStartedBefore_422(t *testing.T) {
	h := newRunsHandler(&fakeIncDB{}, fakeIncScoper{unrestricted: true})
	_, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{StartedBefore: "not-a-time", Offset: 0, Limit: 50})
	requireProblemStatus(t, err, 422)
}

// TestAllRunsTyped_TimeFilters_Bind — валидные RFC3339-границы парсятся и уходят в
// store bind-аргами (time.Time), путь не роняется.
func TestAllRunsTyped_TimeFilters_Bind(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{StartedAfter: "2026-01-01T00:00:00Z", StartedBefore: "2026-12-31T23:59:59Z",
			Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("AllRunsTyped(valid time): %v", err)
	}
	if !db.runsCalled {
		t.Fatal("валидные time-границы не дошли до store")
	}
	var times int
	for _, a := range db.lastRunsArgs {
		if _, ok := a.(time.Time); ok {
			times++
		}
	}
	if times != 2 {
		t.Errorf("ожидались 2 time.Time bind-арга (after/before), got %d: %v", times, db.lastRunsArgs)
	}
}

// --- AllRunsTyped: fail-closed Purview ----------------------------------

// TestAllRunsTyped_NilClaims_FailClosed — нет claims → пустая страница (200), store
// НЕ вызван (не палим прогоны всего флота).
func TestAllRunsTyped_NilClaims_FailClosed(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	reply, err := h.AllRunsTyped(context.Background(), nil, AllRunsInput{Offset: 0, Limit: 50})
	if err != nil {
		t.Fatalf("AllRunsTyped: %v", err)
	}
	if reply.Total != 0 || len(reply.Items) != 0 {
		t.Errorf("Total=%d len=%d, want 0/0 (fail-closed)", reply.Total, len(reply.Items))
	}
	if db.runsCalled {
		t.Error("store вызван при nil claims — fail-closed обязан отсечь ДО БД")
	}
}

// TestAllRunsTyped_EmptyPurview_FailClosed — Purview без измерений → пустая
// страница, store не вызван.
func TestAllRunsTyped_EmptyPurview_FailClosed(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{empty: true})
	reply, err := h.AllRunsTyped(context.Background(), runsClaims(), AllRunsInput{Offset: 0, Limit: 50})
	if err != nil {
		t.Fatalf("AllRunsTyped: %v", err)
	}
	if reply.Total != 0 || len(reply.Items) != 0 {
		t.Errorf("Total=%d len=%d, want 0/0 (fail-closed empty Purview)", reply.Total, len(reply.Items))
	}
	if db.runsCalled {
		t.Error("store вызван при пустом Purview — fail-closed обязан отсечь ДО БД")
	}
}

// TestAllRunsTyped_NilScoper_FailClosed — мис-wire-up (scoper nil) → пустая страница.
func TestAllRunsTyped_NilScoper_FailClosed(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, nil)
	reply, err := h.AllRunsTyped(context.Background(), runsClaims(), AllRunsInput{Offset: 0, Limit: 50})
	if err != nil {
		t.Fatalf("AllRunsTyped: %v", err)
	}
	if reply.Total != 0 || len(reply.Items) != 0 || db.runsCalled {
		t.Errorf("want пустой ответ без захода в store (nil scoper); Total=%d runsCalled=%v",
			reply.Total, db.runsCalled)
	}
}

// --- AllRunsTyped: scope-pushdown и проекция -----------------------------

// TestAllRunsTyped_ScopePushdownSQL — coven-scoped оператор: scope уходит в SQL
// подзапросом по incarnation (coven∪{name}-плечо), scope-значения — bind-args.
func TestAllRunsTyped_ScopePushdownSQL(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{covens: []string{"prod"}})
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(), AllRunsInput{Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("AllRunsTyped: %v", err)
	}
	if !db.runsCalled {
		t.Fatal("store не вызван (ожидался scope-pushdown, не fail-closed)")
	}
	if !strings.Contains(db.lastRunsSQL, "IN (SELECT name FROM incarnation WHERE") {
		t.Errorf("scope не дошёл до SQL подзапросом по incarnation:\n%s", db.lastRunsSQL)
	}
	if !strings.Contains(db.lastRunsSQL, "covens &&") {
		t.Errorf("coven∪{name}-плечо scope не в SQL:\n%s", db.lastRunsSQL)
	}
	foundCovens := false
	for _, a := range db.lastRunsArgs {
		if ss, ok := a.([]string); ok && len(ss) == 1 && ss[0] == "prod" {
			foundCovens = true
		}
	}
	if !foundCovens {
		t.Errorf("scope-ковены не пришли bind-аргом: %v", db.lastRunsArgs)
	}
}

// TestAllRunsTyped_Unrestricted_NoScopeSubquery — Unrestricted-оператор: scope-
// подзапрос в SQL отсутствует (полный список без ограничения).
func TestAllRunsTyped_Unrestricted_NoScopeSubquery(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(), AllRunsInput{Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("AllRunsTyped: %v", err)
	}
	if strings.Contains(db.lastRunsSQL, "SELECT name FROM incarnation") {
		t.Errorf("Unrestricted не должен нести scope-подзапрос:\n%s", db.lastRunsSQL)
	}
}

// TestAllRunsTyped_Filters_Bind — фильтры status/incarnation уходят bind-аргами.
func TestAllRunsTyped_Filters_Bind(t *testing.T) {
	db := &fakeIncDB{}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	if _, err := h.AllRunsTyped(context.Background(), runsClaims(),
		AllRunsInput{Status: "failed", Incarnation: "redis-prod", Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("AllRunsTyped: %v", err)
	}
	if !argsHasString(db.lastRunsArgs, "failed") || !argsHasString(db.lastRunsArgs, "redis-prod") {
		t.Errorf("фильтры не пришли bind-args (failed, redis-prod): %v", db.lastRunsArgs)
	}
}

// TestAllRunsTyped_Projection_OK — happy-path проекции store→View: два прогона
// разных инкарнаций, все поля перенесены.
func TestAllRunsTyped_Projection_OK(t *testing.T) {
	now := time.Now().UTC()
	fin := now.Add(time.Minute)
	aid := "archon-alice"
	db := &fakeIncDB{
		runsCountRow: func(string) pgx.Row { return staticRow{values: []any{int(2)}} },
		applyRunsRows: func() (pgx.Rows, error) {
			return &globalRunRows{rows: []globalRunRow{
				{applyID: "01HRUN2", incarnation: "redis-staging", service: "redis", scenario: "restart",
					startedAt: now, status: "applying"},
				{applyID: "01HRUN1", incarnation: "redis-prod", service: "postgres", scenario: "create",
					startedAt: now.Add(-time.Hour), finishedAt: &fin, startedBy: &aid, status: "success"},
			}}, nil
		},
	}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	reply, err := h.AllRunsTyped(context.Background(), runsClaims(), AllRunsInput{Offset: 0, Limit: 50})
	if err != nil {
		t.Fatalf("AllRunsTyped: %v", err)
	}
	if reply.Total != 2 || len(reply.Items) != 2 {
		t.Fatalf("Total=%d len=%d, want 2/2", reply.Total, len(reply.Items))
	}
	first := reply.Items[0]
	if first.ApplyID != "01HRUN2" || first.Incarnation != "redis-staging" || first.Status != "applying" {
		t.Errorf("Items[0] = %+v, want 01HRUN2/redis-staging/applying", first)
	}
	if first.Service != "redis" {
		t.Errorf("Items[0].Service = %q, want redis (service не перенесён в проекцию)", first.Service)
	}
	if first.FinishedAt != nil || first.StartedByAID != nil {
		t.Errorf("Items[0] applying: FinishedAt/StartedByAID должны быть nil: %+v", first)
	}
	second := reply.Items[1]
	if second.Incarnation != "redis-prod" || second.Status != "success" {
		t.Errorf("Items[1] = %+v, want redis-prod/success", second)
	}
	if second.Service != "postgres" {
		t.Errorf("Items[1].Service = %q, want postgres", second.Service)
	}
	if second.FinishedAt == nil || second.StartedByAID == nil || *second.StartedByAID != aid {
		t.Errorf("Items[1] success: FinishedAt/StartedByAID не перенесены: %+v", second)
	}
}

// --- RunsStatsTyped ------------------------------------------------------

// TestRunsStatsTyped_NilClaims_ZeroStats — fail-closed: нулевой агрегат (200),
// store не вызван.
func TestRunsStatsTyped_NilClaims_ZeroStats(t *testing.T) {
	storeCalled := false
	db := &fakeIncDB{applyRunsRows: func() (pgx.Rows, error) { storeCalled = true; return &emptyRows{}, nil }}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	stats, err := h.RunsStatsTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunsStatsTyped: %v", err)
	}
	if stats != (RunsStatsView{}) {
		t.Errorf("stats = %+v, want нулевой агрегат (fail-closed)", stats)
	}
	if storeCalled {
		t.Error("store вызван при nil claims — fail-closed обязан отсечь ДО БД")
	}
}

// TestRunsStatsTyped_OK — проекция store→View: счётчики по статусам + total.
func TestRunsStatsTyped_OK(t *testing.T) {
	db := &fakeIncDB{
		applyRunsRows: func() (pgx.Rows, error) {
			return &runsStatsRows{rows: [][3]any{
				{"success", int(10), int(3)},
				{"failed", int(2), int(1)},
				{"applying", int(1), int(1)},
			}}, nil
		},
	}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	stats, err := h.RunsStatsTyped(context.Background(), runsClaims())
	if err != nil {
		t.Fatalf("RunsStatsTyped: %v", err)
	}
	wantAll := RunsStatsBucketView{Total: 13, Applying: 1, Success: 10, Failed: 2}
	if stats.All != wantAll {
		t.Errorf("All = %+v, want %+v", stats.All, wantAll)
	}
	want24h := RunsStatsBucketView{Total: 5, Applying: 1, Success: 3, Failed: 1}
	if stats.Last24h != want24h {
		t.Errorf("Last24h = %+v, want %+v", stats.Last24h, want24h)
	}
}

// --- rows-стабы ----------------------------------------------------------

// globalRunRow — одна строка глобального списка (порядок колонок listSQL
// applyrun.ListRuns: apply_id/incarnation/scenario/service/started_at/
// finished_at/started_by_aid/status).
type globalRunRow struct {
	applyID     string
	incarnation string
	scenario    string
	service     string
	startedAt   time.Time
	finishedAt  *time.Time
	startedBy   *string
	status      string
}

// globalRunRows — pgx.Rows-стаб глобального списка прогонов.
type globalRunRows struct {
	rows []globalRunRow
	idx  int
}

func (r *globalRunRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *globalRunRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	vals := []any{row.applyID, row.incarnation, row.scenario, row.service, row.startedAt,
		row.finishedAt, row.startedBy, row.status}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = vals[i].(string)
		case *time.Time:
			*d = vals[i].(time.Time)
		case **time.Time:
			*d = vals[i].(*time.Time)
		case **string:
			*d = vals[i].(*string)
		default:
			return errors.New("globalRunRows.Scan: неподдержанный тип dest")
		}
	}
	return nil
}

func (r *globalRunRows) Err() error                                   { return nil }
func (r *globalRunRows) Close()                                       {}
func (r *globalRunRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *globalRunRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *globalRunRows) Values() ([]any, error)                       { return nil, nil }
func (r *globalRunRows) RawValues() [][]byte                          { return nil }
func (r *globalRunRows) Conn() *pgx.Conn                              { return nil }

// runsStatsRows — pgx.Rows-стаб stats-запроса (status/total/last24h).
type runsStatsRows struct {
	rows [][3]any
	idx  int
}

func (r *runsStatsRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *runsStatsRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = row[i].(string)
		case *int:
			*d = row[i].(int)
		default:
			return errors.New("runsStatsRows.Scan: неподдержанный тип dest")
		}
	}
	return nil
}

func (r *runsStatsRows) Err() error                                   { return nil }
func (r *runsStatsRows) Close()                                       {}
func (r *runsStatsRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *runsStatsRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *runsStatsRows) Values() ([]any, error)                       { return nil, nil }
func (r *runsStatsRows) RawValues() [][]byte                          { return nil }
func (r *runsStatsRows) Conn() *pgx.Conn                              { return nil }
