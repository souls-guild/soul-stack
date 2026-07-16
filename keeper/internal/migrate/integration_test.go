//go:build integration

// Integration tests for Apply through testcontainers-go.
//
// They start postgres:16-alpine, run Apply on a clean DB, and verify
// idempotency + down/up cycle. One container per package; between tests schema
// state is dropped through resetSchema (DROP TABLE audit_log + DROP TABLE
// schema_migrations).
//
// Run:
//
//	make test-integration
//	# or
//	cd keeper && go test -tags=integration -race -count=1 ./internal/migrate/
package migrate_test

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	keepermigrate "github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var (
	integrationDSN  string
	integrationPool *pgxpool.Pool
)

// TestMain delegates setup/teardown to run(), because os.Exit bypasses defers;
// context, container, and pool would otherwise keep hanging.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run starts a Postgres container, puts DSN/pool into package vars, and calls
// m.Run(). It returns exit code; defers inside the function run correctly
// because os.Exit is called later in TestMain over the returned code.
//
// SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true makes testcontainers mandatory
// (CI mode): any setup failure -> log.Fatalf. Without the flag (local mode),
// tests are skipped when docker is unavailable.
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
		if requireDocker() {
			log.Fatalf("migrate integration: setup failed (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER set): %v", err)
		}
		log.Printf("migrate integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("migrate integration: ConnectionString: %v", err)
		return 1
	}
	integrationDSN = dsn

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("migrate integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

// resetSchema returns DB to the initial "clean, no migrations" state. Package
// tests share one container (one per package), so schema must be fully reset
// between runs.
//
// `DROP SCHEMA public CASCADE` + recreate removes everything at once: all
// tables (audit_log/operators/incarnation/souls/apply_runs/providers/profiles/...),
// functions (purge_*/expire_*/mark_disconnected/...), types, indexes, and
// golang-migrate service schema_migrations, regardless of migration count. This
// is robust to new migrations: no need to list objects manually and keep the
// list in sync with migrations/*.up.sql.
func resetSchema(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP SCHEMA public CASCADE`,
		`CREATE SCHEMA public`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("resetSchema %q: %v", stmt, err)
		}
	}
}

func TestIntegration_MigrateApply_FromScratch(t *testing.T) {
	resetSchema(t)
	ctx := context.Background()

	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	assertAuditLogSchema(t, ctx)
	assertOperatorsSchema(t, ctx)
	assertAuditLogOperatorFK(t, ctx)
	assertRBACSchema(t, ctx)
}

// assertAuditLogSchema verifies that audit_log exists, three expected indexes
// are present, and two of them (archon_aid, correlation_id) are partial
// (`WHERE ... IS NOT NULL`). Extracted for reuse between FromScratch and
// DownThenUp (gap-1/gap-3 from qa.A).
func assertAuditLogSchema(t *testing.T, ctx context.Context) {
	t.Helper()

	var exists bool
	err := integrationPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_tables
			WHERE schemaname = current_schema() AND tablename = 'audit_log'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("pg_tables: %v", err)
	}
	if !exists {
		t.Fatal("audit_log table missing")
	}

	// Three indexes from 001_create_audit_log.up.sql.
	wantIndexes := []string{
		"audit_log_event_type_created_at_idx",
		"audit_log_archon_aid_created_at_idx",
		"audit_log_correlation_id_idx",
	}
	for _, idx := range wantIndexes {
		var got bool
		err := integrationPool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_indexes
				WHERE schemaname = current_schema() AND indexname = $1
			)
		`, idx).Scan(&got)
		if err != nil {
			t.Fatalf("pg_indexes %q: %v", idx, err)
		}
		if !got {
			t.Errorf("index %q missing", idx)
		}
	}

	// Partial indexes (`WHERE ... IS NOT NULL`) have pg_index.indpred != NULL.
	// If someone removes the `WHERE` clause in up.sql, index becomes full and
	// stops matching ADR-022 schema.
	partialIndexes := []string{
		"audit_log_archon_aid_created_at_idx",
		"audit_log_correlation_id_idx",
	}
	for _, idx := range partialIndexes {
		var isPartial bool
		err := integrationPool.QueryRow(ctx, `
			SELECT i.indpred IS NOT NULL
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = $1
		`, idx).Scan(&isPartial)
		if err != nil {
			t.Fatalf("pg_index %q: %v", idx, err)
		}
		if !isPartial {
			t.Errorf("index %q expected partial (WHERE ... IS NOT NULL), got full", idx)
		}
	}
}

