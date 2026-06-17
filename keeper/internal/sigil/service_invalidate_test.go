package sigil

import (
	"context"
	"sync/atomic"
	"testing"
)

// countingInvalidator — тестовый [Invalidator], считающий вызовы Invalidate
// (паттерн rbac.countingInvalidator).
type countingInvalidator struct {
	calls atomic.Int64
}

func (c *countingInvalidator) Invalidate(_ context.Context) { c.calls.Add(1) }

// TestService_Invalidate_NilSafe — без подключённого invalidator-а
// Service.invalidate — no-op (single-Keeper/dev без Redis), не паникует.
func TestService_Invalidate_NilSafe(t *testing.T) {
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: &fakeStore{}, Slots: fakeSlotReader{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.invalidate(context.Background())

	svc.SetInvalidator(nil)
	svc.invalidate(context.Background())
}

// TestService_Allow_TriggersInvalidate — успешный Allow дёргает invalidator
// (cluster-wide re-broadcast active-набора, S6c).
func TestService_Allow_TriggersInvalidate(t *testing.T) {
	inv := &countingInvalidator{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: &fakeStore{}, Slots: fakeSlotReader{slot: slotFixture()},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.SetInvalidator(inv)

	if _, err := svc.Allow(context.Background(), AllowInput{
		Namespace: "cloud", Name: "hetzner", Ref: "v1", CallerAID: "archon-test",
	}); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if got := inv.calls.Load(); got != 1 {
		t.Fatalf("Allow invalidate calls = %d, want 1", got)
	}
}

// TestService_Allow_NoInvalidateOnError — провал Insert НЕ дёргает invalidator
// (нет мутации → нечего re-broadcast-ить).
func TestService_Allow_NoInvalidateOnError(t *testing.T) {
	inv := &countingInvalidator{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  &fakeStore{insertErr: ErrSigilAlreadyActive},
		Slots:  fakeSlotReader{slot: slotFixture()},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.SetInvalidator(inv)

	if _, err := svc.Allow(context.Background(), AllowInput{
		Namespace: "cloud", Name: "hetzner", Ref: "v1", CallerAID: "archon-test",
	}); err == nil {
		t.Fatal("Allow: ожидалась ошибка ErrSigilAlreadyActive")
	}
	if got := inv.calls.Load(); got != 0 {
		t.Fatalf("Allow-on-error invalidate calls = %d, want 0", got)
	}
}

// TestService_Revoke_TriggersInvalidate — успешный Revoke дёргает invalidator.
func TestService_Revoke_TriggersInvalidate(t *testing.T) {
	inv := &countingInvalidator{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: &fakeStore{}, Slots: fakeSlotReader{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.SetInvalidator(inv)

	if err := svc.Revoke(context.Background(), "cloud", "hetzner", "v1", "archon-test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := inv.calls.Load(); got != 1 {
		t.Fatalf("Revoke invalidate calls = %d, want 1", got)
	}
}

// TestService_Revoke_NoInvalidateOnError — провал Revoke НЕ дёргает invalidator.
func TestService_Revoke_NoInvalidateOnError(t *testing.T) {
	inv := &countingInvalidator{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  &fakeStore{revokeErr: ErrSigilNotFound},
		Slots:  fakeSlotReader{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.SetInvalidator(inv)

	if err := svc.Revoke(context.Background(), "cloud", "hetzner", "v1", "archon-test"); err == nil {
		t.Fatal("Revoke: ожидалась ошибка ErrSigilNotFound")
	}
	if got := inv.calls.Load(); got != 0 {
		t.Fatalf("Revoke-on-error invalidate calls = %d, want 0", got)
	}
}
