package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/pg"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestBuildBootstrapDeps_DoesNotHandTheListenerTheSharedPool is the NIM-839
// wiring guard.
//
// The whole fix rests on one assignment: the pre-auth Bootstrap listener gets
// its own pool, not `d.pool`. It is the only listener an unauthenticated caller
// can reach and it reads the database before the token it was handed is
// checked, so sharing the pool puts that traffic in the same Acquire queue as
// `/v1`, EventStream, the Reaper and the audit writer — which is the bug.
//
// Write `Pool: d.pool` back into [daemon.buildBootstrapDeps] and this goes red
// on the identity check. No load, no timing, no parsing of source.
func TestBuildBootstrapDeps_DoesNotHandTheListenerTheSharedPool(t *testing.T) {
	shared := mustLazyPoolForWireup(t, 20)
	defer shared.Close()

	d := &daemon{pool: shared, cleanups: &cleanupStack{}, cfg: &config.KeeperConfig{}}
	deps, err := d.buildBootstrapDeps(context.Background())
	if err != nil {
		t.Fatalf("buildBootstrapDeps: %v", err)
	}
	for _, fn := range d.cleanups.fns {
		defer fn()
	}

	if deps.Pool == nil {
		t.Fatal("BootstrapDeps.Pool is nil")
	}
	if any(deps.Pool) == any(shared) {
		t.Fatal("the Bootstrap listener was handed the SHARED pool — unauthenticated traffic queues in front of /v1")
	}

	derived, ok := deps.Pool.(*pgxpool.Pool)
	if !ok {
		t.Fatalf("BootstrapDeps.Pool is %T, want *pgxpool.Pool", deps.Pool)
	}
	if got, want := derived.Config().MaxConns, pg.BootstrapPoolMax(20); got != want {
		t.Errorf("derived pool MaxConns = %d, want %d", got, want)
	}
}

// TestBuildBootstrapDeps_ClosesTheDerivedPoolOnShutdown — the second pool is
// the daemon's to close. Without the cleanup a keeper that fails to start after
// this point leaks it, and so does every test that builds a daemon.
func TestBuildBootstrapDeps_ClosesTheDerivedPoolOnShutdown(t *testing.T) {
	shared := mustLazyPoolForWireup(t, 20)
	defer shared.Close()

	d := &daemon{pool: shared, cleanups: &cleanupStack{}, cfg: &config.KeeperConfig{}}
	before := len(d.cleanups.fns)
	if _, err := d.buildBootstrapDeps(context.Background()); err != nil {
		t.Fatalf("buildBootstrapDeps: %v", err)
	}
	if len(d.cleanups.fns) != before+1 {
		t.Fatalf("cleanups grew by %d, want 1 — the derived pool is never closed", len(d.cleanups.fns)-before)
	}
	d.cleanups.runLIFO()

	// Not asserted here, and worth saying so rather than implying it: that the
	// SHARED pool survives. `Config()` hands back a stored copy Close never
	// touches, so reading it proves nothing, and an Acquire against a DSN that
	// cannot dial fails for its own reason either way.
}

// mustLazyPoolForWireup builds a pool that opens nothing: MinConns and
// MinIdleConns are zero, and the DSN names a unix socket directory that cannot
// exist, so nothing here reaches the network even if that changes.
func mustLazyPoolForWireup(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://keeper:secret@/keeper?host=/nonexistent-soul-stack-test")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	return pool
}
