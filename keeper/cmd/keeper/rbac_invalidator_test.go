package main

// Guard tests for the LOCAL half of RBAC invalidation (NIM-421).
//
// SECURITY invariant: the node that performed the mutation enforces it on its
// very next request — it does NOT wait out its own TTL poll.
//
// This is the half that did not exist. The pub/sub self-filter
// (keeperredis.SubscribeRBACInvalidate drops messages whose origin_kid is its
// own) meant the mutating node was the one node that never learned about its own
// revoke. Measured on a two-node stand before the fix: the subscriber flipped in
// ~0.05s, the origin took up to 10.19s.
//
// The holder below is built with an interval far longer than any test could
// wait, on purpose: if these tests ever pass because a background poll happened
// to fire, they are not testing the mechanism. Nothing here starts Holder.Run.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

const invalidatorTestAID = "archon-invalidator-test"

// mutableSnapshotSource stands in for Postgres: Load returns whatever the last
// write left, so flipping `revoked` models the committed revoke the invalidator
// is called after.
type mutableSnapshotSource struct {
	revoked atomic.Bool
	loads   atomic.Int32
	err     atomic.Pointer[error]
}

func (s *mutableSnapshotSource) Load(_ context.Context) (*rbac.Snapshot, error) {
	s.loads.Add(1)
	if e := s.err.Load(); e != nil {
		return nil, *e
	}
	snap := &rbac.Snapshot{
		Roles:      map[string][]string{"cluster-admin": {"*"}},
		Membership: map[string][]string{invalidatorTestAID: {"cluster-admin"}},
		Revoked:    map[string]time.Time{},
	}
	if s.revoked.Load() {
		snap.Revoked[invalidatorTestAID] = time.Unix(1700000000, 0).UTC()
	}
	return snap, nil
}

// newInvalidatorTestHolder builds a Holder over src with a TTL far out of reach
// of the test, and does NOT run its poll loop.
func newInvalidatorTestHolder(t *testing.T, src rbac.SnapshotSource) *rbac.Holder {
	t.Helper()
	h, err := rbac.NewHolder(context.Background(), src, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	return h
}

// TestRBACInvalidator_RefreshesLocalSnapshot — after Invalidate, the node's own
// snapshot already holds the revoke.
//
// redis is nil here, which is the second half of the claim: the local refresh is
// not a side effect of the cluster fan-out, so a single-node stand gets it too.
func TestRBACInvalidator_RefreshesLocalSnapshot(t *testing.T) {
	src := &mutableSnapshotSource{}
	holder := newInvalidatorTestHolder(t, src)

	if holder.IsRevoked(invalidatorTestAID) {
		t.Fatalf("precondition: the AID must start out active")
	}

	// The revoke commits.
	src.revoked.Store(true)

	// Nothing has told the holder yet — it must still be stale. If this ever
	// flips on its own, some background reload is in play and the assertion
	// below stops meaning anything.
	if holder.IsRevoked(invalidatorTestAID) {
		t.Fatalf("the snapshot refreshed without an invalidate — this test can no longer tell the mechanism from a poll")
	}

	rbacInvalidator{holder: holder, redis: nil, kid: "keeper-test", logger: discardLogger()}.
		Invalidate(context.Background())

	if !holder.IsRevoked(invalidatorTestAID) {
		t.Errorf("after Invalidate the node's own snapshot still does not hold the revoke — the operator who pressed revoke keeps honouring the token until the TTL poll (NIM-421)")
	}
}

// TestRBACInvalidator_RefreshFailureIsBestEffort — a failing snapshot load must
// not panic or propagate: the revoke has already committed, and propagation
// falling back to the TTL poll is not a reason to report the write as failed.
//
// It must, however, be VISIBLE. When the refresh fails the node silently returns
// to the ≤10s window this ticket removed, and the log line is the only thing
// that says so — so the warning is part of the contract, not decoration.
func TestRBACInvalidator_RefreshFailureIsBestEffort(t *testing.T) {
	src := &mutableSnapshotSource{}
	holder := newInvalidatorTestHolder(t, src)

	loadErr := errors.New("snapshot load failed")
	src.err.Store(&loadErr)
	src.revoked.Store(true)

	var logged bytes.Buffer
	rbacInvalidator{
		holder: holder, redis: nil, kid: "keeper-test",
		logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}.Invalidate(context.Background())

	if holder.IsRevoked(invalidatorTestAID) {
		t.Errorf("a failed load must leave the previous enforcer in place, not a half-built one")
	}
	if !strings.Contains(logged.String(), "local snapshot refresh after mutation failed") {
		t.Errorf("a failed refresh must be logged — the node just fell back to the TTL poll and nothing else says so; log was %q", logged.String())
	}
}

// TestRBACInvalidator_SuccessIsQuiet — the other side of the warning above: a
// refresh that worked must not log a failure, or the signal stops meaning
// anything.
func TestRBACInvalidator_SuccessIsQuiet(t *testing.T) {
	src := &mutableSnapshotSource{}
	holder := newInvalidatorTestHolder(t, src)
	src.revoked.Store(true)

	var logged bytes.Buffer
	rbacInvalidator{
		holder: holder, redis: nil, kid: "keeper-test",
		logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}.Invalidate(context.Background())

	if !holder.IsRevoked(invalidatorTestAID) {
		t.Fatalf("precondition: the refresh was supposed to succeed")
	}
	if strings.Contains(logged.String(), "failed") {
		t.Errorf("a successful refresh must not report a failure; log was %q", logged.String())
	}
}

// TestRBACInvalidator_NilHolderIsInert — a zero holder must not panic. The two
// production call sites both pass one; this pins that a third one that forgets
// degrades to the old TTL-poll behaviour instead of taking the daemon down
// inside a committed write.
func TestRBACInvalidator_NilHolderIsInert(t *testing.T) {
	rbacInvalidator{holder: nil, redis: nil, kid: "keeper-test", logger: discardLogger()}.
		Invalidate(context.Background())
}

// TestRBACInvalidator_RefreshRunsWithoutRedis — the local refresh does not
// depend on Redis being reachable. With redis nil the publish half is skipped
// entirely, and the load must still have happened.
//
// Deliberately NOT an ordering assertion. Invalidate publishes before it
// refreshes (see the rationale on the method), but that ordering is a latency
// property — both orders converge on the same snapshot — and observing the
// publish needs a live Redis, since [keeperredis.NewClient] pings on
// construction. A "guard" that could not see the publish would only restate the
// line below under a name that promises more.
func TestRBACInvalidator_RefreshRunsWithoutRedis(t *testing.T) {
	src := &mutableSnapshotSource{}
	holder := newInvalidatorTestHolder(t, src)
	before := src.loads.Load()

	rbacInvalidator{holder: holder, redis: nil, kid: "keeper-test", logger: discardLogger()}.
		Invalidate(context.Background())

	if got := src.loads.Load(); got != before+1 {
		t.Errorf("snapshot loads = %d, want %d — the local refresh did not run without Redis", got, before+1)
	}
}
