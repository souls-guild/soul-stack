//go:build integration

package migrations_test

// Integration tests for migrations 108 and 109 (NIM-330, ADR-044 amendment
// 2026-07-30): the removal of `incarnation.spec.hosts[]` and of the two
// permissions that guarded its editing endpoint.
//
// Both are data-only migrations over shapes that cannot be exercised without a
// real PG: 108 uses the jsonb `-` and `?` operators, and 109 is a plpgsql DO
// block whose whole point is which permission strings it matches. That match is
// the reason this file exists rather than trusting a unit test — migration 095
// wrote the equivalent DELETE with plain equality and would have missed every
// SCOPED grant (`… on coven=prod`), leaving exactly the row that fails the
// enforcer snapshot load.
//
// Shares freshContainer / newMigrator / requireDocker with the other files in
// this package. Under testcontainers-PG, build tag `integration`.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Absolute versions, never relative Steps: a test that walks "one past the end"
// silently re-aims at whatever migration lands next (see the note in the 106
// rollback test).
const (
	dropSpecHostsVersion  = 108
	dropUpdateHostsPermsV = 109
)

// seedSpecHostsFixture writes three incarnations straddling the shapes 108 has
// to tell apart: a spec that is nothing but `hosts`, a spec where `hosts` sits
// beside keys that must survive, and a spec that never had the key.
func seedSpecHostsFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state)
		 VALUES ('only-hosts', 'redis', 'v1', 1, 'ready',
		         '{"hosts":[{"sid":"a.example.com","role":"master"}]}'::jsonb, '{}'::jsonb)`,

		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state)
		 VALUES ('hosts-plus', 'redis', 'v1', 1, 'ready',
		         '{"hosts":[{"sid":"b.example.com"}],"input":{"replicas":2},"traits":{"team":"dba"}}'::jsonb,
		         '{}'::jsonb)`,

		`INSERT INTO incarnation (name, service, service_version, state_schema_version, status, spec, state)
		 VALUES ('no-hosts', 'redis', 'v1', 1, 'ready',
		         '{"input":{"replicas":1}}'::jsonb, '{}'::jsonb)`,
	}
	for _, sql := range stmts {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, sql)
		}
	}
}

func incarnationSpec(t *testing.T, pool *pgxpool.Pool, name string) map[string]any {
	t.Helper()
	var spec map[string]any
	if err := pool.QueryRow(context.Background(),
		`SELECT spec FROM incarnation WHERE name = $1`, name).Scan(&spec); err != nil {
		t.Fatalf("read spec of %s: %v", name, err)
	}
	return spec
}