func TestIntegration_MigrateApply_Idempotent(t *testing.T) {
	resetSchema(t)
	ctx := context.Background()

	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply #1: %v", err)
	}
	// Second call on the same DB should not return error (golang-migrate returns
	// ErrNoChange in this case, and Apply swallows it).
	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply #2 (idempotent): %v", err)
	}
}

func TestIntegration_MigrateApply_DownThenUp(t *testing.T) {
	resetSchema(t)
	ctx := context.Background()

	if err := keepermigrate.Apply(ctx, integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Steps(-1) and Steps(1) are not exposed through public Apply API (down is
	// unsupported from cmd/keeper, ADR-022 forward-only). Here we call migrate
	// directly to verify that .down.sql is valid and schema is actually
	// reconstructed back.
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	// Duplicate Apply scheme rewrite (`postgres://` -> `pgx5://`); pulling the
	// internal toMigrateURL is undesirable because the test should not depend on
	// package internals.
	migrateURL := strings.Replace(integrationDSN, "postgres://", "pgx5://", 1)
	mm, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		t.Fatalf("migrate.NewWithSourceInstance: %v", err)
	}
	defer mm.Close()

	// Full rollback of all applied migrations (down). With migrations 003/004
	// (operators + FK), one-step rollback no longer drops audit_log; it lives in
	// 001 and disappears only when all later migrations are rolled back.
	// mm.Down() is symmetric to Apply (up to latest) and does not depend on
	// migration count.
	if err := mm.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Down: %v", err)
	}

	// After down, neither audit_log nor operators nor rbac_* should exist.
	for _, tbl := range []string{
		"audit_log", "operators",
		"rbac_roles", "rbac_role_permissions", "rbac_role_operators",
	} {
		var exists bool
		err = integrationPool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_tables
				WHERE schemaname = current_schema() AND tablename = $1
			)
		`, tbl).Scan(&exists)
		if err != nil {
			t.Fatalf("pg_tables after down (%s): %v", tbl, err)
		}
		if exists {
			t.Fatalf("%s still exists after Down()", tbl)
		}
	}

	if err := mm.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Up: %v", err)
	}

	// After up, table and three indexes (including partial clause) should be
	// fully restored, symmetric to FromScratch.
	assertAuditLogSchema(t, ctx)
	assertOperatorsSchema(t, ctx)
	assertAuditLogOperatorFK(t, ctx)
	assertRBACSchema(t, ctx)
}

// assertRBACSchema verifies ADR-028 Phase 1: three rbac_* tables exist, seed
// role cluster-admin (builtin=true) with permission `*` is present (migration
// 027 E1), and ON DELETE CASCADE with rbac_roles works (deleting a custom role
// removes its permissions and membership).
func assertRBACSchema(t *testing.T, ctx context.Context) {
	t.Helper()

	for _, tbl := range []string{"rbac_roles", "rbac_role_permissions", "rbac_role_operators"} {
		var exists bool
		err := integrationPool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_tables
				WHERE schemaname = current_schema() AND tablename = $1
			)
		`, tbl).Scan(&exists)
		if err != nil {
			t.Fatalf("pg_tables %s: %v", tbl, err)
		}
		if !exists {
			t.Fatalf("%s table missing", tbl)
		}
	}

	// Seed cluster-admin (E1): builtin=true + permission `*`.
	var builtin bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT builtin FROM rbac_roles WHERE name = 'cluster-admin'`).Scan(&builtin); err != nil {
		t.Fatalf("seed cluster-admin missing: %v", err)
	}
	if !builtin {
		t.Error("cluster-admin builtin = false, want true (seed E1)")
	}
	var seedPerm string
	if err := integrationPool.QueryRow(ctx,
		`SELECT permission FROM rbac_role_permissions WHERE role_name = 'cluster-admin'`).Scan(&seedPerm); err != nil {
		t.Fatalf("seed cluster-admin permission missing: %v", err)
	}
	if seedPerm != "*" {
		t.Errorf("cluster-admin permission = %q, want *", seedPerm)
	}

	// CHECK rbac_roles_name_format: valid kebab-case name passes, invalid
	// (Uppercase) is rejected.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name) VALUES ('cascade-probe')`); err != nil {
		t.Errorf("valid role name rejected: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name) VALUES ('BadName')`); err == nil {
		t.Error("invalid role name accepted, expected CHECK violation")
	}

	// ON DELETE CASCADE: custom role permission is removed with the role.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('cascade-probe', 'soul.list')`); err != nil {
		t.Fatalf("insert cascade-probe perm: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM rbac_roles WHERE name = 'cascade-probe'`); err != nil {
		t.Fatalf("delete cascade-probe role: %v", err)
	}
	var leftover int64
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM rbac_role_permissions WHERE role_name = 'cascade-probe'`).Scan(&leftover); err != nil {
		t.Fatalf("count leftover perms: %v", err)
	}
	if leftover != 0 {
		t.Errorf("ON DELETE CASCADE failed: %d permission rows survived role delete", leftover)
	}
}

// assertOperatorsSchema verifies that operators exists, partial unique index on
// `created_by_aid IS NULL` is attached (single bootstrap Archon invariant from
// ADR-013/014), and CHECK constraints for AID format + auth_method enum
// validate expected values.
func assertOperatorsSchema(t *testing.T, ctx context.Context) {
	t.Helper()

	var exists bool
	err := integrationPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_tables
			WHERE schemaname = current_schema() AND tablename = 'operators'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("pg_tables operators: %v", err)
	}
	if !exists {
		t.Fatal("operators table missing")
	}

	// Partial unique index: without it the single bootstrap Archon invariant
	// would be broken. ADR-058(d): predicate moved from created_by_aid IS NULL to
	// created_via='bootstrap' (migration 085), but index remains UNIQUE+partial.
	var isUniquePartial bool
	err = integrationPool.QueryRow(ctx, `
		SELECT i.indisunique AND i.indpred IS NOT NULL
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relname = 'operators_first_archon_idx'
	`).Scan(&isUniquePartial)
	if err != nil {
		t.Fatalf("pg_index operators_first_archon_idx: %v", err)
	}
	if !isUniquePartial {
		t.Error("operators_first_archon_idx should be UNIQUE + partial (WHERE created_via = 'bootstrap')")
	}

	// ADR-058(d) guard (case 4): migration 086 seeded system operator
	// archon-system with created_via='system' and created_by_aid=NULL.
	var (
		sysVia       string
		sysCreatedBy *string
	)
	err = integrationPool.QueryRow(ctx,
		`SELECT created_via, created_by_aid FROM operators WHERE aid = 'archon-system'`,
	).Scan(&sysVia, &sysCreatedBy)
	if err != nil {
		t.Fatalf("archon-system seed missing (migration 086): %v", err)
	}
	if sysVia != "system" {
		t.Errorf("archon-system created_via = %q, want \"system\"", sysVia)
	}
	if sysCreatedBy != nil {
		t.Errorf("archon-system created_by_aid = %v, want NULL", *sysCreatedBy)
	}

	// CHECK aid_format: valid AID passes, invalid one is rejected.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-test-ok', 'OK', 'jwt')
	`); err != nil {
		t.Errorf("valid AID rejected: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('NOT-AN-AID', 'Bad', 'jwt')
	`); err == nil {
		t.Error("invalid AID accepted, expected CHECK violation")
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-test-bad-auth', 'Bad', 'sso')
	`); err == nil {
		t.Error("invalid auth_method accepted, expected CHECK violation")
	}
	// CHECK created_via_valid: value outside the domain is rejected.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method, created_via)
		VALUES ('archon-test-bad-via', 'Bad', 'jwt', 'wormhole')
	`); err == nil {
		t.Error("invalid created_via accepted, expected CHECK violation")
	}

	// ADR-058(d) invariant (case 1 at DB level): second INSERT with
	// created_via='bootstrap' violates partial unique (first bootstrap slot is
	// already occupied by archon-test-boot). Presence of NON-bootstrap rows with
	// created_by_aid IS NULL (archon-system, archon-test-ok) does NOT break the
	// invariant.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method, created_via)
		VALUES ('archon-test-boot', 'Boot', 'jwt', 'bootstrap')
	`); err != nil {
		t.Errorf("first bootstrap operator rejected: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method, created_via)
		VALUES ('archon-second-bootstrap', 'Bootstrap?', 'jwt', 'bootstrap')
	`); err == nil {
		t.Error("second operator with created_via='bootstrap' accepted, want unique violation")
	}

	// Cleanup to avoid interfering with DownThenUp / later TestIntegration_*.
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM operators WHERE aid IN ('archon-test-ok', 'archon-test-boot')`,
	); err != nil {
		t.Fatalf("cleanup operators: %v", err)
	}
}

