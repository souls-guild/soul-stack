//go:build integration

// Integration tests for the Service registry / keeper_settings CRUD via
// testcontainers-go. Pattern matches keeper/internal/augur/integration_test.go.

package serviceregistry

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
			log.Fatalf("serviceregistry integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("serviceregistry integration: skipping, docker unavailable: %v", err)
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
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE service_registry, keeper_settings, operators, audit_log CASCADE`)
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

func newService(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(ServiceDeps{Pool: integrationPool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func ptrStr(s string) *string { return &s }

func TestIntegration_Service_CRUDRoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	svc := newService(t)
	aid := "archon-alice"

	created, err := svc.CreateService(ctx, CreateServiceInput{
		Name: "web", Git: "git@example.com:web.git", Ref: "v1.0.0",
		Refresh: ptrStr("5m"), CallerAID: &aid,
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("timestamps not filled: %+v", created)
	}

	got, err := svc.GetService(ctx, "web")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Git != "git@example.com:web.git" || got.Ref != "v1.0.0" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Refresh == nil || *got.Refresh != "5m" {
		t.Errorf("refresh round-trip: %v", got.Refresh)
	}
	if got.CreatedByAID == nil || *got.CreatedByAID != aid {
		t.Errorf("created_by_aid round-trip: %v", got.CreatedByAID)
	}

	updated, err := svc.UpdateService(ctx, UpdateServiceInput{
		Name: "web", Git: "git@example.com:web.git", Ref: "main",
		Refresh: nil, CallerAID: &aid,
	})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if updated.Ref != "main" || updated.Refresh != nil {
		t.Errorf("update mismatch: %+v", updated)
	}

	if err := svc.DeleteService(ctx, "web"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := svc.GetService(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
}

func TestIntegration_Service_DuplicateName(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	svc := newService(t)
	if _, err := svc.CreateService(ctx, CreateServiceInput{Name: "web", Git: "g", Ref: "v1"}); err != nil {
		t.Fatalf("CreateService#1: %v", err)
	}
	_, err := svc.CreateService(ctx, CreateServiceInput{Name: "web", Git: "g2", Ref: "v2"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestIntegration_Service_NameFormatCHECK(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// Direct INSERT bypassing Go validation: the SQL CHECK must reject a bad name.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO service_registry (name, git, ref) VALUES ('Bad_Name', 'g', 'r')`)
	if err == nil {
		t.Fatal("expected CHECK violation for name='Bad_Name'")
	}
}

func TestIntegration_Service_GitRefNonemptyCHECK(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// Direct INSERT: empty git → CHECK service_registry_git_nonempty.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO service_registry (name, git, ref) VALUES ('web', '', 'r')`); err == nil {
		t.Fatal("expected CHECK violation for empty git")
	}
	// Empty ref → CHECK service_registry_ref_nonempty.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO service_registry (name, git, ref) VALUES ('web', 'g', '')`); err == nil {
		t.Fatal("expected CHECK violation for empty ref")
	}
}

func TestIntegration_Service_UpdateNotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	svc := newService(t)
	_, err := svc.UpdateService(ctx, UpdateServiceInput{Name: "ghost", Git: "g", Ref: "r"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateService missing = %v, want ErrNotFound", err)
	}
}

func TestIntegration_Service_DeleteNotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	svc := newService(t)
	if err := svc.DeleteService(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteService missing = %v, want ErrNotFound", err)
	}
}

func TestIntegration_Service_UnknownOperatorFK(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	svc := newService(t)
	// Operator not seeded → FK-violation on created_by_aid.
	_, err := svc.CreateService(ctx, CreateServiceInput{
		Name: "web", Git: "g", Ref: "r", CallerAID: ptrStr("archon-ghost"),
	})
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("CreateService unknown operator = %v, want ErrOperatorNotFound", err)
	}
}

func TestIntegration_Service_List(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	svc := newService(t)
	for _, n := range []string{"web", "api", "db"} {
		if _, err := svc.CreateService(ctx, CreateServiceInput{Name: n, Git: "g", Ref: "v1"}); err != nil {
			t.Fatalf("CreateService(%s): %v", n, err)
		}
	}
	got, err := svc.ListServices(ctx)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// ORDER BY name ASC.
	if got[0].Name != "api" || got[1].Name != "db" || got[2].Name != "web" {
		t.Errorf("order mismatch: %s, %s, %s", got[0].Name, got[1].Name, got[2].Name)
	}
}

func TestIntegration_Setting_UpsertRoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	svc := newService(t)
	aid := "archon-alice"

	if _, err := svc.GetSetting(ctx, SettingDefaultDestinySource); !errors.Is(err, ErrSettingNotFound) {
		t.Fatalf("GetSetting before set = %v, want ErrSettingNotFound", err)
	}

	set, err := svc.SetSetting(ctx, SetSettingInput{
		Key: SettingDefaultDestinySource, Value: "git@example.com:destiny.git", CallerAID: &aid,
	})
	if err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if set.UpdatedAt.IsZero() {
		t.Error("UpdatedAt zero — RETURNING did not fill")
	}

	got, err := svc.GetSetting(ctx, SettingDefaultDestinySource)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got.Value != "git@example.com:destiny.git" {
		t.Errorf("value mismatch: %q", got.Value)
	}

	// Repeated SetSetting — upsert (update value).
	if _, err := svc.SetSetting(ctx, SetSettingInput{
		Key: SettingDefaultDestinySource, Value: "git@example.com:destiny2.git",
	}); err != nil {
		t.Fatalf("SetSetting upsert: %v", err)
	}
	got2, err := svc.GetSetting(ctx, SettingDefaultDestinySource)
	if err != nil {
		t.Fatalf("GetSetting after upsert: %v", err)
	}
	if got2.Value != "git@example.com:destiny2.git" {
		t.Errorf("upsert value mismatch: %q", got2.Value)
	}
	// updated_by_aid reset to nil (second call without CallerAID).
	if got2.UpdatedByAID != nil {
		t.Errorf("updated_by_aid = %v after upsert without caller, want nil", got2.UpdatedByAID)
	}
}

func TestIntegration_Setting_KeyFormatCHECK(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// Direct INSERT bypassing Go validation: the SQL CHECK must reject a bad key.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO keeper_settings (key, value) VALUES ('Bad-Key', 'v')`)
	if err == nil {
		t.Fatal("expected CHECK violation for key='Bad-Key'")
	}
}

func TestIntegration_Service_NullCreatedByOnOperatorDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	svc := newService(t)
	aid := "archon-alice"
	// CreateService fills both created_by_aid AND updated_by_aid with the author.
	if _, err := svc.CreateService(ctx, CreateServiceInput{Name: "web", Git: "g", Ref: "r", CallerAID: &aid}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	// FK on created_by_aid / updated_by_aid — ON DELETE SET NULL: deleting
	// the author operator SUCCEEDS, the Service record survives offboarding.
	if _, err := integrationPool.Exec(ctx, `DELETE FROM operators WHERE aid = 'archon-alice'`); err != nil {
		t.Fatalf("expected FK SET NULL: deleting operator that authored a service must succeed: %v", err)
	}
	// Both audit fields reset to nil, the record is intact.
	got, err := svc.GetService(ctx, "web")
	if err != nil {
		t.Fatalf("GetService after operator delete: %v", err)
	}
	if got.CreatedByAID != nil {
		t.Errorf("created_by_aid = %v after author delete, want nil", got.CreatedByAID)
	}
	if got.UpdatedByAID != nil {
		t.Errorf("updated_by_aid = %v after author delete, want nil", got.UpdatedByAID)
	}
}
