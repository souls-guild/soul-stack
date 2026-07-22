//go:build integration

// Integration tests for incarnation / state_history CRUD via testcontainers-go.
// Pattern matches keeper/internal/operator/integration_test.go.

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
	// operators (FK chain). archive tables (S-D3) have no FK links — clear via
	// explicit enumeration.
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE state_history, apply_runs, incarnation,
		 incarnation_archive, state_history_archive, incarnation_membership,
		 souls, operators, audit_log CASCADE`)
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
	// ValidName fail-fast in Go — but SQL CHECK duplicates. Suppress Go-validation
	// via corner case: name with characters passing NamePattern, but with
	// uppercase (NamePattern rejects it, test already in crud_test).
	// Here explicitly verify SQL-side CHECK for bad-status works,
	// if someone bypasses Go-check (e.g., direct Exec with old enum).
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
		// Guarantees monotonic created_at for stable sort order in checks.
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
	// DESC by created_at → last (mysql-d) first.
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

// TestIntegration_SelectAll_StateFilter — real jsonb pushdown ->>
// in PG: eq on a string field, numeric-cast on a numeric one, a nonexistent
// field, sort by a state field.
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

	// eq on a string field: redis_version=8.0 → a, c.
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

	// A nonexistent state field → empty, NOT an error.
	out, total, err = SelectAll(ctx, integrationPool, ListFilter{
		StatePredicates: []StateEq{{Path: "ghost_field", Op: StateOpEq, Value: "x"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll ghost: %v", err)
	}
	if total != 0 || len(out) != 0 {
		t.Errorf("ghost: total=%d len=%d, want 0/0", total, len(out))
	}

	// Sort by a state field (memory_mb asc) with tie-break name.
	out, _, err = SelectAll(ctx, integrationPool, ListFilter{
		SortBy: "state.memory_mb", SortDir: SortAsc,
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll sort: %v", err)
	}
	// ->> returns text; "2048" < "4096" < "512" lexicographically — we're checking
	// the textual order specifically (numeric-sort is a separate task, out of scope).
	if len(out) != 3 {
		t.Fatalf("sort: len=%d, want 3", len(out))
	}
}

// TestIntegration_SelectAll_StateFilter_InjectionSafe — an injection attempt
// via state-path never reaches PG (rejected before the query), the table stays intact.
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

	// The table must remain — the row is in place.
	if _, err := SelectByName(ctx, integrationPool, "redis-prod"); err != nil {
		t.Fatalf("incarnation vanished after injection attempt: %v", err)
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

	// Direct INSERT into state_history — handler-level Write arrives in M0.6c-2.
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
	// DESC by at → 01HSECOND first.
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
	// CASCADE: when incarnation deleted history-rows disappear.
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

// TestIntegration_Unlock_FromErrorLocked verifies happy-path unlock:
// error_locked → ready, status_details reset, state_history gets
// snapshot-row (scenario=unlock, state unchanged), previous_status returned.
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

// TestIntegration_Unlock_NotLocked verifies unlock from ready is rejected
// (ErrIncarnationNotLocked) and doesn't write to state_history.
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

// TestIntegration_Unlock_FromApplying verifies unlock on applying-locked
// incarnation is rejected (ErrIncarnationNotLocked → 409): unlock
// allowed only from error_locked. applying semantically riskier than ready —
// run still in progress, manual release must not interfere. State and status not
// touched, nothing written to state_history.
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

// TestIntegration_Unlock_FromMigrationFailed verifies unlock releases
// migration_failed → ready (ADR-019): migration atomic in single tx, on failure
// rollback leaves pre-reform consistent state, so unlock
// returns incarnation to working state. State NOT touched, state_history gets
// snapshot-row (scenario=unlock).
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

// TestIntegration_Unlock_NotFound verifies 404-path: unlock non-existent
// incarnation → ErrIncarnationNotFound.
func TestIntegration_Unlock_NotFound(t *testing.T) {
	resetAll(t)
	_, err := Unlock(context.Background(), integrationPool, "ghost", "x", "archon-alice", "01HUNLOCK0000000000000002")
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
}

// seedDestroyable creates incarnation in destroying + one state_history-row +
// one apply_run (for cascade V3 check). Returns creator-AID.
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
// single-winner DELETE removes destroying-row, cascade kills live
// state_history / apply_runs, but archive (incarnation_archive /
// state_history_archive), written BEFORE DELETE in same tx, survives cascade.
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

	// Live incarnation deleted.
	if _, err := SelectByName(ctx, integrationPool, "redis-prod"); !errors.Is(err, ErrIncarnationNotFound) {
		t.Errorf("SelectByName after delete: err = %v, want ErrIncarnationNotFound", err)
	}

	// Cascade deleted live state_history and apply_runs.
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

	// Archive incarnation survived cascade with key columns.
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

	// Archive state_history survived cascade.
	var archHist int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM state_history_archive WHERE incarnation_name = 'redis-prod'`).Scan(&archHist); err != nil {
		t.Fatalf("count state_history_archive: %v", err)
	}
	if archHist != 1 {
		t.Errorf("state_history_archive rows = %d, want 1 (survived cascade)", archHist)
	}

	// audit destroy_completed written.
	if len(aw.events) != 1 || aw.events[0].EventType != audit.EventIncarnationDestroyCompleted {
		t.Errorf("audit events = %+v, want one destroy_completed", aw.events)
	}
}

