//go:build integration

// Integration tests for Augur CRUD (omens / rites) using testcontainers-go.
// Pattern matches keeper/internal/provider/integration_test.go.

package augur

import (
	"context"
	"encoding/json"
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
			log.Fatalf("augur integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("augur integration: skipping, docker unavailable: %v", err)
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
	// CASCADE: rites → omens → operators (FK chain).
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE rites, omens, operators, audit_log CASCADE`)
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

func newVaultOmen(name, aid string) *Omen {
	return &Omen{
		Name:         name,
		SourceType:   SourceVault,
		Endpoint:     "https://vault.internal:8200",
		AuthRef:      "vault:secret/keeper/augur/" + name,
		CreatedByAID: &aid,
	}
}

func TestIntegration_Omen_InsertSelectDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	o := newVaultOmen("vault-prod", "archon-alice")
	if err := InsertOmen(ctx, integrationPool, o); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	if o.CreatedAt.IsZero() {
		t.Error("CreatedAt zero — RETURNING did not fill")
	}

	got, err := SelectOmenByName(ctx, integrationPool, "vault-prod")
	if err != nil {
		t.Fatalf("SelectOmenByName: %v", err)
	}
	if got.SourceType != SourceVault || got.Endpoint != o.Endpoint {
		t.Errorf("got = %+v", got)
	}

	if err := DeleteOmen(ctx, integrationPool, "vault-prod"); err != nil {
		t.Fatalf("DeleteOmen: %v", err)
	}
	if _, err := SelectOmenByName(ctx, integrationPool, "vault-prod"); !errors.Is(err, ErrOmenNotFound) {
		t.Fatalf("after delete err = %v, want ErrOmenNotFound", err)
	}
}

func TestIntegration_Omen_DuplicateName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen#1: %v", err)
	}
	err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice"))
	if !errors.Is(err, ErrOmenAlreadyExists) {
		t.Fatalf("err = %v, want ErrOmenAlreadyExists", err)
	}
}

func TestIntegration_Omen_SourceTypeCHECK(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	// Direct INSERT bypassing Go validation: SQL CHECK should reject bad enum.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO omens (name, source_type, endpoint, auth_ref, created_by_aid)
		 VALUES ($1, 'mysql', 'e', 'vault:secret/x', $2)`,
		"bad-omen", "archon-alice")
	if err == nil {
		t.Fatal("expected CHECK violation for source_type='mysql'")
	}
}

func TestIntegration_Omen_NullCreatedByOnOperatorDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `DELETE FROM operators WHERE aid = 'archon-alice'`); err != nil {
		t.Fatalf("DELETE operator: %v", err)
	}
	got, err := SelectOmenByName(ctx, integrationPool, "vault-prod")
	if err != nil {
		t.Fatalf("SelectOmenByName: %v", err)
	}
	if got.CreatedByAID != nil {
		t.Errorf("CreatedByAID = %v after operator delete, want nil", got.CreatedByAID)
	}
}

func TestIntegration_Rite_RoundTrip_VaultDelegate(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}

	aid := "archon-alice"
	r := &Rite{
		Omen:         "vault-prod",
		Coven:        ptr("web"),
		Allow:        json.RawMessage(`{"paths":["secret/app/db"],"policies":["read-db"]}`),
		Delegate:     true,
		TokenTTL:     ptr("5m"),
		TokenNumUses: ptr(3),
		CreatedByAID: &aid,
	}
	if err := InsertRite(ctx, integrationPool, r); err != nil {
		t.Fatalf("InsertRite: %v", err)
	}
	if r.ID == 0 || r.CreatedAt.IsZero() {
		t.Errorf("id/created_at not filled: %d / %v", r.ID, r.CreatedAt)
	}

	got, err := SelectRitesByOmen(ctx, integrationPool, "vault-prod")
	if err != nil {
		t.Fatalf("SelectRitesByOmen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Delegate != true || got[0].TokenTTL == nil || *got[0].TokenTTL != "5m" {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
	if string(got[0].Allow) == "" {
		t.Errorf("allow round-trip empty")
	}
}

func TestIntegration_Rite_OmenCascadeDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	r := &Rite{Omen: "vault-prod", Coven: ptr("web"), Allow: json.RawMessage(`{"paths":["x"]}`)}
	if err := InsertRite(ctx, integrationPool, r); err != nil {
		t.Fatalf("InsertRite: %v", err)
	}
	// Deleting an Omen cascades to remove its Rites.
	if err := DeleteOmen(ctx, integrationPool, "vault-prod"); err != nil {
		t.Fatalf("DeleteOmen: %v", err)
	}
	got, err := SelectRitesByOmen(ctx, integrationPool, "vault-prod")
	if err != nil {
		t.Fatalf("SelectRitesByOmen: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rites survived omen delete: %d", len(got))
	}
}

