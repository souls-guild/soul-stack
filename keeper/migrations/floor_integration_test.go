//go:build integration

package migrations_test

// Integration test for migration 068 (ADR-046 Pass B, floor limit): on a
// clean DB the migration applies (CHECK + index in place); on a DB with an
// already-inserted sub-30 cadence, the pre-flight data guard RAISEs with a
// clear message (fail-fast, NOT a silent UPDATE). Under testcontainers-PG,
// build tag `integration`.
//
// The step-by-step apply (Migrate(67) -> seed -> Steps(1)) is built directly
// via golang-migrate: migrate.Apply applies ALL migrations at once, and we
// need to wedge in between 067 and 068 to seed the sub-30 row before the
// floor CHECK in 068 would reject it on INSERT.

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/migrations"
)

func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

// freshContainer spins up a clean PG testcontainer and returns DSN + teardown.
// When docker is unavailable and REQUIRE_DOCKER isn't set -- skip (parity
// with cadence integration).
func freshContainer(t *testing.T) (dsn string, teardown func()) {
	t.Helper()
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
			log.Fatalf("migrations integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	d, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}
	return d, func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}
}

// newMigrator builds *migrate.Migrate over the embedded FS and a pgx5 URL.
func newMigrator(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs source: %v", err)
	}
	url := "pgx5://" + strings.TrimPrefix(strings.TrimPrefix(dsn, "postgresql://"), "postgres://")
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		t.Fatalf("migrate instance: %v", err)
	}
	return m
}

// TestMigration068_CleanDB_Applies -- clean DB: all migrations (including
// 068) apply; the floor CHECK and MIN index are present in the schema.
func TestMigration068_CleanDB_Applies(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Up on a clean DB: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	var checkExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conname = 'cadences_interval_seconds_floor'
		)`).Scan(&checkExists); err != nil {
		t.Fatalf("query constraint: %v", err)
	}
	if !checkExists {
		t.Error("floor CHECK cadences_interval_seconds_floor is missing after the migration")
	}

	var idxExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_indexes
			WHERE indexname = 'cadences_enabled_interval_idx'
		)`).Scan(&idxExists); err != nil {
		t.Fatalf("query index: %v", err)
	}
	if !idxExists {
		t.Error("MIN index cadences_enabled_interval_idx is missing after the migration")
	}
}

// TestMigration068_SubFloorRow_RaisesGuard -- a DB with an already-inserted
// sub-30 cadence (e.g. a dev stand with a 10s cadence): apply migrations up
// to 067, seed interval=10, then Steps(1) (=068) -> the pre-flight data
// guard RAISEs. We check for a clear error message (minimum 30s / Beacons),
// NOT a silent UPDATE.
func TestMigration068_SubFloorRow_RaisesGuard(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	defer m.Close()

	// Apply all migrations UP TO 068 (067 is the last one before the floor limit).
	if err := m.Migrate(67); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(67): %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}

	// Owner Archon (FK cadences.created_by_aid).
	if _, err := pool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		 VALUES ('archon-floor-it', 'Floor IT', 'jwt', NULL)
		 ON CONFLICT (aid) DO NOTHING`); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	// Sub-30 interval cadence (before the floor CHECK in 068 -- the INSERT
	// passes the positive CHECK).
	if _, err := pool.Exec(ctx, `
		INSERT INTO cadences (
			id, name, schedule_kind, interval_seconds, overlap_policy,
			kind, scenario_name, target, created_by_aid
		) VALUES (
			'01H000000000000000000FLOOR', 'dev-10s', 'interval', 10, 'skip',
			'scenario', 'converge', '{"coven":"prod"}'::jsonb, 'archon-floor-it'
		)`); err != nil {
		t.Fatalf("seed sub-floor cadence: %v", err)
	}
	pool.Close()

	// Steps(1) applies 068 -> the pre-flight DO guard RAISEs.
	err = m.Steps(1)
	if err == nil {
		t.Fatal("expected a RAISE on the sub-30 row, but migration 068 succeeded")
	}
	if !strings.Contains(err.Error(), "interval_seconds < 30") {
		t.Errorf("migration error should carry a clear data-guard message; got: %v", err)
	}
}
