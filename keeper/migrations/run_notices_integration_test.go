//go:build integration

package migrations_test

// Integration test for migration 107 (NIM-237, run notices): the column appears
// on a clean DB, rolls back, and re-applies -- plus the two properties of the
// column itself that the append path depends on.
//
// NOT NULL DEFAULT '[]' is not cosmetic here. AppendRunNotices does a bare
// `notices || $1` with no COALESCE, and in Postgres `NULL || anything` is NULL --
// so a nullable column would make every notice of that run vanish silently. The
// probe below is what keeps that reasoning honest if someone relaxes the column.
//
// The BEHAVIOUR of the append (does it accumulate across separate writes, does
// the read collapse repeats) is tested in internal/applyrun against the real
// CRUD function and the real read projection, where apply_runs' FK to
// incarnation can be satisfied by the package's seed helpers. Re-testing it here
// would mean pasting a copy of the SQL into the test and asserting against the
// copy -- green even if the shipped statement drifted away from it.
//
// Shares freshContainer / newMigrator with floor_integration_test.go (same
// package). Under testcontainers-PG, build tag `integration`.

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func hasNoticesColumn(t *testing.T, pool *pgxpool.Pool, phase string) bool {
	t.Helper()
	const q = `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'apply_runs' AND column_name = 'notices')`
	var got bool
	if err := pool.QueryRow(context.Background(), q).Scan(&got); err != nil {
		t.Fatalf("%s: probe apply_runs.notices: %v", phase, err)
	}
	return got
}

// TestMigration107_AppliesAndRollsBack -- the migration's own acceptance
// criterion, mirroring TestMigration103: applies on a clean DB, its down removes
// the column, and it re-applies with no leftovers blocking a second up.
func TestMigration107_AppliesAndRollsBack(t *testing.T) {
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

	if !hasNoticesColumn(t, pool, "after up") {
		t.Fatal("after up: apply_runs.notices is absent")
	}

	// The two properties the append path relies on. A nullable column, or one
	// defaulting to anything but an empty array, turns `notices || $1` into a
	// silent no-op for every run inserted before its first notice.
	var nullable, colDefault, dataType string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable, COALESCE(column_default, ''), data_type
		 FROM information_schema.columns
		 WHERE table_name = 'apply_runs' AND column_name = 'notices'`,
	).Scan(&nullable, &colDefault, &dataType); err != nil {
		t.Fatalf("probe notices column shape: %v", err)
	}
	if nullable != "NO" {
		t.Errorf("is_nullable = %q, want %q - NULL || $1 is NULL, which would eat every notice", nullable, "NO")
	}
	if colDefault != "'[]'::jsonb" {
		t.Errorf("column_default = %q, want '[]'::jsonb - the append does no COALESCE", colDefault)
	}
	if dataType != "jsonb" {
		t.Errorf("data_type = %q, want jsonb", dataType)
	}

	// Absolute versions, not Steps(±1): the head moves with every migration the
	// release adds, and a relative step would roll back somebody else's.
	if err := m.Migrate(106); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(106) (down through 107): %v", err)
	}
	if hasNoticesColumn(t, pool, "after down") {
		t.Error("after down: apply_runs.notices survived the rollback")
	}

	if err := m.Migrate(107); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(107) (re-apply): %v", err)
	}
	if !hasNoticesColumn(t, pool, "after re-apply") {
		t.Error("after re-apply: apply_runs.notices is absent")
	}
}
