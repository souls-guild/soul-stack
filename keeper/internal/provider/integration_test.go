//go:build integration

// Integration-тесты CRUD provider через testcontainers-go.
// Паттерн совпадает с keeper/internal/incarnation/integration_test.go.

package provider

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
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
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
			log.Fatalf("provider integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("provider integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetAll(t *testing.T) {
	t.Helper()
	// CASCADE: profiles → providers → operators (FK chain).
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE profiles, providers, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{
		AID:         aid,
		DisplayName: aid,
		AuthMethod:  operator.AuthMethodJWT,
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func newProvider(name, aid string) *Provider {
	return &Provider{
		Name:           name,
		Type:           "aws",
		Region:         "eu-central-1",
		CredentialsRef: "vault:secret/cloud/" + name,
		CreatedByAID:   &aid,
	}
}

func TestIntegration_Insert_AndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	p := newProvider("aws-eu", "archon-alice")
	if err := Insert(ctx, integrationPool, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if p.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero — RETURNING did not fill")
	}

	got, err := SelectByName(ctx, integrationPool, "aws-eu")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Name != "aws-eu" || got.Type != "aws" || got.Region != "eu-central-1" {
		t.Errorf("got = %+v", got)
	}
	if got.CredentialsRef != "vault:secret/cloud/aws-eu" {
		t.Errorf("CredentialsRef = %q", got.CredentialsRef)
	}
	if got.CreatedByAID == nil || *got.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", got.CreatedByAID)
	}
}

func TestIntegration_Insert_DuplicateName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newProvider("aws-eu", "archon-alice")); err != nil {
		t.Fatalf("Insert#1: %v", err)
	}
	err := Insert(ctx, integrationPool, newProvider("aws-eu", "archon-alice"))
	if !errors.Is(err, ErrProviderAlreadyExists) {
		t.Fatalf("err = %v, want ErrProviderAlreadyExists", err)
	}
}

func TestIntegration_Insert_FKViolation(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	err := Insert(ctx, integrationPool, newProvider("aws-eu", "archon-ghost"))
	if err == nil {
		t.Fatal("Insert with non-existent operator: expected error")
	}
	if errors.Is(err, ErrProviderAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrProviderAlreadyExists; err = %v", err)
	}
}

func TestIntegration_Insert_CHECKViolation_BadName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	// Прямой INSERT в обход Go-валидации: SQL CHECK должен отбить bad name.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO providers (name, type, region, credentials_ref, created_by_aid)
		 VALUES ($1, 'aws', 'eu', 'vault:x', $2)`,
		"AWS_EU", "archon-alice")
	if err == nil {
		t.Fatal("expected CHECK violation for name='AWS_EU'")
	}
}

func TestIntegration_Insert_NullCreatedByOnOperatorDelete(t *testing.T) {
	// ON DELETE SET NULL: запись Provider-а переживает удаление оператора.
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := Insert(ctx, integrationPool, newProvider("aws-eu", "archon-alice")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM operators WHERE aid = 'archon-alice'`); err != nil {
		t.Fatalf("DELETE operator: %v", err)
	}
	got, err := SelectByName(ctx, integrationPool, "aws-eu")
	if err != nil {
		t.Fatalf("SelectByName after operator delete: %v", err)
	}
	if got.CreatedByAID != nil {
		t.Errorf("CreatedByAID = %v after operator delete, want nil", got.CreatedByAID)
	}
}

func TestIntegration_SelectByName_NotFound(t *testing.T) {
	resetAll(t)
	_, err := SelectByName(context.Background(), integrationPool, "missing")
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("err = %v, want ErrProviderNotFound", err)
	}
}

func TestIntegration_SelectAll_Pagination(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	for _, name := range []string{"aws-a", "aws-b", "yc-c"} {
		if err := Insert(ctx, integrationPool, newProvider(name, "archon-alice")); err != nil {
			t.Fatalf("Insert %s: %v", name, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	out, total, err := SelectAll(ctx, integrationPool, 0, 2)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(out) != 2 {
		t.Errorf("len(out) = %d, want 2", len(out))
	}
	// DESC по created_at → последний (yc-c) первым.
	if out[0].Name != "yc-c" {
		t.Errorf("out[0].Name = %q, want yc-c", out[0].Name)
	}
}
