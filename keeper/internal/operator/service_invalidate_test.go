package operator

import (
	"context"
	"sync/atomic"
	"testing"
)

// countingInvalidator — test [Invalidator] counting Invalidate calls.
type countingInvalidator struct {
	calls atomic.Int64
}

func (c *countingInvalidator) Invalidate(_ context.Context) { c.calls.Add(1) }

// TestService_Invalidate_NilSafe — without connected invalidator Revoke
// succeeds without panic (single-Keeper/dev: pure TTL-poll).
func TestService_Invalidate_NilSafe(t *testing.T) {
	pool := &svcPool{effectiveAdmins: []string{"archon-alice"}}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	// invalidate without SetInvalidator — silently does nothing.
	s.invalidate(context.Background())

	// SetInvalidator(nil) also shouldn't break subsequent invalidate.
	s.SetInvalidator(nil)
	s.invalidate(context.Background())

	// Full Revoke without invalidator — successful commit, no panic
	// at final s.invalidate(ctx).
	if err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

// TestRevoke_PublishesInvalidate — ADR-014 Amendment 2026-05-27: after
// successful Revoke service calls connected Invalidator (cluster-wide
// `rbac:invalidate`) so other nodes near-instantly re-read snapshot.
func TestRevoke_PublishesInvalidate(t *testing.T) {
	pool := &svcPool{effectiveAdmins: []string{"archon-alice"}}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	inv := &countingInvalidator{}
	s.SetInvalidator(inv)

	if err := s.Revoke(context.Background(), RevokeInput{
		AID:       "archon-bob",
		Reason:    "left team",
		CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := inv.calls.Load(); got != 1 {
		t.Fatalf("Invalidate calls = %d, want 1 after successful Revoke", got)
	}
}

// TestRevoke_DoesNotPublishOnLockout — self-lockout invariant triggered,
// commit didn't happen → Invalidate also should NOT be called (rbac.Service pattern).
func TestRevoke_DoesNotPublishOnLockout(t *testing.T) {
	pool := &svcPool{
		// target — only active admin → ErrWouldLockOutCluster.
		effectiveAdmins: []string{"archon-alice"},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	inv := &countingInvalidator{}
	s.SetInvalidator(inv)

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Revoke: err = nil, want ErrWouldLockOutCluster")
	}
	if got := inv.calls.Load(); got != 0 {
		t.Errorf("Invalidate calls = %d, want 0 on lockout (commit failed)", got)
	}
}

// TestRevoke_DoesNotPublishOnCommitFailure — UPDATE succeeded, but Commit failed →
// Invalidate also NOT called (hook strictly after successful commit).
func TestRevoke_DoesNotPublishOnCommitFailure(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: []string{"archon-alice"},
		commitErr:       errCommit{},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	inv := &countingInvalidator{}
	s.SetInvalidator(inv)

	if err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"}); err == nil {
		t.Fatal("Revoke: err = nil, want commit-failure")
	}
	if got := inv.calls.Load(); got != 0 {
		t.Errorf("Invalidate calls = %d, want 0 on commit-fail", got)
	}
}

// errCommit — sentinel error for commit-failure scenario.
type errCommit struct{}

func (errCommit) Error() string { return "fake commit failure" }
