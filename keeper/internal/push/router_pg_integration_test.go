//go:build integration

// Integration test for the Level 2 read of the provider router against a REAL
// Postgres (NIM-251, ADR-080 label inheritance).
//
// A live PG rather than a fake reader: the defect was in WHICH relation the
// reader consulted — `souls.coven[]` alone instead of that column unioned with
// the labels of `incarnation_membership ⋈ incarnation` — and a fake answering
// both questions from one field cannot fail the way production did.
//
// Run:
//
//	SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -count=1 -p 1 ./internal/push/
//
// The container is per-test (the package has no TestMain, and the sshd
// integration test above must not pay for a Postgres it never touches).

package push

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

// requireDockerPush reports whether CI requires docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Without it an unavailable
// docker daemon skips instead of failing — but a skip is NOT a green run.
func requireDockerPush() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

// newRouterPGPool spins a migrated Postgres for one test and returns a pool.
func newRouterPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDockerPush() {
			t.Fatalf("push router integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		t.Skipf("push router integration: skipping, docker unavailable: %v", err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedRouterSoul(t *testing.T, pool *pgxpool.Pool, sid string, coven []string) {
	t.Helper()
	s := &soul.Soul{SID: sid, Status: soul.StatusPending, Coven: coven}
	if err := soul.Insert(context.Background(), pool, s); err != nil {
		t.Fatalf("seedRouterSoul(%s): %v", sid, err)
	}
}

func seedRouterIncarnation(t *testing.T, pool *pgxpool.Pool, name string, covens []string, sids ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO incarnation (name, service, service_version, status, covens)
		 VALUES ($1, 'redis', 'v1.0.0', 'ready', $2)`, name, covens); err != nil {
		t.Fatalf("seedRouterIncarnation(%s): %v", name, err)
	}
	for _, sid := range sids {
		if _, err := pool.Exec(ctx,
			`INSERT INTO incarnation_membership (incarnation_name, sid) VALUES ($1, $2)`,
			name, sid); err != nil {
			t.Fatalf("seedMembership(%s, %s): %v", name, sid, err)
		}
	}
}

// TestIntegration_Router_IncarnationLabelRoutesItsHosts — GUARD (NIM-251): the
// acceptance case. A host of an incarnation for which a per-coven provider is
// configured routes onto that provider, not onto the cluster default. The host
// carries NO such tag of its own — the label is on the incarnation, which is the
// recommended shape (ADR-080: "prefer labelling the incarnation").
//
// The label reaches the router both ways ADR-080 defines inheritance: through
// the incarnation's `covens[]` and through its NAME.
func TestIntegration_Router_IncarnationLabelRoutesItsHosts(t *testing.T) {
	pool := newRouterPGPool(t)
	ctx := context.Background()

	seedRouterSoul(t, pool, "by-tag.example.com", []string{"db"})
	seedRouterSoul(t, pool, "by-name.example.com", []string{"db"})
	seedRouterIncarnation(t, pool, "redis-eu", []string{"eu-bastioned"}, "by-tag.example.com")
	// No covens of its own — this host inherits purely through the NAME.
	seedRouterIncarnation(t, pool, "redis-prod", []string{}, "by-name.example.com")

	router, err := NewPGRouter(NewPGRouterReader(pool), NewStaticRouterConfigSource(RouterConfig{
		CovenDefaultProviders: map[string]string{
			"eu-bastioned": "bastion-eu",
			"redis-prod":   "bastion-prod",
		},
		ClusterDefaultProvider: "static-fallback",
	}))
	if err != nil {
		t.Fatalf("NewPGRouter: %v", err)
	}

	for _, tc := range []struct {
		sid, want string
	}{
		{"by-tag.example.com", "bastion-eu"},    // incarnation.covens[]
		{"by-name.example.com", "bastion-prod"}, // incarnation.name
	} {
		name, src, err := router.RouteFor(ctx, tc.sid)
		if err != nil {
			t.Fatalf("RouteFor(%s): %v", tc.sid, err)
		}
		if name != tc.want || src != SourceCoven {
			t.Errorf("RouteFor(%s) = (%q, %v), want (%q, SourceCoven) — inherited label must reach Level 2",
				tc.sid, name, src, tc.want)
		}
	}
}

// TestIntegration_Router_NonMemberKeepsClusterDefault — GUARD (NIM-251): the
// widening is bounded by MEMBERSHIP, not by a tag that merely looks like one.
// A host outside every incarnation still falls through to the cluster default,
// so inheritance cannot hand an unrelated host a bastion it was never bound to.
func TestIntegration_Router_NonMemberKeepsClusterDefault(t *testing.T) {
	pool := newRouterPGPool(t)
	ctx := context.Background()

	seedRouterSoul(t, pool, "member.example.com", []string{"db"})
	seedRouterSoul(t, pool, "outsider.example.com", []string{"db"})
	seedRouterIncarnation(t, pool, "redis-prod", []string{"eu-bastioned"}, "member.example.com")

	router, err := NewPGRouter(NewPGRouterReader(pool), NewStaticRouterConfigSource(RouterConfig{
		CovenDefaultProviders:  map[string]string{"eu-bastioned": "bastion-eu"},
		ClusterDefaultProvider: "static-fallback",
	}))
	if err != nil {
		t.Fatalf("NewPGRouter: %v", err)
	}

	name, src, err := router.RouteFor(ctx, "outsider.example.com")
	if err != nil {
		t.Fatalf("RouteFor(outsider): %v", err)
	}
	if name != "static-fallback" || src != SourceCluster {
		t.Errorf("outsider = (%q, %v), want (static-fallback, SourceCluster)", name, src)
	}
}

// TestIntegration_Router_OwnTagStillWins — GUARD (NIM-251): inheritance is
// purely additive on live data. The host's own tag and its inherited one are
// both configured, and the own one wins even though the inherited sorts first —
// so an existing fleet does not silently move to another bastion the day an
// incarnation gains a label.
func TestIntegration_Router_OwnTagStillWins(t *testing.T) {
	pool := newRouterPGPool(t)
	ctx := context.Background()

	seedRouterSoul(t, pool, "host.example.com", []string{"zeta"})
	seedRouterIncarnation(t, pool, "redis-prod", []string{"alpha"}, "host.example.com")

	router, err := NewPGRouter(NewPGRouterReader(pool), NewStaticRouterConfigSource(RouterConfig{
		CovenDefaultProviders: map[string]string{"alpha": "a-provider", "zeta": "z-provider"},
	}))
	if err != nil {
		t.Fatalf("NewPGRouter: %v", err)
	}

	name, src, err := router.RouteFor(ctx, "host.example.com")
	if err != nil {
		t.Fatalf("RouteFor: %v", err)
	}
	if name != "z-provider" || src != SourceCoven {
		t.Errorf("got (%q, %v), want (z-provider, SourceCoven) — own tag outranks inherited", name, src)
	}
}
