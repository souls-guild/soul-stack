//go:build integration

// L1 integration for the push-run host-scope pushdown (NIM-842).
//
// WHY this package grew a Postgres. `internal/pushorch` had no PG suite at all —
// its only integration file is a skipped S6 skeleton — so [HostScopeSQL] was the
// one narrowing predicate on a read path whose SQL had never met a database. It
// is a correlated NOT EXISTS over `unnest(inventory_sids)` LEFT JOINed onto
// `souls` and closed with `IS NOT TRUE`; the unit guards can assert its text and
// nothing else.
//
// The same three properties as the Voyage twin live only here: ALL-not-ANY (a run
// whose inventory straddles the boundary is hidden entirely, because showing it
// means showing the SIDs on the far side), `IS NOT TRUE` (a host absent from
// `souls` makes the coven overlap NULL, and plain `NOT` would read that as
// in-scope — fail-OPEN), and `total` counted under the same WHERE.

package pushorch

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(runIntegration(m)) }

func runIntegration(m *testing.M) int {
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
		if integrationenv.RequireDocker() {
			log.Fatalf("pushorch integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("pushorch integration: skipping, docker unavailable: %v", err)
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

// scopeFor renders a purview over this package's subquery aliases and wraps it in
// [HostScopeSQL] — exactly what keeper/internal/api/handlers hands to the store.
func scopeFor(t *testing.T, expr string) func(int) (string, []any, int) {
	t.Helper()
	cols := rbac.ScopeColumns{Coven: ScopeCovenColumn, Host: ScopeHostColumn, Traits: ScopeTraitsColumn}
	var p rbac.Purview
	switch expr {
	case "":
		// zero Purview → FALSE per host (entitled to nothing)
	case "*":
		p = rbac.Purview{Unrestricted: true}
	default:
		e, err := rbac.ParseScopeExpr(expr)
		if err != nil {
			t.Fatalf("ParseScopeExpr(%q): %v", expr, err)
		}
		p = rbac.Purview{Exprs: []*rbac.ScopeExpr{e}}
	}
	return func(startIdx int) (string, []any, int) {
		return HostScopeSQL(func(i int) (string, []any, int) {
			return rbac.PurviewSQL(p, cols, i)
		}, startIdx)
	}
}

func seedPushScopeFixture(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx, `TRUNCATE TABLE push_runs, souls, operators CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-push-it', 'Push IT', 'jwt')`); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	for _, s := range []struct {
		sid    string
		covens []string
	}{
		{"dev-web-01", []string{"dev"}},
		{"dev-web-02", []string{"dev"}},
		{"prod-db-01", []string{"prod"}},
		// retired-01 is deliberately absent.
	} {
		if _, err := integrationPool.Exec(ctx, `
			INSERT INTO souls (sid, coven, status) VALUES ($1, $2, 'connected')`,
			s.sid, s.covens); err != nil {
			t.Fatalf("seed soul %s: %v", s.sid, err)
		}
	}

	store := NewStore(integrationPool)
	for _, r := range []PushRunRow{
		{ApplyID: "01H0000000000000000000DEV1", InventorySIDs: []string{"dev-web-01", "dev-web-02"}},
		{ApplyID: "01H0000000000000000000MIX1", InventorySIDs: []string{"dev-web-01", "prod-db-01"}},
		{ApplyID: "01H000000000000000000PROD1", InventorySIDs: []string{"prod-db-01"}},
		{ApplyID: "01H000000000000000000GONE1", InventorySIDs: []string{"retired-01"}},
	} {
		r.DestinyRef = "git@example:d.git#v1"
		r.StartedByAID = "archon-push-it"
		r.StartedByKID = "keeper-push-it"
		if err := store.Insert(ctx, r); err != nil {
			t.Fatalf("Insert(%s): %v", r.ApplyID, err)
		}
	}
	return store
}

func listPushIDs(t *testing.T, store *Store, expr string) ([]string, int) {
	t.Helper()
	rows, total, err := store.SelectAll(context.Background(),
		ListFilter{HostScope: scopeFor(t, expr)}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll(%q): %v", expr, err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ApplyID)
	}
	return ids, total
}

func containsID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// TestIntegration_PushList_HidesRunsThatStraddleTheBoundary — the ALL rule.
// Showing a run means showing its whole `inventory_sids`, so one host on the far
// side of the boundary hides the run entirely.
func TestIntegration_PushList_HidesRunsThatStraddleTheBoundary(t *testing.T) {
	store := seedPushScopeFixture(t)

	ids, total := listPushIDs(t, store, "coven=dev")

	if !containsID(ids, "01H0000000000000000000DEV1") {
		t.Errorf("the all-dev run is missing from %v", ids)
	}
	if containsID(ids, "01H0000000000000000000MIX1") {
		t.Errorf("a run whose inventory straddles dev and prod was shown to a dev-scoped "+
			"operator: %v", ids)
	}
	if containsID(ids, "01H000000000000000000PROD1") {
		t.Errorf("an all-prod run was shown to a dev-scoped operator: %v", ids)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 — the COUNT must run under the same WHERE as the page; ids=%v",
			total, ids)
	}
}

// TestIntegration_PushList_UnknownHostIsFailClosed — the `IS NOT TRUE` arm.
func TestIntegration_PushList_UnknownHostIsFailClosed(t *testing.T) {
	store := seedPushScopeFixture(t)

	ids, _ := listPushIDs(t, store, "coven=dev")

	if containsID(ids, "01H000000000000000000GONE1") {
		t.Errorf("a run against a host absent from souls was shown to a coven-scoped "+
			"operator — the NULL predicate read as in-scope: %v", ids)
	}
}

// TestIntegration_PushList_EmptyPurviewShowsNothing — the ticket's failure
// scenario: `push.read` held, no soul right at all. Every run is a list of
// machines, so every run is hidden.
func TestIntegration_PushList_EmptyPurviewShowsNothing(t *testing.T) {
	store := seedPushScopeFixture(t)

	ids, total := listPushIDs(t, store, "")

	if len(ids) != 0 || total != 0 {
		t.Errorf("rows = %v, total = %d; an operator entitled to no host must see none", ids, total)
	}
}

// TestIntegration_PushList_UnrestrictedSeesEveryRun — the other direction, so the
// guards above are not satisfied by a predicate that hides everything.
func TestIntegration_PushList_UnrestrictedSeesEveryRun(t *testing.T) {
	store := seedPushScopeFixture(t)

	ids, total := listPushIDs(t, store, "*")

	if len(ids) != 4 || total != 4 {
		t.Errorf("rows = %v, total = %d; an unrestricted operator must see all 4", ids, total)
	}
}

// TestIntegration_PushGetScoped_OutOfScopeIsNotFound — the single-object read
// narrows in the WHERE, so out-of-boundary and absent are ONE answer. Store.Get
// stays unnarrowed for the orchestrator.
func TestIntegration_PushGetScoped_OutOfScopeIsNotFound(t *testing.T) {
	store := seedPushScopeFixture(t)
	ctx := context.Background()

	if _, err := store.GetScoped(ctx, "01H000000000000000000PROD1", scopeFor(t, "coven=dev")); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound for an out-of-scope run", err)
	}
	if _, err := store.GetScoped(ctx, "01H0000000000000000000DEV1", scopeFor(t, "coven=dev")); err != nil {
		t.Errorf("err = %v, want the in-scope run to be served", err)
	}
	if _, err := store.Get(ctx, "01H000000000000000000PROD1"); err != nil {
		t.Errorf("Store.Get must stay unnarrowed for the orchestrator: %v", err)
	}
}
