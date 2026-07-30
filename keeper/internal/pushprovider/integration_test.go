//go:build integration

// L1 integration for the push_providers CRUD layer (NIM-239).
//
// WHY THIS PACKAGE FIRST. docs/testing/README.md is explicit — "CRUD layer /
// DB migration / Vault client → L1 integration_test.go" — and this package is
// the plainest possible instance of that rule: it owns INSERT / SELECT /
// UPDATE / DELETE against a real table and nothing else. Yet nothing anywhere
// ran those statements against a Postgres. `crud_test.go` is entirely on stubs,
// and the two suites above it (internal/api/handlers/pushprovider_test.go,
// internal/api/huma_pushprovider_test.go) carry no `integration` build tag
// either, so the column names, the jsonb round-trip, the unique constraint and
// the migration that creates the table were verified by nobody.
//
// A stubbed CRUD test proves the Go code calls the pool it was handed. It
// cannot notice that a column was renamed, that a NOT NULL arrived, or that
// `params` stopped round-tripping — which are exactly the failures this layer
// exists to have.
package pushprovider

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
			log.Fatalf("pushprovider integration: setup failed (docker required): %v", err)
		}
		log.Printf("pushprovider integration: skipping, docker unavailable: %v", err)
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

const testAID = "archon-alice"

// resetAll truncates and seeds the operator the FK created_by_aid points at.
func resetAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE TABLE push_providers, operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	op := &operator.Operator{
		AID: testAID, DisplayName: testAID, AuthMethod: operator.AuthMethodJWT,
	}
	if err := operator.Insert(ctx, integrationPool, op); err != nil {
		t.Fatalf("seedOperator: %v", err)
	}
}

func newProvider(name string) *PushProvider {
	return &PushProvider{
		Name:         name,
		Params:       map[string]any{"host": "bastion.example.com", "port": float64(22)},
		CreatedByAID: testAID,
	}
}

// TestIntegration_PushProvider_RoundTrip — the whole point of an L1 for a CRUD
// layer: what went into the jsonb column comes back out the same. A stub cannot
// answer this, because the marshalling happens in the driver and the storage in
// Postgres.
func TestIntegration_PushProvider_RoundTrip(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	in := newProvider("bastion-eu")
	if err := Insert(ctx, integrationPool, in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, "bastion-eu")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Name != "bastion-eu" {
		t.Errorf("Name = %q, want bastion-eu", got.Name)
	}
	if got.Params["host"] != "bastion.example.com" {
		t.Errorf("params.host = %v, want bastion.example.com — the jsonb round-trip lost a value", got.Params["host"])
	}
	// Numbers come back through jsonb as float64. Pinning it is the point: a
	// caller that type-asserts int would compile and then panic in production.
	if got.Params["port"] != float64(22) {
		t.Errorf("params.port = %#v (%T), want float64(22)", got.Params["port"], got.Params["port"])
	}
	if got.CreatedByAID != testAID {
		t.Errorf("CreatedByAID = %q, want %q", got.CreatedByAID, testAID)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero — the DB default did not populate it")
	}
	if got.UpdatedByAID != nil {
		t.Errorf("UpdatedByAID = %v on a fresh row, want nil", *got.UpdatedByAID)
	}
}

// TestIntegration_PushProvider_NameIsUnique — the PK is the contract the
// handler's 409 rests on. Only the database can enforce it, so only a real
// database can prove the sentinel is wired to the right constraint.
func TestIntegration_PushProvider_NameIsUnique(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newProvider("bastion-eu")); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err := Insert(ctx, integrationPool, newProvider("bastion-eu"))
	if !errors.Is(err, ErrPushProviderAlreadyExists) {
		t.Fatalf("second Insert: err = %v, want ErrPushProviderAlreadyExists", err)
	}
}

