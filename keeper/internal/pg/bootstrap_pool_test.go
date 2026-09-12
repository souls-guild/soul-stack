package pg

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSN points at a unix socket directory that cannot exist.
//
// `pgxpool.NewWithConfig` creates max(MinConns, MinIdleConns) resources in a
// background goroutine, so a pool built to carry a non-zero MinIdleConns DOES
// dial — a TCP DSN here would have these unit tests opening real sessions
// against whatever is listening on 5432 on the machine, silently, with the
// errors swallowed. A socket path that does not exist fails instantly and
// touches no network.
const testDSN = "postgres://keeper:secret@/keeper?host=/nonexistent-soul-stack-test"

func TestBootstrapPoolMax(t *testing.T) {
	cases := []struct {
		shared, want int32
	}{
		{0, 2},  // shared max unset — the floor is what answers
		{1, 2},  // the floor, not the fraction
		{4, 2},  // pgx's own default: a quarter rounds to 1, the floor wins
		{12, 3}, // the fraction, once it clears the floor
		{20, 5},
		{100, 25},
	}
	for _, c := range cases {
		if got := BootstrapPoolMax(c.shared); got != c.want {
			t.Errorf("BootstrapPoolMax(%d) = %d, want %d", c.shared, got, c.want)
		}
	}
}

// TestNewBootstrapPool_DerivesASmallLazyPool covers what this constructor
// promises: the derived sizing, that an idle keeper pays nothing for the second
// pool, and that deriving it does not resize the first.
//
// What it does NOT cover, deliberately and worth stating: that
// `keeper/cmd/keeper.setupGRPCBootstrap` actually hands the listener THIS pool
// rather than `d.pool`. That is one line of wiring and no test in the tree sees
// it — asserting on it would mean parsing daemon.go, which is a text guard
// wearing a type system. The wiring is covered by review.
func TestNewBootstrapPool_DerivesASmallLazyPool(t *testing.T) {
	shared := mustLazyPool(t, 20)
	defer shared.Close()

	bootstrapPool, err := NewBootstrapPool(context.Background(), shared)
	if err != nil {
		t.Fatalf("NewBootstrapPool: %v", err)
	}
	defer bootstrapPool.Close()

	if got, want := bootstrapPool.Config().MaxConns, BootstrapPoolMax(20); got != want {
		t.Errorf("bootstrap pool MaxConns = %d, want %d", got, want)
	}
	if got := bootstrapPool.Config().MinConns; got != 0 {
		t.Errorf("bootstrap pool MinConns = %d, want 0 — an idle keeper should pay nothing for it", got)
	}
	if got := shared.Config().MaxConns; got != 20 {
		t.Errorf("shared pool MaxConns = %d, want 20 — deriving the second pool resized the first", got)
	}
}

// TestNewBootstrapPool_IgnoresAnInheritedMinIdle — pgxpool creates
// max(MinConns, MinIdleConns) resources eagerly, and MinIdleConns arrives from
// the shared config, where a DSN carrying `pool_min_idle_conns` can have set
// it. Zeroing MinConns alone would leave the second pool holding connections at
// idle on exactly the deployments that tuned their main one.
func TestNewBootstrapPool_IgnoresAnInheritedMinIdle(t *testing.T) {
	withIdle := mustPoolWithMinIdle(t, 20, 4)
	defer withIdle.Close()

	bootstrapPool, err := NewBootstrapPool(context.Background(), withIdle)
	if err != nil {
		t.Fatalf("NewBootstrapPool: %v", err)
	}
	defer bootstrapPool.Close()

	if got := bootstrapPool.Config().MinIdleConns; got != 0 {
		t.Errorf("bootstrap pool MinIdleConns = %d, want 0 — it inherited the shared pool's eagerness", got)
	}
}

func TestNewBootstrapPool_NilShared(t *testing.T) {
	if _, err := NewBootstrapPool(context.Background(), nil); err == nil {
		t.Error("NewBootstrapPool(nil) returned no error")
	}
}

// mustLazyPool builds a pool that opens nothing: MinConns and MinIdleConns are
// both zero, so `NewWithConfig` creates no resources and nothing here asks it
// for a connection.
func mustLazyPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	return mustPoolWithMinIdle(t, maxConns, 0)
}

// mustPoolWithMinIdle builds a pool carrying the given MinIdleConns. With a
// non-zero value this pool DOES try to open that many connections in the
// background — see [testDSN] for why that is harmless here.
func mustPoolWithMinIdle(t *testing.T, maxConns, minIdle int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MinIdleConns = minIdle
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	return pool
}