func TestIntegration_Rite_SubjectXORCHECK(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	// Direct INSERT bypassing Go validation: both subjects → CHECK rites_subject_xor.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO rites (omen, coven, sid, allow) VALUES ('vault-prod', 'web', 'host', '{"paths":["x"]}')`)
	if err == nil {
		t.Fatal("expected XOR CHECK violation for both coven and sid")
	}
	// No subject → CHECK also rejects.
	_, err = integrationPool.Exec(ctx,
		`INSERT INTO rites (omen, allow) VALUES ('vault-prod', '{"paths":["x"]}')`)
	if err == nil {
		t.Fatal("expected XOR CHECK violation for no subject")
	}
}

func TestIntegration_Rite_TokenFieldsCHECK(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	// Direct INSERT: token_ttl with delegate=false → CHECK rites_token_fields_vault_only.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO rites (omen, coven, allow, delegate, token_ttl)
		 VALUES ('vault-prod', 'web', '{"paths":["x"]}', false, '5m')`)
	if err == nil {
		t.Fatal("expected CHECK violation for token_ttl with delegate=false")
	}
}

func TestIntegration_Rite_BySubject(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	// coven-Rite + sid-Rite on one Omen.
	covenRite := &Rite{Omen: "vault-prod", Coven: ptr("web"), Allow: json.RawMessage(`{"paths":["c"]}`)}
	sidRite := &Rite{Omen: "vault-prod", SID: ptr("host.example.com"), Allow: json.RawMessage(`{"paths":["s"]}`)}
	if err := InsertRite(ctx, integrationPool, covenRite); err != nil {
		t.Fatalf("InsertRite coven: %v", err)
	}
	if err := InsertRite(ctx, integrationPool, sidRite); err != nil {
		t.Fatalf("InsertRite sid: %v", err)
	}

	// Subject host.example.com with covens [web] → should match both Rites.
	got, err := SelectRitesBySubject(ctx, integrationPool, "host.example.com", []string{"web"})
	if err != nil {
		t.Fatalf("SelectRitesBySubject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (coven + sid)", len(got))
	}

	// Subject with no matching coven or sid → empty.
	none, err := SelectRitesBySubject(ctx, integrationPool, "other.host", []string{"db"})
	if err != nil {
		t.Fatalf("SelectRitesBySubject none: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("len = %d, want 0", len(none))
	}
}

func TestIntegration_DeleteRite(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	r := &Rite{Omen: "vault-prod", Coven: ptr("web"), Allow: json.RawMessage(`{"paths":["x"]}`)}
	if err := InsertRite(ctx, integrationPool, r); err != nil {
		t.Fatalf("InsertRite: %v", err)
	}
	if err := DeleteRite(ctx, integrationPool, r.ID); err != nil {
		t.Fatalf("DeleteRite: %v", err)
	}
	if err := DeleteRite(ctx, integrationPool, r.ID); !errors.Is(err, ErrRiteNotFound) {
		t.Fatalf("second delete err = %v, want ErrRiteNotFound", err)
	}
}
