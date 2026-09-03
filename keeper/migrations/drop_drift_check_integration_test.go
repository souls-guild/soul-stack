//go:build integration

package migrations_test

// Integration test for migration 114 (NIM-446): the database half of removing
// the Scry drift-check circuit.
//
// It exists for the same reason migration 109's does, and the file that names
// 109 as its precedent should not have skipped it: three of the four blocks are
// plpgsql DO blocks whose entire content is *which rows they match*, and a unit
// test over the SQL text cannot execute a single statement. Two of the shapes
// below were found by review only after the text-only test passed:
//
//   - a `tidings` row whose event_types holds the removed type TWICE. Nothing
//     dedupes on the write path, an equality predicate misses it, and the
//     following array_remove then strips it to `{}` — violating
//     tidings_event_types_nonempty and ABORTING the migration. Because keeper
//     runs migrations at startup, an abort leaves the version dirty and every
//     later start failing: the same cluster-wide wedge block 2 exists to avoid.
//
//   - a permission string with stray whitespace. rbac_role_permissions stores
//     the RAW text and ParsePermission only normalizes on read, so
//     `" incarnation.check-drift"` is a storable grant that a literal predicate
//     misses — and a missed grant is the fail-closed startup outage itself.
//
// Shares freshContainer / newMigrator with the other files in this package.
// Under testcontainers-PG, build tag `integration`.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Absolute version, never relative Steps: a test that walks "one past the end"
// silently re-aims at whatever migration lands next.
const dropDriftCheckVersion = 114

// This file's SQL runs against the schema AS OF MIGRATION 113 — `m.Migrate` is
// called with `dropDriftCheckVersion - 1` before the fixture is seeded. So the
// identifier columns here are still spelled `name`, and they must STAY that way
// even though [ADR-0085] / NIM-729 renamed them in migration 118: a
// ladder-position test seeds at its own version, not at HEAD.
//
// seedDriftFixture writes every shape block 2/3/4 must tell apart.
func seedDriftFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		// --- RBAC grants ---
		`INSERT INTO rbac_roles (name, description) VALUES ('drift-only', ''), ('drift-mixed', ''), ('drift-none', '')`,

		`INSERT INTO rbac_role_permissions (role_name, permission) VALUES
		 ('drift-only', 'incarnation.check-drift'),
		 ('drift-mixed', 'incarnation.check-drift on coven=prod'),
		 ('drift-mixed', 'incarnation.run on coven=prod'),
		 ('drift-mixed', ' incarnation.check-drift'),
		 ('drift-mixed', 'incarnation.check-drift  on  service=redis'),
		 ('drift-none', 'incarnation.run'),
		 ('drift-none', 'incarnation.list')`,

		// --- Tidings (herald FK) ---
		`INSERT INTO heralds (name, type, config) VALUES ('ch', 'webhook', '{"url":"https://example.com/h"}'::jsonb)`,

		`INSERT INTO tidings (name, herald, event_types) VALUES
		 ('only-drift',      'ch', ARRAY['incarnation.drift_checked']),
		 ('only-drift-twice','ch', ARRAY['incarnation.drift_checked','incarnation.drift_checked']),
		 ('drift-plus',      'ch', ARRAY['scenario_run.*','incarnation.drift_checked','cadence.*']),
		 ('no-drift',        'ch', ARRAY['scenario_run.*'])`,

		// --- apply_runs (incarnation FK) ---
		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, state)
		 VALUES ('redis-prod', 'redis', 'v1', 1, 'ready', '{}'::jsonb)`,

		// A check-drift row the keeper never got to dispatch: the one that would
		// otherwise be claimed and APPLIED FOR REAL by the new binary.
		`INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, recipe)
		 VALUES ('01STALEDRYRUN0000000000000', 'host-a.example.com', 'redis-prod', 'converge', 'planned',
		         '{"dry_run":true,"scenario_name":"converge"}'::jsonb)`,

		// A perfectly ordinary queued run: must be left alone.
		`INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, recipe)
		 VALUES ('01NORMALPLANNED00000000000', 'host-b.example.com', 'redis-prod', 'create', 'planned',
		         '{"scenario_name":"create"}'::jsonb)`,

		// A drift run that already finished: history, not a hazard.
		`INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, recipe, finished_at)
		 VALUES ('01DONEDRYRUN00000000000000', 'host-c.example.com', 'redis-prod', 'converge', 'success',
		         '{"dry_run":true,"scenario_name":"converge"}'::jsonb, NOW())`,
	}
	for _, sql := range stmts {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, sql)
		}
	}
}

func driftPermissionsOf(t *testing.T, pool *pgxpool.Pool, role string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT permission FROM rbac_role_permissions WHERE role_name = $1 ORDER BY permission`, role)
	if err != nil {
		t.Fatalf("read permissions of %s: %v", role, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan permission: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate permissions of %s: %v", role, err)
	}
	return out
}

func tidingEventTypes(t *testing.T, pool *pgxpool.Pool, name string) ([]string, bool) {
	t.Helper()
	var types []string
	err := pool.QueryRow(context.Background(),
		`SELECT event_types FROM tidings WHERE name = $1`, name).Scan(&types)
	if err != nil {
		return nil, false
	}
	return types, true
}

