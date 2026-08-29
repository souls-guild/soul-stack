//go:build integration

// Integration test for the Level 2 read of the provider router against a REAL
// Postgres (NIM-251, NIM-281).
//
// The router resolves a provider from a host's coven labels, and under NIM-281
// those are `souls.coven[]` and nothing else: a bind to an incarnation attaches
// no label, so an incarnation's tags and its name route none of its hosts.
//
// A live PG rather than a fake reader: the question is WHICH relation the reader
// consults, and a fake answering every question from one field can neither fail
// the way NIM-251 did nor widen the way NIM-281 forbids.
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
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
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
	return integrationenv.RequireDocker()
}

// newRouterPGPool spins a migrated Postgres for one test and returns a pool.
func newRouterPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
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

// TestIntegration_Router_IncarnationLabelDoesNotRouteItsHosts — GUARD (NIM-281):
// an incarnation's labels route none of its members, neither through its
// `covens[]` nor through its NAME. Both hosts below are bound to an incarnation
// whose label has a provider configured, and both must fall through to the
// cluster default, because neither host carries that label itself.
//
// The last leg tags one of them by hand and expects the bastion, so a green run
// cannot be read as "the per-coven config was never live": same host, same
// router, one operator-made tag apart.
func TestIntegration_Router_IncarnationLabelDoesNotRouteItsHosts(t *testing.T) {
	pool := newRouterPGPool(t)
	ctx := context.Background()

	seedRouterSoul(t, pool, "by-tag.example.com", []string{"db"})
	seedRouterSoul(t, pool, "by-name.example.com", []string{"db"})
	seedRouterIncarnation(t, pool, "redis-eu", []string{"eu-bastioned"}, "by-tag.example.com")
	// No covens of its own — the routable string here is the incarnation's NAME.
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
		sid, why string
	}{
		{"by-tag.example.com", "the label is on the incarnation, not on this host"},
		{"by-name.example.com", "an incarnation's name is an identity, not a host tag"},
	} {
		name, src, err := router.RouteFor(ctx, tc.sid)
		if err != nil {
			t.Fatalf("RouteFor(%s): %v", tc.sid, err)
		}
		if name != "static-fallback" || src != SourceCluster {
			t.Errorf("RouteFor(%s) = (%q, %v), want (static-fallback, SourceCluster) — %s",
				tc.sid, name, src, tc.why)
		}
	}

	if _, err := pool.Exec(ctx,
		`UPDATE souls SET coven = ARRAY['db', 'eu-bastioned'] WHERE sid = 'by-tag.example.com'`); err != nil {
		t.Fatalf("tag host with eu-bastioned: %v", err)
	}
	name, src, err := router.RouteFor(ctx, "by-tag.example.com")
	if err != nil {
		t.Fatalf("RouteFor(by-tag, tagged): %v", err)
	}
	if name != "bastion-eu" || src != SourceCoven {
		t.Errorf("tagged host = (%q, %v), want (bastion-eu, SourceCoven) — an operator attached this tag by hand",
			name, src)
	}
}

// TestIntegration_Router_UnconfiguredTagKeepsClusterDefault — GUARD (NIM-251):
// the Level 2 miss. A host whose own tags have no per-coven provider configured
// falls through to the cluster default rather than to an error or an empty
// provider name.
func TestIntegration_Router_UnconfiguredTagKeepsClusterDefault(t *testing.T) {
	pool := newRouterPGPool(t)
	ctx := context.Background()

	seedRouterSoul(t, pool, "outsider.example.com", []string{"db"})

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

// TestIntegration_Router_OwnTagRoutesPastIncarnationLabel — GUARD (NIM-281): the
// two labels are both configured and only the host's own one is reachable, so
// the incarnation's cannot even tie-break. It sorts FIRST alphabetically, which
// is what makes the result diagnostic: a reader that still unioned the two would
// return `a-provider` here.
func TestIntegration_Router_OwnTagRoutesPastIncarnationLabel(t *testing.T) {
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
		t.Errorf("got (%q, %v), want (z-provider, SourceCoven) — only the host's own tag routes it", name, src)
	}
}
