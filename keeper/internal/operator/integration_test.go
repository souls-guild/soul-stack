//go:build integration

// Integration tests for operator-CRUD via testcontainers-go (postgres:16-alpine).
// Pattern matches keeper/internal/auditpg/integration_test.go.

package operator

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

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
			log.Fatalf("operator integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("operator integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("operator integration: ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("operator integration: migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("operator integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetOperators(t *testing.T) {
	t.Helper()
	// TRUNCATE with CASCADE removes FK dependency audit_log → operators
	// (if previous tests wrote audit records). CASCADE transitively
	// truncates rbac tables (rbac_roles.created_by_aid / rbac_role_operators.aid
	// — FK to operators), so seed role cluster-admin (migration 027) is also
	// erased — must be re-seeded for Slice 3 lockout tests.
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE operators: %v", err)
	}
	// Idempotent re-seed of built-in role cluster-admin (+permission `*`),
	// symmetric to migration 027 — DB source of lockout-probe (Slice 3) JOINs
	// rbac_role_permissions, role must exist.
	_, err = integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_roles (name, description, builtin, created_by_aid)
		 VALUES ('cluster-admin', 'Built-in full access role (permissions: *)', true, NULL)
		 ON CONFLICT (name) DO NOTHING`)
	if err != nil {
		t.Fatalf("re-seed cluster-admin role: %v", err)
	}
	_, err = integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_role_permissions (role_name, permission)
		 VALUES ('cluster-admin', '*')
		 ON CONFLICT (role_name, permission) DO NOTHING`)
	if err != nil {
		t.Fatalf("re-seed cluster-admin permission: %v", err)
	}
}

func TestIntegration_Insert_Bootstrap_AndSelect(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()

	op := &Operator{
		AID:         "archon-alice",
		DisplayName: "Alice Admin",
		AuthMethod:  AuthMethodJWT,
		CreatedVia:  CreatedViaBootstrap,
	}
	if err := Insert(ctx, integrationPool, op); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := SelectByAID(ctx, integrationPool, "archon-alice")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if got.AID != "archon-alice" {
		t.Errorf("AID = %q", got.AID)
	}
	if got.DisplayName != "Alice Admin" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.AuthMethod != AuthMethodJWT {
		t.Errorf("AuthMethod = %q", got.AuthMethod)
	}
	// Bootstrap operator determined by created_via='bootstrap'
	// (ADR-058(d) / ADR-013-amendment), not by created_by_aid IS NULL.
	if got.CreatedVia != CreatedViaBootstrap {
		t.Errorf("CreatedVia = %q, want %q (bootstrap)", got.CreatedVia, CreatedViaBootstrap)
	}
	if got.CreatedByAID != nil {
		t.Errorf("CreatedByAID = %v, want nil", got.CreatedByAID)
	}
	if got.RevokedAt != nil {
		t.Errorf("RevokedAt = %v, want nil", got.RevokedAt)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero — expected DEFAULT NOW()")
	}
}

func TestIntegration_Insert_DuplicateAID(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()

	op := &Operator{AID: "archon-alice", DisplayName: "Alice", AuthMethod: AuthMethodJWT}
	if err := Insert(ctx, integrationPool, op); err != nil {
		t.Fatalf("Insert#1: %v", err)
	}
	err := Insert(ctx, integrationPool, op)
	if !errors.Is(err, ErrOperatorAlreadyExists) {
		t.Fatalf("Insert#2: err = %v, want ErrOperatorAlreadyExists", err)
	}
}

func TestIntegration_Insert_SecondBootstrapBlocked(t *testing.T) {
	// Partial unique index `operators_first_archon_idx` forbids
	// second operator with `created_via='bootstrap'` (migrations 084/085,
	// ADR-058(d) / ADR-013-amendment). Invariant moved from
	// `created_by_aid IS NULL` to `created_via='bootstrap'`: both operators
	// must be explicitly marked bootstrap, otherwise index not triggered.
	resetOperators(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool,
		&Operator{AID: "archon-alice", DisplayName: "Alice", AuthMethod: AuthMethodJWT, CreatedVia: CreatedViaBootstrap},
	); err != nil {
		t.Fatalf("Insert#1: %v", err)
	}
	err := Insert(ctx, integrationPool,
		&Operator{AID: "archon-charlie", DisplayName: "Charlie", AuthMethod: AuthMethodJWT, CreatedVia: CreatedViaBootstrap},
	)
	if !errors.Is(err, ErrOperatorAlreadyExists) {
		t.Fatalf("Insert#2 bootstrap: err = %v, want ErrOperatorAlreadyExists", err)
	}
}

func TestIntegration_Insert_FKViolation(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	parent := "archon-ghost" // does not exist
	op := &Operator{
		AID:          "archon-bob",
		DisplayName:  "Bob",
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: &parent,
	}
	err := Insert(ctx, integrationPool, op)
	if err == nil {
		t.Fatal("Insert with non-existent parent: expected error, got nil")
	}
	if errors.Is(err, ErrOperatorAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrOperatorAlreadyExists; err = %v", err)
	}
}

func TestIntegration_SelectByAID_NotFound(t *testing.T) {
	resetOperators(t)
	_, err := SelectByAID(context.Background(), integrationPool, "archon-ghost")
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("SelectByAID: err = %v, want ErrOperatorNotFound", err)
	}
}

func TestIntegration_Count(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	n, err := Count(ctx, integrationPool)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Fatalf("Count empty = %d, want 0", n)
	}
	if err := Insert(ctx, integrationPool,
		&Operator{AID: "archon-alice", DisplayName: "Alice", AuthMethod: AuthMethodJWT},
	); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	n, err = Count(ctx, integrationPool)
	if err != nil {
		t.Fatalf("Count#2: %v", err)
	}
	if n != 1 {
		t.Errorf("Count after insert = %d, want 1", n)
	}
}
