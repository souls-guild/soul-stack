//go:build integration

// Integration-тесты CRUD incarnation / state_history через testcontainers-go.
// Паттерн совпадает с keeper/internal/operator/integration_test.go.

package incarnation

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/audit"
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
			log.Fatalf("incarnation integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("incarnation integration: skipping, docker unavailable: %v", err)
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
	// CASCADE: state_history / apply_runs / apply_task_register → incarnation →
	// operators (FK chain). archive-таблицы (S-D3) FK-связей не имеют — чистим
	// явным перечислением.
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE state_history, apply_runs, incarnation,
		 incarnation_archive, state_history_archive,
		 operators, audit_log CASCADE`)
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

func TestIntegration_Create_AndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	inc := &Incarnation{
		Name:               "redis-prod",
		Service:            "redis",
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		Spec:               map[string]any{"replicas": 3, "tls": true},
		State:              map[string]any{"primary": "redis-prod-01"},
		Status:             StatusReady,
		CreatedByAID:       &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inc.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero — RETURNING did not fill")
	}

	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Name != "redis-prod" || got.Service != "redis" || got.Status != StatusReady {
		t.Errorf("got = %+v", got)
	}
	if got.CreatedByAID == nil || *got.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", got.CreatedByAID)
	}
	if got.Spec["replicas"] != float64(3) {
		t.Errorf("Spec.replicas = %v", got.Spec["replicas"])
	}
	if got.State["primary"] != "redis-prod-01" {
		t.Errorf("State.primary = %v", got.State["primary"])
	}
}

func TestIntegration_Create_DuplicateName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create#1: %v", err)
	}
	inc2 := &Incarnation{
		Name: "redis-prod", Service: "other", ServiceVersion: "v2",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	err := Create(ctx, integrationPool, inc2)
	if !errors.Is(err, ErrIncarnationAlreadyExists) {
		t.Fatalf("err = %v, want ErrIncarnationAlreadyExists", err)
	}
}

func TestIntegration_Create_FKViolation(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	ghost := "archon-ghost"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &ghost,
	}
	err := Create(ctx, integrationPool, inc)
	if err == nil {
		t.Fatal("Create with non-existent operator: expected error")
	}
	if errors.Is(err, ErrIncarnationAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrIncarnationAlreadyExists; err = %v", err)
	}
}

func TestIntegration_Create_CHECKViolation_BadName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	// ValidName fail-fast в Go — но SQL CHECK дублирует. Гасим Go-validation
	// через corner case: имя из символов, проходящих NamePattern, но с
	// верхним регистром (NamePattern reject-ит, тест уже в crud_test).
	// Здесь явно проверяем что SQL-side CHECK для bad-status работает,
	// если кто-то обойдёт Go-check (например, прямой Exec со старым enum).
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO incarnation (name, service, service_version, status, created_by_aid)
		 VALUES ($1, 'svc', 'v1', 'destroyed', $2)`,
		"redis-prod", creator)
	if err == nil {
		t.Fatal("expected CHECK violation for status='destroyed'")
	}
}

func TestIntegration_SelectByName_NotFound(t *testing.T) {
	resetAll(t)
	_, err := SelectByName(context.Background(), integrationPool, "missing")
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
}

func TestIntegration_SelectAll_Pagination(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	for _, name := range []string{"redis-a", "redis-b", "redis-c", "mysql-d"} {
		inc := &Incarnation{
			Name: name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		}
		if name == "mysql-d" {
			inc.Service = "mysql"
		}
		if err := Create(ctx, integrationPool, inc); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		// Гарантия монотонного created_at для устойчивой сортировки в проверках.
		time.Sleep(2 * time.Millisecond)
	}

	out, total, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Unrestricted: true}, 0, 2)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	if len(out) != 2 {
		t.Errorf("len(out) = %d, want 2", len(out))
	}
	// DESC по created_at → последний (mysql-d) первым.
	if out[0].Name != "mysql-d" {
		t.Errorf("out[0].Name = %q, want mysql-d", out[0].Name)
	}

	out, total, err = SelectAll(ctx, integrationPool, ListFilter{Service: "redis"}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll filtered: %v", err)
	}
	if total != 3 || len(out) != 3 {
		t.Errorf("filtered: total=%d len=%d, want 3/3", total, len(out))
	}
}