// assertAuditLogOperatorFK verifies that migration 004 really created FK
// `audit_log.archon_aid -> operators(aid)` with ON DELETE SET NULL.
func assertAuditLogOperatorFK(t *testing.T, ctx context.Context) {
	t.Helper()

	// confdeltype is PostgreSQL `"char"` (1 byte, OID 18); pgx in binary
	// protocol does not map it to regular Go types. Cast to text directly in SQL;
	// trivial and readable.
	var deleteAction string
	err := integrationPool.QueryRow(ctx, `
		SELECT confdeltype::text
		FROM pg_constraint
		WHERE conname = 'audit_log_archon_aid_fk'
	`).Scan(&deleteAction)
	if err != nil {
		t.Fatalf("pg_constraint audit_log_archon_aid_fk: %v", err)
	}
	// 'n' = SET NULL in pg_constraint.confdeltype.
	if deleteAction != "n" {
		t.Errorf("FK ON DELETE = %q (pg_constraint.confdeltype), want 'n' (SET NULL)", deleteAction)
	}
}

// TestIntegration_MigrateApply_DirtyState verifies that after a failed
// migration (broken SQL), golang-migrate marks the version dirty and repeated
// up returns migrate.ErrDirty. This protects keeper startup from silently
// restarting on a half-applied schema.
//
// Production Apply accepts `embed.FS`; it cannot be replaced in-memory without
// `//go:embed` specifics. Therefore the test uses golang-migrate directly with
// in-memory source (httpfs), simulating what would happen inside Apply with a
// broken up.sql. Coverage is sufficient: dirty state lives in DB
// (schema_migrations.dirty), not in Go Apply logic.
func TestIntegration_MigrateApply_DirtyState(t *testing.T) {
	resetSchema(t)

	// Broken migration: CREATE TABLE, then syntactically invalid SQL. pgx5
	// driver runs up in a transaction; SQL error zeros changes, but
	// schema_migrations.dirty is still set. This is a golang-migrate invariant
	// independent of DDL atomicity itself.
	brokenFS := fstest.MapFS{
		"001_dirty.up.sql":   {Data: []byte("CREATE TABLE dirty_test (id INT); SELECT broken syntax here;")},
		"001_dirty.down.sql": {Data: []byte("DROP TABLE IF EXISTS dirty_test;")},
	}
	migrateURL := strings.Replace(integrationDSN, "postgres://", "pgx5://", 1)

	src1, err := iofs.New(brokenFS, ".")
	if err != nil {
		t.Fatalf("iofs.New #1: %v", err)
	}
	mm1, err := migrate.NewWithSourceInstance("inmem", src1, migrateURL)
	if err != nil {
		t.Fatalf("migrate.NewWithSourceInstance #1: %v", err)
	}
	if err := mm1.Up(); err == nil {
		mm1.Close()
		t.Fatal("Up on broken migration: want error, got nil")
	}
	mm1.Close()

	// Repeated Up should see dirty=true and return migrate.ErrDirty. New driver
	// instance + new source: previous mm1.Close() closed both source and database
	// connection.
	src2, err := iofs.New(brokenFS, ".")
	if err != nil {
		t.Fatalf("iofs.New #2: %v", err)
	}
	mm2, err := migrate.NewWithSourceInstance("inmem", src2, migrateURL)
	if err != nil {
		t.Fatalf("migrate.NewWithSourceInstance #2: %v", err)
	}
	defer mm2.Close()
	err = mm2.Up()
	if err == nil {
		t.Fatal("Up on dirty state: want error, got nil")
	}
	var dirtyErr migrate.ErrDirty
	if !errors.As(err, &dirtyErr) {
		t.Fatalf("Up on dirty state: want migrate.ErrDirty, got %T: %v", err, err)
	}

	// schema_migrations with dirty=true and custom table dirty_test (if partially
	// created) remain between tests; next resetSchema at the beginning of another
	// Test* removes audit_log + schema_migrations, but not dirty_test, so clean
	// it manually here.
	cleanCtx := context.Background()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS dirty_test`,
		`DROP TABLE IF EXISTS schema_migrations`,
	} {
		if _, err := integrationPool.Exec(cleanCtx, stmt); err != nil {
			t.Fatalf("dirty cleanup %q: %v", stmt, err)
		}
	}
}

// TestIntegration_MigrateApply_CtxCanceled models SIGTERM arriving at Apply
// start (pre-cancelled ctx). Interrupting migrations through GracefulStop is
// best-effort, not a guarantee: ctx-bridge goroutine races with m.Up(), and on a
// fresh schema migrations may buffer faster than bridge sends stop. Therefore
// both outcomes are valid: Apply either interrupted (err != nil) or finished
// migrating (err == nil). What the test MUST guarantee: pre-cancel does not
// leave schema in broken/dirty state; repeated Apply on a clean ctx should pass
// idempotently.
func TestIntegration_MigrateApply_CtxCanceled(t *testing.T) {
	resetSchema(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before start

	// Does not panic; interruption error is allowed but not required (best-effort).
	_ = keepermigrate.Apply(ctx, integrationDSN, migrations.FS, ".")

	// State is consistent: repeated Apply on a live ctx brings schema to the end
	// without dirty error; pre-cancel did not tear migration halfway.
	if err := keepermigrate.Apply(context.Background(), integrationDSN, migrations.FS, "."); err != nil {
		t.Fatalf("repeated Apply after pre-cancel: %v (schema remained inconsistent)", err)
	}
}

// TestIntegration_Migrate082_OverPopulated is a guard for forward migration 082
// (ADR-027 amend (m), S0) over NON-EMPTY incarnation: before 082 there is
// already a row in status='applying' (epoch columns do not exist yet). After
// 082, epoch columns are additive and NULL (no backfill), so this legacy row is
// structurally EXCLUDED by reconcile_orphan_applying candidate filter
// (applying_by_kid IS NOT NULL), and Reaper does NOT reclaim it (documented
// known-gap legacy/pre-082).
//
// applyUpTo(81)->insert->applyUpTo(82) on one iofs source: golang-migrate
// Migrate(version) runs migrations up to the exact version, giving a data
// insertion point BETWEEN steps (reuse of DownThenUp harness).
func TestIntegration_Migrate082_OverPopulated(t *testing.T) {
	resetSchema(t)
	ctx := context.Background()

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	migrateURL := strings.Replace(integrationDSN, "postgres://", "pgx5://", 1)
	mm, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		t.Fatalf("migrate.NewWithSourceInstance: %v", err)
	}
	defer mm.Close()

	// Step 1: up to 081 inclusive; incarnation exists, applying-epoch columns
	// (082) do NOT exist yet.
	if err := mm.Migrate(81); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(81): %v", err)
	}

	// Step 2: insert applying row BEFORE 082 (epoch columns physically do not
	// exist, so INSERT does not mention them). applying is a valid enum since 005.
	const name = "legacy-applying-082"
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO incarnation (name, service, service_version, state_schema_version, state, status)
VALUES ($1, 'redis', 'v1', 1, '{"primary":"p"}'::jsonb, 'applying')`, name); err != nil {
		t.Fatalf("insert legacy applying row (before 082): %v", err)
	}

	// Step 3: forward 082 on NON-EMPTY table.
	if err := mm.Migrate(82); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(82): %v", err)
	}

	// Assert A: new columns on legacy row = NULL (additive, no backfill).
	var epochAllNull bool
	if err := integrationPool.QueryRow(ctx, `
SELECT applying_apply_id IS NULL
   AND applying_attempt  IS NULL
   AND applying_by_kid   IS NULL
   AND applying_since    IS NULL
FROM incarnation WHERE name = $1`, name).Scan(&epochAllNull); err != nil {
		t.Fatalf("read epoch columns after 082: %v", err)
	}
	if !epochAllNull {
		t.Errorf("epoch columns of legacy row are not all NULL after 082 (expected no backfill)")
	}

	// Assert B: candidate filter of reconcile_orphan_applying EXCLUDES the legacy
	// row. Reproduce orphanApplyingCandidatesSQL predicate (status='applying' AND
	// applying_since < cutoff AND applying_by_kid IS NOT NULL); do not import
	// reaper package here (that is the point of the guard: NULL applying_by_kid
	// structurally cuts the row out of candidates).
	var candidateCount int
	if err := integrationPool.QueryRow(ctx, `
SELECT count(*)
FROM incarnation
WHERE status = 'applying'
  AND applying_since < NOW()
  AND applying_by_kid IS NOT NULL
  AND name = $1`, name).Scan(&candidateCount); err != nil {
		t.Fatalf("candidate-filter query: %v", err)
	}
	if candidateCount != 0 {
		t.Errorf("legacy applying row entered reconcile candidates (%d), want 0 (NULL applying_by_kid is known-gap, not reclaimed)", candidateCount)
	}

	// Sanity: row really exists and remained applying (migration touched schema,
	// not data), otherwise assert B would be falsely green.
	var status string
	if err := integrationPool.QueryRow(ctx, `SELECT status FROM incarnation WHERE name = $1`, name).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "applying" {
		t.Errorf("status of legacy row = %q, want applying (082 must not change data)", status)
	}
}