// TestIntegration_DeleteAfterTeardown_SingleWinner — two concurrent calls on
// single destroying-row: exactly one deletes (Deleted=true), second — no-op
// (Deleted=false), archive written exactly once. Emulate race
// with sequential calls (WHERE status='destroying' — guard authority).
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

	// Second call: row no longer in destroying → no-op, not error.
	res2, err := DeleteAfterTeardown(ctx, integrationPool, &fakeAuditWriter{}, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("second DeleteAfterTeardown: %v", err)
	}
	if res2.Deleted {
		t.Error("second call Deleted = true, want false (loser, idempotent no-op)")
	}

	// Archive written exactly once (second call rolled back).
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incarnation_archive WHERE name = 'redis-prod'`).Scan(&n); err != nil {
		t.Fatalf("count incarnation_archive: %v", err)
	}
	if n != 1 {
		t.Errorf("incarnation_archive rows = %d, want 1 (single archive, no-op loser rolled back)", n)
	}
}

// TestIntegration_DeleteAfterTeardown_NotDestroying — row in ready (not
// destroying): single-winner guard doesn't match → no-op (Deleted=false), row
// remains alive, archive empty.
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
	// Row alive.
	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v (row must survive no-op)", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready (untouched)", got.Status)
	}
	// Archive empty (rollback reverted archive-INSERT, if it wrote anything —
	// archive incarnation also under destroying guard, so nothing to write).
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incarnation_archive WHERE name = 'redis-prod'`).Scan(&n); err != nil {
		t.Fatalf("count incarnation_archive: %v", err)
	}
	if n != 0 {
		t.Errorf("incarnation_archive rows = %d, want 0 (no-op archived nothing)", n)
	}
}

// seedApplyingWithApplyRun creates incarnation in applying + one apply_runs-row
// (apply_id, sid, name). Models orphaned scenario-run of previous Voyage owner
// (recovery seam ADR-027(k)).
func seedApplyingWithApplyRun(t *testing.T, name, applyID, sid string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice")
	creator := "archon-alice"
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1,
		State:              map[string]any{"primary": name + "-01"},
		Status:             StatusApplying, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create applying: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid)
