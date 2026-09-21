//go:build integration

package migrations_test

// Integration test for migration 110 (NIM-414, ADR-0082): the removal of
// `incarnation.spec.essence`.
//
// Data-only over a shape that cannot be exercised without a real PG — it uses
// the jsonb `-` and `?` operators, and the thing worth proving is what the `-`
// operator does to its NEIGHBOURS, not to the key it names. `spec.input` in
// particular is the create-path payload `rerun-last` replays; losing it here
// would break a feature that has nothing to do with this change, and a unit test
// over a Go map would not notice.
//
// Shares freshContainer / newMigrator with the other files in this package.
// Under testcontainers-PG, build tag `integration`.

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Absolute version, never a relative Step: a test that walks "one past the end"
// silently re-aims at whatever migration lands next.
const dropSpecEssenceVersion = 110

// seedSpecEssenceFixture writes four incarnations straddling the shapes 110 has
// to tell apart: a spec that is nothing but `essence`, a spec where `essence`
// sits beside keys that must survive, a spec that never had the key, and one
// whose `input` merely CONTAINS a nested key of that name — the last is there
// because `-` is a top-level operator and a careless `jsonb_path` rewrite of
// this migration would reach into it.
func seedSpecEssenceFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state)
		 VALUES ('only-essence', 'redis', 'v1', 1, 'ready',
		         '{"essence":{"conf_dir":"/etc/redis"}}'::jsonb, '{}'::jsonb)`,

		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state)
		 VALUES ('essence-plus', 'redis', 'v1', 1, 'ready',
		         '{"essence":{"redis_version":"7.2"},"input":{"replicas":2},"traits":{"team":"dba"}}'::jsonb,
		         '{}'::jsonb)`,

		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state)
		 VALUES ('no-essence', 'redis', 'v1', 1, 'ready',
		         '{"input":{"replicas":1}}'::jsonb, '{}'::jsonb)`,

		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state)
		 VALUES ('nested-namesake', 'redis', 'v1', 1, 'ready',
		         '{"input":{"essence":{"not":"the operator override"}}}'::jsonb, '{}'::jsonb)`,
	}
	for _, sql := range stmts {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, sql)
		}
	}
}

// TestMigration110_StripsEssenceKeepsEverythingElse — the acceptance criterion:
// the top-level `essence` key is gone from every spec that had it, and nothing
// else moves.
func TestMigration110_StripsEssenceKeepsEverythingElse(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(dropSpecEssenceVersion - 1); err != nil {
		t.Fatalf("migrate to %d: %v", dropSpecEssenceVersion-1, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	seedSpecEssenceFixture(t, pool)

	if err := m.Migrate(dropSpecEssenceVersion); err != nil {
		t.Fatalf("migrate to %d: %v", dropSpecEssenceVersion, err)
	}

	// A spec that held nothing else becomes empty, NOT null: `spec` is NOT NULL
	// and the rest of the system reads it as a map.
	if got := incarnationSpec(t, pool, "only-essence"); len(got) != 0 {
		t.Errorf("only-essence spec = %v, want {} (essence was its only key)", got)
	}

	// The neighbours survive. `input` is the create-path payload rerun-last
	// replays — losing it would break a feature unrelated to ADR-0082.
	plus := incarnationSpec(t, pool, "essence-plus")
	if _, still := plus["essence"]; still {
		t.Errorf("essence-plus kept spec.essence: %v", plus)
	}
	if _, ok := plus["input"].(map[string]any); !ok {
		t.Errorf("essence-plus lost spec.input: %v", plus)
	}
	if _, ok := plus["traits"].(map[string]any); !ok {
		t.Errorf("essence-plus lost spec.traits: %v", plus)
	}

	if got := incarnationSpec(t, pool, "no-essence"); len(got) != 1 {
		t.Errorf("no-essence spec = %v, want its single untouched key", got)
	}

	// A nested key of the same name is somebody else's data — an operator input
	// field that happens to be called `essence`. The migration must not see it.
	nested := incarnationSpec(t, pool, "nested-namesake")
	input, ok := nested["input"].(map[string]any)
	if !ok {
		t.Fatalf("nested-namesake lost spec.input entirely: %v", nested)
	}
	if _, kept := input["essence"]; !kept {
		t.Errorf("the migration reached into a NESTED key of the same name: %v", nested)
	}
}

// TestMigration110_IsIdempotent — running it twice matches nothing the second
// time and changes nothing. Migrations are re-applied on a restored dump often
// enough that "the second run is a no-op" is worth holding rather than assuming.
func TestMigration110_IsIdempotent(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(dropSpecEssenceVersion - 1); err != nil {
		t.Fatalf("migrate to %d: %v", dropSpecEssenceVersion-1, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	seedSpecEssenceFixture(t, pool)

	if err := m.Migrate(dropSpecEssenceVersion); err != nil {
		t.Fatalf("migrate to %d: %v", dropSpecEssenceVersion, err)
	}
	first := incarnationSpec(t, pool, "essence-plus")

	// Down is schema-neutral (SELECT 1) and up runs again.
	if err := m.Migrate(dropSpecEssenceVersion - 1); err != nil {
		t.Fatalf("rollback to %d: %v", dropSpecEssenceVersion-1, err)
	}
	if err := m.Migrate(dropSpecEssenceVersion); err != nil {
		t.Fatalf("re-migrate to %d: %v", dropSpecEssenceVersion, err)
	}

	second := incarnationSpec(t, pool, "essence-plus")
	if !reflect.DeepEqual(first, second) {
		t.Errorf("a second run changed the spec: first=%v second=%v", first, second)
	}
	if _, ok := second["input"].(map[string]any); !ok {
		t.Errorf("a second run lost spec.input: %v", second)
	}
}