func runStatus(t *testing.T, pool *pgxpool.Pool, applyID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM apply_runs WHERE apply_id = $1`, applyID).Scan(&status); err != nil {
		t.Fatalf("read status of %s: %v", applyID, err)
	}
	return status
}

// TestMigration114_DropsDriftSurface is the acceptance criterion for the whole
// migration: it must APPLY AT ALL over the awkward shapes (the duplicate-element
// tiding alone would abort it), and then each block must have matched what it
// claims to.
func TestMigration114_DropsDriftSurface(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(dropDriftCheckVersion - 1); err != nil {
		t.Fatalf("migrate to %d: %v", dropDriftCheckVersion-1, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	seedDriftFixture(t, pool)

	// The migration running to completion IS the first assertion.
	if err := m.Migrate(dropDriftCheckVersion); err != nil {
		t.Fatalf("migrate to %d: %v (an abort here leaves schema_migrations DIRTY "+
			"and every later keeper start failing)", dropDriftCheckVersion, err)
	}

	// --- block 1: columns ---
	for _, col := range []string{"last_drift_check_at", "last_drift_summary"} {
		var n int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'incarnation' AND column_name = $1`, col).Scan(&n); err != nil {
			t.Fatalf("column probe %s: %v", col, err)
		}
		if n != 0 {
			t.Errorf("incarnation.%s survived the drop", col)
		}
	}

	// --- block 2: grants, every stored form ---
	if got := driftPermissionsOf(t, pool, "drift-only"); len(got) != 0 {
		t.Errorf("drift-only still holds %v, want none", got)
	}
	var roleRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rbac_roles WHERE name = 'drift-only'`).Scan(&roleRows); err != nil {
		t.Fatalf("count drift-only role: %v", err)
	}
	if roleRows != 1 {
		t.Errorf("the emptied role was deleted (rows=%d), want it kept - memberships and derived children may reference the name", roleRows)
	}

	mixed := driftPermissionsOf(t, pool, "drift-mixed")
	if len(mixed) != 1 || mixed[0] != "incarnation.run on coven=prod" {
		t.Errorf("drift-mixed = %v, want exactly [incarnation.run on coven=prod] "+
			"(bare, scoped AND whitespace-variant dead grants must go; the live one must stay)", mixed)
	}
	if got := driftPermissionsOf(t, pool, "drift-none"); len(got) != 2 {
		t.Errorf("drift-none = %v, want both unrelated grants intact", got)
	}

	// A surviving grant in ANY form fails the next enforcer snapshot load.
	var leftovers int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM rbac_role_permissions
		WHERE permission LIKE '%incarnation.check-drift%'`).Scan(&leftovers); err != nil {
		t.Fatalf("count leftover grants: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("%d dead grants survived - keeper would refuse to start on its next cold load", leftovers)
	}

	// --- block 3: tidings ---
	if _, ok := tidingEventTypes(t, pool, "only-drift"); ok {
		t.Error("only-drift survived: a subscription to an event that can never fire again")
	}
	if _, ok := tidingEventTypes(t, pool, "only-drift-twice"); ok {
		t.Error("only-drift-twice survived: the duplicate-element shape must be deleted like the single one")
	}
	plus, ok := tidingEventTypes(t, pool, "drift-plus")
	if !ok {
		t.Error("drift-plus was DELETED, want it kept with its remaining types")
	} else if len(plus) != 2 || plus[0] != "scenario_run.*" || plus[1] != "cadence.*" {
		t.Errorf("drift-plus = %v, want [scenario_run.* cadence.*] in order", plus)
	}
	if nd, ok := tidingEventTypes(t, pool, "no-drift"); !ok || len(nd) != 1 {
		t.Errorf("no-drift = %v (present=%v), want its single unrelated type untouched", nd, ok)
	}

	// --- block 4: stale planned dry-run runs ---
	if got := runStatus(t, pool, "01STALEDRYRUN0000000000000"); got != "cancelled" {
		t.Errorf("stale planned dry-run status = %q, want cancelled - left planned, "+
			"ClaimNext would dispatch it as a REAL converge apply", got)
	}
	if got := runStatus(t, pool, "01NORMALPLANNED00000000000"); got != "planned" {
		t.Errorf("an ordinary planned run was cancelled (status=%q) - block 4 must only match dry_run recipes", got)
	}
	if got := runStatus(t, pool, "01DONEDRYRUN00000000000000"); got != "success" {
		t.Errorf("a finished drift run was rewritten (status=%q) - terminal rows are history", got)
	}
}

// TestMigration114_RollBackAndReapply — down restores the SCHEMA only (the data
// halves deliberately restore nothing), so up → down → up must run clean and the
// columns must come back for a rolled-back binary to select.
func TestMigration114_RollBackAndReapply(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(dropDriftCheckVersion); err != nil {
		t.Fatalf("migrate to %d: %v", dropDriftCheckVersion, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	if err := m.Migrate(dropDriftCheckVersion - 1); err != nil {
		t.Fatalf("rollback to %d: %v", dropDriftCheckVersion-1, err)
	}
	for _, col := range []string{"last_drift_check_at", "last_drift_summary"} {
		var n int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'incarnation' AND column_name = $1`, col).Scan(&n); err != nil {
			t.Fatalf("column probe %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("after rollback incarnation.%s is absent - a pre-NIM-446 binary selects it and would fail", col)
		}
	}
	// The partial index comes back with it: the iterator ordering depended on it.
	var idx int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'incarnation' AND indexname = 'incarnation_last_drift_check_at_idx'`).Scan(&idx); err != nil {
		t.Fatalf("index probe: %v", err)
	}
	if idx != 1 {
		t.Errorf("incarnation_last_drift_check_at_idx did not come back on rollback")
	}

	if err := m.Migrate(dropDriftCheckVersion); err != nil {
		t.Fatalf("re-apply %d: %v", dropDriftCheckVersion, err)
	}
}