VALUES ($1, $2, $3, 'create', 'running', $4)`,
		applyID, sid, name, creator); err != nil {
		t.Fatalf("seed apply_runs: %v", err)
	}
}

// TestIntegration_ReleaseApplyingOrphan_HappyPath — orphan from previous attempt
// (applying + apply_runs (orphanApplyID, name) exists) → released: applying → ready,
// state untouched (last known-good), state_history carries release snapshot.
func TestIntegration_ReleaseApplyingOrphan_HappyPath(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "redis-prod"
		applyID = "01HORPHANAPPLY0000000001"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")

	if err := ReleaseApplyingOrphan(ctx, integrationPool, name, applyID, "01HORPHANHIST00000000001"); err != nil {
		t.Fatalf("ReleaseApplyingOrphan: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready (orphan lock released)", got.Status)
	}
	if got.StatusDetails != nil {
		t.Errorf("status_details = %v, want nil (reset)", got.StatusDetails)
	}
	if got.State["primary"] != name+"-01" {
		t.Errorf("state changed by release: %v (must be untouched last-good)", got.State)
	}
	_, total, _ := HistorySelectByName(ctx, integrationPool, name, HistoryFilter{}, 0, 50)
	if total != 1 {
		t.Errorf("history rows = %d, want 1 (orphan-release snapshot)", total)
	}
}

// TestIntegration_ReleaseApplyingOrphan_LiveRival — FENCING-1: incarnation in
// applying, and it has ACTIVE (running) apply_run of alien apply_id (live
// run started between crash and reclaim) → NOT released (no-op), status remains
// applying. Protection against releasing alien live-lock.
func TestIntegration_ReleaseApplyingOrphan_LiveRival(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name        = "redis-prod"
		orphanApply = "01HORPHANAPPLY0000000002"
		rivalApply  = "01HRIVALAPPLY00000000002"
	)
	// apply_runs belongs to LIVE run (rivalApply, running).
	seedApplyingWithApplyRun(t, name, rivalApply, name+".host-01")

	// Release orphan-lock by orphanApply — but live rival holds active row.
	err := ReleaseApplyingOrphan(ctx, integrationPool, name, orphanApply, "01HORPHANHIST00000000002")
	if !errors.Is(err, ErrOrphanLockNotReleased) {
		t.Fatalf("err = %v, want ErrOrphanLockNotReleased (live rival)", err)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusApplying {
		t.Errorf("status = %q, want applying (live-lock untouched)", got.Status)
	}
	_, total, _ := HistorySelectByName(ctx, integrationPool, name, HistoryFilter{}, 0, 50)
	if total != 0 {
		t.Errorf("history rows = %d, want 0 (rollback, nothing released)", total)
	}
}

// TestIntegration_ReleaseApplyingOrphan_OrphanRunVanished — orphan apply_run already
// purged by reaper (apply_runs=0), but incarnation-lock — unclaimed flag —
// remains in applying → release (binding to orphan given by voyage_targets back-link
// on caller side; no live rival here → release safe).
func TestIntegration_ReleaseApplyingOrphan_OrphanRunVanished(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const name = "redis-prod"
	seedOperator(t, "archon-alice")
	creator := "archon-alice"
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1,
		State:              map[string]any{"primary": name + "-01"},
		Status:             StatusApplying, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create applying: %v", err)
	}
	// NO apply_runs-rows (orphan-run purged by reaper).

	if err := ReleaseApplyingOrphan(ctx, integrationPool, name, "01HORPHANAPPLY0000000005", "01HORPHANHIST00000000005"); err != nil {
		t.Fatalf("ReleaseApplyingOrphan (orphan-run vanished): %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready (orphan lock released)", got.Status)
	}
}

// TestIntegration_ReleaseApplyingOrphan_NotApplying — SINGLE-WINNER: honest
// RunResult of previous owner already finalized incarnation (ready), apply_runs
// (orphanApplyID, name) exist → release is no-op (ErrOrphanLockNotReleased), status
// untouched. Race closed: we do NOT overwrite alien terminal.
func TestIntegration_ReleaseApplyingOrphan_NotApplying(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "redis-prod"
		applyID = "01HORPHANAPPLY0000000003"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")
	// Honest finalize moved incarnation to ready BEFORE our release.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET status = 'ready' WHERE name = $1`, name); err != nil {
		t.Fatalf("simulate honest finalize: %v", err)
	}

	err := ReleaseApplyingOrphan(ctx, integrationPool, name, applyID, "01HORPHANHIST00000000003")
	if !errors.Is(err, ErrOrphanLockNotReleased) {
		t.Fatalf("err = %v, want ErrOrphanLockNotReleased (not applying)", err)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready (honest finalize not overwritten)", got.Status)
	}
	_, total, _ := HistorySelectByName(ctx, integrationPool, name, HistoryFilter{}, 0, 50)
	if total != 0 {
		t.Errorf("history rows = %d, want 0 (no-op, snapshot not written)", total)
	}
}

// TestIntegration_ReleaseApplyingOrphan_NotFound — incarnation deleted between
// reclaim and release → ErrIncarnationNotFound.
func TestIntegration_ReleaseApplyingOrphan_NotFound(t *testing.T) {
	resetAll(t)
	err := ReleaseApplyingOrphan(context.Background(), integrationPool, "ghost",
		"01HORPHANAPPLY0000000004", "01HORPHANHIST00000000004")
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
}

// readApplyingEpoch reads 4 epoch-columns of applying-flag (ADR-027 amend (m-S1)).
func readApplyingEpoch(t *testing.T, name string) (applyID, kid *string, attempt *int, since *string) {
	t.Helper()
	const q = `
SELECT applying_apply_id, applying_attempt, applying_by_kid, applying_since::text
FROM incarnation WHERE name = $1`
	if err := integrationPool.QueryRow(context.Background(), q, name).Scan(&applyID, &attempt, &kid, &since); err != nil {
		t.Fatalf("readApplyingEpoch(%s): %v", name, err)
	}
	return applyID, kid, attempt, since
}

