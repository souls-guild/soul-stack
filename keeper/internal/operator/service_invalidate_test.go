package operator

import (
	"context"
	"sync/atomic"
	"testing"
)

// countingInvalidator — тестовый [Invalidator], считающий вызовы Invalidate.
type countingInvalidator struct {
	calls atomic.Int64
}

func (c *countingInvalidator) Invalidate(_ context.Context) { c.calls.Add(1) }

// TestService_Invalidate_NilSafe — без подключённого invalidator-а Revoke
// проходит без panic-а (single-Keeper/dev: чистый TTL-poll).
func TestService_Invalidate_NilSafe(t *testing.T) {
	pool := &svcPool{effectiveAdmins: []string{"archon-alice"}}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	// invalidate без SetInvalidator — тихо ничего не делает.
	s.invalidate(context.Background())

	// SetInvalidator(nil) тоже не должен ломать последующий invalidate.
	s.SetInvalidator(nil)
	s.invalidate(context.Background())

	// Сквозной Revoke без invalidator-а — успешный commit, нет paniс-а на
	// финальном s.invalidate(ctx).
	if err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

// TestRevoke_PublishesInvalidate — ADR-014 Amendment 2026-05-27: после
// успешного Revoke service дёргает подключённый Invalidator (cluster-wide
// `rbac:invalidate`), чтобы остальные ноды near-instant перечитали снимок.
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
		t.Fatalf("Invalidate calls = %d, want 1 после успешного Revoke", got)
	}
}

// TestRevoke_DoesNotPublishOnLockout — self-lockout-инвариант сработал,
// commit не произошёл → Invalidate тоже НЕ должен вызываться (паттерн rbac.Service).
func TestRevoke_DoesNotPublishOnLockout(t *testing.T) {
	pool := &svcPool{
		// target — единственный активный admin → ErrWouldLockOutCluster.
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
		t.Errorf("Invalidate calls = %d, want 0 при lockout (commit не прошёл)", got)
	}
}

// TestRevoke_DoesNotPublishOnCommitFailure — UPDATE прошёл, но Commit упал →
// Invalidate тоже НЕ вызывается (хук строго после успешного commit-а).
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
		t.Errorf("Invalidate calls = %d, want 0 при commit-fail", got)
	}
}

// errCommit — sentinel-error для commit-failure-сценария.
type errCommit struct{}

func (errCommit) Error() string { return "fake commit failure" }
