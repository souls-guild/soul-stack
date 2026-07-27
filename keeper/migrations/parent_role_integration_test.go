//go:build integration

package migrations_test

// Integration tests for migration 102 (ADR-078, derived roles): on a clean DB the
// column, its guards and the trigger appear; rolling back removes all of them and
// re-applying restores them; and roles that existed before the migration come out
// the other side as plain roles (parent_role NULL) — the backcompat criterion.
//
// Shares freshContainer / newMigrator / requireDocker with floor_integration_test.go
// (same package). Under testcontainers-PG, build tag `integration`.

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// parentRoleObjects — the schema objects migration 102 owns, as EXISTS probes.
// Used in both directions: all true after up, all false after down.
var parentRoleObjects = map[string]string{
	"column parent_role": `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'rbac_roles' AND column_name = 'parent_role')`,
	"CHECK rbac_roles_parent_not_self": `SELECT EXISTS(
		SELECT 1 FROM pg_constraint WHERE conname = 'rbac_roles_parent_not_self')`,
	"FK rbac_roles_parent_role_fk": `SELECT EXISTS(
		SELECT 1 FROM pg_constraint WHERE conname = 'rbac_roles_parent_role_fk')`,
	"index rbac_roles_parent_role_idx": `SELECT EXISTS(
		SELECT 1 FROM pg_indexes WHERE indexname = 'rbac_roles_parent_role_idx')`,
	"trigger rbac_roles_parent_chain_guard": `SELECT EXISTS(
		SELECT 1 FROM pg_trigger
		WHERE tgname = 'rbac_roles_parent_chain_guard' AND NOT tgisinternal)`,
	"function rbac_roles_parent_chain_guard": `SELECT EXISTS(
		SELECT 1 FROM pg_proc WHERE proname = 'rbac_roles_parent_chain_guard')`,
}

func assertParentRoleObjects(t *testing.T, pool *pgxpool.Pool, want bool, phase string) {
	t.Helper()
	for label, q := range parentRoleObjects {
		var got bool
		if err := pool.QueryRow(context.Background(), q).Scan(&got); err != nil {
			t.Fatalf("%s: probe %s: %v", phase, label, err)
		}
		if got != want {
			t.Errorf("%s: %s present = %v, want %v", phase, label, got, want)
		}
	}
}

// TestMigration102_AppliesAndRollsBack — the acceptance criterion for the
// migration itself: it applies on a clean DB, its down removes every object it
// created, and it re-applies afterwards (no leftovers blocking a second up).
// The FK is asserted to be RESTRICT specifically: the orphan policy is the whole
// point, and a default NO ACTION / a stray CASCADE would change what happens to a
// child role when its parent is deleted.
func TestMigration102_AppliesAndRollsBack(t *testing.T) {
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

	assertParentRoleObjects(t, pool, true, "after up")

	// confdeltype 'r' = ON DELETE RESTRICT (fail-closed orphan policy, ADR-078).
	var delRule string
	if err := pool.QueryRow(ctx,
		`SELECT confdeltype FROM pg_constraint WHERE conname = 'rbac_roles_parent_role_fk'`,
	).Scan(&delRule); err != nil {
		t.Fatalf("query FK delete rule: %v", err)
	}
	if delRule != "r" {
		t.Errorf("rbac_roles_parent_role_fk ON DELETE = %q, want %q (RESTRICT)", delRule, "r")
	}

	// Absolute versions, not Steps(±1): the head moves with every migration the
	// release adds, and a relative step would roll back somebody else's.
	if err := m.Migrate(101); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(101) (down through 102): %v", err)
	}
	assertParentRoleObjects(t, pool, false, "after down")

	if err := m.Migrate(102); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(102) (re-apply 102): %v", err)
	}
	assertParentRoleObjects(t, pool, true, "after re-apply")
}

// TestMigration102_ExistingRolesStayPlain — backcompat: roles written before the
// migration must come out as PLAIN roles (parent_role NULL), i.e. behaving
// exactly as they do today. A non-NULL default would silently give every existing
// role a ceiling it never had.
func TestMigration102_ExistingRolesStayPlain(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	defer m.Close()
	if err := m.Migrate(101); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(101): %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`INSERT INTO rbac_roles (name, description, builtin, default_scope)
		 VALUES ('pre-existing-scoped', 'written before 102', false, 'coven=dba')`,
	); err != nil {
		t.Fatalf("seed a pre-102 role: %v", err)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("Steps(1) (apply 102): %v", err)
	}

	var plain int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rbac_roles WHERE parent_role IS NOT NULL`,
	).Scan(&plain); err != nil {
		t.Fatalf("count derived roles: %v", err)
	}
	if plain != 0 {
		t.Errorf("%d role(s) came out of the migration with a parent; every pre-existing role must be plain", plain)
	}

	// The seeded cluster-admin (migration 027) and the role above must both
	// survive untouched — the migration adds a column, it does not rewrite rows.
	var scope *string
	if err := pool.QueryRow(ctx,
		`SELECT default_scope FROM rbac_roles WHERE name = 'pre-existing-scoped'`,
	).Scan(&scope); err != nil {
		t.Fatalf("read back the pre-102 role: %v", err)
	}
	if scope == nil || *scope != "coven=dba" {
		t.Errorf("default_scope of the pre-102 role = %v, want %q", scope, "coven=dba")
	}
}
