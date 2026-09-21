//go:build integration

package migrations_test

// Integration test for migration 106: the prune of the residue the removed trait
// projection left in `souls.traits`.
//
// The distinction it has to get right is value equality, not key presence. A pair
// that MATCHES what one of the host's incarnations carries is a copy the
// projection made, and must go — left behind it would outlive the removal of the
// key from the incarnation and grant visibility nobody can trace. A pair that
// merely shares a KEY with the incarnation (`owner=bobik` on the host,
// `owner=dba` on the incarnation) is a label the operator attached to that host,
// and must stay — the host's own pair is the only thing that grants (NIM-281).
//
// Shares freshContainer / newMigrator / requireDocker with the other files in
// this package. Under testcontainers-PG, build tag `integration`.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pruneMigrationVersion is the version of 106_prune_projected_soul_traits. Both
// tests below address it absolutely — see the note in the rollback test on why
// relative Steps() is the wrong tool here.
const pruneMigrationVersion = 106

// seedPruneFixture builds one incarnation carrying `{"team":"dba","env":"prod"}`
// and three hosts:
//
//   - copied      — member, traits identical to the incarnation's (pure residue);
//   - mixed       — member, one copied pair + one host-local pair + one pair that
//     shares a key with the incarnation but holds a different value;
//   - unrelated   — NOT a member, traits that happen to equal the incarnation's.
func seedPruneFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state, traits)
		  VALUES ('redis-prod', 'redis', 'v1', 1, 'ready', '{}'::jsonb, '{}'::jsonb, '{"team":"dba","env":"prod"}'::jsonb)`, nil},

		{`INSERT INTO souls (sid, transport, status, coven, traits)
		  VALUES ('copied.example.com', 'agent', 'connected', ARRAY[]::text[], '{"team":"dba","env":"prod"}'::jsonb)`, nil},
		{`INSERT INTO souls (sid, transport, status, coven, traits)
		  VALUES ('mixed.example.com', 'agent', 'connected', ARRAY[]::text[], '{"team":"dba","owner":"bobik","env":"staging"}'::jsonb)`, nil},
		{`INSERT INTO souls (sid, transport, status, coven, traits)
		  VALUES ('unrelated.example.com', 'agent', 'connected', ARRAY[]::text[], '{"team":"dba"}'::jsonb)`, nil},

		{`INSERT INTO incarnation_membership (incarnation_name, sid)
		  VALUES ('redis-prod', 'copied.example.com'), ('redis-prod', 'mixed.example.com')`, nil},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, s.sql)
		}
	}
}

func soulTraitsJSON(t *testing.T, pool *pgxpool.Pool, sid string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := pool.QueryRow(context.Background(),
		`SELECT traits FROM souls WHERE sid = $1`, sid).Scan(&raw); err != nil {
		t.Fatalf("read traits of %s: %v", sid, err)
	}
	return raw
}

// TestMigration106_PrunesCopiesKeepsHostLocal — the acceptance criterion: every
// pair the projection duplicated is gone, and every pair an operator attached to
// a host survives, including one that only collides on the key.
func TestMigration106_PrunesCopiesKeepsHostLocal(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	// Versions are pinned ABSOLUTELY, never as relative Steps: a test that walks
	// "one step past the end" silently re-aims at whatever migration lands next.
	if err := m.Migrate(pruneMigrationVersion - 1); err != nil {
		t.Fatalf("migrate to %d: %v", pruneMigrationVersion-1, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	seedPruneFixture(t, pool)

	if err := m.Migrate(pruneMigrationVersion); err != nil {
		t.Fatalf("migrate to %d: %v", pruneMigrationVersion, err)
	}

	// Pure residue: every pair equalled the incarnation's → nothing left.
	if got := soulTraitsJSON(t, pool, "copied.example.com"); len(got) != 0 {
		t.Errorf("copied host traits = %v, want {} (all pairs were projection copies)", got)
	}

	// Mixed: `team=dba` was a copy and goes; `owner=bobik` is host-local and
	// stays; `env=staging` shares a key with the incarnation's `env=prod` but
	// holds a different value — it is the host's own and must stay.
	mixed := soulTraitsJSON(t, pool, "mixed.example.com")
	if _, still := mixed["team"]; still {
		t.Errorf("mixed host kept the copied pair team=dba: %v", mixed)
	}
	if mixed["owner"] != "bobik" {
		t.Errorf("mixed host lost its own label owner=bobik: %v", mixed)
	}
	if mixed["env"] != "staging" {
		t.Errorf("mixed host lost env=staging — a shared KEY is not a copy, only a shared VALUE is: %v", mixed)
	}

	// Not a member: the projection never reached it, so the identical-looking
	// pair is its own and must survive.
	if got := soulTraitsJSON(t, pool, "unrelated.example.com"); got["team"] != "dba" {
		t.Errorf("non-member host traits = %v, want team=dba intact", got)
	}
}

// TestMigration106_RollsBackAndReapplies — the down is data-neutral (it undoes no
// schema and deliberately does not re-project), so the sequence up → down → up
// must run clean and leave the pruned state in place.
//
// Pinned to this migration's own version rather than "the last one": with Steps(-1)
// the test would roll back whichever migration happens to be newest, and would
// quietly stop testing this one the day another lands.
func TestMigration106_RollsBackAndReapplies(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(pruneMigrationVersion); err != nil {
		t.Fatalf("migrate to %d: %v", pruneMigrationVersion, err)
	}
	if err := m.Migrate(pruneMigrationVersion - 1); err != nil {
		t.Fatalf("roll back %d: %v", pruneMigrationVersion, err)
	}
	if err := m.Migrate(pruneMigrationVersion); err != nil {
		t.Fatalf("re-apply %d: %v", pruneMigrationVersion, err)
	}
}