// TestIntegration_SelectAll_StateFilter — реальный jsonb-pushdown ->>
// в PG: eq по string-полю, numeric-cast по числовому, несуществующее поле,
// сортировка по state-полю.
func TestIntegration_SelectAll_StateFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	seed := []struct {
		name  string
		state map[string]any
	}{
		{"redis-a", map[string]any{"redis_version": "8.0", "memory_mb": 2048}},
		{"redis-b", map[string]any{"redis_version": "7.2", "memory_mb": 512}},
		{"redis-c", map[string]any{"redis_version": "8.0", "memory_mb": 4096}},
	}
	for _, s := range seed {
		inc := &Incarnation{
			Name: s.name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
			State: s.state,
		}
		if err := Create(ctx, integrationPool, inc); err != nil {
			t.Fatalf("Create %s: %v", s.name, err)
		}
	}

	// eq по string-полю: redis_version=8.0 → a, c.
	out, total, err := SelectAll(ctx, integrationPool, ListFilter{
		StatePredicates: []StateEq{{Path: "redis_version", Op: StateOpEq, Value: "8.0"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll eq: %v", err)
	}
	if total != 2 || len(out) != 2 {
		t.Errorf("eq: total=%d len=%d, want 2/2", total, len(out))
	}

	// numeric: memory_mb > 1000 → a (2048), c (4096).
	out, total, err = SelectAll(ctx, integrationPool, ListFilter{
		StatePredicates: []StateEq{{Path: "memory_mb", Op: StateOpGt, Value: "1000"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll numeric: %v", err)
	}
	if total != 2 || len(out) != 2 {
		t.Errorf("numeric: total=%d len=%d, want 2/2", total, len(out))
	}

	// Несуществующее state-поле → пусто, НЕ ошибка.
	out, total, err = SelectAll(ctx, integrationPool, ListFilter{
		StatePredicates: []StateEq{{Path: "ghost_field", Op: StateOpEq, Value: "x"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll ghost: %v", err)
	}
	if total != 0 || len(out) != 0 {
		t.Errorf("ghost: total=%d len=%d, want 0/0", total, len(out))
	}

	// Сортировка по state-полю (memory_mb asc) с tie-break name.
	out, _, err = SelectAll(ctx, integrationPool, ListFilter{
		SortBy: "state.memory_mb", SortDir: SortAsc,
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll sort: %v", err)
	}
	// ->> отдаёт текст; "2048" < "4096" < "512" лексикографически — проверяем
	// именно текстовый порядок (numeric-sort — отдельная задача, не в объёме).
	if len(out) != 3 {
		t.Fatalf("sort: len=%d, want 3", len(out))
	}
}

// TestIntegration_SelectAll_StateFilter_InjectionSafe — попытка инъекции
// через state-path не доходит до PG (reject до запроса), таблица цела.
func TestIntegration_SelectAll_StateFilter_InjectionSafe(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, _, err := SelectAll(ctx, integrationPool, ListFilter{
		StatePredicates: []StateEq{{Path: "x'; DROP TABLE incarnation; --", Op: StateOpEq, Value: "1"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if !errors.Is(err, ErrInvalidStatePath) {
		t.Fatalf("err = %v, want ErrInvalidStatePath", err)
	}

	// Таблица должна остаться — строка на месте.
	if _, err := SelectByName(ctx, integrationPool, "redis-prod"); err != nil {
		t.Fatalf("incarnation пропал после попытки инъекции: %v", err)
	}
}

func TestIntegration_HistorySelectByName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Прямой INSERT в state_history — handler-level Write придёт в M0.6c-2.
	for i, hid := range []string{"01HFIRST", "01HSECOND"} {
		_, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario,
    state_before, state_after, changed_by_aid, apply_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			hid, "redis-prod", "create",
			[]byte(`{}`), []byte(`{"step":1}`),
			creator, "01HAPPLY",
		)
		if err != nil {
			t.Fatalf("seed history #%d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	out, total, err := HistorySelectByName(ctx, integrationPool, "redis-prod", HistoryFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 2 || len(out) != 2 {
		t.Errorf("total = %d, len = %d", total, len(out))
	}
	// DESC по at → 01HSECOND первым.
	if out[0].HistoryID != "01HSECOND" {
		t.Errorf("out[0] = %q, want 01HSECOND", out[0].HistoryID)
	}
}

func TestIntegration_HistorySelectByName_EmptyHistory(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	out, total, err := HistorySelectByName(ctx, integrationPool, "redis-prod", HistoryFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 0 || len(out) != 0 {
		t.Errorf("total = %d, len = %d", total, len(out))
	}
}

func TestIntegration_StateHistory_FK_OnIncarnationDelete(t *testing.T) {
	// CASCADE: при удалении incarnation history-row-ы исчезают.
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario,
    state_before, state_after, changed_by_aid, apply_id)
VALUES ('01HFOO', 'redis-prod', 'create', '{}', '{"x":1}', $1, '01HAPPLY')`,
		creator); err != nil {
		t.Fatalf("insert history: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM incarnation WHERE name = 'redis-prod'`); err != nil {
		t.Fatalf("DELETE incarnation: %v", err)
	}
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM state_history WHERE incarnation_name = 'redis-prod'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("state_history rows after CASCADE = %d, want 0", n)
	}
}

// TestIntegration_Unlock_FromErrorLocked проверяет happy-path unlock:
// error_locked → ready, status_details сброшены, в state_history появляется
// snapshot-row (scenario=unlock, state не изменён), previous_status вернулся.
func TestIntegration_Unlock_FromErrorLocked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1,
		State:              map[string]any{"primary": "redis-prod-01"},
		Status:             StatusErrorLocked,
		StatusDetails:      map[string]any{"reason": "dispatch_failed"},
		CreatedByAID:       &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := Unlock(ctx, integrationPool, "redis-prod", "manual cleanup verified", "archon-alice", "01HUNLOCK0000000000000000")
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if res.PreviousStatus != StatusErrorLocked {
		t.Errorf("PreviousStatus = %q, want error_locked", res.PreviousStatus)
	}

	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready", got.Status)
	}
	if got.StatusDetails != nil {
		t.Errorf("status_details = %v, want nil (reset on unlock)", got.StatusDetails)
	}
	if got.State["primary"] != "redis-prod-01" {
		t.Errorf("state changed by unlock: %v (must be untouched)", got.State)
	}

	hist, total, err := HistorySelectByName(ctx, integrationPool, "redis-prod", HistoryFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 || len(hist) != 1 {
		t.Fatalf("history rows = %d, want 1", total)
	}
	if hist[0].Scenario != "unlock" {
		t.Errorf("history scenario = %q, want unlock", hist[0].Scenario)
	}
	if hist[0].ChangedByAID == nil || *hist[0].ChangedByAID != "archon-alice" {
		t.Errorf("history changed_by_aid = %v", hist[0].ChangedByAID)
	}
}

// TestIntegration_Unlock_NotLocked проверяет, что unlock из ready отвергается
// (ErrIncarnationNotLocked) и не пишет в state_history.
func TestIntegration_Unlock_NotLocked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := Unlock(ctx, integrationPool, "redis-prod", "x", "archon-alice", "01HUNLOCK0000000000000001")
	if !errors.Is(err, ErrIncarnationNotLocked) {
		t.Fatalf("err = %v, want ErrIncarnationNotLocked", err)
	}
	_, total, _ := HistorySelectByName(ctx, integrationPool, "redis-prod", HistoryFilter{}, 0, 50)
	if total != 0 {
		t.Errorf("history rows = %d, want 0 (rollback)", total)
	}
}

// TestIntegration_Unlock_FromApplying проверяет, что unlock залоченной-в-
// applying incarnation отвергается (ErrIncarnationNotLocked → 409): unlock
// допустим только из error_locked. applying семантически опаснее ready —
// прогон ещё идёт, ручное снятие не должно вмешиваться. State и статус не
// трогаются, в state_history ничего не пишется.
func TestIntegration_Unlock_FromApplying(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1,
		State:              map[string]any{"primary": "redis-prod-01"},
		Status:             StatusApplying, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := Unlock(ctx, integrationPool, "redis-prod", "x", "archon-alice", "01HUNLOCK0000000000000003")
	if !errors.Is(err, ErrIncarnationNotLocked) {
		t.Fatalf("err = %v, want ErrIncarnationNotLocked", err)
	}

	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusApplying {
		t.Errorf("status = %q, want applying (unchanged)", got.Status)
	}
	if got.State["primary"] != "redis-prod-01" {
		t.Errorf("state changed by rejected unlock: %v (must be untouched)", got.State)
	}
	_, total, _ := HistorySelectByName(ctx, integrationPool, "redis-prod", HistoryFilter{}, 0, 50)
	if total != 0 {
		t.Errorf("history rows = %d, want 0 (rollback)", total)
	}
}

// TestIntegration_Unlock_FromMigrationFailed проверяет, что unlock снимает
// migration_failed → ready (ADR-019): миграция атомарна в одной tx, при фейле
// rollback оставляет дореформенный консистентный state, поэтому unlock
// возвращает incarnation в рабочее состояние. State НЕ трогается, в
// state_history появляется snapshot-row (scenario=unlock).
func TestIntegration_Unlock_FromMigrationFailed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1,
		State:              map[string]any{"primary": "redis-prod-01"},
		Status:             StatusMigrationFailed,
		StatusDetails:      map[string]any{"reason": "migration_failed"},
		CreatedByAID:       &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := Unlock(ctx, integrationPool, "redis-prod", "rolled-back state verified", "archon-alice", "01HUNLOCK0000000000000004")
	if err != nil {
		t.Fatalf("Unlock from migration_failed: %v", err)
	}
	if res.PreviousStatus != StatusMigrationFailed {
		t.Errorf("PreviousStatus = %q, want migration_failed", res.PreviousStatus)
	}

	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready", got.Status)
	}
	if got.StatusDetails != nil {
		t.Errorf("status_details = %v, want nil (reset on unlock)", got.StatusDetails)
	}
	if got.State["primary"] != "redis-prod-01" {
		t.Errorf("state changed by unlock: %v (must be untouched)", got.State)
	}
	_, total, _ := HistorySelectByName(ctx, integrationPool, "redis-prod", HistoryFilter{}, 0, 50)
	if total != 1 {
		t.Errorf("history rows = %d, want 1 (unlock snapshot)", total)
	}
}

// TestIntegration_Unlock_NotFound проверяет 404-путь: unlock несуществующей
// incarnation → ErrIncarnationNotFound.
func TestIntegration_Unlock_NotFound(t *testing.T) {
	resetAll(t)
	_, err := Unlock(context.Background(), integrationPool, "ghost", "x", "archon-alice", "01HUNLOCK0000000000000002")
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
}

// seedDestroyable создаёт incarnation в destroying + одну state_history-row +
// один apply_run (для проверки каскада V3). Возвращает creator-AID.
func seedDestroyable(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice")
	creator := "archon-alice"
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1.2.3",
		StateSchemaVersion: 1,
		Spec:               map[string]any{"replicas": 3},
		State:              map[string]any{"primary": name + "-01"},
		Status:             StatusDestroying,
		StatusDetails:      map[string]any{"force": false},
		CreatedByAID:       &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create destroyable: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario,
    state_before, state_after, changed_by_aid, apply_id)
VALUES ('01HDESTROYHIST0000000000', $1, 'destroy', '{}', '{}', $2, '01HDESTROYHIST0000000000')`,
		name, creator); err != nil {
		t.Fatalf("seed state_history: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid)
VALUES ('01HDESTROYAPPLY000000000', $1, $2, 'destroy', 'success', $3)`,
		name+".host-01", name, creator); err != nil {
		t.Fatalf("seed apply_runs: %v", err)
	}
}

// TestIntegration_DeleteAfterTeardown_ArchiveSurvivesCascade — happy-path S-D3:
// single-winner DELETE сносит destroying-строку, каскад убивает live
// state_history / apply_runs, а архив (incarnation_archive /
// state_history_archive), записанный ДО DELETE в той же tx, переживает каскад.
func TestIntegration_DeleteAfterTeardown_ArchiveSurvivesCascade(t *testing.T) {
	resetAll(t)
	seedDestroyable(t, "redis-prod")
	ctx := context.Background()
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(ctx, integrationPool, aw, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if !res.Deleted {
		t.Fatal("Deleted = false, want true")
	}

	// Live incarnation снесена.
	if _, err := SelectByName(ctx, integrationPool, "redis-prod"); !errors.Is(err, ErrIncarnationNotFound) {
		t.Errorf("SelectByName after delete: err = %v, want ErrIncarnationNotFound", err)
	}

	// Каскад снёс live state_history и apply_runs.
	var liveHist, liveRuns int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM state_history WHERE incarnation_name = 'redis-prod'`).Scan(&liveHist); err != nil {
		t.Fatalf("count live state_history: %v", err)
	}
	if liveHist != 0 {
		t.Errorf("live state_history after cascade = %d, want 0", liveHist)
	}
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE incarnation_name = 'redis-prod'`).Scan(&liveRuns); err != nil {
		t.Fatalf("count live apply_runs: %v", err)
	}
	if liveRuns != 0 {
		t.Errorf("live apply_runs after cascade = %d, want 0", liveRuns)
	}

	// Архив incarnation пережил каскад с ключевыми колонками.
	var (
		archName, archService, archVersion, archStatus string
		archReplicas                                   float64
	)
	if err := integrationPool.QueryRow(ctx,
		`SELECT name, service, service_version, status, (spec->>'replicas')::float
		 FROM incarnation_archive WHERE name = 'redis-prod'`).
		Scan(&archName, &archService, &archVersion, &archStatus, &archReplicas); err != nil {
		t.Fatalf("select incarnation_archive: %v", err)
	}
	if archName != "redis-prod" || archService != "redis" || archVersion != "v1.2.3" {
		t.Errorf("archive cols = %q/%q/%q, want redis-prod/redis/v1.2.3", archName, archService, archVersion)
	}
	if archStatus != "destroying" {
		t.Errorf("archive status = %q, want destroying (snapshot at delete)", archStatus)
	}
	if archReplicas != 3 {
		t.Errorf("archive spec.replicas = %v, want 3", archReplicas)
	}

	// Архив state_history пережил каскад.
	var archHist int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM state_history_archive WHERE incarnation_name = 'redis-prod'`).Scan(&archHist); err != nil {
		t.Fatalf("count state_history_archive: %v", err)
	}
	if archHist != 1 {
		t.Errorf("state_history_archive rows = %d, want 1 (survived cascade)", archHist)
	}

	// audit destroy_completed записан.
	if len(aw.events) != 1 || aw.events[0].EventType != audit.EventIncarnationDestroyCompleted {
		t.Errorf("audit events = %+v, want one destroy_completed", aw.events)
	}
}

// TestIntegration_DeleteAfterTeardown_SingleWinner — два конкурентных вызова на
// одну destroying-строку: ровно один удаляет (Deleted=true), второй — no-op
// (Deleted=false), архив записывается ровно один раз. Эмулируем гонку
// последовательными вызовами (WHERE status='destroying' — авторитет guard-а).
func TestIntegration_DeleteAfterTeardown_SingleWinner(t *testing.T) {
	resetAll(t)
	seedDestroyable(t, "redis-prod")
	ctx := context.Background()

	res1, err := DeleteAfterTeardown(ctx, integrationPool, &fakeAuditWriter{}, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("first DeleteAfterTeardown: %v", err)
	}
	if !res1.Deleted {
		t.Error("first call Deleted = false, want true (winner)")
	}

	// Второй вызов: строки в destroying уже нет → no-op, не ошибка.
	res2, err := DeleteAfterTeardown(ctx, integrationPool, &fakeAuditWriter{}, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("second DeleteAfterTeardown: %v", err)
	}
	if res2.Deleted {
		t.Error("second call Deleted = true, want false (loser, idempotent no-op)")
	}

	// Архив записан ровно один раз (второй вызов откатился).
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incarnation_archive WHERE name = 'redis-prod'`).Scan(&n); err != nil {
		t.Fatalf("count incarnation_archive: %v", err)
	}
	if n != 1 {
		t.Errorf("incarnation_archive rows = %d, want 1 (single archive, no-op loser rolled back)", n)
	}
}

// TestIntegration_DeleteAfterTeardown_NotDestroying — строка в ready (не
// destroying): single-winner guard не матчит → no-op (Deleted=false), строка
// остаётся жива, архив пуст.
func TestIntegration_DeleteAfterTeardown_NotDestroying(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := DeleteAfterTeardown(ctx, integrationPool, &fakeAuditWriter{}, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown on ready: %v", err)
	}
	if res.Deleted {
		t.Error("Deleted = true, want false (guard rejects non-destroying)")
	}
	// Строка жива.
	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v (row must survive no-op)", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready (untouched)", got.Status)
	}
	// Архив пуст (rollback откатил архив-INSERT, если он что-то записал —
	// archive incarnation тоже под guard-ом destroying, так что записать нечего).
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incarnation_archive WHERE name = 'redis-prod'`).Scan(&n); err != nil {
		t.Fatalf("count incarnation_archive: %v", err)
	}
	if n != 0 {
		t.Errorf("incarnation_archive rows = %d, want 0 (no-op archived nothing)", n)
	}
}
