//go:build integration

// Integration-тесты CRUD profile через testcontainers-go.
// Паттерн совпадает с keeper/internal/provider/integration_test.go.

package profile

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
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
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
			log.Fatalf("profile integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("profile integration: skipping, docker unavailable: %v", err)
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

// setupProvider — TRUNCATE + seed оператора + один Provider, к которому
// привязываются Profile-ы (FK profiles_provider_fk).
func setupProvider(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE TABLE profiles, providers, operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	op := &operator.Operator{
		AID: "archon-alice", DisplayName: "archon-alice", AuthMethod: operator.AuthMethodJWT,
	}
	if err := operator.Insert(ctx, integrationPool, op); err != nil {
		t.Fatalf("seedOperator: %v", err)
	}
	aid := "archon-alice"
	p := &provider.Provider{
		Name: "aws-eu", Type: "aws", Region: "eu-central-1",
		CredentialsRef: "vault:secret/cloud/aws-eu", CreatedByAID: &aid,
	}
	if err := provider.Insert(ctx, integrationPool, p); err != nil {
		t.Fatalf("seedProvider: %v", err)
	}
}

func newProfile(name, providerName, aid string) *Profile {
	return &Profile{
		Name:         name,
		Provider:     providerName,
		Params:       map[string]any{"instance_type": "t3.small"},
		CreatedByAID: &aid,
	}
}

func TestIntegration_Insert_AndSelect(t *testing.T) {
	setupProvider(t)
	ctx := context.Background()

	ci := "#cloud-config\npackages: [nginx]"
	p := newProfile("web-small", "aws-eu", "archon-alice")
	p.CloudInit = &ci
	if err := Insert(ctx, integrationPool, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if p.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero — RETURNING did not fill")
	}

	got, err := SelectByName(ctx, integrationPool, "web-small")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Name != "web-small" || got.Provider != "aws-eu" {
		t.Errorf("got = %+v", got)
	}
	if got.Params["instance_type"] != "t3.small" {
		t.Errorf("Params.instance_type = %v", got.Params["instance_type"])
	}
	if got.CloudInit == nil || *got.CloudInit != ci {
		t.Errorf("CloudInit = %v", got.CloudInit)
	}
}

func TestIntegration_Insert_DuplicateName(t *testing.T) {
	setupProvider(t)
	ctx := context.Background()
	if err := Insert(ctx, integrationPool, newProfile("web-small", "aws-eu", "archon-alice")); err != nil {
		t.Fatalf("Insert#1: %v", err)
	}
	err := Insert(ctx, integrationPool, newProfile("web-small", "aws-eu", "archon-alice"))
	if !errors.Is(err, ErrProfileAlreadyExists) {
		t.Fatalf("err = %v, want ErrProfileAlreadyExists", err)
	}
}

func TestIntegration_Insert_ProviderNotFound(t *testing.T) {
	setupProvider(t)
	ctx := context.Background()
	err := Insert(ctx, integrationPool, newProfile("web-small", "ghost-cloud", "archon-alice"))
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("err = %v, want ErrProviderNotFound", err)
	}
}

func TestIntegration_Insert_CreatedByFKViolation(t *testing.T) {
	setupProvider(t)
	ctx := context.Background()
	err := Insert(ctx, integrationPool, newProfile("web-small", "aws-eu", "archon-ghost"))
	if err == nil {
		t.Fatal("Insert with non-existent operator: expected error")
	}
	if errors.Is(err, ErrProviderNotFound) {
		t.Errorf("created_by FK should NOT map to ErrProviderNotFound; err = %v", err)
	}
}

func TestIntegration_ProviderDelete_Restricted(t *testing.T) {
	// PM-decision: FK ON DELETE RESTRICT — нельзя удалить Provider с
	// зависимыми Profile-ями.
	setupProvider(t)
	ctx := context.Background()
	if err := Insert(ctx, integrationPool, newProfile("web-small", "aws-eu", "archon-alice")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	_, err := integrationPool.Exec(ctx, `DELETE FROM providers WHERE name = 'aws-eu'`)
	if err == nil {
		t.Fatal("DELETE provider with dependent profile: expected RESTRICT violation")
	}
	// Profile должен остаться.
	if _, err := SelectByName(ctx, integrationPool, "web-small"); err != nil {
		t.Errorf("profile gone after blocked provider delete: %v", err)
	}
}

func TestIntegration_SelectByName_NotFound(t *testing.T) {
	setupProvider(t)
	_, err := SelectByName(context.Background(), integrationPool, "missing")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("err = %v, want ErrProfileNotFound", err)
	}
}

func TestIntegration_SelectAll_AndByProvider(t *testing.T) {
	setupProvider(t)
	ctx := context.Background()
	// Второй Provider для проверки фильтра.
	aid := "archon-alice"
	if err := provider.Insert(ctx, integrationPool, &provider.Provider{
		Name: "yc-ru", Type: "yc", Region: "ru-central1",
		CredentialsRef: "vault:secret/cloud/yc-ru", CreatedByAID: &aid,
	}); err != nil {
		t.Fatalf("seed second provider: %v", err)
	}

	for _, c := range []struct{ name, prov string }{
		{"web-a", "aws-eu"}, {"web-b", "aws-eu"}, {"db-c", "yc-ru"},
	} {
		if err := Insert(ctx, integrationPool, newProfile(c.name, c.prov, "archon-alice")); err != nil {
			t.Fatalf("Insert %s: %v", c.name, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	all, total, err := SelectAll(ctx, integrationPool, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Errorf("SelectAll total=%d len=%d, want 3/3", total, len(all))
	}

	byProv, total, err := SelectByProvider(ctx, integrationPool, "aws-eu", 0, 50)
	if err != nil {
		t.Fatalf("SelectByProvider: %v", err)
	}
	if total != 2 || len(byProv) != 2 {
		t.Errorf("SelectByProvider total=%d len=%d, want 2/2", total, len(byProv))
	}
	for _, p := range byProv {
		if p.Provider != "aws-eu" {
			t.Errorf("SelectByProvider returned %q-provider profile %q", p.Provider, p.Name)
		}
	}

	// Несуществующий Provider → пустая страница, total=0.
	empty, total, err := SelectByProvider(ctx, integrationPool, "ghost", 0, 50)
	if err != nil {
		t.Fatalf("SelectByProvider(ghost): %v", err)
	}
	if total != 0 || len(empty) != 0 {
		t.Errorf("SelectByProvider(ghost) total=%d len=%d, want 0/0", total, len(empty))
	}
}