// TestIntegration_PushProvider_NotFound — SelectByName / Update / Delete must
// all agree on what "absent" means, because the handler maps that one sentinel
// to 404. Divergence here is a 500 the operator cannot act on.
func TestIntegration_PushProvider_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	if _, err := SelectByName(ctx, integrationPool, "nope"); !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("SelectByName: err = %v, want ErrPushProviderNotFound", err)
	}
	if err := Update(ctx, integrationPool, "nope", map[string]any{"host": "x"}, testAID); !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("Update: err = %v, want ErrPushProviderNotFound", err)
	}
	if err := Delete(ctx, integrationPool, "nope"); !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("Delete: err = %v, want ErrPushProviderNotFound", err)
	}
}

// TestIntegration_PushProvider_UpdateReplacesParamsAndStampsAuthor — the update
// path writes both the jsonb and the audit columns; a stub would report success
// for either half alone.
func TestIntegration_PushProvider_UpdateReplacesParamsAndStampsAuthor(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, newProvider("bastion-eu")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := Update(ctx, integrationPool, "bastion-eu",
		map[string]any{"host": "moved.example.com"}, "archon-alice"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, "bastion-eu")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Params["host"] != "moved.example.com" {
		t.Errorf("params.host = %v, want moved.example.com", got.Params["host"])
	}
	if got.UpdatedByAID == nil || *got.UpdatedByAID != testAID {
		t.Errorf("UpdatedByAID = %v, want %q — the update did not stamp its author", got.UpdatedByAID, testAID)
	}
}

// TestIntegration_PushProvider_ListAndDelete — SelectAll's ordering and total,
// then removal.
//
// The ordering is `updated_at DESC, name ASC` — newest first, ties broken by
// name. Worth pinning against a real clock rather than a stub: the tiebreaker
// only shows up when two rows share a timestamp, which a stub returning a fixed
// slice can never produce, and the DESC half is what makes the list feel like
// "recently touched" to an operator. Getting it wrong is not a crash, it is a
// list that looks plausible and is in the wrong order.
func TestIntegration_PushProvider_ListAndDelete(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	for _, n := range []string{"bastion-eu", "bastion-us", "bastion-ap"} {
		if err := Insert(ctx, integrationPool, newProvider(n)); err != nil {
			t.Fatalf("Insert(%s): %v", n, err)
		}
	}

	all, total, err := SelectAll(ctx, integrationPool, ListFilter{}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("SelectAll: total=%d len=%d, want 3/3", total, len(all))
	}
	// All three land in one transaction-less burst, so updated_at may or may not
	// tie. Assert what the contract guarantees in both cases: the set is right,
	// and it is sorted by (updated_at DESC, name ASC) — verified pairwise rather
	// than against a hardcoded permutation, which would be asserting the clock.
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if cur.UpdatedAt.After(prev.UpdatedAt) {
			t.Errorf("SelectAll[%d] (%s) is newer than [%d] (%s) — order is not updated_at DESC",
				i, cur.Name, i-1, prev.Name)
		}
		if cur.UpdatedAt.Equal(prev.UpdatedAt) && cur.Name < prev.Name {
			t.Errorf("SelectAll: %q before %q on equal updated_at — the name ASC tiebreaker is gone",
				prev.Name, cur.Name)
		}
	}
	seen := map[string]bool{}
	for _, p := range all {
		seen[p.Name] = true
	}
	for _, n := range []string{"bastion-eu", "bastion-us", "bastion-ap"} {
		if !seen[n] {
			t.Errorf("SelectAll did not return %q", n)
		}
	}

	if err := Delete(ctx, integrationPool, "bastion-eu"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := SelectByName(ctx, integrationPool, "bastion-eu"); !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("after Delete: err = %v, want ErrPushProviderNotFound", err)
	}
	if _, total, _ = SelectAll(ctx, integrationPool, ListFilter{}, 0, 10); total != 2 {
		t.Errorf("total after Delete = %d, want 2", total)
	}
}
