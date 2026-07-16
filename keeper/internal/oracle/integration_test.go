//go:build integration

// Integration tests for the Oracle registries (vigils / decrees / oracle_fires) via
// testcontainers-go. The pattern matches keeper/internal/augur/integration_test.go.

package oracle

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/migrations"
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
		if os.Getenv("REQUIRE_DOCKER") != "" {
			log.Fatalf("oracle integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("oracle integration: skipping, docker unavailable: %v", err)
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
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE oracle_fires, decrees, vigils, operators CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func TestIntegration_SelectActiveVigilsForSubject(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"

	// coven-Vigil (web), sid-Vigil (host-a), disabled-Vigil (web), and an
	// unrelated coven-Vigil (db).
	mustInsertVigil(t, &Vigil{Name: "web-watch", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})
	mustInsertVigil(t, &Vigil{Name: "host-watch", SID: strptr("host-a.example.com"), IntervalSpec: "1m", CheckAddr: "core.beacon.file_changed", Enabled: true, CreatedByAID: &aid})
	mustInsertVigil(t, &Vigil{Name: "web-disabled", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: false, CreatedByAID: &aid})
	mustInsertVigil(t, &Vigil{Name: "db-watch", Coven: []string{"db"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})

	got, err := SelectActiveVigilsForSubject(ctx, integrationPool, "host-a.example.com", []string{"web", "prod"})
	if err != nil {
		t.Fatalf("SelectActiveVigilsForSubject: %v", err)
	}
	gotNames := map[string]bool{}
	for _, v := range got {
		gotNames[v.Name] = true
	}
	// host-watch (sid) + web-watch (coven), not db-watch, not web-disabled.
	if !gotNames["host-watch"] || !gotNames["web-watch"] {
		t.Errorf("ожидали host-watch + web-watch, got %v", gotNames)
	}
	if gotNames["db-watch"] || gotNames["web-disabled"] {
		t.Errorf("db-watch/web-disabled не должны попасть, got %v", gotNames)
	}
}

func TestIntegration_SelectDecreesByBeacon(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"

	mustInsertVigil(t, &Vigil{Name: "svc-down", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})
	mustInsertDecree(t, &Decree{Name: "restart-web", OnBeacon: "svc-down", SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true, CreatedByAID: &aid})
	mustInsertDecree(t, &Decree{Name: "disabled-rule", OnBeacon: "svc-down", SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: false, CreatedByAID: &aid})

	got, err := SelectDecreesByBeacon(ctx, integrationPool, "svc-down")
	if err != nil {
		t.Fatalf("SelectDecreesByBeacon: %v", err)
	}
	if len(got) != 1 || got[0].Name != "restart-web" {
		t.Fatalf("ожидали 1 enabled-Decree restart-web, got %+v", got)
	}
	if got[0].IncarnationName != "web-app" {
		t.Errorf("incarnation_name round-trip: got %q, want web-app", got[0].IncarnationName)
	}

	none, err := SelectDecreesByBeacon(ctx, integrationPool, "other-beacon")
	if err != nil {
		t.Fatalf("SelectDecreesByBeacon(other): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("default-deny: ожидали 0 Decree для несвязанного beacon, got %d", len(none))
	}
}

func TestIntegration_CooldownUpsert(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"
	mustInsertVigil(t, &Vigil{Name: "svc-down", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})
	mustInsertDecree(t, &Decree{Name: "restart-web", OnBeacon: "svc-down", SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true, CreatedByAID: &aid})

	// Before the first fire — the pair doesn't exist.
	_, has, err := LastFiredAt(ctx, integrationPool, "restart-web", "host-a")
	if err != nil {
		t.Fatalf("LastFiredAt: %v", err)
	}
	if has {
		t.Fatal("пара не должна существовать до первого fire")
	}

	t1 := time.Now().UTC().Truncate(time.Second)
	if err := RecordFire(ctx, integrationPool, "restart-web", "host-a", t1); err != nil {
		t.Fatalf("RecordFire #1: %v", err)
	}
	got, has, err := LastFiredAt(ctx, integrationPool, "restart-web", "host-a")
	if err != nil || !has {
		t.Fatalf("LastFiredAt после fire: has=%v err=%v", has, err)
	}
	if !got.Equal(t1) {
		t.Errorf("fired_at = %v, want %v", got, t1)
	}

	// UPSERT: a repeated RecordFire updates, it doesn't multiply rows.
	t2 := t1.Add(10 * time.Minute)
	if err := RecordFire(ctx, integrationPool, "restart-web", "host-a", t2); err != nil {
		t.Fatalf("RecordFire #2: %v", err)
	}
	got2, _, _ := LastFiredAt(ctx, integrationPool, "restart-web", "host-a")
	if !got2.Equal(t2) {
		t.Errorf("после UPSERT fired_at = %v, want %v", got2, t2)
	}
	var count int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM oracle_fires WHERE decree='restart-web' AND subject='host-a'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("UPSERT должен держать одну строку на пару, got %d", count)
	}
}

func TestIntegration_DecreeSubjectXOR(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"
	mustInsertVigil(t, &Vigil{Name: "svc-down", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})

	// Both subjects are set → CHECK decrees_subject_xor.
	err := InsertDecree(ctx, integrationPool, &Decree{
		Name: "bad", OnBeacon: "svc-down",
		SubjectCoven: []string{"web"}, SubjectSID: strptr("host-a"),
		IncarnationName: "web-app",
		ActionScenario:  "restart", Enabled: true, CreatedByAID: &aid,
	})
	if err == nil {
		t.Fatal("ожидали CHECK-violation на subject XOR")
	}
}

func TestIntegration_DecreeIncarnationNameFormat(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"
	mustInsertVigil(t, &Vigil{Name: "svc-down", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})

	// incarnation_name not matching the format (Upper) → CHECK decrees_incarnation_name_format.
	err := InsertDecree(ctx, integrationPool, &Decree{
		Name: "bad-inc", OnBeacon: "svc-down", SubjectCoven: []string{"web"},
		IncarnationName: "Web_App", ActionScenario: "restart", Enabled: true, CreatedByAID: &aid,
	})
	if err == nil {
		t.Fatal("ожидали CHECK-violation на incarnation_name format")
	}

	// Empty incarnation_name → NOT NULL (a Go string "" is written as ''; the CHECK
	// format requires ≥1 character) → rejected.
	err = InsertDecree(ctx, integrationPool, &Decree{
		Name: "empty-inc", OnBeacon: "svc-down", SubjectCoven: []string{"web"},
		IncarnationName: "", ActionScenario: "restart", Enabled: true, CreatedByAID: &aid,
	})
	if err == nil {
		t.Fatal("ожидали отказ на пустой incarnation_name")
	}
}

func TestIntegration_OracleFireCascade(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"
	mustInsertVigil(t, &Vigil{Name: "svc-down", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})
	mustInsertDecree(t, &Decree{Name: "restart-web", OnBeacon: "svc-down", SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true, CreatedByAID: &aid})
	if err := RecordFire(ctx, integrationPool, "restart-web", "host-a", time.Now().UTC()); err != nil {
		t.Fatalf("RecordFire: %v", err)
	}

	// Deleting a Decree cleans up oracle_fires by cascade.
	if _, err := integrationPool.Exec(ctx, `DELETE FROM decrees WHERE name='restart-web'`); err != nil {
		t.Fatalf("delete decree: %v", err)
	}
	var count int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM oracle_fires`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("ON DELETE CASCADE: oracle_fires должны быть пусты, got %d", count)
	}
}

func newIntegrationService(t *testing.T) *Service {
	t.Helper()
	where, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	svc, err := NewService(ServiceDeps{Pool: integrationPool, Where: where})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestIntegration_VigilCRUD — Service create → get → list → delete round-trip
// against a real PG (S3 CRUD).
func TestIntegration_VigilCRUD(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"
	svc := newIntegrationService(t)

	v, err := svc.CreateVigil(ctx, CreateVigilInput{
		Name: "web-conf", Coven: []string{"web"}, Interval: "30s",
		Check: "core.beacon.file_changed", Enabled: true, CallerAID: &aid,
	})
	if err != nil {
		t.Fatalf("CreateVigil: %v", err)
	}
	if v.CreatedAt.IsZero() {
		t.Error("created_at не заполнен после round-trip-а")
	}

	got, err := svc.GetVigil(ctx, "web-conf")
	if err != nil {
		t.Fatalf("GetVigil: %v", err)
	}
	if got.CheckAddr != "core.beacon.file_changed" || len(got.Coven) != 1 || got.Coven[0] != "web" {
		t.Errorf("get = %+v", got)
	}

	list, total, err := svc.ListVigils(ctx, 0, 50)
	if err != nil {
		t.Fatalf("ListVigils: %v", err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("list total=%d len=%d, want 1/1", total, len(list))
	}

	if err := svc.DeleteVigil(ctx, "web-conf"); err != nil {
		t.Fatalf("DeleteVigil: %v", err)
	}
	if _, err := svc.GetVigil(ctx, "web-conf"); !errors.Is(err, ErrVigilNotFound) {
		t.Errorf("после delete: %v, want ErrVigilNotFound", err)
	}

	// Duplicate → ErrVigilAlreadyExists.
	if _, err := svc.CreateVigil(ctx, CreateVigilInput{Name: "dup", Coven: []string{"web"}, Interval: "30s", Check: "core.beacon.file_changed", CallerAID: &aid}); err != nil {
		t.Fatalf("CreateVigil(dup #1): %v", err)
	}
	if _, err := svc.CreateVigil(ctx, CreateVigilInput{Name: "dup", Coven: []string{"web"}, Interval: "30s", Check: "core.beacon.file_changed", CallerAID: &aid}); !errors.Is(err, ErrVigilAlreadyExists) {
		t.Errorf("CreateVigil(dup #2) = %v, want ErrVigilAlreadyExists", err)
	}
}

// TestIntegration_DecreeCRUD — Service create (with where-CEL) → get → list →
// delete round-trip against a real PG.
func TestIntegration_DecreeCRUD(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	aid := "archon-test"
	svc := newIntegrationService(t)
	mustInsertVigil(t, &Vigil{Name: "svc-down", Coven: []string{"db"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})

	where := `event.data.severity == "critical"`
	d, err := svc.CreateDecree(ctx, CreateDecreeInput{
		Name: "restart-on-down", OnBeacon: "svc-down", WhereCEL: &where,
		Coven: []string{"db"}, IncarnationName: "prod-db",
		ActionScenario: "restart_service", Cooldown: "5m", Enabled: true, CallerAID: &aid,
	})
	if err != nil {
		t.Fatalf("CreateDecree: %v", err)
	}
	if d.WhereCEL == nil || *d.WhereCEL != where {
		t.Errorf("where-CEL round-trip: %+v", d.WhereCEL)
	}

	got, err := svc.GetDecree(ctx, "restart-on-down")
	if err != nil {
		t.Fatalf("GetDecree: %v", err)
	}
	if got.IncarnationName != "prod-db" || got.ActionScenario != "restart_service" {
		t.Errorf("get = %+v", got)
	}

	_, total, err := svc.ListDecrees(ctx, 0, 50)
	if err != nil {
		t.Fatalf("ListDecrees: %v", err)
	}
	if total != 1 {
		t.Errorf("list total=%d, want 1", total)
	}

	if err := svc.DeleteDecree(ctx, "restart-on-down"); err != nil {
		t.Fatalf("DeleteDecree: %v", err)
	}
	if _, err := svc.GetDecree(ctx, "restart-on-down"); !errors.Is(err, ErrDecreeNotFound) {
		t.Errorf("после delete: %v, want ErrDecreeNotFound", err)
	}
}

// seedCircuitDecree seeds a vigil + one enabled coven-Decree for the circuit-breaker
// tests and returns its name.
func seedCircuitDecree(t *testing.T) string {
	t.Helper()
	seedOperator(t, "archon-test")
	aid := "archon-test"
	mustInsertVigil(t, &Vigil{Name: "svc-down", Coven: []string{"web"}, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &aid})
	mustInsertDecree(t, &Decree{Name: "restart-web", OnBeacon: "svc-down", SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true, CreatedByAID: &aid})
	return "restart-web"
}

func circuitFireCount(t *testing.T, decree string) (int, bool) {
	t.Helper()
	var cnt int
	err := integrationPool.QueryRow(context.Background(),
		`SELECT fire_count FROM oracle_circuit WHERE decree = $1`, decree).Scan(&cnt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("circuitFireCount: %v", err)
	}
	return cnt, true
}

// TestIntegration_BumpCircuitFixedWindow — increment within the window and reset at
// the boundary (window_start ≤ now - window) (ADR-030(a), circuit-breaker S4).
func TestIntegration_BumpCircuitFixedWindow(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	decree := seedCircuitDecree(t)
	window := 10 * time.Minute

	t0 := time.Now().UTC().Truncate(time.Second)
	// Three bumps within the window — the counter grows 1→2→3.
	for i, want := range []int{1, 2, 3} {
		cnt, err := BumpCircuit(ctx, integrationPool, decree, t0.Add(time.Duration(i)*time.Minute), window)
		if err != nil {
			t.Fatalf("BumpCircuit #%d: %v", i, err)
		}
		if cnt != want {
			t.Errorf("bump #%d: fire_count=%d, want %d", i, cnt, want)
		}
	}

	// Bump past the window boundary (now strictly later than window_start + window) — reset to 1.
	// window_start stayed at t0 (the first bump in this window), the comparison threshold
	// window_start <= now - window is met exactly at the boundary when now = t0+window.
	afterWindow := t0.Add(window + time.Second)
	cnt, err := BumpCircuit(ctx, integrationPool, decree, afterWindow, window)
	if err != nil {
		t.Fatalf("BumpCircuit after window: %v", err)
	}
	if cnt != 1 {
		t.Errorf("после истечения окна fire_count должен сброситься в 1, got %d", cnt)
	}
	// window_start moved to afterWindow → the next bump grows again.
	cnt, err = BumpCircuit(ctx, integrationPool, decree, afterWindow.Add(time.Minute), window)
	if err != nil {
		t.Fatalf("BumpCircuit in new window: %v", err)
	}
	if cnt != 2 {
		t.Errorf("в новом окне fire_count=%d, want 2", cnt)
	}
}

// TestIntegration_BumpCircuitWindowBoundary — reset exactly at the boundary
// (window_start == now - window, the `<=` condition): the window is considered expired.
func TestIntegration_BumpCircuitWindowBoundary(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	decree := seedCircuitDecree(t)
	window := 10 * time.Minute

	t0 := time.Now().UTC().Truncate(time.Second)
	if _, err := BumpCircuit(ctx, integrationPool, decree, t0, window); err != nil {
		t.Fatalf("BumpCircuit #1: %v", err)
	}
	// now == window_start + window: window_start <= now - window holds → reset.
	cnt, err := BumpCircuit(ctx, integrationPool, decree, t0.Add(window), window)
	if err != nil {
		t.Fatalf("BumpCircuit at boundary: %v", err)
	}
	if cnt != 1 {
		t.Errorf("на границе окна (<=) счётчик должен сброситься в 1, got %d", cnt)
	}
}

// TestIntegration_TripDecreeSingleWinner — enabled true→false exactly once; the second
// TripDecree → RowsAffected==0 (tripped=false). single-winner invariant.
func TestIntegration_TripDecreeSingleWinner(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	decree := seedCircuitDecree(t)
	now := time.Now().UTC()

	tripped, err := TripDecree(ctx, integrationPool, decree, now)
	if err != nil {
		t.Fatalf("TripDecree #1: %v", err)
	}
	if !tripped {
		t.Fatal("первый TripDecree должен выиграть (enabled true→false)")
	}
	// Decree is now disabled.
	d, err := SelectDecreeByName(ctx, integrationPool, decree)
	if err != nil {
		t.Fatalf("SelectDecreeByName: %v", err)
	}
	if d.Enabled {
		t.Error("после TripDecree Decree должен быть disabled")
	}

	// A repeated TripDecree — already disabled, RowsAffected==0.
	tripped2, err := TripDecree(ctx, integrationPool, decree, now)
	if err != nil {
		t.Fatalf("TripDecree #2: %v", err)
	}
	if tripped2 {
		t.Error("повторный TripDecree должен проиграть (RowsAffected==0)")
	}
}

// TestIntegration_BumpCircuitConcurrent — cluster race: two concurrent
// BumpCircuit calls on one Decree (live PG) don't lose increments (final fire_count==2).
// Atomicity of the UPSERT under a row lock serializes read-modify-write.
func TestIntegration_BumpCircuitConcurrent(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	decree := seedCircuitDecree(t)
	window := 10 * time.Minute
	now := time.Now().UTC()

	var wg sync.WaitGroup
	results := make([]int, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // synchronous start of both goroutines to maximize the race
			results[idx], errs[idx] = BumpCircuit(ctx, integrationPool, decree, now, window)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("BumpCircuit goroutine #%d: %v", i, err)
		}
	}
	// Concurrent RETURNING values — {1,2} in some order, no increment is lost.
	if results[0]+results[1] != 3 {
		t.Errorf("конкурентные RETURNING fire_count = %v (сумма %d), want {1,2}", results, results[0]+results[1])
	}
	final, ok := circuitFireCount(t, decree)
	if !ok || final != 2 {
		t.Errorf("итоговый fire_count после 2 конкурентных bump = %d (есть=%v), want 2", final, ok)
	}
}

// TestIntegration_CircuitRecreateCascade — recreating a Decree (delete+insert) cleans up
// oracle_circuit by cascade (re-enable MVP = a clean window).
func TestIntegration_CircuitRecreateCascade(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	decree := seedCircuitDecree(t)
	if _, err := BumpCircuit(ctx, integrationPool, decree, time.Now().UTC(), 10*time.Minute); err != nil {
		t.Fatalf("BumpCircuit: %v", err)
	}
	if _, ok := circuitFireCount(t, decree); !ok {
		t.Fatal("после bump oracle_circuit-строка должна существовать")
	}

	// Delete Decree → cascade cleans up oracle_circuit.
	if err := DeleteDecree(ctx, integrationPool, decree); err != nil {
		t.Fatalf("DeleteDecree: %v", err)
	}
	if _, ok := circuitFireCount(t, decree); ok {
		t.Error("ON DELETE CASCADE: oracle_circuit-строка должна уйти с Decree")
	}
	var total int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM oracle_circuit`).Scan(&total); err != nil {
		t.Fatalf("count oracle_circuit: %v", err)
	}
	if total != 0 {
		t.Errorf("oracle_circuit должна быть пуста после delete, got %d", total)
	}

	// Recreate (same name) → the new Decree starts with a clean window.
	aid := "archon-test"
	mustInsertDecree(t, &Decree{Name: decree, OnBeacon: "svc-down", SubjectCoven: []string{"web"}, IncarnationName: "web-app", ActionScenario: "restart", Cooldown: "5m", Enabled: true, CreatedByAID: &aid})
	cnt, err := BumpCircuit(ctx, integrationPool, decree, time.Now().UTC(), 10*time.Minute)
	if err != nil {
		t.Fatalf("BumpCircuit after recreate: %v", err)
	}
	if cnt != 1 {
		t.Errorf("пересозданный Decree должен стартовать с fire_count=1, got %d", cnt)
	}
}

func mustInsertVigil(t *testing.T, v *Vigil) {
	t.Helper()
	if err := InsertVigil(context.Background(), integrationPool, v); err != nil {
		t.Fatalf("InsertVigil(%s): %v", v.Name, err)
	}
}

func mustInsertDecree(t *testing.T, d *Decree) {
	t.Helper()
	if err := InsertDecree(context.Background(), integrationPool, d); err != nil {
		t.Fatalf("InsertDecree(%s): %v", d.Name, err)
	}
}
