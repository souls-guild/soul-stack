//go:build integration

package migrations_test

// Integration test for migration 103 (ADR-0076(l), engine provenance): on a
// clean DB the four provenance columns appear; rolling back removes all of them
// and re-applying restores them. Provenance is facts-only, so the columns are
// nullable by construction -- the down/up cycle must not need any data fixup.
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

// engineProvenanceColumns -- the columns migration 103 owns, as EXISTS probes.
// Used in both directions: all true after up, all false after down.
var engineProvenanceColumns = map[string][2]string{
	"apply_runs.keeper_version":   {"apply_runs", "keeper_version"},
	"apply_runs.soul_version":     {"apply_runs", "soul_version"},
	"incarnation.engine_compat":   {"incarnation", "engine_compat"},
	"state_history.engine_compat": {"state_history", "engine_compat"},
}

func assertEngineProvenanceColumns(t *testing.T, pool *pgxpool.Pool, want bool, phase string) {
	t.Helper()
	const q = `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns
		WHERE table_name = $1 AND column_name = $2)`
	for label, tc := range engineProvenanceColumns {
		var got bool
		if err := pool.QueryRow(context.Background(), q, tc[0], tc[1]).Scan(&got); err != nil {
			t.Fatalf("%s: probe %s: %v", phase, label, err)
		}
		if got != want {
			t.Errorf("%s: %s present = %v, want %v", phase, label, got, want)
		}
	}
}

// TestMigration103_AppliesAndRollsBack -- the acceptance criterion for the
// migration itself: it applies on a clean DB, its down removes every column it
// created, and it re-applies afterwards (no leftovers blocking a second up).
// The stamp columns are asserted nullable specifically: a run predating the
// stamp -- or one whose renderer carried no version -- must still write its row.
func TestMigration103_AppliesAndRollsBack(t *testing.T) {
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

	assertEngineProvenanceColumns(t, pool, true, "after up")

	for label, tc := range engineProvenanceColumns {
		var nullable string
		if err := pool.QueryRow(ctx,
			`SELECT is_nullable FROM information_schema.columns
			 WHERE table_name = $1 AND column_name = $2`, tc[0], tc[1],
		).Scan(&nullable); err != nil {
			t.Fatalf("query nullability of %s: %v", label, err)
		}
		if nullable != "YES" {
			t.Errorf("%s is_nullable = %q, want %q", label, nullable, "YES")
		}
	}

	// Absolute versions, not Steps(±1): the head moves with every migration the
	// release adds, and a relative step would roll back somebody else's.
	if err := m.Migrate(102); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(102) (down through 103): %v", err)
	}
	assertEngineProvenanceColumns(t, pool, false, "after down")

	if err := m.Migrate(103); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(103) (re-apply 103): %v", err)
	}
	assertEngineProvenanceColumns(t, pool, true, "after re-apply")
}
