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

	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ctr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("keeper_test"),
			tcpostgres.WithUsername("keeper"),
			tcpostgres.WithPassword("keeper"),
			tcpostgres.BasicWaitStrategies(),
		)
	})
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
		ID:           name,
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

	got, err := SelectOmenByID(ctx, integrationPool, "vault-prod")
	if err != nil {
		t.Fatalf("SelectOmenByID: %v", err)
	}
	if got.SourceType != SourceVault || got.Endpoint != o.Endpoint {
		t.Errorf("got = %+v", got)
	}

	if err := DeleteOmen(ctx, integrationPool, "vault-prod"); err != nil {
		t.Fatalf("DeleteOmen: %v", err)
	}
	if _, err := SelectOmenByID(ctx, integrationPool, "vault-prod"); !errors.Is(err, ErrOmenNotFound) {
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
		`INSERT INTO omens (id, source_type, endpoint, auth_ref, created_by_aid)
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
	got, err := SelectOmenByID(ctx, integrationPool, "vault-prod")
	if err != nil {
		t.Fatalf("SelectOmenByID: %v", err)
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
		Coven:        []string{"web"},
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
	r := &Rite{Omen: "vault-prod", Coven: []string{"web"}, Allow: json.RawMessage(`{"paths":["x"]}`)}
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

// TestIntegration_Rite_SubjectOneOfCHECK — direct INSERTs, bypassing Go
// validation, against `rites_subject_one_of` (NIM-280, migration 113). A Rite is
// the grant that lets a host read a secret, so "which hosts" must have exactly
// one answer: two dimensions is an ambiguous grant, none at all is a grant to
// the whole fleet.
//
// Every literal below is a real array (`ARRAY['web']`), not `'web'`. A bare
// string against a text[] column is refused by the type system BEFORE any CHECK
// runs, which would leave this test green while proving nothing about the
// constraint it names.
func TestIntegration_Rite_SubjectOneOfCHECK(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}

	cases := []struct {
		name    string
		columns string
		values  string
	}{
		{"coven+sid", "coven, sid", "ARRAY['web'], ARRAY['host']"},
		{"coven+incarnation", "coven, service, incarnation", "ARRAY['web'], 'redis', 'redis-prod'"},
		{"sid+trait", "sid, trait_key, trait_value", "ARRAY['host'], 'tier', 'gold'"},
		{"service-without-incarnation", "service", "'redis'"},
		{"trait-key-without-value", "trait_key", "'tier'"},
		{"none", "", ""},
		// An empty array is not a dimension — `array_length(…, 1)` is NULL, so the
		// CHECK counts it as absent rather than as a grant with no restriction.
		{"empty-coven-array", "coven", "ARRAY[]::TEXT[]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cols, vals := "omen, allow", `'vault-prod', '{"paths":["x"]}'`
			if c.columns != "" {
				cols, vals = cols+", "+c.columns, vals+", "+c.values
			}
			_, err := integrationPool.Exec(ctx, `INSERT INTO rites (`+cols+`) VALUES (`+vals+`)`)
			if err == nil {
				t.Fatal("expected a CHECK violation: the subject must be exactly one dimension")
			}
		})
	}

	// The control: one dimension, spelled correctly, is accepted. Without it every
	// assertion above would also pass against a column list that simply does not
	// exist.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rites (omen, service, incarnation, allow)
		 VALUES ('vault-prod', 'redis', 'redis-prod', '{"paths":["x"]}')`); err != nil {
		t.Fatalf("a single-dimension subject must be accepted: %v", err)
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
		 VALUES ('vault-prod', ARRAY['web'], '{"paths":["x"]}', false, '5m')`)
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
	covenRite := &Rite{Omen: "vault-prod", Coven: []string{"web"}, Allow: json.RawMessage(`{"paths":["c"]}`)}
	sidRite := &Rite{Omen: "vault-prod", SID: []string{"host.example.com"}, Allow: json.RawMessage(`{"paths":["s"]}`)}
	if err := InsertRite(ctx, integrationPool, covenRite); err != nil {
		t.Fatalf("InsertRite coven: %v", err)
	}
	if err := InsertRite(ctx, integrationPool, sidRite); err != nil {
		t.Fatalf("InsertRite sid: %v", err)
	}

	// Subject host.example.com with covens [web] → should match both Rites.
	host := subject.Host{SID: "host.example.com", Covens: []string{"web"}}
	got, err := SelectRitesBySubject(ctx, integrationPool, host)
	if err != nil {
		t.Fatalf("SelectRitesBySubject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (coven + sid)", len(got))
	}

	// Subject with no matching coven or sid → empty.
	other := subject.Host{SID: "other.host", Covens: []string{"db"}}
	none, err := SelectRitesBySubject(ctx, integrationPool, other)
	if err != nil {
		t.Fatalf("SelectRitesBySubject none: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("len = %d, want 0", len(none))
	}
}

// TestIntegration_Rite_BySubject_EveryDimension — one Rite per dimension on one
// Omen, and one host that satisfies all four. The prefilter is a single
// predicate over four OR-arms (subject.MatchSQL): an arm that binds the wrong
// argument or names the wrong column drops its Rite silently, and the caller
// sees a shorter grant list rather than an error.
func TestIntegration_Rite_BySubject_EveryDimension(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}

	svc, inc := "redis", "redis-prod"
	key, value := "tier", "gold"
	rites := map[string]*Rite{
		"sid":         {Omen: "vault-prod", SID: []string{"host.example.com"}, Allow: json.RawMessage(`{"paths":["s"]}`)},
		"incarnation": {Omen: "vault-prod", Service: &svc, Incarnation: &inc, Allow: json.RawMessage(`{"paths":["i"]}`)},
		"coven":       {Omen: "vault-prod", Coven: []string{"web"}, Allow: json.RawMessage(`{"paths":["c"]}`)},
		"trait":       {Omen: "vault-prod", TraitKey: &key, TraitValue: &value, Allow: json.RawMessage(`{"paths":["t"]}`)},
	}
	byID := map[int64]string{}
	for dim, r := range rites {
		if err := InsertRite(ctx, integrationPool, r); err != nil {
			t.Fatalf("InsertRite(%s): %v", dim, err)
		}
		byID[r.ID] = dim
	}

	// A host that satisfies all four at once: its own SID and label, plus an
	// incarnation whose roster it is on, carrying the trait.
	host := subject.Host{
		SID:    "host.example.com",
		Covens: []string{"web"},
		Member: []subject.Incarnation{{
			Service: svc, Name: inc,
			Traits: map[string]any{key: value},
		}},
	}
	got, err := SelectRitesBySubject(ctx, integrationPool, host)
	if err != nil {
		t.Fatalf("SelectRitesBySubject: %v", err)
	}
	matched := map[string]bool{}
	for _, r := range got {
		matched[byID[r.ID]] = true
	}
	for dim := range rites {
		if !matched[dim] {
			t.Errorf("the %s dimension did not reach the host: matched %v", dim, matched)
		}
	}

	// A host sharing nothing with any of the four matches none of them: the arms
	// are selective, not a predicate that happens to be true.
	stranger := subject.Host{SID: "stranger.example.com", Covens: []string{"db"}}
	none, err := SelectRitesBySubject(ctx, integrationPool, stranger)
	if err != nil {
		t.Fatalf("SelectRitesBySubject(stranger): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an unrelated host matched %d rites, want 0 (default-deny)", len(none))
	}
}

func TestIntegration_DeleteRite(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	if err := InsertOmen(ctx, integrationPool, newVaultOmen("vault-prod", "archon-alice")); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	r := &Rite{Omen: "vault-prod", Coven: []string{"web"}, Allow: json.RawMessage(`{"paths":["x"]}`)}
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