// TestMigration108_StripsHostsKeepsEverythingElse — the acceptance criterion:
// the `hosts` key is gone from every spec that had it, and no other key moves.
func TestMigration108_StripsHostsKeepsEverythingElse(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(dropSpecHostsVersion - 1); err != nil {
		t.Fatalf("migrate to %d: %v", dropSpecHostsVersion-1, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	seedSpecHostsFixture(t, pool)

	if err := m.Migrate(dropSpecHostsVersion); err != nil {
		t.Fatalf("migrate to %d: %v", dropSpecHostsVersion, err)
	}

	// A spec that held nothing else becomes empty, NOT null: `spec` is NOT NULL
	// and the rest of the system reads it as a map.
	if got := incarnationSpec(t, pool, "only-hosts"); len(got) != 0 {
		t.Errorf("only-hosts spec = %v, want {} (hosts was its only key)", got)
	}

	// The neighbours survive. `input` in particular is the create-path payload
	// rerun-last replays — losing it would break a feature unrelated to NIM-330.
	plus := incarnationSpec(t, pool, "hosts-plus")
	if _, still := plus["hosts"]; still {
		t.Errorf("hosts-plus kept spec.hosts: %v", plus)
	}
	if _, ok := plus["input"].(map[string]any); !ok {
		t.Errorf("hosts-plus lost spec.input: %v", plus)
	}
	if _, ok := plus["traits"].(map[string]any); !ok {
		t.Errorf("hosts-plus lost spec.traits: %v", plus)
	}

	if got := incarnationSpec(t, pool, "no-hosts"); len(got) != 1 {
		t.Errorf("no-hosts spec = %v, want its single untouched key", got)
	}
}

// seedUpdateHostsGrants writes the four shapes a dead grant can take, plus three
// that must survive. `incarnation.update` and `incarnation.update-hosts` differ
// by a prefix, so a careless LIKE deletes one while matching the other.
func seedUpdateHostsGrants(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	roles := []string{"doomed", "mixed", "untouched"}
	for _, r := range roles {
		if _, err := pool.Exec(ctx,
			`INSERT INTO rbac_roles (name, description) VALUES ($1, '')`, r); err != nil {
			t.Fatalf("seed role %s: %v", r, err)
		}
	}
	grants := []struct{ role, perm string }{
		// Everything this role has is dead — it must survive as a role with no
		// permissions rather than be deleted (memberships and derived children
		// may reference the name).
		{"doomed", "incarnation.update-hosts"},
		{"doomed", "incarnation.update"},

		// The scoped forms: the shape migration 095 would have missed.
		{"mixed", "incarnation.update-hosts on coven=prod"},
		{"mixed", "incarnation.update on incarnation=redis-prod"},
		// ... alongside grants that must NOT be touched. `incarnation.*` is the
		// one most at risk from a sloppy pattern, and `incarnation.updates-something`
		// is the prefix trap.
		{"mixed", "incarnation.run on coven=prod"},
		{"mixed", "incarnation.*"},

		{"untouched", "incarnation.traits-set"},
		{"untouched", "choir.add-voice"},
	}
	for _, g := range grants {
		if _, err := pool.Exec(ctx,
			`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2)`,
			g.role, g.perm); err != nil {
			t.Fatalf("seed grant %s/%s: %v", g.role, g.perm, err)
		}
	}
}

func permissionsOf(t *testing.T, pool *pgxpool.Pool, role string) []string {
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

// TestMigration109_DropsDeadGrantsBareAndScoped — the acceptance criterion: both
// removed names go in every form they can be written, nothing else does, and a
// role stripped bare is kept rather than deleted.
//
// This is not tidiness. The catalog is a closed enum and the enforcer is
// fail-closed: one surviving row aborts the whole RBAC snapshot load at the next
// keeper start, so a missed shape here is a cluster-wide outage, not a lost
// grant.
func TestMigration109_DropsDeadGrantsBareAndScoped(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(dropUpdateHostsPermsV - 1); err != nil {
		t.Fatalf("migrate to %d: %v", dropUpdateHostsPermsV-1, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	seedUpdateHostsGrants(t, pool)

	if err := m.Migrate(dropUpdateHostsPermsV); err != nil {
		t.Fatalf("migrate to %d: %v", dropUpdateHostsPermsV, err)
	}

	if got := permissionsOf(t, pool, "doomed"); len(got) != 0 {
		t.Errorf("doomed still holds %v, want none", got)
	}
	// The role itself stays: dropping it would cascade into memberships and
	// derived roles, far beyond what this change decided.
	var roleRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rbac_roles WHERE name = 'doomed'`).Scan(&roleRows); err != nil {
		t.Fatalf("count doomed role: %v", err)
	}
	if roleRows != 1 {
		t.Errorf("the emptied role was deleted (rows=%d), want it kept", roleRows)
	}

	mixed := permissionsOf(t, pool, "mixed")
	want := []string{"incarnation.*", "incarnation.run on coven=prod"}
	if len(mixed) != len(want) {
		t.Fatalf("mixed = %v, want exactly %v (scoped dead grants must go, live ones must stay)", mixed, want)
	}
	for i := range want {
		if mixed[i] != want[i] {
			t.Errorf("mixed[%d] = %q, want %q", i, mixed[i], want[i])
		}
	}

	untouched := permissionsOf(t, pool, "untouched")
	if len(untouched) != 2 {
		t.Errorf("untouched = %v, want both of its unrelated grants intact", untouched)
	}

	// Nothing anywhere carries either name in any form.
	var leftovers int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM rbac_role_permissions
		WHERE permission IN ('incarnation.update-hosts', 'incarnation.update')
		   OR permission LIKE 'incarnation.update-hosts on %'
		   OR permission LIKE 'incarnation.update on %'`).Scan(&leftovers); err != nil {
		t.Fatalf("count leftovers: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("%d dead grants survived - the next enforcer snapshot load would fail", leftovers)
	}
}

// TestMigrations108And109_RollBackAndReapply — both downs are data-neutral (they
// undo no schema and deliberately restore no data), so up → down → up must run
// clean over the pair.
func TestMigrations108And109_RollBackAndReapply(t *testing.T) {
	dsn, teardown := freshContainer(t)
	defer teardown()

	m := newMigrator(t, dsn)
	if err := m.Migrate(dropUpdateHostsPermsV); err != nil {
		t.Fatalf("migrate to %d: %v", dropUpdateHostsPermsV, err)
	}
	if err := m.Migrate(dropSpecHostsVersion - 1); err != nil {
		t.Fatalf("roll back to %d: %v", dropSpecHostsVersion-1, err)
	}
	if err := m.Migrate(dropUpdateHostsPermsV); err != nil {
		t.Fatalf("re-apply to %d: %v", dropUpdateHostsPermsV, err)
	}
}
