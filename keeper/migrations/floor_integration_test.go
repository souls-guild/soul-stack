//go:build integration

package migrations_test

// Integration-тест миграции 068 (ADR-046 Pass B, floor-лимит): чистая БД —
// миграция применяется (CHECK + индекс на месте); БД с уже-вставленной суб-30
// cadence — pre-flight data-guard RAISE-ит с понятным текстом (fail-fast, НЕ
// тихий UPDATE). Под testcontainers-PG, build-tag `integration`.
//
// Пошаговый apply (Migrate(67) → seed → Steps(1)) строится напрямую через
// golang-migrate: migrate.Apply применяет ВСЕ миграции разом, а нам нужно
// вклиниться между 067 и 068, чтобы засеять суб-30 строку до того, как
// floor-CHECK 068 её бы отверг на INSERT.

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

// freshContainer поднимает чистый PG-testcontainer и возвращает DSN + teardown.
// При недоступном docker и без REQUIRE_DOCKER — skip (parity cadence integration).
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

// newMigrator строит *migrate.Migrate поверх embedded FS и pgx5-URL.
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

// TestMigration068_CleanDB_Applies — чистая БД: все миграции (включая 068)
// применяются; floor-CHECK и MIN-индекс присутствуют в схеме.
func TestMigration068_CleanDB_Applies(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Up на чистой БД: %v", err)
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
		t.Error("floor-CHECK cadences_interval_seconds_floor отсутствует после миграции")
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
		t.Error("MIN-индекс cadences_enabled_interval_idx отсутствует после миграции")
	}
}

// TestMigration068_SubFloorRow_RaisesGuard — БД с уже-вставленной суб-30 cadence
// (напр. dev-стенд с 10s-cadence): применяем миграции до 067, сеем interval=10,
// затем Steps(1) (=068) → pre-flight data-guard RAISE-ит. Проверяем понятный
// текст ошибки (минимум 30s / Beacons), НЕ тихий UPDATE.
func TestMigration068_SubFloorRow_RaisesGuard(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	defer m.Close()

	// Применяем все миграции ДО 068 (067 — последняя перед floor-лимитом).
	if err := m.Migrate(67); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(67): %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}

	// Архонт-владелец (FK cadences.created_by_aid).
	if _, err := pool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		 VALUES ('archon-floor-it', 'Floor IT', 'jwt', NULL)
		 ON CONFLICT (aid) DO NOTHING`); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	// Суб-30 interval-cadence (до floor-CHECK 068 — INSERT проходит positive-CHECK).
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

	// Steps(1) применяет 068 → pre-flight DO-guard RAISE-ит.
	err = m.Steps(1)
	if err == nil {
		t.Fatal("ожидался RAISE на суб-30 строке, но миграция 068 прошла")
	}
	if !strings.Contains(err.Error(), "interval_seconds < 30") {
		t.Errorf("ошибка миграции должна нести понятный текст data-guard; got: %v", err)
	}
}