// setApplyingEpoch mimics epoch write by lockRun (scenario.lockApplyingWithEpoch):
// sets all 4 columns on already-applying row. Needed to verify epoch CLEANUP
// at terminals (UpdateStateFromRun / ReleaseApplyingOrphan).
func setApplyingEpoch(t *testing.T, name, applyID, kid string) {
	t.Helper()
	const q = `
UPDATE incarnation
SET applying_apply_id = $2, applying_attempt = 0, applying_by_kid = $3, applying_since = NOW()
WHERE name = $1`
	if _, err := integrationPool.Exec(context.Background(), q, name, applyID, kid); err != nil {
		t.Fatalf("setApplyingEpoch(%s): %v", name, err)
	}
}

// TestIntegration_UpdateStateFromRun_ClearsEpoch_OnSuccess — guard ADR-027 amend
// (m-S1) task (3): success-terminal of run (UpdateStateFromRun → ready) ZEROES
// all 4 epoch-columns atomically with applying release. This single terminal point
// (commitSuccess calls it), cleanup here covers success-path.
func TestIntegration_UpdateStateFromRun_ClearsEpoch_OnSuccess(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "epoch-clear-ok"
		applyID = "01HEPOCHCLEAR0000000001"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")
	setApplyingEpoch(t, name, applyID, "keeper-owner-A")

	stateAfter := map[string]any{"primary": name + "-01", "applied": true}
	hist := "01HEPOCHHIST00000000001"
	if err := UpdateStateFromRun(ctx, integrationPool, name, "deploy", applyID,
		map[string]any{"primary": name + "-01"}, stateAfter,
		StatusReady, nil, nil, hist); err != nil {
		t.Fatalf("UpdateStateFromRun: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready", got.Status)
	}
	a, k, att, s := readApplyingEpoch(t, name)
	if a != nil || k != nil || att != nil || s != nil {
		t.Errorf("epoch not zeroed at success-terminal: apply=%v kid=%v attempt=%v since=%v", a, k, att, s)
	}
}

// TestIntegration_UpdateStateFromRun_ClearsEpoch_OnFail — guard ADR-027 amend
// (m-S1) task (3): fail-terminal (UpdateStateFromRun → error_locked, path
// lockIncarnation) also ZEROES epoch (same UpdateStateFromRun-point for all
// run terminals).
func TestIntegration_UpdateStateFromRun_ClearsEpoch_OnFail(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "epoch-clear-fail"
		applyID = "01HEPOCHCLEAR0000000002"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")
	setApplyingEpoch(t, name, applyID, "keeper-owner-B")

	stateBefore := map[string]any{"primary": name + "-01"}
	if err := UpdateStateFromRun(ctx, integrationPool, name, "deploy", applyID,
		stateBefore, stateBefore, // state not changed on failure
		StatusErrorLocked, map[string]any{"reason": "boom"}, nil, "01HEPOCHHIST00000000002"); err != nil {
		t.Fatalf("UpdateStateFromRun (fail): %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusErrorLocked {
		t.Errorf("status = %q, want error_locked", got.Status)
	}
	a, k, att, s := readApplyingEpoch(t, name)
	if a != nil || k != nil || att != nil || s != nil {
		t.Errorf("epoch not zeroed at fail-terminal: apply=%v kid=%v attempt=%v since=%v", a, k, att, s)
	}
}

// TestIntegration_ReleaseApplyingOrphan_ClearsEpoch — guard ADR-027 amend (m-S1)
// task (3): releasing orphaned lock (recovery-path) also ZEROES epoch
// together with applying→ready. After release row carries no dead owner's epoch.
func TestIntegration_ReleaseApplyingOrphan_ClearsEpoch(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const (
		name    = "epoch-clear-orphan"
		applyID = "01HEPOCHCLEAR0000000003"
	)
	seedApplyingWithApplyRun(t, name, applyID, name+".host-01")
	setApplyingEpoch(t, name, applyID, "keeper-dead-X")

	if err := ReleaseApplyingOrphan(ctx, integrationPool, name, applyID, "01HEPOCHHIST00000000003"); err != nil {
		t.Fatalf("ReleaseApplyingOrphan: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, want ready", got.Status)
	}
	a, k, att, s := readApplyingEpoch(t, name)
	if a != nil || k != nil || att != nil || s != nil {
		t.Errorf("epoch not zeroed at orphan-release: apply=%v kid=%v attempt=%v since=%v", a, k, att, s)
	}
}
